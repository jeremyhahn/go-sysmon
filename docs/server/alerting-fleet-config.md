# Configuration, alerts, fleet and metrics

Four features that arrived together and depend on each other: settings that can
be reloaded without a restart, alert rules evaluated from those settings, a
fleet view built by polling peers, and a Prometheus endpoint that exports the
same evaluation the dashboard shows.

## Configuration

Precedence, lowest to highest: **built-in defaults → config file → environment
→ command-line flags**. A missing config file is not an error; defaults plus
environment are a valid configuration.

| Layer | Where |
|---|---|
| Defaults | `pkg/config.Defaults()` |
| File | `/etc/go-sysmon/sysmon.yaml` (`--config` to move it) |
| Environment | `SYSMON_` + the key, `.` → `_`, e.g. `SYSMON_ALERTS_DISK_FULL_PCT` |
| Flag | `--addr`, `--interval`, `--tls-cert`, `--tls-key` |

`deploy/sysmon.yaml.example` documents every key. `GET /api/config` returns the
effective value of each setting **and which layer supplied it**, which is what
the dashboard's Settings tab renders.

### The settings view is read-only, deliberately

There is no write path from the dashboard to the configuration. The service
carries no authentication of its own — it is designed to sit behind an
authenticating proxy — and on hosts with NVMe it runs with elevated
capabilities to read SMART. A config-write endpoint on that combination would
let anyone past the proxy reconfigure a near-root daemon. Settings change on
disk and are reloaded.

### Reload without restarting

Restarting to add a peer or retune a threshold would reset the Prometheus
counters, drop every connected browser and empty the 60-second history buffers.
Instead the configuration lives behind an atomic pointer that a reload swaps;
long-lived components read the current pointer each cycle rather than capturing
it at construction.

| Path | How | Notes |
|---|---|---|
| **SIGHUP** | `systemctl reload go-sysmon` | Primary. `ExecReload` is in the unit. Adds no HTTP surface. |
| **File watch** | `server.watch: true` | Watches the containing directory, so an editor that replaces the file still triggers it; debounced. |

**Those are the only two paths — there is no HTTP reload endpoint.** The
service carries no authentication of its own and sits behind an authenticating
proxy, so **no HTTP path changes its configuration**. `systemctl reload`
already delivers everything such an endpoint would have — add a peer or retune
a threshold without restarting, Prometheus counters and connected browsers
preserved — so nothing is lost by not having one.

The single endpoint that changes anything at runtime is `POST /api/interval`
described above: it retunes a transient refresh rate, writes no file, and is
discarded by the next reload.

**Reloadable:** alert thresholds, fleet peers, fleet poll intervals, and
`server.interval_ms` — the collection interval is pushed into the running
monitor, so a reload changes the cadence rather than only the reported value.

**Changed on the running service:** the refresh-rate selector in the dashboard
calls `POST /api/interval`, which retunes the monitor directly without touching
the file. While that override is in effect `GET /api/config` reports the rate
actually running, with source `runtime` rather than the configured source, so
the settings view and `GET /api/interval` never disagree about the live
service. **A reload discards the override** and puts the configured interval
back — reloading means "make the file true", and a rate picked in a browser is
not something the file knows about.

**Boot-only:** `server.addr`, `server.tls_cert`, `server.tls_key` and
`server.watch`. These are consumed once, when the process starts and the socket
binds. A reload reports each changed one in `deferred` **and keeps the boot
value as the effective one**, so `GET /api/config` can never report a setting
that is not actually in force — a value the file asks for but the running
service never adopted would be worse than no answer at all.

A reload that fails to parse — or whose file cannot be read at all — leaves the
running configuration untouched: a half-saved file must not take the daemon's
settings down with it. A reload that parses but is rejected by a runtime
component is different: it is already live for everything that accepted it, so
it is reported as a partial application, not as a failed reload.

One caveat follows from that. Settings are published before they are pushed
into the running components, so if a component rejects one, `GET /api/config`
reports the new value while that component keeps its previous one. The error in
the reload response — or the `reload applied, but some settings were rejected`
log line — is the signal that this happened, and it is the one case where the
settings view can be ahead of the running service. No setting behaves this way
today; the reloadable ones cannot be rejected.

**A missing config file is fine; an unreadable one is fatal.** Running with no
file at all is a supported deployment and starts on defaults. But a path that
cannot be examined — a parent that is not a directory, a directory the process
may not traverse — means the settings exist and cannot be seen, so startup
fails with an error instead of quietly booting on defaults. Silently discarding
an operator's TLS, listener, fleet and alert configuration while looking like a
healthy start is the worse outcome by far.

`GET /api/config` renders `server.tls_cert` and `server.tls_key` as
`(configured)` or `(not set)` rather than their paths. The endpoint is
unauthenticated by design, and the location of a private key is reconnaissance
that whoever reaches it should not be handed for free.

## Alerts

Rules are evaluated **on the server**, and the result travels in the snapshot as
`alerts[]`. That is what keeps the dashboard, `GET /api/snapshot` and
`GET /metrics` from ever disagreeing about whether a host is healthy, and it
puts the first-seen timestamp next to the state that already spans snapshots.

Every rule carries the tab it belongs to, so the bell can jump straight to the
panel that explains it.

| Tab | Rule | Threshold key | Default | Severity |
|---|---|---|---|---|
| storage | `raid_degraded` | — (always on) | — | critical |
| storage | `smart_unhealthy` | — (always on) | — | critical |
| storage | `raid_resync` / `raid_mismatch` | — | — | info / warning |
| storage | `nvme_spare_low` | `nvme_spare_pct` | 10 | critical |
| storage | `nvme_wear_high` | `nvme_wear_pct` | 80 | warning |
| storage | `disk_full` | `disk_full_pct` / `disk_full_crit_pct` | 90 / 95 | warning / critical |
| storage | `disk_util` | `disk_util_pct` | 90 | warning |
| storage | `disk_queue` | `disk_queue_len` | 8 | warning |
| storage | `disk_temp` | `temp_warn_c` / `temp_crit_c` | 70 / 85 | warning / critical |
| storage | `psi_full` (io) | `psi_full_avg10` | 10 | warning |
| cpu | `cpu_high` | `cpu_pct` | 90 | warning |
| cpu | `cpu_temp` | `temp_warn_c` / `temp_crit_c` | 70 / 85 | warning / critical |
| cpu | `cpu_throttle` | — | — | warning |
| cpu | `load_high` | `load_per_core` | 2.0 | warning |
| cpu | `psi_full` (cpu) | `psi_full_avg10` | 10 | warning |
| memory | `memory_high` / `swap_high` | `memory_pct` / `swap_pct` | 90 / 50 | warning |
| memory | `psi_full` (memory) | `psi_full_avg10` | 10 | warning |
| network | `iface_down` | — | — | warning |
| network | `iface_errors` | `iface_err_rate` | 1 per s | warning |
| network | `iface_util` | `net_util_pct` | 80 | warning |
| gpu | `gpu_temp` | `gpu_temp_warn_c` / `gpu_temp_crit_c` | 85 / 95 | warning / critical |
| gpu | `gpu_memory_high` | `gpu_memory_pct` | 90 | warning |
| gpu | `gpu_power_high` | `gpu_power_pct` | 95 | warning |
| containers | `container_memory_high` | `container_mem_pct` | 90 | warning |
| containers | `container_cpu_high` | `container_cpu_pct` | 90 | warning |
| virt | `container_runtime_unreachable` | — | — | warning |

Setting a threshold to **0 disables its rule**. A degraded array and a failing
SMART attribute have no sensible "off" and are not configurable.

Three rules are deliberately narrower than they first appear, because a noisy
bell trains people to ignore it:

- **`iface_down`** fires only when the interface has an address configured.
  Hosts routinely carry unused NICs that are legitimately down.
- **`cpu_throttle`** fires on the throttle counter *rising*, not on a machine
  that throttled at some point in its past.
- **`psi_full`** uses *full* pressure (everything stalled), not *some*.
- **`iface_util`** is skipped where the link speed is unknown (virtual and
  bridged interfaces report 0); a percentage of an unknown capacity is noise.
- **`container_memory_high`** needs the container to have a memory limit.
  Without one the percentage is a share of the whole host and says nothing
  about the container approaching a wall.
- **GPU utilisation is deliberately not a rule.** A GPU at 100% is the card
  doing its job. Only the edges of its envelope — temperature, exhausted video
  memory, the power limit — are reported.

### Per-resource overrides

```yaml
alerts:
  overrides:
    - resource: md0        # a volume expected to run nearly full
      disk_full_pct: 96
```

Only the keys present are overridden. Thresholds are evaluated on each node
against **its own** configuration, so an exception belongs on the node that owns
the resource, not on the aggregator.

## Fleet

Peers turn a node into an aggregator: the dashboard opens on a view of itself
plus its peers, and any node can be drilled into. **A node with no peers behaves
exactly as a standalone node** — the fleet endpoint reports `enabled: false` and
no fleet tab appears.

Aggregation is server-side. A browser fanning out to each node would need CORS
on every peer and would break the single-origin model that lets the whole
dashboard sit behind one authenticating proxy. Here the browser talks only to
the node it loaded from; that node polls the peers and caches their snapshots.

Put the aggregator on a host that does nothing else. A fleet dashboard is what
you open when something is wrong, so it must not be served by a machine that
could itself be the thing that is wrong: an overloaded or swapping workload host
would take the view of the whole fleet down with it, exactly when it is needed.

`fleet.local_name` sets how a node names itself in the view; it defaults to the
hostname.

**An unrecognised key is an error, not a comment.** The file is decoded
strictly: a key that maps to no setting fails the load and names itself. This
matters most for near-misses, which are otherwise invisible -- `peer:` for
`peers:` leaves the fleet empty, and `disk_full_pcnt:` quietly keeps the default
threshold while the file appears to set it. In both cases the daemon would start
clean and report health for a configuration nobody wrote. Renaming or removing a
setting is therefore a breaking change for files that still carry it, which is
deliberate: an obsolete key is discovered at the restart that follows the
upgrade rather than by wondering why a threshold stopped applying.

**A peer entry must carry a URL.** `- name: db-prod` with the `url:` line
missing or misindented is rejected at load, not skipped. Skipping it was the one
unusable-peer case that failed open: the service started cleanly, `db-prod` was
absent, and every remaining card looked healthy -- the operator asked for a
fleet of N and silently got N-1. A missing `url:` is also the likeliest of these
mistakes to be an ordinary typo.

**A peer URL must be absolute `http`/`https` with a host** — `http://host:8081`.
`localhost:8081` (no scheme), `/relative/path`, `ftp://host` and
`http:///missing-host` are all rejected at load. Every one of them parses, and
none can be fetched: accepting them would publish a peer the poller can never
reach while the service reports a clean start, which is the failure mode least
likely to be noticed.

**A peer may not share this node's name.** `fleet.local_name`, or the hostname
when it is unset, identifies the local node, and the fleet endpoints resolve
that name before consulting the peer cache — so a peer named after this node
would render as a second same-named card and answer with the LOCAL machine's
snapshot and configuration, leaving the real host unpolled behind a card that
looks like it. Rejected when the service starts, and again on every reload
before the new configuration is published, so a file edited into a collision is
refused with the running configuration left in place.

Unlike the other fleet rules, this one is **not** checked while parsing. The
local name falls back to the hostname, so folding it into the parse would make
the same file valid on one machine and invalid on another — a config file has to
mean the same thing wherever it is read. The check therefore takes the name as
an input (`(*Config).ValidateAgainstLocalName`) and the serve command registers
it as a validator; the name is read from the configuration being validated, not
from the running one, because `fleet.local_name` is itself reloadable.

**A poll will not follow a redirect off the configured peer.** The poller
reports whatever it decodes as that peer's authoritative health, so where the
response came from is a trust decision. The default HTTP client follows
redirects anywhere, which would let an `https://` peer be walked down to
plaintext or pointed at another host entirely -- and the dashboard would still
label the answer with the configured peer's name, showing a healthy node while
the real one is failing. A redirect that downgrades `https` to `http`, changes
host or port, or leaves http/https altogether is refused and the poll fails;
same-host redirects are followed, up to five hops.

This narrows the window rather than closing it: a peer polled over plaintext
`http` is still unauthenticated, which is tracked in the repository issues
alongside the unauthenticated read endpoints.

**Peer URLs must not carry credentials.** `https://user:token@peer` is rejected
at load, as is a bare `user@peer` — authenticated peers are not supported, and
the fleet view copies peer URLs into `GET /api/config`, which has no
authentication of its own. Rejecting rather than redacting means the secret is
never stored, never rendered and never logged; the error names the peer and its
host but never the URL. A URL that cannot be parsed at all is also a load
error, rather than a connection failure repeated on every poll.

**Peer names must be unique.** A peer's name is its identity throughout the
fleet subsystem — it keys the poller's result cache, titles the card in the
view, and selects the node for `GET /api/fleet/{name}/...`. Two peers sharing a
name would render as one card showing whichever host answered last alongside
the other's configuration, so a duplicate is rejected at load rather than
silently de-duplicated: dropping one would leave a real host unmonitored while
the dashboard looked complete. A peer with no `name` is identified by its
`url`, which is applied before the uniqueness check — so two unnamed peers on
different hosts are fine, and the same URL listed twice is not.

- `GET /api/fleet` — one summary per node: status, alert counts, CPU, memory,
  load per core, worst filesystem, RAID degraded, unhealthy SMART count,
  temperature, power, throughput, uptime and **the go-sysmon version** (so a
  node left behind on an old build is visible).
- `GET /api/fleet/{name}/snapshot` — that node's full snapshot from the cache,
  wrapped with the freshness of the poll behind it:
  `{snapshot, reachable, error, age_seconds}`.

  The wrapper is the point. The poller deliberately keeps a peer's last good
  reading when it stops answering, so a bare snapshot looks the same whether it
  was collected a second ago or before the host disappeared. `reachable`
  describes the **latest** poll and can be false while `snapshot` is still
  present — that is the stale case. A client must mark it rather than fold it
  into a live series: re-applying the same cached reading on every cycle draws a
  flat line that reads as "measured and steady" when nothing was measured. The
  dashboard shows a `stale · Nm old` badge beside the node name and stops
  recording history until the peer answers again.

Peers are polled concurrently, each with its own timeout, so one dead node
cannot delay the refresh. A peer that stops answering keeps its last good
snapshot alongside the error, so a brief blip reads as stale data rather than an
empty card, and an unreachable node is never rendered as a healthy one.

## Prometheus

`GET /metrics` serves the latest snapshot in text exposition format. The host is
deliberately **not** a label: Prometheus attaches `instance` at scrape time, and
duplicating it would create two competing identities for the same machine.

Series include `sysmon_cpu_usage_percent`, `sysmon_memory_used_percent`,
`sysmon_load1`, `sysmon_psi_{some,full}_avg10{resource}`,
`sysmon_power_watts{source}`, `sysmon_disk_{util_percent,queue_length,
used_percent,temperature_celsius,smart_healthy,wear_percent}{device}`,
`sysmon_raid_{degraded,active_devices,sync_percent,mismatch_count}{array}`,
`sysmon_network_*{interface}`, and the alerts:

```
sysmon_alert{rule="raid_degraded",severity="critical",resource="md2",category="storage"} 1
sysmon_alerts_firing{severity="critical"} 1
```

A healthy array still exports `sysmon_raid_degraded 0`. A missing series and a
healthy one must not look the same to an alerting rule.

The exposition is written directly rather than through the client library: every
value is a plain gauge already held in finished form, so the library would add a
large dependency tree for no gain on hosts this is deployed to.
