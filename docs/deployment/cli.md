# Remote deployment — command reference

> **Status: NORMATIVE** · Scope: `make deploy`, `make deploy-binary` · Last verified: 2026-08-19
> Conflicts with this document are defects. Fix them here first.

The deployment feature adds no verbs to the `sysmon` binary. Its surface is two
Make targets. For the binary's own commands and flags see
[../cli/reference.md](../cli/reference.md).

## `make deploy`

Builds the server binary, installs it on a remote host as a supervised systemd
service, and verifies it is serving.

```bash
make deploy HOST=host.example.com
```

Depends on `build-server`, so the frontend bundle and the binary are rebuilt
from the working tree first. Idempotent: run it as often as you like.

### Variables

| Variable | Default | Meaning |
|---|---|---|
| `HOST` | *(required)* | Target hostname or address |
| `USER` | `$USER` | SSH account on the target |
| `SYSMON_ADDR` | `:8081` | Listen address — **first deploy only**, see below |
| `CONF_DIR` | `/etc/go-sysmon` | Where `sysmon.env` is installed |
| `BIN_DIR` | `/usr/local/bin` | Where `sysmon-server` is installed |
| `SVC_USER` | `sysmon` | Service account, created if absent |
| `UNIT_NAME` | `go-sysmon.service` | Installed unit name |
| `SSH_OPTS` | `-o BatchMode=yes -o ConnectTimeout=10` | Passed to `ssh` and `scp` |
| `SUDO` | `sudo` | Set empty when the SSH account is already root |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Service installed, active, not restarting, answering HTTP |
| 1 | A named step failed; the message names the cause and the check to run |
| 2 | Bad invocation — `HOST` unset, or the binary is missing |

### Examples

Deploy to a host, using a different SSH account:

```bash
make deploy HOST=metrics.example.com USER=deploy
```

Choose the port on the **first** deploy:

```bash
make deploy HOST=metrics.example.com SYSMON_ADDR=:9090
```

Bind one interface instead of all of them, on a network you do not control:

```bash
make deploy HOST=metrics.example.com SYSMON_ADDR=10.0.0.5:8081
```

`SYSMON_ADDR` is applied only when `sysmon.env` does not yet exist on the target.
Once it does, that file is authoritative and the deploy will not overwrite it —
it prints a note telling you to edit it there instead. To change the port on a
host that is already deployed:

```bash
ssh metrics.example.com "sudo sed -i 's/^SYSMON_ADDR=.*/SYSMON_ADDR=:9090/' /etc/go-sysmon/sysmon.env"
ssh metrics.example.com 'sudo systemctl restart go-sysmon.service'
```

Deploy as root, with no sudo on the target:

```bash
make deploy HOST=metrics.example.com USER=root SUDO=
```

Install somewhere other than the defaults:

```bash
make deploy HOST=metrics.example.com BIN_DIR=/opt/sysmon/bin CONF_DIR=/opt/sysmon/etc
```

The unit is rewritten on the target to match, so it starts the binary the deploy
actually wrote.

### Running the script without Make

Everything arrives in the environment, so the script stands alone. It does not
build, and requires `bin/sysmon-server` to exist:

```bash
make build-server
HOST=metrics.example.com USER=deploy bash scripts/deploy.sh
```

## `make deploy-binary`

Copies the server binary to a host and stops. Nothing supervises it and nothing
starts it at boot — this is for dropping a build somewhere to run by hand.

```bash
make deploy-binary HOST=metrics.example.com
make deploy-binary HOST=user@host DEST=/opt/bin
```

| Variable | Default | Meaning |
|---|---|---|
| `HOST` | *(required)* | Target, optionally `user@host` |
| `DEST` | `/usr/local/bin` | Install directory |

Then start it yourself:

```bash
ssh metrics.example.com '/usr/local/bin/sysmon-server serve --addr :8081'
```

That process dies with the SSH session. Use `make deploy` for anything you
expect to still be running tomorrow.

## What the two targets install

| | `make deploy` | `make deploy-binary` |
|---|---|---|
| `sysmon-server` binary | yes | yes |
| Service account | yes | no |
| systemd unit, enabled at boot | yes | no |
| `/etc/go-sysmon/sysmon.env` | yes, once | no |
| Restart on failure | yes | no |
| Resource caps and sandboxing | yes | no |
| Desktop binary (`sysmon`) | **no** | **no** |
| Cinnamon applet, system tray | **no** | **no** |
| Container runtime | **no** | **no** |

Neither target installs the desktop build or the Cinnamon panel applet. Both
need a display and the GTK/WebKit libraries, and have no meaning on a headless
host; the applet is installed by `make install-extension-cinnamon` on the
workstation that runs the desktop.

## Managing the service afterwards

These are plain systemd; nothing in this project wraps them.

```bash
systemctl status go-sysmon.service
systemctl restart go-sysmon.service
systemctl stop go-sysmon.service
systemctl disable --now go-sysmon.service     # stop and leave it stopped at boot

journalctl -u go-sysmon.service -f            # follow the log
journalctl -u go-sysmon.service -p warning    # just the problems
```

To see what the installed binary can read on that host:

```bash
/usr/local/bin/sysmon-server doctor
```

To remove it completely:

```bash
sudo systemctl disable --now go-sysmon.service
sudo rm /etc/systemd/system/go-sysmon.service /usr/local/bin/sysmon-server
sudo rm -rf /etc/go-sysmon
sudo systemctl daemon-reload
sudo userdel sysmon
```

Nothing else was written, and there is no data to clean up.
