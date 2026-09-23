// Types mirror the Go pkg/types/types.go structs field-for-field.
// JSON keys match the Go json tags exactly.

export interface Snapshot {
  timestamp: string;
  host: HostInfo;
  cpus: CPUInfo[];
  cpu_summary: CPUSummary;
  memory: MemoryInfo;
  disks: DiskInfo[];
  networks: NetworkInfo[];
  load_avg: LoadAverage;
  processes: ProcessSummary;
  gpus: GPUInfo[];
  npus: NPUInfo[];
  sensors: SensorData;
  virtualization: VirtInfo;
  alerts: Alert[];
}

// ---------------------------------------------------------------------------
// Alerts
// ---------------------------------------------------------------------------

export type Severity = "info" | "warning" | "critical";

/**
 * Alert is one firing rule against one resource. The backend evaluates the
 * rules and ships the finished sentence, so the UI never re-derives thresholds.
 */
export interface Alert {
  /** Stable per rule+resource, so "already seen" survives a reconnect. */
  id: string;
  rule: string;
  severity: Severity;
  /** Matches a dashboard tab id, which is what makes an alert row clickable. */
  category: string;
  /** "md2", "nvme0n1", "eno1" — empty when the alert is host-wide. */
  resource: string;
  message: string;
  value: number;
  threshold: number;
  /** RFC3339 timestamp of when the rule started firing. */
  since: string;
}

// ---------------------------------------------------------------------------
// Fleet
// ---------------------------------------------------------------------------

export type NodeStatus = "healthy" | "warning" | "critical" | "unreachable";

/**
 * Whether a node runs the same go-sysmon build as the aggregator serving the
 * view. The server decides this -- it is the only party that knows its own
 * commit -- so the UI states it rather than recomputing it.
 *
 * It is NOT a status. A rollout puts every node in "different" until it
 * restarts, so this is rendered as a fact and never as a fault.
 *
 * "unknown" never means "same": a node whose build predates the field, or that
 * answered nothing at all, has not agreed with anything.
 */
export type BuildMatch = "same" | "different" | "unknown";

/**
 * NameMatch says whether the name the aggregator holds for a node -- the name
 * its history is queried by -- is the name the node writes its history under
 * (#162). "different" is a misconfigured alias; "unknown" (an older node, or
 * one that did not answer) never means "same".
 */
export type NameMatch = "same" | "different" | "unknown";

/** FleetNode is one peer's rolled-up health, as the local node last saw it. */
/**
 * PeerSnapshot is what GET /api/fleet/{name}/snapshot returns: a peer's
 * snapshot together with the freshness of the poll behind it.
 *
 * `snapshot` may be present while `reachable` is false. That is the stale case:
 * the aggregator keeps the last good reading when a peer stops answering, so
 * the page does not blank on a blip. It must be shown as stale rather than
 * folded into the live series, or the charts record measurements that were
 * never taken.
 */
export interface PeerSnapshot {
  snapshot: Snapshot | null;
  reachable: boolean;
  error?: string;
  age_seconds: number;
}

export interface FleetNode {
  name: string;
  self: boolean;
  reachable: boolean;
  error?: string;
  age_seconds: number;
  status: NodeStatus;
  hostname: string;
  /** The release string. It has read the same on every build this project has
   * produced, so it cannot tell two builds apart -- `git_commit` can. */
  version: string;
  /** The node's build identity. Absent when the node did not state it, which
   * an older build does not. */
  git_commit?: string;
  build_date?: string;
  /** Agreement with the aggregator's own build. Absent from a server that
   * predates the field, which reads as "unknown" and shows nothing. */
  build_match?: BuildMatch;
  /** The name the node itself writes its history under, from its summary (or
   * its snapshot, for a peer too old to serve one). Absent when it did not
   * say, which an older build does not. */
  node_name?: string;
  /** Agreement between `name` and `node_name`. Absent from a server that
   * predates the field, which reads as "unknown" and shows nothing. */
  name_match?: NameMatch;
  uptime_seconds: number;
  /** The interval the node's monitor collects at; 0 (or absent) is unknown. */
  collection_interval_ms?: number;
  critical_count: number;
  warning_count: number;
  cpu_percent: number;
  memory_percent: number;
  swap_percent: number;
  load_per_core: number;
  worst_disk_percent: number;
  /** How many arrays and SMART-capable disks the node has. Without these,
   * `raid_degraded: false` and `smart_unhealthy: 0` cannot be told apart from
   * "this host has none to report on". */
  raid_arrays: number;
  /** Arrays that are degraded OR not serving I/O at all. `raid_degraded` is
   * kept as it was, degraded only, for consumers that already read it. */
  raid_unhealthy: number;
  raid_degraded: boolean;
  smart_capable: number;
  smart_unhealthy: number;
  max_temp_c: number;
  /** Which kind of component `max_temp_c` came from: "cpu", "drive", "gpu"
   * or "npu". Absent when the node reports no temperature. */
  max_temp_source?: TempSource;
  power_w: number;
  /** False when the node has no power sensor: its `power_w` of 0 is absence,
   * not a machine drawing nothing. */
  power_reported?: boolean;
  net_rx_rate: number;
  net_tx_rate: number;
  /** False for a node with no physical network device, whose rates are then
   * not measured rather than zero. */
  net_reported?: boolean;
  /** The warning and critical alerts that set the node's status. */
  alerts?: Alert[];
  /** Swap PAGING (#134). `swap_percent` is occupancy and cannot say whether a
   * node is paging now; these can. When `swap_rate_available` is false the
   * rates are unknown, never zero. */
  swap_counters_available?: boolean;
  swap_rate_available?: boolean;
  /** Bytes per second paged in from / out to swap over the node's last
   * collection interval. */
  swap_in_rate?: number;
  swap_out_rate?: number;
  /** How the aggregator read this node: "local", "summary", or "snapshot" for
   * a peer too old to serve a summary. */
  source?: "local" | "summary" | "snapshot";
}

/** The worst value of one metric across the fleet, and who holds it. */
export interface FleetExtreme {
  node: string;
  value: number;
  /** The component kind, where the metric spans several (temperature). */
  kind?: TempSource;
}

/** The kinds of component a node's hottest reading can come from. */
export type TempSource = "cpu" | "drive" | "gpu" | "npu";

/** A total and how many nodes it was summed over, of how many reachable. */
export interface FleetSum {
  value: number;
  nodes: number;
  of: number;
}

export interface FleetNet {
  rx_rate: number;
  tx_rate: number;
  nodes: number;
  of: number;
}

/** Devices (not nodes) unhealthy of total, found on `nodes` of `of`. */
export interface FleetHealth {
  unhealthy: number;
  devices: number;
  nodes: number;
  of: number;
}

export interface NodeAttention {
  name: string;
  status: NodeStatus;
  age_seconds: number;
  reasons: string[];
}

export interface FleetBuild {
  /** Empty: the nodes did not state their build. */
  commit: string;
  nodes: string[];
}

/**
 * FleetRollup is the fleet-level statement (#46): the worst value with the
 * node named, or a sum with its denominator -- never a mean. A figure no
 * reachable node could report is absent, not zero.
 */
export interface FleetRollup {
  nodes: number;
  healthy: number;
  warning: number;
  critical: number;
  unreachable: number;
  longest_absence_seconds: number;
  never_answered: number;
  attention: NodeAttention[];
  reporting: number;
  cpu?: FleetExtreme;
  memory?: FleetExtreme;
  disk?: FleetExtreme;
  load_per_core?: FleetExtreme;
  temperature?: FleetExtreme;
  swap_out?: FleetExtreme;
  swap_unknown: number;
  power?: FleetSum;
  network?: FleetNet;
  smart?: FleetHealth;
  raid?: FleetHealth;
  builds: FleetBuild[];
}

export interface Fleet {
  enabled: boolean;
  nodes: FleetNode[];
  /** Absent from a server that predates the roll-up. */
  rollup?: FleetRollup | null;
}

// ---------------------------------------------------------------------------
// Configuration (read-only view)
// ---------------------------------------------------------------------------

/**
 * Where a setting's effective value came from. The first four are configuration
 * layers in precedence order; "runtime" means the running service was changed
 * after startup — the refresh-rate selector does this — so the value is real but
 * no configuration layer accounts for it.
 */
export type ConfigSource =
  | "default"
  | "config file"
  | "env"
  | "flag"
  | "runtime";

export interface ConfigEntry {
  section: string;
  key: string;
  value: string;
  /**
   * Which layer won for this key: default < config file < env < flag, or
   * "runtime" when the running service was changed after startup.
   */
  source: ConfigSource | string;
}

export interface ConfigView {
  /**
   * Whether a configuration file is in use — "(configured)" or "(not set)" —
   * and deliberately NOT where it is. GET /api/config has no authentication of
   * its own, so the server withholds the path rather than handing out the
   * host's filesystem layout. Peer entries are withheld the same way: the name
   * is served, the URL is not.
   */
  config_file: string;
  entries: ConfigEntry[];
}

export interface GPUInfo {
  index: number;
  name: string;
  uuid: string;
  serial: string;
  driver_version: string;
  vbios_version: string;
  compute_mode: string;
  perf_state: string;
  memory_total_mib: number;
  memory_used_mib: number;
  memory_free_mib: number;
  memory_percent: number;
  gpu_util_percent: number;
  memory_util_percent: number;
  encoder_percent: number;
  decoder_percent: number;
  temperature_gpu: number;
  temperature_memory: number;
  fan_speed_percent: number;
  power_draw_w: number;
  power_limit_w: number;
  power_default_w: number;
  power_max_w: number;
  clock_graphics_mhz: number;
  clock_memory_mhz: number;
  clock_video_mhz: number;
  clock_max_gfx_mhz: number;
  clock_max_mem_mhz: number;
  pci_bus_id: string;
  pcie_gen_current: number;
  pcie_gen_max: number;
  pcie_width_current: number;
  pcie_width_max: number;
  pcie_rx_mbps: number;
  pcie_tx_mbps: number;
  ecc_enabled: boolean;
  ecc_single_bit: number;
  ecc_double_bit: number;
  process_count: number;
}

/**
 * NPUInfo mirrors types.NPUInfo: one compute accelerator from
 * /sys/class/accel/accelN.
 *
 * THE THREE UTILISATION FIELDS DO NOT MEAN THE SAME THING, and the panel must
 * not blur them:
 *
 * - residency_percent is the share of the interval the device was POWERED UP,
 *   derived from runtime-PM counters. It says how much the NPU was awake, not
 *   how hard it worked. It is meaningless when `state` is empty (the device
 *   reports no runtime PM at all), and must never be labelled "usage".
 * - util_percent is genuine compute utilisation, per-client engine time from
 *   DRM usage-stats fdinfo. It is legitimately 0 when the driver does not
 *   implement fdinfo, or when this process may not read the owning task's
 *   fdinfo -- a plain zero, not an error.
 * - busy_time_us is the driver's RAW cumulative counter, deliberately not a
 *   percentage: on intel_vpu it tracks D0 residency rather than job execution,
 *   so any percent derived from it sits near 100% whenever anything keeps the
 *   device awake. Show it as a counter or not at all.
 */
export interface NPUInfo {
  index: number;
  device: string;
  name: string;
  driver: string;
  vendor: string;
  vendor_id: string;
  device_id: string;
  device_class: string;
  label: string;
  pci_bus_id: string;
  numa_node: number;
  local_cpu_list: string;
  irq: number;
  /** "active", "suspended", "unsupported", or "" for no runtime PM at all. */
  state: string;
  power_state: string;
  /** Time awake, not work done. Meaningless when state is "". */
  residency_percent: number;
  active_time_ms: number;
  suspended_time_ms: number;
  autosuspend_delay_ms: number;
  runtime_usage: number;
  /** Real compute utilisation from DRM fdinfo; 0 is a valid reading. */
  util_percent: number;
  engines: NPUEngine[];
  clients: NPUClient[];
  client_count: number;
  /**
   * Driver counters below are not re-read while the device sleeps, because
   * reading them resumes it. counters_stale marks the carried-forward values
   * and counters_age_sec says how old they are. Normal idle behaviour, not an
   * error.
   */
  counters_stale: boolean;
  counters_age_sec: number;
  /** Raw cumulative driver counter in microseconds. Never a percentage. */
  busy_time_us: number;
  /** The driver's own counter of this name; not "memory in use". */
  memory_utilization_bytes: number;
  sched_mode: string;
  frequency_mhz: number;
  frequency_max_mhz: number;
  governor: string;
  temperature_c: number;
  power_draw_w: number;
  pcie_gen_current: number;
  pcie_gen_max: number;
  pcie_width_current: number;
  pcie_width_max: number;
}

/** NPUEngine is one hardware engine's busy time across every client. */
export interface NPUEngine {
  name: string;
  busy_ns: number;
  /** Genuine engine utilisation from fdinfo. */
  busy_percent: number;
}

/** NPUClient is one process holding the accelerator open. */
export interface NPUClient {
  pid: number;
  name: string;
  client_id: string;
  busy_ns: number;
  /** Genuine per-client utilisation from fdinfo. */
  busy_percent: number;
  memory_bytes: number;
}

export interface CPUSummary {
  sockets: number;
  cores_per_socket: number;
  threads_per_core: number;
  total_cores: number;
  total_threads: number;
  max_mhz: number;
  min_mhz: number;
}

export interface HostInfo {
  hostname: string;
  os: string;
  platform: string;
  platform_version: string;
  kernel_version: string;
  kernel_arch: string;
  uptime: number;
  boot_time: number;
  board_vendor: string;
  board_name: string;
  board_version: string;
  bios_vendor: string;
  bios_version: string;
  bios_date: string;
}

export interface CPUInfo {
  index: number;
  model_name: string;
  vendor_id: string;
  family: string;
  model: string;
  stepping: number;
  physical_id: string;
  core_id: string;
  cores: number;
  threads: number;
  mhz: number;
  cache_size: number;
  microcode: string;
  flags: string[];
  usage_percent: number;
  temperature_celsius: number;
  voltage_v: number;
}

export interface MemoryInfo {
  total_bytes: number;
  used_bytes: number;
  available_bytes: number;
  free_bytes: number;
  used_percent: number;
  buffers_bytes: number;
  cached_bytes: number;
  shared_bytes: number;
  slab_bytes: number;
  // Swap occupancy: how much swap holds pages.
  swap_total_bytes: number;
  swap_used_bytes: number;
  swap_free_bytes: number;
  swap_percent: number;
  // Swap paging counters: how much swapping is happening. Cumulative pages
  // since boot, so a rate is a difference between two snapshots. Occupancy
  // above says nothing about this — a host paging out and back in at the same
  // rate has flat swap_used_bytes while thrashing hard.
  swap_in_pages: number;
  swap_out_pages: number;
  page_size_bytes: number;
  // False when /proc/vmstat could not be read, or carried neither counter.
  // The counts are zero then, and zero is also what a host that has never
  // paged reports.
  swap_counters_available: boolean;
  dimms: DIMMInfo[];
  temp_sensor_detected: boolean;
}

export interface DIMMInfo {
  location: string;
  bank_locator: string;
  manufacturer: string;
  part_number: string;
  serial_number: string;
  size_bytes: number;
  speed_mts: number;
  configured_speed_mts: number;
  type: string;
  form_factor: string;
  data_width_bits: number;
  total_width_bits: number;
  rank: number;
  min_voltage: number;
  max_voltage: number;
  configured_voltage: number;
  temperature: number;
}

/**
 * SMARTStatus mirrors types.SMARTStatus in pkg/types.
 *
 * `""` is the zero value and means nothing classified this device — an older
 * server that predates the field sends no value at all. It must render as the
 * undifferentiated "N/A" this replaced, never as a reassuring one: a tab left
 * open across a rollback holds this bundle against a server that cannot answer.
 */
export type SMARTStatus =
  | ""
  | "not_applicable"
  | "behind_controller"
  | "unsupported"
  | "unreadable"
  | "ok"
  | "failing";

/**
 * DiskMember is one physical drive behind a hardware RAID controller.
 *
 * It is not a block device: no device node, no partitions, no I/O counters.
 * The controller presents the volume, and these are what it is made of.
 */
export interface DiskMember {
  /** The only identity that survives a reboot; device_id and slot describe the bay. */
  serial: string;
  model: string;
  vendor: string;
  device_id: number;
  enclosure: number;
  slot: number;
  temperature_celsius: number;
  smart_enabled: boolean;
  smart_healthy: boolean;
  smart_status?: SMARTStatus;
  smart_reason?: string;
}

export interface DiskInfo {
  name: string;
  model: string;
  serial: string;
  vendor: string;
  size_bytes: number;
  drive_type: string;
  controller: string;
  transport: string;
  rotational: boolean;
  partitions: PartitionInfo[];
  total_bytes: number;
  used_bytes: number;
  free_bytes: number;
  used_percent: number;
  read_count: number;
  write_count: number;
  read_bytes: number;
  write_bytes: number;
  io_time_ms: number;
  weighted_io_ms: number;
  read_bytes_rate: number;
  write_bytes_rate: number;
  queue_length: number;
  avg_queue_length: number;
  util_percent: number;
  peak_read_bytes_rate: number;
  peak_write_bytes_rate: number;
  peak_queue_length: number;
  peak_util_percent: number;
  peak_used_percent: number;
  smart_enabled: boolean;
  smart_healthy: boolean;
  /**
   * Which of several different things "there is no SMART reading" means.
   * Absent from servers older than this field, which is why every consumer
   * treats the empty string as "no claim" rather than as a state.
   */
  smart_status?: SMARTStatus;
  /** Why there is no reading, for the statuses that have a why. */
  smart_reason?: string;
  smart_attrs: SMARTAttr[];
  /**
   * The physical drives behind a hardware RAID controller, for a device that
   * is a virtual disk. Absent from servers older than this field, and empty on
   * everything that is not a controller volume.
   */
  members?: DiskMember[];
  raid?: RAIDInfo | null;
  temperature_celsius: number;
  power_on_hours: number;
  power_cycles: number;
  wear_level_percent: number;
  available_spare_percent: number;
  spare_threshold_percent: number;
  media_errors: number;
  error_log_entries: number;
  unsafe_shutdowns: number;
  critical_warning: number;
  data_units_read: number;
  data_units_written: number;
  warning_temp_time_minutes: number;
  critical_temp_time_minutes: number;
  life_remaining_percent: number;
  estimated_hours_left: number;
  firmware_version: string;
  nvme_version: string;
}

export interface PartitionInfo {
  device: string;
  mountpoint: string;
  fstype: string;
  opts: string;
}

export interface RAIDMember {
  name: string;
  state: string;
  slot: number;
  errors: number;
}

export interface RAIDInfo {
  level: string;
  state: string;
  degraded: boolean;
  total_devices: number;
  active_devices: number;
  failed_devices: number;
  spare_devices: number;
  sync_action: string;
  sync_progress_percent: number;
  mismatch_count: number;
  members: RAIDMember[];
}

export interface SMARTAttr {
  id: number;
  name: string;
  value: number;
  worst: number;
  threshold: number;
  raw_value: number;
  type: string;
  failing: boolean;
}

export interface NetworkInfo {
  name: string;
  hardware_addr: string;
  addresses: string[];
  mtu: number;
  flags: string[];
  speed_mbps: number;
  duplex: string;
  driver: string;
  is_up: boolean;
  is_loopback: boolean;
  is_virtual: boolean;
  kind: string;
  oper_state: string;
  bond_master: string;
  bond_mode: string;
  bond_slaves: string[] | null;
  bridge_ports: string[] | null;
  dns_servers: string[] | null;
  dns_search: string[] | null;
  bytes_sent: number;
  bytes_recv: number;
  packets_sent: number;
  packets_recv: number;
  errors_in: number;
  errors_out: number;
  drops_in: number;
  drops_out: number;
  bytes_sent_rate: number;
  bytes_recv_rate: number;
  /** False when the rates were not measured (first collection, a new
   * interface, unreadable counters); they are then zero, not idle. */
  rates_available?: boolean;
}

export interface LoadAverage {
  load1: number;
  load5: number;
  load15: number;
}

export interface ProcessDetail {
  pid: number;
  name: string;
  username: string;
  cpu_percent: number;
  memory_bytes: number;
  read_bytes: number;
  write_bytes: number;
  read_bytes_rate: number;
  write_bytes_rate: number;
  priority: number;
  status: string;
}

export interface ProcessSummary {
  total: number;
  running: number;
  sleeping: number;
  idle: number;
  stopped: number;
  zombie: number;
  process_list: ProcessDetail[];
}

export interface CoreTemp {
  package_id: number;
  core_id: number;
  label: string;
  temp_celsius: number;
  high_celsius: number;
  crit_celsius: number;
}

export interface CoreVoltage {
  label: string;
  voltage_v: number;
  channel: number;
  hwmon_name: string;
}

export interface PackagePower {
  package_name: string;
  power_w: number;
  max_power_w: number;
  energy_joules: number;
}

export interface ThrottleInfo {
  cpu: number;
  core_throttle_count: number;
  package_throttle_count: number;
}

export interface ThermalZone {
  name: string;
  type: string;
  temp_celsius: number;
  policy: string;
}

export interface FanInfo {
  label: string;
  rpm: number;
  min_rpm: number;
  max_rpm: number;
  hwmon_name: string;
}

export interface PSIResource {
  // False when this resource's file under /proc/pressure could not be read.
  // The averages below are zero in that case and are not a reading.
  available: boolean;
  some_avg10: number;
  some_avg60: number;
  some_avg300: number;
  some_total: number;
  full_avg10: number;
  full_avg60: number;
  full_avg300: number;
  full_total: number;
}

export interface PSIData {
  // False on kernels built without CONFIG_PSI and in containers that do not
  // expose /proc/pressure. Every number below is zero there, identical to a
  // completely idle machine, so the panel must gate on this rather than on the
  // values.
  available: boolean;
  cpu: PSIResource;
  memory: PSIResource;
  io: PSIResource;
}

export interface SensorData {
  core_temps: CoreTemp[];
  core_voltages: CoreVoltage[];
  package_power: PackagePower[];
  thermal_throttle: ThrottleInfo[];
  thermal_zones: ThermalZone[];
  fans: FanInfo[];
  psi: PSIData;
}

// Augment Window for Wails v2 runtime injection.
declare global {
  interface Window {
    runtime?: {
      EventsOn: (event: string, callback: (data: unknown) => void) => void;
      EventsOff: (event: string) => void;
    };
    go?: {
      main: {
        MonitorBinding: {
          GetSnapshot: () => Promise<Snapshot>;
          SetInterval: (ms: number) => Promise<void>;
          // Restarts the frame stream, so the next frame is a whole snapshot.
          // The desktop's equivalent of reopening the socket (#98).
          Resync: () => Promise<void>;
        };
      };
    };
  }
}

export interface ContainerInfo {
  index: number;
  id: string;
  short_id: string;
  name: string;
  runtime: string;
  command: string;
  cgroup_path: string;
  cpu_percent: number;
  cpu_usage_usec: number;
  memory_bytes: number;
  memory_limit_bytes: number;
  memory_percent: number;
  pids: number;
  process_count: number;
  read_bytes: number;
  write_bytes: number;
  read_bytes_rate: number;
  write_bytes_rate: number;
  read_iops: number;
  write_iops: number;
  cpu_limit_cores: number;
  cpu_set: string;
  nr_periods: number;
  nr_throttled: number;
  throttled_usec: number;
  throttled_percent: number;
  memory_peak_bytes: number;
  swap_bytes: number;
  anon_bytes: number;
  file_bytes: number;
  major_faults: number;
  oom_kills: number;
  oom_events: number;
  pids_max: number;
  pids_peak: number;
  cpu_pressure: number;
  memory_pressure: number;
  io_pressure: number;
  uptime_seconds: number;
}

export interface VMInfo {
  index: number;
  name: string;
  uuid: string;
  hypervisor: string;
  accelerator: string;
  pid: number;
  vcpus: number;
  memory_bytes: number;
  rss_bytes: number;
  cpu_percent: number;
  mac_addresses: string[] | null;
  tap_interfaces: string[] | null;
  disk_images: string[] | null;
  vcpu_threads: number;
  thread_count: number;
  uptime_seconds: number;
  net_rx_rate: number;
  net_tx_rate: number;
  disk_image_bytes: number;
  cgroup_path: string;
  disk_read_bytes: number;
  disk_write_bytes: number;
  disk_read_rate: number;
  disk_write_rate: number;
  disk_read_iops: number;
  disk_write_iops: number;
  memory_current_bytes: number;
  memory_peak_bytes: number;
  cpu_pressure: number;
  io_pressure: number;
}

export interface ImageInfo {
  index: number;
  id: string;
  short_id: string;
  tags: string[] | null;
  size_bytes: number;
  shared_size_bytes: number;
  containers: number;
  created_unix: number;
  in_use: boolean;
  dangling: boolean;
}

export interface RuntimeInfo {
  available: boolean;
  engine: string;
  version: string;
  socket_path: string;
  root_dir: string;
  storage_driver: string;
  backing_filesystem: string;
  images_total: number;
  containers_total: number;
  containers_running: number;
  containers_stopped: number;
  containers_paused: number;
  layers_bytes: number;
  reclaimable_bytes: number;
  dangling_images: number;
  unused_images: number;
  volumes_count: number;
  volumes_unused: number;
  volumes_bytes: number;
  volumes_reclaimable_bytes: number;
  build_cache_entries: number;
  build_cache_bytes: number;
  build_cache_reclaimable_bytes: number;
  images: ImageInfo[] | null;
  images_truncated: number;
}

export interface VirtCapability {
  containers_observable: boolean;
  vms_observable: boolean;
  runtime_api_reachable: boolean;
  notes: string[] | null;
}

export interface VirtInfo {
  containers: ContainerInfo[] | null;
  vms: VMInfo[] | null;
  cgroup_version: string;
  runtimes: string[] | null;
  runtime: RuntimeInfo;
  capability: VirtCapability;
}

// ---------------------------------------------------------------------------
// History (Prometheus-backed date ranges)
// ---------------------------------------------------------------------------

/** How a history series' values are formatted. */
export type HistoryUnit =
  | "percent"
  | "bytes_per_second"
  | "celsius"
  | "watts"
  | "count";

/** HistoryMetric is one series the history API can draw. */
export interface HistoryMetric {
  key: string;
  label: string;
  unit: HistoryUnit;
  /** The label that separates one line from another — device, interface, gpu.
   * Empty for a metric a node yields only one of. */
  split_by: string;
}

/**
 * HistoryCatalog is what history is available.
 *
 * `enabled: false` is a node with no Prometheus configured, which is a
 * standalone node rather than a broken one: the date picker is hidden and the
 * dashboard streams as it always has.
 */
export interface HistoryCatalog {
  enabled: boolean;
  metrics: HistoryMetric[];
}

export interface HistoryPoint {
  t: number;
  v: number;
}

export interface HistorySeries {
  name: string;
  points: HistoryPoint[];
}

/**
 * History is one metric over one date range.
 *
 * step_ms comes back because the server may have chosen it: a range wide enough
 * needs a coarser resolution to stay drawable, and a chart that does not know
 * the step cannot tell a gap in the data from the space between samples.
 */
export interface History {
  enabled: boolean;
  node: string;
  metric: string;
  label: string;
  unit: HistoryUnit;
  start_ms: number;
  end_ms: number;
  step_ms: number;
  series: HistorySeries[];
}
