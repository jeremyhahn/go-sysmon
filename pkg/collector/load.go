package collector

import (
	"log/slog"
	"sync/atomic"

	psload "github.com/shirou/gopsutil/v4/load"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// LoadCollector collects system load averages.
type LoadCollector struct {
	avg    atomic.Pointer[types.LoadAverage]
	logger *slog.Logger
}

// NewLoadCollector returns a new LoadCollector.
func NewLoadCollector(logger *slog.Logger) *LoadCollector {
	c := &LoadCollector{logger: logger}
	c.avg.Store(&types.LoadAverage{})
	return c
}

// Collect refreshes the load average readings.
func (c *LoadCollector) Collect() error {
	stat, err := psload.Avg()
	if err != nil {
		return &types.CollectorError{Collector: "load", Cause: err}
	}
	c.avg.Store(&types.LoadAverage{
		Load1:  stat.Load1,
		Load5:  stat.Load5,
		Load15: stat.Load15,
	})
	return nil
}

// Info returns the most recently collected LoadAverage.
func (c *LoadCollector) Info() types.LoadAverage {
	return *c.avg.Load()
}
