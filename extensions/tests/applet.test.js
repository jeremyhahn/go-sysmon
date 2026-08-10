// Load-and-drive tests for the Cinnamon applet.
//
// applet.js is GObject Introspection glue, so its failure mode is a typo or a
// misused API that only appears when Cinnamon loads it. These tests execute the
// real file against a stand-in platform, which catches that class of bug
// without a desktop session.

const test = require("node:test");
const assert = require("node:assert");

const { loadApplet, eventLines } = require("./harness/cinnamon.js");
// The web dashboard's Playwright fixture is the single source of truth for a
// representative snapshot; sharing it keeps the two suites from drifting.
const fixture = require("../../cmd/sysmon/frontend/tests/fixtures/snapshot.json");

test("the applet loads and constructs", () => {
    const { applet } = loadApplet();

    assert.ok(applet, "main() returned nothing");
    assert.ok(applet.menu, "no popup menu was built");
    assert.ok(applet.drawingArea, "no drawing area was created");
});

test("the applet connects to the event stream on construction", () => {
    const { harness } = loadApplet();

    const events = harness.requests.filter((r) => r.uri.indexOf("/api/events") !== -1);
    assert.strictEqual(events.length, 1, "expected exactly one stream request");
    assert.strictEqual(events[0].method, "GET");
    assert.strictEqual(events[0].uri, "http://localhost:8080/api/events");
    assert.strictEqual(events[0].headers["Accept"], "text/event-stream");
});

test("the applet pushes its configured interval over REST", () => {
    const { harness } = loadApplet();

    const posts = harness.requests.filter((r) => r.uri.indexOf("/api/interval") !== -1);
    assert.strictEqual(posts.length, 1, "expected one interval request");
    assert.strictEqual(posts[0].method, "POST");
    assert.deepStrictEqual(
        JSON.parse(posts[0].body.bytes.data),
        { interval_ms: 1000 }
    );
});

test("a snapshot event populates the applet and refreshes the menu", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture) });

    assert.ok(applet.snapshot, "no snapshot was stored");
    assert.strictEqual(applet.snapshot.host.hostname, fixture.host.hostname);

    // Every section should now have a submenu in the popup.
    const submenus = applet.menu.items.filter((i) => i.menu !== undefined);
    assert.strictEqual(submenus.length, 9, "expected one submenu per section");
    assert.ok(submenus[0].label.get_text().startsWith("Host"));
});

test("the submenus are filled with rows, not left empty", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture) });

    const submenus = applet.menu.items.filter((i) => i.menu !== undefined);
    for (const submenu of submenus) {
        assert.ok(
            submenu.menu.items.length > 0,
            `submenu ${submenu.label.get_text()} has no rows`
        );
    }
});

test("a second snapshot refreshes the menu without duplicating sections", () => {
    // A menu that grows a fresh set of submenus per snapshot is unusable
    // within a minute of running.
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    const before = applet.menu.items.length;
    for (const line of eventLines(fixture)) {
        harness.streams[0].push(line);
    }

    assert.strictEqual(applet.menu.items.length, before);
});

test("the applet queues a repaint when a snapshot arrives", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    assert.ok(applet.drawingArea.repaints > 0, "no repaint was queued");
});

test("the tooltip summarises the snapshot", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    assert.match(applet.tooltip, /CPU: /);
    assert.match(applet.tooltip, /RAM: /);
    assert.ok(!applet.tooltip.includes("undefined"), applet.tooltip);
});

test("repainting with a snapshot draws gauges", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture) });

    const ctx = applet.drawingArea.repaint();
    const fills = ctx.ops.filter((op) => op[0] === "fill");

    // One filled bar plus one background bar per core, plus memory and disk.
    assert.ok(fills.length >= fixture.cpus.length * 2, "too few gauge fills");
    assert.ok(
        ctx.ops.some((op) => op[0] === "showText"),
        "network rates were not drawn"
    );
    assert.ok(applet.drawingArea.width > 0, "the drawing area was not sized");
});

test("repainting before any snapshot draws a placeholder and does not throw", () => {
    const { applet } = loadApplet();

    const ctx = applet.drawingArea.repaint();

    assert.ok(ctx.ops.length > 0, "nothing was drawn");
    assert.strictEqual(applet.drawingArea.repaints, 0, "a repaint was queued with no data");
});

test("malformed event data is ignored rather than crashing the applet", () => {
    const { applet } = loadApplet({
        lines: ["event: snapshot", "data: {not json", ""],
    });

    assert.strictEqual(applet.snapshot, null, "malformed data was stored as a snapshot");
});

test("a bye event stops the stream and schedules a retry", () => {
    const { applet, harness } = loadApplet({
        lines: ["event: bye", "data: monitor stopped", ""],
    });

    assert.match(applet.tooltip, /stopped/i);
    assert.ok(harness.timeouts.length > 0, "no reconnect was scheduled after bye");
});

test("end of stream schedules a reconnect", () => {
    // An empty script means the first read returns end-of-stream.
    const { applet, harness } = loadApplet({ lines: [] });

    assert.match(applet.tooltip, /Disconnected/);
    assert.strictEqual(harness.timeouts.length, 1, "expected one reconnect timer");
});

test("the reconnect timer reopens the stream and does not repeat", () => {
    const { harness } = loadApplet({ lines: [] });

    const before = harness.requests.filter((r) => r.uri.indexOf("/api/events") !== -1).length;
    const repeat = harness.timeouts[0].fn();

    const after = harness.requests.filter((r) => r.uri.indexOf("/api/events") !== -1).length;
    assert.strictEqual(after, before + 1, "the reconnect did not reopen the stream");
    assert.strictEqual(repeat, false, "the reconnect timer asked to repeat");
});

test("reconnect backoff grows rather than hammering a dead server", () => {
    // The server accepts the connection and closes it straight away. Resetting
    // the backoff on connect alone would retry every three seconds forever.
    const { harness } = loadApplet({ lines: [] });

    const delays = [];
    for (let i = 0; i < 4; i++) {
        const timer = harness.timeouts[harness.timeouts.length - 1];
        delays.push(timer.ms);
        timer.fn();
    }

    assert.ok(
        delays[delays.length - 1] > delays[0],
        `backoff did not grow: ${delays.join(", ")}`
    );
});

test("a non-200 response schedules a reconnect instead of reading a body", () => {
    const { harness } = loadApplet({ status: 503, lines: [] });

    assert.ok(harness.timeouts.length > 0, "no reconnect after an error status");
});

test("a failed connection schedules a reconnect", () => {
    const { harness } = loadApplet({ connectFails: true });

    assert.ok(harness.timeouts.length > 0, "no reconnect after a failed connect");
});

test("a failed send_finish schedules a reconnect", () => {
    const { harness } = loadApplet({ sendFinishFails: true });

    assert.ok(harness.timeouts.length > 0, "no reconnect after a failed handshake");
});

test("changing the interval setting pushes the new rate", () => {
    const { applet, harness } = loadApplet();

    const before = harness.requests.filter((r) => r.uri.indexOf("/api/interval") !== -1).length;
    applet.updateInterval = 5000;
    harness.settingsBindings["update-interval"].callback();

    const posts = harness.requests.filter((r) => r.uri.indexOf("/api/interval") !== -1);
    assert.strictEqual(posts.length, before + 1);
    assert.deepStrictEqual(
        JSON.parse(posts[posts.length - 1].body.bytes.data),
        { interval_ms: 5000 }
    );
});

test("changing the server address reconnects to the new host", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    applet.serverAddress = "10.0.0.5:9090";
    harness.settingsBindings["server-address"].callback();

    const streams = harness.requests.filter((r) => r.uri.indexOf("/api/events") !== -1);
    assert.strictEqual(streams[streams.length - 1].uri, "http://10.0.0.5:9090/api/events");
});

test("toggling a gauge setting only queues a repaint", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    const before = applet.drawingArea.repaints;
    const streamsBefore = harness.requests.filter((r) => r.uri.indexOf("/api/events") !== -1).length;

    applet.showCpu = false;
    harness.settingsBindings["show-cpu"].callback();

    assert.strictEqual(applet.drawingArea.repaints, before + 1);
    assert.strictEqual(
        harness.requests.filter((r) => r.uri.indexOf("/api/events") !== -1).length,
        streamsBefore,
        "toggling a gauge reconnected the stream"
    );
});

test("hiding every gauge still produces a drawable panel", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    for (const key of ["show-cpu", "show-memory", "show-network", "show-disk"]) {
        applet[harness.settingsBindings[key].property] = false;
    }
    applet.drawingArea.repaint();

    assert.ok(applet.drawingArea.width >= 20, "the panel collapsed to nothing");
});

test("clicking the applet toggles the menu", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    assert.strictEqual(applet.menu.isOpen, false);
    applet.on_applet_clicked({});
    assert.strictEqual(applet.menu.isOpen, true);
});

test("the dashboard item launches the GUI", () => {
    const { applet, harness } = loadApplet();

    const item = applet.menu.items.filter(
        (i) => i.label && i.label.get_text() === "Open full dashboard"
    )[0];
    assert.ok(item, "no dashboard item in the menu");

    item.activate();
    assert.deepStrictEqual(harness.spawned, ["sysmon"]);
});

test("removal from the panel cancels timers and closes the stream", () => {
    // A stream left open keeps a subscription alive on the server, and a live
    // timer resurrects an applet the user removed.
    const { applet, harness } = loadApplet({ lines: [] });

    const timerId = harness.timeouts[0].id;
    applet.on_applet_removed_from_panel();

    assert.deepStrictEqual(harness.removedTimeouts, [timerId]);
    assert.ok(harness.streams[0].closed, "the stream was left open");
    assert.strictEqual(applet.settings, null, "settings were not finalised");
});

test("no reconnect is scheduled after removal", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    applet.on_applet_removed_from_panel();
    const before = harness.timeouts.length;

    // Anything still in flight must not restart the applet.
    harness.streams[0].push(null);

    assert.strictEqual(harness.timeouts.length, before, "a reconnect was scheduled after removal");
});

test("receiving data resets the backoff so a healthy server reconnects quickly", () => {
    // A stream that ran normally and then dropped should come back fast; only
    // a server that never delivers anything earns a growing delay.
    const { harness } = loadApplet({ lines: eventLines(fixture) });

    assert.strictEqual(harness.timeouts[0].ms, 3000);
});
