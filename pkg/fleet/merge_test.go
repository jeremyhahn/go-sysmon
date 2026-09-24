package fleet

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// randomFleet is n nodes with every status, overlapping extremes (so ties are
// decided by name), a few builds, and devices, power and network on some.
func randomFleet(rng *rand.Rand, n int) []types.FleetNode {
	nodes := make([]types.FleetNode, 0, n)
	for i := range n {
		s := types.NodeSummary{
			GitCommit:        fmt.Sprintf("c%d", rng.IntN(7)),
			CPUPercent:       float64(rng.IntN(20)) * 5,
			CPUAvailable:     rng.IntN(4) != 0,
			MemoryPercent:    float64(rng.IntN(10)) * 10,
			LoadPerCore:      float64(rng.IntN(4)) / 2,
			WorstDiskPercent: float64(rng.IntN(5)) * 20,
			MaxTempC:         float64(rng.IntN(3)) * 30,
			MaxTempSource:    "cpu",
			SMARTCapable:     rng.IntN(3),
			RAIDArrays:       rng.IntN(2),
		}
		if s.SMARTCapable > 0 {
			s.SMARTUnhealthy = rng.IntN(s.SMARTCapable + 1)
		}
		if rng.IntN(2) == 0 {
			s.PowerW, s.PowerReported = float64(rng.IntN(300)), true
		}
		if rng.IntN(2) == 0 {
			s.NetRxRate, s.NetTxRate, s.NetReported = float64(rng.IntN(1000)), float64(rng.IntN(1000)), true
		}
		if rng.IntN(3) == 0 {
			s.SwapRateAvailable, s.SwapOutRate = true, float64(rng.IntN(4))*1024
		}
		name := fmt.Sprintf("n-%03d", i)
		switch rng.IntN(5) {
		case 0:
			nodes = append(nodes, down(name, float64(rng.IntN(3)*60), s))
		case 1:
			s.WarningCount, s.Alerts = 1, []types.Alert{warn("w " + name)}
			nodes = append(nodes, up(name, s))
		case 2:
			s.CriticalCount, s.Alerts = 1, []types.Alert{crit("c " + name)}
			nodes = append(nodes, up(name, s))
		default:
			nodes = append(nodes, up(name, s))
		}
	}
	sortByName(nodes)
	return nodes
}

// TestMerge_ComposesExactlyWhatOnePassWouldCompute is #46's rule held across
// a tree (#190): split a fleet among children any way at all, and merging the
// children's roll-ups gives the roll-up of the whole fleet in one pass --
// counts, sums, denominators, every extreme and the node it names, the worst
// nodes needing attention and every build group. Nothing is averaged, so
// nothing is lost by composing.
func TestMerge_ComposesExactlyWhatOnePassWouldCompute(t *testing.T) {
	rng := rand.New(rand.NewPCG(190, 46))
	for trial := range 200 {
		fleet := randomFleet(rng, 5+rng.IntN(60))
		want := rollup(fleet, types.RollupNames)

		// Deal the nodes out to children at random, keeping each child's
		// share in name order, as a child's own view is.
		k := 1 + rng.IntN(5)
		shares := make([][]types.FleetNode, k)
		for _, n := range fleet {
			c := rng.IntN(k)
			shares[c] = append(shares[c], n)
		}
		got := collect(nil, types.RollupNames)
		for _, share := range shares {
			merge(got, collect(share, types.RollupNames), types.RollupNames)
		}
		trimBuilds(got, types.RollupNames)

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d, %d nodes over %d children:\ncomposed %+v\nwant     %+v", trial, len(fleet), k, got, want)
		}
	}
}

// TestMerge_TieGoesToTheEarlierName: an extreme shared by nodes in two
// children names the node first by name, whichever child is merged first.
func TestMerge_TieGoesToTheEarlierName(t *testing.T) {
	a := collect([]types.FleetNode{up("zeta", types.NodeSummary{CPUPercent: 90, CPUAvailable: true})}, types.RollupNames)
	b := collect([]types.FleetNode{up("alpha", types.NodeSummary{CPUPercent: 90, CPUAvailable: true})}, types.RollupNames)
	merge(a, b, types.RollupNames)
	if a.CPU.Node != "alpha" {
		t.Errorf("tie named %q, want alpha", a.CPU.Node)
	}
}

// TestAddUnknown: a subtree that cannot be seen is counted, node by node, as
// unreachable -- never healthy -- with the longest absence carried forward by
// the time since it was read, and a node that never answered left as such.
func TestAddUnknown(t *testing.T) {
	r := collect(nil, types.RollupNames)
	addUnknown(r, []types.FleetIndexEntry{
		{Name: "a", Status: types.NodeStatusHealthy, Reachable: true, AgeSeconds: 1},
		{Name: "b", Status: types.NodeStatusUnreachable, AgeSeconds: 600},
		{Name: "c", Status: types.NodeStatusUnreachable},
	}, 30)
	if r.Nodes != 3 || r.Unreachable != 3 || r.Healthy != 0 || r.Reporting != 0 {
		t.Errorf("counts = %d nodes, %d unreachable, %d healthy, %d reporting; want 3, 3, 0, 0",
			r.Nodes, r.Unreachable, r.Healthy, r.Reporting)
	}
	if r.LongestAbsenceSeconds != 630 || r.NeverAnswered != 1 {
		t.Errorf("absence %v, never answered %d; want 630 and 1", r.LongestAbsenceSeconds, r.NeverAnswered)
	}
}
