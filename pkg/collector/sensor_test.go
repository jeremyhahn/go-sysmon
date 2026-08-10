package collector_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestSensorCollector_NewSensorCollector verifies that NewSensorCollector
// returns a non-nil collector and that Info() yields a zero-value SensorData
// before any collection has taken place.
func TestSensorCollector_NewSensorCollector(t *testing.T) {
	t.Parallel()
	sc := collector.NewSensorCollector(silentLogger())
	if sc == nil {
		t.Fatal("NewSensorCollector returned nil")
	}
	data := sc.Info()
	// A zero-value SensorData has no entries in any slice field.
	if len(data.CoreTemps) != 0 {
		t.Errorf("expected zero CoreTemps before Collect, got %d", len(data.CoreTemps))
	}
	if len(data.ThermalZones) != 0 {
		t.Errorf("expected zero ThermalZones before Collect, got %d", len(data.ThermalZones))
	}
}

// TestSensorCollector_Collect verifies that Collect returns no error and that
// Info returns a valid SensorData after collection.
func TestSensorCollector_Collect(t *testing.T) {
	t.Parallel()
	sc := collector.NewSensorCollector(silentLogger())
	if err := sc.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	// Info must be callable without panic after a successful Collect.
	_ = sc.Info()
}

// TestSensorCollector_InfoBeforeCollect verifies that calling Info before
// Collect does not panic and returns the zero value of SensorData.
func TestSensorCollector_InfoBeforeCollect(t *testing.T) {
	t.Parallel()
	sc := collector.NewSensorCollector(silentLogger())
	// Must not panic.
	data := sc.Info()
	// The atomic pointer is initialised to an empty struct, so all slice
	// fields must be nil / length zero.
	if data.CoreTemps != nil {
		t.Errorf("expected nil CoreTemps before Collect, got %v", data.CoreTemps)
	}
	if data.Fans != nil {
		t.Errorf("expected nil Fans before Collect, got %v", data.Fans)
	}
}
