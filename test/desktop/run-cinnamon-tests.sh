#!/usr/bin/env bash
#
# Tier 3 desktop tests for the go-sysmon Cinnamon applet.
#
# Starts a real Cinnamon session on a headless Xorg display, loads the applet into the panel,
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
readonly X_DISPLAY=":99"

# Overridable so extensions/tests/desktop-runner.test.js can drive this script
# against stand-in tools in a scratch directory. In the container they keep
# their defaults.
readonly CINNAMON_LOG="${CINNAMON_LOG:-/tmp/cinnamon.log}"
readonly STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-90}"
readonly DATA_TIMEOUT="${DATA_TIMEOUT:-30}"
readonly XORG_CONFIG="${XORG_CONFIG:-/etc/X11/xorg-dummy.conf}"

# What mutter logs when it cannot use the display's mode. With a timing-less
# mode it reads "Invalid mode 1280x1024 (-nan)"; any variant of it means the
# display is not the one these tests were designed against.
readonly INVALID_MODE_PATTERN="Invalid mode|\\(-?nan\\)"

# Any line in cinnamon.log that is a warning or worse, in every form the stack
# writes one: GLib's "Domain-WARNING **", "-CRITICAL **" and "-ERROR **"; GJS's
# "JS WARNING"/"JS ERROR"; Cinnamon's LookingGlass error and warning channels;
# muffin's own meta_warning/meta_fatal/meta_bug output ("Window manager
# warning:", "Window manager error:", "Bug in window manager:"), which does not
# go through GLib's logger; and Xlib's complaints about the display. Every
# such line so far was an environment gap -- a timing-less display mode, a
# missing system bus service, a keybinding conflict -- and each was fixed at
# its source rather than tolerated.
readonly LOG_PROBLEM_PATTERN="(WARNING|CRITICAL|ERROR) \\*\\*|JS (ERROR|WARNING)|\\[LookingGlass/(error|warning)\\]|^Window manager (warning|error):|^Bug in window manager:|^Xlib: "

# The kernel's DRM class directory as the container sees it. The container's
# GPU view must be the same on every host (#163), so `make
# test-extensions-desktop` mounts an empty tmpfs over it, and the runner refuses
# to start on any other view. Overridable for the runner tests.
readonly DRM_CLASS_DIR="${DRM_CLASS_DIR:-/sys/class/drm}"

# The one exemption, by owner ruling of 2026-09-22. It is matched as a whole
# line, byte for byte, display included -- no pattern -- so any other Xlib
# line, or this one on another display, still fails the run.
#
# It cannot be fixed at its source in a container:
# - muffin probes for DPMS unconditionally: DPMSCapable() in
#   meta_monitor_manager_xrandr_read_current_state (muffin 6.0.1,
#   src/backends/x11/meta-monitor-manager-xrandr.c:150), and no Cinnamon or
#   muffin setting skips it. libXext prints this line when the server lacks
#   the extension.
# - Xorg registers DPMS only when some screen's driver implements it
#   (xorg-server 21.1.12 Xext/dpms.c: DPMSExtensionInit enables it only if
#   DPMSSupported() finds a screen with a DPMS hook).
# - No headless X server that runs in a container has one: xf86-video-dummy up
#   to master never calls xf86DPMSInit, and Xvfb and TigerVNC's Xvnc have no
#   DPMS either (`xset q`: "Server does not have the DPMS Extension").
# - DPMS would need Xorg's modesetting driver on a kernel DRM device such as
#   vkms, which means loading a module into the host kernel.
readonly DPMS_PROBE_LINE='Xlib:  extension "DPMS" missing on display ":99".'

# The second exemption, by owner ruling of 2026-09-23: XApp's designed
# fallback when switcheroo-control is not running, which the container
# deliberately does not run.
#
# Why switcheroo-control is not run: it lists only DRM cards whose driver is
# nvidia or that udev tagged with ID_PATH_TAG (switcheroo-control 2.6,
# get_card_env). A container has no udev database, so what it reports depends
# on the host's GPUs (#163), and with the GPUs masked it reports none. XApp
# 2.8.2 then takes its no-default-GPU path in helper_init_thread
# (libxapp/xapp-gpu-offload-helper.c), which logs an XApp-CRITICAL and returns
# the GTask twice (a GLib-GIO-CRITICAL). With no switcheroo on the bus, the
# same function finds no name owner (line 308), logs this one warning (line
# 330), and continues with a synthetic default GPU. The NVIDIA entry is added
# only if xapp_util_gpu_offload_supported() finds Ubuntu's PRIME files, which
# the image does not have. So the fallback is the same on every host.
#
# GLib stamps each such line with the process id and the time, so those two
# fields are the only ones not compared. Everything else is compared byte for
# byte: the program name, the domain and level, and the whole message.
readonly XAPP_FALLBACK_MESSAGE="Switcheroo-control doesn't appear to be running, checking for Ubuntu support..."
readonly XAPP_FALLBACK_PREFIX='^\(cinnamon:[0-9]+\): XApp-WARNING \*\*: [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}: (.*)$'

# is_exempt reports whether a log line is one of the two exempt lines above.
is_exempt() {
    local line="$1"

    if [[ "$line" == "$DPMS_PROBE_LINE" ]]; then
        return 0
    fi
    [[ "$line" =~ $XAPP_FALLBACK_PREFIX ]] && [[ "${BASH_REMATCH[1]}" == "$XAPP_FALLBACK_MESSAGE" ]]
}

# log_problems prints every warning or error line in cinnamon.log except the
# exempt ones, and succeeds only if it printed at least one.
log_problems() {
    local line found=1

    while IFS= read -r line; do
        if ! is_exempt "$line"; then
            printf '%s\n' "$line"
            found=0
        fi
    done < <(grep -E "$LOG_PROBLEM_PATTERN" "$CINNAMON_LOG")
    return "$found"
}

# The system bus and udisks2, which Cinnamon reaches at startup through GVFS's
# volume monitor. switcheroo-control is deliberately absent; see above.
# Overridable for the runner tests, like the paths above.
readonly SYSTEM_BUS_DIR="${SYSTEM_BUS_DIR:-/run/dbus}"
readonly UDISKSD="${UDISKSD:-/usr/libexec/udisks2/udisksd}"

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

# WAIT_MIN_POLLS is the fewest times wait_until checks its condition before it
# gives up, however little of the timeout is left. The timeout alone decided it
# before, and on a starved host the whole budget could pass between two checks:
# a session one poll from ready was reported as never starting (a CI flake of
# desktop-runner.test.js). A verdict of "never" now always rests on this many
# observations, and the extra wall time is at most this many polls.
readonly WAIT_MIN_POLLS=3

# wait_until runs a command once a second until it succeeds, and fails once the
# timeout (in seconds) has passed AND the command has been tried at least
# WAIT_MIN_POLLS times.
wait_until() {
    local timeout="$1"
    shift
    local deadline=$((SECONDS + timeout))
    local polls=0

    while true; do
        if "$@"; then
            return 0
        fi
        polls=$((polls + 1))
        if (( SECONDS >= deadline && polls >= WAIT_MIN_POLLS )); then
            return 1
        fi
        sleep 1
    done
}

# eval_contains reports whether an expression's value contains the expected
# text.
# shellcheck disable=SC2317 # invoked through wait_until
eval_contains() {
    local js="$1" expected="$2"
    local value

    value="$(eval_value "$(cinnamon_eval "$js")")" && [[ "$value" == *"$expected"* ]]
}

# wait_for_eval polls an expression until it contains the expected text.
#
# It waits for a state Cinnamon is known to reach on its own, never for an
# assertion to start passing: an expectation that is simply wrong still times
# out and fails.
wait_for_eval() {
    local js="$1" expected="$2" timeout="$3"

    wait_until "$timeout" eval_contains "$js" "$expected"
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
    [[ -n "${XORG_PID:-}" ]] && kill "$XORG_PID" 2>/dev/null
    [[ -n "${DBUS_SESSION_BUS_PID:-}" ]] && kill "$DBUS_SESSION_BUS_PID" 2>/dev/null
    [[ -n "${UDISKSD_PID:-}" ]] && kill "$UDISKSD_PID" 2>/dev/null
    [[ -n "${SYSTEM_BUS_PID:-}" ]] && kill "$SYSTEM_BUS_PID" 2>/dev/null
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

# What the desktop stack logs depends on the DRM devices it can see, so a host
# GPU showing through changes the log -- a CI runner with no usable GPU logged
# two CRITICALs that a workstation never did. Check the view before anything
# reads it.
if [[ ! -d "$DRM_CLASS_DIR" ]] || [[ -n "$(ls -A "$DRM_CLASS_DIR")" ]]; then
    abort "the host's GPUs are visible in ${DRM_CLASS_DIR}; run through \`make test-extensions-desktop\`, which masks them"
fi

echo "== starting sysmon server =="
sysmon-server serve --addr "$SERVER_ADDR" --interval 500 &
SERVER_PID=$!

# server_answers reports whether the sysmon server serves a snapshot.
# shellcheck disable=SC2317 # invoked through wait_until
server_answers() {
    curl -sf "http://${SERVER_ADDR}/api/snapshot" >/dev/null 2>&1
}

wait_until "$STARTUP_TIMEOUT" server_answers ||
    abort "the sysmon server never answered /api/snapshot"

# A runtime directory has to exist and be private before anything GTK starts.
mkdir -p "${XDG_RUNTIME_DIR:-/run/user/0}"
chmod 700 "${XDG_RUNTIME_DIR:-/run/user/0}"

echo "== starting the X server =="
# Xorg with the dummy video driver, configured by xorg-dummy.conf with a VESA
# Modeline, so the display's mode has real timings. Xvfb's has none, and mutter
# logged "Invalid mode 1280x1024 (-nan)" on every run it hosted.
#
# -displayfd makes the server write the display number once it is accepting
# connections, so this waits for the display to exist rather than sleeping and
# assuming. A server that dies first closes the pipe and the read fails.
mkfifo "${SCRATCH}/displayfd"
Xorg "$X_DISPLAY" -config "$XORG_CONFIG" -noreset -nolisten tcp \
    -logfile "${SCRATCH}/xorg.log" -displayfd 3 3>"${SCRATCH}/displayfd" &
XORG_PID=$!
read -r -t 30 display_number <"${SCRATCH}/displayfd" ||
    abort "the X server never reported that ${X_DISPLAY} was ready"
[[ ":${display_number}" == "$X_DISPLAY" ]] ||
    abort "the X server reported display :${display_number}, not ${X_DISPLAY}"

# display_refresh_hz prints the current mode's vertical refresh rate, as the X
# server reports it through RandR, or nothing if that is not a positive number.
# A mode without timings reports 0.00 Hz, and a rate mutter could not use must
# not get as far as mutter.
display_refresh_hz() {
    xrandr --display "$X_DISPLAY" --verbose 2>/dev/null | awk '
        /\*current/ { current = 1; next }
        current && $1 == "v:" {
            rate = $NF
            if (rate ~ /^[0-9]+(\.[0-9]+)?Hz$/) {
                sub(/Hz$/, "", rate)
                if (rate + 0 > 0) print rate
            }
            exit
        }'
}

refresh_hz="$(display_refresh_hz)"
[[ -n "$refresh_hz" ]] ||
    abort "the display's current mode has no real refresh rate (RandR reports no timings)"
echo "display ${X_DISPLAY}: 1280x1024 at ${refresh_hz} Hz"

echo "== starting the system bus and its services =="
# A container has no init to start these, so the runner does, as a desktop
# host's init would. Without them Cinnamon starts but logs a warning for each
# service it could not reach, and the log check below fails the run on any
# warning. polkitd, which udisksd asks for authorisation, is started by the bus
# itself through D-Bus activation.
mkdir -p "$SYSTEM_BUS_DIR"
SYSTEM_BUS_PID="$(dbus-daemon --system --fork --print-pid 2>"${SCRATCH}/system-bus.log")" ||
    abort "the system bus did not start: $(cat "${SCRATCH}/system-bus.log")"
# The bus complains about its own configuration on stderr, for instance a
# policy naming a user that does not exist. That is the same kind of gap as a
# warning in cinnamon.log, so it fails the run the same way.
[[ ! -s "${SCRATCH}/system-bus.log" ]] ||
    abort "the system bus reported a problem: $(cat "${SCRATCH}/system-bus.log")"
"$UDISKSD" >"${SCRATCH}/udisksd.log" 2>&1 &
UDISKSD_PID=$!

# system_bus_has_owner reports whether a name is owned on the system bus.
# shellcheck disable=SC2317 # invoked through wait_until
system_bus_has_owner() {
    gdbus call --system \
        --dest org.freedesktop.DBus \
        --object-path /org/freedesktop/DBus \
        --method org.freedesktop.DBus.NameHasOwner \
        "$1" 2>/dev/null | grep -q "^(true,)$"
}

wait_until "$STARTUP_TIMEOUT" system_bus_has_owner org.freedesktop.UDisks2 ||
    abort "org.freedesktop.UDisks2 never appeared on the system bus"

# XApp's exempt fallback line is only the right line if switcheroo-control is
# really absent. If something owns its name, XApp takes a different path,
# and the exemption would be describing a run that did not happen.
if system_bus_has_owner net.hadess.SwitcherooControl; then
    abort "net.hadess.SwitcherooControl is on the system bus, but this container must not run switcheroo-control"
fi

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
# Cinnamon binds XF86AudioPlay and XF86AudioPause by default, and the standard
# XKB keymaps put both on one key (inet: <I22>/<I172> = [XF86AudioPlay,
# XF86AudioPause]). muffin can bind only one action to that keycode, so it
# logs "Window manager warning: media-keys-9 - Overwriting existing binding"
# on every start. Play already toggles playback, so the separate Pause
# binding is cleared through Cinnamon's own setting, which removes the
# conflict.
gsettings set org.cinnamon.desktop.keybindings.media-keys pause "[]" ||
    abort "could not clear the conflicting media-keys pause binding"
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

# mutter reads the display's modes before Cinnamon draws anything. A mode it
# cannot use is logged, not fatal, which is exactly why it is checked: the
# desktop would come up on a fallback layout and every later check would pass
# on a display nobody configured.
if (( gate_open == 0 )); then
    inconclusive "mutter accepted the display mode (not run: an earlier session check failed)"
elif grep -qE "$INVALID_MODE_PATTERN" "$CINNAMON_LOG"; then
    fail "mutter rejected the display mode"
    grep -E "$INVALID_MODE_PATTERN" "$CINNAMON_LOG" | tail -5 | while read -r line; do log "$line"; done
    gate_open=0
else
    pass "mutter accepted the display mode"
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

# Anything Cinnamon had to warn about while starting is a desktop that did not
# start the way these tests assume. The lines are printed so the failure says
# what the gap was.
if (( gate_open == 0 )); then
    inconclusive "Cinnamon started without a warning or error (not run: an earlier session check failed)"
elif problems="$(log_problems)"; then
    fail "Cinnamon logged warnings or errors while starting"
    tail -10 <<<"$problems" | while IFS= read -r line; do log "$line"; done
    gate_open=0
else
    pass "Cinnamon started without a warning or error"
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
# The Cairo drawing itself is asserted operation by operation in tier 2. What
# this tier adds is that the actor is real, that Clutter gave it a size, and
# that Clutter really painted it. Issue #184: for months the gauge was a
# zero-width actor that Clutter never painted, while "queueing a repaint is
# safe" kept passing, because queueing is safe whether or not a frame follows.
assert_eval "the panel gauge is a real drawing area" \
    "String(${APPLET_JS}.drawingArea instanceof imports.gi.St.DrawingArea)" \
    "true"

# Allocation follows set_width on the next layout pass, so wait for it.
readonly GAUGE_ALLOCATED="(function () { var box = ${APPLET_JS}.drawingArea.get_allocation_box(); return String(box.get_width() > 0 && box.get_height() > 0); })()"
readonly GAUGE_ALLOCATION="(function () { var box = ${APPLET_JS}.drawingArea.get_allocation_box(); return box.get_width() + 'x' + box.get_height(); })()"
if (( session_proven == 0 )); then
    inconclusive "the panel gauge has a non-zero allocation (not run: the session was never proven)"
elif wait_for_eval "$GAUGE_ALLOCATED" "true" "$DATA_TIMEOUT"; then
    pass "the panel gauge has a non-zero allocation"
    log "allocation: $(eval_value "$(cinnamon_eval "$GAUGE_ALLOCATION")")"
else
    fail "the panel gauge has a non-zero allocation"
    log "allocation: $(eval_value "$(cinnamon_eval "$GAUGE_ALLOCATION")")"
fi

# Count the paints Clutter delivers from here on, queue one, and wait for it.
# The counter's handler is connected after the applet's own, so when it has
# run, the applet's drawing has too.
assert_eval "a repaint can be queued" \
    "(function () { var area = ${APPLET_JS}.drawingArea; area._sysmonTestPaints = 0; area.connect('repaint', function () { area._sysmonTestPaints++; }); area.queue_repaint(); return 'queued'; })()" \
    "queued"

if (( session_proven == 0 )); then
    inconclusive "Clutter painted the panel gauge (not run: the session was never proven)"
elif wait_for_eval "String(${APPLET_JS}.drawingArea._sysmonTestPaints > 0)" "true" "$DATA_TIMEOUT"; then
    pass "Clutter painted the panel gauge"
    log "paints: $(eval_value "$(cinnamon_eval "String(${APPLET_JS}.drawingArea._sysmonTestPaints)")")"
else
    fail "Clutter painted the panel gauge"
    log "paints: $(eval_value "$(cinnamon_eval "String(${APPLET_JS}.drawingArea._sysmonTestPaints)")")"
fi

echo
echo "== the log, after the run =="

# The checks above drive the applet; anything it or Cinnamon warned about while
# they ran fails the run just as a startup warning does. This is also what
# makes the paint count above trustworthy: GJS logs an exception thrown by a
# signal handler (as "Gjs-CRITICAL **: JS ERROR") and then runs the next
# handler, so the counter goes up even if _onRepaint threw and left the gauge
# blank. That line fails the run here.
if (( session_proven == 0 )); then
    inconclusive "Cinnamon logged no warning or error during the run (not run: the session was never proven)"
elif problems="$(log_problems)"; then
    fail "Cinnamon logged warnings or errors during the run"
    tail -10 <<<"$problems" | while IFS= read -r line; do log "$line"; done
else
    pass "Cinnamon logged no warning or error during the run"
fi

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
