package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestNetworkCollector_Collect verifies that Collect returns at least one
// interface and that the loopback interface is present.
func TestNetworkCollector_Collect(t *testing.T) {
	c := collector.NewNetworkCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	ifaces := c.Info()
	if len(ifaces) == 0 {
		t.Fatal("expected at least one NetworkInfo after Collect")
	}
	var foundLoopback bool
	for _, iface := range ifaces {
		if iface.Name == "" {
			t.Error("interface has empty name")
		}
		if iface.IsLoopback {
			foundLoopback = true
		}
	}
	if !foundLoopback {
		t.Error("expected to find a loopback interface")
	}
}

// TestNetworkCollector_RateCalculation verifies that a second Collect produces
// non-negative rates.
func TestNetworkCollector_RateCalculation(t *testing.T) {
	c := collector.NewNetworkCollector(silentLogger())
	// First collection seeds the prev snapshot.
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect error: %v", err)
	}
	// Second collection should compute rates.
	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect error: %v", err)
	}
	for _, iface := range c.Info() {
		if iface.BytesSentRate < 0 {
			t.Errorf("interface %q: negative BytesSentRate %v", iface.Name, iface.BytesSentRate)
		}
		if iface.BytesRecvRate < 0 {
			t.Errorf("interface %q: negative BytesRecvRate %v", iface.Name, iface.BytesRecvRate)
		}
	}
}

// TestNetworkCollector_InfoBeforeCollect verifies safe zero-value access.
func TestNetworkCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewNetworkCollector(silentLogger())
	ifaces := c.Info()
	if ifaces == nil {
		t.Error("Info() must return non-nil slice before Collect")
	}
}
