# Remote deployment architecture

> **Status: NORMATIVE** · Scope: `make deploy`, the systemd unit and the service account · Last verified: 2026-08-19
> Conflicts with this document are defects. Fix them here first.

## The problem

Getting sysmon onto a server used to mean copying the binary and then a set of
manual steps nobody wrote down twice: pick a port, write a unit file, decide
whether to run it as root, remember to enable it at boot. The binary landed
reliably; everything that made it a *service* was folklore.

This feature is the second half: one command that leaves a remote host running
the dashboard, supervised, surviving reboot, and reproducible on the next host.

## The shape chosen

`make deploy` builds the server binary **on the build host**, copies it to the
target over the SSH connection it already has, creates a dedicated service
account, installs an environment file and a systemd unit, and starts the
service.

```
  build host                                    target host
  ----------                                    -----------
  make build-server ──┐
                      │  scp
                      └────────────────▶ /usr/local/bin/sysmon-server
  deploy/sysmon.env.example ── scp ────▶ /etc/go-sysmon/sysmon.env  (created once)
  deploy/go-sysmon.service  ── scp ────▶ /etc/systemd/system/go-sysmon.service
                                              │
                                              ▼
                                    systemctl enable --now
                                              │
                                              ▼
                              one process, one TCP listener
```

The target needs systemd, SSH key access and sudo. It does **not** need a
container runtime, a Go toolchain, Node, a registry account, or network access
to anything except the machine deploying to it.

### Why the binary runs directly under systemd, and not in a container

sysmon ships a `Dockerfile` and a `docker-compose.yml`, and running it in a
container is a supported way to use the program
([../server/container.md](../server/container.md)). It is not what this feature
does, and the reason is the kind of host this deploys to.

A container gives isolation you then have to spend back. A monitor that can
only see its own namespace is not a monitor, so the container has to run with
`pid: host`, `network_mode: host` and bind mounts of `/proc`, `/sys` and the
host's root — at which point the isolation is gone and what remains is a
runtime to install, keep patched and debug. On a machine whose entire job is
running one or two monitoring daemons, that is a dependency bought for nothing.

Running the binary directly also makes the sandbox *stronger*, not weaker. The
systemd directives in the unit (see "Privileges" below) drop every capability,
make the whole filesystem read-only and restrict the syscall set — controls the
host-networked, host-PID container explicitly gives up.

Rejected alternatives, and why:

| Alternative | Rejected because |
|---|---|
| Container under systemd | Adds a runtime to a host that needs no other containers, and the isolation has to be disabled for the program to work at all. |
| Distribution package (`.deb`) | Needs build and signing infrastructure this project does not have, and a repository to serve it from. Worth revisiting if sysmon is ever installed on many hosts. |
| Configuration-management module | Same reach as this script for one host, and a dependency the target would have to carry. |
| Pull-based (target fetches a release) | Makes the target depend on a registry or release host being reachable and authenticated at install time. The SSH connection is already required and already authenticated. |

### Why the environment file is never overwritten

`make deploy` creates `/etc/go-sysmon/sysmon.env` once and then treats it as the
operator's. A deploy that resets a hand-tuned interval or bind address is a
deploy that gets run once and then avoided. Nothing in the file is a build
output, so unlike a container image tag there is no field the deploy needs to
reclaim.

### Why the glibc check exists

The binary is dynamically linked, because CGO is required — the NVIDIA NVML
dependency does not build without it. A build host with a newer glibc than the
target therefore produces something that will not start there, and the symptom
is a dynamic-linker error at exec time that systemd reports as a failed unit
with no obvious cause.

The check compares the highest `GLIBC_x.y` symbol version the binary actually
references (read with `objdump -T`) against the target's runtime version, rather
than comparing the two hosts' glibc versions. The distinction matters: a build
on a glibc 2.39 host produced a binary needing only 2.34, which runs fine on a
2.36 target. Comparing the hosts would have rejected a working combination.

## Component boundaries

| Piece | Path | Owns |
|---|---|---|
| Entry point | `Makefile` (`deploy`) | Variable defaults, passing them to the script |
| Orchestration | `scripts/deploy.sh` | Preflight, copy, install, verification |
| Supervision and sandbox | `deploy/go-sysmon.service` | Boot ordering, restart policy, privileges, resource caps |
| Settings | `deploy/sysmon.env.example` | Defaults seeded into the target's env file |

Nothing here touches `pkg/` or `cmd/`. The deployment feature deploys the server
build exactly as it is; it does not change what sysmon does or how it collects,
and a change in collection behaviour is never a change to this feature.

## Data flow, and what is persisted

**Nothing is persisted.** sysmon reads the host live and keeps no history —
there is no database, no volume, no state directory, and nothing to back up or
migrate. Stopping the service loses no data because there is none. The unit
deliberately declares no `StateDirectory`, no `CacheDirectory` and no
`ReadWritePaths`; if one of those appears, something has started keeping state
and that is a design change, not a deployment detail.

The installation footprint is three paths:

| Path | Written by | Survives redeploy |
|---|---|---|
| `/usr/local/bin/sysmon-server` | every deploy | replaced each time |
| `/etc/systemd/system/go-sysmon.service` | every deploy | replaced when it changes |
| `/etc/go-sysmon/sysmon.env` | first deploy only | yes, never overwritten |

`sysmon.env` is the one piece of local state the deployment creates. It is
configuration, not data: deleting it and redeploying restores the shipped
defaults and loses nothing but the operator's own edits.

## Liveness: the watchdog

`Type=exec` reports a unit active while the binary exists. It says nothing about
whether a request can be answered, and the difference is not academic:
vm-host-2 served nothing for two days with `NRestarts=0`, the unit `active`,
and 2929 connections queued on a listener nothing was accepting from.

The unit is `Type=notify` with `WatchdogSec=60`. The service pings at half that,
and **only when both of these held in the same interval**:

1. **A real HTTP request to its own listener succeeded.** `GET /api/version`
   over the address the listener actually bound — not the configured string,
   which is frequently not dialable (`:8081` has no host; a specific bind
   address is not loopback). The response body is read, not just the status: a
   response whose headers arrived before a stall and whose body never followed
   is a served request only if nobody looks.
2. **The monitor collected within three polling intervals.** Three rather than
   one, because a single missed tick is ordinary on a loaded host and
   restarting for it would make the watchdog a source of outages. The interval
   is re-read on every check, since it is settable at runtime.

Each covers what the other cannot. Accept is what failed on vm-host-2, and a
check that does not cross the listener cannot see it; equally, a server
answering promptly with readings from before its poll loop stalled looks
perfect to the probe.

**The trap this design exists to avoid.** A goroutine that pings on a `time.Ticker`
allocates nothing and touches nothing, so it goes on pinging cheerfully through
exactly the garbage-collection stall that has parked every other goroutine in
the process. That watchdog converts "the service is dead" into "the service is
dead and systemd has confirmed it is fine". The tests assert the **absence** of
pings, because a test that only checks pings arriving passes against a bare
ticker.

The notification variables are cleared from the environment after use, so
nothing this process starts can satisfy a watchdog it has stopped satisfying —
a child pinging on behalf of a stalled parent is vm-host-2 again with a
different actor holding the pen.

**Verified end to end**, not only in tests: under a transient unit with
`WatchdogSec=10`, the service stayed `active` through four watchdog periods, and
a `SIGSTOP` — a process alive and unable to serve, which is the failure in
miniature — produced `Watchdog timeout (limit 10s)!` and a kill within one and a
half periods. With `Restart=always` that is a restart.

## Privileges

**The service runs with no capabilities at all**, as a dedicated unprivileged
system account. That is a measured result, not an assumption: every metric
surface was collected as root and as an unprivileged user on the same host and
compared. CPU package temperatures, PSI pressure stalls, per-interface counters
with driver and bond detail, cgroup v2 container metrics, load averages, disk
utilisation and the full process list with command lines are identical either
way.

Two metrics are not, and neither justifies a grant:

**SMART disk health.** Reading it needs the device node, which means the `disk`
group or root. On the reference target that buys nothing: as root, the
`HDIO_DRIVE_CMD(0xD0)` ioctl on the SATA SSD returns `EIO` — the drive refuses
the legacy ATA passthrough regardless of privilege. Verify on any new host with
`sysmon-server doctor` as root **before** granting anything; if it reports SMART
as unavailable there too, the grant is pure cost.

**SMBIOS DIMM detail** (module manufacturer, part number, serial, speed, form
factor). The collector reads `/sys/firmware/dmi/tables/DMI`, which is `0400
root:root`, and when that fails it retries through `sudo -n cat`. Running as a
service account with no sudo rights, that retry fails and the DIMM list is
empty. On the reference target the fields are AMI placeholder strings
(`Manufacturer00`, `SerNum00`) so nothing real is lost.

Note that `NoNewPrivileges=yes` in the unit makes the `sudo` retry
unconditionally impossible, by design — adding the service account to sudoers
would not change the outcome. Recovering DIMM detail would take
`AmbientCapabilities=CAP_DAC_READ_SEARCH`, and that capability bypasses **every**
file read permission on the host, including `/etc/shadow`. For an unauthenticated
dashboard that already publishes process command lines, that is a bad trade for
a placeholder string. It is the owner's call, not the deploying agent's.

### The `/proc` hardening trap

Two directives in the unit are set to their *permissive* values on purpose, and
both are load-bearing:

- `ProtectProc=default` — `invisible` hides every process the service account
  does not own, so the process list and process count silently go to zero and
  the dashboard renders an apparently idle machine.
- `ProcSubset=all` — `pid` hides `/proc/meminfo`, `/proc/stat`, `/proc/net/dev`
  and `/proc/pressure`, so memory, CPU, network and PSI collection all stop
  returning data.

Neither failure produces an error. A future pass that "hardens" these because a
linter suggested it breaks the program in a way no test on the deploy path would
catch.

`RestrictAddressFamilies` must likewise keep `AF_NETLINK`: Go's
`net.Interfaces()` enumerates interfaces over a netlink socket, and dropping the
family empties the network panel rather than failing loudly.

## Trust boundaries

**The dashboard has no authentication.** It publishes the hostname, the full
process list including command lines, disk serial numbers and network
configuration. Anything that can route to the port and is not a browser reads
all of it. `SYSMON_ADDR` exists so the listener can be confined to one
interface; on any network the operator does not control, binding a private
address or fronting it with an authenticating proxy is required, not advisory.

**The deployment mechanism runs `sudo` on the target over SSH.** It therefore
requires an account already trusted with root there, and grants no privilege
that account did not already have. It creates one account (`sysmon`) which has
no shell, no home directory and no sudo rights.

## What this feature deliberately does not do

- **It does not deploy the desktop build.** `bin/sysmon` links GTK, WebKit and
  libsoup and cannot start without a display; only `sysmon-server` is installed.
- **It does not install the Cinnamon applet or the system tray.** Those are
  workstation integrations, installed by `make install-extension-cinnamon` on
  the machine that has the desktop on it. They have no meaning on a headless
  host and the deploy will not put them there.
- **It does not install a container runtime, and does not use one.**
- **It does not migrate anything.** There is no prior state to carry forward, on
  the first deploy or the hundredth.
- **It does not remove or reconfigure anything already on the host.** It adds
  one account, one binary, one config directory and one unit.
- **It does not open firewall ports, configure TLS or install a reverse proxy.**
  Terminating TLS is supported by the server itself ([user.md](user.md),
  "Serving over HTTPS") but is not automated here.
- **It does not deploy to more than one host per invocation.** A loop over hosts
  is the caller's business; the script's contract is one target.
