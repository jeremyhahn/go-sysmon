package collector

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	psprocess "github.com/shirou/gopsutil/v4/process"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// processIOSnapshot records disk I/O byte totals for a single process along
// with the instant they were sampled, so that rates can be expressed per
// second rather than per polling interval.
type processIOSnapshot struct {
	ReadBytes  uint64
	WriteBytes uint64
	At         time.Time
}

// processCPUSnapshot records a process's cumulative CPU time and the wall-clock
// instant it was sampled, enabling delta-based CPU percentage computation.
type processCPUSnapshot struct {
	TotalSeconds float64
	At           time.Time
}

// ProcessCollector collects a summary of process states.
type ProcessCollector struct {
	summary atomic.Pointer[types.ProcessSummary]
	prevIO  map[int32]processIOSnapshot
	prevCPU map[int32]processCPUSnapshot
	logger  *slog.Logger
}

// NewProcessCollector returns a new ProcessCollector.
func NewProcessCollector(logger *slog.Logger) *ProcessCollector {
	c := &ProcessCollector{
		logger:  logger,
		prevIO:  make(map[int32]processIOSnapshot),
		prevCPU: make(map[int32]processCPUSnapshot),
	}
	c.summary.Store(&types.ProcessSummary{})
	return c
}

// Collect refreshes process state counts and per-process details.
func (c *ProcessCollector) Collect() error {
	procs, err := psprocess.Processes()
	if err != nil {
		return &types.CollectorError{Collector: "process", Cause: err}
	}

	summary := &types.ProcessSummary{
		Total:     len(procs),
		Processes: make([]types.ProcessDetail, 0, len(procs)),
	}

	nextIO := make(map[int32]processIOSnapshot, len(procs))
	nextCPU := make(map[int32]processCPUSnapshot, len(procs))

	for _, p := range procs {
		statuses, serr := p.Status()
		if serr != nil || len(statuses) == 0 {
			continue
		}

		// gopsutil returns a slice of lowercase status strings; use the first.
		status := strings.ToLower(statuses[0])
		switch status {
		case psprocess.Running, "r":
			summary.Running++
		case psprocess.Idle, "i":
			// State I is an idle kernel thread. Counting these as sleeping
			// hides several hundred threads inside that bucket on a typical
			// system, so they are reported separately.
			summary.Idle++
		case psprocess.Sleep, "s", "blocked", "d":
			summary.Sleeping++
		case psprocess.Stop, "t":
			summary.Stopped++
		case psprocess.Zombie, "z":
			summary.Zombie++
		}

		detail := c.collectDetail(p, status, nextIO, nextCPU)
		summary.Processes = append(summary.Processes, detail)
	}

	c.prevIO = nextIO
	c.prevCPU = nextCPU
	c.summary.Store(summary)
	return nil
}

// collectDetail gathers per-process metrics and records I/O and CPU snapshots
// for rate computation on the next collection cycle.
func (c *ProcessCollector) collectDetail(
	p *psprocess.Process,
	status string,
	nextIO map[int32]processIOSnapshot,
	nextCPU map[int32]processCPUSnapshot,
) types.ProcessDetail {
	pid := p.Pid
	now := time.Now()

	name, err := p.Name()
	if err != nil {
		name = ""
	}

	username, err := p.Username()
	if err != nil {
		username = ""
	}

	// Compute delta-based CPU percentage using raw kernel CPU times.
	// p.CPUPercent() returns a lifetime average which trends toward 0% for
	// long-running processes; the delta approach matches what tools like top show.
	var cpuPercent float64
	if times, terr := p.Times(); terr == nil {
		currentTotal := times.User + times.System
		snap := processCPUSnapshot{TotalSeconds: currentTotal, At: now}
		nextCPU[pid] = snap

		if prev, ok := c.prevCPU[pid]; ok {
			elapsed := now.Sub(prev.At).Seconds()
			// A CPU time that went backwards means this PID was recycled onto a
			// different process between samples. Reporting the difference would
			// yield a negative percentage, so treat it as a fresh process and
			// wait for the next tick to produce a real delta.
			if elapsed > 0 && currentTotal >= prev.TotalSeconds {
				cpuPercent = ((currentTotal - prev.TotalSeconds) / elapsed) * 100.0
			}
		}
	}

	var memBytes uint64
	if mi, err := p.MemoryInfo(); err == nil && mi != nil {
		memBytes = mi.RSS
	}

	var readBytes, writeBytes uint64
	if io, err := p.IOCounters(); err == nil && io != nil {
		readBytes = io.ReadBytes
		writeBytes = io.WriteBytes
	}

	// Rates are bytes per second, not bytes per polling interval: the interval
	// is user-configurable (500ms to 60s), so a raw delta would silently change
	// meaning whenever the stream rate changes. A process seen for the first
	// time has no baseline and reports 0 rather than its entire lifetime I/O.
	var readRate, writeRate uint64
	if prev, ok := c.prevIO[pid]; ok {
		elapsed := now.Sub(prev.At).Seconds()
		if elapsed > 0 {
			if readBytes >= prev.ReadBytes {
				readRate = uint64(float64(readBytes-prev.ReadBytes) / elapsed)
			}
			if writeBytes >= prev.WriteBytes {
				writeRate = uint64(float64(writeBytes-prev.WriteBytes) / elapsed)
			}
		}
	}

	nextIO[pid] = processIOSnapshot{
		ReadBytes:  readBytes,
		WriteBytes: writeBytes,
		At:         now,
	}

	priority, err := p.Nice()
	if err != nil {
		priority = 0
	}

	return types.ProcessDetail{
		PID:            pid,
		Name:           name,
		Username:       username,
		CPUPercent:     cpuPercent,
		MemoryBytes:    memBytes,
		ReadBytes:      readBytes,
		WriteBytes:     writeBytes,
		ReadBytesRate:  readRate,
		WriteBytesRate: writeRate,
		Priority:       priority,
		Status:         status,
	}
}

// Info returns the most recently collected ProcessSummary.
func (c *ProcessCollector) Info() types.ProcessSummary {
	return *c.summary.Load()
}
