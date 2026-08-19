# Deploying sysmon to a server

> **Status: NORMATIVE** · Scope: installing and operating a deployed instance · Last verified: 2026-08-19
> Conflicts with this document are defects. Fix them here first.

One command puts the sysmon dashboard on a Linux server as a supervised service
that starts at boot and restarts if it dies.

```bash
make deploy HOST=metrics.example.com
```

## Before you start

**On the target you need:**

- systemd, and an `x86_64` CPU
- SSH access with a key (not a password — the deploy runs unattended)
- `sudo` for that account, or SSH in as root and pass `SUDO=`

That is the whole list. No container runtime, no Go, no Node, no package
repository, and no outbound network access.

**On the machine you deploy from you need** the build prerequisites in
[../server/deployment.md](../server/deployment.md) — Go, Node and the C
toolchain — because the deploy builds the binary before it ships it.

**One more thing, and it is the one that bites:** the binary is dynamically
linked against glibc, so a build host with a *newer* glibc than the target can
produce something that will not start there. The deploy checks this for you and
refuses before writing anything, naming both versions. If it refuses, build on
an older host or in a container based on the target's distribution.

## First deploy

```bash
make deploy HOST=metrics.example.com
```

Expect roughly this, ending in a summary:

```
==> checking metrics.example.com
==> checking glibc compatibility
    binary needs glibc 2.34, target has 2.36
==> checking that port 8081 is available
    port 8081 is free
==> copying files to metrics.example.com
==> ensuring the sysmon service account exists
    sysmon created (system account, no home, no shell)
==> installing the binary to metrics.example.com:/usr/local/bin/sysmon-server
==> installing configuration to metrics.example.com:/etc/go-sysmon
    sysmon.env  created from the shipped defaults
==> installing go-sysmon.service
    unit installed, systemd reloaded
    enabled at boot
==> starting the service
==> verifying
    unit is active
    stayed up (no restarts)
    dashboard answered HTTP 200
```

Then open `http://metrics.example.com:8081/`.

The last section, "what this host can see", runs `sysmon-server doctor` on the
target and lists anything that host cannot report. Read it — it is how you find
out that a metric is missing because of the machine rather than because it is
genuinely zero.

## Before you expose it

**The dashboard has no login.** It shows the hostname, every running process
with its full command line, disk serial numbers, mounted filesystems and the
network layout. Anything that can reach the port sees all of it, and command
lines regularly contain things that were never meant to be read by strangers.

The shipped default listens on **every interface**. On any network you do not
control, change that. On the target:

```bash
sudo nano /etc/go-sysmon/sysmon.env      # set SYSMON_ADDR=10.0.0.5:8081
sudo systemctl restart go-sysmon.service
```

Or bind loopback only and put an authenticating reverse proxy in front:

```
SYSMON_ADDR=127.0.0.1:8081
```

## Choosing the port

The deployed default is **8081**. The binary's own default is 8080, which is the
port other services are most likely to want, so the deploy moves out of its way.

Check what is already listening before you pick:

```bash
ssh metrics.example.com 'ss -ltnp'
```

Two services on one port is a straight bind failure at boot, and the one that
loses is whichever starts second. The deploy checks the port before it writes
anything and refuses if something else holds it.

Set the port on the **first** deploy:

```bash
make deploy HOST=metrics.example.com SYSMON_ADDR=:9090
```

After that, `sysmon.env` on the target is authoritative and the deploy will not
overwrite it. Change it there and restart.

## Changing settings

Everything tunable lives in `/etc/go-sysmon/sysmon.env` on the target. Redeploys
never touch it.

| Setting | Default | What it does |
|---|---|---|
| `SYSMON_ADDR` | `:8081` | Listen address. Empty host means every interface. |
| `SYSMON_INTERVAL` | `2000` | Poll interval in milliseconds. |
| `SYSMON_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

```bash
sudo nano /etc/go-sysmon/sysmon.env
sudo systemctl restart go-sysmon.service
```

**On the interval:** CPU cost is very nearly linear in the poll rate. On a slow
4-thread machine, 1000 ms cost 16% of one core where 2000 ms cost about 8% and
5000 ms about 4%. Memory did not move. 2000 is shipped because a metrics
dashboard gains far less from sub-second resolution than a shared host loses.
Drop it to 1000 on a machine with cores to spare.

## Updating

Run the same command again. It is safe to repeat as often as you like.

```bash
make deploy HOST=metrics.example.com
```

Your `sysmon.env` survives. The binary is replaced and the service restarted; if
the unit itself has not changed, systemd is not even reloaded. There is no data
to migrate, because sysmon stores none.

## Day-to-day

```bash
systemctl status go-sysmon.service            # is it up, and what is it using
journalctl -u go-sysmon.service -f            # follow the log
journalctl -u go-sysmon.service -p warning    # just the problems
sudo systemctl restart go-sysmon.service      # after editing sysmon.env
/usr/local/bin/sysmon-server doctor           # what can this host see
```

A healthy service logs a handful of lines at startup and then nothing. If the
journal is filling up, that is a bug worth reporting, not something to configure
around.

`systemctl status` includes the memory and CPU the service is actually using.
Expect around 15–20 MB and a few percent of one core.

## What is not installed

- **The desktop app.** `bin/sysmon` needs GTK, WebKit and a display. Only the
  headless `sysmon-server` is deployed.
- **The Cinnamon panel applet and the system tray icon.** Workstation
  integrations; install those with `make install-extension-cinnamon` on the
  machine that has the desktop.
- **Any container runtime.** The service runs directly under systemd.
- **A firewall rule, TLS certificate or reverse proxy.** Yours to arrange.

Nothing already on the host is removed or reconfigured. The deploy adds one
account, one binary, one config directory and one unit, and touches nothing else.

## Troubleshooting

**`cannot ssh to <host>`** — the deploy uses `BatchMode=yes`, so this is always a
key-authentication failure and never a missed password prompt. Check with
`ssh -v <host> true`. A server offering only keyboard-interactive has public-key
auth turned off.

**`<host> has glibc X but the binary needs Y`** — your build host is newer than
the target. Build somewhere older, or in a container matching the target's
release.

**`PORT CONFLICT: something other than go-sysmon.service is listening`** — the
message includes the process holding it. Pick another port with
`SYSMON_ADDR=:<port>` or stop the other service.

**`go-sysmon.service is crash-looping`** — the service started and then exited
repeatedly. `Restart=always` means `systemctl status` will usually show it as
"active" anyway, which is why the deploy checks the restart counter instead. The
journal printed just above the error says why; the usual causes are a malformed
`SYSMON_ADDR` and a port that was taken between the check and the start.

**The dashboard is empty, or shows an idle machine that is not idle** — almost
always a hardening directive that hides data instead of erroring. Compare the
service against the same binary run by hand:

```bash
curl -s http://127.0.0.1:8081/api/snapshot | head -c 400
sudo -u sysmon /usr/local/bin/sysmon-server overview
```

If the second shows data the first does not, the sandbox is the difference — see
[architecture.md](architecture.md), "The `/proc` hardening trap".

**`SMART / disk health unavailable`** — the service runs unprivileged and cannot
open the disk device. Check whether privilege would even help before granting
any, by running `sudo /usr/local/bin/sysmon-server doctor` on that host: on many
drives the SMART ioctl fails for root too, and then there is nothing to grant.

**`image inventory unavailable`** — expected on a host with no container
runtime. Nothing is wrong.

**DIMM manufacturer and part numbers are blank** — the SMBIOS table is
root-only, and the service account cannot read it. See
[architecture.md](architecture.md), "Privileges"; on most boards the fields are
placeholder text anyway.

## Removing it

```bash
sudo systemctl disable --now go-sysmon.service
sudo rm /etc/systemd/system/go-sysmon.service /usr/local/bin/sysmon-server
sudo rm -rf /etc/go-sysmon
sudo systemctl daemon-reload
sudo userdel sysmon
```

There is no database, no log directory and no state to clean up.
