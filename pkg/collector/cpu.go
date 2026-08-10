package collector

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	pscpu "github.com/shirou/gopsutil/v4/cpu"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// cpuTimes holds the raw jiffies for a single CPU from /proc/stat.
type cpuTimes struct {
	user    uint64
	nice    uint64
	system  uint64
	idle    uint64
	iowait  uint64
	irq     uint64
	softirq uint64
	steal   uint64
}

// total returns the sum of all CPU time fields.
func (t cpuTimes) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

// busy returns the sum of all non-idle CPU time fields.
func (t cpuTimes) busy() uint64 {
	return t.user + t.nice + t.system + t.irq + t.softirq + t.steal
}

// CPUCollector collects per-CPU information and usage percentages.
type CPUCollector struct {
	cpus     atomic.Pointer[[]types.CPUInfo]
	summary  atomic.Pointer[types.CPUSummary]
	prevStat atomic.Pointer[map[int]cpuTimes]
	logger   *slog.Logger
}

// NewCPUCollector returns a new CPUCollector.
func NewCPUCollector(logger *slog.Logger) *CPUCollector {
	c := &CPUCollector{logger: logger}
	c.cpus.Store(&[]types.CPUInfo{})
	c.summary.Store(&types.CPUSummary{})
	return c
}

// Collect refreshes per-CPU info and usage percentages.
// Usage is computed as the delta between consecutive /proc/stat reads,
// with no blocking sleep. On the first call, usage reflects time since boot.
func (c *CPUCollector) Collect() error {
	infos, err := pscpu.Info()
	if err != nil {
		return &types.CollectorError{Collector: "cpu", Cause: err}
	}

	summary := c.buildSummary()

	// Read current /proc/stat and compute per-CPU usage from delta.
	currentStat := readProcStat()
	prevPtr := c.prevStat.Load()

	usageByIndex := make(map[int]float64, len(currentStat))
	if prevPtr != nil {
		prev := *prevPtr
		for idx, cur := range currentStat {
			if old, ok := prev[idx]; ok {
				deltaTotal := cur.total() - old.total()
				deltaBusy := cur.busy() - old.busy()
				if deltaTotal > 0 {
					usageByIndex[idx] = float64(deltaBusy) / float64(deltaTotal) * 100.0
				}
			}
		}
	}
	c.prevStat.Store(&currentStat)

	result := make([]types.CPUInfo, 0, len(infos))
	for _, stat := range infos {
		idx := int(stat.CPU)
		info := types.CPUInfo{
			Index:        idx,
			ModelName:    stat.ModelName,
			VendorID:     stat.VendorID,
			Family:       stat.Family,
			Model:        stat.Model,
			Stepping:     stat.Stepping,
			PhysicalID:   stat.PhysicalID,
			CoreID:       stat.CoreID,
			Cores:        int32(summary.TotalCores),
			Threads:      int32(summary.TotalThreads),
			Mhz:          stat.Mhz,
			CacheSize:    stat.CacheSize,
			Microcode:    stat.Microcode,
			Flags:        stat.Flags,
			UsagePercent: usageByIndex[idx],
		}
		result = append(result, info)
	}

	c.summary.Store(&summary)
	c.cpus.Store(&result)
	return nil
}

// readProcStat parses /proc/stat and returns per-CPU jiffies keyed by CPU index.
func readProcStat() map[int]cpuTimes {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close /proc/stat", "err", err)
		}
	}()

	result := make(map[int]cpuTimes)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Match "cpuN" lines (skip aggregate "cpu" line).
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		name := fields[0]
		if name == "cpu" {
			continue // aggregate line
		}
		idx, err := strconv.Atoi(name[3:])
		if err != nil {
			continue
		}
		var t cpuTimes
		t.user, _ = strconv.ParseUint(fields[1], 10, 64)
		t.nice, _ = strconv.ParseUint(fields[2], 10, 64)
		t.system, _ = strconv.ParseUint(fields[3], 10, 64)
		t.idle, _ = strconv.ParseUint(fields[4], 10, 64)
		if len(fields) > 5 {
			t.iowait, _ = strconv.ParseUint(fields[5], 10, 64)
		}
		if len(fields) > 6 {
			t.irq, _ = strconv.ParseUint(fields[6], 10, 64)
		}
		if len(fields) > 7 {
			t.softirq, _ = strconv.ParseUint(fields[7], 10, 64)
		}
		if len(fields) > 8 {
			t.steal, _ = strconv.ParseUint(fields[8], 10, 64)
		}
		result[idx] = t
	}
	return result
}

// Info returns the most recently collected CPU info slice.
func (c *CPUCollector) Info() []types.CPUInfo {
	return *c.cpus.Load()
}

// Summary returns the most recently computed CPU topology summary.
func (c *CPUCollector) Summary() types.CPUSummary {
	return *c.summary.Load()
}

// buildSummary computes the CPUSummary from sysfs, falling back to gopsutil
// counts when sysfs topology data is unavailable.
func (c *CPUCollector) buildSummary() types.CPUSummary {
	summary, err := summaryFromSysfs()
	if err != nil {
		warnOnce(c.logger, "cpu:topology", "cpu: sysfs topology unavailable, falling back to gopsutil counts", "error", err)
		return summaryFromGopsutil(c.logger)
	}
	return summary
}

// cpuSysfsBase is the sysfs CPU root. It is a variable so tests can point the
// topology reader at a synthetic tree.
var cpuSysfsBase = "/sys/devices/system/cpu"

// procCPUInfoPath is the source for the frequency fallback, likewise
// injectable for tests.
var procCPUInfoPath = "/proc/cpuinfo"

// summaryFromSysfs reads CPU topology and frequency from sysfs and /proc/cpuinfo.
func summaryFromSysfs() (types.CPUSummary, error) {
	cpuBase := cpuSysfsBase

	entries, err := os.ReadDir(cpuBase)
	if err != nil {
		return types.CPUSummary{}, &types.SysfsReadError{Path: cpuBase, Cause: err}
	}

	// Track unique sockets and unique (socket, core) pairs.
	sockets := make(map[int]struct{})
	cores := make(map[[2]int]struct{})
	logicalCount := 0

	for _, e := range entries {
		name := e.Name()
		// Only process cpu0, cpu1, … directories.
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		suffix := name[len("cpu"):]
		if _, convErr := strconv.Atoi(suffix); convErr != nil {
			continue
		}
		logicalCount++

		topoDir := filepath.Join(cpuBase, name, "topology")
		pkgID := readSysfsInt(filepath.Join(topoDir, "physical_package_id"))
		coreID := readSysfsInt(filepath.Join(topoDir, "core_id"))

		sockets[pkgID] = struct{}{}
		cores[[2]int{pkgID, coreID}] = struct{}{}
	}

	numSockets := len(sockets)
	numCores := len(cores)

	if numSockets <= 0 || numCores <= 0 || logicalCount <= 0 {
		return types.CPUSummary{}, &types.SysfsTopologyError{
			Reason: fmt.Sprintf("invalid topology: sockets=%d cores=%d logical=%d", numSockets, numCores, logicalCount),
		}
	}

	coresPerSocket := numCores / numSockets
	if coresPerSocket <= 0 {
		coresPerSocket = 1
	}
	threadsPerCore := logicalCount / numCores
	if threadsPerCore <= 0 {
		threadsPerCore = 1
	}

	maxMHz := readCPUFreqMHz(filepath.Join(cpuBase, "cpu0", "cpufreq", "cpuinfo_max_freq"))
	minMHz := readCPUFreqMHz(filepath.Join(cpuBase, "cpu0", "cpufreq", "cpuinfo_min_freq"))

	// Fall back to /proc/cpuinfo "cpu MHz" when cpufreq is absent.
	if maxMHz == 0 {
		maxMHz = readProcCPUInfoMHz()
	}

	return types.CPUSummary{
		Sockets:        numSockets,
		CoresPerSocket: coresPerSocket,
		ThreadsPerCore: threadsPerCore,
		TotalCores:     numCores,
		TotalThreads:   logicalCount,
		MaxMHz:         maxMHz,
		MinMHz:         minMHz,
	}, nil
}

// readSysfsInt reads a single integer from a sysfs file.
// Returns 0 if the file is missing or unparseable.
func readSysfsInt(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return v
}

// readCPUFreqMHz reads a cpufreq file that contains a frequency in kHz and
// returns the value converted to MHz. Returns 0 on any error.
func readCPUFreqMHz(path string) float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	khz, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil || khz <= 0 {
		return 0
	}
	return khz / 1000.0
}

// readProcCPUInfoMHz parses /proc/cpuinfo for the first "cpu MHz" entry.
// Returns 0 when the field is absent or unparseable.
func readProcCPUInfoMHz() float64 {
	raw, err := os.ReadFile(procCPUInfoPath)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "cpu MHz") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64)
		if err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// summaryFromGopsutil builds a best-effort CPUSummary using gopsutil counts.
func summaryFromGopsutil(logger *slog.Logger) types.CPUSummary {
	logical, lerr := pscpu.Counts(true)
	physical, perr := pscpu.Counts(false)
	if lerr != nil || perr != nil || physical <= 0 {
		if lerr != nil {
			warnOnce(logger, "cpu:logical-count", "cpu: gopsutil logical count failed", "error", lerr)
		}
		if perr != nil {
			warnOnce(logger, "cpu:physical-count", "cpu: gopsutil physical count failed", "error", perr)
		}
		return types.CPUSummary{}
	}
	threadsPerCore := 1
	if physical > 0 {
		threadsPerCore = logical / physical
	}
	return types.CPUSummary{
		Sockets:        1,
		CoresPerSocket: physical,
		ThreadsPerCore: threadsPerCore,
		TotalCores:     physical,
		TotalThreads:   logical,
	}
}
