package fleet

import (
	"reflect"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestView_RollupIsTheWholeFleetWhateverThePage is the contract the roll-up's
// header states -- "N nodes: ..." -- and the reason it is not derived from the
// page: every query, including one matching nothing and one past the end,
// carries the same roll-up as the unpaged view.
func TestView_RollupIsTheWholeFleetWhateverThePage(t *testing.T) {
	p := largeFleet(t, 250)
	whole := p.View(Query{Limit: 25})
	if whole.Rollup == nil || whole.Rollup.Nodes != 251 {
		t.Fatalf("roll-up = %+v; want 251 nodes (250 peers and this one)", whole.Rollup)
	}

	for _, q := range []Query{
		{Offset: 225, Limit: 25},
		{Search: "node-0001", Limit: 5},
		{Search: "matches-nothing", Limit: 25},
		{Offset: 10000, Limit: 25},
	} {
		v := p.View(q)
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

	v := p.View(Query{Search: "NODE-0001", Offset: 5, Limit: 3})
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
		v := p.View(q)
		if v.Self == nil || v.Self.Name != "local" || !v.Self.Self || v.Self.GitCommit != "a46f6cf" {
			t.Errorf("View(%+v).Self = %+v; want the local node with its build", q, v.Self)
		}
	}
}

// TestView_NoFleetIsDisabled: with no peers there is no fleet, no page and no
// roll-up -- the standalone shape, whatever was asked.
func TestView_NoFleetIsDisabled(t *testing.T) {
	p := largeFleet(t, 0)
	v := p.View(Query{Limit: 25})
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
	got, want := *p.Rollup(), *p.View(Query{Limit: 1}).Rollup
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
	whole, paged := p.View(Query{}).Rollup, p.View(Query{Limit: 25}).Rollup
	needing := whole.Critical + whole.Unreachable + whole.Warning
	if len(whole.Attention) != needing {
		t.Errorf("unpaged attention names %d of %d nodes; want all", len(whole.Attention), needing)
	}
	if len(paged.Attention) != types.RollupNames {
		t.Errorf("paged attention names %d; want %d", len(paged.Attention), types.RollupNames)
	}
	// Any parameter at all is a request for less than the fleet, and bounded.
	for _, q := range []Query{{Search: "node-00001"}, {Offset: 3}} {
		if got := len(p.View(q).Rollup.Attention); got != types.RollupNames {
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
	whole := p.View(Query{})
	byName := map[string]types.FleetNode{}
	for _, n := range whole.Nodes {
		byName[n.Name] = n
	}
	for _, q := range []Query{{Offset: 20, Limit: 10}, {Search: "node-0004", Limit: 5}} {
		for _, n := range p.View(q).Nodes {
			w := byName[n.Name]
			if n.Status == "" || n.BuildMatch == "" || n.NameMatch == "" ||
				n.Status != w.Status || n.BuildMatch != w.BuildMatch || n.NameMatch != w.NameMatch {
				t.Errorf("View(%+v) node %s = status %q build %q name %q; unpaged has %q %q %q",
					q, n.Name, n.Status, n.BuildMatch, n.NameMatch, w.Status, w.BuildMatch, w.NameMatch)
			}
		}
	}
}
