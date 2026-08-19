#!/usr/bin/env bash
# deploy.sh -- install go-sysmon as a native systemd service on a remote host.
#
# Driven by `make deploy HOST=<host> [USER=<user>]`; see docs/deployment/ for
# the full surface. Everything it needs arrives in the environment, so it can
# also be run directly:
#
#   HOST=host.example.com USER=deploy scripts/deploy.sh
#
# The design in one line: build the binary HERE and copy it there, so the target
# needs no container runtime, no registry, no Go and no Node -- only systemd,
# SSH and a glibc new enough to run what was built.
#
# Idempotent by construction. Every step either converges on the desired state
# or is skipped because it is already there; re-running after a partial failure
# is the supported recovery, not a risk. The one file it will never overwrite is
# the target's sysmon.env, because that is where an operator's settings live.
set -euo pipefail

# --- inputs -----------------------------------------------------------------

HOST="${HOST:-}"
SSH_USER="${SSH_USER:-}"
CONF_DIR="${CONF_DIR:-/etc/go-sysmon}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"
# The account the service runs as. Created if absent, never removed.
SVC_USER="${SVC_USER:-sysmon}"
# The name the unit is installed under. It does not have to match the file in
# deploy/, which is why the source path below is separate: the shipped unit is
# one file, and what the target calls it is a deployment decision.
UNIT_NAME="${UNIT_NAME:-go-sysmon.service}"
UNIT_SRC="${UNIT_SRC:-deploy/go-sysmon.service}"
BINARY="${BINARY:-bin/sysmon-server}"
SSH_OPTS="${SSH_OPTS:--o BatchMode=yes -o ConnectTimeout=10}"
SUDO="${SUDO-sudo}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "$HOST" ]; then
	echo "HOST is required, e.g. make deploy HOST=host.example.com" >&2
	exit 2
fi

if [ ! -x "$REPO_ROOT/$BINARY" ]; then
	echo "$BINARY is missing or not executable. Build it with: make build-server" >&2
	exit 2
fi

TARGET="$HOST"
[ -n "$SSH_USER" ] && TARGET="$SSH_USER@$HOST"

# word-splitting SSH_OPTS is intentional: it is a list of flags, not one word.
# shellcheck disable=SC2086
remote() { ssh $SSH_OPTS "$TARGET" "$@"; }

step() { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() {
	printf '\nFAILED: %s\n' "$*" >&2
	exit 1
}

# --- 1. preflight -----------------------------------------------------------
#
# Every check here is something that would otherwise fail halfway through, on
# the target, with a message that does not name the cause. Checking costs one
# round trip.

step "checking $TARGET"

remote true 2>/dev/null ||
	fail "cannot ssh to $TARGET.
    BatchMode is on, so this is a key-auth failure, not a password prompt.
    Check:  ssh -v $TARGET true
    A server offering only keyboard-interactive has public-key auth disabled."

TARGET_FACTS="$(remote 'set -e
	arch=$(uname -m)
	[ "$arch" = x86_64 ] || { echo "PREFLIGHT: target is $arch, the binary is amd64" >&2; exit 1; }
	command -v systemctl >/dev/null || { echo "PREFLIGHT: systemd is not present" >&2; exit 1; }
	command -v install >/dev/null || { echo "PREFLIGHT: coreutils install(1) is missing" >&2; exit 1; }
	printf "%s\n" "$(ldd --version | head -1 | grep -oE "[0-9]+\.[0-9]+$")"
')" || fail "preflight checks did not pass on $TARGET"

TARGET_GLIBC="$TARGET_FACTS"
[ -n "$TARGET_GLIBC" ] || fail "could not read the target's glibc version"

# The binary is dynamically linked -- CGO is required, because the NVIDIA NVML
# dependency does not build without it -- so a build host with a newer glibc
# than the target produces something that will not start there. The error when
# it happens is a linker message at exec time, which systemd reports as a
# failed unit with no obvious cause, so it is worth catching here.
#
# What matters is not the build host's glibc but the highest symbol version the
# binary actually references, which is usually far lower.
step "checking glibc compatibility"
NEED_GLIBC="$(objdump -T "$REPO_ROOT/$BINARY" 2>/dev/null |
	grep -oE 'GLIBC_[0-9]+\.[0-9]+' | sed 's/GLIBC_//' | sort -V | tail -1)"

if [ -z "$NEED_GLIBC" ]; then
	note "binary references no versioned glibc symbols (statically linked?)"
else
	note "binary needs glibc $NEED_GLIBC, target has $TARGET_GLIBC"
	# sort -V puts the lower version first; if the target's is not first, the
	# binary needs something the target does not have.
	if [ "$(printf '%s\n%s\n' "$NEED_GLIBC" "$TARGET_GLIBC" | sort -V | head -1)" != "$NEED_GLIBC" ]; then
		fail "$TARGET has glibc $TARGET_GLIBC but the binary needs $NEED_GLIBC.
    The build host's glibc is newer than the target's. Build on a host whose
    glibc is no newer than the target's, or in a container based on the
    target's distribution release."
	fi
fi

# Which port this deploy will actually use: an existing sysmon.env on the target
# wins, because that file is not overwritten. Only when there is none does the
# caller's SYSMON_ADDR (or the shipped default) apply.
EFFECTIVE_ADDR="$(remote "sed -n 's/^SYSMON_ADDR=//p' '$CONF_DIR/sysmon.env' 2>/dev/null | tail -1" || true)"
EFFECTIVE_ADDR="${EFFECTIVE_ADDR:-${SYSMON_ADDR:-:8081}}"
PROBE_PORT="${EFFECTIVE_ADDR##*:}"

if [ -n "${SYSMON_ADDR:-}" ] && [ "$SYSMON_ADDR" != "$EFFECTIVE_ADDR" ]; then
	note "NOTE: you asked for $SYSMON_ADDR, but $CONF_DIR/sysmon.env already sets"
	note "      $EFFECTIVE_ADDR and is not overwritten. Edit it on the target, then:"
	note "      $SUDO systemctl restart $UNIT_NAME"
fi

# A port already in use means the service exits immediately on start. Our own
# unit holding it is the normal case on a redeploy and is not a conflict.
step "checking that port $PROBE_PORT is available"
remote "port='$PROBE_PORT'
	if ! command -v ss >/dev/null 2>&1; then
		# Say so rather than reporting the port free: an absent tool is not
		# evidence of an absent listener, and systemd will find out the hard way.
		echo \"    ss is not installed, cannot check port \$port in advance\"
		exit 0
	fi
	if ss -ltnH 2>/dev/null | grep -q \":\$port \"; then
		if $SUDO systemctl is-active '$UNIT_NAME' >/dev/null 2>&1; then
			echo \"    port \$port is held by the running $UNIT_NAME (will be restarted)\"
		else
			echo \"PORT CONFLICT: something other than $UNIT_NAME is listening on \$port\" >&2
			$SUDO ss -ltnp 2>/dev/null | grep \":\$port \" >&2 || true
			exit 1
		fi
	else
		echo \"    port \$port is free\"
	fi
" || fail "port $PROBE_PORT is taken on $TARGET by another service.
    Pick another with:  make deploy HOST=$HOST SYSMON_ADDR=:<port>
    or free the port on the target."

# --- 2. staging -------------------------------------------------------------
#
# scp cannot elevate, so files land in a temp directory and are installed from
# there. install(1) is used rather than cp so the mode is set atomically with
# the copy instead of in a second step that can be interrupted.

step "copying files to $TARGET"

STAGE="$(remote 'mktemp -d')" || fail "could not create a staging directory on $TARGET"
# shellcheck disable=SC2086
trap 'ssh $SSH_OPTS "$TARGET" "rm -rf $STAGE" 2>/dev/null || true' EXIT

# shellcheck disable=SC2086
scp -q $SSH_OPTS \
	"$REPO_ROOT/$BINARY" \
	"$REPO_ROOT/deploy/sysmon.env.example" \
	"$REPO_ROOT/$UNIT_SRC" \
	"$TARGET:$STAGE/" || fail "could not copy the deployment files to $TARGET"

note "$(du -h "$REPO_ROOT/$BINARY" | cut -f1) binary, unit and defaults staged"

UNIT_STAGED="$STAGE/$(basename "$UNIT_SRC")"
BIN_STAGED="$STAGE/$(basename "$BINARY")"

# --- 3. the service account -------------------------------------------------

step "ensuring the $SVC_USER service account exists"
remote "set -e
	if id -u '$SVC_USER' >/dev/null 2>&1; then
		echo '    $SVC_USER already exists'
	else
		$SUDO useradd --system --no-create-home --home-dir /nonexistent \\
			--shell /usr/sbin/nologin --user-group \\
			--comment 'go-sysmon metrics dashboard' '$SVC_USER'
		echo '    $SVC_USER created (system account, no home, no shell)'
	fi
" || fail "could not create the $SVC_USER account on $TARGET"

# --- 4. the binary ----------------------------------------------------------

step "installing the binary to $TARGET:$BIN_DIR/sysmon-server"

# install(1) truncates its destination, which fails with ETXTBSY while the old
# binary is running. Writing a sibling and renaming over it is atomic and works
# regardless: the running process keeps the old inode until it exits.
remote "set -e
	$SUDO install -d -m 0755 '$BIN_DIR'
	$SUDO install -m 0755 -o root -g root '$BIN_STAGED' '$BIN_DIR/.sysmon-server.new'
	$SUDO mv -f '$BIN_DIR/.sysmon-server.new' '$BIN_DIR/sysmon-server'
	echo \"    \$('$BIN_DIR/sysmon-server' version)\"
" || fail "could not install the binary on $TARGET"

# --- 5. configuration -------------------------------------------------------

step "installing configuration to $TARGET:$CONF_DIR"

remote "set -e
	$SUDO install -d -m 0755 '$CONF_DIR'

	# The env file is operator state. Seed it once, then never touch it again --
	# a deploy that resets a hand-tuned interval or bind address is a deploy
	# nobody runs twice. SYSMON_ADDR below is applied only on that first
	# creation, for the same reason.
	if [ -f '$CONF_DIR/sysmon.env' ]; then
		echo '    sysmon.env  kept (already present, not overwritten)'
	else
		$SUDO install -m 0644 '$STAGE/sysmon.env.example' '$CONF_DIR/sysmon.env'
		echo '    sysmon.env  created from the shipped defaults'
		if [ -n '${SYSMON_ADDR:-}' ]; then
			$SUDO sed -i 's|^SYSMON_ADDR=.*|SYSMON_ADDR=${SYSMON_ADDR:-}|' '$CONF_DIR/sysmon.env'
			echo '    sysmon.env  seeded with SYSMON_ADDR=${SYSMON_ADDR:-}'
		fi
	fi
" || fail "could not install the configuration on $TARGET"

# --- 6. the unit ------------------------------------------------------------

step "installing $UNIT_NAME"

remote "set -e
	install -m 0644 '$UNIT_STAGED' '$STAGE/unit.staged'

	# The unit ships with the default paths baked in. BIN_DIR, CONF_DIR and
	# SVC_USER are documented knobs, so the unit has to follow them or it would
	# start a binary the deploy never wrote.
	if [ '$BIN_DIR' != /usr/local/bin ]; then
		sed -i 's|/usr/local/bin/sysmon-server|$BIN_DIR/sysmon-server|g' '$STAGE/unit.staged'
		echo '    binary directory is $BIN_DIR, unit rewritten to match'
	fi
	if [ '$CONF_DIR' != /etc/go-sysmon ]; then
		sed -i 's|/etc/go-sysmon|$CONF_DIR|g' '$STAGE/unit.staged'
		echo '    config directory is $CONF_DIR, unit rewritten to match'
	fi
	if [ '$SVC_USER' != sysmon ]; then
		sed -i 's|^User=sysmon\$|User=$SVC_USER|; s|^Group=sysmon\$|Group=$SVC_USER|' '$STAGE/unit.staged'
		echo '    service account is $SVC_USER, unit rewritten to match'
	fi

	# Only reload systemd when the unit actually changed; a no-op deploy should
	# be a no-op.
	if $SUDO cmp -s '$STAGE/unit.staged' '/etc/systemd/system/$UNIT_NAME' 2>/dev/null; then
		echo '    unit unchanged'
	else
		$SUDO install -m 0644 '$STAGE/unit.staged' '/etc/systemd/system/$UNIT_NAME'
		$SUDO systemctl daemon-reload
		echo '    unit installed, systemd reloaded'
	fi

	$SUDO systemctl enable '$UNIT_NAME' >/dev/null 2>&1
	echo '    enabled at boot'
" || fail "could not install the systemd unit on $TARGET"

# restart, not start: on a redeploy the service is already active and start
# would leave the old binary running.
step "starting the service"
remote "$SUDO systemctl restart '$UNIT_NAME'" || {
	remote "$SUDO systemctl status '$UNIT_NAME' --no-pager -l | tail -30" || true
	remote "$SUDO journalctl -u '$UNIT_NAME' -n 40 --no-pager" || true
	fail "$UNIT_NAME did not start on $TARGET (status and journal above)"
}

# --- 7. verification --------------------------------------------------------
#
# "systemctl restart returned 0" only proves the unit was accepted. What matters
# is that the process is still alive a moment later and the dashboard answers --
# a bad flag or a taken port exits within milliseconds and Restart=always then
# hides it behind a restart loop.

step "verifying"

remote "for i in \$(seq 1 30); do
		state=\$($SUDO systemctl show -p ActiveState --value '$UNIT_NAME')
		case \"\$state\" in
			active) echo '    unit is active'; exit 0 ;;
			failed) echo '    unit FAILED'; exit 1 ;;
		esac
		sleep 1
	done
	echo \"    unit did not become active within 30s (state: \$state)\"
	exit 1" || {
	remote "$SUDO journalctl -u '$UNIT_NAME' -n 40 --no-pager" || true
	fail "$UNIT_NAME is not active on $TARGET"
}

# NRestarts catches the restart loop that ActiveState=active cannot: a service
# that crashes and is restarted every 5s reads as "active" at almost any instant
# you sample it.
remote "sleep 8
	n=\$($SUDO systemctl show -p NRestarts --value '$UNIT_NAME')
	if [ \"\${n:-0}\" -gt 0 ]; then
		echo \"    service has restarted \$n time(s) since it was started -- it is crash-looping\" >&2
		exit 1
	fi
	echo '    stayed up (no restarts)'" || {
	remote "$SUDO journalctl -u '$UNIT_NAME' -n 40 --no-pager" || true
	fail "$UNIT_NAME is crash-looping on $TARGET (journal above)"
}

remote "curl -fsS -o /dev/null -w '    dashboard answered HTTP %{http_code}\n' 'http://127.0.0.1:$PROBE_PORT/'" ||
	note "dashboard did not answer on 127.0.0.1:$PROBE_PORT (expected if SYSMON_ADDR binds one non-loopback address)"

step "what this host can see"
remote "$BIN_DIR/sysmon-server doctor 2>&1 | sed 's/^/    /'" || true

cat <<EOF

Deployed sysmon-server to $TARGET.

    dashboard   http://$HOST:$PROBE_PORT/
    settings    $TARGET:$CONF_DIR/sysmon.env
    logs        ssh $TARGET '$SUDO journalctl -u $UNIT_NAME -f'
    service     ssh $TARGET '$SUDO systemctl status $UNIT_NAME'

The dashboard has no authentication. If this host is on a network you do not
control, set SYSMON_ADDR in sysmon.env to one private address and put a proxy
in front of it -- docs/deployment/user.md, "Before you expose it".
EOF
