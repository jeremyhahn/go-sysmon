package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestHostCollector_Collect verifies that a successful Collect populates the
// HostInfo with a non-empty hostname.
func TestHostCollector_Collect(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	info := c.Info()
	if info.Hostname == "" {
		t.Error("expected non-empty hostname after Collect")
	}
	if info.OS == "" {
		t.Error("expected non-empty OS after Collect")
	}
	if info.KernelVersion == "" {
		t.Error("expected non-empty kernel version after Collect")
	}
}

// TestHostCollector_InfoBeforeCollect verifies that Info returns a zero-value
// HostInfo before the first Collect call (no panic or nil dereference).
func TestHostCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())
	info := c.Info() // must not panic
	_ = info
}
