### Added

- **Fleet capacity for schedulers.** Each node in `GET /api/fleet` and
  `GET /api/summary` now carries absolute capacity beside its percentages:
  `cpu_count`, raw `load_1`/`load_5`/`load_15`, `memory_total_bytes` and
  `memory_available_bytes`, free disk on the container runtime's data root
  (`disk_free_bytes`, `disk_total_bytes`, `disk_free_path`), and its
  containers' running count, CPU in cores and memory from the cgroup tree
  (`containers_observable`). An older peer's missing figures read as unknown,
  never as zero (#239).
- **Headroom at a utilisation target.** The aggregator adds `headroom` to every
  reachable node it lists: the CPU cores and memory free before the node reaches
  `fleet.headroom_target` (default 0.8, range (0, 1], reloadable). The fleet
  roll-up gains a `capacity` block that sums each figure, and the headroom, over
  the nodes that measured it, with its coverage (#239).
- **Free space on the container data root** is measured by every node with
  `statfs`, without the runtime's API: `virtualization.runtime_disk` in the
  snapshot (#239).
