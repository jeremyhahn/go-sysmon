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

### The rate control drives the poller feeding the view

Owner ruling, 2026-09-04: *"the poller provides new data consistent with what
the user is expecting."* The dashboard's interval control therefore aims at
whichever poller produces what is on screen:

| what is on screen | poller | request |
|---|---|---|
| this node's own data | the local monitor | `{"interval_ms":N}` (or `"scope":"local"`) |
| the fleet, or a selected peer | the aggregator's peer refresh | `{"interval_ms":N,"scope":"fleet"}` |

Without this, choosing 250ms while viewing a peer changed the local monitor —
a cadence nobody was looking at — while the peer's figures kept arriving at
`fleet.poll_interval_ms`, and the control reported a rate nothing was collecting
at.

**A peer refresh is floored at one second** (`fleet.MinPollInterval`). Every
peer is fetched over HTTP on every cycle, on their CPU as well as this node's,
so a sub-second fleet cadence spends the aggregator's time asking ten machines
what changed in the last few hundred milliseconds. **The response reports what
is in force, not what was asked for**, and the header states the effective rate
beside the control — a dropdown reading 250ms over data collected once a second
is the same class of untruth as a stale reading shown as live.

`GET /api/interval` reports both cadences: `interval_ms` for this node's monitor
and `fleet_interval_ms` for the peer refresh, the latter absent on a node with
no fleet.

**The control never reaches into a peer's own poller.** Changing another
machine's collection rate from this dashboard is a question of ownership and
access that no ruling covers; the aggregator changes only how often it asks.

**The desktop build adjusts the same way.** It used to stop the monitor, build a
new one and start it, so the same control also discarded every reading the
monitor had accumulated — a second, unnamed action riding on "polling interval".

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

**Drives behind a hardware RAID controller raise the same storage rules as block
devices.** A failing member raises `smart_unhealthy` and a hot one raises
`disk_temp`, on the same thresholds — deliberately the same rule names, so
notification already configured on those rules catches a drive in a PERC
without being changed. What differs is the message, which names the **bay, the
model and the serial**, because a drive behind a controller has no device path
to print.

`raid_member_offline` is the one rule with no block-device equivalent: a drive
the controller has taken out of service. It exists because `raid_degraded`
cannot say *which* drive, and because a volume reported optimal with an offline
member is a contradiction that would otherwise pass in silence.

`smart_unreadable` is the other member-only rule, and its gate is the whole
design: it fires for a drive that **has answered a health command in this run
and has stopped**, and never for one that has not answered yet.

A drive that has never answered is a **capability fact** — some drives do not do
SMART behind some controllers — and warning about it would put a permanent
alert on every such host, which teaches people to ignore the category and is
worse than saying nothing. A drive that *stops* is an **event**. It is a warning
rather than a critical because an absent reading is not a failing drive.

"In this run" is literal: the state lives in the evaluator and **resets when the
process restarts**, so a drive already silent before a restart is treated as one
that never answered. That is the only claim a process can honestly make about
what it observed; persisting it would mean alerting on something nobody in this
process saw.

Member alerts are de-duplicated by **physical position**, not by serial. The
serial is read from the drive every cycle and goes missing on exactly the drive
an alert is about; keying on it would clear and re-fire a long-running alert
each time that read failed.

**That same string is what a threshold override must be written against.** A
member's resource is `<volume>/<enclosure>:<slot>` — `sdb/64:7` — so an
override for one drive is written for that, not for the volume:

```yaml
alerts:
  overrides:
    - resource: sdb/64:7      # this drive
      temp_warn_c: 60
```

An override on the volume (`sdb`) governs the volume's own alerts and **not**
its members. They are different resources, and a user who wrote one did not
write the other. This matters here more than elsewhere: drives behind a
controller run warmer than block devices in the same chassis, so a per-member
override is usually the first thing reached for.

| Tab | Rule | Threshold key | Default | Severity |
|---|---|---|---|---|
| storage | `raid_degraded` | — (always on) | — | critical |
| storage | `smart_unhealthy` | — (always on) | — | critical |
| storage | `raid_member_offline` | — (always on) | — | critical |
| storage | `smart_unreadable` | — (always on) | — | warning |
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
same-host redirects that keep the request's method are followed, up to five
hops.

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
  load per core, worst filesystem, RAID, SMART, temperature, power, throughput,
  uptime and **the go-sysmon version** (so a node left behind on an old build is
  visible).

  The two health figures carry a **denominator** — `raid_arrays` beside
  `raid_unhealthy`, `smart_capable` beside `smart_unhealthy` — and without them
  neither can be read.

  `raid_unhealthy` counts arrays that are degraded **or not serving I/O at
  all**. Those are different faults: a degraded array still works with reduced
  redundancy, while an `inactive`, `suspended`, `broken` or unreadable one does
  not work at all and raises a critical `raid_not_active` alert. The roll-up
  carried only `degraded`, so such a host read `ok` on the fleet card while its
  own alert said critical and its node page showed it red. The predicate is
  `alert.RAIDNotOperational`, shared with the rule, so there cannot be a second
  list of states to drift from the first. `raid_degraded` is unchanged —
  degraded only — for anything already reading it. `raid_degraded: false` is produced both by "this host has
  no arrays" and by "its arrays are fine"; `smart_unhealthy: 0` both by "no disk
  reported SMART" and by "every disk is healthy". The fleet view rendered each
  pair as a green `ok`, so a node with no RAID advertised a healthy RAID
  subsystem it does not have, and a host whose controller reports nothing
  advertised healthy SMART while its own node page said `N/A` for every device.

  Everything else already made this distinction — `DiskPanel` shows `SMART N/A`
  for a disk with `smart_enabled` false and omits the RAID badge entirely when
  there is no array, and the Prometheus exposition emits `sysmon_raid_degraded`
  only when an array exists and `sysmon_disk_smart_healthy` only when SMART is
  readable, on the stated principle that *"a missing series and a healthy one
  must not look the same to an alerting rule"*. The roll-up was the one surface
  that collapsed them, and the card now omits each cell when its count is zero,
  the same treatment swap, temperature and power already use.

  **A fault is shown even without a denominator.** The frontend is embedded in
  the binary that serves `/api/fleet`, so the two normally match — but a tab
  left open across a *rollback* holds a new bundle against an older server that
  sends no counts, and gating purely on the denominator would hide a genuinely
  degraded array behind a missing field. A missing denominator is a reason to
  say less about a healthy host, never a reason to stay quiet about a broken
  one.
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

### Remote write and history

`GET /metrics` answers "what is true now". To keep a past, add a `prometheus`
section. **Absent, it is off**, and the node behaves exactly as it did before
the section existed — no dependency on a Prometheus running anywhere. A fleet
where one node records history and the rest are standalone is the expected
shape, not a degraded one.

```yaml
prometheus:
  remote_write_url: http://mon0:9090/api/v1/write   # this node pushes
  query_url: http://mon0:9090                       # this node reads back
  sample_interval_ms: 15000
  flush_interval_ms: 30000
  batch_size: 1000
  timeout_ms: 10000
  external_labels:
    site: rack-3
```

The two halves are independent. `remote_write_url` makes the node push its own
metrics, so a Prometheus that cannot route to it still records its history;
`query_url` makes the node able to read ranges back, which is what puts the
range picker in its dashboard. Usually every node pushes and only the
aggregator reads.

**Each node pushes its own metrics.** Having the aggregator store what it
already polls would make one machine a time-series database whose cost grows
with nodes × metrics × retention, on a host chosen for being small — and it
would put the fleet's history behind the one box most likely to be busy. Pushing
is constant cost per node and adds nothing to the existing ones as the fleet
grows. Fleet polling stays what it is: an answer to "is this healthy right now",
never to "what happened on Tuesday".

Series are labelled `instance=<fleet.local_name>` and `job=go-sysmon`, so a node
relabelled in the fleet view is relabelled in its history too. `external_labels`
adds what only a deployment knows. A name that collides with one go-sysmon sets
itself — `instance`, `job`, or any label its metrics carry (`device`,
`interface`, `resource`, `gpu`, …) — is **rejected at load** rather than
ignored: a series carrying a label twice is refused by Prometheus, and it
refuses the whole write request with it, so one such entry would silently stop
the node's entire history.

Retargeting or renaming discards the buffer rather than carrying it over. A
backlog accumulated while endpoint A was unreachable belongs to A, and
delivering it to B would write one deployment's data into another's database
across whatever boundary the operator just moved the node over. Discarded
samples are counted in `sysmon_remote_write_samples_dropped_total_current`, so a
gap always has a number beside it.

Everything in the section is reloadable — `systemctl reload go-sysmon` turns
history on, retargets it, or switches it off without dropping a connected
browser. The sample interval is deliberately separate from `server.interval_ms`:
that one is the dashboard's stream rate, and a browser watching at 250ms must
not write four samples a second into a database.

The receiver needs `--web.enable-remote-write-receiver`.

Both clients refuse a redirect that leaves the configured host or steps https
down to plaintext, the same policy the fleet poller applies to peers
(`pkg/outbound`). A receiver that answers 302 could otherwise move a node's
metrics somewhere the operator never configured, with the write still reported
as succeeding. An upgrade to https on the same host is allowed, since that is an
ingress enforcing TLS.

They also refuse a redirect that **changes the request's method**, which is the
same-host case the two rules above do not cover. Go implements RFC 9110 §15.4:
a 301, 302 or 303 answering a `POST` is followed with a `GET` and no body. So a
proxy answering `/api/v1/write` with `302 -> /` turns a batch of samples into a
fetch of a landing page; the page returns 200, the write is reported as
successful and the batch is deleted. Same host, same scheme, no error recorded
anywhere — the only symptom is a gap in the graph. 307 and 308 preserve the
method and body and are still followed, so an ingress that moves the write path
keeps working.

A rejected write is retried or dropped according to its status, and the rule is
the protocol's rather than a judgement call. Remote-Write 1.0 says senders
"MUST retry write requests on HTTP 5xx responses" and "MUST NOT retry write
requests on HTTP 2xx and 4xx responses other than 429", and says receivers send
4xx only for a request that "will never be able to succeed and should not be
retried". So **429 and every 5xx keep the batch buffered; every other status
drops it** and counts it in `sysmon_remote_write_samples_dropped_total_current`.

408 is the one worth naming, because it reads like a timeout and is not treated
as one. A conforming receiver that wants a write retried sends 503; if it sends
408 it has said the request will never succeed, and holding the batch would
block newer ones behind it for nothing.

A node that is pushing also exports its own delivery health, and **that is worth
alerting on**: history that silently stops being recorded looks exactly like
history that has not happened yet.

```
sysmon_remote_write_enabled 1
sysmon_remote_write_buffered_samples 0
sysmon_remote_write_consecutive_failures 0
sysmon_remote_write_last_success_timestamp_seconds 1788300000
sysmon_remote_write_samples_written_total_current 41820
sysmon_remote_write_samples_dropped_total_current 0
```

Nothing is exported on a node that is not pushing, so a standalone node's
exposition is byte-for-byte what it always was.

The remote-write protobuf is written by hand, for the same reason the exposition
format is. Importing `prometheus/prompb` brings the whole Prometheus server
module — 257 modules in the build graph against this repository's 137 — for four
messages and six fields whose shape is fixed by the remote-write specification.
The encoder is pinned against golden bytes produced by the real `prompb`
encoder, so the oracle is the library rather than a second copy of the same
logic (`pkg/prom/testdata/`).

### The aggregator persists the fleet

Owner ruling, 2026-09-04: **the fleet's history is written by the aggregator,
not by the nodes.** A node with `fleet.peers` and a `prometheus.remote_write_url`
writes each peer's reading as it arrives, under that peer's own `instance`
label — so the peers keep no `prometheus` section, no storage, and no dependency
on a time-series database, and one machine holds the fleet's past.

Two properties of that write path are deliberate and neither is a detail:

- **The timestamp is the reading's, not the aggregator's clock.** A peer's
  history says when that peer measured, not when this node heard about it.
- **The cadence is the fleet poll interval, and nothing is resampled.** One
  reading in, one set of samples out. Writing on the exporter's own interval
  instead would either repeat a reading that happened once or discard readings
  that did happen — inventing observations in the first case, hiding them in the
  second. The resolution of a fleet series therefore *is*
  `fleet.poll_interval_ms`.

**A peer that does not answer contributes nothing.** The poller keeps its last
good snapshot so the fleet card shows stale data rather than emptying, but that
retained reading is never written again: an outage appears in the history as a
gap, which is what an outage is. A synthetic value would be an absence rendered
as a reading, and a gap that needs explaining is better than a number that needs
distrusting.

The submission path cannot slow the dashboard down. Peer readings are queued for
the exporter and dropped, counted, if that queue is full — so an unreachable
Prometheus costs history, never freshness.

### Two topologies, and which to choose

Owner ruling, 2026-09-04. Both are supported; they differ in **where the past
lives**, not in what the dashboard can do with it.

**Topology A -- aggregator persistence.** One node runs the fleet poller and a
`prometheus` section; the other nine have **neither**. They are pure realtime:
no time-series dependency, no storage, nothing to administer. The aggregator
writes every peer's reading under that peer's own `instance` label, so the
fleet's whole past is queryable from one dashboard. This is the preferred shape
and the one the fleet runs.

**Topology B -- node-local store.** A node points `prometheus.query_url` (and
usually `remote_write_url`) at a Prometheus on **its own host**. That node's
dashboard then gains the date picker and the history search for its own past,
independently of the aggregator -- *"so if the aggregator dies, the data
survives on the nodes."* Config-selected, never a default.

B needs no new mechanism. The two halves of the section are independent, and a
node reading back what it has written is the same code path as a node reading
the aggregator's store. What it costs is a Prometheus per node to run, back up
and upgrade, which is why A is preferred.

The halves may point at **different** stores, and that is a real arrangement
rather than a mistake: a node recording locally while its dashboard reads the
fleet's aggregate is exactly what keeps that node's history when the aggregator
is gone.

**One configuration to avoid, and this process cannot detect it.** If a
topology-B node writes to the *same* Prometheus an aggregator writes its peers
into, that node's series are written twice -- once by the node at its sample
interval, once by the aggregator at the fleet poll interval -- under the same
`instance` label. Prometheus accepts both, and a chart shows whichever samples
landed. Neither writer can see the other, so this is a rule for whoever writes
the config: **a node that an aggregator writes does not also write itself into
that aggregator's store.**

### Prometheus is a build tag, not only a config section

Owner ruling, 2026-09-04: *"when an endpoint is configured, use it; otherwise
pure realtime. The prometheus code compiles only under a build tag."*

`sysmon-server` is built with `-tags prometheus`; the **desktop and systray
builds are not**, so a realtime-only user carries no remote-write writer, no
protobuf encoder and no snappy. `make build-server` and `make release-build`
set the tag; `make build-desktop` does not.

**The endpoints exist in both builds and answer the same shape.** A binary
without the tag answers `/api/history/metrics` with `{"enabled":false}` and
HTTP 200 -- exactly what a node with no `query_url` returns -- and never a 404.
The dashboard hides its date picker on that envelope; on a 404 it would be
looking at a failure, and the picker's absence would stop being a decision and
become a symptom. "This node keeps no history" and "this build cannot" are the
same answer to the only question the dashboard asks.

Both shapes are built and tested: `make test` runs the suite twice, `make
build-both` compiles both, and CI does the same in the unit, build and FIPS
jobs. **A tag that is only ever compiled one way is not tested.**

Two guards keep the split honest, because splitting files does not enforce
anything -- one import in one untagged file puts `pkg/prom` back into the
desktop binary and everything still compiles:

- `TestPrometheusTagExcludesProm` walks the import graph **with build
  constraints applied** and fails if `cmd/sysmon`, `pkg/server` or `pkg/metrics`
  can reach `pkg/prom` without the tag.
- `TestPrometheusTagIncludesProm` requires `cmd/sysmon` to import `pkg/prom`
  **directly** when the tag is set. Reachability alone was too weak: the wiring
  file was deleted entirely and a reachability check stayed green, because
  `cmd/sysmon` reaches `pkg/prom` through `pkg/server` regardless.

### History: reading a range back

With `query_url` set, the dashboard gains a **date range control** in the header,
beside the polling interval: `Live`, six presets from 15 minutes to 30 days, and
an explicit start and end.

It is one control for the whole dashboard, and that shape is the point. Select a
range and the charts on the panel you are looking at redraw for that window,
together, so they stay comparable. Select `Live` and everything is the stream it
always was — there is no historical mode to leave, only a range that is or is
not set.

Two consequences are stated on screen rather than left to be inferred:

- A **full-width banner** says the charts are not live, and names the window.
  A chart that has silently stopped updating is the same lie as a frozen peer
  reading, and here it would be self-inflicted.
- The banner also says what is **still** live. Gauges, tables and counters show
  an instantaneous value and have no historical equivalent, so they keep
  streaming.

A chart with no counterpart in the catalog — per-core CPU usage and GPU link
throughput are collected but not exported as series — says so instead of
carrying on streaming beside charts showing last week.

Several charts share a metric: nine disks all draw `disk_read`. The request is
made once per metric per window, not once per chart, and each chart selects its
own device from the response.

The endpoint maps a **fixed catalog of keys** to PromQL rather than forwarding an
expression. This service carries no authentication of its own, so a pass-through
would make it an open query proxy for everything in the time-series database,
including whatever else writes to it. The node name is the one caller-supplied
part of the expression and is validated against a hostname pattern rather than
escaped — escaping alone would leave the safety of every future call site
depending on someone remembering to do it. It is also checked against this
node's name and its configured peers, since a whole fleet writes into one
Prometheus and a merely well-formed name would otherwise read any of them.

```
GET /api/history/metrics
GET /api/history?metric=disk_used&node=fs&start=<ms>&end=<ms>[&step=<ms>]
```

Both answer `{"enabled": false}` on a node with no `query_url`, rather than 404:
a standalone node is not a broken one, and the dashboard needs to tell those
apart to know whether to hide the picker or show a failure. A range that matched
nothing is `200` with no series — a node that was switched off for the window
genuinely has no data, and that is an answer rather than an error.

The server chooses the step when the caller does not, because it owns the
ceiling that makes one necessary: Prometheus returns at most 11000 points, and a
client picking its own would start failing on exactly the widest ranges. It is
reported back in the response, since a chart that does not know the step cannot
tell a gap in the data from the space between samples.
