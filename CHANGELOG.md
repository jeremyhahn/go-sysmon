# Changelog

All notable changes to go-sysmon are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

Changes waiting for the next release are kept as fragments in `changelog.d/`,
one file per change named `<issue>-<slug>.md`. Each release folds them into
its own section.

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
[Gitea release pages](http://git.example.internal/jeremyhahn/go-sysmon/releases).

[0.2.0-alpha]: http://git.example.internal/jeremyhahn/go-sysmon/compare/v0.1.2...v0.2.0-alpha
