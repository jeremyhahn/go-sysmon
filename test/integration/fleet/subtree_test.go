//go:build integration

package fleet

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestTree_ASubtreeThatDoesNotHoldTogetherIsNotComposed polls a child for
// every way its subtree can fail to hold together, and for every way it can
// be refused, and checks that none of them is composed into the fleet, each
// named on the child's card. A child is another process on the network, and
// nothing it sends is trusted.
func TestTree_ASubtreeThatDoesNotHoldTogetherIsNotComposed(t *testing.T) {
	type mutation func(sub *types.FleetSubtree)
	leaves := func() []types.FleetIndexEntry {
		return []types.FleetIndexEntry{entry("x-a", types.NodeStatusHealthy, 0), entry("x-b", types.NodeStatusUnreachable, 30)}
	}
	long := strings.Repeat("h", 254)
	cases := map[string]struct {
		mutate mutation
		want   string
	}{
		"c-norollup":   {func(s *types.FleetSubtree) { s.Rollup = nil }, "no roll-up"},
		"c-deepnode":   {func(s *types.FleetSubtree) { s.Nodes[1].Depth = 9 }, "a node at depth 9"},
		"c-badname":    {func(s *types.FleetSubtree) { s.Nodes[1].Name = "not a name" }, "a node name that is not a usable node name"},
		"c-badstatus":  {func(s *types.FleetSubtree) { s.Nodes[1].Status = "sideways" }, "a node status that is not one"},
		"c-longhost":   {func(s *types.FleetSubtree) { s.Nodes[1].Hostname = long }, "a host name or build longer than any is"},
		"c-longreason": {func(s *types.FleetSubtree) { s.Nodes[2].Reasons = []string{strings.Repeat("r", 1025)} }, "a reason longer than 1024 bytes"},
		"c-twoselves":  {func(s *types.FleetSubtree) { s.Nodes[1].Depth = 0 }, "2 nodes at depth 0"},
		"c-noaggs":     {func(s *types.FleetSubtree) { s.Aggregators = nil }, "it names no aggregators"},
		"c-badagg":     {func(s *types.FleetSubtree) { s.Aggregators = []string{"x"} }, "an aggregator ID that is not one"},
		"c-miscount":   {func(s *types.FleetSubtree) { s.Rollup.Nodes = 7 }, "its roll-up counts 7 nodes and its index lists 3"},
		"c-states":     {func(s *types.FleetSubtree) { s.Rollup.Healthy--; s.Rollup.Warning++ }, "its roll-up's states are not its index's"},
		"c-negative":   {func(s *types.FleetSubtree) { s.Rollup.CPUUnknown = -1 }, "a negative count"},
		"c-overcount":  {func(s *types.FleetSubtree) { s.Rollup.Reporting = 9 }, "a count larger than what it counts"},
		"c-absence":    {func(s *types.FleetSubtree) { s.Rollup.LongestAbsenceSeconds = -1 }, "an absence that is not a duration"},
		"c-extreme":    {func(s *types.FleetSubtree) { s.Rollup.CPU = &types.FleetExtreme{Node: "not a name", Value: 1} }, "an extreme that is not a reading of a node"},
		"c-power":      {func(s *types.FleetSubtree) { s.Rollup.Power = &types.FleetSum{Value: 5, Nodes: 1, Of: 9} }, "a power total whose denominator"},
		"c-network":    {func(s *types.FleetSubtree) { s.Rollup.Network = &types.FleetNet{RxRate: 1, Nodes: 1, Of: 9} }, "a network total whose denominator"},
		"c-smart": {func(s *types.FleetSubtree) {
			s.Rollup.SMART = &types.FleetHealth{Devices: 1, Unhealthy: 2, Nodes: 1, Of: 2}
		}, "a device count whose denominator"},
		"c-attention":   {func(s *types.FleetSubtree) { s.Rollup.Attention[0].Status = "fine" }, "an attention entry that is not one"},
		"c-emptybuild":  {func(s *types.FleetSubtree) { s.Rollup.Builds[0].Count = 0 }, "a build group that is not one"},
		"c-overbuilt":   {func(s *types.FleetSubtree) { s.Rollup.Builds[0].Count = 5 }, "build groups that count more nodes than it has reachable"},
		"c-buildgroups": {func(s *types.FleetSubtree) { s.Rollup.BuildGroups = 0 }, "more build groups than it says it has"},
	}
	refused := map[string]struct {
		mutate mutation
		want   string
	}{
		"r-toodeep": {func(s *types.FleetSubtree) { s.Nodes[1].Depth = 4 }, "levels deep, beyond the 4 an aggregator composes"},
		"r-crowded": {func(s *types.FleetSubtree) {
			for range 1025 {
				s.Aggregators = append(s.Aggregators, instanceID(20))
			}
		}, "aggregators below it, more than the 1024 a fleet tree may have"},
	}

	var peers []peer
	i := 0
	for name, tc := range cases {
		c := newChild(t, name, i, leaves())
		sub := validSubtree(name, c.id, leaves())
		tc.mutate(&sub)
		c.handle("/api/fleet/subtree", jsonAnswer(sub))
		peers = append(peers, peer{name, c.URL})
		i++
	}
	for name, tc := range refused {
		c := newChild(t, name, i, leaves())
		sub := validSubtree(name, c.id, leaves())
		tc.mutate(&sub)
		c.handle("/api/fleet/subtree", jsonAnswer(sub))
		peers = append(peers, peer{name, c.URL})
		i++
	}

	// A child whose subtree names this aggregator: a loop, found from the ID
	// its own poll carried.
	looped := newChild(t, "r-loop", i, leaves())
	looped.handle("/api/fleet/subtree", func(w http.ResponseWriter, r *http.Request) {
		sub := validSubtree("r-loop", looped.id, leaves())
		sub.Aggregators = append(sub.Aggregators, r.Header.Get("X-Sysmon-Fleet-Ancestry"))
		jsonAnswer(sub)(w, r)
	})
	// A child that refuses this aggregator's poll as a loop.
	refusing := newChild(t, "r-refuses", i+1, leaves())
	refusing.handle("/api/fleet/subtree", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Sysmon-Fleet-Refusal", "depth")
		http.Error(w, "too deep", http.StatusLoopDetected)
	})
	// A child that is this aggregator under another name.
	self := newStandIn(t)
	self.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		s := leafSummary("r-self")
		s.Aggregates = 2
		s.InstanceID = r.Header.Get("X-Sysmon-Fleet-Ancestry")
		jsonAnswer(s)(w, r)
	})
	// A child whose subtree cannot be read at all.
	unreadable := newChild(t, "u-broken", i+2, leaves())
	unreadable.handle("/api/fleet/subtree", rawAnswer(http.StatusInternalServerError, "text/plain", "down"))
	// And one that composes, with lists longer than any it may send: they are
	// cut to their bounds, not refused.
	var many []types.FleetIndexEntry
	for j := range 8 {
		many = append(many, entry("v-"+strconv.Itoa(j), types.NodeStatusWarning, 0))
	}
	fine := newChild(t, "v-fine", i+3, many)
	longLists := validSubtree("v-fine", fine.id, many)
	longLists.Nodes[1].Reasons = []string{"1", "2", "3", "4", "5", "6", "7"}
	fine.handle("/api/fleet/subtree", jsonAnswer(longLists))
	peers = append(peers, peer{"r-loop", looped.URL}, peer{"r-refuses", refusing.URL}, peer{"r-self", self.URL},
		peer{"u-broken", unreadable.URL}, peer{"v-fine", fine.URL})

	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500, peers, "")})
	v := awaitFleet(t, agg.Addr, "the one poll of every child", polled)

	for name, tc := range cases {
		if n := node(t, v, name); !strings.Contains(n.SubtreeError, "sent an invalid subtree: "+tc.want) {
			t.Errorf("%s: subtree error %q; want it refused for %q", name, n.SubtreeError, tc.want)
		}
	}
	for name, tc := range refused {
		if n := node(t, v, name); !strings.Contains(n.SubtreeError, tc.want) {
			t.Errorf("%s: subtree error %q; want it refused for %q", name, n.SubtreeError, tc.want)
		}
	}
	for name, want := range map[string]string{
		"r-loop":    "the fleet's peer lists loop back to this aggregator through r-loop",
		"r-refuses": "refused: the fleet tree is deeper than 4 levels there",
		"r-self":    "this peer is this aggregator itself",
		"u-broken":  "returned HTTP 500",
	} {
		if n := node(t, v, name); !strings.Contains(n.SubtreeError, want) {
			t.Errorf("%s: subtree error %q; want %q", name, n.SubtreeError, want)
		}
	}
	for _, n := range v.Nodes {
		if strings.HasPrefix(n.Name, "x-") {
			t.Errorf("a node of a refused or invalid subtree is in the fleet: %+v", n)
		}
	}
	// Only v-fine composed: its eight nodes, beside hub and its peers.
	want := 1 + len(peers) + len(many)
	if v.Rollup.Nodes != want {
		t.Errorf("roll-up counts %d nodes; want %d: every direct peer and v-fine's subtree alone", v.Rollup.Nodes, want)
	}
	if v.Rollup.SubtreeErrors < 5 {
		t.Errorf("roll-up counts %d refused subtrees; want at least the five refused children", v.Rollup.SubtreeErrors)
	}
	if n := node(t, v, "v-3"); n.Via != "v-fine" || n.Status != types.NodeStatusWarning {
		t.Errorf("v-3 = %+v; want it listed via v-fine, warning", n)
	}

	// What this aggregator serves its own parent: v-fine's nodes one level
	// further down, and every aggregator named below it, refused or not.
	var sub types.FleetSubtree
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/subtree", &sub)
	depths := map[string]int{}
	for _, e := range sub.Nodes {
		depths[e.Name] = e.Depth
	}
	if depths["hub"] != 0 || depths["v-fine"] != 1 || depths["v-0"] != 2 {
		t.Errorf("served subtree depths = hub %d, v-fine %d, v-0 %d; want 0, 1, 2", depths["hub"], depths["v-fine"], depths["v-0"])
	}
	if len(sub.Aggregators) < 3 {
		t.Errorf("served aggregators = %d; want this one and the children it heard from", len(sub.Aggregators))
	}
	if sub.Rollup == nil || len(sub.Rollup.Attention) > types.RollupNames {
		t.Errorf("served roll-up attention = %d entries; want at most %d", len(sub.Rollup.Attention), types.RollupNames)
	}
}

// TestTree_AChildsNodesArePagedDrilledAndRelayed checks every request an
// aggregator answers for a node below a child: a page's figures, fetched from
// the child; a drill-in, settings and a history range, relayed through it; and
// each answer the child can give that must not be passed on as it came.
func TestTree_AChildsNodesArePagedDrilledAndRelayed(t *testing.T) {
	entries := []types.FleetIndexEntry{
		entry("n-ok", types.NodeStatusHealthy, 0),
		entry("n-warn", types.NodeStatusWarning, 0),
		entry("n-down", types.NodeStatusUnreachable, 45),
		entry("n-flip", types.NodeStatusHealthy, 0),
		entry("n-gone", types.NodeStatusHealthy, 0),
		entry("n-dup", types.NodeStatusHealthy, 0),
		entry("n-flag", types.NodeStatusHealthy, 0),
		entry("n-loop", types.NodeStatusHealthy, 0),
		entry("n-bad", types.NodeStatusHealthy, 0),
	}
	entries[6].NameConflict = true
	child := newChild(t, "mid", 0, entries)
	flipped := child.nodes["n-flip"]
	flipped.Status = types.NodeStatusWarning
	flipped.WarningCount = 1
	child.setNode("n-flip", flipped)
	child.dropNode("n-gone")

	// The child's answers for relayed requests, one per node and endpoint.
	child.handle("/api/fleet/n-ok/snapshot", jsonAnswer(types.PeerSnapshot{Reachable: true,
		Snapshot: &types.Snapshot{NodeName: "n-ok", Host: types.HostInfo{Hostname: "n-ok.example"}}}))
	child.handle("/api/fleet/n-warn/snapshot", rawAnswer(http.StatusNotFound, "text/plain", "gone"))
	child.handle("/api/fleet/n-flip/snapshot", rawAnswer(http.StatusConflict, "text/plain", "named twice"))
	child.handle("/api/fleet/n-down/snapshot", rawAnswer(http.StatusBadGateway, "text/plain", "unreachable"))
	child.handle("/api/fleet/n-ok/config", jsonAnswer(types.ConfigView{ConfigFile: "/etc/sysmon.yaml", Entries: []types.ConfigEntry{
		{Section: "server", Key: "server.addr", Value: "10.0.0.9:8081", Source: types.ConfigSourceFile},
		// An older build keyed a peer by its URL.
		{Section: "fleet", Key: "fleet.peers.http://10.0.0.8:8081", Value: "http://10.0.0.8:8081", Source: types.ConfigSourceFile},
	}}))
	child.handle("/api/fleet/n-warn/config", rawAnswer(http.StatusNotFound, "text/plain", "gone"))
	child.handle("/api/history", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("node") {
		case "n-ok":
			jsonAnswer(types.History{Enabled: true, Node: "n-ok"})(w, r)
		case "n-warn":
			rawAnswer(http.StatusOK, "text/html", "<html>not json</html>")(w, r)
		case "n-flip":
			rawAnswer(http.StatusBadRequest, "application/json", `{"error":"bad range"}`)(w, r)
		case "n-down":
			rawAnswer(http.StatusInternalServerError, "text/plain", "boom")(w, r)
		case "n-gone":
			jsonAnswer(types.History{Enabled: true, Node: "someone-else"})(w, r)
		case "n-loop":
			w.Header().Set("X-Sysmon-Fleet-Refusal", "loop")
			http.Error(w, "loop", http.StatusLoopDetected)
		default:
			rawAnswer(http.StatusOK, "application/json", "[1,2")(w, r)
		}
	})

	// n-dup is also a direct peer of the root: the name is carried twice.
	dup := newStandIn(t)
	dup.handle("/api/summary", jsonAnswer(leafSummary("n-dup")))
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500,
		[]peer{{"mid", child.URL}, {"n-dup", dup.URL}}, "")})
	awaitFleet(t, agg.Addr, "the one poll", polled)

	// The whole fleet: subtree nodes from the child's index, nothing fetched.
	whole := fleetOf(t, agg.Addr, "")
	if n := node(t, whole, "n-ok"); n.Source != types.SummarySourceIndex || n.Via != "mid" {
		t.Errorf("n-ok in the whole view = source %q via %q; want its index entry, via mid", n.Source, n.Via)
	}
	if got := child.hitsOf("/api/fleet/nodes"); got != 0 {
		t.Errorf("the whole-fleet view asked the child for nodes %d times; want never", got)
	}
	if whole.Rollup.NameConflicts != 3 {
		t.Errorf("roll-up name conflicts = %d; want n-dup twice and n-flag", whole.Rollup.NameConflicts)
	}

	// A page: its subtree nodes' figures fetched from the child.
	page := fleetOf(t, agg.Addr, "?limit=20")
	if n := node(t, page, "n-ok"); n.Source == types.SummarySourceIndex || n.Hostname != "n-ok.example" || n.Via != "mid" {
		t.Errorf("n-ok on a page = %+v; want the child's figures for it", n)
	}
	if n := node(t, page, "n-flip"); !strings.Contains(n.Error, "its figures changed since the last poll") {
		t.Errorf("n-flip on a page = %q; want the disagreement named", n.Error)
	}
	if n := node(t, page, "n-gone"); !strings.Contains(n.Error, "not returned by mid") {
		t.Errorf("n-gone on a page = %q; want it named as not returned", n.Error)
	}
	if n := node(t, page, "n-down"); n.Reachable || n.AgeSeconds < 45 || !strings.Contains(n.Error, "unreachable on n-down") {
		t.Errorf("n-down on a page = %+v; want unreachable, 45s old, its reason", n)
	}
	for _, n := range page.Nodes {
		if n.Name == "unasked" {
			t.Error("a node the child sent unasked is on the page")
		}
	}
	// The same page again: the figures that agreed are reused, and only the
	// two that could not be shown are asked for again.
	fleetOf(t, agg.Addr, "?limit=20")
	if got := child.queryOf("/api/fleet/nodes"); got != "name=n-flip&name=n-gone" {
		t.Errorf("the same page again asked the child for %q; want only the two it could not show", got)
	}
	var nodes types.FleetNodes
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/nodes?name=n-ok&name=n-dup", &nodes)
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Name != "n-ok" {
		t.Errorf("GET /api/fleet/nodes for n-ok and the conflicted n-dup = %v; want n-ok alone", nodes.Nodes)
	}

	// Drill-ins relayed through mid.
	var ps types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/n-ok/snapshot", &ps)
	if !ps.Reachable || ps.Snapshot == nil || ps.Snapshot.NodeName != "n-ok" {
		t.Errorf("n-ok's drill-in through mid = %+v; want its snapshot", ps)
	}
	if got := child.headerOf("/api/fleet/n-ok/snapshot", "X-Sysmon-Fleet-Budget-Ms"); got == "" {
		t.Error("a relayed drill-in carried no budget")
	}
	for path, status := range map[string]int{
		"/api/fleet/n-warn/snapshot": http.StatusNotFound,
		"/api/fleet/n-flip/snapshot": http.StatusConflict,
		"/api/fleet/n-down/snapshot": http.StatusBadGateway,
		"/api/fleet/n-dup/snapshot":  http.StatusConflict,
		"/api/fleet/n-flag/snapshot": http.StatusConflict,
		"/api/fleet/n-warn/config":   http.StatusNotFound,
		"/api/fleet/n-dup/config":    http.StatusConflict,
	} {
		if got, _, body := request(t, agg.Addr, path, nil); got != status {
			t.Errorf("GET %s = %d (%s); want %d", path, got, body, status)
		}
	}

	var cfg types.ConfigView
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/n-ok/config", &cfg)
	if cfg.ConfigFile != "(configured)" {
		t.Errorf("relayed config_file = %q; want the path withheld", cfg.ConfigFile)
	}
	for _, e := range cfg.Entries {
		if strings.Contains(e.Value, "10.0.0.") || strings.Contains(e.Key, "10.0.0.") {
			t.Errorf("a relayed setting carries an address: %+v", e)
		}
	}

	// History ranges, relayed through mid.
	var h types.History
	harness.GetJSON(t, "http://"+agg.Addr+"/api/history?node=n-ok&metric=cpu", &h)
	if !h.Enabled || h.Node != "n-ok" {
		t.Errorf("n-ok's history through mid = %+v; want the child's answer", h)
	}
	for node, status := range map[string]int{
		"n-warn": http.StatusBadGateway, "n-flip": http.StatusBadRequest, "n-down": http.StatusBadGateway,
		"n-gone": http.StatusBadGateway, "n-flag": http.StatusConflict, "n-dup": http.StatusConflict,
		"n-loop": http.StatusLoopDetected, "n-bad": http.StatusBadGateway,
	} {
		if got, _, body := request(t, agg.Addr, "/api/history?node="+node, nil); got != status {
			t.Errorf("GET /api/history?node=%s = %d (%s); want %d", node, got, body, status)
		}
	}
	if status, _, _ := request(t, agg.Addr, "/api/history?node=n-ok", map[string]string{"X-Sysmon-Fleet-Ancestry": "bogus"}); status != http.StatusBadRequest {
		t.Errorf("a relayed history request with a malformed chain = %d; want 400", status)
	}
	var unnamed types.History
	harness.GetJSON(t, "http://"+agg.Addr+"/api/history", &unnamed)
	if unnamed.Enabled {
		t.Errorf("history with no node = %+v; want this build's own answer", unnamed)
	}
	for _, own := range []string{"hub", "mid", "nobody"} {
		var local types.History
		harness.GetJSON(t, "http://"+agg.Addr+"/api/history?node="+own, &local)
		if local.Enabled {
			t.Errorf("history of %s = %+v; want this build's own answer, which keeps none", own, local)
		}
	}
}
