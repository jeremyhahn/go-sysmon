package fleet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// up is a reachable node with the given summary, its status derived the way
// View derives it.
func up(name string, s types.NodeSummary) types.FleetNode {
	n := types.FleetNode{Name: name, Reachable: true, NodeSummary: s}
	n.Status = nodeStatus(true, &n.NodeSummary)
	return n
}

// down is an unreachable node whose last reading is age seconds old (zero: it
// never answered), carrying last known figures that must not be counted.
func down(name string, age float64, last types.NodeSummary) types.FleetNode {
	return types.FleetNode{
		Name: name, Status: types.NodeStatusUnreachable, AgeSeconds: age,
		Error: "connection refused", NodeSummary: last,
	}
}

func warn(msg string) types.Alert {
	return types.Alert{Severity: types.SeverityWarning, Message: msg}
}

func crit(msg string) types.Alert {
	return types.Alert{Severity: types.SeverityCritical, Message: msg}
}

// TestRollup_CountsEveryNodeInExactlyOneState is the header: a count by
// state, unreachable as its own term, and the four adding up to the fleet.
func TestRollup_CountsEveryNodeInExactlyOneState(t *testing.T) {
	nodes := []types.FleetNode{
		up("a", types.NodeSummary{}),
		up("b", types.NodeSummary{WarningCount: 2, Alerts: []types.Alert{warn("cpu sustained")}}),
		up("c", types.NodeSummary{CriticalCount: 1, WarningCount: 1, Alerts: []types.Alert{warn("w"), crit("c")}}),
		down("d", 8040, types.NodeSummary{}),
		down("e", 0, types.NodeSummary{}),
		up("f", types.NodeSummary{}),
	}
	r := boundedRollup(nodes)
	if r.Nodes != 6 || r.Healthy != 2 || r.Warning != 1 || r.Critical != 1 || r.Unreachable != 2 {
		t.Errorf("counts = %d nodes: %d healthy, %d warning, %d critical, %d unreachable; want 6: 2, 1, 1, 2",
			r.Nodes, r.Healthy, r.Warning, r.Critical, r.Unreachable)
	}
	if r.Healthy+r.Warning+r.Critical+r.Unreachable != r.Nodes {
		t.Error("the state counts do not partition the fleet")
	}
	if r.Reporting != 4 {
		t.Errorf("Reporting = %d; want the 4 reachable nodes", r.Reporting)
	}
}

// TestRollup_UnreachableSaysHowLong: "1 unreachable" reads the same for a node
// mid-restart and one dead since Tuesday. The oldest reading among the
// unreachable is what tells them apart, and a node that never answered has no
// reading to be old -- it is counted separately rather than as a zero-second
// absence.
func TestRollup_UnreachableSaysHowLong(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("a", types.NodeSummary{}),
		down("b", 120, types.NodeSummary{}),
		down("c", 8040, types.NodeSummary{}),
		down("d", 0, types.NodeSummary{}),
	})
	if r.LongestAbsenceSeconds != 8040 {
		t.Errorf("LongestAbsenceSeconds = %v, want 8040 (2h14m)", r.LongestAbsenceSeconds)
	}
	if r.NeverAnswered != 1 {
		t.Errorf("NeverAnswered = %d, want 1", r.NeverAnswered)
	}

	if clear := boundedRollup([]types.FleetNode{up("a", types.NodeSummary{})}); clear.LongestAbsenceSeconds != 0 || clear.NeverAnswered != 0 {
		t.Errorf("a fleet with nothing unreachable reports an absence: %+v", clear)
	}
}

// TestRollup_NamesTheReasonsWorstFirst: critical, then unreachable, then
// warning; within a node, critical reasons first; and every non-healthy node
// listed, so the consumer's cap is the only cap.
func TestRollup_NamesTheReasonsWorstFirst(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("alpha", types.NodeSummary{WarningCount: 1, Alerts: []types.Alert{warn("disk /srv 92.1%")}}),
		down("bravo", 60, types.NodeSummary{}),
		up("charlie", types.NodeSummary{}),
		up("delta", types.NodeSummary{CriticalCount: 1, WarningCount: 1,
			Alerts: []types.Alert{warn("temperature 80 C"), crit("array md0 inactive")}}),
	})

	var order []string
	for _, a := range r.Attention {
		order = append(order, a.Name)
	}
	if want := []string{"delta", "bravo", "alpha"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("attention order = %v, want %v", order, want)
	}
	if want := []string{"array md0 inactive", "temperature 80 C"}; !reflect.DeepEqual(r.Attention[0].Reasons, want) {
		t.Errorf("delta's reasons = %v, want critical first: %v", r.Attention[0].Reasons, want)
	}
	if got := r.Attention[1]; got.AgeSeconds != 60 || !reflect.DeepEqual(got.Reasons, []string{"connection refused"}) {
		t.Errorf("unreachable entry = %+v; want its age and the poll's error", got)
	}
	if got := r.Attention[2]; got.AgeSeconds != 0 {
		t.Errorf("a reachable node carries an absence: %+v", got)
	}
}

// TestRollup_PowerIsASumWithItsDenominator is the ratified example: "579 W
// across 3 of 10 nodes", never a bare 579 W, and nothing at all when no node
// has a sensor.
func TestRollup_PowerIsASumWithItsDenominator(t *testing.T) {
	nodes := []types.FleetNode{
		up("ci-node-1", types.NodeSummary{PowerW: 315, PowerReported: true}),
		up("gpu-host", types.NodeSummary{PowerW: 122, PowerReported: true}),
		up("omega", types.NodeSummary{PowerW: 142, PowerReported: true}),
		// A stale reading from an unreachable node is not re-reported.
		down("metal", 30, types.NodeSummary{PowerW: 400, PowerReported: true}),
	}
	for i := range 6 {
		nodes = append(nodes, up("vstation-0"+string(rune('1'+i)), types.NodeSummary{}))
	}
	r := boundedRollup(nodes)
	if r.Power == nil {
		t.Fatal("Power omitted though three nodes report it")
	}
	if *r.Power != (types.FleetSum{Value: 579, Nodes: 3, Of: 9}) {
		t.Errorf("Power = %+v; want 579 W across 3 of 9 reporting nodes", *r.Power)
	}

	if none := boundedRollup([]types.FleetNode{up("vm", types.NodeSummary{})}); none.Power != nil {
		t.Errorf("Power = %+v for a fleet with no power sensor; want it omitted", *none.Power)
	}
}

// TestRollup_DeviceHealthCountsDevicesNotNodes: SMART and RAID are summed over
// devices -- controller members included, because the node summary counts
// them -- with the nodes they were found on beside, and omitted when no node
// has any, so "0 of 0 unhealthy" never reads as a clean bill of health.
func TestRollup_DeviceHealthCountsDevicesNotNodes(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		// Four NVMe plus six drives behind the PERC.
		up("ci-node-1", types.NodeSummary{SMARTCapable: 10, SMARTUnhealthy: 1, RAIDArrays: 2}),
		up("fs", types.NodeSummary{SMARTCapable: 6, RAIDArrays: 3, RAIDUnhealthy: 1}),
		up("vm-host-1", types.NodeSummary{}),
		down("gone", 30, types.NodeSummary{SMARTCapable: 4, SMARTUnhealthy: 4}),
	})
	if r.SMART == nil || *r.SMART != (types.FleetHealth{Unhealthy: 1, Devices: 16, Nodes: 2, Of: 3}) {
		t.Errorf("SMART = %+v; want 1 unhealthy of 16 devices on 2 of 3 nodes", r.SMART)
	}
	if r.RAID == nil || *r.RAID != (types.FleetHealth{Unhealthy: 1, Devices: 5, Nodes: 2, Of: 3}) {
		t.Errorf("RAID = %+v; want 1 unhealthy of 5 arrays on 2 of 3 nodes", r.RAID)
	}

	vms := boundedRollup([]types.FleetNode{up("vm-host-1", types.NodeSummary{}), up("vm-host-2", types.NodeSummary{})})
	if vms.SMART != nil || vms.RAID != nil {
		t.Errorf("a fleet with no SMART devices or arrays reports SMART %+v, RAID %+v; want both omitted", vms.SMART, vms.RAID)
	}
}

// TestRollup_ExtremesNameTheNode: the worst value and who holds it, never a
// mean; a zero that means "nothing to read" never wins; unreachable nodes'
// last values are not candidates; ties keep the first node by name.
func TestRollup_ExtremesNameTheNode(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("alpha", types.NodeSummary{CPUPercent: 44.9, CPUAvailable: true, MemoryPercent: 83.3, LoadPerCore: 1.69, WorstDiskPercent: 70, MaxTempC: 83, MaxTempSource: types.TempGPU}),
		up("bravo", types.NodeSummary{CPUPercent: 44.9, CPUAvailable: true, MemoryPercent: 10, LoadPerCore: 0.2, WorstDiskPercent: 92.1}),
		down("charlie", 30, types.NodeSummary{CPUPercent: 100, CPUAvailable: true, MemoryPercent: 100, WorstDiskPercent: 100, MaxTempC: 120}),
	})
	checks := map[string]struct {
		got  *types.FleetExtreme
		want types.FleetExtreme
	}{
		"cpu (tie goes to alpha)": {r.CPU, types.FleetExtreme{Node: "alpha", Value: 44.9}},
		"memory":                  {r.Memory, types.FleetExtreme{Node: "alpha", Value: 83.3}},
		"load per core":           {r.LoadPerCore, types.FleetExtreme{Node: "alpha", Value: 1.69}},
		"disk":                    {r.Disk, types.FleetExtreme{Node: "bravo", Value: 92.1}},
		"temperature":             {r.Temperature, types.FleetExtreme{Node: "alpha", Value: 83, Kind: types.TempGPU}},
	}
	for name, c := range checks {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %+v, want %+v", name, c.got, c.want)
		}
	}

	cold := boundedRollup([]types.FleetNode{up("vm", types.NodeSummary{CPUPercent: 5, CPUAvailable: true})})
	if cold.Temperature != nil || cold.Disk != nil {
		t.Errorf("zeros that mean no sensor became extremes: temperature %+v, disk %+v", cold.Temperature, cold.Disk)
	}
	if empty := boundedRollup([]types.FleetNode{down("x", 0, types.NodeSummary{})}); empty.CPU != nil || empty.Network != nil {
		t.Errorf("a fleet with nothing reachable reports figures: cpu %+v, network %+v", empty.CPU, empty.Network)
	}
}

// TestRollup_SwapPagingKnownAndUnknown is #134 at fleet scale: the worst
// swap-out rate is taken over the nodes that could measure it, and the ones
// that could not are counted, so a quiet figure is never read as covering
// them.
func TestRollup_SwapPagingKnownAndUnknown(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("ci-node-1", types.NodeSummary{SwapPercent: 51.1, SwapRateAvailable: true, SwapOutRate: 0}),
		up("vm-host-1", types.NodeSummary{SwapPercent: 98.6, SwapRateAvailable: true, SwapOutRate: 4 << 20}),
		up("old", types.NodeSummary{SwapPercent: 100}),
	})
	if r.SwapOut == nil || *r.SwapOut != (types.FleetExtreme{Node: "vm-host-1", Value: 4 << 20}) {
		t.Errorf("SwapOut = %+v, want vm-host-1 at 4 MiB/s", r.SwapOut)
	}
	if r.SwapUnknown != 1 {
		t.Errorf("SwapUnknown = %d, want 1 (the node without counters)", r.SwapUnknown)
	}

	all := boundedRollup([]types.FleetNode{up("old", types.NodeSummary{SwapPercent: 100})})
	if all.SwapOut != nil || all.SwapUnknown != 1 {
		t.Errorf("no node could measure paging: SwapOut %+v, unknown %d; want omitted and 1", all.SwapOut, all.SwapUnknown)
	}
}

// TestRollup_CPUKnownAndUnknown: the busiest node is taken over the nodes
// whose CPU usage was measured, and the ones whose was not are counted -- a
// node on its first collection is not the fleet's idlest, and cannot hide a
// busier one either.
func TestRollup_CPUKnownAndUnknown(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("alpha", types.NodeSummary{CPUPercent: 12, CPUAvailable: true}),
		up("bravo", types.NodeSummary{CPUPercent: 67, CPUAvailable: true}),
		up("starting", types.NodeSummary{}),
		down("gone", 30, types.NodeSummary{}),
	})
	if r.CPU == nil || *r.CPU != (types.FleetExtreme{Node: "bravo", Value: 67}) {
		t.Errorf("CPU = %+v, want bravo at 67", r.CPU)
	}
	if r.CPUUnknown != 1 {
		t.Errorf("CPUUnknown = %d, want 1 (the reachable node with no measurement; the unreachable one is counted as unreachable)", r.CPUUnknown)
	}

	all := boundedRollup([]types.FleetNode{up("starting", types.NodeSummary{}), up("also", types.NodeSummary{})})
	if all.CPU != nil || all.CPUUnknown != 2 {
		t.Errorf("no node could measure CPU: CPU %+v, unknown %d; want omitted and 2", all.CPU, all.CPUUnknown)
	}
}

// TestRollup_NetworkIsAdditive: throughput genuinely sums, over the reachable
// nodes, with the count it was summed over.
func TestRollup_NetworkIsAdditive(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("a", types.NodeSummary{NetRxRate: 100, NetTxRate: 10, NetReported: true}),
		up("b", types.NodeSummary{NetRxRate: 50, NetTxRate: 5, NetReported: true}),
		up("ctr", types.NodeSummary{}),
		down("c", 30, types.NodeSummary{NetRxRate: 1e9, NetReported: true}),
	})
	if r.Network == nil || *r.Network != (types.FleetNet{RxRate: 150, TxRate: 15, Nodes: 2, Of: 3}) {
		t.Errorf("Network = %+v; want 150 rx, 15 tx over 2 of 3 reachable nodes", r.Network)
	}
	if none := boundedRollup([]types.FleetNode{up("ctr", types.NodeSummary{})}); none.Network != nil {
		t.Errorf("Network = %+v for a fleet where no node measures it; want omitted", *none.Network)
	}
}

// TestRollup_BuildsGroupByCommit: identity, not health. The largest group
// leads, and a node that did not state its build is grouped as unknown, never
// with anyone.
func TestRollup_BuildsGroupByCommit(t *testing.T) {
	r := boundedRollup([]types.FleetNode{
		up("a", types.NodeSummary{GitCommit: "a46f6cf"}),
		up("b", types.NodeSummary{GitCommit: "a46f6cf"}),
		up("gitea", types.NodeSummary{GitCommit: "c00eb87"}),
		up("old", types.NodeSummary{}),
		up("dev", types.NodeSummary{GitCommit: unstatedCommit}),
	})
	want := []types.FleetBuild{
		{Commit: "", Count: 2, Nodes: []string{"old", "dev"}},
		{Commit: "a46f6cf", Count: 2, Nodes: []string{"a", "b"}},
		{Commit: "c00eb87", Count: 1, Nodes: []string{"gitea"}},
	}
	if !reflect.DeepEqual(r.Builds, want) {
		t.Errorf("Builds = %+v, want %+v", r.Builds, want)
	}
	if r.BuildGroups != 3 {
		t.Errorf("BuildGroups = %d, want 3", r.BuildGroups)
	}
	if r.Healthy != 5 {
		t.Errorf("build divergence changed health: %d healthy of 5", r.Healthy)
	}
}

// TestRollup_AttentionNamesTheWorstFew: the list is bounded (#15), and the
// bound takes the WORST nodes -- every critical before any unreachable, every
// unreachable before any warning -- by name within a state. The counts still
// cover every node, so the consumer can say how many it was not sent.
func TestRollup_AttentionNamesTheWorstFew(t *testing.T) {
	var nodes []types.FleetNode
	for i := range 10 {
		nodes = append(nodes, up(fmt.Sprintf("w%02d", i), types.NodeSummary{WarningCount: 1, Alerts: []types.Alert{warn("w")}}))
	}
	for i := range 4 {
		nodes = append(nodes, down(fmt.Sprintf("u%02d", i), 60, types.NodeSummary{}))
	}
	for i := range 3 {
		nodes = append(nodes, up(fmt.Sprintf("c%02d", i), types.NodeSummary{CriticalCount: 1, Alerts: []types.Alert{crit("c")}}))
	}
	sortByName(nodes)

	r := boundedRollup(nodes)
	var got []string
	for _, a := range r.Attention {
		got = append(got, a.Name)
	}
	want := []string{"c00", "c01", "c02", "u00", "u01"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Attention = %v, want the three critical then the first two unreachable: %v", got, want)
	}
	if r.Critical+r.Unreachable+r.Warning != 17 {
		t.Errorf("counts = %d critical, %d unreachable, %d warning; want all 17 counted", r.Critical, r.Unreachable, r.Warning)
	}
}

// TestRollup_BuildsAreBounded: at fleet scale a rollout can leave many builds
// and thousands of nodes on each. The largest groups are sent, each naming
// only its first few nodes, with the true sizes beside them.
func TestRollup_BuildsAreBounded(t *testing.T) {
	var nodes []types.FleetNode
	for i := range 12 {
		nodes = append(nodes, up(fmt.Sprintf("n%02d", i), types.NodeSummary{GitCommit: "common"}))
	}
	for i := range 7 {
		nodes = append(nodes, up(fmt.Sprintf("s%02d", i), types.NodeSummary{GitCommit: fmt.Sprintf("c%d", i)}))
	}
	sortByName(nodes)

	r := boundedRollup(nodes)
	if r.BuildGroups != 8 || len(r.Builds) != types.RollupNames {
		t.Fatalf("got %d of %d build groups; want %d of 8", len(r.Builds), r.BuildGroups, types.RollupNames)
	}
	lead := r.Builds[0]
	if lead.Commit != "common" || lead.Count != 12 || !reflect.DeepEqual(lead.Nodes, []string{"n00", "n01", "n02", "n03", "n04"}) {
		t.Errorf("largest group = %+v; want common, 12 nodes, the first five named", lead)
	}
	for _, b := range r.Builds[1:] {
		if b.Count != 1 || len(b.Nodes) != 1 {
			t.Errorf("group %+v: want one node, named", b)
		}
	}
}

// TestRollup_SizeDoesNotGrowWithTheFleet is the property the bounds exist for:
// the roll-up is sent on every poll, so its size must not depend on how many
// nodes it describes. Measured on the encoded form, which is what is sent.
func TestRollup_SizeDoesNotGrowWithTheFleet(t *testing.T) {
	size := func(n int) int {
		var nodes []types.FleetNode
		for i := range n {
			s := types.NodeSummary{GitCommit: fmt.Sprintf("c%d", i%9), WarningCount: 1, Alerts: []types.Alert{warn("disk")}}
			nodes = append(nodes, up(fmt.Sprintf("node-%05d", i), s))
		}
		body, err := json.Marshal(boundedRollup(nodes))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return len(body)
	}
	small, large := size(100), size(10000)
	// The counts gain digits; nothing else may grow.
	if large > small+64 {
		t.Errorf("roll-up is %d bytes at 100 nodes and %d at 10000; it grows with the fleet", small, large)
	}
}

func sortByName(nodes []types.FleetNode) {
	sort.Slice(nodes, func(i, j int) bool { return CompareNodeNames(nodes[i].Name, nodes[j].Name) < 0 })
}

// boundedRollup is the roll-up as a page and GET /api/fleet/rollup carry it.
func boundedRollup(nodes []types.FleetNode) *types.FleetRollup {
	return rollup(nodes, types.RollupNames)
}

// TestRollup_UnboundedNamesEveryNode: a whole-fleet request gets the lists
// whole, with the same counts, so a consumer written before paging still
// reads "and N more" and a group's size from the lengths.
func TestRollup_UnboundedNamesEveryNode(t *testing.T) {
	var nodes []types.FleetNode
	for i := range 12 {
		nodes = append(nodes, up(fmt.Sprintf("w%02d", i), types.NodeSummary{GitCommit: fmt.Sprintf("c%d", i%7), WarningCount: 1, Alerts: []types.Alert{warn("w")}}))
	}
	sortByName(nodes)
	r := rollup(nodes, 0)
	if len(r.Attention) != 12 || len(r.Builds) != 7 || r.BuildGroups != 7 {
		t.Fatalf("unbounded roll-up has %d attention and %d of %d build groups; want 12, 7 of 7", len(r.Attention), len(r.Builds), r.BuildGroups)
	}
	for _, b := range r.Builds {
		if b.Count != len(b.Nodes) {
			t.Errorf("group %+v names %d of %d nodes; want all", b, len(b.Nodes), b.Count)
		}
	}
}
