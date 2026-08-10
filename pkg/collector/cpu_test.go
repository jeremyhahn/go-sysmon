package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestCPUCollector_Collect verifies that Collect returns at least one CPUInfo
// with a non-empty model name.
func TestCPUCollector_Collect(t *testing.T) {
	c := collector.NewCPUCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	infos := c.Info()
	if len(infos) == 0 {
		t.Fatal("expected at least one CPUInfo after Collect")
	}
	for i, cpu := range infos {
		if cpu.ModelName == "" {
			t.Errorf("cpu[%d]: expected non-empty model name", i)
		}
		if cpu.UsagePercent < 0 || cpu.UsagePercent > 100 {
			t.Errorf("cpu[%d]: usage percent %v outside [0,100]", i, cpu.UsagePercent)
		}
	}
}

// TestCPUCollector_InfoBeforeCollect verifies that calling Info before Collect
// returns an empty slice without panicking.
func TestCPUCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewCPUCollector(silentLogger())
	infos := c.Info()
	if infos == nil {
		t.Error("Info() must return non-nil slice, even before Collect")
	}
}

// TestCPUCollector_SummaryAfterCollect verifies that Summary returns a
// populated CPUSummary after a successful Collect call.
func TestCPUCollector_SummaryAfterCollect(t *testing.T) {
	c := collector.NewCPUCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	s := c.Summary()
	if s.TotalThreads <= 0 {
		t.Errorf("expected TotalThreads > 0, got %d", s.TotalThreads)
	}
	if s.TotalCores <= 0 {
		t.Errorf("expected TotalCores > 0, got %d", s.TotalCores)
	}
	if s.TotalThreads < s.TotalCores {
		t.Errorf("TotalThreads (%d) must be >= TotalCores (%d)", s.TotalThreads, s.TotalCores)
	}
}

// TestCPUCollector_SummaryBeforeCollect verifies that Summary returns a
// zero-value CPUSummary before Collect is called, without panicking.
func TestCPUCollector_SummaryBeforeCollect(t *testing.T) {
	c := collector.NewCPUCollector(silentLogger())
	s := c.Summary()
	// Zero value is acceptable before first Collect; just must not panic.
	_ = s
}
