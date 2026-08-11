// Package collector implements system metrics collectors for go-sysmon.
// Each sub-collector is responsible for a specific subsystem (CPU, memory,
// disk, network, etc.).  The top-level SystemCollector aggregates all
// sub-collectors into a single types.Snapshot.
package collector

import (
	"context"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Collection frequency tiers. Collectors in the fast tier run every tick,
// medium-tier collectors run every mediumInterval ticks, and slow-tier
// collectors run every slowInterval ticks. Sub-collectors cache their
// last result via atomic pointers, so skipped ticks still return stale
// (but valid) data in the snapshot.
const (
	mediumInterval = 5
	slowInterval   = 30
)

// collectorTier classifies how often a collector runs.
type collectorTier int

const (
	tierFast   collectorTier = iota // every tick
	tierMedium                      // every mediumInterval ticks
	tierSlow                        // every slowInterval ticks
)

// Collector refreshes a specific category of system metrics.
type Collector interface {
	Collect() error
}

// SystemCollector aggregates all sub-collectors and produces a
// complete types.Snapshot on each call to Snapshot.
type SystemCollector struct {
	host    *HostCollector
	cpu     *CPUCollector
	gpu     *GPUCollector
	memory  *MemoryCollector
	disk    *DiskCollector
	network *NetworkCollector
	process *ProcessCollector
	load    *LoadCollector
	sensor  *SensorCollector
	virt    *VirtCollector
	runtime *RuntimeCollector
	logger  *slog.Logger

	// tick is the monotonic snapshot counter used for frequency tiering.
	tick atomic.Uint64

	// tiering controls whether frequency tiering is active. When false,
	// all collectors run on every Snapshot call.
	tiering atomic.Bool
}

// NewSystemCollector creates a SystemCollector with all sub-collectors
// initialised and ready to use.
func NewSystemCollector(logger *slog.Logger) *SystemCollector {
	if logger == nil {
		logger = slog.Default()
	}
	sc := &SystemCollector{
		host:    NewHostCollector(logger),
		cpu:     NewCPUCollector(logger),
		gpu:     NewGPUCollector(logger),
		memory:  NewMemoryCollector(logger),
		disk:    NewDiskCollector(logger),
		network: NewNetworkCollector(logger),
		process: NewProcessCollector(logger),
		load:    NewLoadCollector(logger),
		sensor:  NewSensorCollector(logger),
		virt:    NewVirtCollector(logger),
		runtime: NewRuntimeCollector(logger),
		logger:  logger,
	}
	sc.tiering.Store(true) // enabled by default for streaming mode
	return sc
}

// SetTiering enables or disables frequency tiering. When disabled, all
// collectors run on every Snapshot call regardless of tier.
func (s *SystemCollector) SetTiering(enabled bool) {
	s.tiering.Store(enabled)
}

// shouldCollect returns true when the collector at the given tier is due
// to run on the current tick.
func shouldCollect(tick uint64, tier collectorTier) bool {
	switch tier {
	case tierMedium:
		return tick%mediumInterval == 0
	case tierSlow:
		return tick%slowInterval == 0
	default:
		return true
	}
}

// Snapshot collects subsystem metrics and returns a complete Snapshot.
// Expensive collectors are tiered so they only run every N ticks.
// Skipped collectors still contribute their most recently cached data
// to the snapshot, keeping the output structure complete on every call.
// Errors from individual sub-collectors are logged as warnings;
// collection continues even when a single subsystem fails.
func (s *SystemCollector) Snapshot(ctx context.Context) (types.Snapshot, error) {
	snap := types.Snapshot{Timestamp: time.Now()}

	// tick starts at 0 so that the first snapshot runs all collectors.
	tick := s.tick.Add(1) - 1

	// gpuCollect wraps GPUCollector.Collect to match the func() error signature.
	gpuCollect := func() error { return s.gpu.Collect(ctx) }

	collectors := []struct {
		name string
		fn   func() error
		tier collectorTier
	}{
		// Fast tier: lightweight counters that change rapidly.
		{"host", s.host.Collect, tierFast},
		{"cpu", s.cpu.Collect, tierFast},
		{"memory", s.memory.Collect, tierFast},
		{"network", s.network.Collect, tierFast},
		{"load", s.load.Collect, tierFast},

		// Medium tier: heavier collection that doesn't need per-second updates.
		{"gpu", gpuCollect, tierMedium},
		{"process", s.process.Collect, tierMedium},
		{"sensor", s.sensor.Collect, tierMedium},
		{"virt", s.virt.Collect, tierMedium},
		{"runtime", s.runtime.Collect, tierSlow},

		// Slow tier: expensive I/O (SMART ioctls, DIMM temp module probing).
		{"disk", s.disk.Collect, tierSlow},
	}

	for _, c := range collectors {
		select {
		case <-ctx.Done():
			return snap, &types.CollectorError{Collector: "system", Cause: ctx.Err()}
		default:
		}
		if s.tiering.Load() && !shouldCollect(tick, c.tier) {
			continue
		}
		if err := c.fn(); err != nil {
			s.logger.WarnContext(ctx, "collector warning", "collector", c.name, "error", err)
		}
	}

	// Always read the latest cached data from every collector,
	// even those that were skipped on this tick.
	snap.Host = s.host.Info()
	snap.CPUSummary = s.cpu.Summary()
	snap.CPUs = s.cpu.Info()
	snap.GPUs = s.gpu.Info()
	snap.Memory = s.memory.Info()
	snap.Disks = s.disk.Info()
	snap.Networks = s.network.Info()
	snap.Processes = s.process.Info()
	snap.LoadAvg = s.load.Info()
	snap.Sensors = s.sensor.Info()
	snap.Virt = s.virt.Info()
	snap.Virt.Runtime = s.runtime.Info()
	snap.Virt.Capability.RuntimeAPIReachable = snap.Virt.Runtime.Available
	if !snap.Virt.Capability.RuntimeAPIReachable {
		snap.Virt.Capability.Notes = append(snap.Virt.Capability.Notes,
			"no container runtime socket answered; image inventory is unavailable. Check that the daemon is running and that this user is in the docker group")
	}

	// Attribute tap-device throughput to the guest behind it. This runs after
	// both sections are assembled because it joins across them.
	MergeNetworkIntoVMs(snap.Virt.VMs, snap.Networks)

	// Merge per-core temperatures into CPUInfo entries.
	mergeSensorIntoCPUs(snap.CPUs, snap.Sensors)

	return snap, nil
}

// mergeSensorIntoCPUs populates CPUInfo.TemperatureCelsius and VoltageV
// from sensor data by matching (PackageID, CoreID) to (PhysicalID, CoreID).
func mergeSensorIntoCPUs(cpus []types.CPUInfo, sensors types.SensorData) {
	// Build temperature lookup: "packageID:coreID" → temp.
	tempMap := make(map[string]float64, len(sensors.CoreTemps))
	for _, ct := range sensors.CoreTemps {
		key := strconv.Itoa(ct.PackageID) + ":" + strconv.Itoa(ct.CoreID)
		tempMap[key] = ct.TempCelsius
	}

	// Build voltage lookup: first "Vcore" or "VID" voltage per hwmon.
	var vcoreV float64
	for _, cv := range sensors.CoreVoltages {
		if cv.Label == "Vcore" || cv.Label == "VID" {
			vcoreV = cv.VoltageV
			break
		}
	}

	for i := range cpus {
		key := cpus[i].PhysicalID + ":" + cpus[i].CoreID
		if temp, ok := tempMap[key]; ok {
			cpus[i].TemperatureCelsius = temp
		}
		if vcoreV > 0 {
			cpus[i].VoltageV = vcoreV
		}
	}
}

// WaitForRuntimeDiskUsage blocks until the container runtime's disk usage has
// been collected or ctx is done. The query takes many seconds, so it runs in
// the background; one-shot commands call this to avoid printing empty sizes.
func (s *SystemCollector) WaitForRuntimeDiskUsage(ctx context.Context) {
	s.runtime.WaitForDiskUsage(ctx)
}
