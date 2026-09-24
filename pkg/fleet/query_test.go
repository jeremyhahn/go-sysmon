package fleet

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestParseQuery_ReadsEachParameter(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  Query
	}{
		{"nothing asked is the whole fleet", "", Query{}},
		{"search trimmed", "q=%20%20vstation%20", Query{Search: "vstation"}},
		{"search of only space matches everything", "q=%20%20", Query{}},
		{"bounds", "offset=50&limit=25", Query{Offset: 50, Limit: 25}},
		{"all three", "q=fs&offset=0&limit=1", Query{Search: "fs", Limit: 1}},
		{"anchor", "q=db&limit=25&anchor=db", Query{Search: "db", Limit: 25, Anchor: "db"}},
		{"unknown parameters are not ours to judge", "sort=name&limit=5", Query{Limit: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseQuery(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

// TestParseQuery_RejectsWhatItCannotHonour: a malformed bound is refused, not
// defaulted. A limit quietly dropped would hand the client the whole fleet it
// asked not to be sent.
func TestParseQuery_RejectsWhatItCannotHonour(t *testing.T) {
	tests := []struct {
		name  string
		query string
		param string
	}{
		{"limit not a number", "limit=ten", QueryParamLimit},
		{"limit empty", "limit=", QueryParamLimit},
		{"limit zero", "limit=0", QueryParamLimit},
		{"limit negative", "limit=-5", QueryParamLimit},
		{"limit overflows", "limit=99999999999999999999", QueryParamLimit},
		{"offset negative", "offset=-1", QueryParamOffset},
		{"offset fractional", "offset=1.5", QueryParamOffset},
		{"offset repeated", "offset=0&offset=25", QueryParamOffset},
		{"limit repeated", "limit=25&limit=250", QueryParamLimit},
		{"search repeated", "q=a&q=b", QueryParamSearch},
		{"search not UTF-8", "q=%ff%fe", QueryParamSearch},
		{"anchor not UTF-8", "anchor=%ff", QueryParamAnchor},
		{"anchor repeated", "anchor=a&anchor=b", QueryParamAnchor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseQuery(tt.query)
			var invalid *ErrInvalidQuery
			if !errors.As(err, &invalid) {
				t.Fatalf("ParseQuery(%q) error = %v, want *ErrInvalidQuery", tt.query, err)
			}
			if invalid.Param != tt.param {
				t.Errorf("ParseQuery(%q) blamed %q, want %q", tt.query, invalid.Param, tt.param)
			}
		})
	}
}

// namedNodes builds nodes in display order with the given names; hostname is
// the name with "-host" appended unless hosts supplies one.
// TestParseQuery_RejectsAStringThatDoesNotParse: url.URL.Query drops a pair it
// cannot parse and says nothing, which made "limit=25;offset=0" an unbounded
// request answered with the whole fleet.
func TestParseQuery_RejectsAStringThatDoesNotParse(t *testing.T) {
	for _, raw := range []string{"limit=25;offset=0", "q=%zz", "limit=%"} {
		_, err := ParseQuery(raw)
		var malformed *ErrMalformedQuery
		if !errors.As(err, &malformed) {
			t.Errorf("ParseQuery(%q) error = %v, want *ErrMalformedQuery", raw, err)
		}
	}
}

func namedNodes(names []string, hosts map[string]string) []types.FleetNode {
	nodes := make([]types.FleetNode, 0, len(names))
	for _, name := range names {
		host, ok := hosts[name]
		if !ok {
			host = name + "-host"
		}
		nodes = append(nodes, types.FleetNode{Name: name, NodeSummary: types.NodeSummary{Hostname: host}})
	}
	sortByName(nodes)
	return nodes
}

func names(nodes []types.FleetNode) []string {
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func TestPage_Bounds(t *testing.T) {
	nodes := namedNodes([]string{"a", "b", "c", "d", "e"}, nil)
	tests := []struct {
		name string
		q    Query
		want []string
	}{
		{"no limit is everything", Query{}, []string{"a", "b", "c", "d", "e"}},
		{"first page", Query{Limit: 2}, []string{"a", "b"}},
		{"middle page", Query{Offset: 2, Limit: 2}, []string{"c", "d"}},
		{"short last page", Query{Offset: 4, Limit: 2}, []string{"e"}},
		{"offset with no limit runs to the end", Query{Offset: 3}, []string{"d", "e"}},
		{"offset exactly at the end", Query{Offset: 5, Limit: 2}, []string{}},
		{"offset past the end", Query{Offset: 99, Limit: 2}, []string{}},
		{"limit larger than the fleet", Query{Limit: 250}, []string{"a", "b", "c", "d", "e"}},
		// start+limit would overflow to a negative bound and panic the slice.
		{"limit at the integer maximum", Query{Offset: 1, Limit: math.MaxInt}, []string{"b", "c", "d", "e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched, _ := page(nodes, tt.q)
			if !reflect.DeepEqual(names(got), tt.want) {
				t.Errorf("page(%+v) = %v, want %v", tt.q, names(got), tt.want)
			}
			if matched != 5 {
				t.Errorf("matched = %d, want all 5 whatever the page", matched)
			}
		})
	}
}

// TestPage_SearchMatchesNameOrHostnameIgnoringCase: what the dashboard's search
// box did before it moved here -- name or hostname, case-insensitive -- with
// the case rule the node order uses.
func TestPage_SearchMatchesNameOrHostnameIgnoringCase(t *testing.T) {
	nodes := namedNodes(
		[]string{"ci-node-1", "FS", "gpu-host", "mon0", "vm-host-1", "vm-host-2"},
		map[string]string{"gpu-host": "GPU-Box.lan", "mon0": "monitor"},
	)
	tests := []struct {
		search string
		want   []string
	}{
		{"vstation", []string{"vm-host-1", "vm-host-2"}},
		{"VSTATION-0", []string{"vm-host-1", "vm-host-2"}},
		{"fs", []string{"FS"}},
		{"gpu-box", []string{"gpu-host"}}, // hostname only
		{"monitor", []string{"mon0"}},
		{"-01", []string{"ci-node-1", "vm-host-1"}},
		{"-host", []string{"ci-node-1", "FS", "vm-host-1", "vm-host-2"}},
		{"nothing-like-it", []string{}},
		{"", []string{"ci-node-1", "FS", "gpu-host", "mon0", "vm-host-1", "vm-host-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.search, func(t *testing.T) {
			// Query.Search arrives trimmed but not folded; page folds it.
			got, matched, _ := page(nodes, Query{Search: tt.search})
			if !reflect.DeepEqual(names(got), tt.want) {
				t.Errorf("search %q = %v, want %v", tt.search, names(got), tt.want)
			}
			if matched != len(tt.want) {
				t.Errorf("search %q matched %d, want %d", tt.search, matched, len(tt.want))
			}
		})
	}
}

// TestPage_PagesPartitionTheMatches: walking the pages of a search visits every
// matching node exactly once, in name order. A page boundary that dropped or
// repeated a node would make it unreachable, or listed twice, from the UI.
func TestPage_PagesPartitionTheMatches(t *testing.T) {
	var all []string
	for i := range 103 {
		all = append(all, string(rune('a'+i%26))+"-node-"+string(rune('A'+i/26)))
	}
	nodes := namedNodes(all, nil)
	whole, matched, _ := page(nodes, Query{Search: "NODE"})

	for _, limit := range []int{1, 7, 25, 250} {
		var walked []string
		for offset := 0; offset < matched; offset += limit {
			got, m, _ := page(nodes, Query{Search: "NODE", Offset: offset, Limit: limit})
			if m != matched {
				t.Fatalf("limit %d offset %d: matched %d, want %d on every page", limit, offset, m, matched)
			}
			walked = append(walked, names(got)...)
		}
		if !reflect.DeepEqual(walked, names(whole)) {
			t.Errorf("limit %d: pages concatenate to %d nodes differing from the %d-node list", limit, len(walked), len(whole))
		}
	}
}

func TestContainsFolded(t *testing.T) {
	tests := []struct {
		s, needle string
		want      bool
	}{
		{"Alpha", "alp", true},
		{"ALPHA", "pha", true},
		{"alpha", "", true},
		{"al", "alpha", false},
		{"", "a", false},
		{"node-É", "é", false}, // only A-Z fold; É is not folded to é
		{"node-É", "É", true},
	}
	for _, tt := range tests {
		if got := containsFolded(tt.s, tt.needle); got != tt.want {
			t.Errorf("containsFolded(%q, %q) = %v, want %v", tt.s, tt.needle, got, tt.want)
		}
	}
}

// TestPage_AnchorOpensThePageHoldingTheNode: a search for a name can match
// many other nodes first -- "db" is in every hostname containing it -- so the
// anchor, not page one, is how a client lands on that node's card.
func TestPage_AnchorOpensThePageHoldingTheNode(t *testing.T) {
	var all []string
	hosts := map[string]string{}
	for i := range 30 {
		name := fmt.Sprintf("a-%02d", i)
		all = append(all, name)
		hosts[name] = "rack-db.lan" // matches "db" by hostname, sorts first
	}
	all = append(all, "db", "db-01")
	nodes := namedNodes(all, hosts)

	got, matched, offset := page(nodes, Query{Search: "db", Limit: 25, Anchor: "db"})
	if matched != 32 || offset != 25 || !slices.Contains(names(got), "db") {
		t.Errorf("anchored page = %v at offset %d of %d; want the page at 25, holding db", names(got), offset, matched)
	}
	// Page one, which is where a search alone lands, does not hold it.
	if first, _, _ := page(nodes, Query{Search: "db", Limit: 25}); slices.Contains(names(first), "db") {
		t.Fatal("fixture does not reproduce the problem: db is already on page one")
	}

	// An anchor that is not among the matches leaves the offset as asked.
	if _, _, offset := page(nodes, Query{Search: "db", Limit: 25, Offset: 25, Anchor: "gone"}); offset != 25 {
		t.Errorf("unmatched anchor moved the offset to %d; want 25", offset)
	}
	// Without a limit there are no pages to anchor to.
	if _, _, offset := page(nodes, Query{Search: "db", Anchor: "db"}); offset != 0 {
		t.Errorf("anchor without a limit set offset %d; want 0", offset)
	}
}

// TestPage_NegativeBoundsFromADirectCaller: View is public, and a negative
// bound from an embedding caller must read as none rather than panic the
// slice. ParseQuery refuses them at the HTTP edge.
func TestPage_NegativeBoundsFromADirectCaller(t *testing.T) {
	nodes := namedNodes([]string{"a", "b", "c"}, nil)
	for _, q := range []Query{{Offset: -1}, {Limit: -5}, {Offset: -2, Limit: -2, Search: "b"}} {
		got, _, offset := page(nodes, q)
		if offset != 0 || len(got) == 0 {
			t.Errorf("page(%+v) = %v at %d; want from the start, no limit", q, names(got), offset)
		}
	}
}
