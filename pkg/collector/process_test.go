package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestProcessCollector_Collect verifies that Collect returns a summary with
// a positive Total and that the component counts are individually non-negative.
func TestProcessCollector_Collect(t *testing.T) {
	c := collector.NewProcessCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	s := c.Info()
	if s.Total <= 0 {
		t.Errorf("expected positive Total, got %d", s.Total)
	}
	if s.Running < 0 || s.Sleeping < 0 || s.Stopped < 0 || s.Zombie < 0 {
		t.Errorf("negative state count: running=%d sleeping=%d stopped=%d zombie=%d",
			s.Running, s.Sleeping, s.Stopped, s.Zombie)
	}
}

// TestProcessCollector_InfoBeforeCollect verifies safe zero-value access.
func TestProcessCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewProcessCollector(silentLogger())
	s := c.Info()
	if s.Total != 0 {
		t.Errorf("expected zero Total before Collect, got %d", s.Total)
	}
}
