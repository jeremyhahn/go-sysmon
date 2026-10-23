# Types reference

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of the exported types.

`pkg/types` holds every structure the rest of the project speaks in. The
collector fills them, the monitor passes them around, the server encodes them,
and the frontend decodes them — so this file is the contract between all four.

It imports nothing from the other packages, which is what stops the dependency
graph from becoming a circle.

**The dashboard's TypeScript is generated from these declarations.** Change a
struct here, run `make generate`, and commit the result — `make lint` fails
otherwise. Two files come out, both marked DO NOT EDIT:

| File | What it is |
|---|---|
| `cmd/sysmon/frontend/src/lib/types.generated.ts` | The interfaces the page reads snapshots with |
| `cmd/sysmon/frontend/src/lib/schema.generated.ts` | The runtime schema each arriving frame is validated against |

They were hand-mirrored until #38, and the mirror had drifted: ten fields and a
whole interface that the server was sending and the dashboard did not
declare. See [../gui/architecture.md](../gui/architecture.md) for how the
schema is used and how strict it is.

A field whose legal values are a closed set of strings is a **named Go type
with typed constants** — `Severity`, `NodeStatus`, `BuildMatch`, `NameMatch`,
`ConfigSource`, `HistoryUnit`, `SummarySource`, `TempSource`, `Presence` and
`SMARTStatus` — and its TypeScript union is generated from those constants.
Never declare such a field as a plain `string` with loose constants beside it:
the generator cannot see a set no type names, and there is no hand-written
union for the page to fall back on (#202). Producers use the typed constants,
so a value outside the set has to be spelled as a deliberate conversion.

## Lists are never null

Every slice in these types reaches the wire as a JSON array, never as `null`: a
host with none of something sends `[]`. A nil Go slice encodes as `null`, and
the dashboard reads these lists with `.map`, `.length` and spreads, so a single
`null` used to be a thrown exception that blanked a panel. The owner ruled on
#151 that an empty list and "none" are the same answer, and dropped the
distinction.

`types.ReplaceNilSlices` does it. It walks a value by reflection and replaces
every nil slice it can reach through structs, pointers, slices and arrays with
an empty one, so a list field added later is covered without anyone listing
it. It runs wherever a value is finished and
before it is shared:

| Where | Covers |
|-------|--------|
| `collector.SystemCollector.Snapshot` | every snapshot, served or printed by the CLI |
| the monitor, before it publishes | anything a `Snapshotter` returns, including the alerts attached on the way |
| `fleet`, as a peer's snapshot or configuration is decoded | a peer on an older build that still sends `null` |
| the server's `writeJSON` | every REST response built per request: fleet view, settings, history |

It writes only to a slice that is nil, so a value that has been through it
once is only read by later passes — which is what makes the later passes safe
over published, shared data. A pass over a busy host's snapshot costs a few
microseconds, about 1–3% of encoding it (`BenchmarkReplaceNilSlices`).

The guard is `TestReplaceNilSlices_NoListInAnyPayloadEncodesAsNull`. It builds
every wire payload with nil lists at every depth, driven by reflection over
these types, and fails if any list encodes as `null` — or if the fixture ever
stops reaching a list field, so it cannot pass by checking nothing. Tests at
each call site above use `typestest.NullSlices` to check their own output the
same way.

Maps, interfaces and unexported embedded structs are not entered: a map's
values are not addressable, rewriting one in place would write to a map other
goroutines may be reading, and reflection will not set a field promoted from an
unexported embedding. No payload has any of these today, and
`TestWirePayloads_HoldNothingTheWalkCannotEnter` fails if one is added, so the
lists inside it cannot be skipped silently.

## Snapshot

The top-level structure. One of these per collection.

| Field        | Type             | Description                           |
|--------------|------------------|---------------------------------------|
| `timestamp`  | `time.Time`      | When this snapshot was taken          |
| `host`       | `HostInfo`       | General host information              |
| `cpu_summary`| `CPUSummary`     | Aggregate CPU topology                |
| `cpus`       | `[]CPUInfo`      | Per-logical-processor details         |
| `gpus`       | `[]GPUInfo`      | Per-GPU details                       |
| `memory`     | `MemoryInfo`     | RAM, swap, and DIMM hardware          |
| `disks`      | `[]DiskInfo`     | Per-disk details with SMART health    |
| `networks`   | `[]NetworkInfo`  | Per-interface details with counters   |
| `load_avg`   | `LoadAverage`    | 1/5/15-minute load averages           |
| `processes`  | `ProcessSummary` | Process counts and top process list   |
| `sensors`    | `SensorData`     | Temperatures, voltages, fans, PSI     |

## HostInfo

| Field              | Type     | Description                    |
|--------------------|----------|--------------------------------|
| `hostname`         | `string` | System hostname                |
| `os`               | `string` | Operating system name          |
| `platform`         | `string` | Distribution name              |
| `platform_version` | `string` | Distribution version           |
| `kernel_version`   | `string` | Kernel version string          |
| `kernel_arch`      | `string` | Architecture (e.g. `x86_64`)  |
| `uptime`           | `uint64` | Uptime in seconds              |
| `boot_time`        | `uint64` | Boot time as Unix timestamp    |
| `board_vendor`     | `string` | Motherboard vendor             |
| `board_name`       | `string` | Motherboard model              |
| `board_version`    | `string` | Board revision                 |
| `bios_vendor`      | `string` | BIOS vendor                    |
| `bios_version`     | `string` | BIOS version                   |
| `bios_date`        | `string` | BIOS release date              |

## CPUSummary

| Field              | Type      | Description                      |
|--------------------|-----------|----------------------------------|
| `sockets`          | `int`     | Number of physical sockets       |
| `cores_per_socket` | `int`     | Physical cores per socket        |
| `threads_per_core` | `int`     | Threads per physical core        |
| `total_cores`      | `int`     | Total physical cores             |
| `total_threads`    | `int`     | Total logical processors         |
| `max_mhz`         | `float64` | Maximum frequency (MHz)          |
| `min_mhz`         | `float64` | Minimum frequency (MHz)          |

## CPUInfo

Per-logical-processor fields including `index`, `model_name`, `vendor_id`, `family`, `model`, `stepping`, `physical_id`, `core_id`, `cores`, `threads`, `mhz`, `cur_mhz`, `cur_mhz_available`, `cur_mhz_source`, `cache_size`, `microcode`, `flags`, `usage_percent`, `usage_available`, `temperature_celsius`, and `voltage_v`.

`cache_size` is in kilobytes (KB), as gopsutil reports it. `mhz` is the cpufreq
maximum where cpufreq exists; `cur_mhz` is the core's current frequency, read
from `/sys/devices/system/cpu/cpuN/cpufreq/scaling_cur_freq` (`cur_mhz_source`
`cpufreq`) or, without cpufreq, the core's `cpu MHz` in `/proc/cpuinfo`
(`cpuinfo`). When neither exists `cur_mhz_available` is false, `cur_mhz` is 0
and the source is empty: unknown, not a stopped core.

`usage_percent` is the difference of two `/proc/stat` readings, so it exists
only when `usage_available` is true. It is false on the collector's first
collection, which has nothing to difference against; for a core whose
`/proc/stat` line was missing or malformed in either reading; and for a core
whose counters did not advance or went backwards. `usage_percent` is then 0,
which is **unknown, not idle**. Every consumer — the `cpu_high` alert, the
`sysmon_cpu_usage_percent` gauge, the fleet summary, the CLI, the tray and the
dashboard — leaves such a core out of its mean (`types.MeanCPUUsage`), and a
node with no measured core reports its CPU as unknown. A one-shot CLI command
takes two readings half a second apart so its usage is measured, and prints
`-` for a core the second reading still could not measure.

## MemoryInfo

Physical RAM fields: `total_bytes`, `used_bytes`, `available_bytes`, `free_bytes`, `used_percent`.

Linux kernel fields: `buffers_bytes`, `cached_bytes`, `shared_bytes`, `slab_bytes`.

Swap **occupancy** — how much swap holds pages: `swap_total_bytes`,
`swap_used_bytes`, `swap_free_bytes`, `swap_percent`.

Swap **paging counters** — how much swapping is happening: `swap_in_pages`,
`swap_out_pages` (cumulative pages since boot, read from `/proc/vmstat`'s
`pswpin`/`pswpout`), `page_size_bytes`, `swap_counters_available`.

The two answer different questions, and only the second says whether a host is
in trouble. A machine that paged something out days ago and has been idle since
carries resident swap and is perfectly healthy; a machine paying disk latency
for memory it is using right now is not, and `swap_used_bytes` cannot tell them
apart. Nor can a consumer recover the difference by differencing occupancy: a
host paging out and back in at the same rate has flat `swap_used_bytes` while
thrashing hard, which is the case worth catching. The counters are cumulative
rather than pre-computed rates because a rate needs two readings and a window,
and the window belongs to the consumer.

`swap_counters_available` is false when `/proc/vmstat` could not be read, or
carried neither counter (a kernel built without `CONFIG_SWAP` omits them). The
counts are zero in that case, and zero is also what a host that has never paged
reports — so an unread reading must not be mistaken for a quiet one. Multiply
by `page_size_bytes` for bytes; the kernel counts pages, and that is not 4096
everywhere.

`GET /metrics` exports the two totals as Prometheus counters,
`sysmon_swap_in_pages_total` and `sysmon_swap_out_pages_total`, and only when
`swap_counters_available` is true — see
[the Prometheus section](../server/alerting-fleet-config.md#prometheus). It
exports `page_size_bytes` as the gauge `sysmon_memory_page_size_bytes`, when
known, so those counters can be read in bytes.

Hardware: `dimms` (array of `DIMMInfo`), `temp_sensor_detected`.

### DIMMInfo

Physical memory module details: `location`, `bank_locator`, `manufacturer`, `part_number`, `serial_number`, `size_bytes`, `speed_mts`, `configured_speed_mts`, `type`, `form_factor`, `data_width_bits`, `total_width_bits`, `rank`, `min_voltage`, `max_voltage`, `configured_voltage`, `temperature`.

## DiskInfo

Identity: `name`, `model`, `serial`, `vendor`, `size_bytes`, `drive_type`, `controller`, `transport`, `rotational`.

Usage: `total_bytes`, `used_bytes`, `free_bytes`, `used_percent`.

I/O counters: `read_count`, `write_count`, `read_bytes`, `write_bytes`, `io_time_ms`, `weighted_io_ms`.

SMART: `smart_enabled`, `smart_healthy`, `smart_status`, `smart_reason`, `smart_attrs`, `temperature_celsius`, `power_on_hours`, `power_cycles`.

`smart_enabled` and `smart_healthy` mean what they have always meant: a reading
was obtained, and it was good. They cannot say *why* there is no reading, and
four unrelated situations produced the same pair of `false`s. `smart_status`
carries the distinction:

| `smart_status` | meaning |
|---|---|
| `""` | not classified. A server older than this field sends nothing. **Treat as "no claim" and say less — never as a reassuring one.** |
| `not_applicable` | a virtual block device (device-mapper, md, an unbound nbd or loop): there is no medium to ask |
| `behind_controller` | served by a hardware RAID controller and did not answer SMART directly. Whether *this* device is a volume or a passthrough disk is not knowable from sysfs, so `smart_reason` says "if it is a volume" rather than asserting it |
| `unsupported` | the device answered, and reports that it implements no SMART |
| `unreadable` | the device should be able to answer and did not. `smart_reason` says what stopped it, and names the remedy that applies: a refused **open** is file permissions (the `disk` group), a refused **passthrough** is a capability check (`CAP_SYS_RAWIO`, which group membership does not grant). Either way it is a fact about the deployment rather than about the hardware |
| `ok` / `failing` | a reading |

`smart_reason` is a sentence for the statuses that have a why, and is empty for
`ok` and `failing`.

NVMe health: `wear_level_percent`, `available_spare_percent`, `spare_threshold_percent`, `media_errors`, `error_log_entries`, `unsafe_shutdowns`, `critical_warning`, `data_units_read`, `data_units_written`, `warning_temp_time_minutes`, `critical_temp_time_minutes`, `life_remaining_percent`, `estimated_hours_left`.

Firmware: `firmware_version`, `nvme_version`.

Partitions: array of `PartitionInfo` with `device`, `mountpoint`, `fstype`, `opts`.

Members: array of `DiskMember`, the physical drives behind a hardware RAID
controller. Empty for everything that is not a controller volume, and absent
from servers older than the field.

## DiskMember

`serial`, `model`, `vendor`, `device_id`, `enclosure`, `slot`, `online`,
`temperature_celsius`, `smart_enabled`, `smart_healthy`, `smart_status`,
`smart_reason`.

`online` is the **controller's** verdict — the drive is in service as part of
its array — and is what `RAIDInfo.active_devices` counts. It is a different
question from whether the drive answers a health command: a failed drive can
still answer, and a working one can refuse a command it does not implement.

**The bay is `enclosure` and `slot`, two numbers, and there is no field called
`bay`.** The controller reports the pair, so the payload carries the pair — but
"bay 64:7" is what people say out loud, and two readers in a row have asked for
`bay` or `position` and got nothing back for every drive. A missing key returns
no value rather than an error, so the wrong name reads as missing data. The
alert messages render the pair as `bay <enclosure>:<slot>`; the payload does
not.

A drive behind a RAID controller is not a block device: it has no device node,
no partitions and no I/O counters. `serial` is the only identity that survives a
reboot — `device_id` and `slot` describe where the drive is plugged in, not
which drive it is — and it is what the `sysmon_disk_member_*` metrics are
labelled by.

This is a separate type from `RAIDMember`, which names a block device (`sda2`)
because an md array's components are devices the host addresses directly.

## GPUInfo

Identity: `index`, `name`, `uuid`, `serial`, `driver_version`, `vbios_version`, `compute_mode`, `perf_state`.

Memory: `memory_total_mib`, `memory_used_mib`, `memory_free_mib`, `memory_percent`.

Utilization: `gpu_util_percent`, `memory_util_percent`, `encoder_percent`, `decoder_percent`.

Thermals: `temperature_gpu`, `temperature_memory`, `fan_speed_percent`.

Power: `power_draw_w`, `power_limit_w`, `power_default_w`, `power_max_w`.

Clocks: `clock_graphics_mhz`, `clock_memory_mhz`, `clock_video_mhz`, `clock_max_gfx_mhz`, `clock_max_mem_mhz`.

PCIe: `pci_bus_id`, `pcie_gen_current`, `pcie_gen_max`, `pcie_width_current`, `pcie_width_max`, `pcie_rx_mbps`, `pcie_tx_mbps`.

ECC: `ecc_enabled`, `ecc_single_bit`, `ecc_double_bit`.

Processes: `process_count`.

## NetworkInfo

Identity: `name`, `hardware_addr`, `addresses`, `mtu`, `flags`, `speed_mbps`, `duplex`, `driver`, `is_up`, `is_loopback`, `is_virtual`.

Traffic: `bytes_sent`, `bytes_recv`, `packets_sent`, `packets_recv`, `errors_in`, `errors_out`, `drops_in`, `drops_out`.

Rates: `bytes_sent_rate`, `bytes_recv_rate`. `rates_available` is false when
they were not measured: on the first collection, for an interface that has just
appeared, and when the counters could not be read. The rates are then zero only
because they have to be something.

Addressing: `address_details` is `addresses` in structured form, one
`AddressInfo` per address in the same order (a string that is not CIDR is left
out):

| Field           | Type     | Description                                   |
|-----------------|----------|-----------------------------------------------|
| `address`       | `string` | IP address without the prefix                 |
| `prefix_length` | `int`    | CIDR prefix length                            |
| `netmask`       | `string` | Dotted netmask for IPv4, empty for IPv6       |
| `family`        | `string` | `ipv4` or `ipv6`                              |

Routing: `gateway_ipv4`, `gateway_ipv6` (the interface's next hop, empty when
it has none), `default_route_ipv4`, `default_route_ipv6` (it carries the
system's preferred default route of that family). `routes_available` is false
when the IPv4 routing table could not be read, so the routing fields are
unknown rather than empty.

Wireless (`wireless`, wifi interfaces only): `ssid`, `bssid`, `link_quality`,
`signal_level_dbm`, `noise_level_dbm`, `frequency_mhz`, `channel` and
`frequency_available`, `tx_bitrate_mbps`, `rx_bitrate_mbps` and
`bitrate_available`. Each `*_available` flag is false when its values were not
learned, which makes the zeros beside it meaningless.

## SensorData

| Field             | Type               | Description                    |
|-------------------|--------------------|--------------------------------|
| `core_temps`      | `[]CoreTemp`       | Per-core temperature readings  |
| `core_voltages`   | `[]CoreVoltage`    | Voltage readings from hwmon    |
| `package_power`   | `[]PackagePower`   | RAPL power readings            |
| `thermal_throttle`| `[]ThrottleInfo`   | Throttle event counts          |
| `thermal_zones`   | `[]ThermalZone`    | System thermal zones           |
| `fans`            | `[]FanInfo`        | Fan speed readings             |
| `psi`             | `PSIData`          | Pressure stall information     |

## VirtInfo

Containers and VMs; the fields are described with their collector in
[docs/collector/virtualization.md](../collector/virtualization.md). Two of them
decide whether the dashboard has a Containers and a Virtualization tab at all:

| Field | Type | Meaning |
|-------|------|---------|
| `container_runtime` | `StackDetection` | whether a container runtime is installed or containers are running |
| `hypervisor` | `StackDetection` | whether a VM stack is installed or VMs are running |
| `runtime_disk` | `RuntimeDisk` | free space on the container runtime's data root, or `/`; an empty `path` is unknown (#239) |

`StackDetection` is `state`, `evidence` and `reason`.

`state` is a `Presence`: `present`, `absent` or `unknown`. `absent` is a proof
-- every place the software could be was checked -- and is the only state that
hides a tab. `unknown` says the node could not settle it, and `reason` says
why. A peer on a build without these fields sends neither, which consumers must
read as `unknown`.

`evidence` lists what was found, sorted by stack, kind and path. Each
`StackEvidence` is `stack` (`docker`, `libvirt`, ...), `kind` and `path`:

| `kind` | Found as | `path` |
|--------|----------|--------|
| `binary` | an executable in a system binary directory | the executable |
| `socket` | a daemon API socket | the socket |
| `module` | a loaded kernel module | `/sys/module/<name>` |
| `control_domain` | a Xen dom0 | `/proc/xen/capabilities` |
| `running` | a container or VM observed running | empty |

The frontend's hand-written `src/lib/types.ts` mirrors these until #38
generates it from the Go types.

## PSIData

| Field       | Type          | Description                                  |
|-------------|---------------|----------------------------------------------|
| `available` | `bool`        | All three resources were read                |
| `cpu`       | `PSIResource` | CPU pressure                                 |
| `memory`    | `PSIResource` | Memory pressure                              |
| `io`        | `PSIResource` | I/O pressure                                 |

`available` is the field that makes the rest of the struct safe to read.
Pressure accounting is absent on kernels built without `CONFIG_PSI` and in
containers that do not expose `/proc/pressure`, and every number below is then
zero — byte-identical to a completely idle machine. A consumer that skips the
flag concludes the hosts it cannot see are the calmest in the fleet, which is
the wrong direction to be wrong in: an unmeasurable node treated as unmeasured
is safe, one treated as idle is the most attractive placement target there is.

It is true only when cpu, memory and io were all read, so a partial reading
fails closed. `PSIResource.available` says which ones individually were.

## PSIResource

| Field         | Type      | Description                                     |
|---------------|-----------|-------------------------------------------------|
| `available`   | `bool`    | This resource's file was read and parsed        |
| `some_avg10`  | `float64` | % of time ≥1 task stalled, 10s average          |
| `some_avg60`  | `float64` | Same, 60s average                               |
| `some_avg300` | `float64` | Same, 300s average                              |
| `some_total`  | `uint64`  | Cumulative stall microseconds (some)            |
| `full_avg10`  | `float64` | % of time *every* runnable task stalled, 10s    |
| `full_avg60`  | `float64` | Same, 60s average                               |
| `full_avg300` | `float64` | Same, 300s average                              |
| `full_total`  | `uint64`  | Cumulative stall microseconds (full)            |

`available` is false when the file could not be read, and also when it was read
but did not parse: every line must carry all four of `avg10`, `avg60`, `avg300`
and `total`, each a valid number, and there must be a `some` line — a truncated
read produces the same unquestionable zeros as an absent one. A `full` line is
required of `memory` and `io`. It is optional for `cpu`, whose `full` line
arrived in Linux 5.13, and a missing one leaves the resource available with the
full fields at zero; a `full` line that is present but malformed makes the
whole resource unavailable. The collector logs the malformed file once.

## Errors

Errors are named types, not `fmt.Errorf` calls. That means a caller can react
to a specific failure with `errors.As` instead of matching on message text —
which is how `runRoot` knows a GUI launch failed because the build has no GUI,
and falls back to the CLI overview rather than exiting.

Every package names its errors the same way (handbook `errors.md` §1): a type
that implements `error` ends in `Error` (`CollectorError`, `config.PeerNameError`,
`fleet.InvalidQueryError`). There are no sentinel error values: production code
never calls `errors.New` or `fmt.Errorf`, so every failure has a type to match.
The `errname` and `forbidigo` linters in `make lint` enforce both.

| Type                        | Description                                    |
|-----------------------------|------------------------------------------------|
| `GUIUnavailableError`       | GUI cannot launch (stub build, no display)     |
| `TrayIconEncodeError`       | Tray icon could not be encoded as a PNG        |
| `ServerStartError`          | HTTP server failed to start                    |
| `NoListenerError`           | Server has no bound listener to reach it on    |
| `CollectorError`            | A sub-collector encountered an error           |
| `InvalidIntervalError`      | Polling interval is zero or negative           |
| `MonitorAlreadyRunningError`| `Start()` called on a running monitor          |
| `MonitorNotRunningError`    | `Stop()` or `Snapshot()` called before `Start()`|
