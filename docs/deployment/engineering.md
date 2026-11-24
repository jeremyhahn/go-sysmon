# Remote deployment — implementation contract

> **Status: NORMATIVE** · Scope: `scripts/deploy.sh`, `deploy/go-sysmon.service`, `deploy/sysmon.env.example` · Last verified: 2026-08-19
> Conflicts with this document are defects. Fix them here first.

This feature has no Go surface. It is a shell script, a systemd unit and an
environment file; the contract below is what each guarantees.

## Exported surface

There is none. Nothing under `pkg/` or `cmd/` is added, changed or imported by
this feature, and no Go identifier exists to call. The entry points are:

| Entry point | Invoked as |
|---|---|
| `make deploy` | `make deploy HOST=<host> [USER=<user>] [...]` |
| `scripts/deploy.sh` | `HOST=<host> [USER=<user>] bash scripts/deploy.sh` |

`make deploy` depends on `build-server`, so the binary is always rebuilt from
the working tree before it ships. Running the script directly does not build;
it requires `bin/sysmon-server` to exist and exits 2 if it does not.

## Configuration

Every knob is an environment variable. The Makefile passes its variables through
to the script, so `make deploy VAR=x` and `VAR=x bash scripts/deploy.sh` are
equivalent.

| Variable | Default | Meaning |
|---|---|---|
| `HOST` | *(required)* | Target hostname or address |
| `USER` / `SSH_USER` | `$USER` | SSH account on the target |
| `CONF_DIR` | `/etc/go-sysmon` | Where `sysmon.env` is installed |
| `BIN_DIR` | `/usr/local/bin` | Where `sysmon-server` is installed |
| `SVC_USER` | `sysmon` | Service account, created if absent |
| `UNIT_NAME` | `go-sysmon.service` | Installed unit name |
| `UNIT_SRC` | `deploy/go-sysmon.service` | Unit source in the repo |
| `BINARY` | `bin/sysmon-server` | Binary to ship |
| `SYSMON_ADDR` | `:8081` | Listen address, applied **only on first deploy** |
| `SSH_OPTS` | `-o BatchMode=yes -o ConnectTimeout=10` | Passed to `ssh` and `scp` |
| `SUDO` | `sudo` | Set empty to deploy as root without sudo |

`CONF_DIR`, `BIN_DIR` and `SVC_USER` are rewritten into the unit on the target
by `sed` when they differ from the shipped defaults, so the unit always points at
the paths the deploy actually wrote.

Runtime configuration is separate and lives in `sysmon.env` on the target:
`SYSMON_ADDR`, `SYSMON_INTERVAL`, `SYSMON_LOG_LEVEL`. The unit declares the same
three as `Environment=` defaults *before* reading the file, so commenting a line
out falls back to a working value instead of passing an empty flag.

**Precedence, first deploy:** `SYSMON_ADDR` from the command line → shipped
default in `sysmon.env.example`.
**Precedence, every later deploy:** the target's `sysmon.env` wins over
everything. The script detects a mismatch and prints how to change it rather
than changing it.

## Invariants

1. **Idempotent.** Re-running converges. Every step is either a no-op when
   already satisfied or a converging write. A partial failure is recovered by
   running it again; there is no cleanup step and no lock.
2. **`sysmon.env` is written exactly once, ever.** The only code path that
   writes it is guarded by `[ -f ... ]`. Nothing in the file is a build output.
3. **The unit is only reinstalled when its content changes**, and
   `systemctl daemon-reload` runs only then. A no-op deploy performs no reload.
4. **The service account is created but never modified or removed.** A deploy
   against a host where `sysmon` already exists leaves its shell, home and group
   membership alone.
5. **The binary is replaced by rename, not by truncation.** `install(1)` writes
   `.sysmon-server.new` alongside the target and `mv -f` renames over it. A
   direct `install` to the live path fails with `ETXTBSY` while the old binary
   is running; the rename succeeds because the running process keeps its inode
   until it exits.
6. **Nothing is written outside `BIN_DIR`, `CONF_DIR`, the unit path and a
   `mktemp -d` staging directory**, and the staging directory is removed by an
   `EXIT` trap whether the script succeeds or fails.
7. **The service starts as an unprivileged account with an empty capability
   set.** See [architecture.md](architecture.md), "Privileges", for what that
   costs and why it costs nothing worth having.

## Failure semantics

The script is `set -euo pipefail`, and every remote step is followed by
`|| fail "..."` naming the cause. It **fails closed**: there is no step that
warns and continues after a write it could not complete.

| Exit | Condition |
|---|---|
| 0 | Service installed, active, not restarting, and answering HTTP |
| 1 | A named step failed; the message says which and what to check |
| 2 | Bad invocation — `HOST` unset, or `bin/sysmon-server` missing |

Preflight failures happen before anything is written to the target:

| Check | Failure mode caught |
|---|---|
| `ssh <target> true` | Key auth broken (BatchMode is on, so this is never a password prompt) |
| `uname -m` is `x86_64` | Architecture mismatch |
| `systemctl` present | Not a systemd host |
| glibc comparison | Binary needs a newer glibc than the target has |
| port free, or held by our own unit | A different service already on the port |

The one deliberate non-failure is the port check when `ss` is not installed: it
reports that it could not check rather than reporting the port free, because an
absent tool is not evidence of an absent listener.

## Verification, and why each step is there

`systemctl restart` returning 0 only proves the unit was accepted. The script
then checks three separate things, because each catches a failure the others
miss:

1. **`ActiveState` becomes `active`** within 30s — catches a unit that fails to
   start at all.
2. **`NRestarts` is still 0** after a further 8s — catches the crash loop that
   the first check cannot see. With `Restart=always`, a service that exits
   immediately and is restarted every 5s reads as `active` at almost any instant
   you sample it. This is the check that catches a bad flag or a port lost to a
   race.
3. **The dashboard answers HTTP on the loopback** — catches a process that is
   alive but not serving. This one is advisory: it is expected to fail when
   `SYSMON_ADDR` binds a single non-loopback address, so it downgrades to a note.

## Resource footprint

A collection tick's cost scales with **how much host there is to read**, the
process table above all, not with CPU speed. Measure on the host you deploy to,
and record its process count beside the figures:

```bash
CG=/sys/fs/cgroup/system.slice/go-sysmon.service
a=$(awk '/usage_usec/{print $2}' $CG/cpu.stat); sleep 120
b=$(awk '/usage_usec/{print $2}' $CG/cpu.stat)
awk -v d=$((b-a)) 'BEGIN{printf "%.2f%% of one core\n", d/1200000}'
cat $CG/memory.current $CG/memory.peak
grep nr_throttled $CG/cpu.stat
```

`memory.current` is what `MemoryMax` bounds and reads lower than RSS, because
file-backed pages of the binary are charged elsewhere. Memory is roughly flat in
the poll rate; CPU is roughly linear in it.

- **Read `nr_throttled`, not an average.** Collection is bursty: every fifth
  tick can use more than one core for seconds. A hard `CPUQuota` freezes the
  cgroup mid-burst, which stalls the Go GC and the listener (#65), so the unit
  uses `CPUWeight`.
- **Read `MemoryHigh` and `MemoryMax` as one setting.** The kernel enforces
  `max` first, so a watermark at or above the hard limit is unreachable and
  `memory.events` `high` reads zero. `sysmon doctor` reports the pair as
  degraded when `high >= max`.

**`GOMEMLIMIT` is not configured anywhere.** The service derives it from the
cgroup watermark at startup (`pkg/memlimit`, 90% of the ceiling) and logs what
it derived. The ceiling is the tightest `memory.high` and `memory.max` on the
path from the service's cgroup up to the root, because a parent's limit binds
every cgroup beneath it. An explicit `GOMEMLIMIT` in the environment still wins,
and the service logs that it deferred to it.

## Test strategy

There is no unit test for this feature and there should not be one: every
meaningful assertion involves a second machine, an SSH connection and a running
`systemd`, none of which a Go test can honestly stand in for. A mock would only
assert that the script calls the commands the script calls.

The gate is **execution against a real host**, and these are the cases that
matter:

| Case | Expected |
|---|---|
| First deploy to a clean host | Account created, all three paths written, service active |
| Immediate redeploy, nothing changed | `sysmon.env kept`, `unit unchanged`, no `daemon-reload` |
| Redeploy after an operator edits `sysmon.env` | The edit survives and takes effect |
| Redeploy while the service is running | Port reported as held by our own unit; binary replaced without `ETXTBSY` |
| `HOST` unset | Exit 2 before any connection |
| Port held by another service | Exit 1 at preflight, before anything is written |
| Build host glibc newer than the target's | Exit 1 at preflight with both versions named |

The verification worth running by hand after any change to the unit is a
**comparison of `/api/snapshot` from the sandboxed service against the same
binary run unsandboxed**. The hardening failures described in
[architecture.md](architecture.md) are silent — they produce empty lists, not
errors — so the only way to catch them is to look at the data. Check
specifically that `processes.total` is non-zero, `networks` lists every
interface, `sensors.psi` is populated and `cpus[].temperature_celsius` is
non-zero on hardware that reports it.

## Observability

The service logs to the journal under the `sysmon` syslog identifier; there are
no metrics and no audit records. A healthy start is **8 journal lines** and then
silence — the startup banner, the environment-check warnings for whatever this
host cannot see, and any once-per-run collector warnings. Steady-state logging at
`info` is zero lines per cycle by design; every permanent condition goes through
the collector's `warnOnce` helper.

```bash
journalctl -u go-sysmon.service -f          # follow
journalctl -u go-sysmon.service -p warning  # just the problems
systemctl status go-sysmon.service          # includes the cgroup's memory and CPU
```

If the journal is filling with a repeated line, that is a defect in the
collector that emits it, not a logging-configuration problem.
