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
	// Version is the go-sysmon build serving this snapshot. It travels with the
	// data so the fleet view can spot a node left behind on an older build.
	Version string `json:"version"`
	// GitCommit and BuildDate identify the BUILD, which Version alone cannot.
	// The release string has read "0.1.0-alpha" for every build this project
	// has ever produced, so a fleet on five different builds reported one word
	// ten times and a stale node was indistinguishable from a current one.
	// GET /api/version has always carried these two; the snapshot, and so the
	// fleet payload built from it, dropped them.
	//
	// EMPTY MEANS THE NODE DID NOT STATE ITS BUILD -- a binary predating these
	// fields, or one built without the -ldflags that inject them. That is not
	// agreement with anything and must never be read as agreement. A binary
	// built without the flags reports the literal "unknown" instead, for the
	// same reason and with the same meaning.
	GitCommit string `json:"git_commit,omitempty"`
	BuildDate string `json:"build_date,omitempty"`
	// Alerts are the threshold evaluations for this snapshot. Evaluated
	// server-side so the dashboard, the API and the Prometheus endpoint all
	// report the same conditions from the same rules.
	Alerts []Alert `json:"alerts"`
}

// Alert severities, ordered by urgency.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Alert is one firing threshold rule. ID is stable across snapshots for the
// same rule and resource, which is what lets the UI track "new since last seen"
// and the evaluator keep a first-seen timestamp.
type Alert struct {
	ID        string    `json:"id"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Category  string    `json:"category"`
	Resource  string    `json:"resource"`
	Message   string    `json:"message"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	Since     time.Time `json:"since"`
}

// PeerSnapshot is one peer's snapshot together with the freshness of the poll
// that produced it.
//
// The snapshot alone is not enough to render honestly. The poller deliberately
// keeps the last good reading when a peer stops answering, so a bare snapshot
// looks identical whether it was collected a moment ago or hours before the
// host went away. Serving that as an ordinary success let the drill-in view
// show frozen numbers as live ones -- the fleet card reported the node
// unreachable while the detail page beside it did not.
//
// Reachable describes the LATEST poll; Snapshot may still be present when it is
// false, which is the stale case the client has to mark rather than hide.
type PeerSnapshot struct {
	Snapshot   *Snapshot `json:"snapshot"`
	Reachable  bool      `json:"reachable"`
	Error      string    `json:"error,omitempty"`
	AgeSeconds float64   `json:"age_seconds"`
}

// FleetNode is one node's summary in the fleet view: enough to judge health at
// a glance without opening the node.
type FleetNode struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Self      bool   `json:"self"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	// AgeSeconds is how old the reading in this summary is -- it dates the
	// DATA, not the last poll attempt, so a failing poll does not make stale
	// data look freshly fetched.
	//
	// ZERO MEANS THERE IS NO READING AT ALL: a peer that has never answered,
	// or has not been polled yet. It does not mean "fetched just now". The
	// distinction matters because the only other thing a consumer knows about
	// an unreachable node is how long it has been that way, and a node that
	// never answered would otherwise report a two-second gap forever.
	AgeSeconds    float64 `json:"age_seconds"`
	Status        string  `json:"status"`
	Hostname      string  `json:"hostname"`
	Version       string  `json:"version"`
	UptimeSeconds uint64  `json:"uptime_seconds"`

	// GitCommit and BuildDate are the node's build identity, carried up from
	// its snapshot. Version is the release string and has never varied, so
	// these are the only fields in this summary that can tell one build from
	// another. Empty means the node did not state its build.
	GitCommit string `json:"git_commit,omitempty"`
	BuildDate string `json:"build_date,omitempty"`
	// BuildMatch says whether this node runs the same build as the aggregator
	// serving the view: "same", "different" or "unknown". The aggregator knows
	// its own commit, so this is a comparison it can make once rather than
	// leaving every consumer to make it -- and get wrong for the node that
	// answered nothing.
	//
	// IT IS NOT A HEALTH SIGNAL, and deliberately does not touch Status,
	// WarningCount or CriticalCount. Mid-rollout every node disagrees with the
	// aggregator for as long as the rollout takes; a fleet that went amber on
	// every deploy would teach operators to ignore the colour. Divergence is a
	// fact to display, not a fault to raise.
	//
	// "unknown" is its own answer and never collapses into "same". A node that
	// did not state its build has not agreed with anything, and reporting the
	// silence as agreement is the exact failure this field was added to end.
	BuildMatch string `json:"build_match"`

	CriticalCount int `json:"critical_count"`
	WarningCount  int `json:"warning_count"`

	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	SwapPercent   float64 `json:"swap_percent"`
	LoadPerCore   float64 `json:"load_per_core"`

	WorstDiskPercent float64 `json:"worst_disk_percent"`

	// RAIDArrays and SMARTCapable are the DENOMINATORS, and without them the
	// two fields below cannot be read.
	//
	// RAIDDegraded false is produced both by "this host has no arrays" and by
	// "its arrays are fine". SMARTUnhealthy zero is produced both by "no disk
	// reported SMART at all" and by "every disk is healthy". The fleet view
	// rendered both pairs as a green ok, so a node with no RAID advertised a
	// healthy RAID subsystem it does not have, and ci-node-1 -- whose PERC and
	// SAS devices report nothing -- advertised healthy SMART while its own node
	// page said N/A for every one of them.
	//
	// The per-disk model already distinguishes all of this: DiskPanel shows
	// "SMART N/A" for a disk with SMARTEnabled false and omits the RAID badge
	// when Disk.RAID is nil. It was the roll-up that discarded it, so these
	// carry it through rather than inventing anything new.
	RAIDArrays int `json:"raid_arrays"`
	// RAIDUnhealthy counts arrays that are degraded OR not serving I/O at all.
	// RAIDDegraded is kept as it was -- degraded only -- so an older consumer
	// reading it still gets what it expects.
	RAIDUnhealthy  int  `json:"raid_unhealthy"`
	RAIDDegraded   bool `json:"raid_degraded"`
	SMARTCapable   int  `json:"smart_capable"`
	SMARTUnhealthy int  `json:"smart_unhealthy"`

	MaxTempC  float64 `json:"max_temp_c"`
	PowerW    float64 `json:"power_w"`
	NetRxRate float64 `json:"net_rx_rate"`
	NetTxRate float64 `json:"net_tx_rate"`
}

// Fleet is the aggregated view served at /api/fleet. Enabled is false when no
// peers are configured, which is what keeps a standalone node behaving exactly
// as it did before fleet mode existed.
type Fleet struct {
	Enabled bool        `json:"enabled"`
	Nodes   []FleetNode `json:"nodes"`
}

// ConfigEntry is one effective setting and where its value came from, for the
// read-only settings view.
type ConfigEntry struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	Source  string `json:"source"`
}

// ConfigView is the effective configuration served at /api/config.
type ConfigView struct {
	// ConfigFile says WHETHER a configuration file is in use -- "(configured)"
	// or "(not set)" -- and deliberately not where it is. The endpoint has no
	// authentication of its own, and an absolute path is a map of the host's
	// filesystem layout handed to anyone who can reach the port (#16).
	ConfigFile string        `json:"config_file"`
	Entries    []ConfigEntry `json:"entries"`
}

// HistoryMetric is one series the history API can draw, as advertised by
// GET /api/history/metrics.
type HistoryMetric struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Unit    string `json:"unit"`
	SplitBy string `json:"split_by"`
}

// HistoryCatalog lists what history is available. Enabled is false on a node
// with no Prometheus configured, which is a standalone node rather than a
// broken one -- the dashboard hides the date picker and streams as it always
// has.
type HistoryCatalog struct {
	Enabled bool            `json:"enabled"`
	Metrics []HistoryMetric `json:"metrics"`
}

// HistoryPoint is one instant of a history series. The names are short because
// a full range carries hundreds of them per series.
type HistoryPoint struct {
	TimeMs int64   `json:"t"`
	Value  float64 `json:"v"`
}

// HistorySeries is one line on a history chart. Name is the value of the
// metric's splitting label -- the device, the interface, the GPU index -- and
// is empty for a metric a node yields only one of.
type HistorySeries struct {
	Name   string         `json:"name"`
	Points []HistoryPoint `json:"points"`
}

// History is a date range answered from Prometheus, served at GET /api/history.
//
// StepMs is reported back because the server may have chosen it: a caller that
// asks for a month gets whatever resolution keeps the response drawable, and a
// chart that does not know the step cannot tell a gap in the data from a gap
// between samples.
type History struct {
	Enabled bool            `json:"enabled"`
	Node    string          `json:"node"`
	Metric  string          `json:"metric"`
	Label   string          `json:"label"`
	Unit    string          `json:"unit"`
	StartMs int64           `json:"start_ms"`
	EndMs   int64           `json:"end_ms"`
	StepMs  int64           `json:"step_ms"`
	Series  []HistorySeries `json:"series"`
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
	// SMART status. SMARTEnabled and SMARTHealthy keep the meaning they have
	// always had -- "a reading was obtained" and "that reading was good" --
	// because pkg/metrics and pkg/fleet are built on them. SMARTStatus says
	// the thing two booleans cannot: which of several different absences a
	// missing reading is.
	SMARTEnabled bool        `json:"smart_enabled"`
	SMARTHealthy bool        `json:"smart_healthy"`
	SMARTStatus  SMARTStatus `json:"smart_status"`
	// SMARTReason is a sentence naming why there is no reading, for the
	// statuses that have a why. Empty for SMARTOK and SMARTFailing.
	SMARTReason string      `json:"smart_reason"`
	SMARTAttrs  []SMARTAttr `json:"smart_attrs"`
	// Members are the physical drives behind a hardware RAID controller, for a
	// device that is a virtual disk. Empty for everything else, including md
	// arrays -- their components are in RAID.Members, which is a different
	// thing: those are block devices this host can address directly.
	Members      []DiskMember `json:"members"`
	Temperature  float64      `json:"temperature_celsius"`
	PowerOnHours uint64       `json:"power_on_hours"`
	PowerCycles  uint64       `json:"power_cycles"`
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
	// RAID is non-nil when this device is an array: a Linux md array, or a
	// hardware RAID controller's virtual disk. It carries the array's level,
	// health and member counts; see RAIDInfo for which fields each source
	// fills in.
	RAID *RAIDInfo `json:"raid,omitempty"`
}

// RAIDInfo describes the state of a RAID array, attached to the DiskInfo of the
// device that presents it and nil for devices that are not arrays.
//
// It covers two sources, and they do not fill in the same fields. A Linux md
// array (md0) publishes everything below. A HARDWARE RAID volume -- a
// controller's virtual disk, presented to the kernel as an ordinary SCSI disk
// -- publishes its level, state, degraded flag and member counts, and leaves
// the background-operation fields empty.
//
// SyncAction is the flag for which: an EMPTY action means the source does not
// report md-specific state, and FOUR fields are meaningless in that state --
// SyncProgressPercent, MismatchCount, FailedDevices and SpareDevices. See their
// comments; the distinction is not cosmetic, because their zero values are what
// "everything is fine" looks like in this struct.
//
// One sentinel covers all four deliberately. A second flag meaning almost the
// same thing is how two flags drift apart, and a consumer holding one rule in
// its head is likelier to apply it than one holding two.
type RAIDInfo struct {
	// Level is the array personality, e.g. "raid1", "raid10", "raid5".
	Level string `json:"level"`
	// State is the md array_state, e.g. "clean", "active", "read-auto".
	State string `json:"state"`
	// Degraded is true when fewer than the configured members are in sync.
	Degraded bool `json:"degraded"`
	// TotalDevices is the number of member slots the array is configured for;
	// ActiveDevices is how many are currently in sync.
	TotalDevices  int `json:"total_devices"`
	ActiveDevices int `json:"active_devices"`
	// FailedDevices and SpareDevices count members in those roles.
	//
	// MEANINGLESS WHEN SyncAction IS EMPTY, for the same reason as the two
	// fields below: they are md counters, read from that array's own state,
	// and a hardware RAID volume publishes neither. Zero there is "nobody
	// asked", not "none failed" -- and on a redundant array those are opposite
	// claims, only one of which is reassuring.
	//
	// They are NOT derived from the controller's per-drive states, though a
	// count of members that are not online would be easy to produce. It would
	// also be wrong: a drive rebuilding is not a failed drive, a hot spare is
	// not a member, and the MFI state constants beyond ONLINE have never been
	// observed on real hardware here. A count assembled from unverified
	// constants is a confident number, which is exactly what this comment
	// exists to prevent.
	FailedDevices int `json:"failed_devices"`
	SpareDevices  int `json:"spare_devices"`
	// SyncAction is the current background operation: "idle", "resync",
	// "recover", "check" or "repair" -- or **empty, meaning NOT REPORTED**.
	//
	// The empty value is the sentinel for a source that does not publish
	// background-operation state at all, which today means a hardware RAID
	// volume: the controller is never asked about rebuild, consistency-check
	// or background-init progress. It is NOT "idle". Reporting idle would say
	// a volume in the middle of a rebuild had finished one, on the single
	// occasion this matters most.
	//
	// A consumer must render and alert on nothing when this is empty, rather
	// than treating it as malformed or as a completed sync.
	SyncAction string `json:"sync_action"`
	// SyncProgressPercent is 0-100 while a resync/recover/check runs, or 100
	// when the array is fully in sync (sync_action idle).
	//
	// MEANINGLESS WHEN SyncAction IS EMPTY. Zero there is the absence of a
	// reading, not a stalled rebuild.
	SyncProgressPercent float64 `json:"sync_progress_percent"`
	// MismatchCount is the md mismatch_cnt: sectors that differed on the last
	// check. Non-zero on a redundant array warrants a repair.
	//
	// LIKEWISE MEANINGLESS WHEN SyncAction IS EMPTY, and for the same reason:
	// a source that does not report background operations does not report
	// their results either, so zero is "no check was read" rather than "the
	// last check was clean". The two are opposite claims about a redundant
	// array and only one of them is reassuring.
	MismatchCount uint64 `json:"mismatch_count"`
	// Members are the array's component devices in slot order.
	Members []RAIDMember `json:"members"`
}

// RAIDMember is one component device of an md array.
type RAIDMember struct {
	// Name is the component block device, e.g. "sda2".
	Name string `json:"name"`
	// State is the md per-member state, e.g. "in_sync", "faulty", "spare",
	// "writemostly". Multiple flags are joined with ",".
	State string `json:"state"`
	// Slot is the RAID role/slot number, or -1 for a device with no slot
	// (spare or removed).
	Slot int `json:"slot"`
	// Errors is the md per-member read/corrected error count.
	Errors uint64 `json:"errors"`
}

// DiskMember is one physical drive behind a hardware RAID controller.
//
// It is not a block device and cannot be made into one. The controller presents
// the volume; its members have no device node, no name in /sys/block, no
// partitions, no filesystem and no I/O counters -- every field DiskInfo is
// built around. So this carries only what a controller can be asked about a
// drive: what it is, where it is plugged in, and whether it says it is well.
//
// It is a separate type from RAIDMember rather than a reuse of it. RAIDMember
// names a block device ("sda2") because an md array's components are devices
// this host addresses directly; a drive behind a PERC is not, and widening that
// field's meaning to fit would leave one name covering two different things.
type DiskMember struct {
	// Serial is the only identity that survives a reboot. The device ID and
	// the slot describe where the drive is plugged in, not which drive it is,
	// and ci-node-1's 2026-08-31 rebuild renamed every device on the machine
	// -- so a metric keyed on anything else silently becomes a different
	// series, which is worse than a missing one because the history looks
	// continuous.
	Serial string `json:"serial"`
	Model  string `json:"model"`
	Vendor string `json:"vendor"`
	// DeviceID is how the controller's firmware addresses this drive, and is
	// unique across the controller.
	DeviceID int `json:"device_id"`
	// Enclosure and Slot are the physical location: the bay to pull.
	Enclosure int `json:"enclosure"`
	Slot      int `json:"slot"`
	// Online is the CONTROLLER's verdict on the drive: in service as part of
	// its array. It is a different question from whether the drive answers a
	// health command -- a failed drive can still answer, and a working one can
	// refuse a command it does not implement -- so the two are reported
	// separately rather than one standing in for the other.
	Online bool `json:"online"`
	// Temperature is 0 when the drive did not report one.
	Temperature float64 `json:"temperature_celsius"`
	// SMART status, with the same meanings as on DiskInfo.
	SMARTEnabled bool        `json:"smart_enabled"`
	SMARTHealthy bool        `json:"smart_healthy"`
	SMARTStatus  SMARTStatus `json:"smart_status"`
	SMARTReason  string      `json:"smart_reason"`
}

// PartitionInfo describes a disk partition.
type PartitionInfo struct {
	Device     string `json:"device"`
	Mountpoint string `json:"mountpoint"`
	Fstype     string `json:"fstype"`
	Opts       string `json:"opts"`
}

// SMARTStatus names which of several different things "there is no SMART
// reading for this device" means.
//
// Before this existed, a device with no reading carried SMARTEnabled=false and
// SMARTHealthy=false, and four unrelated situations were emitted identically.
// Measured on ci-node-1 (2026-09-02), all four were present at once on the
// same host and rendered as the same "SMART N/A": dm-0, a device-mapper node
// that can never have SMART; sda and sdb, PERC virtual disks whose member
// drives answer perfectly well behind the controller; sdc, a DELLBOSS card
// that answers "I do not implement SMART" when asked with enough privilege to
// hear it; and, without CAP_SYS_RAWIO, every ATA device on the host, refused
// at the ioctl. "N/A" reads as "nothing to see" for all four, and only one of
// them means that.
type SMARTStatus string

const (
	// SMARTUnknown is the zero value: nothing has classified this device. An
	// older server that predates this field sends no value, which decodes to
	// this, so a consumer must treat it as "no claim" and say less -- never as
	// a reassuring one.
	SMARTUnknown SMARTStatus = ""
	// SMARTNotApplicable is a virtual block device -- a device-mapper node, an
	// md array, an unbound nbd or loop device. There is no medium to ask.
	SMARTNotApplicable SMARTStatus = "not_applicable"
	// SMARTBehindController is a device served by a hardware RAID controller
	// that did not answer SMART directly. That -- and not "this is a volume" --
	// is what the evidence supports: the SCSI host driver is knowable, whether
	// THIS device is a volume or a passthrough disk is not. When it is a
	// volume, SMART belongs to its member drives behind the controller.
	// SMARTReason names the controller and says so conditionally.
	SMARTBehindController SMARTStatus = "behind_controller"
	// SMARTUnsupported is a physical device that answered the question, and
	// answered that it does not implement the SMART feature set.
	SMARTUnsupported SMARTStatus = "unsupported"
	// SMARTUnreadable is a device that should be able to answer but did not.
	// SMARTReason says what stopped it -- most often a withheld privilege,
	// which is a fact about this deployment rather than about the hardware.
	SMARTUnreadable SMARTStatus = "unreadable"
	// SMARTOK is a reading, and the reading is good.
	SMARTOK SMARTStatus = "ok"
	// SMARTFailing is a reading, and the drive is reporting failure.
	SMARTFailing SMARTStatus = "failing"
)

// HasReading reports whether s is an actual reading from the device rather
// than one of the ways of having none.
func (s SMARTStatus) HasReading() bool {
	return s == SMARTOK || s == SMARTFailing
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
