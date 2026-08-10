package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestLoadCollector_Collect verifies that Collect returns sane load averages.
func TestLoadCollector_Collect(t *testing.T) {
	c := collector.NewLoadCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	avg := c.Info()
	// Load averages must be non-negative.
	if avg.Load1 < 0 || avg.Load5 < 0 || avg.Load15 < 0 {
		t.Errorf("negative load average: load1=%v load5=%v load15=%v",
			avg.Load1, avg.Load5, avg.Load15)
	}
	// At least one of the averages should be non-zero on an active system.
	if avg.Load1 == 0 && avg.Load5 == 0 && avg.Load15 == 0 {
		// This is technically possible on an idle system, so warn rather than fail.
		t.Log("all load averages are zero; system may be completely idle")
	}
}

// TestLoadCollector_InfoBeforeCollect verifies safe zero-value access.
func TestLoadCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewLoadCollector(silentLogger())
	avg := c.Info()
	if avg.Load1 != 0 || avg.Load5 != 0 || avg.Load15 != 0 {
		t.Error("expected all-zero LoadAverage before Collect")
	}
}
