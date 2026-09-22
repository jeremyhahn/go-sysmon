// Tests for the tier 3 runner, test/desktop/run-cinnamon-tests.sh.
//
// The runner is itself a gate, and a gate can be wrong in the direction that
// hurts most: reporting passes about a desktop that never ran. These tests run
// the real script against stand-in executables for the desktop stack (Xvfb,
// dbus-launch, gsettings, cinnamon, gdbus) and the server, so each startup
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

const LOADED_LOG = [
    "(cinnamon:56): mutter-WARNING **: Failed to use linear monitor configuration: Invalid mode 1280x1024 (-nan) for monitor 'unknown unknown'",
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
    curl: `#!/bin/sh
[ "$FAKE_SERVER" = "down" ] && exit 7
exit 0
`,
    Xvfb: `#!/bin/sh
[ "$FAKE_XVFB" = "dies" ] && exit 1
fd=""
while [ $# -gt 0 ]; do
    [ "$1" = "-displayfd" ] && fd="$2"
    shift
done
eval "printf '99\\n' >&$fd"
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
    cinnamon: `#!/bin/sh
cat "$FAKE_DIR/cinnamon.log.src"
exec sleep 60 </dev/null >/dev/null 2>&1
`,
    // gdbus models the order Cinnamon's main.js starts in: the panel exists
    // from the first answer, runState reaches RUNNING only after
    // FAKE_RUNNING_AFTER polls (or never), and the applet is in the loaded set
    // only once it has -- AppletManager.init() resolves before RUNNING is set.
    gdbus: `#!/bin/sh
for js; do :; done
count_file="$FAKE_DIR/runstate-polls"
polls=$(cat "$count_file" 2>/dev/null || echo 0)
running=false
if [ "$FAKE_RUNNING_AFTER" != "never" ] && [ "$polls" -ge "$FAKE_RUNNING_AFTER" ]; then
    running=true
fi
reply() { printf "(true, '%s')\\n" "$1"; exit 0; }
case "$js" in
    "1+1") reply 2 ;;
    *"runState === "*)
        echo $((polls + 1)) > "$count_file"
        reply "$running" ;;
    "String(imports.ui.main.runState)") reply 1 ;;
    *"panelManager"*"> 0)") [ "$FAKE_PANEL" = "down" ] && reply false; reply true ;;
    *".some(function (a)"*)
        if [ "$FAKE_APPLET" = "missing" ] || [ "$running" = "false" ]; then reply false; fi
        reply true ;;
    *"xlet_startup_error"*) [ "$FAKE_APPLET" = "missing" ] && reply true; reply false ;;
    *"i.menu !== undefined; }).length)") reply 10 ;;
    *"indexOf('undefined')"*) reply false ;;
    *"get_text()") reply "CPU: 12%" ;;
    *"queue_repaint"*) reply queued ;;
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
        for (const [name, body] of Object.entries(FAKES)) {
            fs.writeFileSync(path.join(bin, name), body, { mode: 0o755 });
        }
        fs.writeFileSync(path.join(dir, "cinnamon.log.src"), scenario.log ?? LOADED_LOG);

        const result = spawnSync("bash", [RUNNER], {
            encoding: "utf8",
            timeout: 30000,
            env: {
                PATH: `${bin}:${process.env.PATH}`,
                HOME: dir,
                TMPDIR: dir,
                XDG_RUNTIME_DIR: path.join(dir, "run"),
                CINNAMON_LOG: path.join(dir, "cinnamon.log"),
                STARTUP_TIMEOUT: "2",
                DATA_TIMEOUT: "2",
                FAKE_DIR: dir,
                FAKE_RUNNING_AFTER: scenario.runningAfter ?? "0",
                FAKE_PANEL: scenario.panel ?? "up",
                FAKE_APPLET: scenario.applet ?? "loaded",
                FAKE_SERVER: scenario.server ?? "up",
                FAKE_XVFB: scenario.xvfb ?? "ok",
                FAKE_GSETTINGS: scenario.gsettings ?? "ok",
            },
        });
        assert.ifError(result.error);

        const lines = result.stdout.split("\n");
        return {
            status: result.status,
            stdout: result.stdout,
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
    assert.strictEqual(run.ok.length, 13, run.stdout);
    assert.match(run.stdout, /inconclusive: 0/);
});

// Job 42277, 2026-09-03: Eval answered and the panel existed before
// AppletManager.init() had resolved, so a one-shot "is the applet registered"
// check ran 55 ms too early and failed on a desktop that was about to be fine.
test("a session still loading its applets is waited for, not failed", () => {
    const run = runRunner({ runningAfter: "1" });

    assert.strictEqual(run.status, 0, run.stdout);
    assert.deepStrictEqual(run.failed, []);
    assert.ok(run.ok.includes("ok   the sysmon applet is registered with the panel"), run.stdout);
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
    assert.strictEqual(run.notRun.length, 12, run.stdout);
});

test("an applet Cinnamon dropped fails the gate and nothing after it runs", () => {
    const run = runRunner({
        applet: "missing",
        log: "Gjs-Message: JS LOG: [LookingGlass/error] [Applet \"sysmon-go@sysmon\"]: Error importing applet.js",
    });

    assert.strictEqual(run.status, 1, run.stdout);
    assert.deepStrictEqual(run.failed, ["FAIL the sysmon applet is registered with the panel"]);
    // Only the steps that really were proven may pass.
    assert.deepStrictEqual(run.ok, ["ok   Cinnamon finished starting", "ok   the panel exists"]);
    assert.ok(run.notRun.some((l) => l.includes("applet received a snapshot")), run.stdout);
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
    ["an Xvfb that exits before its display is ready", { xvfb: "dies" }, "Xvfb never reported that :99 was ready"],
    ["an applet that cannot be enabled", { gsettings: "fails" }, "could not enable the applet in org.cinnamon enabled-applets"],
]) {
    test(`${name} aborts the run before any assertion`, () => {
        const run = runRunner(scenario);

        assert.strictEqual(run.status, 1, run.stdout);
        assert.deepStrictEqual(run.ok, []);
        assert.deepStrictEqual(run.failed, [`FAIL ${reason}`]);
    });
}
