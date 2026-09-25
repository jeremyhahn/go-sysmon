package collector

import (
	"log/slog"
)

// Test seams: state and constructors only tests use, kept out of the build.

// newSensorCollectorWithPaths creates a SensorCollector with custom base paths
// for testing against fake sysfs trees.
func newSensorCollectorWithPaths(logger *slog.Logger, hwmon, thermal, powercap, cpu, pressure string) *SensorCollector {
	c := NewSensorCollector(logger)
	c.hwmonBase = hwmon
	c.thermalBase = thermal
	c.powercapBase = powercap
	c.cpuBase = cpu
	c.pressureBase = pressure
	return c
}

// resetWarnOnce clears the recorded conditions. It exists for tests, which
// would otherwise see state carried between cases.
func resetWarnOnce() {
	warnedConditions.Range(func(k, _ any) bool {
		warnedConditions.Delete(k)
		return true
	})
}
