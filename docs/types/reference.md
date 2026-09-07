# Types reference

`pkg/types` holds every structure the rest of the project speaks in. The
collector fills them, the monitor passes them around, the server encodes them,
and the frontend decodes them — so this file is the contract between all four.

It imports nothing from the other packages, which is what stops the dependency
graph from becoming a circle.

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

Per-logical-processor fields including `index`, `model_name`, `vendor_id`, `family`, `model`, `stepping`, `physical_id`, `core_id`, `cores`, `threads`, `mhz`, `cache_size`, `microcode`, `flags`, `usage_percent`, `temperature_celsius`, and `voltage_v`.

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

Rates: `bytes_sent_rate`, `bytes_recv_rate`.

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
but carried no parseable `some` line — a truncated read produces the same
unquestionable zeros as an absent one. A missing `full` line is normal for
`cpu` and leaves the resource available with the full fields at zero.

## Errors

Errors are named types, not `fmt.Errorf` calls. That means a caller can react
to a specific failure with `errors.As` instead of matching on message text —
which is how `runRoot` knows a GUI launch failed because the build has no GUI,
and falls back to the CLI overview rather than exiting.

| Type                        | Description                                    |
|-----------------------------|------------------------------------------------|
| `GUIUnavailableError`       | GUI cannot launch (stub build, no display)     |
| `ServerStartError`          | HTTP server failed to start                    |
| `CollectorError`            | A sub-collector encountered an error           |
| `InvalidIntervalError`      | Polling interval is zero or negative           |
| `MonitorAlreadyRunningError`| `Start()` called on a running monitor          |
| `MonitorNotRunningError`    | `Stop()` or `Snapshot()` called before `Start()`|
