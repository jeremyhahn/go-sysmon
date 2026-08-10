package collector

import (
	"os"
	"testing"
	"time"

	psprocess "github.com/shirou/gopsutil/v4/process"
)

// TestCollectDetail_VanishedProcess verifies a process that exits between
// enumeration and inspection yields a usable record rather than a panic or a
// half-populated one. Processes come and go constantly on a busy host, so
// every per-process read has to tolerate the target disappearing.
func TestCollectDetail_VanishedProcess(t *testing.T) {
	c := NewProcessCollector(quietLogger())

	// A PID far above the default pid_max, so nothing can be reading it.
	gone := &psprocess.Process{Pid: 4_194_303}

	nextIO := make(map[int32]processIOSnapshot)
	nextCPU := make(map[int32]processCPUSnapshot)
	detail := c.collectDetail(gone, "sleeping", nextIO, nextCPU)

	if detail.PID != gone.Pid {
		t.Errorf("PID = %d, want %d", detail.PID, gone.Pid)
	}
	if detail.Name != "" || detail.Username != "" {
		t.Errorf("name = %q, user = %q; want both empty for a process that is gone",
			detail.Name, detail.Username)
	}
	if detail.CPUPercent != 0 || detail.MemoryBytes != 0 {
		t.Errorf("cpu = %v, mem = %d; want zeroes", detail.CPUPercent, detail.MemoryBytes)
	}
	if detail.Priority != 0 {
		t.Errorf("Priority = %d, want 0", detail.Priority)
	}
}

// TestCollectDetail_ComputesRatesFromPreviousSample verifies I/O and CPU are
// reported as per-second rates derived from the previous sample, not as raw
// lifetime totals.
func TestCollectDetail_ComputesRatesFromPreviousSample(t *testing.T) {
	self, err := psprocess.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("open own process: %v", err)
	}

	c := NewProcessCollector(quietLogger())

	// A sample taken one second ago, with counters one second behind.
	oneSecondAgo := time.Now().Add(-time.Second)
	c.prevIO[self.Pid] = processIOSnapshot{At: oneSecondAgo}
	c.prevCPU[self.Pid] = processCPUSnapshot{TotalSeconds: 0, At: oneSecondAgo}

	nextIO := make(map[int32]processIOSnapshot)
	nextCPU := make(map[int32]processCPUSnapshot)
	detail := c.collectDetail(self, "running", nextIO, nextCPU)

	if detail.Name == "" {
		t.Error("Name is empty for the running test process")
	}
	if detail.MemoryBytes == 0 {
		t.Error("MemoryBytes is 0 for the running test process")
	}
	if _, ok := nextIO[self.Pid]; !ok {
		t.Error("no I/O snapshot was recorded for the next cycle")
	}
	if _, ok := nextCPU[self.Pid]; !ok {
		t.Error("no CPU snapshot was recorded for the next cycle")
	}
	// The baseline reported zero bytes read, so the rate is this process's
	// entire I/O spread over one second — a positive, finite number.
	if detail.ReadBytesRate == 0 && detail.ReadBytes > 0 {
		t.Error("ReadBytes is non-zero but ReadRate is 0 despite a one-second baseline")
	}
	if detail.CPUPercent < 0 {
		t.Errorf("CPUPercent = %v, want a non-negative rate", detail.CPUPercent)
	}
}

// TestCollectDetail_RecycledPIDReportsNoRate verifies a PID reused by a
// different process does not produce a negative rate. Counters that went
// backwards mean the baseline belongs to something else.
func TestCollectDetail_RecycledPIDReportsNoRate(t *testing.T) {
	self, err := psprocess.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("open own process: %v", err)
	}

	c := NewProcessCollector(quietLogger())

	// Implausibly large baselines, as if left behind by a long-lived process
	// that previously held this PID.
	oneSecondAgo := time.Now().Add(-time.Second)
	c.prevCPU[self.Pid] = processCPUSnapshot{TotalSeconds: 1e9, At: oneSecondAgo}
	c.prevIO[self.Pid] = processIOSnapshot{
		ReadBytes:  1 << 60,
		WriteBytes: 1 << 60,
		At:         oneSecondAgo,
	}

	detail := c.collectDetail(self, "running",
		make(map[int32]processIOSnapshot), make(map[int32]processCPUSnapshot))

	if detail.CPUPercent != 0 {
		t.Errorf("CPUPercent = %v, want 0 when CPU time went backwards", detail.CPUPercent)
	}
	if detail.ReadBytesRate != 0 || detail.WriteBytesRate != 0 {
		t.Errorf("rates = %d read / %d write, want 0 when the counters went backwards",
			detail.ReadBytesRate, detail.WriteBytesRate)
	}
}

// TestCollectDetail_SameInstantSampleReportsNoRate verifies a baseline taken at
// the same instant does not divide by zero.
func TestCollectDetail_SameInstantSampleReportsNoRate(t *testing.T) {
	self, err := psprocess.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("open own process: %v", err)
	}

	c := NewProcessCollector(quietLogger())
	future := time.Now().Add(time.Hour) // guarantees a non-positive elapsed time
	c.prevCPU[self.Pid] = processCPUSnapshot{At: future}
	c.prevIO[self.Pid] = processIOSnapshot{At: future}

	detail := c.collectDetail(self, "running",
		make(map[int32]processIOSnapshot), make(map[int32]processCPUSnapshot))

	if detail.CPUPercent != 0 || detail.ReadBytesRate != 0 || detail.WriteBytesRate != 0 {
		t.Errorf("got cpu=%v read=%d write=%d, want zeroes with no elapsed time",
			detail.CPUPercent, detail.ReadBytesRate, detail.WriteBytesRate)
	}
}
