package collector

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	pshost "github.com/shirou/gopsutil/v4/host"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// HostCollector collects general host information.
type HostCollector struct {
	info   atomic.Pointer[types.HostInfo]
	logger *slog.Logger
}

// NewHostCollector returns a new HostCollector.
func NewHostCollector(logger *slog.Logger) *HostCollector {
	c := &HostCollector{logger: logger}
	c.info.Store(&types.HostInfo{})
	return c
}

// Collect refreshes host information from the OS.
func (c *HostCollector) Collect() error {
	stat, err := pshost.Info()
	if err != nil {
		return &types.CollectorError{Collector: "host", Cause: err}
	}
	info := &types.HostInfo{
		Hostname:        stat.Hostname,
		OS:              stat.OS,
		Platform:        stat.Platform,
		PlatformVersion: stat.PlatformVersion,
		KernelVersion:   stat.KernelVersion,
		KernelArch:      stat.KernelArch,
		Uptime:          stat.Uptime,
		BootTime:        stat.BootTime,
		BoardVendor:     readDMI("board_vendor"),
		BoardName:       readDMI("board_name"),
		BoardVersion:    readDMI("board_version"),
		BIOSVendor:      readDMI("bios_vendor"),
		BIOSVersion:     readDMI("bios_version"),
		BIOSDate:        readDMI("bios_date"),
	}
	c.info.Store(info)
	return nil
}

// Info returns the most recently collected HostInfo.
func (c *HostCollector) Info() types.HostInfo {
	return *c.info.Load()
}

// readDMI reads a single DMI identity file from sysfs.
func readDMI(name string) string {
	data, err := os.ReadFile("/sys/devices/virtual/dmi/id/" + name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
