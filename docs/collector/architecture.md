# Collector architecture

`pkg/collector` reads system metrics from the kernel — procfs, sysfs, hwmon and
a couple of ioctls — and assembles them into one `types.Snapshot`. There is no
agent and no daemon. Every number in the UI came out of a file the kernel
exposes, or an ioctl issued against a block device.

## Data flow

```
+----------------+     +-------------------+     +-----------+
|  Linux kernel  |     |  SystemCollector  |     |  Monitor  |
|  procfs/sysfs  +---->+  (sub-collectors) +---->+  (poll)   |
|  hwmon / ioctl |     |                   |     |           |
+----------------+     +-------------------+     +-----+-----+
                                                       |
                                           +-----------+-----------+
                                           |           |           |
                                        Server       GUI          CLI
                                         (SSE)     (Wails)     (one-shot)
```

## Sub-collectors

Each one owns a subsystem and implements a single method:

```go
type Collector interface {
    Collect() error
}
```

| Sub-collector      | Source          | What it reads                                     |
|--------------------|-----------------|---------------------------------------------------|
| `HostCollector`    | gopsutil, DMI   | Hostname, OS, kernel, board, BIOS, uptime         |
| `CPUCollector`     | gopsutil, sysfs | Per-core usage, model, topology, flags, frequency |
| `GPUCollector`     | NVML, sysfs     | NVIDIA, Intel and AMD utilisation, memory, thermals |
| `MemoryCollector`  | gopsutil, SMBIOS| RAM and swap usage, buffers, cache, per-DIMM hardware |
| `DiskCollector`    | gopsutil, SMART | Partitions, I/O counters, SMART and NVMe health   |
| `NetworkCollector` | gopsutil, sysfs | Interface detail, traffic counters, rates         |
| `ProcessCollector` | gopsutil        | Per-process CPU, memory and I/O                   |
| `LoadCollector`    | gopsutil        | 1, 5 and 15-minute load averages                  |
| `SensorCollector`  | sysfs, hwmon    | Core temperatures, voltages, fans, RAPL, PSI, zones |
| `VirtCollector`    | cgroups, procfs | Containers and virtual machines                   |
| `RuntimeCollector` | Docker API      | Image inventory (opt-in; see [virtualization](virtualization.md)) |

## Frequency tiering

Metrics do not change at the same rate, and some are far more expensive to read
than others. A SMART ioctl can take tens of milliseconds and wakes a sleeping
disk; CPU usage is a cheap read from `/proc/stat`. Running both at 500ms would
mean spinning up drives twice a second to watch a number that moves once an
hour.

So the collector counts ticks and only runs the expensive collectors on some of
them:

| Tier   | Runs            | Collectors                          |
|--------|-----------------|-------------------------------------|
| Fast   | Every tick      | Host, CPU, memory, network, load    |
| Medium | Every 5th tick  | GPU, process, sensors, virt         |
| Slow   | Every 30th tick | Disk (the SMART ioctls), runtime    |

A skipped collector still contributes its last cached value, so the snapshot
shape never changes and a consumer never has to handle a missing field.

One consequence worth knowing: on the slow tier at a 1s interval, a disk needs
roughly a minute of uptime before two samples exist and rates become real.

Tiering is on for streaming (GUI and server) and off for one-shot CLI commands,
where you asked for a reading and should get a fresh one.

## Sensor merging

hwmon reports temperatures against `(package, core)` identifiers; `/proc/stat`
reports usage against logical CPU index. They are different namespaces. After
all sub-collectors run, `mergeSensorIntoCPUs` matches them up and fills in
`CPUInfo.TemperatureCelsius` and `CPUInfo.VoltageV`.

This is fiddlier than it sounds. `coretemp` numbers its hwmon inputs sparsely —
`temp1`, `temp2`, `temp6`, `temp10` on a 24-thread part — so scanning
sequentially and stopping at the first gap finds exactly one core's temperature
and silently drops the rest. The collector globs the directory instead.

## Thread safety

There are no mutexes in the collection path. Each sub-collector stores its
latest result behind an `atomic.Pointer`, and the tick counter is an
`atomic.Uint64`. A reader always gets a complete, internally consistent result —
just possibly one tick old, which is the correct trade for a monitor.
