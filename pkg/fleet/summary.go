package fleet

import (
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Summarize reduces a node's snapshot to its NodeSummary: the handful of
// figures the fleet roll-up and its cards show.
//
// Every node runs it over its own snapshots to serve GET /api/summary, and the
// aggregator runs the same function over itself and over any peer too old to
// serve a summary, so the three can never disagree about what a figure means.
//
// prev is the snapshot collected immediately before cur, and is read only for
// its timestamp and swap counters: the paging rate is the difference between
// the two. A nil prev is allowed and yields a summary whose rate is marked
// unavailable -- never one that reports zero paging. cur must not be nil.
func Summarize(prev, cur *types.Snapshot) types.NodeSummary {
	n := types.NodeSummary{
		Hostname:              cur.Host.Hostname,
		NodeName:              cur.NodeName,
		Version:               cur.Version,
		GitCommit:             cur.GitCommit,
		BuildDate:             cur.BuildDate,
		UptimeSeconds:         cur.Host.Uptime,
		MemoryPercent:         cur.Memory.UsedPercent,
		SwapPercent:           cur.Memory.SwapPercent,
		SwapCountersAvailable: cur.Memory.SwapCountersAvailable,
		Alerts:                []types.Alert{},
	}
	n.SwapInRate, n.SwapOutRate, n.SwapRateAvailable = swapRates(prev, cur)

	// A per-node mean over that node's own cores is the node's utilisation.
	// The rule against means is about the FLEET: averaging across machines of
	// different sizes, which the roll-up never does.
	for _, c := range cur.CPUs {
		n.CPUPercent += c.UsagePercent
	}
	if len(cur.CPUs) > 0 {
		n.CPUPercent /= float64(len(cur.CPUs))
	}
	if cur.CPUSummary.TotalThreads > 0 {
		n.LoadPerCore = cur.LoadAvg.Load1 / float64(cur.CPUSummary.TotalThreads)
	}

	summarizeDisks(&n, cur.Disks)

	for _, c := range cur.Sensors.CoreTemps {
		hotter(&n, types.TempCPU, c.TempCelsius)
	}
	// Accelerators count as components like any other: a GPU at 90 °C is the
	// hottest thing in the machine whether or not its cores are.
	for i := range cur.GPUs {
		hotter(&n, types.TempGPU, cur.GPUs[i].TemperatureGPU)
		hotter(&n, types.TempGPU, cur.GPUs[i].TemperatureMemory)
	}
	for i := range cur.NPUs {
		hotter(&n, types.TempNPU, cur.NPUs[i].TemperatureC)
	}
	// Power is reported only when there is a sensor to report it. A node with
	// none sends zero, and a fleet total that counted that zero would read a
	// machine with no power sensor as a machine drawing nothing (#46).
	// A RAPL package reads zero until it has two energy samples to take a
	// difference of, so a zero is "not yet measured", never a measured
	// idle -- a running package always draws something.
	for _, pp := range cur.Sensors.PackagePower {
		addPower(&n, pp.PowerW)
	}
	for i := range cur.GPUs {
		addPower(&n, cur.GPUs[i].PowerDrawW)
	}
	for i := range cur.NPUs {
		addPower(&n, cur.NPUs[i].PowerDrawW)
	}
	summarizeNetwork(&n, cur.Networks)

	// Only warning and critical alerts travel: they are what makes a node
	// something other than healthy, and so what the roll-up names as the
	// reason. Informational alerts change no status and would only grow the
	// payload every aggregator polls.
	for _, a := range cur.Alerts {
		switch a.Severity {
		case types.SeverityCritical:
			n.CriticalCount++
		case types.SeverityWarning:
			n.WarningCount++
		default:
			continue
		}
		n.Alerts = append(n.Alerts, a)
	}
	return n
}

// LocalSummary is the summary a node serves of itself: Summarize over its two
// latest readings, with the interval its monitor collects at. Only the node
// itself knows that interval, so only this path carries it; a peer summarised
// from its whole snapshot reports it as unknown.
func LocalSummary(prev, cur *types.Snapshot, interval time.Duration) types.NodeSummary {
	n := Summarize(prev, cur)
	n.CollectionIntervalMs = interval.Milliseconds()
	return n
}

// summarizeDisks fills the storage figures: the fullest filesystem, the RAID
// and SMART counts with their denominators, and the hottest drive.
func summarizeDisks(n *types.NodeSummary, disks []types.DiskInfo) {
	// A controller can present several virtual disks on one disk group, and
	// each lists the same drives as its members. A drive is counted once, by
	// its serial -- the identity that survives a reboot. A member without one
	// cannot be told apart from another and is counted where it appears.
	counted := make(map[string]bool)
	for i := range disks {
		d := &disks[i]
		if d.TotalBytes > 0 && d.UsedPercent > n.WorstDiskPercent {
			n.WorstDiskPercent = d.UsedPercent
		}
		// Counted whether or not it is degraded: a numerator with no
		// denominator cannot say whether zero means "all well" or "nothing to
		// look at", and those are different answers for an operator.
		if d.RAID != nil {
			n.RAIDArrays++
			// Degraded is not the whole of "unhealthy". An inactive, suspended,
			// broken or unreadable array is not serving I/O at all, raises a
			// CRITICAL raid_not_active alert, and had Degraded false -- so the
			// roll-up called it ok while the alert called it critical. The
			// predicate is alert's, so there cannot be a second list of states
			// to drift from the first.
			if d.RAID.Degraded || alert.RAIDNotOperational(d.RAID.State) {
				n.RAIDUnhealthy++
			}
			if d.RAID.Degraded {
				n.RAIDDegraded = true
			}
		}
		if d.SMARTEnabled {
			n.SMARTCapable++
			if !d.SMARTHealthy {
				n.SMARTUnhealthy++
			}
		}
		hotter(n, types.TempDrive, d.Temperature)
		// The drives behind a hardware RAID controller count too. They are not
		// block devices and never appear in the disk list, so a denominator
		// summed over that list alone would report ci-node-1 as having three
		// SMART-capable devices while six readable drives sat behind its PERC.
		// That is #44's defect surviving its own fix: a denominator that does
		// not know what it left out. A member's temperature is the drive's own,
		// and on a host whose only spinning disks are behind a controller it is
		// the only disk temperature there is.
		for _, m := range d.Members {
			hotter(n, types.TempDrive, m.Temperature)
			if !m.SMARTEnabled {
				continue
			}
			if m.Serial != "" {
				if counted[m.Serial] {
					continue
				}
				counted[m.Serial] = true
			}
			n.SMARTCapable++
			if !m.SMARTHealthy {
				n.SMARTUnhealthy++
			}
		}
	}
}

// hotter records c as the node's hottest reading if it is. A sensor that did
// not report reads zero, and zero is never hotter than anything: an absent
// reading contributes nothing.
func hotter(n *types.NodeSummary, kind types.TempSource, c float64) {
	if c > n.MaxTempC {
		n.MaxTempC, n.MaxTempSource = c, kind
	}
}

// addPower adds an accelerator's draw. A device that reports no draw is not a
// power sensor for this purpose, so it neither adds nor marks power reported.
func addPower(n *types.NodeSummary, w float64) {
	if w > 0 {
		n.PowerW += w
		n.PowerReported = true
	}
}

// summarizeNetwork sums throughput over the node's physical devices. It is
// reported only when every one of them has measured rates: a device whose
// counters have no earlier reading to difference -- the first collection, a
// device that just appeared, counters that could not be read or were reset --
// reads zero, and a sum that included it would present an unknown as idle.
//
// A peer too old to send rates_available, read by its whole snapshot, never
// reports throughput. Its rates are probably measured, but the field that
// says so is absent, and an absent field from an older peer is unknown, not
// agreement (#46's silence rules). The network total says how many nodes it
// covers, so the gap is visible for as long as the rollout lasts.
func summarizeNetwork(n *types.NodeSummary, all []types.NetworkInfo) {
	wire := wireInterfaces(all)
	if len(wire) == 0 {
		return
	}
	for _, iface := range wire {
		if !iface.RatesAvailable {
			return
		}
	}
	for _, iface := range wire {
		n.NetRxRate += iface.BytesRecvRate
		n.NetTxRate += iface.BytesSentRate
	}
	n.NetReported = true
}

// wireInterfaces are the interfaces a node's throughput is summed over: its
// physical network devices (IsVirtual false), where its traffic meets the
// wire. No two of them carry the same packet.
//
// Summing every interface double-counts whatever is layered: a bond master
// carries its slaves' traffic, a bridge its ports', a guest's packets cross
// its vnet port and then the NIC, a VLAN or tunnel rides on the device below
// it. Physical devices are the one set with no overlap. A node with none -- a
// container in its own network namespace -- has no such set that sysfs can
// identify, and reports no throughput rather than an overlapping sum; see
// NodeSummary.NetReported.
func wireInterfaces(all []types.NetworkInfo) []types.NetworkInfo {
	var out []types.NetworkInfo
	for _, iface := range all {
		if !iface.IsLoopback && !iface.IsVirtual {
			out = append(out, iface)
		}
	}
	return out
}

// swapRates is the paging rate between two consecutive readings, in bytes per
// second paged in and out (#134).
//
// ok is false whenever the rate was not measured, and the rates are then zero
// only because they have to be something: the caller must render them as
// unknown. That covers a missing previous reading, counters either reading
// could not supply, readings that do not move forward in time, and counters
// that went backwards or a boot time that changed -- a reset, as when the host
// rebooted between the two.
// Differencing across a reset would produce a huge or negative rate for an
// interval in which the true figure is unknowable.
func swapRates(prev, cur *types.Snapshot) (in, out float64, ok bool) {
	if prev == nil || cur == nil {
		return 0, 0, false
	}
	a, b := &prev.Memory, &cur.Memory
	if !a.SwapCountersAvailable || !b.SwapCountersAvailable || b.PageSizeBytes == 0 {
		return 0, 0, false
	}
	if b.SwapInPages < a.SwapInPages || b.SwapOutPages < a.SwapOutPages {
		return 0, 0, false
	}
	// A reboot between the two resets the counters, and by the second reading
	// they may already have climbed past where they were -- so falling
	// counters alone cannot be relied on to see it. A changed boot time can.
	// Zero is an unknown boot time and is not taken as a change.
	if pb, cb := prev.Host.BootTime, cur.Host.BootTime; pb != 0 && cb != 0 && pb != cb {
		return 0, 0, false
	}
	elapsed := cur.Timestamp.Sub(prev.Timestamp).Seconds()
	if elapsed <= 0 {
		return 0, 0, false
	}
	page := float64(b.PageSizeBytes)
	in = float64(b.SwapInPages-a.SwapInPages) * page / elapsed
	out = float64(b.SwapOutPages-a.SwapOutPages) * page / elapsed
	return in, out, true
}

// swapBaseline is what a later Summarize needs of a snapshot to difference
// its swap counters: its timestamp and memory, and nothing else. The
// aggregator keeps one per peer that serves only whole snapshots, rather than
// the whole snapshot, which on a busy host is megabytes held for two numbers.
func swapBaseline(s *types.Snapshot) *types.Snapshot {
	return &types.Snapshot{
		Timestamp: s.Timestamp,
		Host:      types.HostInfo{BootTime: s.Host.BootTime},
		Memory:    s.Memory,
	}
}

// nodeStatus is a node's single health state. Unreachable wins over any alert:
// an alert on a node we cannot currently see is last known, not current.
func nodeStatus(reachable bool, s *types.NodeSummary) types.NodeStatus {
	switch {
	case !reachable:
		return types.NodeStatusUnreachable
	case s.CriticalCount > 0:
		return types.NodeStatusCritical
	case s.WarningCount > 0:
		return types.NodeStatusWarning
	default:
		return types.NodeStatusHealthy
	}
}
