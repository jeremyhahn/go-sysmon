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
  sensors: SensorData;
  virtualization: VirtInfo;
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
  swap_total_bytes: number;
  swap_used_bytes: number;
  swap_free_bytes: number;
  swap_percent: number;
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
  smart_enabled: boolean;
  smart_healthy: boolean;
  smart_attrs: SMARTAttr[];
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
  runtime_api_enabled: boolean;
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
