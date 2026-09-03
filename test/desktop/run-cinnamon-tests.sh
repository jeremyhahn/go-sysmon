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

readonly IS_SYSMON="function (a) { return a._uuid === '${UUID}' || (a.metadata && a.metadata.uuid === '${UUID}'); }"

failures=0
passes=0

log()  { printf '  %s\n' "$*"; }
pass() { passes=$((passes + 1)); printf 'ok   %s\n' "$*"; }
fail() { failures=$((failures + 1)); printf 'FAIL %s\n' "$*"; }

# inconclusive is neither a pass nor a failure: the check could not be
# evaluated because something it depends on was already broken.
#
# It exists because "Cinnamon logged no error for the applet" used to be printed
# as a pass whenever the log held no applet line -- which is equally true of an
# applet that loaded cleanly and of a Cinnamon that never loaded applets at all.
# On 2026-09-03 the virtual display failed to configure with a NaN refresh rate,
# the panel never came up, and that assertion reported success about a component
# that had never run, beside the assertion that reported the real failure.
#
# It does not add to failures because the check it depends on has already failed
# and will fail the run; counting it twice would say two things are broken.
inconclusives=0
inconclusive() { inconclusives=$((inconclusives + 1)); printf 'n/a  %s\n' "$*"; }

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

# assert_eval evaluates js and checks the returned value contains expected.
assert_eval() {
    local name="$1" js="$2" expected="$3"
    local reply value
    reply="$(cinnamon_eval "$js")"

    if [[ -z "$reply" ]]; then
        fail "$name (Eval returned nothing)"
        return
    fi
    if ! value="$(eval_value "$reply")"; then
        fail "$name (the expression threw)"
        log "reply: $reply"
        return
    fi
    if [[ "$value" != *"$expected"* ]]; then
        fail "$name"
        log "expected to contain: $expected"
        log "got:                 $value"
        return
    fi
    pass "$name"
}

# wait_for_eval polls an expression until it contains the expected text.
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

cleanup() {
    [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null
    [[ -n "${CINNAMON_PID:-}" ]] && kill "$CINNAMON_PID" 2>/dev/null
    [[ -n "${XVFB_PID:-}" ]] && kill "$XVFB_PID" 2>/dev/null
    [[ -n "${DBUS_SESSION_BUS_PID:-}" ]] && kill "$DBUS_SESSION_BUS_PID" 2>/dev/null
    return 0
}
trap cleanup EXIT

# ---- bring the environment up ----------------------------------------------

echo "== starting sysmon server =="
sysmon-server serve --addr "$SERVER_ADDR" --interval 500 &
SERVER_PID=$!

for _ in $(seq 1 30); do
    if curl -sf "http://${SERVER_ADDR}/api/snapshot" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

# A runtime directory has to exist and be private before anything GTK starts.
mkdir -p "${XDG_RUNTIME_DIR:-/run/user/0}"
chmod 700 "${XDG_RUNTIME_DIR:-/run/user/0}"

echo "== starting Xvfb =="
Xvfb "$XVFB_DISPLAY" -screen 0 1280x1024x24 -nolisten tcp &
XVFB_PID=$!
sleep 2

echo "== starting a session bus =="
# Cinnamon and this script must share one bus: dbus-run-session would give
# Cinnamon a private bus that gdbus here could never reach.
eval "$(dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS DBUS_SESSION_BUS_PID

echo "== enabling the applet =="
# Cinnamon reads its panel layout from gsettings before it starts, so the applet
# has to be enabled up front rather than added afterwards.
gsettings set org.cinnamon enabled-applets \
    "['panel1:right:0:${UUID}:0']" || true
# The server deliberately listens on the applet's default address, so the test
# does not depend on Cinnamon's settings plumbing to make a connection. The
# settings file is still written -- to both the historical and the current
# location -- so the configured path is exercised too.
for dir in /root/.cinnamon/configs/${UUID} /root/.config/cinnamon/spices/${UUID}; do
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
cinnamon --replace >/tmp/cinnamon.log 2>&1 &
CINNAMON_PID=$!

# Cinnamon needs to claim its bus name before Eval is answerable.
ready=0
for _ in $(seq 1 90); do
    if cinnamon_eval "1+1" | grep -q "2"; then
        ready=1
        break
    fi
    sleep 1
done

if (( ready == 0 )); then
    echo "Cinnamon never became reachable on the session bus." >&2
    echo "--- cinnamon.log ---" >&2
    tail -50 /tmp/cinnamon.log >&2
    exit 1
fi
echo "Cinnamon is up."

# ---- assertions -------------------------------------------------------------

echo
echo "== the desktop itself =="

# Asserted before anything about the applet, because every applet assertion
# below is meaningless without a panel to load into -- and the failure they
# produce in that case reads as "the applet is broken" rather than "the desktop
# did not start".
#
# Cinnamon answers Eval as soon as it claims its bus name, which it does before
# the panel exists, so reachability is not the same question -- a display that
# fails to configure leaves a reachable Cinnamon with no panel. That is also why
# this waits rather than asserting once: on a healthy start the panel is
# legitimately absent for a moment.
panel_up=0
if wait_for_eval "String(${PANEL_COUNT} > 0)" "true" 30; then
    pass "the panel exists"
    panel_up=1
else
    fail "the panel never came up, so nothing below could load into it"
    log "--- display and monitor lines from cinnamon.log ---"
    grep -iE "monitor|mode|display|mutter" /tmp/cinnamon.log | tail -10 |
        while read -r line; do log "$line"; done
fi

echo
echo "== applet loading =="

# The applet must be in the loaded set. If require() failed or a GI call threw
# during _init, Cinnamon drops it and this is where that shows up.
assert_eval "an applet is registered with the panel" \
    "String(${APPLET_INSTANCES}.length > 0)" \
    "true"

# A require() that cannot resolve, or a GI call that throws during _init, makes
# Cinnamon drop the applet. It never reaches appletObj, and the reason is only
# ever in the log.
assert_eval "the sysmon applet loaded without an exception" \
    "String(${APPLET_INSTANCES}.some(${IS_SYSMON}))" \
    "true"

readonly APPLET_JS="${APPLET_INSTANCES}.find(${IS_SYSMON})"

# Cinnamon reports a failed applet load only in its own log.
#
# An empty log is only evidence when Cinnamon actually tried to load the applet.
# Without the panel it never gets that far, and reporting "no error" then is a
# pass earned by nothing having happened.
if grep -q "\[${UUID}\]" /tmp/cinnamon.log; then
    fail "Cinnamon logged an error for the applet"
    grep "\[${UUID}\]" /tmp/cinnamon.log | tail -10 | while read -r line; do log "$line"; done
elif (( panel_up == 0 )); then
    inconclusive "Cinnamon logged no error for the applet (no panel, so it never tried to load one)"
else
    pass "Cinnamon logged no error for the applet"
fi

echo
echo "== live data =="

if wait_for_eval "String(${APPLET_JS}.snapshot !== null)" "true" 30; then
    pass "applet received a snapshot over the event stream"
else
    fail "applet never received a snapshot"
    log "applet tooltip: $(cinnamon_eval "${APPLET_JS}.tooltip || 'no applet'")"
    log "server reachable: $(curl -so /dev/null -w '%{http_code}' "http://${SERVER_ADDR}/api/snapshot")"
    tail -20 /tmp/cinnamon.log
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

assert_eval "popup has one submenu per section" \
    "String(${APPLET_JS}.menu._getMenuItems().filter(function (i) { return i.menu !== undefined; }).length)" \
    "9"

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

if (( failures > 0 )); then
    echo
    echo "--- cinnamon.log (tail) ---"
    tail -50 /tmp/cinnamon.log
    exit 1
fi

exit 0
