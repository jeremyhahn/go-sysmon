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

Every layer is reachable for every key, the listen address included: `:8081` in
the config file, `SYSMON_SERVER_ADDR=:8081`, or `--addr :8081`, in that rising
order. A flag counts only when it was actually given — one left at its default
does not displace the file.

**`SYSMON_ADDR` in `sysmon.env` is not the environment layer.** It is a systemd
variable that the unit interpolates into `--addr`, so it arrives as a *flag* and
outranks a `server.addr` set in the config file. The environment layer is spelt
`SYSMON_SERVER_ADDR`: the prefix, then the config key with `.` replaced by `_`.

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

**`fleet_interval_ms` is the interval plus the time the last refresh took, so it
is not the same number as `fleet.poll_interval_ms`.** The poll loop refreshes
every peer and *then* sleeps for the interval, so the cadence peers are actually
polled at is interval + work. On mon0, configured at 3000 ms with a refresh
costing about 200 ms, peers are refreshed every 3.20 s — measured, as an
autocorrelation peak at exactly 3.20 s in that node's CPU trace (#76).

The endpoint therefore reports 3200 where the config file says 3000, and that is
deliberate. Reporting the configured value restates the file rather than
describing the service, which is the same class of untruth as a dropdown reading
250 ms over data collected once a second. It reports the **last** cycle rather
than an average: a cycle in which a peer times out genuinely does stretch, and
smoothing it would trade a true jumpy number for a calm false one.

The loop is still "work, then sleep" rather than a ticker, and that is a
deferral rather than an oversight. Converting it would poll every 3.00 s instead
of 3.20 s — 6.7% *more* aggregation on mon0, the node with the least CPU to
spare and where aggregation is 94% of the sysmon process's CPU. That is a
correctness fix paid for with the resource under pressure, so it is held until
the larger question is settled: whether the poll path should carry peer process
lists at all, which would remove most of the load and make the ticker free.

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

### What the settings view withholds

`GET /api/config` is unauthenticated by design — go-sysmon sits behind an
authenticating proxy — so it reports **whether** each of these is set up, as
`(configured)` or `(not set)`, and never what it is:

| Withheld | Why |
|---|---|
| `server.tls_cert`, `server.tls_key` | The location of a private key. |
| `prometheus.remote_write_url`, `prometheus.query_url` | The address of the fleet's time-series database. |
| `config_file` | An absolute path is a map of the host's filesystem layout. |
| `fleet.peers.<name>` | Another machine's scheme, host and port. |

Peers keep their **names**. The name is how the dashboard, the fleet proxy and
the history queries all address a node, and the browser reaches a peer through
`/api/fleet/{name}/...` on this node rather than by dialling it — so the URL
buys a client nothing and hands an anonymous one the internal topology. Together
with the config file's path it let a single reachable listener enumerate
hostnames, ports, transport choices and filesystem layout (#16).

**A peer with no `name:` is keyed by its URL's host**, which is its name (see
*A peer's name is its fleet-wide identity* below). Before #37 such a peer took
its whole URL as its name and was listed as `fleet.peers.(unnamed 1)` so the
address stayed off the wire; that is no longer needed. No name can carry a
scheme, port or path, and the host is the node's identity — the string
`GET /api/fleet` titles its card with and routes `/api/fleet/{name}/...` by —
so withholding it here would hide nothing.

`GET /api/fleet` still carries each peer's `url`, which the fleet card renders.
That is the same exposure on a different endpoint and is tracked separately.

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
| memory | `memory_high` | `memory_pct` | 90 | warning |
| memory | `swap_high` | `swap_out_pages_per_sec` | 200 | warning |
| memory | `psi_full` (memory) | `psi_full_avg10` | 10 | warning |

`swap_high` is a **paging rate**, not an occupancy. Swap that is merely resident
costs nothing: pages parked there by a burst that ended hours ago stay counted
until something reclaims them, so occupancy answers "how much is out there" and
never "is this host paging now". Two healthy fleet nodes sat amber indefinitely
on the old occupancy threshold while a host genuinely thrashing with a small swap
device stayed silent.

The rule needs two consecutive intervals over the threshold before it fires,
because launching something large pages hard for one interval and then stops. It
stays silent when `/proc/vmstat` could not be read (`swap_counters_available` is
false), on the first evaluation of a run, and when the counter goes backwards
across a reboot — in each case there is no rate to report, and reporting zero
would be indistinguishable from a quiet host.

The old `alerts.swap_pct` key is gone, and a configuration that still sets it
**will not start**: the loader decodes strictly, so a key with no field behind it
is an error rather than a comment. Rename it to `alerts.swap_out_pages_per_sec`
and give it a rate — the value does not carry over, because 50% occupancy and a
paging rate are not the same quantity.

The error names the replacement rather than leaving you to find it:

```
/etc/go-sysmon/sysmon.yaml sets alerts.swap_pct, which this build no longer has:
replaced by alerts.swap_out_pages_per_sec, which is a paging RATE in
pages/second rather than an occupancy percentage. …
```

Accepting the key and ignoring it was the alternative, and it is the one thing
strict decoding exists to prevent: the file would go on stating a threshold the
daemon no longer applies.
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

**What a poll costs is JSON decoding, almost entirely.** Each cycle fetches every
peer's full `/api/snapshot` (about 2.9 MB across the fleet in September 2026,
from 66 KB for gitea to 715 KB for ci-node-2) and decodes it into a
`types.Snapshot`. Profiled on those real bodies, `encoding/json` is over 80% of
the cycle. Remote-write sampling of the readings adds about 4%, and serving the
fleet view about 4 µs per request. Process lists are about 60% of the bytes and
the per-CPU `flags` arrays about 25%.

Each peer keeps its read buffer between cycles (`pkg/fleet/body.go`). Peers
answer chunked with no Content-Length, so a fresh reader grows by doubling on
every poll. Reusing the buffer cut per-cycle allocation by 58% on the fleet's
real bodies. It took only 7–13% off the CPU, because the decode itself is the
cost. What remains can only be removed by changing the payload or the decoder,
and both are open decisions. On the same bodies, dropping process lists would
cut decode CPU by 56%. Dropping per-CPU flags as well would cut it by 79%.
Building with `GOEXPERIMENT=jsonv2` would cut it by 43% with no payload change.
`make bench-fleet` measures the cycle.

`fleet.local_name` sets how a node names itself in the view; it defaults to the
hostname. It is held to the same name rule as a peer name (below), for the same
reason: it is the `instance` label this node's history is written under, and the
name an aggregator's peer entry for this node must carry. **An unusable
`fleet.local_name` is a startup error** naming the value and the rule, and a
reload carrying one is refused with the running configuration kept. **When it is
unset and the hostname itself breaks the rule, that is a startup error too**,
naming the hostname and telling you to set `fleet.local_name` — the fallback is
not silently repaired, because a repaired name would be one no peer entry
matches. The configured value is checked while parsing; the hostname fallback is
checked with the other machine-dependent rules below.

**A peer's name is its fleet-wide identity** (ruling on #37). It is the name
that node calls itself — its own `fleet.local_name`, or its hostname — and so it
is also the Prometheus `instance` label that peer's history is written and
queried under. Two names for one node agree right up until someone edits one of
them, and the disagreement shows up as an empty chart indistinguishable from a
quiet period, so there is no separate alias: **name each peer exactly what it
calls itself.**

- **A peer with no `name:` is named after its URL's host.**
  `url: http://db-01:8081` is the node `db-01` — the port is not part of the
  name. Before #37 it took the whole URL, which no `instance` label can be, so
  its history was a 400 on an otherwise valid peer.
- **The name is checked at load, and a name that cannot be an `instance` label
  is a startup error** (owner ruling, 2026-09-22). It must start with a letter
  or digit and contain only letters, digits, `.`, `_`, `:` and `-`, at most 253
  characters (`pkg/nodename`). The error names the peer and the rule; on a
  reload the file is refused and the running configuration kept. An unnamed
  peer whose host cannot stand in for a name — an IPv6 literal such as
  `http://[fe80::1]:8081` — fails the same way, and the fix is to give it a
  `name:`.
- The history query escapes the label value as well, underneath the check
  rather than instead of it.

What the load cannot check is **correspondence**: that the name configured here
is the one the peer actually writes under. That value lives on the peer. A peer
aliased `database` pointing at a node that calls itself `db-01` still loads; it
is the operator's rule above that prevents it, not a check.

**Upgrading:** a file that loaded before #37 can now fail at startup. Look for a
peer named with a URL or with spaces or other punctuation, two unnamed peers on
one host (they are now the same node, and fail the uniqueness check), unnamed
IPv6 peers, a `fleet.local_name` outside the rule, and — with `fleet.local_name`
unset — a hostname outside it. An unnamed peer's card, `/api/fleet/{name}/...` path and
history identity change from its URL to its host.

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

**This node's own name must be usable, and a peer may not share it.** `fleet.local_name`, or the hostname
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
an input (`(*Config).ValidateLocalName`) and the serve command registers
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
`GET /api/fleet` copies peer URLs into a response that has no authentication of
its own. Rejecting rather than redacting means the secret is
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
URL's host, which is applied before the uniqueness check — so two unnamed peers
on different hosts are fine, and two on the same host, whether the same URL
twice or two ports, are not.

- `GET /api/fleet` — one summary per node: status, alert counts, CPU, memory,
  load per core, worst filesystem, RAID, SMART, temperature, power, throughput,
  uptime and **the build each node is running**.

  The build is three fields: `version`, `git_commit` and `build_date`, carried
  up from that node's snapshot. `version` alone never answered the question it
  was added for. It is the release string, it has read `0.1.0-alpha` on every
  build this project has produced, and a fleet running five different builds
  therefore reported one identical word ten times — five nodes sat two builds
  behind for days and the view showed them green. `git_commit` is the field
  that distinguishes builds; `version` is kept beside it unchanged for anything
  already reading it.

  `build_match` states the comparison rather than leaving it to the reader:
  `same`, `different` or `unknown`, against the commit of the node **serving
  the view**. The aggregator is the only party that knows its own build, so it
  makes the comparison once.

  **`build_match` is not a health signal, by design.** It does not touch
  `status`, `warning_count` or `critical_count`, and it must not: during a
  rollout every node disagrees with the aggregator until it restarts, and a
  fleet that went amber for the length of every deploy would teach operators to
  ignore the colour. A node on another build is a fact to display. The
  dashboard shows it as a neutral badge beside the health badges, not among
  them.

  **`unknown` never collapses into `same`.** A node whose binary predates these
  fields sends no commit; a local build sends the literal `unknown`; a node
  that has not answered sends nothing at all. None of them has agreed with
  anything, and reading their silence as agreement is exactly the defect — a
  stale node looking current — that these fields exist to end. `git_commit` and
  `build_date` are omitted from the payload when the node did not state them;
  `build_match` is always present.

  Adding these three is an **additive schema change**: nothing was removed or
  reinterpreted, and a client reading only the existing fields is unaffected.

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

### Peer transport: HTTPS and mTLS

The aggregator treats a peer's answer as authoritative — it is what the fleet
dashboard shows and what gets recorded into history. Over plain HTTP it does
not authenticate the node that answered, so anything able to intercept or
impersonate a peer can return a fabricated healthy snapshot. That is a
fail-open property of a monitoring signal.

**The URL scheme decides the transport. There is no negotiation and no
downgrade.** A peer with an `http://` URL is polled in the clear exactly as
before; a peer with an `https://` URL is polled over TLS with its certificate
verified.

```yaml
fleet:
  peers:
    - name: gpu-host
      url: https://198.51.100.91:8081     # verified TLS
    - name: fs
      url: http://198.51.100.10:8081      # still plaintext, still works
  tls:
    ca_file: /etc/go-sysmon/fleet-ca.pem   # verify peers against this authority
    cert_file: /etc/go-sysmon/node.pem     # this node's client certificate
    key_file: /etc/go-sysmon/node-key.pem  # ...and its key (mTLS, client half)
    insecure_skip_verify: false            # never true in a deployment
    require_tls: false                     # true forbids http peers entirely
```

| Setting | Default | Effect |
|---|---|---|
| `ca_file` | unset | Verify peer certificates against this PEM bundle instead of the system roots. An internal CA needs this; a publicly signed certificate does not. |
| `cert_file` / `key_file` | unset | The certificate this node presents when a peer asks for one. Both together or neither. |
| `insecure_skip_verify` | `false` | Accept any certificate a peer offers. Encrypted and unauthenticated — the property this feature exists to remove. Logged as a warning at startup. |
| `require_tls` | `false` | Refuse to poll `http://` peers at all. |

**Every default is the existing behaviour, and the zero value is too.** That
second part is load-bearing: a `config.Fleet` is not always built by the
defaults layer — a test, or anything embedding go-sysmon as a library, gets the
zero value. The setting is therefore phrased as opting *in* to requiring TLS
rather than opting *out* of plaintext, so an unconfigured fleet permits
plaintext however it was constructed. A field meaning "allow plaintext" would
have denied every peer in exactly those cases.

Setting `require_tls: true` is how an operator states that this fleet has
finished moving to TLS. After that an `http://` peer is a configuration error
reported against that peer by name, rather than a connection quietly made in
the clear. It is read per peer per cycle, so **it can be switched on by a
reload** — the enforcement is reloadable even though the material it enforces
with is not.

`ca_file`, `cert_file`, `key_file` and `insecure_skip_verify` are **boot-only**,
like `server.tls_cert` and `server.tls_key`. They are resolved once into the
poller's HTTP client, so a CA file that cannot be read stops the process at
startup naming the file, rather than surfacing three seconds later as every
peer being unreachable — which looks identical to a switch being down. A reload
that changes them is reported as deferred and the running values are kept.

#### When one side has TLS and the other does not

Nothing negotiates, so every mismatch is a clean failure attributed to the peer
that caused it, and the rest of the fleet is unaffected:

| Peer's configured URL | What that peer serves | Result |
|---|---|---|
| `http://` | HTTP | Works, unchanged. This is every current deployment. |
| `https://` | HTTPS, certificate signed by `ca_file` | Works, and the peer is authenticated. |
| `https://` | HTTP | Handshake fails. Peer marked unreachable; last good snapshot kept and badged stale. |
| `http://` | HTTPS | Request fails. Same unreachable path. |
| `https://` | HTTPS, certificate from an untrusted CA | Verification fails, peer unreachable. This is the attack being refused. |
| `https://`, peer requires a client certificate | this node has none configured | The **peer** refuses the handshake. Unreachable. |
| `http://` with `require_tls: true` | anything | Refused before connecting, with an error naming the peer and both ways out. |

A failing peer never takes the fleet view down. It is one unreachable card
among the rest — the same handling a peer that has simply stopped answering
already gets.

#### What this does not cover

The server half of mTLS — a node *requiring* a client certificate before it
will serve `/api/snapshot` — is not configurable here, and that is a scope
boundary rather than an oversight. The dashboard and the peer API share one
listener and one mux, so requiring client certificates for peers would require
them from browsers too; enforcing it per-endpoint instead would break the
dashboard, which fetches `/api/snapshot` itself. Deciding what happens to
browsers is dashboard authentication, a separate question from the peer
transport.

The consequence is worth stating plainly: configuring `cert_file` and `key_file`
lets this node prove who it is **to a peer that asks**, and nothing in go-sysmon
currently asks. It is the client half of mTLS — useful against a peer fronted by
a proxy that does require certificates, and inert otherwise. What is fully
solved here is the direction the risk actually runs: the aggregator verifying
the node that answered.

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

Everything above is a gauge. The swap paging totals are **counters**, declared
`# TYPE ... counter`:

```
# TYPE sysmon_swap_out_pages_total counter
sysmon_swap_out_pages_total 5310
```

`sysmon_swap_in_pages_total` and `sysmon_swap_out_pages_total` are pages
swapped in and out since boot (`/proc/vmstat` `pswpin`/`pswpout`). Query them
with `rate()`: that is how much swapping is happening now, which
`sysmon_swap_used_percent` cannot say. The type matters because `rate()` treats
a counter's drop as a reset — a reboot zeroes both — where over a gauge it
would read as a large decrease. Multiply by the host's page size for bytes.

Unlike the gauges, these are **omitted** when `/proc/vmstat` could not be read,
rather than exported as zero. A counter stuck at zero reads as a host that has
never paged, and one that drops to zero reads as a reset; an absent series is
a gap somebody can see. A host that has genuinely never paged reads the file
and exports `0`.

The exposition is written directly rather than through the client library: every
value is a plain gauge or counter already held in finished form, so the library
would add a large dependency tree for no gain on hosts this is deployed to.

Label values come from the host (device, interface and mount names, sensor and
accelerator names), so they are escaped exactly as the format specifies:
backslash, double quote and line feed become `\\`, `\"` and `\n`, and every
other byte, including non-ASCII UTF-8, is written verbatim. `HELP` text escapes
only backslash and line feed.

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
relabelled in the fleet view is relabelled in its history too — and the peer
entry naming it on an aggregator must be renamed with it (see *A peer's name is
its fleet-wide identity*). `external_labels`
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

### Searching the history

The owner asked, 2026-09-02, for **the ability to search across historical data
points**. The date range answered half of that; the search box beside it answers
the other half: naming what you want to see rather than finding the panel it
lives on.

Type a metric (`swap`, `disk read`) or a resource (`sda`, `eth0`, a core
number). Matches are listed best-first -- an exact resource, then a partial one,
then a metric name -- and choosing one charts it over the selected range,
above the tabs, beside the question that produced it. Up to six at once.

**What is searched is the catalog crossed with the resources this node actually
has.** `disk_util` is one catalogued metric and six answerable questions on a
machine with six drives, and `sda` is what an operator types. The resource list
comes from the **live snapshot**, not from a new endpoint and not from
Prometheus: the dashboard already knows every disk, interface, GPU and core, and
asking the time-series database what labels exist would be a second source for
the same list -- one that still offers a drive that was pulled yesterday.

Four states are distinguished, because three of them are not charts and each
means something different:

| state | what is shown |
|---|---|
| no range selected | "Pick a date range to chart these" |
| query matches nothing | "Nothing recorded matches that" |
| metric answered, this resource absent from the window | "Nothing recorded for this in the selected range" |
| the query failed | the reason the server gave |

**A node with no Prometheus has no search box at all** -- not a disabled one.
There is nothing to search, and a control that always answers "nothing" is worse
than no control.

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
