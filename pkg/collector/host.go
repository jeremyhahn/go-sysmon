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
//
// The identity of a machine -- its board, its BIOS, its kernel, the instant it
// booted -- cannot change while that kernel is running, so it is read once and
// kept. Only the hostname, the uptime and the distribution strings are read per
// tick.
//
// That split is worth 41 ms per tick on mon0 (issue #76), and the reason it was
// ever that expensive is instructive: this collector called host.Info(), and
// host.Info() counts every entry in /proc to fill a Procs field nothing here
// reads. A second walk of the process table, every two seconds, for a number
// thrown away.
type HostCollector struct {
	// identity holds the fields fixed for the life of the running kernel. It is
	// a value rather than a pointer because it is written once, before the
	// collector is handed to anything.
	identity types.HostInfo

	info   atomic.Pointer[types.HostInfo]
	logger *slog.Logger
}

// NewHostCollector returns a new HostCollector, reading the host's fixed
// identity as it does so.
//
// A read that fails here leaves its field empty and is not an error: an empty
// board vendor is what a virtual machine reports anyway, and refusing to
// construct the collector would take down every other metric with it.
func NewHostCollector(logger *slog.Logger) *HostCollector {
	c := &HostCollector{logger: logger}
	c.info.Store(&types.HostInfo{})

	c.identity = types.HostInfo{
		BoardVendor:  readDMI("board_vendor"),
		BoardName:    readDMI("board_name"),
		BoardVersion: readDMI("board_version"),
		BIOSVendor:   readDMI("bios_vendor"),
		BIOSVersion:  readDMI("bios_version"),
		BIOSDate:     readDMI("bios_date"),
	}

	if v, err := pshost.KernelVersion(); err == nil {
		c.identity.KernelVersion = v
	} else {
		logger.Debug("host: kernel version unreadable", "error", err)
	}
	if a, err := pshost.KernelArch(); err == nil {
		c.identity.KernelArch = a
	} else {
		logger.Debug("host: kernel architecture unreadable", "error", err)
	}
	if t, err := pshost.BootTime(); err == nil {
		c.identity.BootTime = t
	} else {
		logger.Debug("host: boot time unreadable", "error", err)
	}

	return c
}

// Collect refreshes the host information that can change.
//
// Three fields are read every time, and each is here on purpose:
//
//   - Hostname, because hostnamectl can change it while the service runs.
//   - Uptime, which is the point of the field.
//   - Platform and PlatformVersion, because /etc/os-release is rewritten by a
//     distribution upgrade. Caching them would be defensible -- the running
//     system is still the old one -- but it would change how fresh a field is,
//     and that is a decision about what the dashboard promises rather than a
//     saving. PlatformInformation costs a couple of file reads and does not
//     count processes.
func (c *HostCollector) Collect() error {
	info := c.identity

	hostname, err := os.Hostname()
	if err != nil {
		return &types.CollectorError{Collector: "host", Cause: err}
	}
	info.Hostname = hostname

	uptime, err := pshost.Uptime()
	if err != nil {
		return &types.CollectorError{Collector: "host", Cause: err}
	}
	info.Uptime = uptime

	platform, _, version, err := pshost.PlatformInformation()
	if err != nil {
		// A distribution this cannot name is still a running host, and every
		// other field is good. Naming it is not worth failing the collector.
		warnOnce(c.logger, "host:platform", "host: could not identify the distribution", "error", err)
	} else {
		info.Platform, info.PlatformVersion = platform, version
		info.OS = "linux"
	}

	c.info.Store(&info)
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
