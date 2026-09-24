package fleet

import (
	"sort"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// attentionRank orders the nodes that need attention: a node raising a
// critical alert first, then one that cannot be seen at all, then one raising
// only warnings. Unreachable sits between them because its state is unknown --
// it may be fine or it may be the worst node in the fleet -- while a warning is
// known to be a warning.
var attentionRank = map[string]int{
	StatusCritical:    0,
	StatusUnreachable: 1,
	StatusWarning:     2,
}

// statusCounters routes a node to the count its status belongs in.
var statusCounters = map[string]func(*types.FleetRollup){
	StatusHealthy:     func(r *types.FleetRollup) { r.Healthy++ },
	StatusWarning:     func(r *types.FleetRollup) { r.Warning++ },
	StatusCritical:    func(r *types.FleetRollup) { r.Critical++ },
	StatusUnreachable: func(r *types.FleetRollup) { r.Unreachable++ },
}

// rollup builds the fleet-level statement from the per-node view. nodes must
// already be in display order (by name), which is what makes a tie for an
// extreme go to the same node every time rather than flickering between polls.
//
// The rule it implements is #46's: the worst value with the node named, or a
// sum with its denominator -- never a mean. And only reachable nodes contribute
// to a figure: an unreachable node is counted once, as unreachable, and its
// last known values are not re-reported as current.
//
// names bounds every per-node list -- attention, the build groups and the
// names in each -- at that many; zero leaves them whole. See View for which
// requests get which.
func rollup(nodes []types.FleetNode, names int) *types.FleetRollup {
	within := func(n int) bool { return names == 0 || n < names }
	r := &types.FleetRollup{
		Nodes:     len(nodes),
		Attention: []types.NodeAttention{},
		Builds:    []types.FleetBuild{},
	}
	builds := map[string]*types.FleetBuild{}
	// The nodes needing attention, by rank, in name order within each: only
	// the first RollupNames of them overall are ever named, so only indexes
	// are kept, and a NodeAttention is built for no more than are sent.
	needing := make([][]int, len(attentionRank))

	for i := range nodes {
		n := &nodes[i]
		if count, ok := statusCounters[n.Status]; ok {
			count(r)
		}
		if rank, ok := attentionRank[n.Status]; ok && within(len(needing[rank])) {
			needing[rank] = append(needing[rank], i)
		}
		if !n.Reachable {
			if n.AgeSeconds > 0 {
				r.LongestAbsenceSeconds = max(r.LongestAbsenceSeconds, n.AgeSeconds)
			} else {
				r.NeverAnswered++
			}
			continue
		}

		r.Reporting++
		s := &n.NodeSummary
		worst(&r.CPU, n.Name, s.CPUPercent)
		worst(&r.Memory, n.Name, s.MemoryPercent)
		worst(&r.LoadPerCore, n.Name, s.LoadPerCore)
		// Zero here means "nothing to read" -- no filesystem, no thermometer
		// -- so it is never allowed to be the fleet's worst.
		if s.WorstDiskPercent > 0 {
			worst(&r.Disk, n.Name, s.WorstDiskPercent)
		}
		if s.MaxTempC > 0 {
			worst(&r.Temperature, n.Name, s.MaxTempC)
			if r.Temperature.Node == n.Name {
				r.Temperature.Kind = s.MaxTempSource
			}
		}
		if s.SwapRateAvailable {
			worst(&r.SwapOut, n.Name, s.SwapOutRate)
		} else {
			r.SwapUnknown++
		}

		if s.PowerReported {
			if r.Power == nil {
				r.Power = &types.FleetSum{}
			}
			r.Power.Value += s.PowerW
			r.Power.Nodes++
		}
		if s.NetReported {
			if r.Network == nil {
				r.Network = &types.FleetNet{}
			}
			r.Network.RxRate += s.NetRxRate
			r.Network.TxRate += s.NetTxRate
			r.Network.Nodes++
		}

		r.SMART = addDevices(r.SMART, s.SMARTCapable, s.SMARTUnhealthy)
		r.RAID = addDevices(r.RAID, s.RAIDArrays, s.RAIDUnhealthy)

		commit := s.GitCommit
		if commit == unstatedCommit {
			commit = ""
		}
		b, ok := builds[commit]
		if !ok {
			b = &types.FleetBuild{Commit: commit, Nodes: []string{}}
			builds[commit] = b
		}
		b.Count++
		if within(len(b.Nodes)) {
			b.Nodes = append(b.Nodes, n.Name)
		}
	}

	if r.Power != nil {
		r.Power.Of = r.Reporting
	}
	if r.Network != nil {
		r.Network.Of = r.Reporting
	}
	for _, h := range []*types.FleetHealth{r.SMART, r.RAID} {
		if h != nil {
			h.Of = r.Reporting
		}
	}

	for _, rank := range needing {
		for _, i := range rank {
			if !within(len(r.Attention)) {
				break
			}
			r.Attention = append(r.Attention, attention(&nodes[i]))
		}
	}

	for _, b := range builds {
		r.Builds = append(r.Builds, *b)
	}
	// The largest group first, so the common build leads and the stragglers
	// follow; commit breaks ties so the order does not depend on map order.
	sort.Slice(r.Builds, func(i, j int) bool {
		a, b := r.Builds[i], r.Builds[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Commit < b.Commit
	})
	r.BuildGroups = len(r.Builds)
	if names > 0 {
		r.Builds = r.Builds[:min(len(r.Builds), names)]
	}
	return r
}

// worst keeps the larger of the current extreme and name's value. Strictly
// larger, so the first node in display order keeps a tie.
func worst(cur **types.FleetExtreme, name string, v float64) {
	if *cur == nil || v > (*cur).Value {
		*cur = &types.FleetExtreme{Node: name, Value: v}
	}
}

// addDevices folds one node's device count into a fleet total. A node with no
// devices contributes nothing -- not a node to the coverage, and not a zero to
// the total -- and a fleet where no node has any leaves the total nil, so the
// roll-up says nothing rather than "0 of 0 unhealthy", which reads as a clean
// bill of health for hardware that does not exist.
func addDevices(h *types.FleetHealth, devices, unhealthy int) *types.FleetHealth {
	if devices == 0 {
		return h
	}
	if h == nil {
		h = &types.FleetHealth{}
	}
	h.Devices += devices
	h.Unhealthy += unhealthy
	h.Nodes++
	return h
}

// attention names why a node is not healthy: its alert messages, critical
// first, or for a node that cannot be seen, the reason the poll gave.
func attention(n *types.FleetNode) types.NodeAttention {
	a := types.NodeAttention{Name: n.Name, Status: n.Status, Reasons: []string{}}
	if n.Status == StatusUnreachable {
		a.AgeSeconds = n.AgeSeconds
		if n.Error != "" {
			a.Reasons = append(a.Reasons, n.Error)
		}
		return a
	}
	for _, severity := range []string{types.SeverityCritical, types.SeverityWarning} {
		for _, al := range n.Alerts {
			if al.Severity == severity {
				a.Reasons = append(a.Reasons, al.Message)
			}
		}
	}
	return a
}
