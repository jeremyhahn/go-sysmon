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

## Which devices are disks, and which absences are which

`DiskCollector` reports a device only when the kernel has put a medium behind
it — `/sys/block/<dev>/size` is non-zero. Any host with the `nbd` module loaded
gets sixteen empty stubs at boot, and on ci-node-1 they sorted ahead of every
real disk, so the storage panel opened on seventeen rows of zeroes while the
machine was writing 70 MB/s further down. The test is the medium, not a list of
names: on that host `nbd0` still carried lifetime I/O counters from a
disconnected export, so "has it ever done I/O" would have kept the emptiest one.

An unreadable `/sys` reads as *unknown*, not as *empty*, so a host without sysfs
mounted keeps its disk list instead of losing it.

The rule is "devices with media", which is deliberately broader than "unused nbd
stubs": an empty card reader, a drive with no disc, and a device inside a
hotplug or rescan window are also omitted, the last of them for one cycle until
the kernel publishes a size.

SMART is then classified rather than merely attempted, because "no reading"
covers four unrelated situations that used to render identically (see
[`smart_status`](../types/reference.md#diskinfo)):

- **No medium to ask.** Only a device with hardware under it has a `device` link
  in sysfs, which makes this structural rather than a list of name prefixes.
  Device-mapper nodes stay in the disk list — `dm-0` is where `/` lives on
  ci-node-1, and dropping it would take the root filesystem's usage with it.
- **Refused.** Reported first, not because it is the most specific answer but
  because it is a fault in how the service was deployed and it is fixable. The
  two shapes of refusal have different remedies and are reported separately:
  the device node could not be **opened** (file permissions — group
  membership), or the node opened and the kernel rejected the **passthrough**
  (a capability check — `CAP_SYS_RAWIO`, which no group membership satisfies).
  Conflating them produces confident advice that cannot work: on ci-node-1 the
  node opens and the ioctl is refused. The probe below is skipped once we know
  we were refused, since it would be refused identically.
- **Behind a RAID controller** — meaning *served by one and did not answer
  SMART directly*, which is what the evidence supports. The SCSI host driver is
  knowable from `/sys/class/scsi_host/<host>/proc_name`; whether **this** device
  is a volume or a passthrough disk is not, so the reason says "if it is a
  volume" rather than asserting it. This is consulted only for failures that
  are *not* refusals and where **nothing came back at all**. A checksum
  failure, an all-zero sector, a short transfer or a sector that decoded to no
  attributes all mean the command got through and bytes were returned, so the
  controller was not what stood in the way; blaming it would send the reader
  looking for member drives instead of at the device in front of them. Note the
  claim is deliberately no stronger than that — an all-zero payload is what a
  bridge returns when it answers *without asking the drive*, so "bytes came
  back" is knowable and "the drive replied" is not. A JBOD disk that was merely refused would likewise be labelled
  as having member drives it does not have, hiding the fixable fault behind the
  claim. Measured on ci-node-1, the ordering costs nothing: a PERC volume on
  a host that *does* hold `CAP_SYS_RAWIO` fails with `unsupported scsi opcode`,
  a check condition rather than `EPERM`, so this is still reached wherever it is
  the whole truth. When a refusal happens on a RAID controller the controller is
  still named in the reason, because on a volume no capability will produce a
  reading and the operator should know that before granting one.
- **The device says it has none.** Asked with ATA IDENTIFY DEVICE over SG_IO,
  never over `HDIO_DRIVE_CMD`: that path segfaults the service on at least one
  deployed host (#20), and this probe runs only after a read has already failed,
  where a crash would be the worst possible way to report bad news. A response
  whose command-set words are marked invalid yields an error, not a `false` —
  "the drive did not say" must not become "the drive said no".
All of the classification reads world-readable sysfs, which matters most in
exactly the case where the reason is that the service is unprivileged.

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
