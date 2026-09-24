package fleet

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestView_RollupIsTheWholeFleetWhateverThePage is the contract the roll-up's
// header states -- "N nodes: ..." -- and the reason it is not derived from the
// page: every query, including one matching nothing and one past the end,
// carries the same roll-up as the unpaged view.
func TestView_RollupIsTheWholeFleetWhateverThePage(t *testing.T) {
	p := largeFleet(t, 250)
	whole := p.View(context.Background(), Query{Limit: 25})
	if whole.Rollup == nil || whole.Rollup.Nodes != 251 {
		t.Fatalf("roll-up = %+v; want 251 nodes (250 peers and this one)", whole.Rollup)
	}

	for _, q := range []Query{
		{Offset: 225, Limit: 25},
		{Search: "node-0001", Limit: 5},
		{Search: "matches-nothing", Limit: 25},
		{Offset: 10000, Limit: 25},
	} {
		v := p.View(context.Background(), q)
		// AgeSeconds is measured at request time, so the absence figures move
		// between two calls; everything else must be identical.
		got, want := *v.Rollup, *whole.Rollup
		got.LongestAbsenceSeconds, want.LongestAbsenceSeconds = 0, 0
		got.Attention, want.Attention = withoutAges(got.Attention), withoutAges(want.Attention)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("View(%+v).Rollup differs from the whole fleet's:\n got %+v\nwant %+v", q, got, want)
		}
	}
}

func withoutAges(in []types.NodeAttention) []types.NodeAttention {
	out := make([]types.NodeAttention, len(in))
	for i, a := range in {
		a.AgeSeconds = 0
		out[i] = a
	}
	return out
}

// TestView_DescribesItsPage: the page carries the counts a client needs to draw
// its controls -- how many matched, how many there are -- and echoes the query
// it answered, so a response can be matched to the request that asked for it.
func TestView_DescribesItsPage(t *testing.T) {
	p := largeFleet(t, 250)

	v := p.View(context.Background(), Query{Search: "NODE-0001", Offset: 5, Limit: 3})
	want := types.FleetPage{Query: "NODE-0001", Offset: 5, Limit: 3, Matched: 10, Total: 251}
	if v.Page == nil || *v.Page != want {
		t.Fatalf("Page = %+v, want %+v", v.Page, want)
	}
	if got := names(v.Nodes); !reflect.DeepEqual(got, []string{"node-00015", "node-00016", "node-00017"}) {
		t.Errorf("Nodes = %v, want node-00015..17", got)
	}
}

// TestView_SelfIsSentWhateverThePage: the node serving the view is named
// outside the page. Its build is what every node's build_match was compared
// with, and a consumer looking at page four still needs to say which build
// "other build" means.
func TestView_SelfIsSentWhateverThePage(t *testing.T) {
	p := largeFleet(t, 250)
	for _, q := range []Query{{}, {Offset: 200, Limit: 25}, {Search: "matches-nothing"}} {
		v := p.View(context.Background(), q)
		if v.Self == nil || v.Self.Name != "local" || !v.Self.Self || v.Self.GitCommit != "a46f6cf" {
			t.Errorf("View(%+v).Self = %+v; want the local node with its build", q, v.Self)
		}
	}
}

// TestView_NoFleetIsDisabled: with no peers there is no fleet, no page and no
// roll-up -- the standalone shape, whatever was asked.
func TestView_NoFleetIsDisabled(t *testing.T) {
	p := largeFleet(t, 0)
	v := p.View(context.Background(), Query{Limit: 25})
	if v.Enabled || v.Page != nil || v.Self != nil || v.Rollup != nil || len(v.Nodes) != 0 {
		t.Errorf("View with no peers = %+v; want the disabled view and nothing else", v)
	}
	if r := p.Rollup(); r != nil {
		t.Errorf("Rollup with no peers = %+v; want nil", r)
	}
}

// TestRollup_MatchesTheView: GET /api/fleet/rollup and GET /api/fleet serve
// one roll-up, not two computations that could drift.
func TestRollup_MatchesTheView(t *testing.T) {
	p := largeFleet(t, 40)
	got, want := *p.Rollup(), *p.View(context.Background(), Query{Limit: 1}).Rollup
	got.LongestAbsenceSeconds, want.LongestAbsenceSeconds = 0, 0
	got.Attention, want.Attention = withoutAges(got.Attention), withoutAges(want.Attention)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rollup() = %+v\nwant the view's %+v", got, want)
	}
}

// TestHas answers membership from the configuration: this node and every
// configured peer, polled or not, and nothing else.
func TestHas(t *testing.T) {
	p := largeFleet(t, 3)
	for name, want := range map[string]bool{
		"local":      true,
		"node-00000": true,
		"node-00002": true,
		"node-00003": false,
		"":           false,
		"NODE-00000": false, // names are exact; only order and search fold case
	} {
		if got := p.Has(name); got != want {
			t.Errorf("Has(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestView_UnpagedRollupNamesEveryNode: a request with no limit is the whole
// fleet, and its roll-up names every node needing attention and every build
// group whole -- the shape every consumer written before paging reads, a
// dashboard left open across an upgrade included. The counts are the same.
func TestView_UnpagedRollupNamesEveryNode(t *testing.T) {
	p := largeFleet(t, 250)
	whole, paged := p.View(context.Background(), Query{}).Rollup, p.View(context.Background(), Query{Limit: 25}).Rollup
	needing := whole.Critical + whole.Unreachable + whole.Warning
	if len(whole.Attention) != needing {
		t.Errorf("unpaged attention names %d of %d nodes; want all", len(whole.Attention), needing)
	}
	if len(paged.Attention) != types.RollupNames {
		t.Errorf("paged attention names %d; want %d", len(paged.Attention), types.RollupNames)
	}
	// Any parameter at all is a request for less than the fleet, and bounded.
	for _, q := range []Query{{Search: "node-00001"}, {Offset: 3}} {
		if got := len(p.View(context.Background(), q).Rollup.Attention); got != types.RollupNames {
			t.Errorf("View(%+v) attention names %d; want %d", q, got, types.RollupNames)
		}
	}
	for _, b := range whole.Builds {
		if len(b.Nodes) != b.Count {
			t.Errorf("unpaged build %s names %d of %d nodes", b.Commit, len(b.Nodes), b.Count)
		}
	}
	if whole.Healthy != paged.Healthy || whole.Warning != paged.Warning || whole.Unreachable != paged.Unreachable {
		t.Errorf("counts differ between the unpaged and paged roll-ups: %+v vs %+v", whole, paged)
	}
}

// TestView_PageCarriesEveryStampedField: a page is cut from the stamped list,
// so every node on it carries the aggregator's comparisons -- status, build
// agreement and name agreement (#162) -- exactly as the unpaged list does.
func TestView_PageCarriesEveryStampedField(t *testing.T) {
	p := largeFleet(t, 60)
	whole := p.View(context.Background(), Query{})
	byName := map[string]types.FleetNode{}
	for _, n := range whole.Nodes {
		byName[n.Name] = n
	}
	for _, q := range []Query{{Offset: 20, Limit: 10}, {Search: "node-0004", Limit: 5}} {
		for _, n := range p.View(context.Background(), q).Nodes {
			w := byName[n.Name]
			if n.Status == "" || n.BuildMatch == "" || n.NameMatch == "" ||
				n.Status != w.Status || n.BuildMatch != w.BuildMatch || n.NameMatch != w.NameMatch {
				t.Errorf("View(%+v) node %s = status %q build %q name %q; unpaged has %q %q %q",
					q, n.Name, n.Status, n.BuildMatch, n.NameMatch, w.Status, w.BuildMatch, w.NameMatch)
			}
		}
	}
}

// withoutClock clears what View measures at the moment it is called -- ages,
// and the absences built from them -- so two views of one state compare.
func withoutClock(v types.Fleet) types.Fleet {
	nodes := append([]types.FleetNode(nil), v.Nodes...)
	for i := range nodes {
		nodes[i].AgeSeconds = 0
	}
	v.Nodes = nodes
	if v.Self != nil {
		self := *v.Self
		self.AgeSeconds = 0
		v.Self = &self
	}
	if v.Rollup != nil {
		r := *v.Rollup
		r.LongestAbsenceSeconds = 0
		r.Attention = withoutAges(r.Attention)
		v.Rollup = &r
	}
	return v
}

// TestView_TheFlatPathIsTheTreePathOnAFlatFleet: a fleet with no child
// aggregators is served by its own fast path, which pays nothing for tree
// support. It must give exactly the answer the tree path gives the same fleet,
// for every shape of request.
func TestView_TheFlatPathIsTheTreePathOnAFlatFleet(t *testing.T) {
	p := largeFleet(t, 250)
	g := p.published()
	if len(g.subtrees.childURL) != 0 {
		t.Fatal("the fixture is not a flat fleet")
	}
	for _, q := range []Query{
		{}, {Limit: 25}, {Offset: 225, Limit: 25}, {Search: "NODE-0001", Limit: 5},
		{Search: "node-00", Offset: 10}, {Limit: 25, Anchor: "node-00200"}, {Offset: 9999, Limit: 25},
	} {
		cached, live := p.flatView(g, q), p.flatViewLive(g, q)
		tree := p.treeView(context.Background(), g, q)
		for name, v := range map[string]types.Fleet{"the cached flat path": cached, "the whole flat path": live} {
			if !reflect.DeepEqual(withoutClock(v), withoutClock(tree)) {
				t.Errorf("View(%+v): %s and the tree path disagree:\n%+v\n%+v", q, name, v.Rollup, tree.Rollup)
			}
			// And the ages it cleared agree too: the cache ages its readings
			// by the time since the refresh.
			for i := range v.Nodes {
				if d := v.Nodes[i].AgeSeconds - tree.Nodes[i].AgeSeconds; d > 0.5 || d < -0.5 {
					t.Errorf("View(%+v): %s ages %s at %v, the tree path at %v", q, name, v.Nodes[i].Name, v.Nodes[i].AgeSeconds, tree.Nodes[i].AgeSeconds)
				}
			}
			if d := v.Rollup.LongestAbsenceSeconds - tree.Rollup.LongestAbsenceSeconds; d > 0.5 || d < -0.5 {
				t.Errorf("View(%+v): %s's longest absence %v, the tree path's %v", q, name, v.Rollup.LongestAbsenceSeconds, tree.Rollup.LongestAbsenceSeconds)
			}
		}
	}
}

// TestView_PageCarriesHistoryState is TestView_PageCarriesEveryStampedField
// for the history gate's decision (#159): a node shown on a page carries the
// same history state and reason as in the whole view -- a refused peer is not
// shown as recorded because it was reached by paging or search.
func TestView_PageCarriesHistoryState(t *testing.T) {
	p := largeFleet(t, 60)
	p.Observe(func(string, *types.Snapshot) {}, func() time.Duration { return time.Hour })
	states := []types.PeerHistoryState{types.PeerHistoryRecorded, types.PeerHistoryRefused, types.PeerHistoryUnverified}
	for i, peer := range p.cfgFn().Peers {
		p.historyStatus.Store(peer.Name, historyDecision{url: peer.URL, status: types.PeerHistory{
			State: states[i%len(states)], Reason: "reason for " + peer.Name,
		}})
	}

	whole := p.View(context.Background(), Query{})
	byName := map[string]types.FleetNode{}
	for _, n := range whole.Nodes {
		byName[n.Name] = n
	}
	for _, q := range []Query{{Offset: 20, Limit: 10}, {Search: "node-0004", Limit: 5}, {Anchor: "node-00031", Limit: 7}} {
		page := p.View(context.Background(), q).Nodes
		if len(page) == 0 {
			t.Fatalf("View(%+v) returned no nodes", q)
		}
		for _, n := range page {
			if n.Self {
				continue
			}
			w := byName[n.Name]
			if n.History == nil || w.History == nil || *n.History != *w.History {
				t.Errorf("View(%+v) node %s history = %+v; unpaged has %+v", q, n.Name, n.History, w.History)
			}
		}
	}
}

// TestFlatView_FallsBackWhenTheCacheIsNotTheFleet: the refresh's cache is
// used only while it still describes the fleet. A reload that replaced the
// peer list, or a build of this node other than the one the peers were
// compared with, is served the long way -- and correctly.
func TestFlatView_FallsBackWhenTheCacheIsNotTheFleet(t *testing.T) {
	p := largeFleet(t, 30)
	g := p.published()
	if g.flat == nil {
		t.Fatal("a flat fleet was published with no cache")
	}

	cfg := p.cfgFn()
	reloaded := cfg
	reloaded.Peers = append([]config.Peer{{Name: "aaa", URL: "https://10.9.9.9:8081"}}, cfg.Peers...)
	p.cfgFn = func() config.Fleet { return reloaded }
	if v := p.View(context.Background(), Query{Limit: 5}); v.Page.Total != 32 || v.Nodes[0].Name != "aaa" {
		t.Errorf("after a reload = %d nodes, first %q; want 32 from aaa", v.Page.Total, v.Nodes[0].Name)
	}
	p.cfgFn = func() config.Fleet { return cfg }

	g.flat.selfCommit = "another-build"
	v := p.View(context.Background(), Query{Limit: 5})
	if want := compareBuild(v.Self.GitCommit, v.Nodes[0].GitCommit); v.Nodes[0].BuildMatch != want {
		t.Errorf("after this node's build changed, build_match = %q, want %q against the running build", v.Nodes[0].BuildMatch, want)
	}
}

// TestNodes_AReloadedPeerListIsInOrder: the name order is sorted once per
// refresh, for the peer list the refresh published. A reload that replaces
// the list before the next refresh is sorted then and there, so the view is in
// order either way.
func TestNodes_AReloadedPeerListIsInOrder(t *testing.T) {
	p := largeFleet(t, 20)
	cfg := p.cfgFn()
	reloaded := cfg
	reloaded.Peers = append([]config.Peer{{Name: "aaa", URL: "https://10.9.9.9:8081"}, {Name: "zzz", URL: "https://10.9.9.8:8081"}}, cfg.Peers...)
	p.cfgFn = func() config.Fleet { return reloaded }

	nodes, _ := p.nodes(p.published())
	if len(nodes) != 23 || nodes[0].Name != "aaa" || nodes[len(nodes)-1].Name != "zzz" {
		t.Fatalf("nodes after a reload = %d, first %q, last %q; want 23 from aaa to zzz", len(nodes), nodes[0].Name, nodes[len(nodes)-1].Name)
	}
	for i := 1; i < len(nodes); i++ {
		if CompareNodeNames(nodes[i-1].Name, nodes[i].Name) >= 0 {
			t.Fatalf("out of order at %d: %q before %q", i, nodes[i-1].Name, nodes[i].Name)
		}
	}
	if find(t, nodes, "aaa").Error != "not polled yet" {
		t.Error("a peer added by the reload is not shown as not yet polled")
	}
}
