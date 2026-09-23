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

The whole cgroup tree is walked, looking for directories whose name encodes a
container ID; the walk does not descend into a container's own scope. It is
not depth-bounded: rootless runtimes nest under `user@UID.service/app.slice`,
and a desktop's own scopes are already six levels deep, so any bound either
misses containers or leaves the scan unable to vouch for having seen them all.
The tree is small (118 directories on the development host). Every mainstream
runtime uses this convention:

| cgroup directory prefix | Runtime |
|---|---|
| `docker-<id>.scope` | docker |
| `libpod-<id>.scope` | podman |
| `crio-<id>.scope` | crio |
| `cri-containerd-<id>.scope` | containerd |
| `lxc-<id>`, `lxc.payload.<id>` | lxc |
| `<id>` directly under `docker/` (the cgroupfs driver) | docker |

A name only counts as a container when the remainder is at least 12 hex
characters, so ordinary units such as `docker.service` are not misread as
containers.

A container's processes are read from its scope **and every cgroup beneath
it**: a container running systemd delegates its scope and keeps no process in
it directly. A scope with no process anywhere in its subtree has exited and is
skipped rather than reported with zeroed metrics.

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
| `firecracker` (exact) | firecracker |
| `VBoxHeadless`, `VirtualBoxVM` | virtualbox |
| `vmware-vmx` | vmware |

`VBoxHeadless` runs a guest without a window and `VirtualBoxVM` runs one with
it. The `VirtualBox` manager window runs no guest and is not matched. Firecracker
is matched by its whole name, because `firecracker-containerd` manages microVMs
without being one.

Guest configuration is parsed from the command line:

| Field | Source |
|---|---|
| `name` | `-name guest=NAME`, or Firecracker's `--id NAME`, falling back to the process name |
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

## Is there a runtime or a hypervisor at all?

Most hosts run no containers and no VMs, and a tab that always opens onto
"nothing running" is noise. So the snapshot says whether the node has a
container runtime (`container_runtime`) and a VM stack (`hypervisor`), and the
dashboard hides a tab only when the answer is `absent` (#187).

Each is a tri-state with the evidence behind it:

```json
"container_runtime": {
  "state": "present",
  "evidence": [
    { "stack": "docker", "kind": "binary", "path": "/usr/bin/docker" },
    { "stack": "docker", "kind": "socket", "path": "/run/docker.sock" },
    { "stack": "docker", "kind": "running", "path": "" }
  ],
  "reason": ""
}
```

| State | Meaning |
|---|---|
| `present` | at least one piece of evidence was found |
| `absent` | every place the software could be was checked and none of it was there, **and** running workloads could be observed |
| `unknown` | the question could not be settled; `reason` says why |

`absent` is a proof, not a default. A node that could not look is not a node
that looked and found nothing, and only the second may hide a tab. A peer on an
older build sends neither field, which the dashboard reads as `unknown`.

### Evidence

Nothing is executed and no socket is opened: existence is the whole question.

**Running workloads** (`kind: running`) are the containers and VMs described
above. They are evidence whenever the workloads are scanned, so a container
that starts shows the Containers tab in the same snapshot that lists it. A
hypervisor process counts even when its command
line could not be read and it produced no VM entry: it was seen, and seeing it
is the proof. A running qemu guest is reported under `qemu`, the same stack
name its binaries are, so the panel shows one qemu row.

**Binaries** (`kind: binary`) are looked for in `/usr/local/sbin`,
`/usr/local/bin`, `/usr/sbin`, `/usr/bin`, `/usr/libexec`, `/sbin`, `/bin`,
`/snap/bin` and NixOS's `/run/current-system/sw/bin`. Each directory is listed once; a directory that is a symlink to
one already listed (`/bin` on merged-/usr) is skipped, so each executable is
reported once, under its `/usr` path. A symlink counts only if it resolves to
a file: a dangling snap alias, or a directory of that name, is not a runtime.

| Stack | Binaries | Other evidence |
|---|---|---|
| docker | `docker`, `dockerd` | socket `/run/docker.sock` |
| containerd | `containerd` | socket `/run/containerd/containerd.sock` |
| nerdctl | `nerdctl` | |
| podman | `podman` | socket `/run/podman/podman.sock` |
| crio | `crio` | socket `/run/crio/crio.sock` |
| lxc, lxd, incus | `lxc-start`, `lxd`, `incus` | |
| qemu | `qemu-system-*`, `qemu-kvm` | |
| libvirt | `libvirtd`, `virtqemud` | sockets `/run/libvirt/libvirt-sock`, `/run/libvirt/virtqemud-sock` |
| virtualbox | `VBoxManage` | module `vboxdrv` (`/sys/module/vboxdrv`) |
| xen | `xl` | `control_d` in `/proc/xen/capabilities` |
| cloud-hypervisor | `cloud-hypervisor` | |
| firecracker | `firecracker` | |
| vmware | `vmware` | module `vmmon` |

Daemonless podman usually has no socket, which is why binaries count at all.
Only `/run` is probed for sockets: `/var/run` is a symlink to it on every
current distribution, and probing both would report one socket twice.

**What does not count.** `/dev/kvm`, and the `kvm` modules behind it, are
present on nearly every x86 machine whether or not anyone runs VMs there. A Xen
**guest** has `/proc/xen` too; only a control domain (dom0), whose capabilities
say `control_d`, hosts VMs.

### When the answer is unknown

- **A probe failed** for any reason other than the file not existing -- a
  permission error, a symlink loop. It proves nothing either way. Evidence
  found elsewhere still makes the state `present`.
- **sysmon is running in a container.** A container's filesystem is not the
  host's: finding no binaries proves nothing, and finding one proves only that
  the image ships it, so binaries are not looked for at all. This is
  recognised two ways. First, from the files container engines place inside a
  container: `/.dockerenv`, `/run/.containerenv`, `/run/systemd/container`,
  `/run/host/container-manager`, and the Kubernetes service account mount
  `/var/run/secrets/kubernetes.io`. Second, because a containerd or CRI-O pod
  with no service account token leaves none of those, from `/proc/self/cgroup`:
  on a host every process sits in a named cgroup, while in a container it is at
  the root of a private cgroup namespace (`0::/`, or on pure cgroup v1 every
  hierarchy at `/`, which a systemd host never shows because its
  `name=systemd` membership is named) or under a container scope, a
  cgroupfs-driver `/docker/<id>`, or a `kubepods` slice. A host without systemd that keeps processes in the root
  cgroup is read as a container too, and the cost is a tab left showing.
  Evidence the container *can* see still counts: a mounted docker socket is
  the host's, `/sys/module` and `/proc/xen` describe the host's kernel, and with
  `pid: host` the host's VMs are visible.
- **sysmon is in a private PID namespace** (`unshare --pid`, systemd's
  `PrivatePIDs=`, a container without `pid: host`). The process list then
  holds only the namespace's own processes, so VMs are not observable. This is
  read from `/proc/self/ns/pid`, whose inode is `4026531836` only in the
  initial namespace (the kernel's fixed `PROC_PID_INIT_INO`). `NSpid` in
  `/proc/self/status` cannot answer it: it is relative to the namespace that
  mounted `/proc`, and inside a container that mounted its own it reads as one
  PID. A link that cannot be read counts the same as a nested namespace.
- **`/proc` hides other users' processes.** Mounted with `hidepid` (any value
  but `0`/`off`), `/proc` still lists, but a guest running as `libvirt-qemu`
  is simply not in it. Unless sysmon holds `CAP_SYS_PTRACE` in the initial
  user namespace (one held inside `unshare -U` or a rootless sandbox does not
  reach the host's processes; the namespace is identified by the inode of
  `/proc/self/ns/user`, `PROC_USER_INIT_INO`, not by its UID map, which a child
  namespace can make look like the identity) or is in the mount's `gid=` group, VMs are then
  not observable, and the capability notes say why.
  If the `/proc` mount options (including a missing `/proc/self/mountinfo`, or
  one that lists no procfs on `/proc`),
  the capability mask or the namespace links cannot be read, whether processes are hidden cannot be ruled out, which has
  the same consequence.
- **A listed process could not be identified**, for any reason other than it
  having exited: it may have been a hypervisor, so VMs are not observable. The
  same holds for a hypervisor process whose command line cannot be read: its
  stack counts as running, but the VM list is missing a guest.
- **A container check failed.** If a marker file or `/proc/self/cgroup` cannot
  be read -- and a missing `/proc/self/cgroup`, or one with no parseable
  membership line, counts, since every procfs has one -- whether the filesystem is the host's is unsettled, and binaries are
  not looked for, exactly as inside a container.
- **The workloads cannot be observed**: no cgroup v2 tree, or one the scan could
  not fully read -- a directory it could not list, or a container scope whose
  `cgroup.procs` it could not read -- for containers (`containers_observable` is then false and a
  note names the failure); an unreadable process list for VMs.

### Cost

A scan lists the eight binary directories and stats about a dozen other paths:
roughly a millisecond, most of it listing `/usr/bin`. Collection can run every
500 ms, so the scan is reused for **30 seconds**. That bounds how long a newly
installed but idle runtime goes unnoticed, which is shorter than it takes to
install one and then look at the dashboard.

The decision is refreshed on every snapshot, not only when the virtualization
collector runs. That collector is in the medium tier, every fifth snapshot,
which at a 60-second interval would stretch the 30-second recheck to five
minutes. Refreshing costs a clock read unless the scan is due. Whether `/proc` shows
every process (private PID namespace, hidepid) is not part of that cached
scan: it describes a process list, so it is read with each VM scan and stays
paired with the list it vouches for.

Running workloads are as fresh as the container and VM lists themselves: both
come from the same medium-tier scan, and the refresh re-judges that scan
rather than repeating it. So a container appears in the tab's data and makes
the tab present in the same snapshot; there is no moment when the dashboard
holds a running workload behind a hidden tab. Rescanning workloads on every
snapshot would make the whole collector fast-tier -- about 30 ms of process
walking per tick on a host with a thousand processes -- to learn earlier about
a workload the panel could not show yet anyway.

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
    "vms": [ ... ],
    "container_runtime": { "state": "present", "evidence": [ ... ], "reason": "" },
    "hypervisor": { "state": "absent", "evidence": [], "reason": "" }
  }
}
```

There are two tabs, mirroring the two CLI subcommands. Each exists unless the
node proved it has no runtime or VM stack (see above), and each lists the
evidence it was shown for:

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
