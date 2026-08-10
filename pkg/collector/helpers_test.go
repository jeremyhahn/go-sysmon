package collector_test

// This file holds additional unit tests for internal helper functions exposed
// indirectly via the collector behaviour, plus table-driven tests for helper
// logic that needs broader coverage.

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestDiskCollector_MultipleCollects verifies that repeated Collect calls do
// not panic and consistently return disks.
func TestDiskCollector_MultipleCollects(t *testing.T) {
	c := collector.NewDiskCollector(silentLogger())
	for i := range 3 {
		if err := c.Collect(); err != nil {
			t.Fatalf("Collect iteration %d returned error: %v", i, err)
		}
		if len(c.Info()) == 0 {
			t.Errorf("Collect iteration %d: expected at least one disk", i)
		}
	}
}

// TestCPUCollector_MultipleCollects verifies repeated Collect calls are safe.
func TestCPUCollector_MultipleCollects(t *testing.T) {
	c := collector.NewCPUCollector(silentLogger())
	for i := range 3 {
		if err := c.Collect(); err != nil {
			t.Fatalf("Collect iteration %d returned error: %v", i, err)
		}
	}
	if len(c.Info()) == 0 {
		t.Error("expected at least one CPU after repeated Collect calls")
	}
}

// TestProcessCollector_StateCountsConsistent verifies that state counts never
// individually exceed Total.
func TestProcessCollector_StateCountsConsistent(t *testing.T) {
	c := collector.NewProcessCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect error: %v", err)
	}
	s := c.Info()
	states := []struct {
		name  string
		count int
	}{
		{"Running", s.Running},
		{"Sleeping", s.Sleeping},
		{"Stopped", s.Stopped},
		{"Zombie", s.Zombie},
	}
	for _, st := range states {
		if st.count > s.Total {
			t.Errorf("%s count %d exceeds Total %d", st.name, st.count, s.Total)
		}
	}
}

// TestNetworkCollector_CounterMonotonicity verifies that absolute counters do
// not decrease between two consecutive collections.
func TestNetworkCollector_CounterMonotonicity(t *testing.T) {
	c := collector.NewNetworkCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect error: %v", err)
	}
	first := c.Info()

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect error: %v", err)
	}
	second := c.Info()

	firstByName := make(map[string]struct{ sent, recv uint64 })
	for _, iface := range first {
		firstByName[iface.Name] = struct{ sent, recv uint64 }{iface.BytesSent, iface.BytesRecv}
	}

	for _, iface := range second {
		prev, ok := firstByName[iface.Name]
		if !ok {
			continue // interface appeared between collects; skip
		}
		if iface.BytesSent < prev.sent {
			t.Errorf("interface %q: BytesSent decreased from %d to %d",
				iface.Name, prev.sent, iface.BytesSent)
		}
		if iface.BytesRecv < prev.recv {
			t.Errorf("interface %q: BytesRecv decreased from %d to %d",
				iface.Name, prev.recv, iface.BytesRecv)
		}
	}
}

// TestLoadCollector_MultipleCollects verifies that repeated Collect calls
// return sane values each time.
func TestLoadCollector_MultipleCollects(t *testing.T) {
	c := collector.NewLoadCollector(silentLogger())
	for i := range 3 {
		if err := c.Collect(); err != nil {
			t.Fatalf("Collect iteration %d error: %v", i, err)
		}
		avg := c.Info()
		if avg.Load1 < 0 || avg.Load5 < 0 || avg.Load15 < 0 {
			t.Errorf("iteration %d: negative load average", i)
		}
	}
}

// TestMemoryCollector_SwapValuesConsistent checks that swap values are
// internally consistent (UsedBytes + FreeBytes <= TotalBytes with tolerance).
func TestMemoryCollector_SwapValuesConsistent(t *testing.T) {
	c := collector.NewMemoryCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect error: %v", err)
	}
	info := c.Info()
	// Swap may be 0 if not configured; skip check in that case.
	if info.SwapTotalBytes == 0 {
		return
	}
	if info.SwapUsedBytes > info.SwapTotalBytes {
		t.Errorf("SwapUsedBytes %d > SwapTotalBytes %d",
			info.SwapUsedBytes, info.SwapTotalBytes)
	}
	if info.SwapPercent < 0 || info.SwapPercent > 100 {
		t.Errorf("SwapPercent %v outside [0,100]", info.SwapPercent)
	}
}

// TestDiskCollector_SmartFieldsValid checks SMART attribute fields when present.
func TestDiskCollector_SmartFieldsValid(t *testing.T) {
	c := collector.NewDiskCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect error: %v", err)
	}
	for _, d := range c.Info() {
		if d.Temperature < 0 {
			t.Errorf("disk %q: negative temperature %v", d.Name, d.Temperature)
		}
		for j, attr := range d.SMARTAttrs {
			if attr.Value < 0 {
				t.Errorf("disk %q attr[%d] %q: negative value", d.Name, j, attr.Name)
			}
		}
	}
}

// TestHostCollector_UptimeSane verifies that uptime is positive after collect.
func TestHostCollector_UptimeSane(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect error: %v", err)
	}
	info := c.Info()
	if info.Uptime == 0 {
		t.Error("expected non-zero Uptime after Collect")
	}
	if info.BootTime == 0 {
		t.Error("expected non-zero BootTime after Collect")
	}
}
