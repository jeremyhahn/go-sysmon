package collector

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Interface kinds reported in NetworkInfo.Kind.
const (
	kindEthernet = "ethernet"
	kindWifi     = "wifi"
	kindBridge   = "bridge"
	kindBond     = "bond"
	kindVLAN     = "vlan"
	kindTun      = "tun"
	kindVeth     = "veth"
	kindLoopback = "loopback"
	kindVirtual  = "virtual"
)

// sysClassNet is the sysfs root for network interfaces. It is a variable so
// tests can point the classifier at a synthetic tree.
var sysClassNet = "/sys/class/net"

// procNetWireless holds per-interface radio statistics.
var procNetWireless = "/proc/net/wireless"

// resolvConfPaths are consulted in order for system-wide resolver settings.
// The systemd-resolved file is preferred because /etc/resolv.conf usually
// points at the 127.0.0.53 stub rather than the real upstream servers.
var resolvConfPaths = []string{
	"/run/systemd/resolve/resolv.conf",
	"/etc/resolv.conf",
}

// systemdResolveNetif holds per-link resolver state written by systemd-resolved.
var systemdResolveNetif = "/run/systemd/resolve/netif"

// classifyInterface determines what kind of interface name is, using only
// sysfs attributes. Order matters: a bridge is also virtual, and a wireless
// interface is also an ethernet-like device, so the most specific test wins.
func classifyInterface(name string) string {
	base := filepath.Join(sysClassNet, name)

	switch {
	case name == "lo":
		return kindLoopback
	case dirExists(filepath.Join(base, "bridge")):
		return kindBridge
	case dirExists(filepath.Join(base, "bonding")):
		return kindBond
	case dirExists(filepath.Join(base, "wireless")), pathExists(filepath.Join(base, "phy80211")):
		return kindWifi
	case pathExists(filepath.Join(base, "tun_flags")):
		return kindTun
	case strings.HasPrefix(name, "veth"):
		return kindVeth
	case pathExists(filepath.Join(base, "lower_")) || strings.Contains(name, "."):
		return kindVLAN
	case pathExists(filepath.Join(base, "device")):
		return kindEthernet
	default:
		return kindVirtual
	}
}

// readOperState returns the kernel's operational state for an interface, which
// is distinct from the administrative IFF_UP flag.
func readOperState(name string) string {
	return readSysfsString(filepath.Join(sysClassNet, name, "operstate"))
}

// readBridgePorts lists the interfaces enslaved to a bridge, sorted by name.
func readBridgePorts(name string) []string {
	entries, err := os.ReadDir(filepath.Join(sysClassNet, name, "brif"))
	if err != nil {
		return nil
	}
	ports := make([]string, 0, len(entries))
	for _, e := range entries {
		ports = append(ports, e.Name())
	}
	sort.Strings(ports)
	return ports
}

// readBondInfo returns the bonding mode and slave list for a bond master.
// The mode file reads like "balance-rr 0"; only the name is reported.
func readBondInfo(name string) (string, []string) {
	base := filepath.Join(sysClassNet, name, "bonding")

	mode := readSysfsString(filepath.Join(base, "mode"))
	if fields := strings.Fields(mode); len(fields) > 0 {
		mode = fields[0]
	}

	var slaves []string
	if raw := readSysfsString(filepath.Join(base, "slaves")); raw != "" {
		slaves = strings.Fields(raw)
		sort.Strings(slaves)
	}
	return mode, slaves
}

// readMaster returns the name of the bridge or bond an interface is enslaved
// to, or an empty string when it is not enslaved.
func readMaster(name string) string {
	target, err := os.Readlink(filepath.Join(sysClassNet, name, "master"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// readWirelessInfo returns radio statistics for a wifi interface from
// /proc/net/wireless, or nil when the interface has no entry.
//
// Lines look like:
//
//	wlp3s0: 0000   70.  -40.  -256        0      0      0     12      0        0
//
// The quality, level and noise columns may carry a trailing dot.
func readWirelessInfo(name string) *types.WirelessInfo {
	raw, err := os.ReadFile(procNetWireless)
	if err != nil {
		return nil
	}

	for _, line := range strings.Split(string(raw), "\n") {
		iface, rest, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(iface) != name {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 4 {
			return nil
		}
		// fields[0] is the status word; quality, level and noise follow.
		return &types.WirelessInfo{
			LinkQuality:    parseWirelessValue(fields[1]),
			SignalLevelDBm: parseWirelessValue(fields[2]),
			NoiseLevelDBm:  parseWirelessValue(fields[3]),
		}
	}
	return nil
}

// parseWirelessValue parses a /proc/net/wireless numeric column, tolerating
// the trailing "." the kernel appends to indicate an updated value.
func parseWirelessValue(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "."), 64)
	if err != nil {
		return 0
	}
	return v
}

// readLinkDNS returns the resolver configuration for a specific interface
// index as recorded by systemd-resolved, or nil when there is no per-link
// state for it.
func readLinkDNS(ifIndex int) ([]string, []string) {
	raw, err := os.ReadFile(filepath.Join(systemdResolveNetif, strconv.Itoa(ifIndex)))
	if err != nil {
		return nil, nil
	}

	var servers, search []string
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		switch strings.TrimSpace(key) {
		case "DNS":
			servers = append(servers, fields...)
		case "DOMAINS":
			search = append(search, fields...)
		}
	}
	return servers, search
}

// readSystemDNS returns the system-wide resolver configuration. It is used for
// interfaces with no per-link state, since those interfaces still resolve
// through these servers.
func readSystemDNS() ([]string, []string) {
	for _, path := range resolvConfPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var servers, search []string
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "nameserver":
				servers = append(servers, fields[1])
			case "search", "domain":
				for _, d := range fields[1:] {
					// systemd writes a bare "." to mean the root domain, which
					// is not a useful search suffix to display.
					if d != "." {
						search = append(search, d)
					}
				}
			}
		}
		if len(servers) > 0 || len(search) > 0 {
			return servers, search
		}
	}
	return nil, nil
}

// applyInterfaceDetail enriches info with classification, topology, resolver
// and radio data. sysDNS and sysSearch are the system-wide resolver settings,
// passed in so they are read once per collection rather than per interface.
//
// ifIndex is the kernel interface index, which is what systemd-resolved keys
// its per-link state by. It is deliberately separate from NetworkInfo.Index,
// which is this snapshot's slice position and does not match.
func applyInterfaceDetail(info *types.NetworkInfo, ifIndex int, sysDNS, sysSearch []string) {
	info.Kind = classifyInterface(info.Name)
	info.OperState = readOperState(info.Name)
	info.BondMaster = readMaster(info.Name)

	switch info.Kind {
	case kindBridge:
		info.BridgePorts = readBridgePorts(info.Name)
	case kindBond:
		info.BondMode, info.BondSlaves = readBondInfo(info.Name)
	case kindWifi:
		info.Wireless = readWirelessInfo(info.Name)
	}

	if servers, search := readLinkDNS(ifIndex); len(servers) > 0 {
		info.DNSServers = servers
		info.DNSSearch = search
		return
	}

	// No per-link configuration: report the system resolvers, which is what
	// this interface's traffic will actually use.
	info.DNSServers = sysDNS
	info.DNSSearch = sysSearch
}

// dirExists reports whether path exists and is a directory.
//
// #nosec G703 -- this only stats a path the kernel reported (a cgroup scope
// read out of /proc). Nothing is opened or read, so the worst a crafted name
// achieves is a false answer to "does this directory exist".
func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// pathExists reports whether path exists, following symlinks.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
