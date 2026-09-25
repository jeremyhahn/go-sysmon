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

// APPLY_PRIORITY is the main-loop priority a received snapshot is applied at.
// It must stay below Clutter's redraw, which muffin dispatches at
// CLUTTER_PRIORITY_REDRAW (G_PRIORITY_HIGH_IDLE + 50, clutter-main.h), so a
// queued frame is always painted before the next snapshot is applied. See
// _onEvent.
const APPLY_PRIORITY = GLib.PRIORITY_DEFAULT_IDLE;

// Reconnect delay in milliseconds, used until the stream advertises its own.
const RECONNECT_DELAY_MS = 3000;
const MAX_RECONNECT_DELAY_MS = 30000;

/**
 * SysmonPopupMenu is the applet's popup. It calls beforeOpen before it opens,
 * so the applet can bring its rows up to date first.
 *
 * Every way the popup opens -- a click through toggle(), the keyboard, or the
 * menu manager moving between panel menus -- goes through open(). And open()
 * works out the popup's size and position before it emits
 * open-state-changed, so rows filled in from that signal would be laid out in
 * a popup sized for the old ones.
 */
class SysmonPopupMenu extends Applet.AppletPopupMenu {
    _init(launcher, orientation, beforeOpen) {
        super._init(launcher, orientation);
        this._beforeOpen = beforeOpen;
    }

    open(animate) {
        if (!this.isOpen) {
            this._beforeOpen();
        }
        super.open(animate);
    }
}

/**
 * _usageColor returns the RGBA array for a usage fraction, using the shared
 * classification so thresholds are defined in exactly one place.
 */
function _usageColor(fraction) {
    return LEVEL_COLORS[snapshotLib.usageLevel(fraction)] || COLOR_GREEN;
}

/**
 * _sectionLines flattens a section's blocks into the lines its submenu shows,
 * in order: a header per titled block, then one line per row. A block with no
 * rows shows nothing, not even its header.
 */
function _sectionLines(section) {
    const lines = [];
    for (const block of section.blocks) {
        if (block.rows.length === 0) {
            continue;
        }
        if (block.title) {
            lines.push({ header: true, text: block.title });
        }
        for (const row of block.rows) {
            lines.push({
                header: false,
                text: row.label ? row.label + ":  " + row.value : row.value,
            });
        }
    }
    return lines;
}

/**
 * _setText sets a label's text only when it differs, so an unchanged row does
 * not cost a relayout.
 */
function _setText(label, text) {
    if (label.get_text() !== text) {
        label.set_text(text);
    }
}

class SysmonApplet extends Applet.TextIconApplet {

    _init(metadata, orientation, panelHeight, instanceId) {
        super._init(orientation, panelHeight, instanceId);

        this.metadata = metadata;
        this.settings = new Settings.AppletSettings(this, metadata.uuid, instanceId);

        // Bind user-configurable settings to instance properties. The panel
        // toggles only affect the gauge, so they just resize and repaint it; a
        // changed server address or interval has to be pushed to the server.
        this.settings.bind("server-address", "serverAddress", Lang.bind(this, this._onServerChanged));
        this.settings.bind("update-interval", "updateInterval", Lang.bind(this, this._onIntervalChanged));
        this.settings.bind("show-cpu", "showCpu", Lang.bind(this, this._queueRepaint));
        this.settings.bind("show-memory", "showMemory", Lang.bind(this, this._queueRepaint));
        this.settings.bind("show-network", "showNetwork", Lang.bind(this, this._queueRepaint));
        this.settings.bind("show-disk", "showDisk", Lang.bind(this, this._queueRepaint));

        // Runtime state. The snapshot is set before the drawing area is sized,
        // because its width is computed from it.
        this.snapshot = null;
        this.session = null;
        this.stream = null;
        this.cancellable = null;
        this.parser = null;
        this.pending = "";
        this.reconnectTimer = null;
        this.reconnectDelay = RECONNECT_DELAY_MS;
        // The newest received snapshot not yet applied, and the idle source
        // that will apply it. See _onEvent.
        this.pendingSnapshot = null;
        this.applySource = null;
        this.stopped = false;

        // Cairo drawing area for gauge rendering. An St.DrawingArea has no
        // natural width of its own, and Clutter never paints an actor with a
        // zero-sized allocation -- so the width has to be set here, not in
        // the repaint handler, or the handler never runs to set it.
        this.drawingArea = new St.DrawingArea();
        this.drawingArea.connect("repaint", Lang.bind(this, this._onRepaint));
        this.actor.add_actor(this.drawingArea);
        this._resizeGauge();

        this._buildMenu();

        this.set_applet_tooltip("System Monitor - Connecting...");
        this._connect();
    }

    // ---- menu ------------------------------------------------------------

    /**
     * _buildMenu creates the popup. Its per-section submenus are created on
     * the first refresh and kept from then on, so the menu does not collapse
     * while the user has it open.
     */
    _buildMenu() {
        this.menuManager = new PopupMenu.PopupMenuManager(this);
        this.menu = new SysmonPopupMenu(this, this._orientation, Lang.bind(this, this._onMenuOpening));
        this.menuManager.addMenu(this.menu);

        this.statusItem = new PopupMenu.PopupMenuItem("Connecting...", { reactive: false });
        this.menu.addMenuItem(this.statusItem);
        this.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());

        // One submenu per section, created once and updated in place. Each
        // section's rows are kept as [{ item, header }] in display order.
        this.sectionMenus = {};
        this.sectionOrder = [];
        this.sectionRows = {};
        // Whether the popup shows an older snapshot than this.snapshot.
        this.menuStale = false;

        this.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());

        const openItem = new PopupMenu.PopupMenuItem("Open full dashboard");
        openItem.connect("activate", Lang.bind(this, this._openDashboard));
        this.menu.addMenuItem(openItem);
    }

    /**
     * _refreshMenu brings the popup up to the latest snapshot, but only while
     * the user can see it. Filling hundreds of rows costs the compositor
     * thread tens of milliseconds per snapshot (#233), which a closed popup
     * would spend for nothing; it is marked stale instead, and _onMenuOpening
     * catches it up before it opens.
     */
    _refreshMenu() {
        if (!this.menu.isOpen) {
            this.menuStale = true;
            return;
        }
        this._fillMenu();
    }

    /**
     * _onMenuOpening runs just before the popup opens, and fills it if a
     * snapshot arrived while it was closed.
     */
    _onMenuOpening() {
        if (this.menuStale) {
            this._fillMenu();
        }
    }

    /**
     * _fillMenu updates the section submenus to the latest snapshot. Rows are
     * updated in place: a row whose text is unchanged is not touched, and
     * items are created or destroyed only where the number or kind of rows
     * changed. That keeps an open popup's layout and scroll position steady
     * while the numbers in it move.
     */
    _fillMenu() {
        this.menuStale = false;
        const model = sections.buildSections(this.snapshot);

        for (let i = 0; i < model.length; i++) {
            const section = model[i];
            let entry = this.sectionMenus[section.key];

            if (!entry) {
                entry = new PopupMenu.PopupSubMenuMenuItem(section.title);
                this.sectionMenus[section.key] = entry;
                this.sectionOrder.push(section.key);
                this.sectionRows[section.key] = [];
                // Insert before the trailing separator and dashboard item.
                this.menu.addMenuItem(entry, this.menu.numMenuItems - 2);
            }

            _setText(entry.label, section.title + "   " + section.summary);
            this._fillSection(entry.menu, this.sectionRows[section.key], _sectionLines(section));
        }
    }

    /**
     * _fillSection makes a submenu show lines, reusing the items in rows (the
     * submenu's current rows, in order) wherever the kind of row matches, and
     * updates rows to match.
     */
    _fillSection(submenu, rows, lines) {
        let i = 0;
        while (i < lines.length && i < rows.length && rows[i].header === lines[i].header) {
            _setText(rows[i].item.label, lines[i].text);
            i++;
        }

        // From the first row that cannot be reused, replace the rest. Rows are
        // appended, so everything after it has to go to keep the order.
        for (const row of rows.splice(i)) {
            row.item.destroy();
        }
        for (; i < lines.length; i++) {
            const item = new PopupMenu.PopupMenuItem(lines[i].text, { reactive: false });
            if (lines[i].header) {
                item.actor.add_style_class_name("popup-subtitle-menu-item");
            }
            submenu.addMenuItem(item);
            rows.push({ item: item, header: lines[i].header });
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
     * _onEvent handles a dispatched server-sent event. A snapshot is only
     * parsed here; _applySnapshot shows it.
     */
    _onEvent(event) {
        if (event.name === "bye") {
            // The monitor stopped. Reconnecting would only hammer a server
            // that has said it is done, so wait for the user to act. Tear
            // down first: that shows the stream's last snapshot, which must
            // not then overwrite this.
            this._teardownStream();
            this.set_applet_tooltip("System Monitor - Server stopped");
            this.statusItem.label.set_text("Server stopped");
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

        // Applying a snapshot can cost more than receiving it -- rebuilding
        // the popup once took tens of milliseconds (#233) -- and this callback
        // runs at the stream's
        // default priority, above Clutter's redraw. Applying it here let a
        // host that took longer than the interval to apply each snapshot keep
        // Cinnamon's main loop busy with one after another, so the redraw never
        // ran and the whole desktop, gauge included, stopped painting (#232).
        // So keep only the newest snapshot and apply it from an idle source
        // below the redraw priority: every queued frame is painted first, and a
        // snapshot that arrives before the last one was applied replaces it
        // rather than queueing behind it.
        this.pendingSnapshot = parsed;
        if (this.applySource === null) {
            this.applySource = Mainloop.idle_add(Lang.bind(this, this._applySnapshot), APPLY_PRIORITY);
        }
    }

    /**
     * _applySnapshot shows the newest received snapshot in the gauge, the
     * tooltip and the popup. It is the idle source _onEvent adds, and runs
     * once per scheduling.
     */
    _applySnapshot() {
        this.applySource = null;
        this.snapshot = this.pendingSnapshot;
        this.pendingSnapshot = null;

        this._queueRepaint();
        this._updateTooltip();
        this._refreshMenu();
        return GLib.SOURCE_REMOVE;
    }

    /**
     * _cancelApply removes the idle source that would apply the pending
     * snapshot, and reports whether there was one.
     */
    _cancelApply() {
        if (this.applySource === null) {
            return false;
        }
        Mainloop.source_remove(this.applySource);
        this.applySource = null;
        return true;
    }

    /**
     * _flushApply applies the pending snapshot now, if there is one. A stream
     * that ends has delivered everything it will, so the last snapshot it sent
     * is shown before the applet reports the stream's end -- the order the
     * events arrived in -- rather than overwriting that report later.
     */
    _flushApply() {
        if (this._cancelApply()) {
            this._applySnapshot();
        }
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
     * _teardownStream applies the last snapshot the current stream delivered,
     * then closes the stream and session without touching the reconnect
     * timer.
     */
    _teardownStream() {
        this._flushApply();
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

    /**
     * _queueRepaint resizes the gauge to fit what it is about to draw and
     * schedules a frame. Every change to the gauge's content comes through
     * here, so the size is always settled before Clutter paints.
     */
    _queueRepaint() {
        if (this.drawingArea) {
            this._resizeGauge();
            this.drawingArea.queue_repaint();
        }
    }

    /**
     * _resizeGauge sets the drawing area's width to the gauge's natural width.
     */
    _resizeGauge() {
        this.drawingArea.set_width(this._gaugeWidth());
    }

    /**
     * _gaugeWidth is the width in pixels the gauge needs for the current
     * snapshot and settings, never less than MIN_WIDTH.
     */
    _gaugeWidth() {
        let width = 0;
        for (const segment of this._gaugeSegments()) {
            width += segment.width;
        }
        return Math.max(width, MIN_WIDTH);
    }

    /**
     * _gaugeSegments lists the gauge's visible parts, left to right, each with
     * its width in pixels and a function that draws it at a given x. Sizing
     * and drawing both walk this list, so they cannot disagree.
     */
    _gaugeSegments() {
        const snap = this.snapshot;
        if (!snap) {
            return [];
        }

        const segments = [];

        // CPU bars: one thin bar per core.
        if (this.showCpu && snap.cpus) {
            segments.push({
                width: snap.cpus.length * (BAR_WIDTH + BAR_GAP) + SECTION_GAP,
                draw: (ctx, x, h) => this._drawCpuBars(ctx, snap.cpus, x, h),
            });
        }

        // Memory bar: single wider bar.
        if (this.showMemory && snap.memory) {
            segments.push({
                width: MEM_BAR_WIDTH + SECTION_GAP,
                draw: (ctx, x, h) => this._drawUsageBar(
                    ctx, (snap.memory.used_percent || 0) / 100, x, h, MEM_BAR_WIDTH),
            });
        }

        // Network text: upload/download rates.
        if (this.showNetwork && snap.networks) {
            segments.push({
                width: NET_TEXT_WIDTH + SECTION_GAP,
                draw: (ctx, x, h) => this._drawNetworkText(ctx, snap.networks, x, h),
            });
        }

        // Disk bar: aggregate usage across all disks.
        if (this.showDisk && snap.disks) {
            segments.push({
                width: DISK_BAR_WIDTH + PADDING,
                draw: (ctx, x, h) => this._drawUsageBar(
                    ctx, snapshotLib.diskUsage(snap.disks).percent / 100, x, h, DISK_BAR_WIDTH),
            });
        }

        return segments;
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
     * context provided by the St.DrawingArea. It never changes the area's
     * size: Clutter only calls it once the area already has one.
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
        for (const segment of this._gaugeSegments()) {
            segment.draw(ctx, x, h);
            x += segment.width;
        }
    }

    /**
     * _drawCpuBars renders one thin vertical bar per CPU core, starting at x.
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
    }

    /**
     * _drawUsageBar renders a single vertical usage bar at x.
     * fraction is 0.0-1.0.
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
    }

    /**
     * _drawNetworkText renders upload/download rate text at x.
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

        // A removed applet has nothing left to show a snapshot on.
        this._cancelApply();
        this.pendingSnapshot = null;
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
