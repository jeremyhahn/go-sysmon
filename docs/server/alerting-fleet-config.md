# Configuration, alerts, fleet and metrics

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of configuration, alerting, fleet and metrics.

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
| Flag | `--addr`, `--interval`, `--tls-cert`, `--tls-key`, `--trusted-proxies` |

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
| **File watch** | `server.watch: true` | Watches the directories the config path resolves through (Linux inotify), so an editor that replaces the file, or a Kubernetes ConfigMap update, still triggers it. A file written in place reloads only once its writer closes it; see below. |

**Those are the only two paths — there is no HTTP reload endpoint.** The
service carries no authentication of its own and sits behind an authenticating
proxy, so **no HTTP path changes its configuration**. `systemctl reload`
already delivers everything such an endpoint would have — add a peer or retune
a threshold without restarting, Prometheus counters and connected browsers
preserved — so nothing is lost by not having one.

The single endpoint that changes anything at runtime is `POST /api/interval`
described above: it retunes a transient refresh rate, writes no file, and is
discarded by the next reload.

**Reloadable:** alert thresholds, fleet peers, fleet poll intervals,
`fleet.headroom_target`, and `server.interval_ms` — the collection interval is pushed into the running
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

Without this, choosing 500ms while viewing a peer changed the local monitor —
a cadence nobody was looking at — while the peer's figures kept arriving at
`fleet.poll_interval_ms`, and the control reported a rate nothing was collecting
at.

**A peer refresh is floored at one second** (`fleet.MinPollInterval`). Every
peer is fetched over HTTP on every cycle, on their CPU as well as this node's,
so a sub-second fleet cadence spends the aggregator's time asking ten machines
what changed in the last few hundred milliseconds. **The response reports what
is in force, not what was asked for**, and the header states the effective rate
beside the control — a dropdown reading 500ms over data collected once a second
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
500 ms over data collected once a second. It reports the **last** cycle rather
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

**Boot-only:** `server.addr`, `server.tls_cert`, `server.tls_key`,
`server.trusted_proxies` and `server.watch`. These are consumed once, when the
process starts and the socket binds. The peer-transport keys under
`fleet.tls` (all but `require_tls`) and every `bmc` key are boot-only for the
same reason: the poller's HTTP client and the drive-slot collector are built at
startup. A reload reports each changed one in `deferred` **and keeps the boot
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

#### What the file watcher reloads, and when

A half-saved file does not always fail to parse. A tool that rewrites the file
in place truncates it and then writes it, and a file cut off at a line boundary
is valid YAML: loading it would publish the lines already written and defaults
for the rest. An empty file is valid too, and it means every default, which
silently drops alert thresholds and fleet peers. So the watcher decides from the
kernel's events, not from whether the file parses:

| Event on the config file | Watcher does |
|---|---|
| Written or truncated (`IN_MODIFY`) | Holds: nothing is reloaded while a writer has the file open. |
| Writer closes it (`IN_CLOSE_WRITE`) | Reloads, once. |
| Renamed over or created (`IN_MOVED_TO`, `IN_CREATE`) | Reloads, once. A created file that is then written is held until closed, like any write. |
| Removed or renamed away | Reloads: with no file the service runs on defaults, as it would have started. |
| A symlink on the path is replaced or removed | Reloads the file the path now leads to, and moves the watches with it. |
| The config file's own directory is deleted, moved or unmounted | Logs a warning and stops watching. `systemctl reload` still works. |

**Symlinks are followed link by link.** When the config path is, or passes
through, a symlink, the watcher resolves it one component at a time and watches
the directory holding each link as well as the directory holding the file it
ends at. That covers:

- **A Kubernetes ConfigMap volume.** `sysmon.yaml -> ..data/sysmon.yaml`, and
  the kubelet updates it by renaming a new `..data` link over the old one,
  pointing at a new timestamped directory it has already written in full. The
  swap reloads the new file; removing the old directory afterwards changes
  nothing. See [the container guide](container.md#configuration-from-a-configmap).
- **A link to a file kept elsewhere**, such as `/etc/go-sysmon/sysmon.yaml ->
  /srv/config/sysmon.yaml`. Edits to the target are followed in its own
  directory, under the same close-write rule as any other file.
- **A switched release directory**, such as `/opt/sysmon/current ->
  releases/v2`, swapped by renaming a new link over `current`.

If a directory the path resolves through disappears without being replaced, the
file is gone and the service runs on defaults until it comes back, as with a
removed file. If the config path's own directory disappears, watching stops, as
in the table.

Events arriving within 250ms of each other are coalesced into one reload.
Changes to other files in the directory, including an editor's swap file, are
ignored.

**An empty file is refused on a watcher reload** (whitespace only counts as
empty): the watcher logs `config: reload failed, keeping previous settings`
carrying a `ConfigEmptyError`, and keeps the running configuration. That covers
a tool that truncates, closes and writes later, and the reload happens when the
contents arrive. **Starting on an empty file, or `systemctl reload` of one, still
runs on defaults**: there the operator asked for the file as it stands, while a
file the watcher sees empty is far more likely a rewrite caught in the middle.

**A writer that keeps the file open is held, and warned about.** The kernel
reports a write as finished only when the writer closes the file, so a file
still open for writing is never reloaded, however complete it looks. If it goes
30 seconds with no change while still open, the watcher logs `config: file is
still open for writing and has not changed; reload waits until it is closed` at
warning level, with the path. `systemctl reload` applies the file at once, and
closing it reloads it as usual.

**A close is taken at its word.** A writer that closes the file partway through
and reopens it more than 250ms later has its partial file reloaded, then the
finished one when it closes again: when the close arrives, nothing yet shows
that another write will follow. The reopen cannot be used to tell either. The
kernel reports a reader's open exactly as it reports a writer's, so treating an
open as the end of a write would let anyone reading the file mid-write (`cat`,
a backup, the service's own reload) publish it half written. Write the file in
one pass, or write a temp file beside it and rename it over (what editors and
config-management tools do), and every reload sees a whole file.

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
| `server.trusted_proxies` | The addresses of the proxies in front of the node. |
| `prometheus.remote_write_url`, `prometheus.query_url` | The address of the fleet's time-series database. |
| `config_file` | An absolute path is a map of the host's filesystem layout. |
| `fleet.peers.<name>` | Another machine's scheme, host and port. |
| `bmc.redfish_url`, `bmc.username`, `bmc.password_file`, `bmc.ca_file` | The BMC's address, its account, and where its password and CA are kept. |

Peers keep their **names**. The name is how the dashboard, the fleet proxy and
the history queries all address a node, and the browser reaches a peer through
`/api/fleet/{name}/...` on this node rather than by dialling it — so the URL
buys a client nothing and hands an anonymous one the internal topology. Together
with the config file's path it let a single reachable listener enumerate
hostnames, ports, transport choices and filesystem layout (#16).

**Every peer is keyed by the name the operator gave it.** A peer with no
`name:` is a startup error (see *A peer's name is its fleet-wide identity*
below), so no key can be a URL or a host the loader filled in. No name can
carry a scheme or path, and the name is the node's identity — the string
`GET /api/fleet` titles its card with and routes `/api/fleet/{name}/...` by —
so withholding it here would hide nothing.

**A peer's settings, relayed through `GET /api/fleet/{name}/config`, withhold
its bind address as well** (#120). Served by the node itself, `server.addr` is
the address the caller already connected to and is shown as it is. Relayed by
an aggregator it is another machine's listening port, handed to anyone who can
name that machine, so it becomes `(configured)`. The aggregator applies the
same withheld list to what it relays rather than trusting the peer to have
applied it: a build older than #16 sends its paths and addresses verbatim, and
one older than #37 keys an unnamed peer by its whole URL, which is re-keyed
`fleet.peers.(unnamed N)`. The list lives in one place (`withheld` in
`pkg/config/keys.go`), so a setting cannot be withheld on one path and served
on the other.

`GET /api/fleet` carries no peer URL either (#120). A card is titled by the
host name the node reports; a peer that has never answered shows its `name`.
A poll error is worded without the address — see
[`GET /api/fleet`](api.md#get-apifleet).

### BMC

The `bmc` section configures the [drive-slot collector](../collector/drive-slots.md).
With the section absent, only the in-band IPMI fallback is tried.

| Key | Default | Meaning |
|---|---|---|
| `bmc.redfish_url` | — | The BMC's `https` Redfish endpoint. It must not contain credentials. |
| `bmc.username` | — | The Redfish account, required with `redfish_url`. |
| `bmc.password_file` | — | The password file, required with `redfish_url`. It is read on every poll and refused if other users can read it. |
| `bmc.ca_file` | — | A PEM bundle that pins the BMC's CA. |
| `bmc.insecure_skip_verify` | `false` | Turns certificate verification off. Explicit only. |
| `bmc.timeout_ms` | `10000` | The timeout for one Redfish request. |

All six keys are boot-only.

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
| storage | `drive_slot_fault` ([drive slots](../collector/drive-slots.md)) | — (always on) | — | critical (fault) / warning (predicted, degraded) |
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
| host | `history_cadence_unchartable` | — (the database's query lookback) | — | warning |

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

`cpu_high` compares the mean over the cores whose usage was measured, and says
how many threads that was. With no core measured — the monitor's first
collection, or an unreadable `/proc/stat` — the rule does not evaluate: an
unknown is neither over the threshold nor under it.

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
the node it loaded from; that node polls the peers and caches their summaries.

Put the aggregator on a host that does nothing else. A fleet dashboard is what
you open when something is wrong, so it must not be served by a machine that
could itself be the thing that is wrong: an overloaded or swapping workload host
would take the view of the whole fleet down with it, exactly when it is needed.

**The aggregator polls summaries, not snapshots** (owner ruling on #76,
2026-09-22). Every node serves `GET /api/summary`: its own figures for the fleet
view, about a kilobyte, computed from its two latest collections. The
aggregator fetches that from each peer every cycle. It used to fetch each peer's
whole `/api/snapshot`, about 2.9 MB a cycle across the fleet in September 2026,
and decoding those snapshots was over 80% of the cycle's CPU. Most of the bytes
were process lists (about 60%) and per-CPU `flags` arrays (about 25%), and the
fleet view reads neither.

**History still gets full readings, from workers of its own.** While the
aggregator records history (`prometheus.remote_write_url` is set), each peer
has a history worker that fetches `GET /api/snapshot/history` once per
`prometheus.sample_interval_ms` (floored at a second). That is the snapshot
without its process list and per-CPU flags, which `metrics.Write` never reads.
The workers run apart from the summary poll. A history endpoint that stalls or
fails costs that peer's reading and never delays a summary or a card: the
poll does not wait for them, and the test for it (`pkg/fleet/recorder_test.go`)
stalls a history handler indefinitely under a minute-long timeout while the
poll keeps going. At most four history payloads are fetched at once, over the
poller's one HTTP client. A worker is cancelled, and its in-flight request with
it, when its peer is removed or repointed, when recording is turned off by
reload, and when the poller stops. The workers are restarted when the sample
interval changes, so none waits out a timer set for the old one. They are
reconciled with the configuration before and after every poll, and a worker
re-checks its peer's URL before fetching and before recording. A reading that
arrives after a reload has moved the name to another address is dropped.
A history path answered with 404, or with a non-JSON page from a proxy, falls
back to the whole snapshot, so the history stays complete. Fetching at the sample
interval cannot double-write, because the exporter writes a peer's reading
only when the peer has collected since the last one it wrote (#173). A peer
too old to serve a summary gets no history fetch: its poll already fetches the
whole snapshot, and that snapshot is what gets recorded. A test fills every snapshot field by reflection and runs
`metrics.Write` on the whole snapshot and on the history payload. Both runs must
emit the same samples. So if `metrics.Write` starts reading a removed field, the
test fails and names the samples that would be lost (`pkg/fleet/history_test.go`).

**Drilling into a node fetches its whole snapshot on demand**, through
`GET /api/fleet/{name}/snapshot`. Only someone looking at that node pays for it.
Concurrent drill-ins of one peer share a fetch. An attempt made within the
fleet's poll interval is answered again, whether it succeeded or failed, so a dead peer does
not build a queue of requests that each wait out its timeout. A peer whose URL
changes on reload loses its cached snapshot.

**Older peers keep working, and they are marked.** A peer that answers 404 on
`/api/summary` predates it. So does one that answers it with a body that is
not JSON: an older build embedded as a library without dashboard assets
answers unknown paths from its plain-text status page. The aggregator fetches its whole snapshot, as it
always did, summarises it itself and records it as history, all in one fetch.
The node's `source` is then `snapshot` instead of `summary`, and its card shows
a neutral *polled by full snapshot* badge. A fleet in the middle of a rollout
works throughout, and you can see which peers still cost the aggregator a whole
snapshot. For those peers the aggregator computes the swap paging rate across
two of its own polls. If a poll returns the same snapshot as the last one, the
last measured rate is kept. A counter that goes backwards, as it does when the peer
reboots, makes that interval unknown.

Measured with `make bench-fleet`: ten peers, each serving a busy-host snapshot
shaped like ci-node-1 (112 threads with their flags, 1410 processes, 567 KB).
The CPU figure is an upper bound because the peers run in the benchmark's own
process.

| Poll shape                                 | Peers send per cycle | CPU per cycle | Allocated per cycle |
| ------------------------------------------ | -------------------: | ------------: | ------------------: |
| whole snapshots (the shape before #76)     |              5670 KB |        ~42 ms |             14.2 MB |
| summaries only                             |                10 KB |       ~1.5 ms |              0.1 MB |
| summaries + one history reading per peer   |              1011 KB |        ~11 ms |              2.2 MB |

Measured with Go 1.27. Before that toolchain the whole-snapshot row was about
68 ms, and the other rows fell in about the same proportion.

An aggregator that does not record history does about 1/30 of the work. The
last row is a poll and a history reading landing together, which happens once
per sample interval (15 s by default) rather than every poll. Every series
written before is still written.

Each peer endpoint keeps its read buffer between cycles (`pkg/fleet/body.go`).
Peers answer chunked with no Content-Length, so a fresh reader grows by
doubling on every poll.

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

- **Every peer must have a `name:`; an unnamed peer is a startup error**
  (owner ruling 2026-09-23, under #120). The name is published on the
  unauthenticated fleet endpoints as the peer's card title, route and history
  label. The earlier defaults — the whole URL before #37, then the URL's host —
  put the peer's address on the wire as its routing key, which #120 removed.
  The error names the peer by its position, `fleet peer 3 in fleet.peers has no
  name`, and quotes nothing from the entry. On a reload the file is refused and
  the running configuration kept.
- **The name is checked at load, and a name that cannot be an `instance` label
  is a startup error** (owner ruling, 2026-09-22). It must start with a letter
  or digit and contain only letters, digits, `.`, `_`, `:` and `-`, at most 253
  characters (`pkg/nodename`). The error names the peer and the rule; on a
  reload the file is refused and the running configuration kept.
- The history query escapes the label value as well, underneath the check
  rather than instead of it.

What the load cannot check is **correspondence**: that the name configured here
is the one the peer actually writes under. That value lives on the peer, so a
peer aliased `database` pointing at a node that calls itself `db-01` still
loads. It is checked at **poll** time instead (#162): every node states the name
it writes its history under as `node_name` in its snapshot — the same value its
exporter uses for the `instance` label — and the aggregator compares it with the
name it holds, reporting `name_match` in `GET /api/fleet` (below). The Fleet
card shows a mismatch as a warning naming both names.

**Upgrading:** a file that loaded before #37 or #120 can now fail at startup.
Look for **any peer without a `name:`** — give it the name that node calls
itself (its `fleet.local_name`, else its hostname), which for a peer that
loaded after #37 is its URL's host and keeps its card, routes and history
unchanged — a peer named with a URL or with spaces or other punctuation, a
`fleet.local_name` outside the rule, and — with `fleet.local_name` unset — a
hostname outside it. A running service refuses a reload that adds an unnamed
peer and keeps its current fleet, so fix the file before restarting.

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
a peer's URL is written to this node's log when a poll of it fails. Rejecting
rather than redacting means the secret is
never stored, never rendered and never logged; the error names the peer and its
host but never the URL. A URL that cannot be parsed at all is also a load
error, rather than a connection failure repeated on every poll.

**Peer names must be unique.** A peer's name is its identity throughout the
fleet subsystem — it keys the poller's result cache, titles the card in the
view, and selects the node for `GET /api/fleet/{name}/...`. Two peers sharing a
name would render as one card showing whichever host answered last alongside
the other's configuration, so a duplicate is rejected at load rather than
silently de-duplicated: dropping one would leave a real host unmonitored while
the dashboard looked complete. Two ports on one host are two processes and
need two names.

- `GET /api/fleet` — one summary per node and the **fleet roll-up** over them.

  **Paged server-side** (#15). `?q=&offset=&limit=` select a page: the nodes
  whose name or hostname contains `q` (A–Z case-folded), in name order, from
  `offset`, at most `limit`. `page` carries `{q, offset, limit, matched,
  total}`, and `self` the serving node. The roll-up is computed over every
  node whatever page is asked for; `GET /api/fleet/rollup` serves it alone.
  The parameters, their bounds and their errors are in
  [api.md](api.md#get-apifleet).

  Each node's figures are its `NodeSummary`: status, alert counts and the
  firing warning and critical alerts, CPU, memory, load per core, swap
  occupancy **and paging rate**, worst filesystem, RAID, SMART, temperature,
  power, throughput, uptime, **absolute capacity** (cores, load, memory, free
  disk under the container runtime, containers; see
  [Capacity and headroom](#capacity-and-headroom)) and **the build each node
  is running**. `source`
  says how the aggregator read the node: `local`, `summary`, or `snapshot`
  for a peer too old to serve a summary.

  **Swap is reported as paging, not only occupancy** (#134). `swap_percent`
  cannot tell a node thrashing now from one that swapped out idle pages last
  week. `swap_in_rate` and `swap_out_rate` are bytes per second over the node's
  own last collection interval, taken from two consecutive readings.
  `swap_rate_available` is false whenever that could not be measured: the
  counters were unreadable (`swap_counters_available` false), there is no
  previous reading yet, the counters went backwards, or the host's boot
  time changed between the two readings (a reboot). A false there means
  unknown, never zero paging.

  `collection_interval_ms` is the interval the node's monitor actually
  collects at, reported by the node itself. It lets an aggregator judge
  whether a peer collects often enough to chart. `0` is unknown: a peer too
  old to report it, or one read by its whole snapshot. It is never assumed.

  `power_reported` says the node has a power sensor. Without a sensor,
  `power_w` is 0 and means nothing. `power_w` is CPU packages plus GPUs and
  NPUs. A sensor reading zero, like a CPU package before its second energy
  sample, is not yet a reading.

  `net_rx_rate` and `net_tx_rate` are summed over the node's physical
  network devices, where its traffic meets the wire. Summing every interface
  would count bonded, bridged, tunnelled and guest traffic more than once.
  `net_reported` is false for a node with no physical device (a container in
  its own network namespace): sysfs identifies no non-overlapping set there,
  so it reports no throughput rather than an inflated one. It is also false
  while any device's rate is unmeasured (`rates_available`: first
  collection, new interface, reset counters), and for a peer too old to send
  that field. For the older peer the field's absence is unknown, not
  agreement, and the network total's denominator shows the gap for as long
  as the rollout lasts.

  `max_temp_c` is the hottest reading from every component that reports one:
  CPU cores, drives (controller members included), GPUs (core and memory)
  and NPUs. `max_temp_source` names the kind: `cpu`, `drive`, `gpu` or `npu`.
  A sensor that reports nothing contributes nothing.

  **The roll-up** (`rollup`, #46) follows the rule ratified on that issue:
  *show the worst and name the node, or show a sum with its denominator —
  never a mean.*

  - `nodes`, `healthy`, `warning`, `critical`, `unreachable` count every node,
    each in exactly one state. Unreachable is its own term. It is never added
    to the healthy count or hidden in a fraction.
  - `longest_absence_seconds` is the age of the oldest reading among the
    unreachable nodes. It tells a node restarting from one that has been gone
    since Tuesday. `never_answered` counts unreachable nodes that have never
    sent a reading.
  - `attention` names the worst five nodes that are not healthy: critical
    first, then unreachable, then warning, by name within each. Each entry
    gives the node's alert messages, or the poll error for an unreachable
    node. On a page, and from `/api/fleet/rollup`, the list is bounded on the
    server (#15): the roll-up is sent on every poll and must not grow with
    the fleet. A request with no parameters names every node. `critical + unreachable +
    warning` is how many there are in all, which the dashboard shows as
    `and N more`.
  - `cpu`, `memory`, `disk`, `load_per_core`, `temperature` and `swap_out`
    are `{node, value}`: the worst value and the node that holds it.
    `temperature` also carries `kind`, the component it came from. A zero
    that means "nothing to read" (no filesystem, no thermometer) is never the
    worst. `swap_unknown` counts nodes whose paging could not be measured,
    and `cpu_unknown` nodes whose CPU usage could not be (`cpu_available`
    false in their summary: a first collection, or an unreadable
    `/proc/stat`); neither kind is a candidate for the worst.
  - `power` is `{value, nodes, of}`: watts summed over the `nodes` that have
    a sensor, of the `of` reachable nodes. It is omitted when no node has a
    sensor. `network` is `{rx_rate, tx_rate, nodes, of}`: throughput summed
    over the nodes that measure it (`net_reported`), of the reachable ones.
  - `capacity` is the fleet's absolute capacity (#239), each figure a total
    `{value, nodes, of}` over the reachable nodes that measured it:
    `cpu_count`, `memory_total_bytes`, `memory_available_bytes`,
    `disk_free_bytes`, `containers_running`, and `headroom` with the summed
    per-node headroom. Each is omitted when no node measured it, and
    `capacity` itself when none measured anything. See
    [Capacity and headroom](#capacity-and-headroom).
  - `smart` and `raid` are `{unhealthy, devices, nodes, of}`. They count
    **devices**, not nodes, and the drives behind a hardware RAID controller
    count too (#45/#51). Each is omitted when no node has any, because
    "0 of 0 unhealthy" would read as a clean bill of health for hardware that
    does not exist.
  - `builds` groups the reachable nodes by commit, the largest group first.
    It is identity, not health. Each group is `{commit, count, nodes}`:
    `count` is its size and `nodes` names its first five by name (all of
    them with no parameters). At most five groups are sent otherwise;
    `build_groups` is how many there are.
  - **Only reachable nodes contribute a figure.** `reporting` says how many
    there are. An unreachable node's last known values would record an
    observation nobody made.

  The build is three fields: `version`, `git_commit` and `build_date`, carried
  up from that node's snapshot. `version` alone never answered the question it
  was added for. It is the release string, which changes only when a release
  is cut, so every build between two releases carries the same one: a fleet
  running five different builds reported one identical word ten times — five nodes sat two builds
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

  `name_match` does the same for **names** (#162): `same`, `different` or
  `unknown`, comparing `name` — the name this aggregator holds for the node
  and queries its history by — with `node_name`, the name the node says it
  writes its history under (its `fleet.local_name`, or its hostname). A peer
  states it in its `GET /api/summary`; one too old to serve a summary is
  summarised from its whole snapshot, which carries the same `node_name`, so
  the comparison is made either way. `different` means the peer is configured
  under a name it does not answer to, so one node has two history identities:
  in topology A the aggregator writes the peer under the alias while the node,
  and any dashboard reading by the node's own name, uses the other, and a
  chart queried by either misses what was written under the other. The
  dashboard shows it as a warning on the card naming both. It is
  a configuration fact, not a health signal, and does not touch `status` or the
  counts.

  `unknown` again never collapses into `same`: it is reported for a node whose
  build predates `node_name` and sends none, and for a node that did not answer
  its latest poll — whether the alias matches is a question about the node as
  it is now, so a stale reading does not answer it. The dashboard shows a
  reachable `unknown` quietly, as "name unverified". `node_name` is omitted
  when the node did not state a name, and may still carry the last one heard
  from a node that has since stopped answering; `name_match` is always present.
  The aggregator's own card is compared with the name it resolves at the time
  of the request, not the one in its latest snapshot: the two are the same
  function, and the snapshot is restamped just after a reload of
  `fleet.local_name` goes live, so a view built in that instant would
  otherwise flag this node against itself.
  Like the build fields, this is an additive change, and it reports `unknown`
  for every peer until the peers themselves run a build that sends the field.

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
- `GET /api/fleet/{name}/snapshot` — that node's full snapshot, fetched from
  the node on demand, wrapped with the freshness of that fetch:
  `{snapshot, reachable, error, age_seconds}`.

  The wrapper is the point. When a fetch fails, the last snapshot fetched is
  still served, so a blip does not blank the page. A bare snapshot would look
  the same whether it was collected a second ago or before the host
  disappeared. `reachable` describes the **latest** fetch and can be false
  while `snapshot` is still present. That is the stale case. A client must
  mark it rather than fold it into a live series: re-applying the same
  reading every cycle draws a flat line that reads as "measured and steady"
  when nothing was measured. The dashboard shows a `stale · Nm old` badge
  beside the node name and stops recording history until the peer answers
  again.

  `404` means the name is not in the fleet, or this node has not collected
  yet. `502` means the peer could not be read and has never been read, or did
  not answer within the response's write budget. `503` means a reload moved
  the name to another address while its snapshot was in flight; that answer
  belonged to the old address and is discarded, and asking again reaches the
  new one.

Peers are polled concurrently, each with its own timeout, so one dead node
cannot delay the refresh. A peer that stops answering keeps its last good
summary alongside the error, so a brief blip reads as stale data rather than an
empty card, and an unreachable node is never rendered as a healthy one.

### Capacity and headroom

`/api/fleet` carries enough for a scheduler to place work from one query
(#239): a coordinator keeping each worker at about 80% CPU without swapping
needs absolute capacity, not only percentages.

Each node's summary carries `cpu_count`, `load_1`, `load_5`, `load_15`,
`memory_total_bytes`, `memory_available_bytes`, `disk_free_bytes`,
`disk_total_bytes`, `disk_free_path`, `containers_observable`,
`containers_running`, `container_cpu_cores` and `container_memory_bytes`. The
fields and their unknown forms are listed under
[`GET /api/summary`](api.md#get-apisummary). In short: a zero `cpu_count` or
`memory_total_bytes`, an empty `disk_free_path` and a false
`containers_observable` are unknown, and a peer on an older build sends none of
these, so all of its figures read as unknown rather than as an empty machine.
A peer too old to serve a summary is summarised here from its snapshot and gets
the fields that snapshot can supply.

The free disk is the container runtime's data root, measured by the node itself
with `statfs`. It works when the runtime's API is unreachable and when the root
is a bind mount the disk list cannot name; see
[runtime storage](../collector/virtualization.md#free-space-on-the-data-root).
The container figures come from the cgroup tree, so they separate CI-runner load
from the rest of the node's without the docker group.

**Headroom** is how much more work a node can take before it reaches the
aggregator's utilisation target:

```yaml
fleet:
  headroom_target: 0.8   # (0, 1]; default 0.8
```

- `cpu_cores_free_at_target` = max(0, `cpu_count` × target −
  `cpu_count` × `cpu_percent` / 100). It is computed only when `cpu_count` is
  known and `cpu_available` is true; `cpu_reported` says whether it was.
- `memory_bytes_free` = max(0, `memory_available_bytes` − (1 − target) ×
  `memory_total_bytes`). It is computed only when `memory_total_bytes` is
  known; `memory_reported` says whether it was. Available memory is what can be
  had without swapping, so a node with memory headroom can take the work
  without paging.

Both are clamped at zero. A node past its target has no room, not negative room
that would cancel another node's room in a fleet sum.

Headroom is a field of the fleet entry, not of the summary, because the target
is the aggregator's configuration and a node cannot know which aggregator is
asking. Every node in the view gets it, `self` and child-subtree nodes included.
A subtree node is restated at this aggregator's target, whatever its child
computed. It is absent for an unreachable node, because a stale reading is not
capacity to place work on. It is also absent when neither half could be
computed. When only one half could be, the object is present and that half's
`*_reported` flag is false. That keeps the half that is known, and the object's
absence still means there is nothing to read.

A target outside (0, 1] is a load error naming the key
(`HeadroomTargetError`). It is not repaired to the default, because the file
would then state a target the view does not use. An unset key is the default.
The key is reloadable and takes effect on the next view.

The roll-up's `capacity` sums each figure over the reachable nodes that
measured it, with `nodes` of `of` coverage, and never averages them: a fleet's
mean free memory says nothing about whether any one node can take a job.
`capacity.headroom` sums the per-node headroom at its `target`, which is always
the serving aggregator's own `fleet.headroom_target`. In a tree whose
aggregators are configured with different targets, a child's headroom sums at
another target are left out rather than added, and the coverage counts show the
gap. That holds even before the aggregator has headroom of its own (just after
startup): it never adopts a child's target.

### Hierarchical aggregation

One aggregator polling every node has a limit (#190). Polling costs about
0.08 ms of CPU per peer per cycle, and recording history costs about 103 KB
per peer per sample, which is about 1 GB per sample at ten thousand peers. So
aggregators form a tree: **a peer can itself be an aggregator**, and each
aggregator polls and records only its own direct peers (topology A at every
level). Nothing is configured to make this happen. A node whose summary
reports `aggregates` greater than 0 is a child aggregator, and its parent also
polls its `GET /api/fleet/subtree` each cycle.

- **Names are unique across the whole tree** (#37): a node's name is its
  Prometheus `instance` label, so it is never qualified by path. If two nodes
  carry the same name, both are listed with `name_conflict`, neither can be
  drilled into, configured or charted (`409`), and the roll-up counts them in
  `name_conflicts`. This applies to the aggregator's own name too: a node
  elsewhere in the tree called the same is a conflict, not a loop.
- **Loops are found by identity, not by name.** Each aggregator process has a
  random 128-bit instance ID, created at start and never configured.
  `/api/summary` carries it as `instance_id`, and every request one aggregator
  makes of another carries `X-Sysmon-Fleet-Ancestry`: the IDs of the
  aggregators the request has passed through. A poll carries the chain the
  aggregator learned from its own parent's latest poll; a relayed drill-in,
  settings request, history range or page fill carries the chain of the
  request it relays, and a poll carries the poller's own ID. An aggregator
  refuses with `508` (`X-Sysmon-Fleet-Refusal: loop`) a request carrying its
  own ID, and (`depth`) one that has passed through more than `MaxTreeDepth`
  (4) aggregators, so no relay can go round a loop.
- **Loops among polls are found from the subtrees.** Every subtree lists, in
  `aggregators`, the instance IDs of every aggregator at or below it,
  including children it refused. A parent that finds its own ID there refuses
  the subtree. Every edge of a loop is cut, and because refused children stay
  in the list, the evidence survives the cut: the same edges are cut on every
  cycle until the loop is removed. A child's IDs carried past failed reads
  expire after three poll intervals. A child listing more than 1024 is
  refused outright, and none of its IDs are passed further up.
- **What a child's report can do (by design).** A child's subtree is composed
  or refused whole. A child whose list contains this aggregator's ID, whose
  tree is too deep, whose data does not validate, or that refuses this
  aggregator's poll takes its entire subtree out of this view. Nothing below
  it is shown or counted, only the child itself with the reason. So one
  misconfigured or misbehaving aggregator can hide everything beneath it from
  every ancestor. That is deliberate: a subtree that cannot be trusted is not
  composed in part, and the cause is stated on the child's card and counted
  in `subtree_errors`. It cannot reach sideways: a sibling subtree, or this
  aggregator's own peers, are unaffected.
- A parent whose peer reports the parent's own instance ID, or whose child's
  subtree is deeper than 4 levels, does not compose that subtree. It says why
  in the child's `subtree_error`, counts it in `subtree_errors`, and keeps it
  refused even if a later read fails.
- **The view is served from polled state.** Search, order, counts, offsets and
  routing come from each child's index: name, hostname, status, age, reasons
  and build for each node. Only the summaries of the nodes on a page are
  fetched from their children on request (`GET /api/fleet/nodes`), and they
  are reused for the poll interval (owner ruling 2026-09-23). Page fetches are
  bounded:
  - a cached summary is keyed by the child and the child's address;
  - concurrent requests for one batch share a single fetch;
  - at most 8 fetches are in flight;
  - at most 250 subtree summaries are fetched per view.

  A request with no parameters fetches nothing from the children. Its subtree
  nodes, like any beyond the per-view bound or any a child could not give in
  time, carry `source: "index"`: their status and age are known, and their
  figures are absent, not zero. A card's status, reachability and age are
  always the index's, which is what the roll-up is counted from.
- **Deadlines are passed down the tree.** A request carries
  `X-Sysmon-Fleet-Budget-Ms`: the time its sender will still wait, less a
  margin for each hop. A slow grandchild therefore fails only its own nodes,
  and a node whose peer has become slow is answered with its last reading,
  marked stale, rather than timing out at the root. The header is honoured
  only on a request carrying a valid ancestry chain, i.e. from another
  aggregator; from a browser it is ignored.
- **Work done for requests is shared and bounded.** Page fills, relayed
  drill-ins, settings and history ranges run as shared work:
  - identical work runs once, and every asker waits for it;
  - the work runs detached from whoever started it, so one asker leaving, or
    arriving with a short budget, cannot fail the others. It runs to the
    latest deadline any asker has, never past `fleet.timeout_ms`;
  - an identical relay is reused for one second;
  - at most 8 run at once. Views, parents' page fills and relays each have
    their own bound, so none can starve another.
- **The roll-up composes exactly.** Counts, sums and denominators add, and
  extremes keep the worse value with its node named. A node whose CPU usage
  was not measured is counted in `cpu_unknown` at every level, never read as
  0%. On a tie the earlier name
  wins, as a single pass over the whole fleet would pick it. The top five
  attention entries (each carrying `via`) and every build group merge without
  loss. An unbounded roll-up is the same composition with its lists taken
  whole from the index. A test checks that composed roll-ups equal the
  one-pass roll-up. Each view is built from one published refresh, so a child
  whose poll has just failed is never shown beside its last subtree merged as
  healthy.
- **A child that stops answering takes its subtree with it.** Every node of its
  last known subtree is unreachable, never healthy, with its age carried
  forward. The child's attention entry carries `subtree`, the number of nodes
  now unknown, and its card carries `subtree_error`.
- **Drill-in, settings and history route to the owner.** Requests for a subtree
  node go to the child that owns it, which may pass them to its own child in
  turn. History is relayed whether or not the parent keeps any history of its
  own.
- **Nothing a child sends is passed through.** Every relayed answer is decoded
  as the type asked for and served as JSON this node writes. A history answer
  must be `application/json` and the range of the node asked about; anything
  else is refused with `502`, as is an answer larger than 8 MiB. A child's
  error comes back as its status, if that status is one of 400, 404 or 409
  (otherwise `502`), and never with the child's body or headers.
- **A child's data is validated.** A subtree must meet all of these:
  - exactly one node at depth 0 (the child itself), and every depth between
    0 and 4;
  - usable names, and statuses that are node states;
  - bounded host names, reasons and builds;
  - roll-up counts that are counts, and sums that are finite and
    non-negative;
  - states that partition its nodes, as its index lists them;
  - denominators equal to its reachable nodes;
  - build groups that count no more than those nodes.

  Its lists are clamped to their bounds, and the subtree response to 32 MiB.
  An invalid or oversized subtree is a read failure: the last good one stays,
  marked stale.
- **Mixed versions.** A child running a build from before #190 does not report
  `aggregates`, so it appears as the single node it says it is.
- **Security.** Every endpoint is a `GET`. No level publishes an address, and
  relayed settings are redacted again at each hop (#120).

**A flat fleet pays nothing for any of this.** A fleet with no child
aggregators is served by a path of its own. Each refresh builds the peers'
nodes and roll-up once, and a request adds only this node's live reading (a
merge, exact whatever the split), ages the readings by the time since the
refresh, and copies out the page. A test holds this path to the tree path's
answer. A 25-node page of a 10,000-node flat fleet costs about 0.08 ms of CPU,
against 2.3 ms in #15.

Measured on a tree of 10,000 nodes (a root with 100 child aggregators of 100
nodes each, `BenchmarkTree_*`): a root cycle takes about 33 ms of CPU and
1.35 MB of polling. The root records 10 MB of history per sample (100 peers),
and so does each child. A 25-node page from the root takes about 1.0 ms of CPU
from the cache, or about 1.6 ms with one fetch from a child. One flat
aggregator of the same fleet takes about 0.77 s of CPU per cycle and records
about 1 GB per sample.

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

Series include `sysmon_cpu_usage_percent` (the mean over the cores whose usage
was measured; omitted, never zero, when none was), `sysmon_memory_used_percent`,
`sysmon_load1`, `sysmon_psi_{some,full}_avg10{resource}`,
`sysmon_power_watts{source}`, `sysmon_disk_{util_percent,queue_length,
used_percent,temperature_celsius,smart_healthy,wear_percent}{device}` (the
drive health series also carry `serial`, see below),
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
`sysmon_swap_used_percent` cannot say. The dashboard's history charts do
exactly that — the catalog entries `swap_in` and `swap_out` are declared
counters and drawn as bytes per second (see
[the history API](api.md#counters)). The type matters because `rate()` treats
a counter's drop as a reset — a reboot zeroes both — where over a gauge it
would read as a large decrease. Multiply by `sysmon_memory_page_size_bytes`,
the host's own page size (omitted when unknown), for bytes:

```
rate(sysmon_swap_out_pages_total[5m]) * sysmon_memory_page_size_bytes
```

### Drive health by serial, and NVMe write volume

A drive's health series — `sysmon_disk_smart_healthy`,
`sysmon_disk_power_on_hours`, and for NVMe `sysmon_disk_wear_percent` and
`sysmon_disk_spare_percent` — carry the drive's `serial` beside `device`.
Device names are assigned at boot, and NVMe names can change between boots (on
ci-node-1 a rebuild swapped two of four), so `device` alone can join one
drive's history to another's. Select or group on `serial` to follow a drive.
`device` stays so existing queries keep working. A drive that reports no serial
has no `serial` label, never an empty one.

NVMe drives also export their lifetime read and write volume from the SMART log,
as **counters**. They are present only when the drive reported a non-zero
value, so a disk with no NVMe data units has none of these series:

| Metric | Meaning |
|---|---|
| `sysmon_disk_data_units_written_total` | Data units written, as the drive counts them |
| `sysmon_disk_data_units_read_total` | Data units read |
| `sysmon_disk_written_bytes_total` | Data units written × 512000 bytes |
| `sysmon_disk_read_bytes_total` | Data units read × 512000 bytes |

One NVMe data unit is 1000 × 512 = 512000 bytes, as the NVMe specification
defines it. That is a decimal thousand, not 1024.

The weekly question, how much did each drive write and how much did its wear
move, is:

```
sum by (instance, serial) (increase(sysmon_disk_written_bytes_total[7d]))
sum by (instance, serial) (delta(sysmon_disk_wear_percent[7d]))
```

Wear is a gauge, so its weekly change is `delta()`. `increase()` is for
counters, and it would read any dip in a gauge as a reset. The `sum by serial`
is what keeps the answer right across a reboot that renames the device: the
week then holds two series for one drive, one per device name, and the sum
joins them. Wear is reported in whole percent, so a week's `delta()` is often 0
and is interpolated when it isn't. Use it to compare drives over a week, not as
a precise figure.

The mon0 History view does not offer these. Its catalog charts a counter as a
`rate()` whose window follows the chart's step (see
[the history API](api.md#counters)), so it cannot hold a fixed seven-day
`increase()`. Run the queries above in Prometheus directly.

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
that one is the dashboard's stream rate, and a browser watching at 500ms must
not write two samples a second into a database.

#### Every sample is a real measurement

Owner ruling on #173, 2026-09-22: **a reading is written once, stamped with the
time it was collected, and never again.** Every `sample_interval_ms` the
exporter looks at the node's latest snapshot; if that snapshot has been written
already it writes nothing. So `sample_interval_ms` is a ceiling on the write
rate, not the write rate: a dashboard collecting every 60s under the default 15s
sampling writes one reading a minute, and one collecting every second writes the
latest one every 15s.

The exporter used to write the cached snapshot at every tick, re-dated to the
tick. With collection slower than sampling, gauges drew a staircase of copies
presented as fresh measurements, and a counter's `rate()` over a short window
saw a flat run and then a jump — zero, then a spike — instead of the rate that
happened. Now the time between two stored samples is the time between two
collections, and any rate taken over them is the true one.

Timestamps strictly increase per series, because remote write requires it: a
receiver refuses an out-of-order or duplicate sample with a 4xx for the **whole
request**, so one such sample would cost every other series in its batch. The
exporter therefore keeps the newest timestamp written for each source — this
node, and each peer separately (below) — and:

| The latest reading is… | What happens |
|---|---|
| newer than the last one written | written, at its collection time |
| the same one (same collection time) | nothing — it is not a new measurement |
| not newer, but a different reading — the source's clock **stepped backwards** | dropped, its samples counted in `sysmon_remote_write_samples_dropped_total_current`, logged once per step |
| undated (a zero time) | dropped and counted, logged once — there is no truthful time to write it at |

A backwards clock step therefore leaves a gap exactly as long as the step, and
the series resumes by itself as soon as the clock passes the newest timestamp
written. Nothing is re-dated to fill the gap: the receiver already holds those
timestamps for the series and would refuse the readings anyway, and a re-dated
reading is one the history would then misreport. A large step (a clock that ran
hours ahead and was corrected) is a gap of that size; the fix is the host's
clock, and the dropped counter and the log line say which source it was.

**Gaps and the 5-minute lookback.** Prometheus answers each point of a range
query with the newest sample in the five minutes before it, and remote write
sends no staleness markers. Readings land less than one collection interval
plus one sample interval apart — at most 75s at the slowest dashboard rate (60s)
and the default sampling — well inside the lookback, so a chart at any step is
continuous: at a step finer than the collection interval it holds each reading
until the next, which is what was measured. Only a real absence longer than five
minutes — a stopped monitor, an unreachable peer, a clock step of that size —
leaves points missing, and it should. A monitor that stops no longer has its
last snapshot written forever, so a stopped node now shows as stopped.

A cadence slower than the lookback would write history that is stale between
its own samples, so **while `remote_write_url` is set, a configuration is
refused** (`HistoryCadenceError`) — on reload, once this node has read the lookback
— if two stored samples could be further apart than the database's query
lookback, read from the database itself (see [the history API](api.md#counters) for what each
backend reports). A writer stores the newest reading each time it ticks, so
samples can be a tick **plus** a collection apart, and both are counted:

- `sample_interval_ms` plus the collection interval — the larger of
  `server.interval_ms` and 60s, the slowest the dashboard can switch to at
  runtime — plus `prometheus.timeout_ms`, since the exporter flushes in line
  and samples nothing while a write is outstanding;
- with peers to relay, the fleet poll — the larger of `fleet.poll_interval_ms`
  and the 60s the dashboard can override it to at runtime — plus
  four `fleet.timeout_ms` (the poller sleeps after each refresh, which waits
  for its slowest peer -- asked for its summary, then its snapshot -- so one
  peer's readings can be two such refreshes apart), plus
  `prometheus.timeout_ms` for the write. The peers' own
  collection intervals are not this node's to configure, and are checked per
  peer instead, below.

**Each relayed peer is checked against the interval it reports.** Its summary
carries `collection_interval_ms`, and two of its stored samples can be a relay
tick plus that interval apart. The tick is how a peer that serves a summary has
its history fetched, by its own worker: `sample_interval_ms` plus, for each round
of the queue it can wait in (history fetches run four at a time:
`ceil(peers / 4)` rounds), two timeouts — a history request and its fallback to
the whole snapshot — plus `prometheus.timeout_ms` once, for the write that
carries the reading to the database. A peer too old to serve a summary is read
by its snapshot, which reports no interval, so it is never judged by a tick: it
is "history unverified", below. The gate is asked before
the fetch and again before the reading is handed on, so an approval that went
stale during the fetch is not acted on. If that gap exceeds the database's lookback, the peer's
readings are **not written** — nor fetched: the check is made from the summary
before the history payload is asked for — and its fleet card says "history not
recorded", with the figures. A peer that reports no interval
(an older build; any peer read by snapshot), or a database whose lookback has
not been read yet, is written and marked "history unverified": nothing is
assumed in place of the missing figure. The check asks the database nothing; it
uses the last lookback the database gave, which the background check keeps and
which stands through a failed refresh until the database gives another — so a
refusal does not lapse, and the peer is not relayed again, merely because the
database is briefly unreachable.

A node that writes no history may run all three as slowly as it likes.

The database asked is the one `query_url` reads from, or — on a node that only
writes — the Prometheus whose `/api/v1/write` receiver `remote_write_url` names.
**Starting and reloading never wait on the database** (owner ruling,
2026-09-23): the load-time check asks it nothing, and uses only a lookback
already read. Until one has been — at startup, always; later, whenever the
database is down, publishes none, or the write path belongs to another
product's receiver — the configuration is accepted and the check is
**pending**, not skipped: a warning in the log and the unit's status
(`systemctl status`, from sdnotify `STATUS=`) say so and why. No lookback is
substituted for the one the database did not give.

A background check runs at once, then every 30 seconds until the lookback is
learned,
and after that every ten minutes (the lookback cache's lifetime, so a database
restarted with a different lookback is noticed) and on every reload. A violation
it finds is reported loudly, in three places:

- the unit's status reads `degraded: ` and the reason;
- the alert `history_cadence_unchartable` fires (Host tab, warning), naming the
  settings that add up to the cadence, the cadence and the lookback — so it
  reaches the dashboard, the event stream, the API and `/metrics` like any other
  alert;
- an error in the log.

All three clear when a reload, or a database lookback raised past the cadence,
fixes it. A database that stops answering after a violation was measured does
not clear it: nothing that decides the cadence has changed, so the node stays
degraded with the re-check pending. A reload is a new configuration, and its
check starts afresh.

For a counter's `rate()`, the window has to hold at least two samples at the
cadence the data really has, which no one setting describes; the history API
measures it from the stored samples — see
[Counters](api.md#counters).

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
label. The reading is the peer's history payload (`GET /api/snapshot/history`,
its snapshot without the process list and per-CPU flags, which no series is
built from), fetched by a per-peer history worker at the sample interval
while remote write is on. The peers keep no `prometheus` section, no storage, and no dependency
on a time-series database, and one machine holds the fleet's past.

Two properties of that write path are deliberate and neither is a detail:

- **The timestamp is the reading's, not the aggregator's clock.** A peer's
  history says when that peer measured, not when this node heard about it. The
  peer's clock is used even though it may disagree with the aggregator's: a
  constant skew shifts that peer's series in time but leaves every rate over it
  exact, whereas the aggregator's receive time would jitter each interval by up
  to a poll interval and falsify exactly the rates #173 set out to fix. Keep the
  fleet on NTP and the skew is milliseconds.
- **Each reading is written once, and nothing is resampled.** Every poll offers
  the peer's reading to the exporter, which writes it only if the peer has
  collected since the reading last written for that peer — a peer polled twice
  between its collections contributes one sample, not two (#173). Writing on the
  exporter's own interval instead would either repeat a reading that happened
  once or discard readings that did happen. The resolution of a fleet series is
  therefore the slower of `fleet.poll_interval_ms` and the peer's own collection
  interval.
- **Each peer is ordered against its own clock.** The newest timestamp written
  is kept per peer, so a peer whose clock runs behind the aggregator's is skew,
  not a backwards step, and is written normally. A peer whose own clock steps
  backwards is handled as above: a gap as long as the step, then it resumes.

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
