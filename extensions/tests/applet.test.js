// Load-and-drive tests for the Cinnamon applet.
//
// applet.js is GObject Introspection glue, so its failure mode is a typo or a
// misused API that only appears when Cinnamon loads it. These tests execute the
// real file against a stand-in platform, which catches that class of bug
// without a desktop session.

const test = require("node:test");
const assert = require("node:assert");

const { loadApplet, eventLines, CLUTTER_PRIORITY_REDRAW } = require("./harness/cinnamon.js");
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

// sectionItems lists the popup's section submenus.
function sectionItems(applet) {
    return applet.menu.items.filter((i) => i.menu !== undefined);
}

// sectionRows lists the rows of the section whose title starts with title.
function sectionRows(applet, title) {
    return sectionItems(applet).find((i) => i.label.get_text().startsWith(title)).menu.items;
}

// withHostname is the fixture as a later snapshot that renamed the host.
function withHostname(name) {
    return Object.assign({}, fixture, { host: Object.assign({}, fixture.host, { hostname: name }) });
}

// pushSnapshot delivers a snapshot on the open stream and applies it.
function pushSnapshot(harness, snapshot) {
    for (const line of eventLines(snapshot)) {
        harness.streams[0].push(line);
    }
    assert.strictEqual(harness.runIdle(), 1, "the snapshot was not applied");
}

test("a snapshot event populates the applet and, once opened, the menu", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture) });

    assert.ok(applet.snapshot, "no snapshot was stored");
    assert.strictEqual(applet.snapshot.host.hostname, fixture.host.hostname);

    applet.on_applet_clicked({});

    // Every section should now have a submenu in the popup.
    const submenus = sectionItems(applet);
    assert.strictEqual(submenus.length, 10, "expected one submenu per section");
    assert.ok(submenus[0].label.get_text().startsWith("Host"));
});

test("the submenus are filled with rows, not left empty", () => {
    const { applet } = loadApplet({ lines: eventLines(fixture) });
    applet.on_applet_clicked({});

    const submenus = sectionItems(applet);
    assert.strictEqual(submenus.length, 10);
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
    harness.runIdle();
    applet.on_applet_clicked({});

    const before = applet.menu.items.length;
    pushSnapshot(harness, fixture);

    assert.strictEqual(applet.menu.items.length, before);
});

// Issue #233: every snapshot rebuilt every row of every submenu, popup open or
// not -- tens of milliseconds of the compositor thread per snapshot. These
// fail on that code.
test("a closed popup is not filled on each snapshot", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    pushSnapshot(harness, withHostname("second"));

    assert.strictEqual(sectionItems(applet).length, 0, "a closed popup was filled");
});

test("opening the popup fills it with the latest snapshot before it is laid out", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    pushSnapshot(harness, withHostname("latest"));

    applet.on_applet_clicked({});

    assert.strictEqual(applet.menu.isOpen, true);
    assert.strictEqual(
        applet.menu.itemsAtOpen.filter((i) => i.menu !== undefined).length, 10,
        "the popup opened before its sections were filled");
    assert.ok(
        sectionRows(applet, "Host").some((r) => r.label.get_text() === "Hostname:  latest"),
        "the popup does not show the latest snapshot");
});

test("an open popup follows each snapshot", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    applet.on_applet_clicked({});

    pushSnapshot(harness, withHostname("renamed"));

    assert.ok(sectionRows(applet, "Host").some((r) => r.label.get_text() === "Hostname:  renamed"));
});

test("an open popup updates its rows in place and leaves unchanged ones alone", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    applet.on_applet_clicked({});
    const before = sectionRows(applet, "Host").slice();
    const setsBefore = before.map((r) => r.label.sets);

    pushSnapshot(harness, withHostname("renamed"));

    const after = sectionRows(applet, "Host");
    assert.strictEqual(after.length, before.length);
    after.forEach((row, i) => assert.strictEqual(row, before[i], `row ${i} was replaced`));
    const changed = after.filter((row, i) => row.label.sets !== setsBefore[i]);
    assert.deepStrictEqual(changed.map((r) => r.label.get_text()), ["Hostname:  renamed"]);
});

test("rows are added and removed as a section grows and shrinks, in order", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    applet.on_applet_clicked({});
    const full = sectionRows(applet, "CPU").map((r) => r.label.get_text());

    pushSnapshot(harness, Object.assign({}, fixture, { cpus: fixture.cpus.slice(0, 1) }));
    const shrunk = sectionRows(applet, "CPU").map((r) => r.label.get_text());
    pushSnapshot(harness, fixture);
    const regrown = sectionRows(applet, "CPU");

    assert.ok(shrunk.length < full.length, "the section did not shrink");
    assert.deepStrictEqual(regrown.map((r) => r.label.get_text()), full);
    assert.ok(regrown.every((r) => !r.destroyed), "a destroyed row is still listed");
    const headers = regrown.filter((r) => r.actor.styleClasses.includes("popup-subtitle-menu-item"));
    assert.deepStrictEqual(headers.map((r) => r.label.get_text()),
        full.filter((t) => t === "Per-core" || t.startsWith("Flags")));
});

test("reopening without a new snapshot does not refill the popup", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    applet.on_applet_clicked({});
    applet.on_applet_clicked({});
    const sets = sectionRows(applet, "Host").map((r) => r.label.sets);

    applet.on_applet_clicked({});

    assert.deepStrictEqual(sectionRows(applet, "Host").map((r) => r.label.sets), sets);
});

test("the applet queues a repaint when a snapshot arrives", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();

    assert.ok(applet.drawingArea.repaints > 0, "no repaint was queued");
});

test("the tooltip summarises the snapshot", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();

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

// Issue #184: Clutter never paints a zero-sized actor, so a gauge that sized
// itself only from inside its repaint handler was never drawn at all. The
// harness paints nothing at zero size, as Clutter does; these fail on that
// code.
test("the gauge has a size before its first paint, so it can be painted", () => {
    const { applet } = loadApplet();

    assert.ok(applet.drawingArea.width > 0, "the gauge starts with no width");
    assert.ok(applet.drawingArea.height > 0, "the gauge starts with no height");
    assert.notStrictEqual(applet.drawingArea.repaint(), null, "Clutter would never paint the gauge");
    assert.strictEqual(applet.drawingArea.paints, 1);
});

test("a snapshot sizes the gauge to fit it before it is painted", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();

    // Bars, memory, network text and disk: wider than the empty placeholder,
    // and settled without any paint having happened.
    assert.strictEqual(applet.drawingArea.paints, 0);
    assert.ok(applet.drawingArea.width > 20 + fixture.cpus.length * 4,
        `width ${applet.drawingArea.width} does not fit ${fixture.cpus.length} cores`);
});

test("hiding a gauge narrows the area before the next paint", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();
    const before = applet.drawingArea.width;

    applet.showCpu = false;
    harness.settingsBindings["show-cpu"].callback();

    assert.strictEqual(applet.drawingArea.width, before - fixture.cpus.length * 4 - 6);
    assert.strictEqual(applet.drawingArea.paints, 0);
});

test("the drawing fits inside the width the gauge was given", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    harness.runIdle();

    const ctx = applet.drawingArea.repaint();
    const rightEdges = ctx.ops
        .filter((op) => op[0] === "rectangle")
        .map((op) => op[1] + op[3]);

    assert.ok(Math.max(...rightEdges) <= applet.drawingArea.width,
        `a bar ends at ${Math.max(...rightEdges)}, past the width ${applet.drawingArea.width}`);
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
    harness.runIdle();

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
    harness.runIdle();

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

    assert.deepStrictEqual(harness.removedSources, [timerId]);
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

// Issue #232: the applet applied each snapshot -- tooltip, gauge and a rebuilt
// popup -- inside the stream's read callback, at the default priority. Clutter
// paints at a lower one (CLUTTER_PRIORITY_REDRAW), so a host that took longer
// than the interval to apply a snapshot kept Cinnamon's main loop busy with
// nothing else, and no frame was painted for as long as that lasted. These
// fail on that code.
test("a snapshot is applied from an idle source below Clutter's redraw, not in the stream callback", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });

    assert.strictEqual(applet.snapshot, null, "the snapshot was applied inside the stream callback");
    assert.strictEqual(applet.drawingArea.repaints, 0);
    assert.strictEqual(harness.idles.length, 1, "no idle source was scheduled to apply the snapshot");
    assert.ok(harness.idles[0].priority > CLUTTER_PRIORITY_REDRAW,
        `the snapshot is applied at priority ${harness.idles[0].priority}, which pre-empts Clutter's redraw`);

    harness.runIdle();

    assert.strictEqual(applet.snapshot.host.hostname, fixture.host.hostname);
    assert.ok(applet.drawingArea.repaints > 0, "applying the snapshot queued no repaint");
    assert.match(applet.tooltip, /CPU: /);
});

test("snapshots that arrive faster than they are applied are coalesced to the newest", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    const newer = Object.assign({}, fixture, { host: Object.assign({}, fixture.host, { hostname: "newer" }) });

    for (const line of eventLines(newer)) {
        harness.streams[0].push(line);
    }

    assert.strictEqual(harness.idles.length, 1, "each snapshot queued its own application");
    assert.strictEqual(harness.runIdle(), 1);
    assert.strictEqual(applet.snapshot.host.hostname, "newer");
});

test("a stream that ends shows its last snapshot, then says it disconnected", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture) });

    assert.strictEqual(applet.snapshot.host.hostname, fixture.host.hostname);
    assert.strictEqual(harness.idles.length, 0, "the ended stream left an application pending");
    assert.match(applet.tooltip, /Disconnected/);
});

test("a bye shows the last snapshot, then says the server stopped", () => {
    const { applet, harness } = loadApplet({
        lines: eventLines(fixture).concat(["event: bye", "data: monitor stopped", ""]),
    });

    assert.strictEqual(applet.snapshot.host.hostname, fixture.host.hostname);
    assert.strictEqual(harness.idles.length, 0, "the ended stream left an application pending");
    assert.match(applet.tooltip, /stopped/i);
});

test("removal from the panel drops a snapshot not yet applied", () => {
    const { applet, harness } = loadApplet({ lines: eventLines(fixture), keepOpen: true });
    const applyId = harness.idles[0].id;

    applet.on_applet_removed_from_panel();

    assert.ok(harness.removedSources.includes(applyId), "the idle source outlived the applet");
    assert.strictEqual(harness.idles.length, 0);
    assert.strictEqual(applet.snapshot, null, "a removed applet applied a snapshot");
});
