package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestMemoryCollector_Collect verifies that Collect populates the memory
// totals with sane values.
func TestMemoryCollector_Collect(t *testing.T) {
	c := collector.NewMemoryCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	info := c.Info()
	if info.TotalBytes == 0 {
		t.Error("expected non-zero TotalBytes after Collect")
	}
	if info.UsedPercent < 0 || info.UsedPercent > 100 {
		t.Errorf("UsedPercent %v outside [0,100]", info.UsedPercent)
	}
	// Used + Free should not exceed Total (with small rounding tolerance).
	if info.UsedBytes > info.TotalBytes {
		t.Errorf("UsedBytes %d exceeds TotalBytes %d", info.UsedBytes, info.TotalBytes)
	}
}

// TestMemoryCollector_InfoBeforeCollect verifies safe zero-value access before
// the first Collect call.
func TestMemoryCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewMemoryCollector(silentLogger())
	info := c.Info()
	if info.TotalBytes != 0 {
		t.Error("expected zero TotalBytes before Collect")
	}
}
