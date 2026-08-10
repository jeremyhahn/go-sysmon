package collector

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	psnet "github.com/shirou/gopsutil/v4/net"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// netSnapshot captures raw counters at a point in time for rate calculation.
type netSnapshot struct {
	at       time.Time
	counters map[string]psnet.IOCountersStat
}

// NetworkCollector collects network interface info and traffic rates.
type NetworkCollector struct {
	networks atomic.Pointer[[]types.NetworkInfo]
	prev     atomic.Pointer[netSnapshot]
	logger   *slog.Logger
}

// NewNetworkCollector returns a new NetworkCollector.
func NewNetworkCollector(logger *slog.Logger) *NetworkCollector {
	c := &NetworkCollector{logger: logger}
	c.networks.Store(&[]types.NetworkInfo{})
	return c
}

// Collect refreshes network interface information and computes byte rates.
func (c *NetworkCollector) Collect() error {
	ifaces, err := psnet.Interfaces()
	if err != nil {
		return &types.CollectorError{Collector: "network", Cause: err}
	}

	counters, err := psnet.IOCounters(true)
	if err != nil {
		warnOnce(c.logger, "network:io-counters", "network: could not collect I/O counters", "error", err)
		counters = nil
	}

	now := time.Now()
	ctrMap := make(map[string]psnet.IOCountersStat, len(counters))
	for _, ctr := range counters {
		ctrMap[ctr.Name] = ctr
	}

	// Compute rates from the previous snapshot if available.
	var elapsed float64
	var prevMap map[string]psnet.IOCountersStat
	if prev := c.prev.Load(); prev != nil {
		elapsed = now.Sub(prev.at).Seconds()
		prevMap = prev.counters
	}

	// Resolver settings are system-wide; read them once rather than per NIC.
	sysDNS, sysSearch := readSystemDNS()

	result := make([]types.NetworkInfo, 0, len(ifaces))
	for i, iface := range ifaces {
		info := buildNetworkInfo(iface, ctrMap, prevMap, elapsed)
		info.Index = i
		applyInterfaceDetail(&info, iface.Index, sysDNS, sysSearch)
		result = append(result, info)
	}

	c.networks.Store(&result)
	c.prev.Store(&netSnapshot{at: now, counters: ctrMap})
	return nil
}

// buildNetworkInfo constructs a NetworkInfo from gopsutil data and sysfs.
func buildNetworkInfo(
	iface psnet.InterfaceStat,
	current, prev map[string]psnet.IOCountersStat,
	elapsed float64,
) types.NetworkInfo {
	info := types.NetworkInfo{
		Name:         iface.Name,
		HardwareAddr: iface.HardwareAddr,
		MTU:          iface.MTU,
		Flags:        iface.Flags,
	}

	for _, addr := range iface.Addrs {
		info.Addresses = append(info.Addresses, addr.Addr)
	}

	for _, flag := range iface.Flags {
		switch strings.ToLower(flag) {
		case "up":
			info.IsUp = true
		case "loopback":
			info.IsLoopback = true
		}
	}

	sysBase := "/sys/class/net/" + iface.Name
	info.Speed = readSysfsNICSpeed(sysBase + "/speed")
	info.Duplex = readSysfsString(sysBase + "/duplex")
	info.Driver = readNICDriver(sysBase + "/device/driver")
	info.IsVirtual = isVirtualInterface(sysBase)

	if ctr, ok := current[iface.Name]; ok {
		info.BytesSent = ctr.BytesSent
		info.BytesRecv = ctr.BytesRecv
		info.PacketsSent = ctr.PacketsSent
		info.PacketsRecv = ctr.PacketsRecv
		info.ErrorsIn = ctr.Errin
		info.ErrorsOut = ctr.Errout
		info.DropsIn = ctr.Dropin
		info.DropsOut = ctr.Dropout

		if prev != nil && elapsed > 0 {
			if prevCtr, ok := prev[iface.Name]; ok {
				sentDelta := safeSub(ctr.BytesSent, prevCtr.BytesSent)
				recvDelta := safeSub(ctr.BytesRecv, prevCtr.BytesRecv)
				info.BytesSentRate = float64(sentDelta) / elapsed
				info.BytesRecvRate = float64(recvDelta) / elapsed
			}
		}
	}

	return info
}

// readSysfsNICSpeed reads the interface speed in Mbps from sysfs.
// Returns 0 on any error (interface may be down or virtual).
func readSysfsNICSpeed(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(data))
	// Speed may be -1 when not known (e.g., interface is down).
	if trimmed == "" || trimmed == "-1" {
		return 0
	}
	var v uint64
	if _, err := fmt.Sscan(trimmed, &v); err != nil {
		return 0
	}
	return v
}

// readNICDriver resolves the driver symlink and returns the driver name.
func readNICDriver(driverPath string) string {
	target, err := os.Readlink(driverPath)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// isVirtualInterface returns true when the interface has no physical device
// directory in sysfs, which is characteristic of software/virtual NICs.
func isVirtualInterface(sysBase string) bool {
	_, err := os.Stat(sysBase + "/device")
	return os.IsNotExist(err)
}

// safeSub returns a - b, clamped to 0 on underflow (counter reset scenario).
func safeSub(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return 0
}

// Info returns the most recently collected NetworkInfo slice.
func (c *NetworkCollector) Info() []types.NetworkInfo {
	return *c.networks.Load()
}
