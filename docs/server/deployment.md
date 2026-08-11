# Build and deployment

The server binary embeds the dashboard, so deploying it is copying one file.
No assets, no config, no database, no runtime on the target.

That is the short version. The rest of this page is the detail.

## Prerequisites

- **Go** — whatever `go.mod`'s `toolchain` directive pins. Go downloads it for you.
- **Node.js 22+** — builds the embedded frontend.
- **CGO** — not optional. `pkg/collector` imports NVIDIA's `go-nvml`, which does
  not compile with `CGO_ENABLED=0`.
- **Linux with glibc.**
- Docker, for `make integration-test` only.

### Desktop build dependencies

The desktop binary links GTK 3 and WebKit2GTK through Wails. These are needed
at build time *and* at run time, and only for `bin/sysmon` — `bin/sysmon-server`
needs none of them.

```bash
make deps          # picks the right packages for your distribution
```

By hand:

```bash
# Debian, Ubuntu, Mint
sudo apt install pkg-config build-essential libgtk-3-dev \
    libwebkit2gtk-4.1-dev libsoup-3.0-dev

# Fedora, RHEL
sudo dnf install pkgconf-pkg-config gcc gtk3-devel \
    webkit2gtk4.1-devel libsoup3-devel

# Arch
sudo pacman -S pkgconf base-devel gtk3 webkit2gtk-4.1 libsoup3
```

At run time the desktop binary needs the non-`-dev` equivalents:
`libgtk-3-0`, `libwebkit2gtk-4.1-0`, `libsoup-3.0-0`, `libglib2.0-0`.
`sysmon doctor` names whatever is missing and prints the install command.

## Make targets

### Building

| Target                | What it does                                   |
|-----------------------|------------------------------------------------|
| `make build`          | Frontend, then both binaries                   |
| `make build-server`   | The portable binary only — no GTK or WebKit needed |
| `make build-desktop`  | The desktop binary only                        |
| `make build-frontend` | Frontend assets only (`npm install`, check, build) |
| `make dev`            | Wails dev mode, with hot reload                |

`WITH_SYSTRAY=0` drops the tray from the desktop build. Tags are assembled as
`desktop,production,webkit2_41`, plus `systray` unless disabled.

### Testing

| Target                                             | Scope                          |
|----------------------------------------------------|--------------------------------|
| `make test`                                        | Everything, under `-race`      |
| `make test-cli`, `-collector`, `-monitor`, `-server`, `-types`, `-cmd` | One package    |
| `make coverage-cli`, `-collector`, `-monitor`, `-server`, `-types` | Coverage report |
| `make bench-cli`, `-collector`, `-monitor`, `-server` | Benchmarks                  |
| `make integration-test`                            | Full suite in a container      |
| `make integration-test-cli`                        | CLI integration suite on the host |
| `make test-e2e`                                    | Playwright, against a real browser |

The unit suite runs with the race detector because two genuine data races
shipped undetected while it did not.

### Checking

| Target            | What it does                                     |
|-------------------|--------------------------------------------------|
| `make lint`       | gofmt, `go vet`, then golangci-lint              |
| `make fmt`        | Rewrites files with gofmt                        |
| `make vulncheck`  | govulncheck across dependencies and the stdlib   |
| `make audit`      | `npm audit` on the frontend                      |
| `make ci`         | Everything CI runs, in the same order            |
| `make ci-fast`    | The same, minus containers and browsers          |

`make ci` calls the same targets the GitHub workflow calls, so the two cannot
drift apart. Green locally means green in CI.

### Releasing and deploying

| Target                         | What it does                                    |
|--------------------------------|-------------------------------------------------|
| `make release-build`           | Stripped, trimmed server binary into `dist/`    |
| `make release-build-desktop`   | The same for the desktop binary                 |
| `make deploy HOST=<host>`      | Builds, checks the target, installs, verifies   |
| `make install-extension-cinnamon` | Installs the panel applet                    |
| `make clean`                   | Removes build artifacts                         |

## Which binary

`make build` produces two binaries from the same source. Same subcommands, same
dashboard. They differ only in what they link.

| | `bin/sysmon` | `bin/sysmon-server` |
|---|---|---|
| Build tags | `desktop,production,webkit2_41,systray` | none |
| Shared libraries | 129 (GTK, WebKit, libsoup, …) | 3 (libc, ld.so, vdso) |
| Size | ~22 MB | ~15 MB |
| Native GUI window | yes | no |
| CLI and `serve` | yes | yes |
| Runs headless | **no** | yes |

The desktop build is a functional superset — everything the server build does,
plus a window. What it cannot do is start without a desktop stack. The dynamic
linker resolves those libraries before `main()` runs, so a missing one is a
loader error the program never gets a chance to explain:

```
bin/sysmon: error while loading shared libraries:
            libglib-2.0.so.0: cannot open shared object file
```

Workstations get `sysmon`. Servers get `sysmon-server`. `make deploy` copies
the latter.

Run `sysmon doctor` anywhere to see what that build can do on that machine.

## Running it

### Desktop

```bash
bin/sysmon                  # opens the window
bin/sysmon --no-tray        # window, no tray icon
```

### The web dashboard

```bash
bin/sysmon-server serve --addr :8080                  # every interface
bin/sysmon-server serve --addr 192.168.101.90:8080    # one interface
bin/sysmon-server serve --addr :8080 --interval 500   # faster polling
bin/sysmon-server serve --addr :8080                  # image inventory is automatic
```

The desktop binary serves the identical dashboard, on a host that has the GUI
libraries.

### CLI, from either binary

```bash
sysmon                    # one-shot overview when there is no display
sysmon host               # hostname, kernel, board, BIOS, uptime
sysmon cpu                # topology, per-core usage, temperatures, flags
sysmon memory             # usage, swap, per-DIMM detail
sysmon storage            # disks, SMART, queue depth, peaks
sysmon network            # interfaces, DNS, bridges, bonds, wifi
sysmon gpu                # NVIDIA, AMD and Intel
sysmon containers         # containers, with throttling and OOM history
sysmon vms                # virtual machines
sysmon images             # image inventory and reclaimable space
sysmon doctor             # what works here, and how to fix what does not
sysmon version

sysmon cpu --json         # every command takes --json
sysmon cpu --refresh 1    # and --refresh
sysmon storage --index 0  # narrow to one device
```

## Logging

Diagnostics go to stderr, not stdout. That is what keeps `sysmon cpu --json |
jq` parseable — and it means a plain redirect captures nothing:

```bash
sysmon-server serve > /tmp/sysmon.log                        # empty file
sysmon-server serve --log-file /var/log/sysmon/server.log    # what you wanted
```

| Flag | Default | What it does |
|---|---|---|
| `--log-file` | *(none)* | Writes to this path instead of stderr. Parent directories are created. The file is appended, never truncated, so a restart keeps the history that explains why the last run stopped |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | `text` | `text`, or `json` for log aggregators |

All three are global — every subcommand honours them, not just `serve`.

Under systemd, leave `--log-file` off and let the journal take stderr
(`journalctl -u sysmon -f`). Use it when running outside systemd.

### Conditions that cannot change are logged once

An unreadable SMBIOS table, or a SMART ioctl refused for lack of privileges,
will not resolve itself while the process runs. Warning about it every second
is noise that buries everything else. Those conditions are reported once per
run — once per device, where the condition is per-device — with a `note` field
saying so.

The difference is not marginal. The SMBIOS warning alone used to be 85% of the
log: 34 lines in 30 seconds, roughly 12 MB a day at a 1-second interval. The
same window now produces 9 lines, all at startup, after which the log stops
growing.

## Deploying to another host

### The easy way

```bash
make deploy HOST=192.168.101.90
```

Builds the binary, checks the target's architecture and glibc, installs to
`/usr/local/bin/sysmon-server`, and verifies it runs. `DEST=` overrides the
path; `HOST=user@host` picks a user.

### By hand

```bash
make build-server
scp bin/sysmon-server 192.168.101.90:/tmp/
ssh 192.168.101.90 'sudo install -m 0755 /tmp/sysmon-server /usr/local/bin/'
ssh 192.168.101.90 '/usr/local/bin/sysmon-server serve --addr :8080'
```

Then open `http://192.168.101.90:8080/`.

### What the target needs

| Requirement | Why |
|---|---|
| Linux, matching architecture | The binary is compiled for one |
| glibc at least as new as the build host's | CGO is mandatory (NVML), so the binary is dynamically linked |
| A free TCP port | That is genuinely all |

To find the minimum glibc a binary requires:

```bash
objdump -T bin/sysmon-server | grep -o 'GLIBC_[0-9.]*' | sort -uV | tail -1
```

If the target's glibc is older, build on the target, or build in a container
based on the target's distribution. Nothing else needs installing there — no Go
toolchain, no Node, no runtime.

## Running it as a service

`sysmon-server serve` from an SSH session dies with the session. For something
persistent:

```ini
# /etc/systemd/system/sysmon.service
[Unit]
Description=go-sysmon web dashboard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sysmon-server serve --addr :8080 --interval 1000
Restart=on-failure
RestartSec=5s

# The dashboard exposes host inventory and process listings and has no
# authentication, so run it unprivileged and confine what it can reach.
User=nobody
Group=nogroup
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now sysmon
systemctl status sysmon
```

Running as `nobody` costs you SMART data and DIMM temperatures, which need
privileges. Add `disk` to `SupplementaryGroups=` to get SMART back, or accept
the reduced detail — `sysmon doctor` will tell you exactly what you lost.

## Behind a reverse proxy

No upgrade headers are needed — the stream is an ordinary HTTP response. What
it does need is for the proxy to stop buffering it and to allow a long-lived
read, or the dashboard freezes on its first frame.

```nginx
server {
    listen 443 ssl;
    server_name sysmon.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
    }

    location /api/events {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;

        # Buffering is the whole problem: nginx would otherwise hold events
        # until its buffer filled, which for a stream means indefinitely.
        proxy_buffering off;
        proxy_cache off;

        # A stream that is working as intended never finishes.
        proxy_read_timeout 86400;
    }
}
```

The server already sends `X-Accel-Buffering: no`, which nginx honours on its
own; the explicit `proxy_buffering off` is belt and braces, and documents the
requirement for proxies that do not read that header.

### Terminating TLS in sysmon instead

A proxy is not required for HTTPS. The server can hold the certificate itself:

```bash
sysmon serve --addr :8443 \
    --tls-cert /etc/ssl/sysmon.crt \
    --tls-key /etc/ssl/sysmon.key
```

Both flags must be given together. TLS 1.2 is the floor. An unreadable or
mismatched key pair fails at startup with a `tls config:` error rather than at
the first request.

## Before you expose it

The dashboard has no authentication. It publishes hostnames, the full process
list, disk serial numbers and network configuration.

The event stream is subject to the browser's same-origin policy, so a random
web page cannot read it cross-origin. That is not authentication: anything that
can route to the port and is not a browser reads everything.

Bind it to a private interface (`--addr 192.168.101.90:8080`) or put an
authenticating proxy in front of it. Binding to `:8080` listens on everything,
which is convenient and, on an untrusted network, a mistake.

Two smaller notes:

- SMART and DIMM detail need root or the `disk` group.
- RAPL energy readings need read access to `/sys/class/powercap/`.
