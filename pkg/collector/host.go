package collector

import (
	"log/slog"
	"os"
	"path/filepath"
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

	platform, version, err := platformInformation()
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

// Paths consulted when working out the distribution. Variables so tests can
// point them at a synthetic tree; nothing else reassigns them.
var (
	etcRoot           = "/etc"
	usrLibOSRelease   = "/usr/lib/os-release"
	lsbReleaseBinary  = "/usr/bin/lsb_release"
	raspiConfigBinary = "/usr/bin/raspi-config"
)

// platformInformation returns the distribution name and version.
//
// It exists to avoid a subprocess. gopsutil's PlatformInformation calls
// getlsbStruct first, which reads /etc/lsb-release when that file exists and
// otherwise FORKS /usr/bin/lsb_release -- a Python script on Debian. On a host
// without the file that is an interpreter spawned on every collection.
//
// Measured 2026-09-03 across the fleet: HostCollector cost 38.65 ms on fs and
// 40.96 ms on mon0, and about 1 ms everywhere else. Those two are exactly the
// two hosts with no /etc/lsb-release, ten hosts, no exceptions. On fs that was
// 19.33 ms/s -- 29% of the machine's entire CPU -- spent forking a script to
// report a distribution name.
//
// The short-circuit below fires only where gopsutil would fork AND its answer
// is fully determined by files we can read. Everything else calls gopsutil
// unchanged, so this narrows a hot path rather than replacing a dependency.
func platformInformation() (platform, version string, err error) {
	if p, v, ok := debianWithoutFork(); ok {
		return p, v, nil
	}
	p, _, v, err := pshost.PlatformInformation()
	return p, v, err
}

// debianWithoutFork reports gopsutil's answer for a plain Debian host that has
// no /etc/lsb-release, without running anything.
//
// Every condition is necessary and each rules out a case where gopsutil's
// answer would differ:
//
//   - /etc/lsb-release absent: with the file, gopsutil reads it and no fork
//     happens, so there is nothing to avoid and its named cases may apply.
//   - /usr/bin/lsb_release present: without it gopsutil does not fork either.
//   - /etc/os-release ID is exactly "debian": on Ubuntu, Mint, Kylin, Deepin
//     and the rest, gopsutil takes a named branch and reports that name with
//     the lsb release string. Only the default branch is reproducible here.
//   - /usr/bin/raspi-config absent: its presence makes gopsutil say "raspbian".
//
// The version is then the first line of /etc/debian_version verbatim, which is
// what gopsutil's default branch returns. Verified byte-for-byte on both
// affected hosts: fs reports "debian 12.13" against a debian_version of
// "12.13", mon0 "debian 12.15" against "12.15".
func debianWithoutFork() (platform, version string, ok bool) {
	if fileExists(filepath.Join(etcRoot, "lsb-release")) {
		return "", "", false
	}
	if !fileExists(lsbReleaseBinary) {
		return "", "", false
	}
	if fileExists(raspiConfigBinary) {
		return "", "", false
	}
	if osReleaseID() != "debian" {
		return "", "", false
	}

	raw, err := os.ReadFile(filepath.Join(etcRoot, "debian_version"))
	if err != nil {
		return "", "", false
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	return "debian", line, true
}

// osReleaseID returns the ID field of os-release, or "".
//
// /etc/os-release first and /usr/lib/os-release second is the order the
// os-release specification defines: the second is where a vendor ships it and
// the first is where an administrator may override it. This is not a fallback
// for a failure, it is the two documented locations of one file.
func osReleaseID() string {
	for _, path := range []string{filepath.Join(etcRoot, "os-release"), usrLibOSRelease} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			value, found := strings.CutPrefix(strings.TrimSpace(line), "ID=")
			if !found {
				continue
			}
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
		return ""
	}
	return ""
}

// fileExists reports whether path is present. A path that cannot be stated for
// any other reason counts as absent, which sends the caller to gopsutil -- the
// conservative direction.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readDMI reads a single DMI identity file from sysfs.
func readDMI(name string) string {
	data, err := os.ReadFile("/sys/devices/virtual/dmi/id/" + name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
