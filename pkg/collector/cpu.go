package collector

import (
	"bufio"
	"errors"
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
//
// Usage is the delta between consecutive /proc/stat reads, with no blocking
// sleep, so the first call has no usage to report: every core is marked
// UsageAvailable false until a second reading exists. A /proc/stat that cannot
// be read, or a core whose line in it is malformed, leaves those cores' usage
// unknown for this interval and the next; the CPU info is still published, and
// the failure is returned as a *types.CollectorError.
func (c *CPUCollector) Collect() error {
	infos, err := pscpu.Info()
	if err != nil {
		return &types.CollectorError{Collector: "cpu", Cause: err}
	}

	summary := c.buildSummary()

	// A failed read yields no readings at all, and a partial one yields the
	// cores it could parse. Either way the reading replaces the previous one,
	// so a core absent from it has no baseline for the next interval either:
	// its next delta would otherwise span two intervals and be reported as one.
	currentStat, statErr := readProcStat()
	var prev map[int]cpuTimes
	if p := c.prevStat.Load(); p != nil {
		prev = *p
	}
	usageByIndex := cpuUsage(prev, currentStat)
	c.prevStat.Store(&currentStat)

	result := make([]types.CPUInfo, 0, len(infos))
	for _, stat := range infos {
		idx := int(stat.CPU)
		usage, measured := usageByIndex[idx]
		info := types.CPUInfo{
			Index:          idx,
			ModelName:      stat.ModelName,
			VendorID:       stat.VendorID,
			Family:         stat.Family,
			Model:          stat.Model,
			Stepping:       stat.Stepping,
			PhysicalID:     stat.PhysicalID,
			CoreID:         stat.CoreID,
			Cores:          int32(summary.TotalCores),
			Threads:        int32(summary.TotalThreads),
			Mhz:            stat.Mhz,
			CacheSize:      stat.CacheSize,
			Microcode:      stat.Microcode,
			Flags:          stat.Flags,
			UsagePercent:   usage,
			UsageAvailable: measured,
		}
		result = append(result, info)
	}

	c.summary.Store(&summary)
	c.cpus.Store(&result)
	if statErr != nil {
		return &types.CollectorError{Collector: "cpu", Cause: statErr}
	}
	return nil
}

// cpuUsage returns each core's busy percentage between two /proc/stat
// readings, keyed by CPU index. A core is present in the result only when its
// usage was measured: it has a line in both readings, and its counters moved
// forward between them. A core whose total did not advance had no interval to
// measure, and one whose counters went backwards was reset or replugged; for
// either, any number would be invented. prev is nil on the first collection,
// which therefore measures nothing.
func cpuUsage(prev, cur map[int]cpuTimes) map[int]float64 {
	usage := make(map[int]float64, len(cur))
	for idx, now := range cur {
		old, ok := prev[idx]
		if !ok {
			continue
		}
		if now.total() <= old.total() || now.busy() < old.busy() {
			continue
		}
		deltaTotal := now.total() - old.total()
		deltaBusy := now.busy() - old.busy()
		usage[idx] = float64(deltaBusy) / float64(deltaTotal) * 100.0
	}
	return usage
}

// procStatPath is the source of per-CPU jiffies. It is a variable so tests can
// point the reader at a synthetic file.
var procStatPath = "/proc/stat"

// procStatFields names the per-CPU counters of a /proc/stat line, in order.
// The first seven have been there since Linux 2.6; steal, the eighth, since
// 2.6.11, and a line without it is read with steal at zero, which is what the
// kernel means by omitting it. The guest counters that follow are already
// included in user and nice, and are not read.
var procStatFields = []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

// procStatRequired is how many of procStatFields every line must carry.
const procStatRequired = 7

// readProcStat parses procStatPath and returns per-CPU jiffies keyed by CPU
// index.
//
// A cpuN line that cannot be parsed is left out of the result, so that core's
// usage is unknown rather than computed from counters read as zero, and is
// reported as a *types.ProcStatLineError; every such line is reported, joined.
// The lines that did parse are still returned alongside the error. A file that
// cannot be read at all returns no readings and the read error.
func readProcStat() (map[int]cpuTimes, error) {
	f, err := os.Open(procStatPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close /proc/stat", "path", procStatPath, "err", err)
		}
	}()

	result := make(map[int]cpuTimes)
	var lineErrs []error
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Only "cpuN" lines; the aggregate "cpu" line and every other
		// statistic in the file are someone else's.
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") || fields[0] == "cpu" {
			continue
		}
		idx, t, err := parseProcStatLine(fields)
		if err != nil {
			lineErrs = append(lineErrs, &types.ProcStatLineError{Path: procStatPath, CPU: fields[0], Cause: err})
			continue
		}
		result[idx] = t
	}
	if err := scanner.Err(); err != nil {
		lineErrs = append(lineErrs, err)
	}
	return result, errors.Join(lineErrs...)
}

// parseProcStatLine parses the fields of one "cpuN" line of /proc/stat: the
// CPU's index and its counters. Every counter it needs must be present and a
// valid unsigned integer; there is no partial result.
func parseProcStatLine(fields []string) (int, cpuTimes, error) {
	idx, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
	if err != nil {
		return 0, cpuTimes{}, err
	}
	values := fields[1:]
	if len(values) < procStatRequired {
		return 0, cpuTimes{}, &types.ProcStatusFieldError{Path: procStatPath, Field: procStatFields[len(values)]}
	}
	var counters [8]uint64
	for i := range min(len(values), len(procStatFields)) {
		if counters[i], err = strconv.ParseUint(values[i], 10, 64); err != nil {
			return 0, cpuTimes{}, err
		}
	}
	return idx, cpuTimes{
		user:    counters[0],
		nice:    counters[1],
		system:  counters[2],
		idle:    counters[3],
		iowait:  counters[4],
		irq:     counters[5],
		softirq: counters[6],
		steal:   counters[7],
	}, nil
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
