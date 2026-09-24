package collector

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// accelDevicePattern matches accelerator device directories (accel0, accel1, ...).
var accelDevicePattern = regexp.MustCompile(`^accel\d+$`)

// accelRoot is where the kernel exposes compute accelerators. Every in-tree
// NPU driver registers in this class -- intel_vpu, amdxdna, qaic, habanalabs,
// rocket -- because the accel subsystem is a DRM device type with its own
// major number, so enumerating it finds NPUs on hardware this build has never
// seen. It is a variable so tests can enumerate a synthetic tree containing
// accelerators the build host does not have.
var accelRoot = "/sys/class/accel"

// pciIDsPaths are the locations of the hwdata PCI identifier database, tried
// in order. It is packaged separately from the kernel, so it may be absent;
// npuVendorNames covers the vendors that matter when it is.
var pciIDsPaths = []string{
	"/usr/share/misc/pci.ids",
	"/usr/share/hwdata/pci.ids",
}

// npuVendorNames names the PCI vendors shipping accel-class hardware, for
// hosts with no pci.ids database installed.
var npuVendorNames = map[string]string{
	"0x8086": "Intel Corporation",
	"0x1002": "Advanced Micro Devices, Inc. [AMD/ATI]",
	"0x1022": "Advanced Micro Devices, Inc. [AMD]",
	"0x17cb": "Qualcomm Inc",
	"0x1da3": "Habana Labs Ltd.",
}

// NPUCollector collects metrics for compute accelerators in the kernel accel
// class. Collection is layered so that it degrades by field rather than by
// device: identity, power-state residency, hwmon and devfreq come from generic
// sysfs that any driver provides, per-client compute time comes from the
// standard DRM usage-stats fdinfo interface, and only the small extra
// attribute set each driver invents needs an entry in npuDriverReaders.
//
// When no accelerators are present Collect is a no-op and Info returns an
// empty slice without error.
type NPUCollector struct {
	logger *slog.Logger
	npus   atomic.Pointer[[]types.NPUInfo]
	prev   atomic.Pointer[npuSampleSet]
}

// NewNPUCollector returns an NPUCollector ready for use.
func NewNPUCollector(logger *slog.Logger) *NPUCollector {
	c := &NPUCollector{logger: logger}
	empty := []types.NPUInfo{}
	c.npus.Store(&empty)
	return c
}

// npuCounters holds the values read from a driver's own sysfs attributes.
// Reading these can resume a runtime-suspended device, so they are sampled
// only while the accelerator is already awake.
type npuCounters struct {
	busyTimeUs  uint64
	memoryBytes uint64
	schedMode   string
	freqMHz     uint32
	freqMaxMHz  uint32
	read        bool
}

// npuSample is one device's previous reading, kept so the next collection can
// turn monotonic counters into rates.
type npuSample struct {
	activeMs    uint64
	suspendedMs uint64
	engineNs    map[string]uint64
	// clientNs is the previous cumulative busy time per client, keyed by
	// npuClientSample.key. Without it a client's whole lifetime history lands
	// in every window rather than only the first, which reads as 100% forever
	// for anything that has ever done real work.
	clientNs   map[string]uint64
	counters   npuCounters
	countersAt time.Time
}

// npuSampleSet is the previous collection for every device, with the instant
// it was taken.
type npuSampleSet struct {
	at      time.Time
	devices map[string]npuSample
}

// Collect enumerates the accel class and refreshes every accelerator's
// metrics. Results are stored atomically and returned by Info.
func (c *NPUCollector) Collect() error {
	names, err := enumerateAccelDevices()
	if err != nil {
		warnOnce(c.logger, "npu:enumerate", "failed to enumerate accel devices", "error", err)
		empty := []types.NPUInfo{}
		c.npus.Store(&empty)
		return nil
	}
	if len(names) == 0 {
		empty := []types.NPUInfo{}
		c.npus.Store(&empty)
		return nil
	}

	now := time.Now()
	prev := c.prev.Load()
	var elapsed time.Duration
	if prev != nil {
		elapsed = now.Sub(prev.at)
	}

	clients := c.collectAccelClients()
	cur := &npuSampleSet{at: now, devices: make(map[string]npuSample, len(names))}
	npus := make([]types.NPUInfo, 0, len(names))

	for i, name := range names {
		var last npuSample
		var hasLast bool
		if prev != nil {
			last, hasLast = prev.devices[name]
		}
		n, sample := c.collectAccelDevice(name, i, last, hasLast, elapsed, now, clients)
		cur.devices[name] = sample
		npus = append(npus, n)
	}

	sort.Slice(npus, func(i, j int) bool { return npus[i].Index < npus[j].Index })

	c.prev.Store(cur)
	c.npus.Store(&npus)
	return nil
}

// Info returns the most recently collected accelerator information.
func (c *NPUCollector) Info() []types.NPUInfo {
	return *c.npus.Load()
}

// enumerateAccelDevices returns the sorted accelerator device names found in
// accelRoot (e.g. ["accel0"]). A host with no accel class at all is not an
// error; it reports no devices, exactly as a host with the class present and
// empty does.
func enumerateAccelDevices() ([]string, error) {
	entries, err := os.ReadDir(accelRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", accelRoot, err)
	}
	var devices []string
	for _, e := range entries {
		if accelDevicePattern.MatchString(e.Name()) {
			devices = append(devices, e.Name())
		}
	}
	sort.Strings(devices)
	return devices, nil
}

// collectAccelDevice reads one accelerator and returns both the reported
// metrics and the sample to carry into the next collection.
func (c *NPUCollector) collectAccelDevice(
	name string, index int,
	last npuSample, hasLast bool,
	elapsed time.Duration, now time.Time,
	clients map[string][]npuClientSample,
) (types.NPUInfo, npuSample) {
	devPath := accelDevicePath(name)

	n := types.NPUInfo{
		Index:  index,
		Device: name,
		Driver: sysfsDriverName(devPath),
	}
	sample := npuSample{}

	// Identity. None of these reads touch the device; they are properties the
	// bus recorded at enumeration.
	n.VendorID = strings.ToLower(readSysfsString(filepath.Join(devPath, "vendor")))
	n.DeviceID = strings.ToLower(readSysfsString(filepath.Join(devPath, "device")))
	n.Label = readSysfsString(filepath.Join(devPath, "label"))
	n.PCIBusID = sysfsPCIBusID(devPath)
	n.LocalCPUList = readSysfsString(filepath.Join(devPath, "local_cpulist"))
	n.IRQ = readSysfsInt(filepath.Join(devPath, "irq"))
	n.NumaNode = readSysfsInt(filepath.Join(devPath, "numa_node"))
	n.Vendor, n.Name, n.DeviceClass = npuIdentity(devPath, n.VendorID, n.DeviceID, n.Driver, n.Label)

	n.PCIeGenCurrent = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "current_link_speed")))
	n.PCIeGenMax = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "max_link_speed")))
	n.PCIeWidthCurrent = pcieLinkWidth(readSysfsUint64(filepath.Join(devPath, "current_link_width")))
	n.PCIeWidthMax = pcieLinkWidth(readSysfsUint64(filepath.Join(devPath, "max_link_width")))

	// Power state and residency, from the runtime-PM counters every driver
	// gets for free. Reading these does not resume the device.
	powerPath := filepath.Join(devPath, "power")
	n.State = readSysfsString(filepath.Join(powerPath, "runtime_status"))
	n.PowerState = readSysfsString(filepath.Join(devPath, "power_state"))
	n.ActiveTimeMs = readSysfsUint64(filepath.Join(powerPath, "runtime_active_time"))
	n.SuspendedTimeMs = readSysfsUint64(filepath.Join(powerPath, "runtime_suspended_time"))
	n.AutosuspendDelayMs = int64(readSysfsInt(filepath.Join(powerPath, "autosuspend_delay_ms")))
	n.RuntimeUsage = readSysfsInt(filepath.Join(powerPath, "runtime_usage"))
	sample.activeMs = n.ActiveTimeMs
	sample.suspendedMs = n.SuspendedTimeMs
	n.ResidencyPercent = npuResidency(n.State, n.ActiveTimeMs, n.SuspendedTimeMs, last, hasLast)

	// Driver-specific counters. Reading them resumes a suspended accelerator,
	// which on intel_vpu costs idle power for as long as the monitor keeps
	// polling -- a 10 ms autosuspend delay is no defence against a 500 ms
	// poll. So a sleeping device is left asleep and its last known counters
	// are carried forward, labelled with their age.
	suspended := n.State == "suspended"
	if suspended {
		sample.counters = last.counters
		sample.countersAt = last.countersAt
		n.CountersStale = true
		if hasLast && !last.countersAt.IsZero() {
			n.CountersAgeSec = now.Sub(last.countersAt).Seconds()
		}
	} else if reader, ok := npuDriverReaders[n.Driver]; ok {
		sample.counters = reader(devPath)
		sample.countersAt = now
	} else {
		sample.counters = last.counters
		sample.countersAt = last.countersAt
		warnOnce(c.logger, "npu:driver:"+n.Driver,
			"no driver-specific NPU attributes known for this accelerator; reporting generic metrics only",
			"device", name, "driver", n.Driver,
			"note", "this is logged once per driver per run")
	}
	n.BusyTimeUs = sample.counters.busyTimeUs
	n.MemoryUtilizationBytes = sample.counters.memoryBytes
	n.SchedMode = sample.counters.schedMode
	n.FrequencyMHz = sample.counters.freqMHz
	n.FrequencyMaxMHz = sample.counters.freqMaxMHz

	// devfreq is how SoC accelerators expose clocks, and it does not need the
	// device awake. It fills in only what the driver table did not supply.
	if n.FrequencyMHz == 0 || n.FrequencyMaxMHz == 0 || n.Governor == "" {
		cur, max, governor := npuDevfreq(devPath)
		if n.FrequencyMHz == 0 {
			n.FrequencyMHz = cur
		}
		if n.FrequencyMaxMHz == 0 {
			n.FrequencyMaxMHz = max
		}
		n.Governor = governor
	}

	// Thermals and power, when the driver registers an hwmon node.
	if hwmonPath := sysfsHwmonPath(devPath); hwmonPath != "" {
		n.TemperatureC = float64(readSysfsUint64(filepath.Join(hwmonPath, "temp1_input"))) / 1000.0
		n.PowerDrawW = float64(readSysfsUint64(filepath.Join(hwmonPath, "power1_input"))) / 1_000_000.0
	}

	// Per-client compute time. This is the only genuine utilisation signal
	// here, and it is standardised across every DRM and accel driver that
	// implements the usage-stats interface.
	samples := clients[n.PCIBusID]
	if len(samples) == 0 {
		samples = clients[name]
	}
	n.Clients, sample.clientNs = npuClients(samples, last.clientNs, hasLast, elapsed)
	n.ClientCount = len(n.Clients)
	sample.engineNs = npuEngineTotals(samples)
	n.Engines, n.UtilPercent = npuEngineRates(sample.engineNs, last.engineNs, hasLast, elapsed)

	return n, sample
}

// pcieLinkWidth converts a sysfs link-width attribute into a lane count,
// rejecting the all-ones value the PCIe specification uses for "unknown".
//
// An integrated accelerator is enumerated as a PCI device without being on a
// real link: the Arrow Lake NPU reports its speed as the literal "Unknown" and
// its maximum width as 255, which read verbatim would claim a 255-lane link.
func pcieLinkWidth(raw uint64) int {
	if raw == 0 || raw >= 0xff {
		return 0
	}
	return int(raw)
}

// accelDevicePath resolves an accel class entry to the bus device directory
// holding its attributes. The class entry is a symlink into /sys/devices and
// its "device" child points at the parent bus device; a tree of plain
// directories, as tests build, resolves to itself.
func accelDevicePath(name string) string {
	devPath := filepath.Join(accelRoot, name, "device")
	if resolved, err := filepath.EvalSymlinks(devPath); err == nil {
		return resolved
	}
	return devPath
}

// sysfsDriverName returns the name of the driver bound to a device, read from
// the driver symlink.
func sysfsDriverName(devPath string) string {
	resolved, err := filepath.EvalSymlinks(filepath.Join(devPath, "driver"))
	if err != nil {
		// A device with no driver bound has no driver link. Fall back to the
		// uevent DRIVER key, which tests can write without symlinks.
		return ueventValue(devPath, "DRIVER")
	}
	return filepath.Base(resolved)
}

// ueventValue returns one KEY=value entry from a device's uevent file.
func ueventValue(devPath, key string) string {
	uevent := readSysfsString(filepath.Join(devPath, "uevent"))
	for _, line := range strings.Split(uevent, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// npuResidency returns the share of the sampling interval the accelerator
// spent powered up.
//
// The runtime-PM counters are the denominator, not wall time: they are the
// device's own account of where the interval went, so a delayed collection
// cycle cannot inflate the figure. The first sample has no interval to
// measure, so the instantaneous power state stands in for it -- the best
// estimate available for a zero-length window, and it stops a freshly started
// monitor from reporting a busy accelerator as idle.
func npuResidency(state string, activeMs, suspendedMs uint64, last npuSample, hasLast bool) float64 {
	if hasLast {
		dActive := safeSub(activeMs, last.activeMs)
		dSuspended := safeSub(suspendedMs, last.suspendedMs)
		if total := dActive + dSuspended; total > 0 {
			return float64(dActive) / float64(total) * 100
		}
	}
	switch state {
	case "active":
		return 100
	case "suspended":
		return 0
	}
	// No runtime PM at all. The caller documents that ResidencyPercent is
	// meaningless when State is empty, rather than pretending to a zero.
	return 0
}

// npuIdentity resolves the human-readable vendor, model and class of an
// accelerator. The PCI identifier database gives real model names for hardware
// this build has never heard of ("Arrow Lake NPU" for 8086:ad1d); where it is
// not installed, the firmware label, the vendor table and the driver name in
// turn keep the device recognisable.
func npuIdentity(devPath, vendorID, deviceID, driver, label string) (vendor, name, class string) {
	vendor, name, class = lookupPCINames(vendorID, deviceID, readSysfsString(filepath.Join(devPath, "class")))
	if vendor == "" {
		vendor = npuVendorNames[vendorID]
	}
	if name == "" {
		switch {
		case label != "" && !strings.EqualFold(label, "Onboard - Other"):
			// "Onboard - Other" is what firmware writes when it has nothing to
			// say, and naming a device after it tells the reader nothing.
			name = label
		case vendor != "":
			name = vendor + " accelerator"
		case driver != "":
			name = driver
		default:
			name = "accelerator"
		}
	}
	if class == "" {
		class = "Processing accelerators"
	}
	return vendor, name, class
}

// npuDevfreq reads the current and maximum frequency in MHz, and the
// governor, from a device's devfreq node. SoC accelerators expose their clocks
// this way rather than through driver-specific attributes.
func npuDevfreq(devPath string) (cur, max uint32, governor string) {
	matches, err := filepath.Glob(filepath.Join(devPath, "devfreq", "*"))
	if err != nil || len(matches) == 0 {
		return 0, 0, ""
	}
	node := matches[0]
	// devfreq reports hertz; every other frequency in this collector is MHz.
	const hzPerMHz = 1_000_000
	cur = uint32(readSysfsUint64(filepath.Join(node, "cur_freq")) / hzPerMHz)
	max = uint32(readSysfsUint64(filepath.Join(node, "max_freq")) / hzPerMHz)
	return cur, max, readSysfsString(filepath.Join(node, "governor"))
}

// npuEngineTotals sums each engine's busy time across every client of one
// device.
func npuEngineTotals(samples []npuClientSample) map[string]uint64 {
	if len(samples) == 0 {
		return nil
	}
	totals := make(map[string]uint64)
	for _, cl := range samples {
		for engine, busyNs := range cl.engines {
			totals[engine] += busyNs
		}
	}
	return totals
}

// npuClients converts the raw fdinfo samples for one device into the reported
// client list, ordered by who is busiest NOW.
//
// The DRM usage-stats values are cumulative for the life of the file
// descriptor, not per-interval, so the rate is a delta against the previous
// sample -- the same arithmetic npuEngineRates does one function below, for the
// same reason. Dividing the lifetime total by the interval instead does not
// merely overstate the first window: it pins every client that has ever done a
// meaningful amount of work at 100% on every sample afterwards, idle or not,
// with clampPercent hiding the arithmetic rather than fixing it. On the same
// screen that would have read 5% for the device and 100% for the one process
// holding it.
//
// A client seen for the first time reports 0%: its rate over a window this
// process did not observe is not knowable, and the next sample gives a real one.
// A total that goes backwards -- which a driver reusing a client id can produce
// -- reads as no activity rather than as a negative rate, via safeSub.
//
// The returned map is the caller's next previous-sample state.
func npuClients(samples []npuClientSample, prev map[string]uint64, hasPrev bool, elapsed time.Duration) ([]types.NPUClient, map[string]uint64) {
	if len(samples) == 0 {
		return []types.NPUClient{}, nil
	}
	clients := make([]types.NPUClient, 0, len(samples))
	totals := make(map[string]uint64, len(samples))

	for _, cl := range samples {
		var busyNs uint64
		for _, ns := range cl.engines {
			busyNs += ns
		}
		totals[cl.key] = busyNs

		c := types.NPUClient{
			PID:         cl.pid,
			Name:        cl.name,
			ClientID:    cl.clientID,
			BusyNs:      busyNs,
			MemoryBytes: cl.memoryBytes,
		}
		// BusyNs stays the raw cumulative counter it is; only the percentage
		// comes from the delta. That is the invariant this collector holds
		// everywhere: utilisation from deltas, counters shown as counters.
		if hasPrev && elapsed > 0 {
			if before, seen := prev[cl.key]; seen {
				c.BusyPercent = clampPercent(
					float64(safeSub(busyNs, before)) / float64(elapsed.Nanoseconds()) * 100)
			}
		}
		clients = append(clients, c)
	}

	// Busiest now first, so the process responsible for current load leads.
	// Ordering by the cumulative total instead would put a long-running idle
	// client above the one actually working.
	sort.Slice(clients, func(i, j int) bool {
		if clients[i].BusyPercent != clients[j].BusyPercent {
			return clients[i].BusyPercent > clients[j].BusyPercent
		}
		if clients[i].BusyNs != clients[j].BusyNs {
			return clients[i].BusyNs > clients[j].BusyNs
		}
		return clients[i].PID < clients[j].PID
	})
	return clients, totals
}

// npuEngineRates turns per-engine busy-time totals into percentages of the
// sampling interval, and returns the busiest engine's figure as the device
// utilisation.
//
// The totals are only monotonic while the same clients stay open: a client
// that exits takes its accumulated time with it and the total falls. safeSub
// reports that as no activity rather than as a negative rate, which understates
// the interval in which a client exited and is the safe direction to be wrong.
func npuEngineRates(cur, prev map[string]uint64, hasPrev bool, elapsed time.Duration) ([]types.NPUEngine, float64) {
	if len(cur) == 0 {
		return []types.NPUEngine{}, 0
	}
	engines := make([]types.NPUEngine, 0, len(cur))
	var busiest float64
	for name, busyNs := range cur {
		e := types.NPUEngine{Name: name, BusyNs: busyNs}
		if hasPrev && elapsed > 0 {
			delta := safeSub(busyNs, prev[name])
			e.BusyPercent = clampPercent(float64(delta) / float64(elapsed.Nanoseconds()) * 100)
		}
		if e.BusyPercent > busiest {
			busiest = e.BusyPercent
		}
		engines = append(engines, e)
	}
	sort.Slice(engines, func(i, j int) bool { return engines[i].Name < engines[j].Name })
	return engines, busiest
}

// clampPercent bounds a computed rate to 0-100.
func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// pciIDsOnce guards the one-time parse of the PCI identifier database.
var (
	pciIDsOnce sync.Once
	pciVendors map[string]string
	pciDevices map[string]string
	pciClasses map[string]string
)

// lookupPCINames resolves a PCI vendor, device and class identifier to their
// names using the hwdata database. classHex is the sysfs class attribute
// ("0x120000"); its first byte is the class the database names.
func lookupPCINames(vendorID, deviceID, classHex string) (vendor, device, class string) {
	if vendorID == "" && classHex == "" {
		return "", "", ""
	}
	pciIDsOnce.Do(loadPCIIDs)
	v := strings.TrimPrefix(strings.ToLower(vendorID), "0x")
	d := strings.TrimPrefix(strings.ToLower(deviceID), "0x")
	vendor = pciVendors[v]
	device = pciDevices[v+":"+d]
	if c := strings.TrimPrefix(strings.ToLower(classHex), "0x"); len(c) >= 2 {
		class = pciClasses[c[:2]]
	}
	return vendor, device, class
}

// loadPCIIDs parses the hwdata PCI identifier database once. The file is
// roughly 1.5 MB of tab-indented text; vendors and classes sit at column zero,
// their devices and subclasses one tab in.
func loadPCIIDs() {
	pciVendors = make(map[string]string)
	pciDevices = make(map[string]string)
	pciClasses = make(map[string]string)

	var f *os.File
	var path string
	for _, candidate := range pciIDsPaths {
		opened, err := os.Open(candidate)
		if err == nil {
			f, path = opened, candidate
			break
		}
	}
	if f == nil {
		return
	}
	// Read-only: a close error loses nothing that was read, and there is no
	// caller to hand it to, since the parse runs once behind a sync.Once.
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("npu: close pci.ids", "path", path, "error", err)
		}
	}()

	scanner := bufio.NewScanner(f)
	var vendor string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "C "):
			// "C 12  Processing accelerators"
			id, name, ok := splitPCIEntry(strings.TrimPrefix(line, "C "))
			if ok {
				pciClasses[id] = name
				vendor = ""
			}
		case strings.HasPrefix(line, "\t\t"):
			// Subsystem entry; not used.
		case strings.HasPrefix(line, "\t"):
			if vendor == "" {
				continue
			}
			if id, name, ok := splitPCIEntry(strings.TrimPrefix(line, "\t")); ok {
				pciDevices[vendor+":"+id] = name
			}
		default:
			if id, name, ok := splitPCIEntry(line); ok {
				pciVendors[id] = name
				vendor = id
			}
		}
	}
	// What was parsed before the failure is correct and is kept; names past
	// it resolve to nothing, the same as an identifier the database lacks.
	if err := scanner.Err(); err != nil {
		slog.Warn("npu: pci.ids read stopped early; later PCI names are unresolved", "path", path, "error", err)
	}
}

// splitPCIEntry splits a pci.ids entry into its hex identifier and name.
func splitPCIEntry(line string) (id, name string, ok bool) {
	id, name, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found {
		return "", "", false
	}
	if _, err := strconv.ParseUint(id, 16, 32); err != nil {
		return "", "", false
	}
	return strings.ToLower(id), strings.TrimSpace(name), true
}
