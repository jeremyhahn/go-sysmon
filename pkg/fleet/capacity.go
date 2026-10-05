package fleet

import (
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Fleet capacity (#239): the absolute figures a scheduler places work by, per
// node and summed over the fleet, and each node's headroom at the aggregator's
// utilisation target.
//
// The summary carries what the node measured; everything here is derived from
// it by the aggregator, under the same rule as the rest of the roll-up (#46): a
// sum carries the nodes it was taken over, a figure no node measured is
// absent, and nothing is averaged across machines.

// nodeHeadroom is n's spare capacity at target, or nil when n cannot be seen or
// neither half can be computed.
//
// An unreachable node's figures are its last known ones, kept so its card can
// show them as stale; capacity computed from them would invite work onto a
// node nobody can currently see, so it has none. Each half is computed only
// from figures the node says it measured, and clamped at zero: a node already
// past its target has no room, not negative room that would cancel another's
// in a sum.
func nodeHeadroom(n *types.FleetNode, target float64) *types.NodeHeadroom {
	if !n.Reachable {
		return nil
	}
	s := &n.NodeSummary
	h := types.NodeHeadroom{Target: target}
	if s.CPUCount > 0 && s.CPUAvailable {
		h.CPUCoresFreeAtTarget = max(0, float64(s.CPUCount)*(target-s.CPUPercent/100))
		h.CPUReported = true
	}
	if s.MemoryTotalBytes > 0 {
		reserve := (1 - target) * float64(s.MemoryTotalBytes)
		if free := float64(s.MemoryAvailableBytes) - reserve; free > 0 {
			h.MemoryBytesFree = uint64(free)
		}
		h.MemoryReported = true
	}
	if !h.CPUReported && !h.MemoryReported {
		return nil
	}
	return &h
}

// addCapacity folds one reporting node's capacity into r. Each figure counts
// only where the node measured it -- a known core count, a known memory total,
// a measured disk path, a readable cgroup tree -- so an older peer's zeros
// add neither to a total nor to its coverage. Its headroom counts only at
// target, this aggregator's.
func addCapacity(r *types.FleetRollup, n *types.FleetNode, target float64) {
	s := &n.NodeSummary
	if s.CPUCount > 0 {
		addTotal(&capacityOf(r).CPUCount, uint64(s.CPUCount))
	}
	if s.MemoryTotalBytes > 0 {
		c := capacityOf(r)
		addTotal(&c.MemoryTotalBytes, s.MemoryTotalBytes)
		addTotal(&c.MemoryAvailableBytes, s.MemoryAvailableBytes)
	}
	if s.DiskFreePath != "" {
		addTotal(&capacityOf(r).DiskFreeBytes, s.DiskFreeBytes)
	}
	if s.ContainersObservable {
		addTotal(&capacityOf(r).ContainersRunning, uint64(s.ContainersRunning))
	}
	if h := n.Headroom; h != nil {
		if sums := headroomAt(capacityOf(r), h.Target, target); sums != nil {
			if h.CPUReported {
				addSum(&sums.CPUCoresFreeAtTarget, h.CPUCoresFreeAtTarget)
			}
			if h.MemoryReported {
				addTotal(&sums.MemoryBytesFree, h.MemoryBytesFree)
			}
		}
	}
}

// mergeCapacity adds a child aggregator's capacity sums into r's, as merge
// adds every other sum: totals and coverage add, and the denominators are set
// after, from the reachable nodes of both.
//
// Headroom at a target other than this aggregator's is left out rather than
// added; see types.FleetHeadroom.
func mergeCapacity(r *types.FleetRollup, c *types.FleetCapacity, target float64) {
	if c == nil {
		return
	}
	dst := capacityOf(r)
	mergeTotal(&dst.CPUCount, c.CPUCount)
	mergeTotal(&dst.MemoryTotalBytes, c.MemoryTotalBytes)
	mergeTotal(&dst.MemoryAvailableBytes, c.MemoryAvailableBytes)
	mergeTotal(&dst.DiskFreeBytes, c.DiskFreeBytes)
	mergeTotal(&dst.ContainersRunning, c.ContainersRunning)
	if h := c.Headroom; h != nil {
		if sums := headroomAt(dst, h.Target, target); sums != nil {
			mergeSum(&sums.CPUCoresFreeAtTarget, h.CPUCoresFreeAtTarget)
			mergeTotal(&sums.MemoryBytesFree, h.MemoryBytesFree)
		}
	}
}

// capacityDenominators sets every capacity total's denominator to the
// reachable nodes, as for every other sum in the roll-up.
func capacityDenominators(r *types.FleetRollup) {
	for _, t := range capacityTotals(r.Capacity) {
		t.Of = r.Reporting
	}
	if c := r.Capacity; c != nil && c.Headroom != nil && c.Headroom.CPUCoresFreeAtTarget != nil {
		c.Headroom.CPUCoresFreeAtTarget.Of = r.Reporting
	}
}

// capacityTotals is every whole-number total c holds.
func capacityTotals(c *types.FleetCapacity) []*types.FleetTotal {
	if c == nil {
		return nil
	}
	all := []*types.FleetTotal{c.CPUCount, c.MemoryTotalBytes, c.MemoryAvailableBytes, c.DiskFreeBytes, c.ContainersRunning}
	if c.Headroom != nil {
		all = append(all, c.Headroom.MemoryBytesFree)
	}
	present := all[:0]
	for _, t := range all {
		if t != nil {
			present = append(present, t)
		}
	}
	return present
}

// capacityOf is r's capacity block, created on first use: a fleet in which no
// node measured anything has none, and says nothing.
func capacityOf(r *types.FleetRollup) *types.FleetCapacity {
	if r.Capacity == nil {
		r.Capacity = &types.FleetCapacity{}
	}
	return r.Capacity
}

// headroomAt is c's headroom sums at target, this aggregator's, to add
// headroom computed at from into: created on first use, and nil when from is
// another target, whose headroom cannot be added to these.
//
// The target is always the aggregator's own, never the first one to arrive.
// An aggregator with no headroom of its own yet -- just after startup, before
// its first collection -- would otherwise adopt whichever child it merged
// first, and then leave out every child at its configured target. The block
// is created only when something is added, so a roll-up with no headroom at
// target carries none, rather than an empty one.
func headroomAt(c *types.FleetCapacity, from, target float64) *types.FleetHeadroom {
	if from != target {
		return nil
	}
	if c.Headroom == nil {
		c.Headroom = &types.FleetHeadroom{Target: target}
	}
	return c.Headroom
}

// cloneCapacity is a deep copy of c, which merge may add into without
// touching c.
func cloneCapacity(c *types.FleetCapacity) *types.FleetCapacity {
	if c == nil {
		return nil
	}
	out := &types.FleetCapacity{
		CPUCount:             cloneTotal(c.CPUCount),
		MemoryTotalBytes:     cloneTotal(c.MemoryTotalBytes),
		MemoryAvailableBytes: cloneTotal(c.MemoryAvailableBytes),
		DiskFreeBytes:        cloneTotal(c.DiskFreeBytes),
		ContainersRunning:    cloneTotal(c.ContainersRunning),
	}
	if h := c.Headroom; h != nil {
		out.Headroom = &types.FleetHeadroom{Target: h.Target, MemoryBytesFree: cloneTotal(h.MemoryBytesFree)}
		if h.CPUCoresFreeAtTarget != nil {
			v := *h.CPUCoresFreeAtTarget
			out.Headroom.CPUCoresFreeAtTarget = &v
		}
	}
	return out
}

func cloneTotal(t *types.FleetTotal) *types.FleetTotal {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// addTotal adds one node's reading to a total, creating it on first use.
func addTotal(t **types.FleetTotal, v uint64) {
	if *t == nil {
		*t = &types.FleetTotal{}
	}
	(*t).Value += v
	(*t).Nodes++
}

// addSum adds one node's reading to a sum, creating it on first use.
func addSum(s **types.FleetSum, v float64) {
	if *s == nil {
		*s = &types.FleetSum{}
	}
	(*s).Value += v
	(*s).Nodes++
}

// mergeTotal adds a child's total into *dst; its denominator is set after.
func mergeTotal(dst **types.FleetTotal, src *types.FleetTotal) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = &types.FleetTotal{}
	}
	(*dst).Value += src.Value
	(*dst).Nodes += src.Nodes
}

// mergeSum adds a child's sum into *dst; its denominator is set after.
func mergeSum(dst **types.FleetSum, src *types.FleetSum) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = &types.FleetSum{}
	}
	(*dst).Value += src.Value
	(*dst).Nodes += src.Nodes
}

// capacityInconsistency says why a child's capacity block cannot be merged,
// or "": every total's coverage within its reachable nodes, every sum a
// finite, non-negative reading, and headroom at a target that is one. A block
// with nothing in it is not one collect produces.
func capacityInconsistency(c *types.FleetCapacity, reporting int) string {
	if c == nil {
		return ""
	}
	totals := capacityTotals(c)
	if len(totals) == 0 && c.Headroom == nil {
		return "a capacity block with nothing in it"
	}
	for _, t := range totals {
		if t.Nodes < 0 || t.Nodes > t.Of || t.Of != reporting {
			return "a capacity total whose denominator is not its reachable nodes"
		}
	}
	h := c.Headroom
	if h == nil {
		return ""
	}
	switch s := h.CPUCoresFreeAtTarget; {
	case !(h.Target > 0 && h.Target <= 1):
		return "headroom at a target that is not a utilisation"
	case s == nil && h.MemoryBytesFree == nil:
		return "a headroom block with nothing in it"
	case s != nil && (!measure(s.Value) || s.Nodes < 0 || s.Nodes > s.Of || s.Of != reporting):
		return "a headroom sum whose denominator is not its reachable nodes"
	}
	return ""
}
