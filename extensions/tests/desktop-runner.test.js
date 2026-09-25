// Tests for the tier 3 runner, test/desktop/run-cinnamon-tests.sh.
//
// The runner is itself a gate, and a gate can be wrong in the direction that
// hurts most: reporting passes about a desktop that never ran. These tests run
// the real script against stand-in executables for the desktop stack (Xorg,
// xrandr, dbus-daemon, udisksd, dbus-launch, gsettings, cinnamon, gdbus) and
// the server, so each startup
// shape -- healthy, slow, broken -- can be produced on demand instead of
// waiting for CI to hit it by chance.
//
// The stand-in gdbus answers every applet assertion the way a healthy applet
// would. That is deliberate: in the failure scenarios, any "ok" line after the
// session gate would be a pass the runner granted without evidence.

const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const RUNNER = path.join(__dirname, "..", "..", "test", "desktop", "run-cinnamon-tests.sh");

// What mutter logged on every run while the display was Xvfb, whose only mode
// has no timings.
const NAN_MODE_WARNING =
    "(cinnamon:56): mutter-WARNING **: Failed to use linear monitor configuration: Invalid mode 1280x1024 (-nan) for monitor 'unknown unknown'";

const LOADED_LOG = [
    "Gjs-Message: JS LOG: [LookingGlass/info] Loaded applet sysmon-go@sysmon in 11 ms",
    "Gjs-Message: JS LOG: [LookingGlass/info] Cinnamon took 87 ms to start",
].join("\n");

// The stand-in executables. Each reads its behaviour from FAKE_* variables so
// one set of files serves every scenario. Long-lived ones exec sleep with their
// output detached, as the real daemons would, and are killed by the runner's
// cleanup trap.
const FAKES = {
    "sysmon-server": `#!/bin/sh
exec sleep 60 </dev/null >/dev/null 2>&1
`,
    // curl answers GET /api/interval with the interval the server runs at:
    // the applet's 500 ms once its push has landed, or the schema default's
    // 1000 ms when the applet never read its settings file.
    curl: `#!/bin/sh
[ "$FAKE_SERVER" = "down" ] && exit 7
for url; do :; done
case "$url" in
    */api/interval) printf '{"interval_ms":%s,"fleet_interval_ms":0}\n' "$FAKE_SERVER_INTERVAL" ;;
esac
exit 0
`,
    Xorg: `#!/bin/sh
[ "$FAKE_XORG" = "dies" ] && exit 1
fd=""
while [ $# -gt 0 ]; do
    [ "$1" = "-displayfd" ] && fd="$2"
    [ "$1" = "-logfile" ] && echo "X.Org X Server 1.21.1" > "$2"
    shift
done
eval "printf '99\\n' >&$fd"
exec sleep 60 </dev/null >/dev/null 2>&1
`,
    // xrandr --verbose, trimmed to the lines the runner reads. FAKE_REFRESH is
    // the v: clock of the current mode; Xvfb reports 0.00Hz.
    xrandr: `#!/bin/sh
cat <<EOF
screen connected 1280x1024+0+0 (0x3a) normal (normal) 0mm x 0mm
  1280x1024 (0x5e) 108.000MHz +HSync +VSync *current +preferred
        h: width  1280 start 1328 end 1440 total 1688 skew    0 clock  63.98KHz
        v: height 1024 start 1025 end 1028 total 1066           clock  $FAKE_REFRESH
EOF
`,
    "dbus-daemon": `#!/bin/sh
[ "$FAKE_SYSTEM_BUS" = "fails" ] && { echo "Failed to start message bus" >&2; exit 1; }
[ "$FAKE_SYSTEM_BUS" = "complains" ] && echo 'dbus[1]: Unknown username "whoopsie" in message bus configuration file' >&2
sleep 60 </dev/null >/dev/null 2>&1 &
echo $!
`,
    udisksd: `#!/bin/sh
exec sleep 60 </dev/null >/dev/null 2>&1
`,
    "dbus-launch": `#!/bin/sh
sleep 60 </dev/null >/dev/null 2>&1 &
echo "DBUS_SESSION_BUS_ADDRESS='unix:path=$FAKE_DIR/bus'; export DBUS_SESSION_BUS_ADDRESS;"
echo "DBUS_SESSION_BUS_PID=$!;"
`,
    gsettings: `#!/bin/sh
[ "$FAKE_GSETTINGS" = "fails" ] && exit 1
exit 0
`,
    // Real Cinnamon has written the startup lines -- the applet loading, any
    // display-mode warning -- before it sets runState to RUNNING. The stand-in
    // keeps that order as an event rather than a race: once its log is
    // written it says so on a FIFO, and gdbus reads that before it first
    // answers RUNNING. Without it the runner, started in the background, could
    // be told RUNNING and scan the log before cat had written it -- which a
    // loaded CI runner did.
    cinnamon: `#!/bin/sh
cat "$FAKE_DIR/cinnamon.log.src"
echo written > "$FAKE_DIR/log-written"
exec sleep 60 </dev/null >/dev/null 2>&1
`,
    // gdbus models the order Cinnamon's main.js starts in: the panel exists
    // from the first answer, runState reaches RUNNING only after
    // FAKE_RUNNING_AFTER polls (or never), and the applet is in the loaded set
    // only once it has -- AppletManager.init() resolves before RUNNING is set.
    gdbus: `#!/bin/sh
for js; do :; done
if [ "$2" = "--system" ]; then
    # udisks2 is on the bus unless the scenario removes it; switcheroo-control
    # is absent unless the scenario puts it there.
    case "$js" in
        "$FAKE_MISSING_SERVICE") echo "(false,)" ;;
        net.hadess.SwitcherooControl) [ "$FAKE_SWITCHEROO" = "running" ] && echo "(true,)" || echo "(false,)" ;;
        *) echo "(true,)" ;;
    esac
    exit 0
fi
count_file="$FAKE_DIR/runstate-polls"
polls=$(cat "$count_file" 2>/dev/null || echo 0)
running=false
if [ "$FAKE_RUNNING_AFTER" != "never" ] && [ "$polls" -ge "$FAKE_RUNNING_AFTER" ]; then
    running=true
    # The first RUNNING waits for the log to be written; see the cinnamon
    # stand-in.
    if [ ! -e "$FAKE_DIR/log-synced" ]; then
        read -r _ < "$FAKE_DIR/log-written"
        : > "$FAKE_DIR/log-synced"
    fi
fi
reply() { printf "(true, '%s')\\n" "$1"; exit 0; }
case "$js" in
    "1+1") reply 2 ;;
    # The popup checks (#233). FAKE_POPUP=rebuilt is an applet that refills
    # the popup while it is closed; stale is one that opens before it
    # catches up.
    *"stale when shown: "*)
        [ "$FAKE_POPUP" = "stale" ] && reply "open: true, stale when shown: true"
        reply "open: true, stale when shown: false" ;;
    *"return 'open: ' + applet.menu.isOpen; })()") reply "open: true" ;;
    *"return 'closed: ' + !applet.menu.isOpen; })()") reply "closed: true" ;;
    *"return 'watching'; })()") reply watching ;;
    *"return 'marked'; })()") reply marked ;;
    *"', text changes: '"*)
        [ "$FAKE_POPUP" = "rebuilt" ] && reply "snapshots applied: 2, rows: 294, replaced: 294, text changes: 287"
        reply "snapshots applied: 2, rows: 295, replaced: 0, text changes: 0" ;;
    *"'interval: ' + applet.updateInterval"*)
        [ "$FAKE_SERVER_INTERVAL" = "500" ] && reply "interval: 500 ms, server: localhost:8080"
        reply "interval: $FAKE_SERVER_INTERVAL ms, server: localhost:8080" ;;
    *"_sysmonTestTextChanges === 0"*) [ "$FAKE_POPUP" = "rebuilt" ] && reply false; reply true ;;
    *"runState === "*)
        echo $((polls + 1)) > "$count_file"
        reply "$running" ;;
    "String(imports.ui.main.runState)") reply 1 ;;
    *"panelManager"*"> 0)") [ "$FAKE_PANEL" = "down" ] && reply false; reply true ;;
    *".some(function (a)"*)
        if [ "$FAKE_APPLET" = "missing" ] || [ "$running" = "false" ]; then reply false; fi
        reply true ;;
    *"xlet_startup_error"*) [ "$FAKE_APPLET" = "missing" ] && reply true; reply false ;;
    *"i.menu !== undefined; }).length)")
        [ -n "$FAKE_LATE_LOG" ] && printf '%s\\n' "$FAKE_LATE_LOG" >> "$CINNAMON_LOG"
        reply 10 ;;
    *"indexOf('undefined')"*) reply false ;;
    *"get_text()") reply "CPU: 12%" ;;
    *"_sysmonTestSlowApplies = 0"*) reply slowed ;;
    *"_sysmonTestSlowApplies >= "*)
        case "$FAKE_GAUGE" in zero-width|starved) reply false ;; esac
        reply true ;;
    *"queue_repaint"*) reply queued ;;
    *"get_allocation_box"*"String("*) [ "$FAKE_GAUGE" = "zero-width" ] && reply false; reply true ;;
    *"get_allocation_box"*) [ "$FAKE_GAUGE" = "zero-width" ] && reply 0x40; reply 60x40 ;;
    *"_sysmonTestPaints > 0)")
        [ -n "$FAKE_PAINT_ERROR" ] && printf '%s\\n' "$FAKE_PAINT_ERROR" >> "$CINNAMON_LOG"
        [ "$FAKE_GAUGE" = "zero-width" ] && reply false; reply true ;;
    *"stage frames"*)
        [ "$FAKE_GAUGE" = "zero-width" ] && reply "paints: 0, stage frames: 1, mapped: true, visible: true"
        reply "paints: 1, stage frames: 1, mapped: true, visible: true" ;;
    *) reply true ;;
esac
`,
};

// runRunner executes the real runner against the stand-ins with the given
// scenario and returns its exit status and the lines it reported.
function runRunner(scenario = {}) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "sysmon-desktop-runner-"));
    try {
        const bin = path.join(dir, "bin");
        fs.mkdirSync(bin);
        // The masked GPU view make test-extensions-desktop gives the container:
        // an empty DRM class directory, unless the scenario leaks a card in.
        const drmDir = path.join(dir, "drm");
        fs.mkdirSync(drmDir);
        for (const card of scenario.hostGpus ?? []) {
            fs.mkdirSync(path.join(drmDir, card));
        }
        for (const [name, body] of Object.entries(FAKES)) {
            fs.writeFileSync(path.join(bin, name), body, { mode: 0o755 });
        }
        // Cinnamon ends every log line with a newline, so the stand-in does too.
        fs.writeFileSync(path.join(dir, "cinnamon.log.src"), `${scenario.log ?? LOADED_LOG}\n`);
        // The FIFO the cinnamon stand-in announces its written log on.
        const mkfifo = spawnSync("mkfifo", [path.join(dir, "log-written")]);
        assert.ifError(mkfifo.error);
        assert.strictEqual(mkfifo.status, 0, mkfifo.stderr?.toString());

        const result = spawnSync("bash", [RUNNER], {
            encoding: "utf8",
            timeout: 30000,
            env: {
                PATH: `${bin}:${process.env.PATH}`,
                HOME: dir,
                TMPDIR: dir,
                XDG_RUNTIME_DIR: path.join(dir, "run"),
                LOG_DIR: path.join(dir, "logs"),
                CINNAMON_LOG: path.join(dir, "logs", "cinnamon.log"),
                DRM_CLASS_DIR: drmDir,
                SYSTEM_BUS_DIR: path.join(dir, "run-dbus"),
                UDISKSD: path.join(bin, "udisksd"),
                STARTUP_TIMEOUT: "2",
                DATA_TIMEOUT: "2",
                FAKE_DIR: dir,
                FAKE_RUNNING_AFTER: scenario.runningAfter ?? "0",
                FAKE_PANEL: scenario.panel ?? "up",
                FAKE_APPLET: scenario.applet ?? "loaded",
                FAKE_GAUGE: scenario.gauge ?? "painted",
                FAKE_PAINT_ERROR: scenario.paintError ?? "",
                FAKE_SERVER: scenario.server ?? "up",
                FAKE_XORG: scenario.xorg ?? "ok",
                FAKE_REFRESH: scenario.refresh ?? "60.02Hz",
                FAKE_GSETTINGS: scenario.gsettings ?? "ok",
                FAKE_SYSTEM_BUS: scenario.systemBus ?? "ok",
                FAKE_MISSING_SERVICE: scenario.missingService ?? "",
                FAKE_SWITCHEROO: scenario.switcheroo ?? "absent",
                FAKE_LATE_LOG: scenario.lateLog ?? "",
                FAKE_POPUP: scenario.popup ?? "fresh",
                FAKE_SERVER_INTERVAL: scenario.serverInterval ?? "500",
            },
        });
        assert.ifError(result.error);

        const lines = result.stdout.split("\n");
        const pollsFile = path.join(dir, "runstate-polls");
        // What the run left in LOG_DIR, which make test-extensions-desktop
        // copies out of the container once the runner has exited.
        const logDir = path.join(dir, "logs");
        const logs = fs.existsSync(logDir)
            ? Object.fromEntries(fs.readdirSync(logDir).map((f) => [f, fs.readFileSync(path.join(logDir, f), "utf8")]))
            : {};
        return {
            status: result.status,
            // How many times the runner asked whether Cinnamon had finished
            // starting: what its wait did, counted rather than timed.
            runStatePolls: fs.existsSync(pollsFile) ? Number(fs.readFileSync(pollsFile, "utf8").trim()) : 0,
            stdout: result.stdout,
            logs,
            ok: lines.filter((l) => l.startsWith("ok   ")),
            failed: lines.filter((l) => l.startsWith("FAIL ")),
            notRun: lines.filter((l) => l.startsWith("n/a  ")),
        };
    } finally {
        fs.rmSync(dir, { recursive: true, force: true });
    }
}

test("a healthy session passes every check and exits zero", () => {
    const run = runRunner();

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(run.failed, []);
    assert.deepStrictEqual(run.notRun, []);
    assert.strictEqual(run.ok.length, 30, run.stdout);
    assert.match(run.stdout, /inconclusive: 0/);
});

// The session's logs are what a failed CI run uploads (#225). They used to be
// written to the scratch directory, which the runner removes on exit, so every
// one but cinnamon.log was gone before anything could copy it out.
test("every session log outlives the run, in LOG_DIR", () => {
    const run = runRunner();

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(Object.keys(run.logs).sort(), ["cinnamon.log", "system-bus.log", "udisksd.log", "xorg.log"]);
    assert.match(run.logs["xorg.log"], /X\.Org X Server/);
});

test("a failed run keeps the log that explains it", () => {
    const run = runRunner({ systemBus: "complains" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.match(run.logs["system-bus.log"] ?? "", /Unknown username "whoopsie"/);
});

// Job 42277, 2026-09-03: Eval answered and the panel existed before
// AppletManager.init() had resolved, so a one-shot "is the applet registered"
// check ran 55 ms too early and failed on a desktop that was about to be fine.
//
// The stand-in finishes starting on a poll count, not after a delay, and the
// runner's wait checks at least WAIT_MIN_POLLS (3) times however slow the host
// is, so a session that needs its third poll is waited for deterministically.
// Its timeout alone once decided, and a starved CI runner reported this very
// session as never starting.
test("a session still loading its applets is waited for, not failed", () => {
    const run = runRunner({ runningAfter: "2" });

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(run.failed, []);
    assert.ok(run.ok.includes("ok   the sysmon applet is registered with the panel"), run.stdout);
    assert.strictEqual(run.runStatePolls, 3, "the runner stopped waiting at a different poll than the one that answered RUNNING");
});

// Issue #83's shape: a Cinnamon that answers Eval but never finishes starting.
// Every applet assertion would pass if asked -- the stand-in answers them all
// as a healthy applet would -- so any "ok" here is a pass about a desktop that
// never ran.
test("a session that never finishes starting passes nothing", () => {
    const run = runRunner({ runningAfter: "never", panel: "down" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.ok, [], "a check passed without a proven session");
    assert.deepStrictEqual(run.failed, [
        "FAIL Cinnamon never finished starting (runState did not reach RUNNING)",
    ]);
    assert.strictEqual(run.notRun.length, 29, run.stdout);
    // "Never" is a verdict on several observations, not on however few fit in
    // the timeout on a loaded host.
    assert.ok(run.runStatePolls >= 3, `the runner gave up after ${run.runStatePolls} polls`);
});

test("an applet Cinnamon dropped fails the gate and nothing after it runs", () => {
    const run = runRunner({
        applet: "missing",
        log: "Gjs-Message: JS LOG: [LookingGlass/error] [Applet \"sysmon-go@sysmon\"]: Error importing applet.js",
    });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL the sysmon applet is registered with the panel"]);
    // Only the steps that really were proven may pass.
    assert.deepStrictEqual(run.ok, [
        "ok   Cinnamon finished starting",
        "ok   mutter accepted the display mode",
        "ok   the panel exists",
    ]);
    assert.ok(run.notRun.some((l) => l.includes("applet received a snapshot")), run.stdout);
});

// Issue #83: mutter logged this on every run hosted by Xvfb. It must now fail
// the run, and nothing about the applet may pass on a display mutter rejected.
test("a NaN display-mode warning fails the gate", () => {
    const run = runRunner({ log: `${NAN_MODE_WARNING}\n${LOADED_LOG}` });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL mutter rejected the display mode"]);
    assert.deepStrictEqual(run.ok, ["ok   Cinnamon finished starting"]);
    assert.match(run.stdout, /Invalid mode 1280x1024 \(-nan\)/);
});

// There is no allow-list: every form of warning or error the stack writes
// fails the gate, and the line is shown so the failure names the gap.
for (const line of [
    "(cinnamon:56): GVFS-RemoteVolumeMonitor-WARNING **: remote volume monitor with dbus name org.gtk.vfs.UDisks2VolumeMonitor is not supported",
    "(cinnamon:56): XApp-CRITICAL **: Could not create switcheroo proxy",
    "(cinnamon:56): Gjs-ERROR **: fatal",
    "Gjs-Message: JS ERROR: TypeError: x is undefined",
    "Gjs-Message: JS WARNING: [resource:///org/cinnamon/js/ui/panel.js 1]: reference to undefined property",
    "Gjs-Message: JS LOG: [LookingGlass/error] something failed",
    "Gjs-Message: JS LOG: [LookingGlass/warning] something is off",
    'Xlib:  extension "RANDR" missing on display ":99".',
    // muffin writes its own warnings without GLib's logger.
    "Window manager warning: media-keys-9 - Overwriting existing binding of keysym 1008ff14 with keysym 1008ff31 (keycode a2).",
    "Window manager error: Unable to open X display :99",
    "Bug in window manager: something impossible happened",
    // Near misses of the one exempt line: the exemption is that exact line,
    // not a pattern, so each of these must still fail.
    'Xlib:  extension "DPMS" missing on display ":98".',
    'Xlib:  extension "DPMS" missing on display ":99".  ',
    'Xlib: extension "DPMS" missing on display ":99".',
    '(cinnamon:56): Gjs-WARNING **: Xlib:  extension "DPMS" missing on display ":99".',
]) {
    test(`a startup log line "${line.slice(0, 48)}..." fails the gate`, () => {
        const run = runRunner({ log: `${LOADED_LOG}\n${line}` });

        assert.strictEqual(run.status, 1, run.stdout);
        assert.deepStrictEqual(run.failed, ["FAIL Cinnamon logged warnings or errors while starting"]);
        assert.ok(run.stdout.includes(`  ${line}`), run.stdout);
        assert.ok(!run.ok.some((l) => l.includes("snapshot")), run.stdout);
    });
}

// The owner-ruled exemption (2026-09-22): muffin's DPMS probe on a server that
// cannot have DPMS. Exactly this line, and only this line, passes.
test("the exact DPMS probe line is the one exempt line", () => {
    const dpms = 'Xlib:  extension "DPMS" missing on display ":99".';
    const run = runRunner({ log: `${dpms}\n${dpms}\n${LOADED_LOG}` });

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(run.failed, []);
});

test("the exempt line does not hide another problem beside it", () => {
    const dpms = 'Xlib:  extension "DPMS" missing on display ":99".';
    const run = runRunner({ log: `${dpms}\n${LOADED_LOG}\n(cinnamon:56): St-WARNING **: something` });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL Cinnamon logged warnings or errors while starting"]);
    assert.ok(run.stdout.includes("  (cinnamon:56): St-WARNING **: something"), run.stdout);
    assert.ok(!run.stdout.includes(`  ${dpms}`), "the exempt line was reported as a problem");
});

// The second exemption (owner ruling 2026-09-23): XApp's designed fallback
// with switcheroo-control absent. GLib's pid and timestamp vary; everything
// else must match byte for byte.
const XAPP_FALLBACK =
    "XApp-WARNING **: 03:27:20.581: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...";

test("the exact XApp no-switcheroo fallback line is exempt, whatever its pid and time", () => {
    const run = runRunner({
        log: `(cinnamon:91): ${XAPP_FALLBACK}\n${LOADED_LOG}\n(cinnamon:4242): XApp-WARNING **: 23:59:59.999: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...`,
    });

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(run.failed, []);
});

// Near misses of the XApp exemption, and the lines XApp logs on its other
// paths: every one must still fail.
for (const line of [
    // the buggy no-default-GPU path the ruling avoids
    "(cinnamon:120): XApp-CRITICAL **: 02:18:34.593: Could not get gpu info from switcheroo proxy: (0): No default GPU exists.",
    "(cinnamon:120): GLib-GIO-CRITICAL **: 02:18:34.593: g_task_return_boolean: assertion '!task->ever_returned' failed",
    // the same message at another level
    "(cinnamon:91): XApp-CRITICAL **: 03:27:20.581: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...",
    // another domain
    "(cinnamon:91): Gjs-WARNING **: 03:27:20.581: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...",
    // another program
    "(nemo:91): XApp-WARNING **: 03:27:20.581: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...",
    // a changed message: truncated, extended, re-cased
    "(cinnamon:91): XApp-WARNING **: 03:27:20.581: Switcheroo-control doesn't appear to be running, checking for Ubuntu support..",
    "(cinnamon:91): XApp-WARNING **: 03:27:20.581: Switcheroo-control doesn't appear to be running, checking for Ubuntu support... and more",
    "(cinnamon:91): XApp-WARNING **: 03:27:20.581: switcheroo-control doesn't appear to be running, checking for Ubuntu support...",
    // no timestamp where GLib always writes one
    "(cinnamon:91): XApp-WARNING **: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...",
    // any other XApp warning
    "(cinnamon:91): XApp-WARNING **: 03:27:20.581: Invalid environment returned from switcheroo:",
]) {
    test(`an XApp-adjacent line "${line.slice(0, 60)}..." still fails`, () => {
        const run = runRunner({ log: `${LOADED_LOG}\n${line}` });

        assert.strictEqual(run.status, 1, run.stdout);
        assert.deepStrictEqual(run.failed, ["FAIL Cinnamon logged warnings or errors while starting"]);
        assert.ok(run.stdout.includes(`  ${line}`), run.stdout);
    });
}

// Informational lines are not problems; a check that failed on them would be
// one nobody could pass.
test("informational log lines do not fail the gate", () => {
    const run = runRunner({
        log: `${LOADED_LOG}\nGjs-Message: Profiler is disabled. Not setting up signals.\n** Message: Tried to become the session notification handler but failed.`,
    });

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(run.failed, []);
});

// A warning logged while the checks drive the applet fails the run as well.
test("a warning logged during the run fails it", () => {
    const run = runRunner({ lateLog: "(cinnamon:56): St-CRITICAL **: st_widget_get_theme_node called on an unmapped widget" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL Cinnamon logged warnings or errors during the run"]);
});

// Issue #184: a gauge Clutter never sized is never painted. Queueing a repaint
// on it still succeeds, which is why that alone proved nothing.
test("a gauge with no width that is never painted fails the run", () => {
    const run = runRunner({ gauge: "zero-width" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, [
        "FAIL the panel gauge has a non-zero allocation",
        "FAIL Clutter painted the panel gauge",
        "FAIL the gauge is painted while snapshots are slower to apply than the interval",
    ]);
    assert.match(run.stdout, /allocation: 0x40/);
    assert.match(run.stdout, /paints: 0, stage frames: 1, mapped: true, visible: true/);
});

// Issue #234: the runner wrote the applet's settings where Cinnamon never
// looked, so the applet ran on the schema's defaults and pushed 1000 ms to the
// server. Nothing noticed.
test("an applet that never read its settings file fails the run", () => {
    const run = runRunner({ serverInterval: "1000" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, [
        "FAIL the applet uses the settings file",
        "FAIL the server runs at the applet's interval",
    ]);
    assert.match(run.stdout, /got: +interval: 1000 ms, server: localhost:8080/);
    assert.match(run.stdout, /interval: 1000 ms, expected 500 ms/);
});

// Issue #233: an applet that rebuilds its popup on every snapshot, open or
// not, spends the compositor thread on rows nobody can see.
test("a popup refilled while it is closed fails the run", () => {
    const run = runRunner({ popup: "rebuilt" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL a closed popup is left alone while snapshots arrive"]);
    assert.match(run.stdout, /replaced: 294, text changes: 287/);
});

// A popup that catches up only after Cinnamon has laid it out opens at the
// wrong size.
test("a popup shown before it caught up fails the run", () => {
    const run = runRunner({ popup: "stale" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL opening the popup brings it up to date before it is shown"]);
});

// Issue #232: while each snapshot took longer than the interval to apply, the
// applet kept Cinnamon's main loop busy above Clutter's redraw priority, and
// nothing was painted. A gauge that paints once and then stops fails the run.
test("a gauge that stops painting while snapshots are slow to apply fails the run", () => {
    const run = runRunner({ gauge: "starved" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, [
        "FAIL the gauge is painted while snapshots are slower to apply than the interval",
    ]);
    assert.ok(run.ok.includes("ok   Clutter painted the panel gauge"), run.stdout);
    assert.ok(run.ok.includes("ok   applying a snapshot is fast again"), run.stdout);
});

// A handler that throws still lets later handlers run, so a paint counter alone
// would pass a gauge whose drawing failed. GJS logs the exception as a
// Gjs-CRITICAL, which the final log scan fails on.
test("a repaint handler that throws fails the run even though a paint was counted", () => {
    const run = runRunner({
        paintError: "(cinnamon:55): Gjs-CRITICAL **: 01:38:38.554: JS ERROR: TypeError: ctx is null\n_onRepaint@/root/.local/share/cinnamon/applets/sysmon-go@sysmon/applet.js:540:9",
    });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL Cinnamon logged warnings or errors during the run"]);
    assert.ok(run.ok.includes("ok   Clutter painted the panel gauge"), run.stdout);
    assert.match(run.stdout, /JS ERROR: TypeError: ctx is null/);
});

// An empty log is no evidence of a clean load; the line saying the applet
// loaded has to be there.
test("a log with no record of the applet loading fails the gate", () => {
    const run = runRunner({ log: "" });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL Cinnamon never logged loading the applet"]);
    assert.ok(!run.ok.some((l) => l.includes("snapshot")), run.stdout);
});

test("an applet error in the log fails the gate even when the applet registered", () => {
    const run = runRunner({
        log: `${LOADED_LOG}\nGjs-Message: JS LOG: [LookingGlass/error] [sysmon-go@sysmon]: TypeError: x is undefined`,
    });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL Cinnamon logged an error for the applet"]);
    assert.ok(!run.ok.some((l) => l.includes("snapshot")), run.stdout);
});

// The environment steps must prove they worked. Each of these used to be
// followed by the assertions regardless.
for (const [name, scenario, reason] of [
    ["a server that never answers", { server: "down" }, "the sysmon server never answered /api/snapshot"],
    ["an X server that exits before its display is ready", { xorg: "dies" }, "the X server never reported that :99 was ready"],
    ["a display mode without timings", { refresh: "0.00Hz" }, "the display's current mode has no real refresh rate (RandR reports no timings)"],
    ["a display mode with a NaN refresh rate", { refresh: "-nanHz" }, "the display's current mode has no real refresh rate (RandR reports no timings)"],
    ["a system bus that does not start", { systemBus: "fails" }, "the system bus did not start: Failed to start message bus"],
    ["a host GPU showing through", { hostGpus: ["card0"] }, "the host's GPUs are visible in DRM_DIR; run through `make test-extensions-desktop`, which masks them"],
    ["a system bus that reports a configuration problem", { systemBus: "complains" }, 'the system bus reported a problem: dbus[1]: Unknown username "whoopsie" in message bus configuration file'],
    ["a udisks2 daemon that never claims its name", { missingService: "org.freedesktop.UDisks2" }, "org.freedesktop.UDisks2 never appeared on the system bus"],
    // The container must not run switcheroo-control (owner ruling 2026-09-23):
    // XApp's exempt fallback line only describes a run without it.
    ["a switcheroo-control that is on the bus after all", { switcheroo: "running" }, "net.hadess.SwitcherooControl is on the system bus, but this container must not run switcheroo-control"],
    ["an applet that cannot be enabled", { gsettings: "fails" }, "could not enable the applet in org.cinnamon enabled-applets"],
]) {
    test(`${name} aborts the run before any assertion`, () => {
        const run = runRunner(scenario);

        assert.strictEqual(run.status, 1, run.stdout);
        assert.deepStrictEqual(run.ok, []);
        // The scratch DRM directory differs per run; the message names it.
        const failed = run.failed.map((l) => l.replace(/ \/\S+\/drm;/, " DRM_DIR;"));
        assert.deepStrictEqual(failed, [`FAIL ${reason}`]);
    });
}
