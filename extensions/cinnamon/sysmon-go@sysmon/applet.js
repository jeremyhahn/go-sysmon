// Cinnamon panel applet for go-sysmon.
// Connects to the sysmon WebSocket server and renders live system gauges
// using Cairo on a St.DrawingArea embedded in the panel.

const Applet = imports.ui.applet;
const St = imports.gi.St;
const Mainloop = imports.mainloop;
const Soup = imports.gi.Soup;
const GLib = imports.gi.GLib;
const Lang = imports.lang;
const Settings = imports.ui.settings;

// Color constants for gauge bars (RGBA).
const COLOR_GREEN  = [0.21, 0.83, 0.60, 1.0];
const COLOR_YELLOW = [0.98, 0.74, 0.14, 1.0];
const COLOR_RED    = [0.97, 0.45, 0.45, 1.0];
const COLOR_BG     = [0.30, 0.30, 0.30, 0.3];
const COLOR_TEXT   = [0.80, 0.80, 0.80, 1.0];

// Layout constants (pixels).
const BAR_WIDTH     = 3;
const BAR_GAP       = 1;
const SECTION_GAP   = 6;
const MEM_BAR_WIDTH = 8;
const DISK_BAR_WIDTH = 8;
const NET_TEXT_WIDTH = 55;
const MIN_WIDTH     = 20;
const FONT_SIZE     = 9;
const PADDING       = 2;

// Reconnect delay in milliseconds.
const RECONNECT_DELAY_MS = 3000;

/**
 * _usageColor returns the appropriate RGBA color array for a usage fraction.
 * Green below 50%, yellow between 50-80%, red above 80%.
 */
function _usageColor(fraction) {
    if (fraction < 0.5) {
        return COLOR_GREEN;
    }
    if (fraction < 0.8) {
        return COLOR_YELLOW;
    }
    return COLOR_RED;
}

/**
 * _formatRate converts a bytes-per-second rate to a compact string.
 */
function _formatRate(bytesPerSec) {
    if (bytesPerSec >= 1073741824) {
        return (bytesPerSec / 1073741824).toFixed(1) + "G";
    }
    if (bytesPerSec >= 1048576) {
        return (bytesPerSec / 1048576).toFixed(1) + "M";
    }
    if (bytesPerSec >= 1024) {
        return (bytesPerSec / 1024).toFixed(0) + "K";
    }
    return bytesPerSec.toFixed(0) + "B";
}

class SysmonApplet extends Applet.Applet {

    _init(metadata, orientation, panelHeight, instanceId) {
        super._init(orientation, panelHeight, instanceId);

        this.metadata = metadata;
        this.settings = new Settings.AppletSettings(this, metadata.uuid, instanceId);

        // Bind user-configurable settings to instance properties.
        this.settings.bind("server-address", "serverAddress");
        this.settings.bind("update-interval", "updateInterval");
        this.settings.bind("show-cpu", "showCpu");
        this.settings.bind("show-memory", "showMemory");
        this.settings.bind("show-network", "showNetwork");
        this.settings.bind("show-disk", "showDisk");

        // Cairo drawing area for gauge rendering.
        this.drawingArea = new St.DrawingArea();
        this.drawingArea.connect("repaint", Lang.bind(this, this._onRepaint));
        this.actor.add_actor(this.drawingArea);

        // Runtime state.
        this.snapshot = null;
        this.wsConnection = null;
        this.reconnectTimer = null;

        this.set_applet_tooltip("System Monitor - Connecting...");
        this._connectWebSocket();
    }

    /**
     * _connectWebSocket opens a WebSocket connection to the sysmon server.
     * On success it wires up the message and closed handlers. On failure
     * it schedules a reconnect attempt.
     */
    _connectWebSocket() {
        try {
            let session = new Soup.Session();
            let uri = GLib.Uri.parse(
                "ws://" + this.serverAddress + "/ws",
                GLib.UriFlags.NONE
            );
            let msg = new Soup.Message({ method: "GET", uri: uri });

            session.websocket_connect_async(
                msg,
                null,  // origin
                null,  // protocols
                GLib.PRIORITY_DEFAULT,
                null,  // cancellable
                Lang.bind(this, function (sess, result) {
                    try {
                        this.wsConnection = sess.websocket_connect_finish(result);
                        this.wsConnection.connect(
                            "message",
                            Lang.bind(this, this._onMessage)
                        );
                        this.wsConnection.connect(
                            "closed",
                            Lang.bind(this, this._onClosed)
                        );
                        this.set_applet_tooltip("System Monitor - Connected");

                        // Send the configured interval to the server.
                        this._sendInterval();
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
     * _sendInterval sends a set_interval command to the server so the
     * stream rate matches the user's configured update interval.
     */
    _sendInterval() {
        if (!this.wsConnection) {
            return;
        }
        let cmd = JSON.stringify({
            type: "set_interval",
            interval_ms: this.updateInterval
        });
        this.wsConnection.send_text(cmd);
    }

    /**
     * _onMessage handles an incoming WebSocket text frame containing a
     * JSON snapshot. The snapshot is stored and the drawing area is
     * queued for repaint.
     */
    _onMessage(connection, type, data) {
        if (type !== Soup.WebsocketDataType.TEXT) {
            return;
        }
        try {
            let text = data.get_data().toString();
            this.snapshot = JSON.parse(text);
            this.drawingArea.queue_repaint();
            this._updateTooltip();
        } catch (e) {
            // Malformed message; skip.
        }
    }

    /**
     * _onClosed handles WebSocket disconnection and triggers reconnect.
     */
    _onClosed() {
        this.wsConnection = null;
        this.set_applet_tooltip("System Monitor - Disconnected");
        this._scheduleReconnect();
    }

    /**
     * _scheduleReconnect queues a reconnect attempt after a delay.
     * Only one pending reconnect timer is allowed at a time.
     */
    _scheduleReconnect() {
        if (this.reconnectTimer) {
            return;
        }
        this.reconnectTimer = Mainloop.timeout_add(
            RECONNECT_DELAY_MS,
            Lang.bind(this, function () {
                this.reconnectTimer = null;
                this._connectWebSocket();
                return false; // do not repeat
            })
        );
    }

    /**
     * _updateTooltip sets the applet tooltip to a summary of the latest
     * snapshot data.
     */
    _updateTooltip() {
        if (!this.snapshot) {
            return;
        }
        let cpus = this.snapshot.cpus || [];
        let mem = this.snapshot.memory || {};
        let avgCpu = 0;
        if (cpus.length > 0) {
            let sum = 0;
            for (let i = 0; i < cpus.length; i++) {
                sum += cpus[i].usage_percent || 0;
            }
            avgCpu = sum / cpus.length;
        }
        this.set_applet_tooltip(
            "CPU: " + avgCpu.toFixed(1) + "% | RAM: " +
            (mem.used_percent || 0).toFixed(1) + "%"
        );
    }

    /**
     * _onRepaint renders the gauge bars and network text onto the Cairo
     * context provided by the St.DrawingArea.
     */
    _onRepaint(area) {
        let ctx = area.get_context();
        let [w, h] = area.get_surface_size();
        let snap = this.snapshot;

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
            let totalBytes = 0;
            let usedBytes = 0;
            for (let i = 0; i < snap.disks.length; i++) {
                totalBytes += snap.disks[i].total_bytes || 0;
                usedBytes += snap.disks[i].used_bytes || 0;
            }
            let diskPct = totalBytes > 0 ? usedBytes / totalBytes : 0;
            x = this._drawUsageBar(ctx, diskPct, x, h, DISK_BAR_WIDTH);
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
            let pct = (cpus[i].usage_percent || 0) / 100;
            let barH = Math.max(1, Math.floor(h * pct));
            let color = _usageColor(pct);

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
        let barH = Math.max(1, Math.floor(h * fraction));
        let color = _usageColor(fraction);

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
        let totalSent = 0;
        let totalRecv = 0;
        for (let i = 0; i < networks.length; i++) {
            totalSent += networks[i].bytes_sent_rate || 0;
            totalRecv += networks[i].bytes_recv_rate || 0;
        }

        ctx.setSourceRGBA(COLOR_TEXT[0], COLOR_TEXT[1], COLOR_TEXT[2], COLOR_TEXT[3]);
        ctx.selectFontFace("monospace", 0, 0);
        ctx.setFontSize(FONT_SIZE);

        let upStr = "\u2191" + _formatRate(totalSent);
        let dnStr = "\u2193" + _formatRate(totalRecv);

        ctx.moveTo(x, h * 0.4);
        ctx.showText(upStr);
        ctx.moveTo(x, h * 0.85);
        ctx.showText(dnStr);

        return x + NET_TEXT_WIDTH;
    }

    /**
     * on_applet_clicked launches the sysmon GUI application.
     */
    on_applet_clicked(event) {
        GLib.spawn_command_line_async("sysmon");
    }

    /**
     * on_applet_removed_from_panel cleans up timers and the WebSocket
     * connection when the applet is removed from the panel.
     */
    on_applet_removed_from_panel() {
        if (this.reconnectTimer) {
            Mainloop.source_remove(this.reconnectTimer);
            this.reconnectTimer = null;
        }
        if (this.wsConnection) {
            this.wsConnection.close(
                Soup.WebsocketCloseCode.NORMAL,
                "applet removed"
            );
            this.wsConnection = null;
        }
    }
}

/**
 * main is the Cinnamon applet entry point.
 */
function main(metadata, orientation, panelHeight, instanceId) {
    return new SysmonApplet(metadata, orientation, panelHeight, instanceId);
}
