# Changelog

All notable changes to go-sysmon are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

Changes waiting for the next release are kept as fragments in `changelog.d/`,
one file per change named `<issue>-<slug>.md`. Each release folds them into
its own section.

## [0.3.6-alpha] - 2026-11-25

### Added

- **`make check-cpu-baseline`** fails when a binary's ELF notes demand a CPU
  above its architecture's baseline. `release.yml` runs it on the binaries it
  has built, before they are uploaded. To check any other binary, run `make
  check-cpu-baseline BINARIES=bin/sysmon-server`.
- **`make check-release-builder`**, part of `make lint`, fails when
  `release.yml` would build the release binaries in any image but the
  `Dockerfile`'s builder image, digest included.

### Changed

- **go-sysmon is licensed under the Apache License 2.0**, in place of MIT.
  `LICENSE` holds the Apache 2.0 text, the container image's
  `org.opencontainers.image.licenses` label reads `Apache-2.0`, and the README
  badge and License section say so. Releases before this one remain available
  under MIT.
- **GitHub is the only forge.** Every Gitea code path is gone: the Gitea
  release and image jobs in `release.yml`, the off-GitHub branches of the
  `ci.yml`, `release.yml` and `desktop.yml` conditionals, the
  `internal/gitea` package, and `.gitea/workflows` as a place `make
  check-pins` and `make check-latest` look for workflows. The jobs, runners
  and gates CI and releases run on GitHub are unchanged.
- **The daily check-latest run files its issue on GitHub.** The new
  `internal/github` package finds an open check-latest issue by the marker in
  its body and files one `task:ready` issue when none is open, with the
  workflow's own `GITHUB_TOKEN`. `check-latest.yml` runs on a daily schedule
  again. `cmd/checklatest` takes `-github-api` (default
  `https://api.github.com`) in place of `-gitea-api`, and reads its token from
  `GITHUB_TOKEN` by default.
- **Dependabot PRs merge themselves once CI is green.** The new
  `dependabot-automerge.yml` enables GitHub's native auto-merge on each
  Dependabot PR and replaces `automerge.yml`.

### Removed

- **The manual release targets.** `make release`, `make release-candidate`,
  `make release-alpha` and `scripts/release.sh` pushed a signed tag to the
  Gitea remote. A green merge that bumps `VERSION` releases instead
  (`auto-release.yml`).

### Fixed

- **The `linux-amd64` release binaries start on every x86-64 CPU again.**
  v0.3.4-alpha and v0.3.5-alpha needed a CPU with AVX2 (x86-64-v3). On an
  older one, such as an Intel Atom, glibc refused them with `CPU ISA level is
  lower than required`. They were built on GitHub's `ubuntu-26.04` runner,
  whose C toolchain targets x86-64-v3. `release.yml` now builds the server and
  desktop binaries, `amd64` and `arm64`, inside the `Dockerfile`'s builder
  image. Nothing changes on a newer CPU: Go builds for `GOAMD64=v1` as before,
  and its runtime still uses newer instructions where it detects them. The
  container image was not affected.
- **The README's binary install works on arm64.** It downloaded the
  `linux-amd64` build on every host, although each release also ships
  `linux-arm64` binaries. It now picks the build from `uname -m`, checks it
  against its `.sha256` file before installing, and says that the container
  image covers both architectures.
- **The README describes what ships.** It now covers NPUs (the Metrics table
  and `sysmon npu`), drive-bay health from the BMC, md and MegaRAID arrays,
  alerts, the fleet, Prometheus export and history, and configuration reload.
  The command list gains `host`, `npu` and `version`, the project layout lists
  every `cmd/`, `pkg/` and `internal/` directory, and the documentation table
  links the SPEC, the alerts/fleet/config page, delta streaming, remote
  deployment and testing. It no longer says there is no configuration file.
- **`docs/cli/reference.md` documents `sysmon npu`**, and
  `docs/collector/user.md` says what NPU and drive-bay readings need.
- **`docs/SPEC.md` describes the release pipeline as it runs**: a green merge
  to `main` that bumps `VERSION` is tagged and released by
  `auto-release.yml`, not by a signed tag pushed by hand. `ROADMAP.md`
  records the change, names v0.3.5-alpha as the latest release, links the
  GitHub release-candidate milestone, and stops linking issues that only
  existed on the retired Gitea forge.

## [0.3.5-alpha] - 2026-11-23

### Changed

- **A green merge to `main` that bumps `VERSION` publishes the release.**
  `auto-release.yml` runs when CI passes on `main`. If `v$VERSION` is not a
  tag yet, it checks the changelog and the version order, tags the commit and
  calls `release.yml`, which publishes the binaries, the GitHub release and the
  `ghcr.io` image. Merges that leave `VERSION` alone publish nothing.
- **Before 1.0, every release is the Latest release and `:latest`.** An
  `-alpha` used to be a GitHub pre-release that never moved the image's
  `:latest`, so both stayed on 0.1.2. From 1.0 on only a plain `vX.Y.Z` does.
  The install examples now follow the latest release instead of naming one.
- **Dependabot keeps Go modules, frontend packages, GitHub Actions and base
  images current**, with one grouped PR per ecosystem a day.
- **Node.js 26.13.1 LTS.** `.node-version` and the Dockerfile's frontend stage
  move from Node.js 24.21.0 to 26.13.1, the current LTS line (Lithium). The
  `golang:1.27.4-trixie` and `debian:trixie-slim` image digests move to the
  current builds of the same tags.
- **Analysis tools at their latest releases.** golangci-lint v2.14.0 -> v2.15.0
  and govulncheck v1.9.0 -> v1.10.0.
- **Go modules at their latest releases.** go-nvml v0.13.4-1, ghw v0.27.0,
  gopsutil v4.26.10, Wails v2.17.1, `go.yaml.in/yaml/v3` v3.0.6,
  `golang.org/x/mod` v0.43.0, `golang.org/x/net` v0.63.0, `golang.org/x/sys`
  v0.50.0, and every indirect module. No call site changed. go-nvml's NVML
  header adds a second die sensor, `TEMPERATURE_GPU_MAX`; memory temperature
  still comes from `FI_DEV_MEMORY_TEMP`.
- **Dashboard dependencies at their latest releases.** @playwright/test 1.65.1,
  svelte 5.59.1, svelte-check 4.7.8, @sveltejs/vite-plugin-svelte 7.3.2,
  tailwindcss and @tailwindcss/vite 4.3.5, daisyui 5.8.2 and @types/node 26.6.6.
  The postcss override moves from 8.5.26 to 8.5.33, because 8.5.26 sat below
  vite 8.3.8's own `^8.5.32` requirement. TypeScript stays at 6.0.3:
  svelte-check 4.7.8 rejects TypeScript 7 unless TypeScript 6 is installed
  beside it and its experimental `--tsgo` flag is used.
- **The Permissions-Policy header no longer names `browsing-topics`.**
  Chromium 159, the browser Playwright 1.65.1 drives, no longer has the Topics
  API, and it logged a warning about the unrecognised feature on every page
  load. The header lists only features Chromium recognises.

### Fixed

- **The Cinnamon applet keeps a 500 ms or 1 minute update interval when it is
  updated.** The interval was a spinbutton bounded at 500 and 60000 ms.
  Cinnamon's settings upgrade keeps a spinbutton value only when it lies
  strictly between those bounds, so an applet update reset both values to
  1000 ms. The setting is now a dropdown of exactly the intervals the server
  accepts, and Cinnamon keeps a dropdown value that is still one of the
  options. The desktop test now checks both the upgrade path and a saved
  settings file.

## [0.3.4-alpha] - 2026-11-23

### Security

- **Go toolchain 1.27.4 for GO-2026-6859, net/http.** govulncheck reports
  GO-2026-6859 in the go1.27.3 standard library, fixed in go1.27.4
  (2026-11-02). Every Go version pin moves to 1.27.4: the `go.mod` go
  directive, the digest-pinned `golang:1.27.4-trixie` builder in
  `Dockerfile`, and the `golang:1.27.4-trixie` images of the devcontainer and
  the desktop test.
- **echo v4.16.3, vite 8.3.8 and source-map-js 1.2.4.** These close the open
  advisories against `github.com/labstack/echo/v4` v4.16.1 (an indirect
  dependency through Wails' dev server), and against vite 8.2.1 and
  source-map-js 1.2.1 in the dashboard's build toolchain. The committed bundle
  in `cmd/sysmon/frontend/dist` is rebuilt with vite 8.3.8.

## [0.3.3-alpha] - 2026-10-28

### Added

- **Drive-slot (bay) health from the BMC.** A new read-only collector reads
  each drive bay's presence, health, fault LED, predicted failure, rebuild
  state and media life left over Redfish (`bmc.redfish_url`) or, when Redfish
  is not configured or fails, in-band IPMI. The result is the snapshot's
  `drive_slots` field, a **Drive bays** card on the Storage tab and the
  `sysmon_drive_slot_*` series. See `docs/collector/drive-slots.md` (#289).
- **A `drive_slot_fault` alert** fires for a faulted bay, which the kernel's
  block devices do not show when the drive sits behind a RAID controller.
- **Six `bmc.*` config keys**: `redfish_url`, `username`, `password_file`,
  `ca_file`, `insecure_skip_verify` and `timeout_ms`. All are boot-only.
- **The unit's device access.** `deploy/go-sysmon.service` now grants the
  service access to the IPMI device for the in-band source.

### Fixed

- **`Reload` defers and pins the `bmc.*` boot-only keys.** It compared and
  pinned only `server.*` and `fleet.tls.*`, so a new `bmc.redfish_url` went
  live at runtime while the settings view showed the boot source beside it.
  The six `bmc.*` keys are now reported as deferred and keep their boot values.
  The boot-only settings are one table, so a key can no longer be listed
  boot-only without `Reload` enforcing it.
- **A Redfish failure after the deadline is a timeout.** A response body cut
  off by the request deadline, or a response that arrives after it, is now
  reported as a timed-out `RedfishRequestError`, not as a decode error. Each
  request now takes the client timeout as its context deadline.

### Security

- **`golang.org/x/net` v0.60.0 -> v0.62.0** for GO-2026-6866 and
  GO-2026-6860, with the matching `golang.org/x/sys`, `golang.org/x/crypto`
  and `golang.org/x/text`.

## [0.3.2-alpha] - 2026-10-26

### Fixed

- **The Gitea image is pushed where the release runner may write.** The
  `Gitea Image` job now pushes to the namespace of the release runner's
  registry user; v0.3.1-alpha's push to the old namespace was refused.

## [0.3.1-alpha] - 2026-10-26

### Fixed

- **Gitea releases publish the container image.** The push now runs in its own
  `Gitea Image` job on the release runner and logs in with that runner's
  registry credential (`RELEASE_REGISTRY`, `RELEASE_REGISTRY_USER`,
  `RELEASE_REGISTRY_TOKEN`) instead of the workflow token, which
  the registry rejected on every release. The login lives in a private Docker
  config removed when the push ends.
- **Release notes claim an image only when it was pushed.** The Gitea release
  notes say the image was pushed only when the image job succeeded; a failed
  or skipped image job no longer blocks the binary release (#284).

## [0.3.0-rc.1] - 2026-10-25

### Added

- **NVMe write and read volume on `/metrics`.** NVMe drives that report SMART
  data units export `sysmon_disk_data_units_written_total`,
  `sysmon_disk_data_units_read_total`, and the same volume in bytes as
  `sysmon_disk_written_bytes_total` and `sysmon_disk_read_bytes_total` (one
  data unit is 1000 × 512 = 512000 bytes). All four are counters, so
  `increase(...[7d])` gives a drive's weekly write volume (#253).
- **Drive serial on the disk health series.** `sysmon_disk_smart_healthy`,
  `sysmon_disk_power_on_hours`, `sysmon_disk_wear_percent`,
  `sysmon_disk_spare_percent` and the new volume counters carry a `serial`
  label beside `device`, so a drive's history can be followed across a boot
  that renames NVMe devices. A drive that reports no serial has no `serial`
  label. Adding the label starts new series for those metrics; queries that
  select on `device` keep working (#253).

### Added

- **Project specification.** `docs/SPEC.md` lists every feature with its
  requirements and status, and `ROADMAP.md` names the SPEC feature on each
  line, with Complete and Future sections (#256).

### Changed

- **Network rates in bits per second.** The Network panel and the Overview show each
  interface's send and receive rates, and its traffic chart, in decimal bit
  units (bps, Kbps, Mbps, Gbps, Tbps), as links are rated. Byte totals stay in
  bytes (#257).

### Added

- **NIC manufacturer and model.** Each physical interface in `NetworkInfo`
  now carries its `bus` (`pci` or `usb`), `vendor_id` and `device_id`, a PCI
  board's `subsystem_vendor_id` and `subsystem_device_id`, and the `vendor`,
  `model` and `subsystem_vendor` names from `pci.ids` or `usb.ids` (a USB
  adapter's own strings when `usb.ids` is absent). The network panel shows
  them on one line per NIC; virtual interfaces report none (#258).

### Added

- **Wifi link detail.** The Network panel shows a wifi interface's link
  quality, signal level and noise floor (dBm), which the collector already
  read from `/proc/net/wireless`. The collector now also reports the SSID and
  BSSID of the associated network (`wireless.ssid`, `wireless.bssid`), read
  unprivileged from nl80211, and the Network panel and the CLI show them.
  Wired interfaces are unchanged (#259).

### Added

- **Peak disk throughput.** Each disk card in the Storage panel shows the peak
  read and write rates since the server started (`peak_read_bytes_rate`,
  `peak_write_bytes_rate`), beside the peak utilisation and queue length (#260).

### Added

- `GET /healthz` (liveness) and `GET /readyz` (readiness). `/readyz` answers
  503 with a reason when the latest snapshot is older than three stream
  intervals (never less than 10s) or the server is draining (#261).

### Changed

- The container `HEALTHCHECK` and `docker-compose.yml` probe `/readyz` instead
  of `/`, so a stalled collector marks the container unhealthy (#261).

### Added

- **Trusted reverse proxies.** `server.trusted_proxies` (`--trusted-proxies`,
  `SYSMON_SERVER_TRUSTED_PROXIES`) lists the CIDR prefixes of the proxies in
  front of a node. For a request from one of them, go-sysmon reads `Forwarded`,
  else `X-Forwarded-For` and `X-Forwarded-Proto`, and uses the client's scheme
  and address to send HSTS and to log the real client. Other peers' forwarding
  headers are ignored, malformed headers are ignored rather than refused, and an
  invalid prefix stops startup. Empty by default, so nothing changes unless it
  is set (#262).

### Security

- **`golang.org/x/net` v0.60.0 and Go 1.27.3.** x/net moves from v0.57.0 to
  v0.60.0 for CVE-2026-78669, and the Go toolchain from 1.27.2 to 1.27.3 for
  GO-2026-6809 (`crypto/x509`). `golang.org/x/crypto`, `golang.org/x/sys` and
  `golang.org/x/text` move with x/net (#263).

### Fixed

- **systemd notifications can no longer block forever.** Each `READY`,
  `WATCHDOG` and `STATUS` write to `NOTIFY_SOCKET` now has a one-second
  deadline (`sdnotify.SendTimeout`); a manager that stops draining its socket
  produces a `*types.NotifySendError` instead of stalling the watchdog loop.
  The abstract-socket unit test binds a name unique per run, so concurrent
  test runs sharing a network namespace cannot collide (#265).

### Changed

- **CI runs in two tiers.** Every pull request runs the PR tier: Format, Vet &
  Lint, Unit Tests (race), Build Binaries and Bundle Currency. The full tier
  adds Security Scan, FIPS, Integration Tests, Browser Tests, Desktop Extension
  Tests and Container Image, and runs on a push to `main`, on
  `workflow_dispatch`, and on a pull request that carries the `ci:full` label
  when it is opened. Adding the label later starts no run; dispatch the
  workflow on the PR's branch instead. CI Passed accepts a full-tier job as
  skipped only in a run that did not ask for the full tier (#267).
- **No scheduled workflows.** The desktop session tests and the toolchain
  currency check lost their `schedule:` triggers; their nightly runs are
  dispatched with `workflow_dispatch` (#267).

## [0.3.0-alpha] - 2026-10-05

### Added

- **Fleet capacity for schedulers.** Each node in `GET /api/fleet` and
  `GET /api/summary` now carries absolute capacity beside its percentages:
  `cpu_count`, raw `load_1`/`load_5`/`load_15`, `memory_total_bytes` and
  `memory_available_bytes`, free disk on the container runtime's data root
  (`disk_free_bytes`, `disk_total_bytes`, `disk_free_path`), and its
  containers' running count, CPU in cores and memory from the cgroup tree
  (`containers_observable`). An older peer's missing figures read as unknown,
  never as zero (#239).
- **Headroom at a utilisation target.** The aggregator adds `headroom` to every
  reachable node it lists: the CPU cores and memory free before the node reaches
  `fleet.headroom_target` (default 0.8, range (0, 1], reloadable). The fleet
  roll-up gains a `capacity` block that sums each figure, and the headroom, over
  the nodes that measured it, with its coverage (#239).
- **Free space on the container data root** is measured by every node with
  `statfs`, without the runtime's API: `virtualization.runtime_disk` in the
  snapshot (#239).

## [0.2.0-alpha] - 2026-09-29

### Added

- **Configuration file** (`/etc/go-sysmon/sysmon.yaml`), hot-reloaded on change
  and on SIGHUP. That includes a Kubernetes ConfigMap swap and the removal of the
  config directory. Decoding is strict, so a misspelled key is an error, not an
  empty setting (#11, #220, #227).
- **Fleet aggregation.** A node can poll named peers and show the whole fleet on
  one page. Each peer is polled for a summary, and the fleet is rolled up with
  health, RAID, SMART, thermal, power and swap-rate cells (#11, #46, #134).
  The fleet view is paged on the server and the roll-up is bounded (#15).
  Aggregators compose into a tree for fleets larger than one poller can serve (#190).
  An unreachable node shows how long it has been unreachable (#89).
- **Alerting**, including a drive that stops reporting SMART health (#11, #90).
- **Hardware RAID:** drives behind MegaRAID controllers are read and alerted on (#51).
- **Compute accelerators (NPUs)** are reported on any Linux host and shown on the
  Overview (#141, #154).
- **Prometheus:** optional remote write, date-range history, and per-node
  history persisted by the aggregator. Readings are stamped with their collection
  time (#27, #102, #173). A `/metrics` exposition also carries the swap paging
  counters (#157).
- **History charts:** counters are charted as rates, swap paging in bytes/s. Gaps
  are drawn as breaks. History can be searched by metric or resource
  (#159, #179, #109, #31).
- **Delta streaming:** the event stream sends patches instead of whole snapshots
  on opt-in (`?deltas=1`). The desktop window always uses them (#29, #104).
- **Swap paging rate** alongside swap occupancy, so a host that is actively
  paging can be told from one that merely holds swap (#132).
- **`GET /api/version`** and a UI footer showing the running build. Snapshots and
  fleet entries carry `git_commit` and `build_date`, so a node left on an old
  build is visible (#121).
- **Native systemd deployment** with `make deploy`: a hardened unit with a
  512M/768M memory guard and `CPUWeight` scheduling. `GOMEMLIMIT` is derived from
  the tightest cgroup ceiling (#71, #74).
- **A systemd watchdog** that pings only on evidence the service is working (#78).
- **Light/dark theme toggle** (#25).
- **Cinnamon applet settings** and a lazily built popup (#233, #234).
- **FIPS build** and a CI gate for it.
- **Release artifacts** published to Gitea releases by CI (#225).
- **Release checks:** a tag is refused unless it matches `VERSION` and
  `CHANGELOG.md` has a section for it. Every release job builds the commit that
  passed verification, and each publishing job confirms the tag still names it.
  `-alpha` tags are published as pre-releases.

### Changed — BREAKING: Go API of `pkg/collector` and `pkg/server`

These changes break code that imports `github.com/jeremyhahn/go-sysmon`.
The HTTP API, the CLI and the snapshot JSON removed nothing since v0.1.2.
The list is `apidiff -incompatible` of v0.1.2 against this release, for every
package.

- `collector.ErrSMARTPermission` is now `SMARTPermissionError`, and
  `collector.ErrSMARTIoctl` is now `SMARTIoctlError`. Error types follow Go's
  `...Error` naming, which a lint rule now enforces (#194).
- `(*collector.SystemCollector).WaitForRuntimeDiskUsage` and
  `(*collector.RuntimeCollector).WaitForDiskUsage` now return `error`. They
  wait for the query to finish instead of polling, and return the query's
  error, or the context's if it ends first (#195).
- `collector.MemoryCollector` and `server.Config` are no longer comparable
  with `==`.

### Changed

- The default build is cgo-free. NVIDIA GPU metrics through NVML need the
  `nvml` build tag, which the release and `make build-server` builds set (#148).
- On multi-socket hosts, cores are named by package and core on every surface,
  because both sockets number their cores from 0.
- The fleet list is ordered by name, not by severity (#23).
- The rate control drives the poller that feeds the view (#103).
- The Containers and Virtualization tabs are hidden on proven absence. The GPU
  and NPU tabs are hidden on hosts without that hardware (#187, #154).
- The dashboard loads from page-relative URLs, so it works behind a reverse
  proxy under a sub-path.
- Lists are never sent as JSON `null`, only as `[]` (#151).
- Host identity, `lsb_release` and the DIMM inventory are read once, not every
  tick. That cuts tens of thousands of forks and reads a day per host (#79, #82, #85).
- gopsutil's boot time is cached (#73).
- Each fleet peer's read buffer is kept between polls (#170).

### Removed

- The `alerts.swap_pct` config key, replaced by `alerts.swap_out_pages_per_sec`.
  Setting the old key is a startup error that names the replacement (#144).

### Fixed

- `swap_high` fires on the paging rate, not on how full swap is (#144).
- On a kernel without PSI, pressure is reported as unavailable, not as the same
  zeros an idle machine reports (#130).
- The fleet dashboard reports the cadence at which peers are actually polled (#131).
- The fleet health cells count devices and arrays actually checked. The SMART cell
  no longer labels a healthy host unhealthy (#47, #50, #54).
- A missing SMART reading says why: refused, hidden, or unsupported (#48, #146).
- The `HDIO_DRIVE_CMD` sector count is read correctly (#56). `doctor` counts
  drives behind a RAID controller and names a remedy that applies (#59, #63).
- Every `-blockdev` disk is recorded, whatever its format. CPU clock is read
  without sysfs where it is absent.
- SMBIOS is never read by escalating privileges, and DIMM temperatures never
  load modules or write to SMBus adapters. `doctor` explains how to grant access.
- Serve flags no longer override the config file with their own defaults (#117).
- SIGTERM received before the socket is bound shuts the server down (#107).
- Closing a subscriber twice no longer panics the process (#88).
- Ordinary response writes are bounded in time, and the event stream is exempt (#70).
- A monitor that has not collected yet is distinguished from one that never
  started (#118).
- Unmeasured CPU usage is reported as unknown, not as zero (#207).
- Disk-usage queries wait for completion instead of polling (#195).
- Prometheus: label values are escaped once, HELP text follows the spec, empty
  label values are omitted, and one duplicate series no longer costs a whole
  batch (#166).
- The UI keeps the unit on byte figures below 1 B or below zero. The Storage tab
  renders when one device is mounted twice. Selected table rows stay on their
  card's surface and can be operated from the keyboard. Dark-mode primary text
  is readable (#177, #168, #147, #165, #156).
- The desktop tray handles icon encode errors (#210). The Cinnamon applet
  sizes its gauge before paint (#232).

### Security

- Every response carries `nosniff`, a strict Content-Security-Policy,
  `X-Frame-Options`, `Referrer-Policy`, and cross-origin opener and resource
  policies (#200).
- `/api/config` and the fleet endpoints no longer disclose the fleet's topology
  or peer addresses to anonymous callers (#119, #120).
- The aggregator verifies that a peer's answer came from that peer, not from
  whatever answered on its address (#128).
- A fleet peer must have an explicit name. An unnamed peer, or an invalid name,
  is refused at startup and on reload instead of being named after its address
  (#37, #120).
- The container runtime stage takes base-image security updates (#143). The
  toolchain is kept on patched Go releases (#7).

Releases up to v0.1.2 predate this changelog. They are described on the
[release pages](https://github.com/jeremyhahn/go-sysmon/releases).

[0.2.0-alpha]: https://github.com/jeremyhahn/go-sysmon/compare/v0.1.2...v0.2.0-alpha
