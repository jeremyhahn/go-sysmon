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
var attentionRank = map[types.NodeStatus]int{
	types.NodeStatusCritical:    0,
	types.NodeStatusUnreachable: 1,
	types.NodeStatusWarning:     2,
}

// statusCounters routes a node to the count its status belongs in.
var statusCounters = map[types.NodeStatus]func(*types.FleetRollup){
	types.NodeStatusHealthy:     func(r *types.FleetRollup) { r.Healthy++ },
	types.NodeStatusWarning:     func(r *types.FleetRollup) { r.Warning++ },
	types.NodeStatusCritical:    func(r *types.FleetRollup) { r.Critical++ },
	types.NodeStatusUnreachable: func(r *types.FleetRollup) { r.Unreachable++ },
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
	r := collect(nodes, names)
	trimBuilds(r, names)
	return r
}

// collect is rollup without the last step: every build group is kept, each
// naming at most names nodes. That is the form a child aggregator serves its
// parent (#190), because groups can only be summed across children exactly if
// none has been dropped; the parent bounds them once, after composing.
func collect(nodes []types.FleetNode, names int) *types.FleetRollup {
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
		if s.CPUAvailable {
			worst(&r.CPU, n.Name, s.CPUPercent)
		} else {
			r.CPUUnknown++
		}
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
	sortBuilds(r)
	return r
}

// sortBuilds puts the largest group first, so the common build leads and the
// stragglers follow; commit breaks ties so the order does not depend on map
// order.
func sortBuilds(r *types.FleetRollup) {
	sort.Slice(r.Builds, func(i, j int) bool {
		a, b := r.Builds[i], r.Builds[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Commit < b.Commit
	})
	r.BuildGroups = len(r.Builds)
}

// trimBuilds bounds the build groups at names, the largest first; zero keeps
// them all.
func trimBuilds(r *types.FleetRollup, names int) {
	if names > 0 {
		r.Builds = r.Builds[:min(len(r.Builds), names)]
	}
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
	if n.Status == types.NodeStatusUnreachable {
		a.AgeSeconds = n.AgeSeconds
		if n.Error != "" {
			a.Reasons = append(a.Reasons, n.Error)
		}
		return a
	}
	for _, severity := range []types.Severity{types.SeverityCritical, types.SeverityWarning} {
		for _, al := range n.Alerts {
			if al.Severity == severity {
				a.Reasons = append(a.Reasons, al.Message)
			}
		}
	}
	return a
}

// merge folds a child aggregator's roll-up into r (#190). It is exact, by the
// same rule rollup follows (#46): counts, sums and denominators add, extremes
// keep the larger value and name its node -- the earlier name on a tie, as a
// single pass over the whole fleet would -- and nothing is averaged.
//
// The per-node lists compose exactly too. Each side's attention list is its
// worst names nodes by (rank, name), so the worst names of the union are among
// them; each build group names its first names nodes by name, so the merged
// group's first names are among them. Build groups must all be present on both
// sides (collect keeps them), or a group's count would be short.
func merge(r, c *types.FleetRollup, names int) {
	r.Nodes += c.Nodes
	r.Healthy += c.Healthy
	r.Warning += c.Warning
	r.Critical += c.Critical
	r.Unreachable += c.Unreachable
	r.LongestAbsenceSeconds = max(r.LongestAbsenceSeconds, c.LongestAbsenceSeconds)
	r.NeverAnswered += c.NeverAnswered
	r.SubtreeErrors += c.SubtreeErrors
	r.NameConflicts += c.NameConflicts
	r.Reporting += c.Reporting
	r.CPUUnknown += c.CPUUnknown
	r.SwapUnknown += c.SwapUnknown

	for _, e := range []struct{ dst, src **types.FleetExtreme }{
		{&r.CPU, &c.CPU}, {&r.Memory, &c.Memory}, {&r.Disk, &c.Disk},
		{&r.LoadPerCore, &c.LoadPerCore}, {&r.Temperature, &c.Temperature}, {&r.SwapOut, &c.SwapOut},
	} {
		mergeExtreme(e.dst, *e.src)
	}

	if c.Power != nil {
		if r.Power == nil {
			r.Power = &types.FleetSum{}
		}
		r.Power.Value += c.Power.Value
		r.Power.Nodes += c.Power.Nodes
	}
	if c.Network != nil {
		if r.Network == nil {
			r.Network = &types.FleetNet{}
		}
		r.Network.RxRate += c.Network.RxRate
		r.Network.TxRate += c.Network.TxRate
		r.Network.Nodes += c.Network.Nodes
	}
	r.SMART = mergeHealth(r.SMART, c.SMART)
	r.RAID = mergeHealth(r.RAID, c.RAID)
	// Every denominator is the reachable nodes, summed above.
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

	r.Attention = mergeAttention(r.Attention, c.Attention, names)
	r.Builds = mergeBuilds(r.Builds, c.Builds, names)
	sortBuilds(r)
}

// mergeExtreme keeps the worse of *dst and src. A tie keeps the node that
// comes first by name, which is the node a single pass over the whole fleet
// in name order would have kept.
func mergeExtreme(dst **types.FleetExtreme, src *types.FleetExtreme) {
	if src == nil {
		return
	}
	cur := *dst
	if cur == nil || src.Value > cur.Value ||
		(src.Value == cur.Value && CompareNodeNames(src.Node, cur.Node) < 0) {
		cp := *src
		*dst = &cp
	}
}

func mergeHealth(dst, src *types.FleetHealth) *types.FleetHealth {
	if src == nil {
		return dst
	}
	if dst == nil {
		dst = &types.FleetHealth{}
	}
	dst.Devices += src.Devices
	dst.Unhealthy += src.Unhealthy
	dst.Nodes += src.Nodes
	return dst
}

// mergeAttention is the worst names entries of a and b, by rank and then name.
func mergeAttention(a, b []types.NodeAttention, names int) []types.NodeAttention {
	out := make([]types.NodeAttention, 0, len(a)+len(b))
	out = append(append(out, a...), b...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := attentionRank[out[i].Status], attentionRank[out[j].Status]
		if ri != rj {
			return ri < rj
		}
		return CompareNodeNames(out[i].Name, out[j].Name) < 0
	})
	if names > 0 && len(out) > names {
		out = out[:names]
	}
	return out
}

// mergeBuilds adds b's groups into a's by commit: counts summed, and the first
// names nodes of each by name.
func mergeBuilds(a, b []types.FleetBuild, names int) []types.FleetBuild {
	byCommit := make(map[string]int, len(a))
	out := make([]types.FleetBuild, 0, len(a)+len(b))
	for _, g := range a {
		byCommit[g.Commit] = len(out)
		out = append(out, types.FleetBuild{Commit: g.Commit, Count: g.Count, Nodes: append([]string{}, g.Nodes...)})
	}
	for _, g := range b {
		i, ok := byCommit[g.Commit]
		if !ok {
			byCommit[g.Commit] = len(out)
			out = append(out, types.FleetBuild{Commit: g.Commit, Count: g.Count, Nodes: append([]string{}, g.Nodes...)})
			continue
		}
		out[i].Count += g.Count
		merged := append(out[i].Nodes, g.Nodes...)
		sort.Slice(merged, func(x, y int) bool { return CompareNodeNames(merged[x], merged[y]) < 0 })
		if names > 0 && len(merged) > names {
			merged = merged[:names]
		}
		out[i].Nodes = merged
	}
	return out
}

// addUnknown counts the nodes of a subtree that cannot be seen -- its child
// aggregator is unreachable, or its subtree could not be read -- as #46 counts
// any node that cannot be seen: unreachable, contributing to no figure, with
// how long its last reading has been. They are not named one by one in the
// attention list; the child's own entry carries how many there are.
func addUnknown(r *types.FleetRollup, entries []types.FleetIndexEntry, since float64) {
	for _, e := range entries {
		r.Nodes++
		r.Unreachable++
		if e.AgeSeconds > 0 || (e.Reachable && since > 0) {
			r.LongestAbsenceSeconds = max(r.LongestAbsenceSeconds, e.AgeSeconds+since)
		} else {
			r.NeverAnswered++
		}
	}
}
