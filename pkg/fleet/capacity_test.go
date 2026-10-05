package fleet

import (
	"math"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const gib = 1 << 30

// capable is a node summary carrying every capacity figure: 16 cores at 25%,
// 64 GiB of which 40 GiB is available, 500 GiB free on its data root, and two
// containers using 1.5 cores and 3 GiB.
func capable() types.NodeSummary {
	return types.NodeSummary{
		CPUCount: 16, CPUPercent: 25, CPUAvailable: true,
		MemoryTotalBytes: 64 * gib, MemoryAvailableBytes: 40 * gib,
		DiskFreePath: "/var/lib/docker", DiskFreeBytes: 500 * gib, DiskTotalBytes: 1000 * gib,
		ContainersObservable: true, ContainersRunning: 2, ContainerCPUCores: 1.5, ContainerMemoryBytes: 3 * gib,
	}
}

// TestNodeHeadroom is the arithmetic: cores that could be kept busy, and
// memory that could be allocated, before the node reaches the target -- each
// clamped at zero, each computed only from figures the node measured, and
// none at all for a node that cannot be seen.
func TestNodeHeadroom(t *testing.T) {
	tests := []struct {
		name   string
		node   types.FleetNode
		target float64
		want   *types.NodeHeadroom
	}{
		{
			// 16 x 0.75 = 12 cores at target, 4 in use: 8 free. 40 GiB
			// available, a quarter of 64 GiB kept back: 24 GiB free.
			name: "both halves", node: up("n", capable()), target: 0.75,
			want: &types.NodeHeadroom{Target: 0.75, CPUCoresFreeAtTarget: 8, CPUReported: true,
				MemoryBytesFree: 24 * gib, MemoryReported: true},
		},
		{
			// A target of the whole machine leaves every idle core and all
			// the available memory.
			name: "target of one", node: up("n", capable()), target: 1,
			want: &types.NodeHeadroom{Target: 1, CPUCoresFreeAtTarget: 12, CPUReported: true,
				MemoryBytesFree: 40 * gib, MemoryReported: true},
		},
		{
			// Past the target on both: no room, never negative room that
			// would cancel another node's in a sum.
			name: "past the target clamps to zero",
			node: up("n", func() types.NodeSummary {
				s := capable()
				s.CPUPercent, s.MemoryAvailableBytes = 95, 4*gib
				return s
			}()),
			target: 0.75,
			want:   &types.NodeHeadroom{Target: 0.75, CPUReported: true, MemoryReported: true},
		},
		{
			// The node's first collection: memory is known, CPU usage is not.
			name: "cpu usage unknown",
			node: up("n", func() types.NodeSummary {
				s := capable()
				s.CPUAvailable, s.CPUPercent = false, 0
				return s
			}()),
			target: 0.75,
			want:   &types.NodeHeadroom{Target: 0.75, MemoryBytesFree: 24 * gib, MemoryReported: true},
		},
		{
			name: "core count unknown",
			node: up("n", func() types.NodeSummary {
				s := capable()
				s.CPUCount = 0
				return s
			}()),
			target: 0.75,
			want:   &types.NodeHeadroom{Target: 0.75, MemoryBytesFree: 24 * gib, MemoryReported: true},
		},
		{
			name: "memory unknown",
			node: up("n", func() types.NodeSummary {
				s := capable()
				s.MemoryTotalBytes, s.MemoryAvailableBytes = 0, 0
				return s
			}()),
			target: 0.75,
			want:   &types.NodeHeadroom{Target: 0.75, CPUCoresFreeAtTarget: 8, CPUReported: true},
		},
		{
			// An older peer, with its CPU usage measured but nothing else.
			name: "an older peer: nothing to compute", node: up("n", types.NodeSummary{CPUPercent: 25, CPUAvailable: true}),
			target: 0.75, want: nil,
		},
		{
			name: "unreachable: its last figures are not capacity", node: down("n", 30, capable()),
			target: 0.75, want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeHeadroom(&tc.node, tc.target)
			switch {
			case got == nil && tc.want == nil:
			case got == nil || tc.want == nil:
				t.Fatalf("nodeHeadroom() = %+v, want %+v", got, tc.want)
			case *got != *tc.want:
				t.Errorf("nodeHeadroom() = %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

// TestNodeHeadroom_AtTheDefaultTarget: the 0.8 a coordinator runs with, whose
// arithmetic is not exact in binary, is within a byte and a hair of a core.
func TestNodeHeadroom_AtTheDefaultTarget(t *testing.T) {
	n := up("n", capable())
	h := nodeHeadroom(&n, 0.8)
	if h == nil || !h.CPUReported || !h.MemoryReported {
		t.Fatalf("nodeHeadroom() = %+v, want both halves", h)
	}
	if want := 16 * (0.8 - 0.25); math.Abs(h.CPUCoresFreeAtTarget-want) > 1e-9 {
		t.Errorf("cores free = %v, want %v", h.CPUCoresFreeAtTarget, want)
	}
	want := 40*gib - 0.2*64*gib
	if diff := math.Abs(float64(h.MemoryBytesFree) - want); diff > 1 {
		t.Errorf("memory free = %d, want %v", h.MemoryBytesFree, want)
	}
}

// TestStamp_HeadroomAtTheGivenTarget: stamp derives headroom with the rest of
// a node's derived fields, so every node of a view -- this one and its peers
// -- carries it at the target it is given.
func TestStamp_HeadroomAtTheGivenTarget(t *testing.T) {
	n := types.FleetNode{Name: "n", Reachable: true, NodeSummary: capable()}
	stamp(&n, "abc", 0.5)
	if n.Headroom == nil || n.Headroom.Target != 0.5 || n.Headroom.CPUCoresFreeAtTarget != 4 {
		t.Errorf("Headroom = %+v, want 4 cores free at 0.5", n.Headroom)
	}

	gone := types.FleetNode{Name: "gone", NodeSummary: capable(), Headroom: &types.NodeHeadroom{Target: 0.9}}
	stamp(&gone, "abc", 0.5)
	if gone.Headroom != nil {
		t.Errorf("unreachable node Headroom = %+v, want none", gone.Headroom)
	}
}

// TestFromChild_RestatesHeadroomAtThisAggregatorsTarget: a subtree node's
// headroom is this aggregator's statement, not its child's -- the child may
// run another target, or be too old to compute any.
func TestFromChild_RestatesHeadroomAtThisAggregatorsTarget(t *testing.T) {
	entry := &types.FleetIndexEntry{Name: "deep", Status: types.NodeStatusHealthy, Reachable: true}
	r := row{name: "deep", sub: &subRow{entry: entry, via: "child", readAt: time.Now()}}

	sent := types.FleetNode{Name: "deep", Reachable: true, NodeSummary: capable(),
		Headroom: &types.NodeHeadroom{Target: 0.9, CPUCoresFreeAtTarget: 99, CPUReported: true}}
	got := fromChild(sent, r, "abc", 0.75, time.Now())
	if got.Headroom == nil || got.Headroom.Target != 0.75 || got.Headroom.CPUCoresFreeAtTarget != 8 {
		t.Errorf("Headroom = %+v, want this aggregator's 8 cores at 0.75", got.Headroom)
	}

	older := types.FleetNode{Name: "deep", Reachable: true, NodeSummary: types.NodeSummary{CPUPercent: 25, CPUAvailable: true}}
	if got := fromChild(older, r, "abc", 0.75, time.Now()); got.Headroom != nil {
		t.Errorf("Headroom = %+v for a node with no capacity figures, want none", got.Headroom)
	}

	entry.Reachable, entry.Status = false, types.NodeStatusUnreachable
	if got := fromChild(sent, r, "abc", 0.75, time.Now()); got.Headroom != nil {
		t.Errorf("Headroom = %+v for a node its child cannot see, want none", got.Headroom)
	}
}

// TestRollup_CapacityIsSummedOverTheNodesThatMeasuredIt: every capacity
// figure is a total with its coverage. An older peer's zeros add to nothing,
// a node whose containers cannot be observed is not counted as running none,
// and an unreachable node's last figures are not counted at all.
func TestRollup_CapacityIsSummedOverTheNodesThatMeasuredIt(t *testing.T) {
	full := up("alpha", capable())
	full.Headroom = nodeHeadroom(&full, 0.75)

	partial := capable()
	partial.CPUCount, partial.CPUPercent = 8, 50
	partial.DiskFreePath, partial.DiskFreeBytes = "", 0
	partial.ContainersObservable, partial.ContainersRunning = false, 0
	partial.MemoryTotalBytes, partial.MemoryAvailableBytes = 32*gib, 4*gib // past the target
	half := up("bravo", partial)
	half.Headroom = nodeHeadroom(&half, 0.75)

	older := up("charlie", types.NodeSummary{CPUPercent: 10, CPUAvailable: true, MemoryPercent: 30})
	gone := down("delta", 60, capable())

	r := rollup([]types.FleetNode{full, half, older, gone}, 0, 0.75)
	c := r.Capacity
	if c == nil {
		t.Fatal("Capacity omitted though two nodes measured it")
	}
	checks := []struct {
		name string
		got  *types.FleetTotal
		want types.FleetTotal
	}{
		{"cpu_count", c.CPUCount, types.FleetTotal{Value: 24, Nodes: 2, Of: 3}},
		{"memory_total_bytes", c.MemoryTotalBytes, types.FleetTotal{Value: 96 * gib, Nodes: 2, Of: 3}},
		{"memory_available_bytes", c.MemoryAvailableBytes, types.FleetTotal{Value: 44 * gib, Nodes: 2, Of: 3}},
		{"disk_free_bytes", c.DiskFreeBytes, types.FleetTotal{Value: 500 * gib, Nodes: 1, Of: 3}},
		{"containers_running", c.ContainersRunning, types.FleetTotal{Value: 2, Nodes: 1, Of: 3}},
	}
	for _, ck := range checks {
		if ck.got == nil || *ck.got != ck.want {
			t.Errorf("%s = %+v, want %+v", ck.name, ck.got, ck.want)
		}
	}

	// alpha 8 cores and 24 GiB; bravo 8 x (0.75 - 0.5) = 2 cores and no
	// memory (4 GiB available against an 8 GiB reserve) -- a zero, counted.
	h := c.Headroom
	if h == nil || h.Target != 0.75 {
		t.Fatalf("Headroom = %+v, want sums at 0.75", h)
	}
	if h.CPUCoresFreeAtTarget == nil || *h.CPUCoresFreeAtTarget != (types.FleetSum{Value: 10, Nodes: 2, Of: 3}) {
		t.Errorf("cores free = %+v, want 10 across 2 of 3", h.CPUCoresFreeAtTarget)
	}
	if h.MemoryBytesFree == nil || *h.MemoryBytesFree != (types.FleetTotal{Value: 24 * gib, Nodes: 2, Of: 3}) {
		t.Errorf("memory free = %+v, want 24 GiB across 2 of 3", h.MemoryBytesFree)
	}
}

// TestRollup_NoCapacityWhenNoNodeMeasuredAny: a fleet of older builds reports
// no capacity, rather than a fleet with none to spare.
func TestRollup_NoCapacityWhenNoNodeMeasuredAny(t *testing.T) {
	r := rollup([]types.FleetNode{
		up("alpha", types.NodeSummary{CPUPercent: 10, CPUAvailable: true}),
		down("bravo", 30, capable()),
	}, 0, 0.75)
	if r.Capacity != nil {
		t.Errorf("Capacity = %+v, want it omitted", *r.Capacity)
	}
}

// TestMerge_CapacityComposesAcrossChildren: a child's capacity adds into its
// parent's -- totals, coverage, headroom -- with the denominators the
// reachable nodes of both.
func TestMerge_CapacityComposesAcrossChildren(t *testing.T) {
	a := up("alpha", capable())
	a.Headroom = nodeHeadroom(&a, 0.75)
	b := up("bravo", capable())
	b.Headroom = nodeHeadroom(&b, 0.75)

	r := collect([]types.FleetNode{a, up("charlie", types.NodeSummary{})}, 0, 0.75)
	merge(r, collect([]types.FleetNode{b}, 0, 0.75), 0, 0.75)

	if got := *r.Capacity.CPUCount; got != (types.FleetTotal{Value: 32, Nodes: 2, Of: 3}) {
		t.Errorf("cpu_count = %+v, want 32 across 2 of 3", got)
	}
	if got := *r.Capacity.Headroom.CPUCoresFreeAtTarget; got != (types.FleetSum{Value: 16, Nodes: 2, Of: 3}) {
		t.Errorf("cores free = %+v, want 16 across 2 of 3", got)
	}
}

// TestMerge_HeadroomAtAnotherTargetIsLeftOut: sums at two targets do not add.
// The child's are left out of the parent's headroom, its coverage says so,
// and every other capacity figure, which has no target, still adds.
func TestMerge_HeadroomAtAnotherTargetIsLeftOut(t *testing.T) {
	a := up("alpha", capable())
	a.Headroom = nodeHeadroom(&a, 0.75)
	b := up("bravo", capable())
	b.Headroom = nodeHeadroom(&b, 0.5)

	r := collect([]types.FleetNode{a}, 0, 0.75)
	merge(r, collect([]types.FleetNode{b}, 0, 0.75), 0, 0.75)

	h := r.Capacity.Headroom
	if h.Target != 0.75 || *h.CPUCoresFreeAtTarget != (types.FleetSum{Value: 8, Nodes: 1, Of: 2}) {
		t.Errorf("headroom = target %v, cores %+v; want alpha's 8 at 0.75 across 1 of 2", h.Target, *h.CPUCoresFreeAtTarget)
	}
	if got := *r.Capacity.CPUCount; got != (types.FleetTotal{Value: 32, Nodes: 2, Of: 2}) {
		t.Errorf("cpu_count = %+v, want 32 across 2 of 2", got)
	}

	// A node at a target other than the aggregator's is left out the same
	// way within one pass, whichever comes first.
	r = collect([]types.FleetNode{a, b}, 0, 0.75)
	if got := *r.Capacity.Headroom.CPUCoresFreeAtTarget; got.Nodes != 1 {
		t.Errorf("cores free = %+v, want only alpha's, at the aggregator's target", got)
	}
}

// TestAgedRollup_CapacityIsACopy: the flat view merges this node into a copy
// of the peers' cached roll-up on every request. A copy that shared the
// capacity block would add this node into the cache again on each one.
func TestAgedRollup_CapacityIsACopy(t *testing.T) {
	a := up("alpha", capable())
	a.Headroom = nodeHeadroom(&a, 0.75)
	cached := collect([]types.FleetNode{a}, 0, 0.75)

	for range 3 {
		c := agedRollup(cached, 1)
		merge(c, collect([]types.FleetNode{a}, 0, 0.75), 0, 0.75)
		if got := c.Capacity.CPUCount.Value; got != 32 {
			t.Fatalf("merged cpu_count = %d, want 32", got)
		}
	}
	if got := cached.Capacity.CPUCount.Value; got != 16 {
		t.Errorf("cached cpu_count = %d after three requests, want 16: the cache was written through", got)
	}
	if got := cached.Capacity.Headroom.CPUCoresFreeAtTarget.Value; got != 8 {
		t.Errorf("cached cores free = %v, want 8", got)
	}
	if cloneCapacity(nil) != nil {
		t.Error("cloneCapacity(nil) != nil")
	}
}

// TestCapacityInconsistency: a child's capacity block is merged only if every
// total's coverage is within its reachable nodes, every sum is a reading, and
// its headroom is at a target that is a utilisation.
func TestCapacityInconsistency(t *testing.T) {
	good := func() *types.FleetCapacity {
		a := up("alpha", capable())
		a.Headroom = nodeHeadroom(&a, 0.75)
		return collect([]types.FleetNode{a}, 0, 0.75).Capacity
	}
	if reason := capacityInconsistency(good(), 1); reason != "" {
		t.Fatalf("a block collect produced is refused: %s", reason)
	}
	if reason := capacityInconsistency(nil, 1); reason != "" {
		t.Errorf("an absent block (an older child) is refused: %s", reason)
	}
	noHeadroom := good()
	noHeadroom.Headroom = nil
	if reason := capacityInconsistency(noHeadroom, 1); reason != "" {
		t.Errorf("a block without headroom (no node's could be computed) is refused: %s", reason)
	}

	bad := map[string]func(c *types.FleetCapacity){
		"empty":                     func(c *types.FleetCapacity) { *c = types.FleetCapacity{} },
		"coverage beyond reachable": func(c *types.FleetCapacity) { c.CPUCount.Nodes = 2 },
		"denominator not reachable": func(c *types.FleetCapacity) { c.DiskFreeBytes.Of = 5 },
		"negative coverage":         func(c *types.FleetCapacity) { c.MemoryTotalBytes.Nodes = -1 },
		"target zero":               func(c *types.FleetCapacity) { c.Headroom.Target = 0 },
		"target past one":           func(c *types.FleetCapacity) { c.Headroom.Target = 1.5 },
		"target NaN":                func(c *types.FleetCapacity) { c.Headroom.Target = math.NaN() },
		"empty headroom": func(c *types.FleetCapacity) {
			c.Headroom.CPUCoresFreeAtTarget, c.Headroom.MemoryBytesFree = nil, nil
		},
		"negative cores":           func(c *types.FleetCapacity) { c.Headroom.CPUCoresFreeAtTarget.Value = -1 },
		"infinite cores":           func(c *types.FleetCapacity) { c.Headroom.CPUCoresFreeAtTarget.Value = math.Inf(1) },
		"cores beyond reachable":   func(c *types.FleetCapacity) { c.Headroom.CPUCoresFreeAtTarget.Nodes = 3 },
		"headroom memory coverage": func(c *types.FleetCapacity) { c.Headroom.MemoryBytesFree.Of = 0 },
	}
	for name, spoil := range bad {
		t.Run(name, func(t *testing.T) {
			c := good()
			spoil(c)
			if reason := capacityInconsistency(c, 1); reason == "" {
				t.Errorf("block %+v accepted", c)
			}
			// And the roll-up as a whole is refused for it.
			r := &types.FleetRollup{Nodes: 1, Healthy: 1, Reporting: 1, Capacity: c}
			if reason := rollupInconsistency(r, 1, map[types.NodeStatus]int{types.NodeStatusHealthy: 1}); reason == "" {
				t.Error("a roll-up carrying it was accepted")
			}
		})
	}
}

// TestMerge_HeadroomIsAnchoredToThisAggregatorsTarget is Codex's finding on
// #239: a parent with no headroom of its own yet -- just after startup, before
// it has a local snapshot -- must not adopt the first child's target. A child
// at 0.5 merged first is left out, and a later child at the parent's 0.8 is
// the one summed.
func TestMerge_HeadroomIsAnchoredToThisAggregatorsTarget(t *testing.T) {
	at := func(name string, target float64) *types.FleetRollup {
		n := up(name, capable())
		n.Headroom = nodeHeadroom(&n, target)
		return collect([]types.FleetNode{n}, 0, target)
	}
	r := collect(nil, 0, 0.8)
	merge(r, at("other", 0.5), 0, 0.8)
	merge(r, at("same", 0.8), 0, 0.8)

	h := r.Capacity.Headroom
	if h == nil || h.Target != 0.8 {
		t.Fatalf("headroom = %+v, want sums at this aggregator's 0.8", h)
	}
	want := nodeHeadroom(&types.FleetNode{Reachable: true, NodeSummary: capable()}, 0.8)
	if s := h.CPUCoresFreeAtTarget; s == nil || s.Value != want.CPUCoresFreeAtTarget || s.Nodes != 1 || s.Of != 2 {
		t.Errorf("cores free = %+v, want only the 0.8 child's %v across 1 of 2", s, want.CPUCoresFreeAtTarget)
	}
	if s := h.MemoryBytesFree; s == nil || s.Value != want.MemoryBytesFree || s.Nodes != 1 || s.Of != 2 {
		t.Errorf("memory free = %+v, want only the 0.8 child's %d across 1 of 2", s, want.MemoryBytesFree)
	}
}

// TestMerge_NoHeadroomAtThisTargetIsNoHeadroomBlock: when every child's
// headroom is at another target, the roll-up carries no headroom block --
// neither an empty one nor the child's.
func TestMerge_NoHeadroomAtThisTargetIsNoHeadroomBlock(t *testing.T) {
	n := up("other", capable())
	n.Headroom = nodeHeadroom(&n, 0.5)
	r := collect(nil, 0, 0.8)
	merge(r, collect([]types.FleetNode{n}, 0, 0.5), 0, 0.8)
	if r.Capacity == nil || r.Capacity.CPUCount == nil {
		t.Fatalf("capacity = %+v; the child's targetless totals still add", r.Capacity)
	}
	if r.Capacity.Headroom != nil {
		t.Errorf("headroom = %+v, want none: nothing was at 0.8", r.Capacity.Headroom)
	}
}
