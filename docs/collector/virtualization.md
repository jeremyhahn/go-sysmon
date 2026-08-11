# Virtualization

Reports the containers and virtual machines running on the host.

Both are observed **from the host side only**, using files the kernel already
exposes. The collector opens no container runtime socket, contacts no guest
agent, and needs no elevated privileges.

## Why host-side only

A container's resource usage is accounted for by the kernel in its cgroup, so
reading `/sys/fs/cgroup` gives exact CPU, memory, PID and I/O figures without
asking Docker or containerd for them. Avoiding the daemon socket keeps the
collector free of a runtime dependency and of the permissions that socket
carries.

The same reasoning applies to VMs: the hypervisor process holds the guest
configuration on its command line and its resource usage in procfs. Metrics
from *inside* a guest would require a guest agent, which is a different trust
boundary and is deliberately out of scope.

## Containers

### Discovery

The cgroup tree is walked to a depth of four, looking for directories whose
name encodes a container ID. Every mainstream runtime uses this convention:

| cgroup directory prefix | Runtime |
|---|---|
| `docker-<id>.scope` | docker |
| `libpod-<id>.scope` | podman |
| `crio-<id>.scope` | crio |
| `cri-containerd-<id>.scope` | containerd |
| `lxc-<id>`, `lxc.payload.<id>` | lxc |

A name only counts as a container when the remainder is at least 12 hex
characters, so ordinary units such as `docker.service` are not misread as
containers.

A scope whose `cgroup.procs` is empty has already exited and is skipped rather
than reported as a container with zeroed metrics.

### Metrics

| Field | Source |
|---|---|
| `cpu_usage_usec`, `cpu_percent` | `cpu.stat` → `usage_usec`, delta over elapsed time |
| `memory_bytes` | `memory.current` |
| `memory_limit_bytes`, `memory_percent` | `memory.max`; `max` means unlimited and reports 0 |
| `pids` | `pids.current` |
| `read_bytes`, `write_bytes` | `io.stat`, summed across devices |
| `read_bytes_rate`, `write_bytes_rate` | delta over elapsed time |
| `process_count` | line count of `cgroup.procs` |

`cpu_percent` is a percentage of a **single core**, so a container using two
cores fully reports `200` — the same convention as `docker stats`.

### Diagnostics

These are the fields that explain *why* a container is slow, rather than just
how much it is using.

| Field | Source | What it tells you |
|---|---|---|
| `cpu_limit_cores` | `cpu.max` | The quota in whole cores; 0 is unlimited |
| `nr_throttled` / `nr_periods` / `throttled_percent` | `cpu.stat` | How often the kernel ran the container out of quota and stopped it. **A container can average well below its limit and still be throttled in bursts** — this is the usual explanation for latency that CPU usage does not account for |
| `throttled_usec` | `cpu.stat` | Total wall-clock time spent stopped |
| `cpu_set` | `cpuset.cpus.effective` | CPU affinity; a narrow mask caps throughput regardless of quota |
| `memory_peak_bytes` | `memory.peak` | High-water mark. This, not current usage, is what a memory limit must accommodate |
| `anon_bytes` / `file_bytes` | `memory.stat` | Process memory vs reclaimable page cache. Near the limit on cache is usually fine; on anonymous memory it is an OOM candidate |
| `swap_bytes` | `memory.swap.current` | Swapping is a latency problem long before it is a capacity one |
| `major_faults` | `memory.stat` `pgmajfault` | Faults served from disk. A climbing value means thrashing |
| `oom_kills` / `oom_events` | `memory.events` | Processes killed by the OOM killer. Non-zero explains an otherwise mysterious restart |
| `pids_peak` / `pids_max` | `pids.peak`, `pids.max` | A leak that current usage hides, and how close it is to the fork limit |
| `cpu_pressure` / `memory_pressure` / `io_pressure` | PSI `*.pressure` | Share of the last 10s in which work was **stalled** waiting on that resource. Non-zero CPU pressure with low CPU usage means the container is waiting for runtime it cannot get |
| `read_iops` / `write_iops` | `io.stat` | Operation counts, which expose small-random-I/O workloads that byte rates make look idle |
| `uptime_seconds` | main process start time | Short uptime on a long-lived service means it restarted |

The CLI prints an **Attention** block below the table listing any container
that is throttled, OOM-killed, at its memory peak, under stall pressure, or
swapping, so these conditions do not have to be spotted by eye.

Rates are per second, not per polling interval. A container seen for the first
time reports `0` rather than its lifetime totals, because there is no baseline
to subtract yet.

### Naming

`name` is the **main process name**, read from `/proc/<pid>/comm`. The
human-friendly name you passed to `docker run` lives in the runtime daemon's
database and cannot be read without connecting to its socket. In practice the
process name is usually the useful one (`grafana`, `prometheus`, `webui`).

## Virtual machines

VMs are found by scanning for hypervisor processes:

| Process name prefix | Hypervisor |
|---|---|
| `qemu-system`, `qemu-kvm` | qemu/kvm |
| `cloud-hyper` | cloud-hypervisor |
| `VBoxHeadless` | virtualbox |
| `vmware-vmx` | vmware |

Guest configuration is parsed from the command line:

| Field | Source |
|---|---|
| `name` | `-name guest=NAME`, falling back to the process name |
| `uuid` | `-uuid` |
| `vcpus` | `-smp N` or `-smp cpus=N` |
| `memory_bytes` | `-m`, accepting `4096`, `4G` and `size=65536000k` |
| `accelerator` | `-accel`, or `accel=` inside `-machine` |
| `mac_addresses` | MACs in `-device` netdev arguments |
| `disk_images` | `filename` in `-blockdev` arguments |
| `rss_bytes` | the hypervisor process itself |
| `cpu_percent` | CPU-time delta over the sampling window, as a percentage of one core. **Not** the process's lifetime average, which for a guest running for days understates current usage severalfold |

`memory_bytes` is the guest's **configured** RAM while `rss_bytes` is what the
hypervisor actually occupies on the host. The host figure is normally lower
because guest memory is faulted in lazily.

### Operational fields

| Field | Source | What it tells you |
|---|---|---|
| `vcpu_threads` | `/proc/<pid>/task/*/comm` matching `CPU N/KVM` | vCPU threads the hypervisor actually created. A mismatch with `vcpus` means the guest is still starting or the topology changed at runtime |
| `thread_count` | thread directory count | Every thread including I/O and emulation workers |
| `uptime_seconds` | process start time | How long the guest has been running |
| `net_rx_rate` / `net_tx_rate` | joined from the tap interfaces | The guest's real network throughput, measured from the host. **Reported from the guest's perspective**: traffic the host receives on the tap is traffic the guest sent |
| `disk_image_bytes` | `stat` of the image files | On-disk size; 0 when the images are not readable by this user |

The GUI compares total vCPUs against host threads and highlights the count
when the guests are oversubscribed.

### Linking a guest to host networking

`tap_interfaces` maps each guest NIC to the host tap device carrying its
traffic. A tap's own MAC differs from the guest NIC MAC only in the first
octet's locally-administered bit, so the last five octets are compared. This is
what lets you connect a `vnet0` traffic spike in the Network tab to the guest
producing it.

## Interfaces

### CLI

```
sysmon containers            # table of running containers
sysmon containers --json     # machine-readable
sysmon containers --index 2  # a single container
sysmon vms                   # detail block per virtual machine
sysmon vms --json
sysmon vms --index 0
```

Both accept the global `--refresh` flag to poll continuously.

### API and GUI

The snapshot carries a `virtualization` object:

```json
{
  "virtualization": {
    "cgroup_version": "v2",
    "runtimes": ["docker"],
    "containers": [ ... ],
    "vms": [ ... ]
  }
}
```

There are two tabs, mirroring the two CLI subcommands:

- **Containers** — rollup (count, total CPU and memory, PIDs, throttled count,
  OOM kills), a table with limit/throttling/peak/uptime columns, and a detail
  drawer per container showing throttling, memory breakdown, OOM history, PID
  limits and PSI.
- **Virtualization** — rollup (VM count, total CPU, vCPUs against host threads,
  configured and resident RAM) and a detail card per VM.

## Container images and runtime storage

Image inventory is the one thing that **cannot** be read from the kernel. Layer
sizes and image metadata live in the runtime's own database, and its storage
directory is root-owned. So this is the only collector that talks to a daemon,
and it is **disabled by default**: the runtime socket is effectively
root-equivalent access, which a monitor should not take unasked.

Enable it with `sysmon images` (running the command is the opt-in) or
`sysmon serve`.

### What it reports

| Field | Why an operator wants it |
|---|---|
| `root_dir` | Where images, layers and volumes actually live. Frequently relocated away from the default, so it is the first thing to check when a filesystem fills up |
| `storage_driver`, `backing_filesystem` | overlay2 on extfs, and so on |
| `layers_bytes` | Deduplicated size of all image layers |
| `reclaimable_bytes`, `unused_images`, `dangling_images` | What an image prune would return |
| `volumes_*` | Volume count and size, and how much belongs to volumes nothing references |
| `build_cache_*` | Build cache size, which routinely dwarfs the images themselves |
| `images[]` | Per-image tag, size, shared size and how many containers use it, largest first |

Per-image sizes **include layers shared with other images**, so they do not sum
to the total; the reclaimable estimate is capped at the deduplicated size on
disk for the same reason.

### Why it is asynchronous

The runtime's disk usage query walks every layer, volume and cache entry. On a
host with a large image library it takes the better part of a minute — nine
seconds on the machine this was developed against. A snapshot must never block
on that, so the query runs on a background goroutine at most once every five
minutes, and each snapshot serves the last completed result. Engine details
(`/info`) are cheap and are collected inline.

`sysmon images` waits for the first result so a one-shot command prints sizes
rather than zeros.

## Limitations

- **VM disk I/O is not reported.** `/proc/<pid>/io` for a hypervisor running as
  another user (libvirt-qemu) is not readable unprivileged. Guest disk activity
  still shows up in the host's per-device figures on the Storage tab.

- **Container names** are process names, not runtime names (see above).
- **No in-guest metrics.** A guest's own CPU and memory usage, filesystem
  utilisation and processes are not visible from the host.
- **cgroup v1** is detected and reported in `cgroup_version`, but the per
  container metrics are read from v2 files. On a v1-only host the container
  list will be empty.
- **Kubernetes** pods are discovered through their containerd/crio scopes; pod
  level grouping is not reconstructed.
