#!/usr/bin/env bash
#
# Tier 3 desktop tests for the go-sysmon Cinnamon applet.
#
# Starts a real Cinnamon session under Xvfb, loads the applet into the panel,
# and asserts on its live state through the org.Cinnamon D-Bus Eval method,
# which runs JavaScript inside the Cinnamon process and returns the result.
#
# Everything the earlier tiers can check is already checked in-process by
# `make test-extensions`. What only this tier can prove is that Cinnamon itself
# loads the applet: that require("./lib/...") resolves, that the GObject
# Introspection calls are real, and that the popup builds against the actual
# PopupMenu implementation.

set -uo pipefail

readonly UUID="sysmon-go@sysmon"
readonly SERVER_ADDR="localhost:8080"
readonly XVFB_DISPLAY=":99"

# Overridable so extensions/tests/desktop-runner.test.js can drive this script
# against stand-in tools in a scratch directory. In the container they keep
# their defaults.
readonly CINNAMON_LOG="${CINNAMON_LOG:-/tmp/cinnamon.log}"
readonly STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-90}"
readonly DATA_TIMEOUT="${DATA_TIMEOUT:-30}"

# Where Cinnamon keeps loaded applet instances differs across the versions
# metadata.json claims support for: older releases index them by id in
# appletManager.appletObj, newer ones hang them off the definition. Collect from
# both so the probe is not pinned to one release.
readonly APPLET_INSTANCES="[].concat(Object.values(imports.ui.appletManager.appletObj || {}), Object.values(imports.ui.appletManager.definitions || {}).map(function (d) { return d.applet; })).filter(function (a) { return a; })"

# Cinnamon's panels, however the running version exposes them. panelManager is
# current; Main.panel is the older single-panel field, kept because a version
# without panelManager would otherwise report "no panel" on a healthy desktop --
# which is the same false reading in the opposite direction.
readonly PANEL_COUNT="(imports.ui.main.panelManager && imports.ui.main.panelManager.panels ? imports.ui.main.panelManager.panels.filter(function (p) { return p; }).length : (imports.ui.main.panel ? 1 : 0))"

# Cinnamon's own statement that it has finished starting. main.js moves runState
# to RUNNING only once AppletManager.init() -- and the extension, desklet and
# search-provider managers beside it -- have resolved. It is the one signal that
# says every applet has either loaded or failed; nothing earlier does. A release
# without RunState makes this evaluate to false, which fails the run rather
# than letting it pass on a guess.
readonly RUN_STATE_RUNNING="String(imports.ui.main.RunState !== undefined && imports.ui.main.runState === imports.ui.main.RunState.RUNNING)"

readonly IS_SYSMON="function (a) { return a._uuid === '${UUID}' || (a.metadata && a.metadata.uuid === '${UUID}'); }"

failures=0
passes=0

log()  { printf '  %s\n' "$*"; }
pass() { passes=$((passes + 1)); printf 'ok   %s\n' "$*"; }
fail() { failures=$((failures + 1)); printf 'FAIL %s\n' "$*"; }

# inconclusive is neither a pass nor a failure: the check was not evaluated
# because something it depends on was already broken. It does not add to
# failures because the check it depends on has already failed and will fail
# the run; counting it twice would say two things are broken.
inconclusives=0
inconclusive() { inconclusives=$((inconclusives + 1)); printf 'n/a  %s\n' "$*"; }

# abort fails the run before any assertion has been made, for when the
# environment the assertions need could not be brought up at all.
abort() {
    fail "$*"
    if [[ -s "$CINNAMON_LOG" ]]; then
        echo "--- cinnamon.log (tail) ---"
        tail -50 "$CINNAMON_LOG"
    fi
    exit 1
}

# cinnamon_eval runs JavaScript inside the Cinnamon process and prints whatever
# the expression evaluated to. A non-zero exit means the call itself failed.
cinnamon_eval() {
    local js="$1"
    gdbus call --session \
        --dest org.Cinnamon \
        --object-path /org/Cinnamon \
        --method org.Cinnamon.Eval \
        "$js" 2>/dev/null
}

# eval_value extracts the returned value from a gdbus reply, which looks like
#   (true, '<value>')
# The leading boolean is Eval's success flag. Matching against the whole reply
# would let a failed expression pass any assertion that expects "true" or
# "false", because the flag itself contains both -- a gate that passes
# vacuously is worse than no gate at all.
eval_value() {
    local reply="$1"

    if [[ "$reply" != "(true, "* ]]; then
        return 1
    fi

    local value="${reply#(true, \'}"
    value="${value%\')}"
    printf '%s' "$value"
}

# check_eval evaluates js, checks the returned value contains expected, and
# records a pass or a failure. It returns non-zero on failure so a caller can
# stop depending on what it checked.
check_eval() {
    local name="$1" js="$2" expected="$3"
    local reply value
    reply="$(cinnamon_eval "$js")"

    if [[ -z "$reply" ]]; then
        fail "$name (Eval returned nothing)"
        return 1
    fi
    if ! value="$(eval_value "$reply")"; then
        fail "$name (the expression threw)"
        log "reply: $reply"
        return 1
    fi
    if [[ "$value" != *"$expected"* ]]; then
        fail "$name"
        log "expected to contain: $expected"
        log "got:                 $value"
        return 1
    fi
    pass "$name"
}

# wait_for_eval polls an expression until it contains the expected text.
#
# It waits for a state Cinnamon is known to reach on its own, never for an
# assertion to start passing: an expectation that is simply wrong still times
# out and fails.
wait_for_eval() {
    local js="$1" expected="$2" timeout="$3"
    local deadline=$((SECONDS + timeout))
    local value

    while (( SECONDS < deadline )); do
        if value="$(eval_value "$(cinnamon_eval "$js")")" && [[ "$value" == *"$expected"* ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# ---- the session gate ---------------------------------------------------------
#
# Nothing about the applet may pass until the run has positively proven that
# Cinnamon finished starting and loaded the applet. Without that proof an
# applet assertion is not evidence of anything: on 2026-09-03 (job 42277) the
# "an applet is registered" check ran 55 ms before Cinnamon logged "Loaded
# applet sysmon-go@sysmon", failed, and was followed by passes that happened to
# land after it. The gate replaces "check once and hope the timing is right"
# with "wait for Cinnamon to say it is done, then check once".
#
# session_proven is set only when every gate check has passed. Until then
# assert_eval reports each assertion as not run rather than evaluating it, so a
# desktop that never came up produces one failure and a list of checks that
# were never made -- not a column of passes about a component that was never
# running.
session_proven=0
gate_open=1

# gate records one step of the session proof. A step runs only if every step
# before it passed; once one fails, the rest are reported as not run.
gate() {
    local name="$1" js="$2" expected="$3"

    if (( gate_open == 0 )); then
        inconclusive "$name (not run: an earlier session check failed)"
        return
    fi
    if ! check_eval "$name" "$js" "$expected"; then
        gate_open=0
    fi
}

# assert_eval is an assertion about the loaded applet. It is only evaluated
# once the session gate has passed.
assert_eval() {
    local name="$1" js="$2" expected="$3"

    if (( session_proven == 0 )); then
        inconclusive "$name (not run: the session was never proven)"
        return
    fi
    check_eval "$name" "$js" "$expected"
}

cleanup() {
    [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null
    [[ -n "${CINNAMON_PID:-}" ]] && kill "$CINNAMON_PID" 2>/dev/null
    [[ -n "${XVFB_PID:-}" ]] && kill "$XVFB_PID" 2>/dev/null
    [[ -n "${DBUS_SESSION_BUS_PID:-}" ]] && kill "$DBUS_SESSION_BUS_PID" 2>/dev/null
    [[ -n "${SCRATCH:-}" ]] && rm -rf "$SCRATCH"
    return 0
}
trap cleanup EXIT

# ---- bring the environment up ----------------------------------------------
#
# Every step here either proves it worked or ends the run. A step that is
# allowed to fail quietly leaves the assertions below measuring whatever state
# it happened to leave behind.

SCRATCH="$(mktemp -d)"

echo "== starting sysmon server =="
sysmon-server serve --addr "$SERVER_ADDR" --interval 500 &
SERVER_PID=$!

server_deadline=$((SECONDS + STARTUP_TIMEOUT))
until curl -sf "http://${SERVER_ADDR}/api/snapshot" >/dev/null 2>&1; do
    (( SECONDS < server_deadline )) || abort "the sysmon server never answered /api/snapshot"
    sleep 1
done

# A runtime directory has to exist and be private before anything GTK starts.
mkdir -p "${XDG_RUNTIME_DIR:-/run/user/0}"
chmod 700 "${XDG_RUNTIME_DIR:-/run/user/0}"

echo "== starting Xvfb =="
# -displayfd makes Xvfb write the display number once it is accepting
# connections, so this waits for the display to exist rather than sleeping and
# assuming. An Xvfb that dies first closes the pipe and the read fails.
#
# Expect mutter to log "Invalid mode 1280x1024 (-nan)" on every run, passing
# ones included. Xvfb's single RandR mode carries no timings -- dot clock,
# htotal and vtotal are all 0 -- so the refresh rate mutter derives is 0/0.
# mutter rejects its own linear and fallback layouts over it and keeps the X
# server's current one, and the desktop comes up normally. It is not what failed
# job 42277, which logged the same two lines as every healthy run.
mkfifo "${SCRATCH}/displayfd"
Xvfb "$XVFB_DISPLAY" -screen 0 1280x1024x24 -nolisten tcp -displayfd 3 3>"${SCRATCH}/displayfd" &
XVFB_PID=$!
read -r -t 30 display_number <"${SCRATCH}/displayfd" ||
    abort "Xvfb never reported that ${XVFB_DISPLAY} was ready"
[[ ":${display_number}" == "$XVFB_DISPLAY" ]] ||
    abort "Xvfb reported display :${display_number}, not ${XVFB_DISPLAY}"

echo "== starting a session bus =="
# Cinnamon and this script must share one bus: dbus-run-session would give
# Cinnamon a private bus that gdbus here could never reach.
eval "$(dbus-launch --sh-syntax)"
[[ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ]] || abort "dbus-launch did not start a session bus"
export DBUS_SESSION_BUS_ADDRESS DBUS_SESSION_BUS_PID

echo "== enabling the applet =="
# Cinnamon reads its panel layout from gsettings before it starts, so the applet
# has to be enabled up front rather than added afterwards.
gsettings set org.cinnamon enabled-applets "['panel1:right:0:${UUID}:0']" ||
    abort "could not enable the applet in org.cinnamon enabled-applets"
# The server deliberately listens on the applet's default address, so the test
# does not depend on Cinnamon's settings plumbing to make a connection. The
# settings file is still written -- to both the historical and the current
# location -- so the configured path is exercised too.
for dir in "${HOME}/.cinnamon/configs/${UUID}" "${HOME}/.config/cinnamon/spices/${UUID}"; do
    mkdir -p "$dir"
    cat > "${dir}/0.json" <<EOF
{
  "server-address": { "type": "entry", "default": "localhost:8080", "value": "${SERVER_ADDR}" },
  "update-interval": { "type": "spinbutton", "default": 1000, "value": 500 },
  "show-cpu": { "type": "switch", "default": true, "value": true },
  "show-memory": { "type": "switch", "default": true, "value": true },
  "show-network": { "type": "switch", "default": true, "value": true },
  "show-disk": { "type": "switch", "default": true, "value": true }
}
EOF
done

echo "== starting Cinnamon =="
cinnamon --replace >"$CINNAMON_LOG" 2>&1 &
CINNAMON_PID=$!

# Cinnamon needs to claim its bus name before Eval is answerable.
wait_for_eval "1+1" "2" "$STARTUP_TIMEOUT" ||
    abort "Cinnamon never became reachable on the session bus"
echo "Cinnamon is reachable."

# ---- assertions -------------------------------------------------------------

echo
echo "== the session =="

# Cinnamon answers Eval as soon as it claims its bus name, long before it has
# built a panel or loaded an applet, so reachability proves nothing about
# either. Wait for Cinnamon's own "finished starting" state; every check after
# it is then made exactly once, against a desktop that says it is done.
if wait_for_eval "$RUN_STATE_RUNNING" "true" "$STARTUP_TIMEOUT"; then
    pass "Cinnamon finished starting"
else
    fail "Cinnamon never finished starting (runState did not reach RUNNING)"
    log "runState: $(cinnamon_eval "String(imports.ui.main.runState)")"
    gate_open=0
fi

# PanelManager is built synchronously before the applets load, so once the run
# state is RUNNING a missing panel is a desktop that did not come up, not one
# that has not come up yet.
gate "the panel exists" "String(${PANEL_COUNT} > 0)" "true"

# If require() failed or a GI call threw during _init, Cinnamon drops the applet
# and it never reaches the loaded set.
gate "the sysmon applet is registered with the panel" \
    "String(${APPLET_INSTANCES}.some(${IS_SYSMON}))" \
    "true"

# Cinnamon's own summary: set when any applet, desklet or extension failed to
# load, and the reason it shows the "Problems during Cinnamon startup" notice.
gate "Cinnamon reports no applet failed to load" \
    "String(imports.ui.main.xlet_startup_error)" \
    "false"

# Cinnamon reports why an applet failed only in its own log. The log is read
# for evidence in both directions: the line saying the applet loaded must be
# there, and no error tagged with its UUID may be. An empty log is not "no
# error"; it is no evidence.
if (( gate_open == 0 )); then
    inconclusive "Cinnamon logged the applet loading without error (not run: an earlier session check failed)"
elif ! grep -q "Loaded applet ${UUID}" "$CINNAMON_LOG"; then
    fail "Cinnamon never logged loading the applet"
    gate_open=0
elif grep -q "\[${UUID}\]" "$CINNAMON_LOG"; then
    fail "Cinnamon logged an error for the applet"
    grep "\[${UUID}\]" "$CINNAMON_LOG" | tail -10 | while read -r line; do log "$line"; done
    gate_open=0
else
    pass "Cinnamon logged the applet loading without error"
fi

(( gate_open == 1 )) && session_proven=1

readonly APPLET_JS="${APPLET_INSTANCES}.find(${IS_SYSMON})"

echo
echo "== live data =="

if (( session_proven == 0 )); then
    inconclusive "applet received a snapshot over the event stream (not run: the session was never proven)"
elif wait_for_eval "String(${APPLET_JS}.snapshot !== null)" "true" "$DATA_TIMEOUT"; then
    pass "applet received a snapshot over the event stream"
else
    fail "applet never received a snapshot"
    log "applet tooltip: $(cinnamon_eval "${APPLET_JS}.tooltip || 'no applet'")"
    log "server reachable: $(curl -so /dev/null -w '%{http_code}' "http://${SERVER_ADDR}/api/snapshot")"
fi

assert_eval "snapshot carries a hostname" \
    "String(${APPLET_JS}.snapshot.host.hostname.length > 0)" \
    "true"

# Cinnamon keeps the tooltip in a PanelItemTooltip, not on the applet, so read
# the label the user would actually see.
assert_eval "tooltip reports CPU usage" \
    "${APPLET_JS}._applet_tooltip._tooltip.get_text()" \
    "CPU:"

echo
echo "== popup menu =="

# Counted twice on purpose -- here against a real Cinnamon session, and in
# extensions/tests/applet.test.js against the harness. Adding a section to
# SECTION_BUILDERS means updating BOTH; the NPU section (2026-09-16) was caught
# by this one after the harness count had already been raised.
assert_eval "popup has one submenu per section" \
    "String(${APPLET_JS}.menu._getMenuItems().filter(function (i) { return i.menu !== undefined; }).length)" \
    "10"

assert_eval "submenus are populated" \
    "String(${APPLET_JS}.menu._getMenuItems().filter(function (i) { return i.menu !== undefined; }).every(function (i) { return i.menu._getMenuItems().length > 0; }))" \
    "true"

assert_eval "no row renders undefined" \
    "String(${APPLET_JS}.menu._getMenuItems().filter(function (i) { return i.menu !== undefined; }).some(function (i) { return i.label.get_text().indexOf('undefined') !== -1; }))" \
    "false"

echo
echo "== panel rendering =="

# Do not call _onRepaint directly from here. get_context() is only valid inside
# a paint cycle, and invoking the handler by hand takes the whole Cinnamon
# process down -- Eval never answers. Queue a repaint instead and let the
# compositor drive it.
#
# The Cairo drawing itself is asserted operation by operation in tier 2; what
# this tier adds is that the actor is real and that queueing work on it from
# inside Cinnamon is safe.
assert_eval "the panel gauge is a real drawing area" \
    "String(${APPLET_JS}.drawingArea instanceof imports.gi.St.DrawingArea)" \
    "true"

assert_eval "queueing a repaint is safe" \
    "(function () { ${APPLET_JS}.drawingArea.queue_repaint(); return 'queued'; })()" \
    "queued"

# ---- results ----------------------------------------------------------------

echo
echo "== results =="
echo "passed: $passes"
echo "failed: $failures"
# Reported even when zero. A run that says "10 passed, 0 failed" while a check
# could not be evaluated is the same omission this line exists to prevent.
echo "inconclusive: $inconclusives"

# A run that never proved the session cannot pass, whatever the counters say.
# Today a failed gate always records a failure too; this keeps that true if a
# later edit forgets to.
if (( failures > 0 || session_proven == 0 )); then
    echo
    echo "--- cinnamon.log (tail) ---"
    tail -50 "$CINNAMON_LOG"
    exit 1
fi

exit 0
