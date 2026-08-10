// Cinnamon panel applet for go-sysmon.
//
// The panel shows compact Cairo gauges. Everything the CLI and the web UI
// report is in the popup menu, which is built from the shared section model in
// extensions/shared/sections.js, installed alongside this file.
//
// Data arrives as server-sent events. GJS has no EventSource, so the applet
// reads the response body through libsoup and feeds lines to the parser in
// sse.js. All parsing, formatting and layout logic lives in those modules so it
// can be unit tested without a desktop session; this file is GObject
// Introspection glue.

const Applet = imports.ui.applet;
const St = imports.gi.St;
const Mainloop = imports.mainloop;
const Soup = imports.gi.Soup;
const Gio = imports.gi.Gio;
const GLib = imports.gi.GLib;
const Lang = imports.lang;
const PopupMenu = imports.ui.popupMenu;
const Settings = imports.ui.settings;

// Cinnamon's require() resolves against the applet directory only -- a path
// with a subdirectory in it is reported as "Path does not exist" -- so the
// shared modules are installed alongside this file rather than under lib/.
const fmt = require("./format.js");
const sse = require("./sse.js");
const snapshotLib = require("./snapshot.js");
const sections = require("./sections.js");

// Color constants for gauge bars (RGBA).
const COLOR_GREEN = [0.21, 0.83, 0.60, 1.0];
const COLOR_YELLOW = [0.98, 0.74, 0.14, 1.0];
const COLOR_RED = [0.97, 0.45, 0.45, 1.0];
const COLOR_BG = [0.30, 0.30, 0.30, 0.3];
const COLOR_TEXT = [0.80, 0.80, 0.80, 1.0];

// LEVEL_COLORS maps the shared usage classification onto gauge colours, so the
// applet and any other consumer agree on what counts as a warning.
const LEVEL_COLORS = {
    normal: COLOR_GREEN,
    warn: COLOR_YELLOW,
    critical: COLOR_RED,
};

// Layout constants (pixels).
const BAR_WIDTH = 3;
const BAR_GAP = 1;
const SECTION_GAP = 6;
const MEM_BAR_WIDTH = 8;
const DISK_BAR_WIDTH = 8;
const NET_TEXT_WIDTH = 55;
const MIN_WIDTH = 20;
const FONT_SIZE = 9;
const PADDING = 2;

// Reconnect delay in milliseconds, used until the stream advertises its own.
const RECONNECT_DELAY_MS = 3000;
const MAX_RECONNECT_DELAY_MS = 30000;

/**
 * _usageColor returns the RGBA array for a usage fraction, using the shared
 * classification so thresholds are defined in exactly one place.
 */
function _usageColor(fraction) {
    return LEVEL_COLORS[snapshotLib.usageLevel(fraction)] || COLOR_GREEN;
}

class SysmonApplet extends Applet.TextIconApplet {

    _init(metadata, orientation, panelHeight, instanceId) {
        super._init(orientation, panelHeight, instanceId);

        this.metadata = metadata;
        this.settings = new Settings.AppletSettings(this, metadata.uuid, instanceId);

        // Bind user-configurable settings to instance properties. The panel
        // toggles only affect drawing, so they just queue a repaint; a changed
        // server address or interval has to be pushed to the server.
        this.settings.bind("server-address", "serverAddress", Lang.bind(this, this._onServerChanged));
        this.settings.bind("update-interval", "updateInterval", Lang.bind(this, this._onIntervalChanged));
        this.settings.bind("show-cpu", "showCpu", Lang.bind(this, this._queueRepaint));
        this.settings.bind("show-memory", "showMemory", Lang.bind(this, this._queueRepaint));
        this.settings.bind("show-network", "showNetwork", Lang.bind(this, this._queueRepaint));
        this.settings.bind("show-disk", "showDisk", Lang.bind(this, this._queueRepaint));

        // Cairo drawing area for gauge rendering.
        this.drawingArea = new St.DrawingArea();
        this.drawingArea.connect("repaint", Lang.bind(this, this._onRepaint));
        this.actor.add_actor(this.drawingArea);

        // Runtime state.
        this.snapshot = null;
        this.session = null;
        this.stream = null;
        this.cancellable = null;
        this.parser = null;
        this.pending = "";
        this.reconnectTimer = null;
        this.reconnectDelay = RECONNECT_DELAY_MS;
        this.stopped = false;

        this._buildMenu();

        this.set_applet_tooltip("System Monitor - Connecting...");
        this._connect();
    }

    // ---- menu ------------------------------------------------------------

    /**
     * _buildMenu creates the popup and its per-section submenus. The structure
     * is stable across snapshots; only the contents are rebuilt, so the menu
     * does not collapse while the user has it open.
     */
    _buildMenu() {
        this.menuManager = new PopupMenu.PopupMenuManager(this);
        this.menu = new Applet.AppletPopupMenu(this, this._orientation);
        this.menuManager.addMenu(this.menu);

        this.statusItem = new PopupMenu.PopupMenuItem("Connecting...", { reactive: false });
        this.menu.addMenuItem(this.statusItem);
        this.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());

        // One submenu per section, created once and refilled on each snapshot.
        this.sectionMenus = {};
        this.sectionOrder = [];

        this.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());

        const openItem = new PopupMenu.PopupMenuItem("Open full dashboard");
        openItem.connect("activate", Lang.bind(this, this._openDashboard));
        this.menu.addMenuItem(openItem);
    }

    /**
     * _refreshMenu rebuilds the section submenus from the latest snapshot.
     */
    _refreshMenu() {
        const model = sections.buildSections(this.snapshot);

        for (let i = 0; i < model.length; i++) {
            const section = model[i];
            let entry = this.sectionMenus[section.key];

            if (!entry) {
                entry = new PopupMenu.PopupSubMenuMenuItem(section.title);
                this.sectionMenus[section.key] = entry;
                this.sectionOrder.push(section.key);
                // Insert before the trailing separator and dashboard item.
                this.menu.addMenuItem(entry, this.menu.numMenuItems - 2);
            }

            entry.label.set_text(section.title + "   " + section.summary);

            // Rebuilding the submenu wholesale keeps the code simple; a menu
            // the user has not opened is not realised, so this is cheap.
            entry.menu.removeAll();
            for (let b = 0; b < section.blocks.length; b++) {
                const block = section.blocks[b];
                if (block.rows.length === 0) {
                    continue;
                }
                if (block.title) {
                    const header = new PopupMenu.PopupMenuItem(block.title, { reactive: false });
                    header.actor.add_style_class_name("popup-subtitle-menu-item");
                    entry.menu.addMenuItem(header);
                }
                for (let r = 0; r < block.rows.length; r++) {
                    const rowData = block.rows[r];
                    const text = rowData.label
                        ? rowData.label + ":  " + rowData.value
                        : rowData.value;
                    entry.menu.addMenuItem(
                        new PopupMenu.PopupMenuItem(text, { reactive: false })
                    );
                }
            }
        }
    }

    /**
     * _openDashboard launches the desktop GUI.
     */
    _openDashboard() {
        GLib.spawn_command_line_async("sysmon");
    }

    // ---- event stream ----------------------------------------------------

    /**
     * _connect opens the server-sent events stream. On failure it schedules a
     * reconnect attempt.
     */
    _connect() {
        if (this.stopped) {
            return;
        }

        this._teardownStream();
        this.parser = new sse.Parser();
        this.pending = "";

        try {
            this.session = new Soup.Session();
            this.cancellable = new Gio.Cancellable();

            const uri = "http://" + this.serverAddress + "/api/events";
            const message = Soup.Message.new("GET", uri);
            message.request_headers.append("Accept", "text/event-stream");
            // The applet inflates nothing itself, so ask for the stream as-is.
            message.request_headers.append("Accept-Encoding", "identity");

            this.session.send_async(
                message,
                GLib.PRIORITY_DEFAULT,
                this.cancellable,
                Lang.bind(this, function (session, result) {
                    try {
                        const inputStream = session.send_finish(result);
                        if (message.get_status() !== Soup.Status.OK) {
                            this._scheduleReconnect();
                            return;
                        }
                        this.stream = new Gio.DataInputStream({
                            base_stream: inputStream,
                            close_base_stream: true,
                        });
                        this.set_applet_tooltip("System Monitor - Connected");
                        this.statusItem.label.set_text("Connected to " + this.serverAddress);
                        this._pushInterval();
                        this._readNext();
                    } catch (e) {
                        this._scheduleReconnect();
                    }
                })
            );
        } catch (e) {
            this._scheduleReconnect();
        }
    }

    /**
     * _readNext queues an asynchronous read of the next line from the stream.
     * Each completed line is fed to the parser, and a dispatched event is
     * applied to the applet.
     */
    _readNext() {
        if (this.stopped || !this.stream) {
            return;
        }

        this.stream.read_line_async(
            GLib.PRIORITY_DEFAULT,
            this.cancellable,
            Lang.bind(this, function (stream, result) {
                let line;
                try {
                    const [data] = stream.read_line_finish_utf8(result);
                    line = data;
                } catch (e) {
                    this._onDisconnected();
                    return;
                }

                // A null line means end of stream: the server closed, so
                // reconnect rather than spinning on an exhausted reader.
                if (line === null) {
                    this._onDisconnected();
                    return;
                }

                const event = this.parser.push(line);
                if (event) {
                    this._onEvent(event);
                }

                this._readNext();
            })
        );
    }

    /**
     * _onEvent applies a dispatched server-sent event.
     */
    _onEvent(event) {
        if (event.name === "bye") {
            // The monitor stopped. Reconnecting would only hammer a server
            // that has said it is done, so wait for the user to act.
            this.set_applet_tooltip("System Monitor - Server stopped");
            this.statusItem.label.set_text("Server stopped");
            this._teardownStream();
            this._scheduleReconnect();
            return;
        }

        if (event.name !== "snapshot") {
            return;
        }

        const parsed = snapshotLib.parseSnapshot(event.data);
        if (!parsed) {
            return;
        }

        // Reset the backoff on data, not merely on a successful connect: a
        // server that accepts the connection and closes it immediately would
        // otherwise be retried every three seconds forever.
        this.reconnectDelay = RECONNECT_DELAY_MS;

        this.snapshot = parsed;
        this._queueRepaint();
        this._updateTooltip();
        this._refreshMenu();
    }

    /**
     * _onDisconnected handles a stream that ended or failed.
     */
    _onDisconnected() {
        this._teardownStream();
        if (this.stopped) {
            return;
        }
        this.set_applet_tooltip("System Monitor - Disconnected");
        this.statusItem.label.set_text("Disconnected");
        this._scheduleReconnect();
    }

    /**
     * _teardownStream closes the current stream and session without touching
     * the reconnect timer.
     */
    _teardownStream() {
        if (this.cancellable) {
            this.cancellable.cancel();
            this.cancellable = null;
        }
        if (this.stream) {
            try {
                this.stream.close(null);
            } catch (e) {
                // The peer may already be gone; nothing further to release.
            }
            this.stream = null;
        }
        this.session = null;
    }

    /**
     * _scheduleReconnect queues a reconnect attempt, backing off so a server
     * that stays down is not polled every few seconds indefinitely.
     */
    _scheduleReconnect() {
        if (this.stopped || this.reconnectTimer) {
            return;
        }

        // The server advertises its own reconnect hint; prefer it on the first
        // attempt and back off from there.
        const hint = this.parser && this.parser.retryMs > 0
            ? this.parser.retryMs
            : this.reconnectDelay;
        const delay = Math.min(hint, MAX_RECONNECT_DELAY_MS);

        this.reconnectTimer = Mainloop.timeout_add(
            delay,
            Lang.bind(this, function () {
                this.reconnectTimer = null;
                this.reconnectDelay = Math.min(
                    this.reconnectDelay * 2,
                    MAX_RECONNECT_DELAY_MS
                );
                this._connect();
                return false; // do not repeat
            })
        );
    }

    /**
     * _pushInterval tells the server the poll rate this applet wants. The rate
     * belongs to the server's single monitor, so it is a REST call rather than
     * something carried on the stream.
     */
    _pushInterval() {
        try {
            const uri = "http://" + this.serverAddress + "/api/interval";
            const message = Soup.Message.new("POST", uri);
            const body = JSON.stringify({ interval_ms: this.updateInterval });
            message.set_request_body_from_bytes(
                "application/json",
                new GLib.Bytes(body)
            );

            const session = new Soup.Session();
            session.send_async(message, GLib.PRIORITY_DEFAULT, null,
                function (sess, result) {
                    try {
                        sess.send_finish(result);
                    } catch (e) {
                        // Best effort: a dropped rate change leaves the server
                        // on its previous interval and the stream unaffected.
                    }
                });
        } catch (e) {
            // As above: the stream matters, the rate hint does not.
        }
    }

    // ---- settings callbacks ----------------------------------------------

    _onServerChanged() {
        this.reconnectDelay = RECONNECT_DELAY_MS;
        this._connect();
    }

    _onIntervalChanged() {
        this._pushInterval();
    }

    _queueRepaint() {
        if (this.drawingArea) {
            this.drawingArea.queue_repaint();
        }
    }

    /**
     * _updateTooltip sets the applet tooltip to a summary of the latest
     * snapshot data.
     */
    _updateTooltip() {
        if (!this.snapshot) {
            return;
        }

        const avgCpu = snapshotLib.avgCpuUsage(this.snapshot.cpus);
        const mem = this.snapshot.memory || {};
        const rates = snapshotLib.netRates(this.snapshot.networks);
        const maxTemp = snapshotLib.maxCpuTemp(
            (this.snapshot.sensors || {}).core_temps
        );

        this.set_applet_tooltip(
            "CPU: " + fmt.formatPercent(avgCpu) +
            "   RAM: " + fmt.formatPercent(mem.used_percent) +
            "   Temp: " + fmt.formatTemp(maxTemp) +
            "\n↑ " + fmt.formatBits(rates.sent) + "   ↓ " + fmt.formatBits(rates.recv)
        );
    }

    // ---- drawing ---------------------------------------------------------

    /**
     * _onRepaint renders the gauge bars and network text onto the Cairo
     * context provided by the St.DrawingArea.
     */
    _onRepaint(area) {
        const ctx = area.get_context();
        const [w, h] = area.get_surface_size();
        const snap = this.snapshot;

        if (!snap) {
            // Draw a placeholder when no data is available.
            ctx.setSourceRGBA(0.5, 0.5, 0.5, 0.3);
            ctx.rectangle(0, 0, w, h);
            ctx.fill();
            return;
        }

        // Clear the surface.
        ctx.setSourceRGBA(0, 0, 0, 0);
        ctx.setOperator(1); // Cairo.Operator.CLEAR
        ctx.paint();
        ctx.setOperator(0); // Cairo.Operator.OVER

        let x = 0;

        // CPU bars: one thin bar per core.
        if (this.showCpu && snap.cpus) {
            x = this._drawCpuBars(ctx, snap.cpus, x, h);
            x += SECTION_GAP;
        }

        // Memory bar: single wider bar.
        if (this.showMemory && snap.memory) {
            x = this._drawUsageBar(
                ctx, (snap.memory.used_percent || 0) / 100,
                x, h, MEM_BAR_WIDTH
            );
            x += SECTION_GAP;
        }

        // Network text: upload/download rates.
        if (this.showNetwork && snap.networks) {
            x = this._drawNetworkText(ctx, snap.networks, x, h);
            x += SECTION_GAP;
        }

        // Disk bar: aggregate usage across all disks.
        if (this.showDisk && snap.disks) {
            const usage = snapshotLib.diskUsage(snap.disks);
            x = this._drawUsageBar(ctx, usage.percent / 100, x, h, DISK_BAR_WIDTH);
            x += PADDING;
        }

        // Resize the drawing area to fit the rendered content.
        area.set_width(Math.max(x, MIN_WIDTH));
        area.set_height(h);
    }

    /**
     * _drawCpuBars renders one thin vertical bar per CPU core.
     * Returns the new x offset after drawing.
     */
    _drawCpuBars(ctx, cpus, x, h) {
        for (let i = 0; i < cpus.length; i++) {
            const pct = (cpus[i].usage_percent || 0) / 100;
            const barH = Math.max(1, Math.floor(h * pct));
            const color = _usageColor(pct);

            // Filled portion (bottom-aligned).
            ctx.setSourceRGBA(color[0], color[1], color[2], color[3]);
            ctx.rectangle(x, h - barH, BAR_WIDTH, barH);
            ctx.fill();

            // Background portion.
            ctx.setSourceRGBA(COLOR_BG[0], COLOR_BG[1], COLOR_BG[2], COLOR_BG[3]);
            ctx.rectangle(x, 0, BAR_WIDTH, h - barH);
            ctx.fill();

            x += BAR_WIDTH + BAR_GAP;
        }
        return x;
    }

    /**
     * _drawUsageBar renders a single vertical usage bar.
     * fraction is 0.0-1.0. Returns the new x offset.
     */
    _drawUsageBar(ctx, fraction, x, h, barWidth) {
        const barH = Math.max(1, Math.floor(h * fraction));
        const color = _usageColor(fraction);

        ctx.setSourceRGBA(color[0], color[1], color[2], color[3]);
        ctx.rectangle(x, h - barH, barWidth, barH);
        ctx.fill();

        ctx.setSourceRGBA(COLOR_BG[0], COLOR_BG[1], COLOR_BG[2], COLOR_BG[3]);
        ctx.rectangle(x, 0, barWidth, h - barH);
        ctx.fill();

        return x + barWidth;
    }

    /**
     * _drawNetworkText renders upload/download rate text.
     * Returns the new x offset.
     */
    _drawNetworkText(ctx, networks, x, h) {
        const rates = snapshotLib.netRates(networks);

        ctx.setSourceRGBA(COLOR_TEXT[0], COLOR_TEXT[1], COLOR_TEXT[2], COLOR_TEXT[3]);
        ctx.selectFontFace("monospace", 0, 0);
        ctx.setFontSize(FONT_SIZE);

        ctx.moveTo(x, h * 0.4);
        ctx.showText("↑" + fmt.formatCompactRate(rates.sent));
        ctx.moveTo(x, h * 0.85);
        ctx.showText("↓" + fmt.formatCompactRate(rates.recv));

        return x + NET_TEXT_WIDTH;
    }

    // ---- lifecycle -------------------------------------------------------

    /**
     * on_applet_clicked opens the popup menu.
     */
    on_applet_clicked(event) {
        this.menu.toggle();
    }

    /**
     * on_applet_removed_from_panel cleans up timers and the event stream when
     * the applet is removed from the panel.
     */
    on_applet_removed_from_panel() {
        this.stopped = true;

        if (this.reconnectTimer) {
            Mainloop.source_remove(this.reconnectTimer);
            this.reconnectTimer = null;
        }
        this._teardownStream();

        if (this.settings) {
            this.settings.finalize();
            this.settings = null;
        }
    }
}

/**
 * main is the Cinnamon applet entry point.
 */
function main(metadata, orientation, panelHeight, instanceId) {
    return new SysmonApplet(metadata, orientation, panelHeight, instanceId);
}
