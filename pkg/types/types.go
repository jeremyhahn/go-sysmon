// Package types defines shared data structures for system monitoring.
package types

import "time"

// Snapshot contains a complete system state at a point in time.
type Snapshot struct {
	Timestamp  time.Time      `json:"timestamp"`
	Host       HostInfo       `json:"host"`
	CPUSummary CPUSummary     `json:"cpu_summary"`
	CPUs       []CPUInfo      `json:"cpus"`
	GPUs       []GPUInfo      `json:"gpus"`
	Memory     MemoryInfo     `json:"memory"`
	Disks      []DiskInfo     `json:"disks"`
	Networks   []NetworkInfo  `json:"networks"`
	LoadAvg    LoadAverage    `json:"load_avg"`
	Processes  ProcessSummary `json:"processes"`
	Sensors    SensorData     `json:"sensors"`
	Virt       VirtInfo       `json:"virtualization"`
}

// VirtInfo describes the containerised and virtualised workloads running on
// the host. Both are observed from the host side only: container metrics come
// from cgroup accounting and VM metrics from the hypervisor process, so no
// runtime daemon socket or in-guest agent is required.
type VirtInfo struct {
	Containers []ContainerInfo `json:"containers"`
	VMs        []VMInfo        `json:"vms"`
	// CgroupVersion is "v2", "v1" or "" when no cgroup filesystem was found.
	CgroupVersion string `json:"cgroup_version"`
	// Runtimes lists the container runtimes detected from cgroup naming.
	Runtimes []string `json:"runtimes"`
	// Runtime carries image inventory and storage usage from the container
	// runtime's API. It is only populated when the runtime collector is
	// explicitly enabled, because that API socket is root-equivalent access.
	Runtime RuntimeInfo `json:"runtime"`

	// Capability explains an empty result. Without it a host with nothing
	// running and a host we cannot observe look identical: both render as
	// zeroes, and the reader cannot tell which they are looking at.
	Capability VirtCapability `json:"capability"`
}

// VirtCapability records whether each source of virtualization data could be
// consulted, so an empty section can say why it is empty.
type VirtCapability struct {
	// ContainersObservable is true when the cgroup tree could be read. False
	// means container metrics are impossible here, not that none are running.
	ContainersObservable bool `json:"containers_observable"`
	// VMsObservable is true when the process list could be read.
	VMsObservable bool `json:"vms_observable"`
	// RuntimeAPIReachable reports whether a container runtime socket answered.
	// There is nothing to opt into: the socket is used whenever one can be
	// opened, so unreachable means absent, not switched off.
	RuntimeAPIReachable bool `json:"runtime_api_reachable"`
	// Notes are human-readable explanations for anything unavailable.
	Notes []string `json:"notes"`
}

// RuntimeInfo describes a container runtime's engine, storage layout and disk
// consumption. Available is false when no runtime was reachable.
type RuntimeInfo struct {
	Available  bool   `json:"available"`
	Engine     string `json:"engine"`
	Version    string `json:"version"`
	SocketPath string `json:"socket_path"`
	// RootDir is where the runtime stores images, layers and volumes. It is
	// frequently relocated away from the default, which makes it the first
	// thing to check when a filesystem fills up.
	RootDir           string `json:"root_dir"`
	StorageDriver     string `json:"storage_driver"`
	BackingFilesystem string `json:"backing_filesystem"`

	ImagesTotal       int `json:"images_total"`
	ContainersTotal   int `json:"containers_total"`
	ContainersRunning int `json:"containers_running"`
	ContainersStopped int `json:"containers_stopped"`
	ContainersPaused  int `json:"containers_paused"`

	// LayersBytes is the total size of all image layers on disk.
	LayersBytes uint64 `json:"layers_bytes"`
	// ReclaimableBytes is what pruning unused images would free.
	ReclaimableBytes uint64 `json:"reclaimable_bytes"`
	DanglingImages   int    `json:"dangling_images"`
	UnusedImages     int    `json:"unused_images"`

	VolumesCount            int    `json:"volumes_count"`
	VolumesUnused           int    `json:"volumes_unused"`
	VolumesBytes            uint64 `json:"volumes_bytes"`
	VolumesReclaimableBytes uint64 `json:"volumes_reclaimable_bytes"`

	BuildCacheEntries          int    `json:"build_cache_entries"`
	BuildCacheBytes            uint64 `json:"build_cache_bytes"`
	BuildCacheReclaimableBytes uint64 `json:"build_cache_reclaimable_bytes"`

	// Images is capped to the largest entries; ImagesTruncated counts those
	// omitted. The aggregate figures above always cover every image.
	Images          []ImageInfo `json:"images"`
	ImagesTruncated int         `json:"images_truncated"`
	// ImagesFiltered counts images removed by CLI filter or limit flags, so a
	// shortened list is not mistaken for the whole inventory.
	ImagesFiltered int `json:"images_filtered,omitempty"`
}

// ImageInfo describes a single container image on disk.
type ImageInfo struct {
	Index   int      `json:"index"`
	ID      string   `json:"id"`
	ShortID string   `json:"short_id"`
	Tags    []string `json:"tags"`
	// SizeBytes is the image's total size; SharedSizeBytes is the portion in
	// layers shared with other images, so summing SizeBytes double-counts.
	SizeBytes       uint64 `json:"size_bytes"`
	SharedSizeBytes uint64 `json:"shared_size_bytes"`
	// Containers is how many containers reference this image; 0 means it can
	// be pruned.
	Containers  int   `json:"containers"`
	CreatedUnix int64 `json:"created_unix"`
	InUse       bool  `json:"in_use"`
	Dangling    bool  `json:"dangling"`
}

// ContainerInfo describes a single running container, derived from its cgroup.
type ContainerInfo struct {
	Index int `json:"index"`
	// ID is the full container ID; ShortID is the 12-character form shown by
	// container tooling.
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	// Name is the main process name. Human-friendly names live in the runtime
	// daemon's database, which would require a socket connection to read, so
	// the process name is used instead.
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Command string `json:"command"`
	// CgroupPath is the absolute path this data was read from.
	CgroupPath string `json:"cgroup_path"`
	// CPUPercent is percent of a single core, so a container using two cores
	// fully reports 200 — the same convention as "docker stats".
	CPUPercent   float64 `json:"cpu_percent"`
	CPUUsageUsec uint64  `json:"cpu_usage_usec"`
	// MemoryLimitBytes is 0 when the cgroup is unlimited ("max").
	MemoryBytes      uint64  `json:"memory_bytes"`
	MemoryLimitBytes uint64  `json:"memory_limit_bytes"`
	MemoryPercent    float64 `json:"memory_percent"`
	PIDs             uint64  `json:"pids"`
	ProcessCount     int     `json:"process_count"`
	ReadBytes        uint64  `json:"read_bytes"`
	WriteBytes       uint64  `json:"write_bytes"`
	ReadBytesRate    uint64  `json:"read_bytes_rate"`
	WriteBytesRate   uint64  `json:"write_bytes_rate"`
	ReadIOPS         uint64  `json:"read_iops"`
	WriteIOPS        uint64  `json:"write_iops"`

	// CPULimitCores is the quota from cpu.max expressed in cores (1.5 means
	// one and a half cores). 0 means unlimited.
	CPULimitCores float64 `json:"cpu_limit_cores"`
	// CPUSet is the effective CPU affinity mask, e.g. "0-23".
	CPUSet string `json:"cpu_set"`

	// Throttling is the clearest signal that a container is CPU-capped: the
	// kernel counts each scheduling period in which it ran out of quota and
	// was forcibly stopped. A container can sit well below its limit on
	// average and still be throttled in bursts.
	NrPeriods        uint64  `json:"nr_periods"`
	NrThrottled      uint64  `json:"nr_throttled"`
	ThrottledUsec    uint64  `json:"throttled_usec"`
	ThrottledPercent float64 `json:"throttled_percent"`

	// MemoryPeakBytes is the high-water mark since the container started,
	// which is what a memory limit must actually accommodate.
	MemoryPeakBytes uint64 `json:"memory_peak_bytes"`
	SwapBytes       uint64 `json:"swap_bytes"`
	// AnonBytes is process memory; FileBytes is reclaimable page cache. A
	// container near its limit on file memory is usually fine; on anonymous
	// memory it is a candidate for the OOM killer.
	AnonBytes uint64 `json:"anon_bytes"`
	FileBytes uint64 `json:"file_bytes"`
	// MajorFaults counts page faults served from disk. A climbing value means
	// the workload is thrashing rather than running.
	MajorFaults uint64 `json:"major_faults"`

	// OOMKills counts processes killed in this container by the OOM killer.
	// Non-zero explains an unexplained restart.
	OOMKills  uint64 `json:"oom_kills"`
	OOMEvents uint64 `json:"oom_events"`

	// PIDsMax is the fork limit (0 when unlimited); PIDsPeak is the highest
	// count reached, which reveals a leak that current usage hides.
	PIDsMax  uint64 `json:"pids_max"`
	PIDsPeak uint64 `json:"pids_peak"`

	// Pressure Stall Information: the share of the last 10 seconds in which
	// work was delayed waiting on each resource. Non-zero CPU pressure with
	// low CPU usage means the container is waiting for runtime it cannot get.
	CPUPressure    float64 `json:"cpu_pressure"`
	MemoryPressure float64 `json:"memory_pressure"`
	IOPressure     float64 `json:"io_pressure"`

	// UptimeSeconds is how long the container's main process has been running.
	UptimeSeconds uint64 `json:"uptime_seconds"`
}

// VMInfo describes a virtual machine observed from the host, by inspecting the
// hypervisor process. Guest-internal metrics would need a guest agent and are
// deliberately out of scope.
type VMInfo struct {
	Index int `json:"index"`
	// Name is the guest name from the hypervisor command line, falling back to
	// the process name.
	Name       string `json:"name"`
	UUID       string `json:"uuid"`
	Hypervisor string `json:"hypervisor"`
	// Accelerator is "kvm" for hardware-accelerated guests, "tcg" for pure
	// emulation, or empty when it could not be determined.
	Accelerator string `json:"accelerator"`
	PID         int32  `json:"pid"`
	VCPUs       int    `json:"vcpus"`
	// MemoryBytes is the guest's configured RAM; RSSBytes is what the
	// hypervisor process actually occupies on the host, which is normally
	// lower because guest RAM is faulted in lazily.
	MemoryBytes uint64  `json:"memory_bytes"`
	RSSBytes    uint64  `json:"rss_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`
	// MACAddresses and TapInterfaces link the guest to host networking.
	MACAddresses  []string `json:"mac_addresses"`
	TapInterfaces []string `json:"tap_interfaces"`
	DiskImages    []string `json:"disk_images"`

	// VCPUThreads is the number of vCPU threads the hypervisor actually
	// created. A mismatch with VCPUs means the guest is still starting or the
	// topology was changed at runtime.
	VCPUThreads int `json:"vcpu_threads"`
	// ThreadCount is every thread in the hypervisor process, including I/O
	// and emulation workers.
	ThreadCount int `json:"thread_count"`
	// UptimeSeconds is how long the hypervisor process has been running.
	UptimeSeconds uint64 `json:"uptime_seconds"`

	// Network throughput across the guest's host-side tap devices, summed.
	// This is the guest's real network usage measured from the host.
	NetRxRate uint64 `json:"net_rx_rate"`
	NetTxRate uint64 `json:"net_tx_rate"`

	// DiskImageBytes is the total on-disk size of the guest's image files,
	// 0 when they are not readable by this user.
	DiskImageBytes uint64 `json:"disk_image_bytes"`

	// CgroupPath is the libvirt machine scope backing this guest, when one
	// exists. libvirt places each VM in its own cgroup, which accounts for the
	// guest's real block I/O, memory and CPU without needing to read the
	// hypervisor process (whose /proc/<pid>/io is owner-only).
	CgroupPath string `json:"cgroup_path"`
	// Disk I/O attributed to the guest by its cgroup.
	DiskReadBytes  uint64 `json:"disk_read_bytes"`
	DiskWriteBytes uint64 `json:"disk_write_bytes"`
	DiskReadRate   uint64 `json:"disk_read_rate"`
	DiskWriteRate  uint64 `json:"disk_write_rate"`
	DiskReadIOPS   uint64 `json:"disk_read_iops"`
	DiskWriteIOPS  uint64 `json:"disk_write_iops"`
	// MemoryCurrentBytes is the cgroup's charge for the guest, which includes
	// host page cache for its disk images and so exceeds the process RSS.
	MemoryCurrentBytes uint64 `json:"memory_current_bytes"`
	MemoryPeakBytes    uint64 `json:"memory_peak_bytes"`
	// Stall pressure for the guest's cgroup.
	CPUPressure float64 `json:"cpu_pressure"`
	IOPressure  float64 `json:"io_pressure"`
}

// HostInfo contains general host information.
type HostInfo struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelVersion   string `json:"kernel_version"`
	KernelArch      string `json:"kernel_arch"`
	Uptime          uint64 `json:"uptime"`
	BootTime        uint64 `json:"boot_time"`
	BoardVendor     string `json:"board_vendor"`
	BoardName       string `json:"board_name"`
	BoardVersion    string `json:"board_version"`
	BIOSVendor      string `json:"bios_vendor"`
	BIOSVersion     string `json:"bios_version"`
	BIOSDate        string `json:"bios_date"`
}

// CPUSummary provides aggregate CPU topology information.
type CPUSummary struct {
	Sockets        int     `json:"sockets"`
	CoresPerSocket int     `json:"cores_per_socket"`
	ThreadsPerCore int     `json:"threads_per_core"`
	TotalCores     int     `json:"total_cores"`
	TotalThreads   int     `json:"total_threads"`
	MaxMHz         float64 `json:"max_mhz"`
	MinMHz         float64 `json:"min_mhz"`
}

// CPUInfo contains detailed information about a CPU.
type CPUInfo struct {
	Index              int      `json:"index"`
	ModelName          string   `json:"model_name"`
	VendorID           string   `json:"vendor_id"`
	Family             string   `json:"family"`
	Model              string   `json:"model"`
	Stepping           int32    `json:"stepping"`
	PhysicalID         string   `json:"physical_id"`
	CoreID             string   `json:"core_id"`
	Cores              int32    `json:"cores"`
	Threads            int32    `json:"threads"`
	Mhz                float64  `json:"mhz"`
	CacheSize          int32    `json:"cache_size"`
	Microcode          string   `json:"microcode"`
	Flags              []string `json:"flags"`
	UsagePercent       float64  `json:"usage_percent"`
	TemperatureCelsius float64  `json:"temperature_celsius"`
	VoltageV           float64  `json:"voltage_v"`
}

// MemoryInfo contains detailed memory information.
type MemoryInfo struct {
	// Physical RAM
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	UsedPercent    float64 `json:"used_percent"`
	// Buffers/Cache (Linux)
	BuffersBytes uint64 `json:"buffers_bytes"`
	CachedBytes  uint64 `json:"cached_bytes"`
	SharedBytes  uint64 `json:"shared_bytes"`
	SlabBytes    uint64 `json:"slab_bytes"`
	// Swap
	SwapTotalBytes uint64  `json:"swap_total_bytes"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes"`
	SwapFreeBytes  uint64  `json:"swap_free_bytes"`
	SwapPercent    float64 `json:"swap_percent"`
	// Hardware info
	DIMMs              []DIMMInfo `json:"dimms"`
	TempSensorDetected bool       `json:"temp_sensor_detected"`
}

// DIMMInfo describes a physical memory module.
type DIMMInfo struct {
	Index              int     `json:"index"`
	Location           string  `json:"location"`
	BankLocator        string  `json:"bank_locator"`
	Manufacturer       string  `json:"manufacturer"`
	PartNumber         string  `json:"part_number"`
	SerialNumber       string  `json:"serial_number"`
	SizeBytes          uint64  `json:"size_bytes"`
	SpeedMTs           uint32  `json:"speed_mts"`
	ConfiguredSpeedMTs uint32  `json:"configured_speed_mts"`
	Type               string  `json:"type"`
	FormFactor         string  `json:"form_factor"`
	DataWidthBits      uint32  `json:"data_width_bits"`
	TotalWidthBits     uint32  `json:"total_width_bits"`
	Rank               uint32  `json:"rank"`
	MinVoltage         float64 `json:"min_voltage"`
	MaxVoltage         float64 `json:"max_voltage"`
	ConfiguredVoltage  float64 `json:"configured_voltage"`
	Temperature        float64 `json:"temperature"`
}

// DiskInfo contains detailed disk information.
type DiskInfo struct {
	// Identity
	Index      int    `json:"index"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	Vendor     string `json:"vendor"`
	SizeBytes  uint64 `json:"size_bytes"`
	DriveType  string `json:"drive_type"`
	Controller string `json:"controller"`
	Transport  string `json:"transport"`
	Rotational bool   `json:"rotational"`
	// Partitions
	Partitions []PartitionInfo `json:"partitions"`
	// Usage across all mount points
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
	// I/O counters
	ReadCount  uint64 `json:"read_count"`
	WriteCount uint64 `json:"write_count"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
	IoTime     uint64 `json:"io_time_ms"`
	WeightedIo uint64 `json:"weighted_io_ms"`
	// I/O rates in bytes per second, computed between snapshots.
	ReadBytesRate  uint64 `json:"read_bytes_rate"`
	WriteBytesRate uint64 `json:"write_bytes_rate"`
	// Queue depth. QueueLength is the instantaneous count of in-flight
	// requests from /sys/block/<dev>/inflight. AvgQueueLength is derived from
	// the weighted I/O time delta and corresponds to iostat's avgqu-sz.
	QueueLength    uint64  `json:"queue_length"`
	AvgQueueLength float64 `json:"avg_queue_length"`
	// UtilPercent is the share of wall-clock time the device spent servicing
	// I/O, derived from the io_time delta. This is iostat's %util.
	UtilPercent float64 `json:"util_percent"`
	// Peaks observed since the process started, for spotting bursts that a
	// point-in-time reading would miss.
	PeakReadBytesRate  uint64  `json:"peak_read_bytes_rate"`
	PeakWriteBytesRate uint64  `json:"peak_write_bytes_rate"`
	PeakQueueLength    uint64  `json:"peak_queue_length"`
	PeakUtilPercent    float64 `json:"peak_util_percent"`
	PeakUsedPercent    float64 `json:"peak_used_percent"`
	// SMART status
	SMARTEnabled bool        `json:"smart_enabled"`
	SMARTHealthy bool        `json:"smart_healthy"`
	SMARTAttrs   []SMARTAttr `json:"smart_attrs"`
	Temperature  float64     `json:"temperature_celsius"`
	PowerOnHours uint64      `json:"power_on_hours"`
	PowerCycles  uint64      `json:"power_cycles"`
	// NVMe health metrics
	WearLevelPercent      int    `json:"wear_level_percent"`
	AvailableSparePercent int    `json:"available_spare_percent"`
	SpareThresholdPercent int    `json:"spare_threshold_percent"`
	MediaErrors           uint64 `json:"media_errors"`
	ErrorLogEntries       uint64 `json:"error_log_entries"`
	UnsafeShutdowns       uint64 `json:"unsafe_shutdowns"`
	CriticalWarning       int    `json:"critical_warning"`
	DataUnitsRead         uint64 `json:"data_units_read"`
	DataUnitsWritten      uint64 `json:"data_units_written"`
	WarningTempTime       uint64 `json:"warning_temp_time_minutes"`
	CriticalTempTime      uint64 `json:"critical_temp_time_minutes"`
	// Estimated life remaining (100 - wear_level_percent)
	LifeRemainingPercent int `json:"life_remaining_percent"`
	// Estimated hours remaining based on wear rate
	EstimatedHoursLeft uint64 `json:"estimated_hours_left"`
	// Firmware
	FirmwareVersion string `json:"firmware_version"`
	// NVMe protocol version
	NVMeVersion string `json:"nvme_version"`
}

// PartitionInfo describes a disk partition.
type PartitionInfo struct {
	Device     string `json:"device"`
	Mountpoint string `json:"mountpoint"`
	Fstype     string `json:"fstype"`
	Opts       string `json:"opts"`
}

// SMARTAttr is a single SMART attribute.
type SMARTAttr struct {
	ID        uint8  `json:"id"`
	Name      string `json:"name"`
	Value     int64  `json:"value"`
	Worst     int64  `json:"worst"`
	Threshold int64  `json:"threshold"`
	RawValue  int64  `json:"raw_value"`
	Type      string `json:"type"`
	Failing   bool   `json:"failing"`
}

// GPUInfo contains detailed information about a GPU.
type GPUInfo struct {
	// Identity
	Index         int    `json:"index"`
	Name          string `json:"name"`
	UUID          string `json:"uuid"`
	Serial        string `json:"serial"`
	DriverVersion string `json:"driver_version"`
	VBIOSVersion  string `json:"vbios_version"`
	ComputeMode   string `json:"compute_mode"`
	PerfState     string `json:"perf_state"`
	// Memory
	MemoryTotalMiB uint64  `json:"memory_total_mib"`
	MemoryUsedMiB  uint64  `json:"memory_used_mib"`
	MemoryFreeMiB  uint64  `json:"memory_free_mib"`
	MemoryPercent  float64 `json:"memory_percent"`
	// Utilization
	GPUUtilPercent    float64 `json:"gpu_util_percent"`
	MemoryUtilPercent float64 `json:"memory_util_percent"`
	EncoderPercent    float64 `json:"encoder_percent"`
	DecoderPercent    float64 `json:"decoder_percent"`
	// Thermals
	TemperatureGPU    float64 `json:"temperature_gpu"`
	TemperatureMemory float64 `json:"temperature_memory"`
	FanSpeedPercent   float64 `json:"fan_speed_percent"`
	// Power
	PowerDrawW    float64 `json:"power_draw_w"`
	PowerLimitW   float64 `json:"power_limit_w"`
	PowerDefaultW float64 `json:"power_default_w"`
	PowerMaxW     float64 `json:"power_max_w"`
	// Clocks
	ClockGraphicsMHz uint32 `json:"clock_graphics_mhz"`
	ClockMemoryMHz   uint32 `json:"clock_memory_mhz"`
	ClockVideoMHz    uint32 `json:"clock_video_mhz"`
	ClockMaxGfxMHz   uint32 `json:"clock_max_gfx_mhz"`
	ClockMaxMemMHz   uint32 `json:"clock_max_mem_mhz"`
	// PCIe
	PCIBusID         string  `json:"pci_bus_id"`
	PCIeGenCurrent   int     `json:"pcie_gen_current"`
	PCIeGenMax       int     `json:"pcie_gen_max"`
	PCIeWidthCurrent int     `json:"pcie_width_current"`
	PCIeWidthMax     int     `json:"pcie_width_max"`
	PCIeRxMBps       float64 `json:"pcie_rx_mbps"`
	PCIeTxMBps       float64 `json:"pcie_tx_mbps"`
	// ECC
	ECCEnabled   bool   `json:"ecc_enabled"`
	ECCSingleBit uint64 `json:"ecc_single_bit"`
	ECCDoubleBit uint64 `json:"ecc_double_bit"`
	// Processes running on this GPU
	ProcessCount int `json:"process_count"`
}

// GPUProcess describes a process using a GPU.
type GPUProcess struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	MemoryMiB  uint64  `json:"memory_mib"`
	GPUPercent float64 `json:"gpu_percent"`
}

// WirelessInfo holds link statistics for a wifi interface, read from
// /proc/net/wireless. SSID and the associated BSSID require an nl80211 netlink
// query and are therefore not reported here.
type WirelessInfo struct {
	// LinkQuality is the driver's link quality metric (higher is better).
	LinkQuality float64 `json:"link_quality"`
	// SignalLevelDBm is the received signal strength in dBm (negative).
	SignalLevelDBm float64 `json:"signal_level_dbm"`
	// NoiseLevelDBm is the noise floor in dBm, 0 when the driver omits it.
	NoiseLevelDBm float64 `json:"noise_level_dbm"`
}

// NetworkInfo contains detailed network interface information.
type NetworkInfo struct {
	// Identity
	Index        int      `json:"index"`
	Name         string   `json:"name"`
	HardwareAddr string   `json:"hardware_addr"`
	Addresses    []string `json:"addresses"`
	MTU          int      `json:"mtu"`
	Flags        []string `json:"flags"`
	Speed        uint64   `json:"speed_mbps"`
	Duplex       string   `json:"duplex"`
	Driver       string   `json:"driver"`
	IsUp         bool     `json:"is_up"`
	IsLoopback   bool     `json:"is_loopback"`
	IsVirtual    bool     `json:"is_virtual"`
	// OperState is the kernel's operational state ("up", "down", "unknown",
	// "lowerlayerdown"). It differs from IsUp, which reflects the
	// administrative IFF_UP flag: a cable-less NIC is administratively up but
	// operationally down.
	OperState string `json:"oper_state"`
	// Kind classifies the interface: ethernet, wifi, bridge, bond, vlan, tun,
	// veth, loopback or virtual.
	Kind string `json:"kind"`
	// Resolver configuration. Per-link servers come from systemd-resolved when
	// present; otherwise the system-wide resolv.conf values are reported.
	DNSServers []string `json:"dns_servers"`
	DNSSearch  []string `json:"dns_search"`
	// Bridge membership, populated when Kind is "bridge".
	BridgePorts []string `json:"bridge_ports"`
	// Bond topology. BondMode and BondSlaves are set on a bond master;
	// BondMaster names the bond an enslaved interface belongs to.
	BondMode   string   `json:"bond_mode"`
	BondSlaves []string `json:"bond_slaves"`
	BondMaster string   `json:"bond_master"`
	// Wireless is non-nil only for wifi interfaces.
	Wireless *WirelessInfo `json:"wireless,omitempty"`
	// Traffic counters
	BytesSent   uint64 `json:"bytes_sent"`
	BytesRecv   uint64 `json:"bytes_recv"`
	PacketsSent uint64 `json:"packets_sent"`
	PacketsRecv uint64 `json:"packets_recv"`
	ErrorsIn    uint64 `json:"errors_in"`
	ErrorsOut   uint64 `json:"errors_out"`
	DropsIn     uint64 `json:"drops_in"`
	DropsOut    uint64 `json:"drops_out"`
	// Rates (calculated between snapshots)
	BytesSentRate float64 `json:"bytes_sent_rate"`
	BytesRecvRate float64 `json:"bytes_recv_rate"`
}

// LoadAverage contains system load averages.
type LoadAverage struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// ProcessDetail contains per-process information for the process table.
type ProcessDetail struct {
	PID            int32   `json:"pid"`
	Name           string  `json:"name"`
	Username       string  `json:"username"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryBytes    uint64  `json:"memory_bytes"`
	ReadBytes      uint64  `json:"read_bytes"`
	WriteBytes     uint64  `json:"write_bytes"`
	ReadBytesRate  uint64  `json:"read_bytes_rate"`
	WriteBytesRate uint64  `json:"write_bytes_rate"`
	Priority       int32   `json:"priority"`
	Status         string  `json:"status"`
}

// ProcessSummary contains an overview of running processes.
//
// The counts mirror the Linux process states so that
// Running+Sleeping+Idle+Stopped+Zombie accounts for Total. Running counts only
// processes in state R (currently on, or queued for, a CPU), which is normally
// a very small number on an otherwise idle machine; Idle counts state I, used
// by kernel worker threads that would otherwise inflate Sleeping.
type ProcessSummary struct {
	Total     int             `json:"total"`
	Running   int             `json:"running"`
	Sleeping  int             `json:"sleeping"`
	Idle      int             `json:"idle"`
	Stopped   int             `json:"stopped"`
	Zombie    int             `json:"zombie"`
	Processes []ProcessDetail `json:"process_list"`
}

// SensorData contains system-wide sensor readings collected from sysfs.
type SensorData struct {
	CoreTemps       []CoreTemp     `json:"core_temps"`
	CoreVoltages    []CoreVoltage  `json:"core_voltages"`
	PackagePower    []PackagePower `json:"package_power"`
	ThermalThrottle []ThrottleInfo `json:"thermal_throttle"`
	ThermalZones    []ThermalZone  `json:"thermal_zones"`
	Fans            []FanInfo      `json:"fans"`
	PSI             PSIData        `json:"psi"`
}

// CoreTemp is a per-core temperature reading from hwmon.
type CoreTemp struct {
	PackageID   int     `json:"package_id"`
	CoreID      int     `json:"core_id"`
	Label       string  `json:"label"`
	TempCelsius float64 `json:"temp_celsius"`
	HighCelsius float64 `json:"high_celsius"`
	CritCelsius float64 `json:"crit_celsius"`
}

// CoreVoltage is a voltage reading from hwmon.
type CoreVoltage struct {
	Label     string  `json:"label"`
	VoltageV  float64 `json:"voltage_v"`
	Channel   int     `json:"channel"`
	HwmonName string  `json:"hwmon_name"`
}

// PackagePower is a CPU package power reading from RAPL.
type PackagePower struct {
	PackageName  string  `json:"package_name"`
	PowerW       float64 `json:"power_w"`
	MaxPowerW    float64 `json:"max_power_w"`
	EnergyJoules float64 `json:"energy_joules"`
}

// ThrottleInfo contains thermal throttle event counts for a CPU.
type ThrottleInfo struct {
	CPU                  int    `json:"cpu"`
	CoreThrottleCount    uint64 `json:"core_throttle_count"`
	PackageThrottleCount uint64 `json:"package_throttle_count"`
}

// ThermalZone describes a system thermal zone.
type ThermalZone struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	TempCelsius float64 `json:"temp_celsius"`
	Policy      string  `json:"policy"`
}

// FanInfo is a fan speed reading from hwmon.
type FanInfo struct {
	Label     string `json:"label"`
	RPM       uint64 `json:"rpm"`
	MinRPM    uint64 `json:"min_rpm"`
	MaxRPM    uint64 `json:"max_rpm"`
	HwmonName string `json:"hwmon_name"`
}

// PSIData contains Pressure Stall Information for cpu, memory, and io.
type PSIData struct {
	CPU    PSIResource `json:"cpu"`
	Memory PSIResource `json:"memory"`
	IO     PSIResource `json:"io"`
}

// PSIResource holds some/full pressure data for a single resource.
type PSIResource struct {
	SomeAvg10  float64 `json:"some_avg10"`
	SomeAvg60  float64 `json:"some_avg60"`
	SomeAvg300 float64 `json:"some_avg300"`
	SomeTotal  uint64  `json:"some_total"`
	FullAvg10  float64 `json:"full_avg10"`
	FullAvg60  float64 `json:"full_avg60"`
	FullAvg300 float64 `json:"full_avg300"`
	FullTotal  uint64  `json:"full_total"`
}
