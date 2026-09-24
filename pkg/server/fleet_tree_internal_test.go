package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// A fleet tree of real go-sysmon servers (#190): leaves, child aggregators
// polling them, and a root polling the children -- every hop over HTTP, every
// endpoint the one the service serves.

// namedSnapshotter is a node that calls itself name, at a fixed CPU, raising
// the given alerts. cpuUnknown makes its one core unmeasured, as on a
// first collection or an unreadable /proc/stat line (#207).
type namedSnapshotter struct {
	name       string
	cpu        float64
	cpuUnknown bool
	alerts     []types.Alert
}

func (s namedSnapshotter) Snapshot() (*types.Snapshot, error) {
	snap := &types.Snapshot{
		Timestamp: time.Now(),
		NodeName:  s.name,
		GitCommit: "a46f6cf",
		Host:      types.HostInfo{Hostname: s.name + ".lan"},
		CPUs:      []types.CPUInfo{{UsagePercent: s.cpu, UsageAvailable: !s.cpuUnknown}},
		Alerts:    s.alerts,
	}
	types.ReplaceNilSlices(snap)
	return snap, nil
}

// treeNode is one server of the tree.
type treeNode struct {
	name   string
	srv    *Server
	ts     *httptest.Server
	poller *fleet.Poller
}

func (n *treeNode) url() string { return n.ts.URL }

// startTreeNode serves a node called name. With peers it is an aggregator,
// its poller running for the life of the test.
func startTreeNode(t *testing.T, s namedSnapshotter, peers ...*treeNode) *treeNode {
	t.Helper()
	return startTreeNodeWithHistory(t, s, nil, peers...)
}

// startTreeNodeWithHistory is startTreeNode answering history ranges from h.
func startTreeNodeWithHistory(t *testing.T, s namedSnapshotter, h HistoryQuerier, peers ...*treeNode) *treeNode {
	t.Helper()
	n := &treeNode{name: s.name}
	mon := startedMonitor(t, s, 5*time.Millisecond)
	cfg := Config{Monitor: mon, History: h}
	if len(peers) > 0 {
		fc := config.Fleet{PollIntervalMs: 1000, TimeoutMs: 2000}
		for _, p := range peers {
			fc.Peers = append(fc.Peers, config.Peer{Name: p.name, URL: p.url()})
		}
		n.poller = fleet.New(
			func() config.Fleet { return fc },
			func() (*types.Snapshot, *types.Snapshot, time.Duration) {
				prev, cur, err := mon.Consecutive()
				if err != nil {
					return nil, nil, 0
				}
				return prev, cur, mon.Interval()
			},
			func() string { return s.name },
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
		cfg.Fleet = n.poller
	}
	n.srv = newServer(cfg, nil)
	n.ts = httptest.NewServer(n.srv)
	t.Cleanup(n.ts.Close)
	if n.poller != nil {
		done := make(chan struct{})
		t.Cleanup(func() { close(done) })
		go n.poller.Run(done)
	}
	return n
}

// bound serves through the server srv holds once it is set, and answers 503
// until then: aggregators that poll one another start one at a time, and one
// polled before its server is bound is still starting.
func bound(srv *atomic.Pointer[Server]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := srv.Load()
		if s == nil {
			http.Error(w, "not started", http.StatusServiceUnavailable)
			return
		}
		s.ServeHTTP(w, r)
	})
}

// fleetOf reads a node's whole fleet view.
func fleetOf(t *testing.T, n *treeNode, query string) types.Fleet {
	t.Helper()
	var v types.Fleet
	if code := getJSON(t, n.srv, "/api/fleet"+query, &v); code != http.StatusOK {
		t.Fatalf("GET %s/api/fleet%s = %d", n.name, query, code)
	}
	return v
}

// waitFor waits for cond, which reads the tree's served state, to hold.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	await.Until(t, what, func(context.Context) bool { return cond() })
}

func reachableCount(v types.Fleet) int {
	n := 0
	for _, node := range v.Nodes {
		if node.Reachable {
			n++
		}
	}
	return n
}

// threeLevels is root -> {mid-a -> {a1, a2}, mid-b -> {b1}}, with root also
// polling a leaf of its own, r1. a2 raises a warning and b1 runs hottest.
func threeLevels(t *testing.T) (root, midA, midB *treeNode) {
	t.Helper()
	a1 := startTreeNode(t, namedSnapshotter{name: "a1", cpu: 10})
	a2 := startTreeNode(t, namedSnapshotter{name: "a2", cpu: 20,
		alerts: []types.Alert{{ID: "cpu", Severity: types.SeverityWarning, Message: "cpu sustained"}}})
	b1 := startTreeNode(t, namedSnapshotter{name: "b1", cpu: 95})
	r1 := startTreeNode(t, namedSnapshotter{name: "r1", cpu: 5})
	midA = startTreeNode(t, namedSnapshotter{name: "mid-a", cpu: 30}, a1, a2)
	midB = startTreeNode(t, namedSnapshotter{name: "mid-b", cpu: 40}, b1)
	waitFor(t, "children see their leaves", func() bool {
		return reachableCount(fleetOf(t, midA, "")) == 3 && reachableCount(fleetOf(t, midB, "")) == 2
	})
	root = startTreeNode(t, namedSnapshotter{name: "root", cpu: 1}, midA, midB, r1)
	waitFor(t, "root sees the whole tree", func() bool {
		return reachableCount(fleetOf(t, root, "")) == 7
	})
	return root, midA, midB
}

// TestTree_ThreeLevels: the root lists every node of the tree in one name
// order, says which child each is reached through, pages across children,
// and rolls up the whole tree -- the same whether the roll-up is composed
// from the children's or taken over every node.
func TestTree_ThreeLevels(t *testing.T) {
	root, _, _ := threeLevels(t)

	whole := fleetOf(t, root, "")
	var names []string
	via := map[string]string{}
	for _, n := range whole.Nodes {
		names = append(names, n.Name)
		via[n.Name] = n.Via
	}
	if want := []string{"a1", "a2", "b1", "mid-a", "mid-b", "r1", "root"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("root's fleet = %v, want %v", names, want)
	}
	if via["a1"] != "mid-a" || via["b1"] != "mid-b" || via["mid-a"] != "" || via["r1"] != "" {
		t.Errorf("via = %v", via)
	}

	// A page spanning both children, and the composed roll-up beside it.
	paged := fleetOf(t, root, "?offset=1&limit=3")
	var got []string
	for _, n := range paged.Nodes {
		got = append(got, n.Name)
	}
	if !reflect.DeepEqual(got, []string{"a2", "b1", "mid-a"}) {
		t.Errorf("page = %v, want [a2 b1 mid-a]", got)
	}
	for _, r := range []*types.FleetRollup{whole.Rollup, paged.Rollup} {
		if r.Nodes != 7 || r.Healthy != 6 || r.Warning != 1 || r.Unreachable != 0 {
			t.Errorf("roll-up = %d nodes, %d healthy, %d warning, %d unreachable; want 7, 6, 1, 0",
				r.Nodes, r.Healthy, r.Warning, r.Unreachable)
		}
		if r.CPU == nil || r.CPU.Node != "b1" || r.CPU.Value != 95 {
			t.Errorf("highest CPU = %+v, want b1 at 95", r.CPU)
		}
		if len(r.Attention) != 1 || r.Attention[0].Name != "a2" {
			t.Errorf("attention = %+v, want a2", r.Attention)
		}
	}

	// And the roll-up alone, and the summary's claim to aggregate.
	var alone types.FleetRollup
	getJSON(t, root.srv, "/api/fleet/rollup", &alone)
	if alone.Nodes != 7 || alone.CPU.Node != "b1" {
		t.Errorf("GET /api/fleet/rollup = %+v", alone)
	}
	var sum types.NodeSummary
	getJSON(t, root.srv, "/api/summary", &sum)
	if sum.Aggregates != 6 {
		t.Errorf("root's summary aggregates %d, want 6", sum.Aggregates)
	}
}

// TestTree_AnUnknownCPUStaysUnknownAtEveryLevel (#207): a leaf whose CPU
// usage was not measured is counted as unknown by its aggregator, and that
// count is carried up to the root -- never averaged in, and never the fleet's
// idlest node at 0%. Its card says the same at every level.
func TestTree_AnUnknownCPUStaysUnknownAtEveryLevel(t *testing.T) {
	busy := startTreeNode(t, namedSnapshotter{name: "busy", cpu: 40})
	blind := startTreeNode(t, namedSnapshotter{name: "blind", cpuUnknown: true})
	mid := startTreeNode(t, namedSnapshotter{name: "mid", cpu: 20}, busy, blind)
	waitFor(t, "mid sees its leaves", func() bool { return reachableCount(fleetOf(t, mid, "")) == 3 })
	root := startTreeNode(t, namedSnapshotter{name: "root", cpu: 10}, mid)
	waitFor(t, "root sees the whole tree", func() bool { return reachableCount(fleetOf(t, root, "")) == 4 })

	for _, level := range []struct {
		n     *treeNode
		nodes int
	}{{mid, 3}, {root, 4}} {
		var r types.FleetRollup
		if code := getJSON(t, level.n.srv, "/api/fleet/rollup", &r); code != http.StatusOK {
			t.Fatalf("GET %s/api/fleet/rollup = %d", level.n.name, code)
		}
		if r.Reporting != level.nodes || r.CPUUnknown != 1 {
			t.Errorf("%s's roll-up: %d reporting, CPU unknown on %d; want %d and 1", level.n.name, r.Reporting, r.CPUUnknown, level.nodes)
		}
		if r.CPU == nil || r.CPU.Node != "busy" || r.CPU.Value != 40 {
			t.Errorf("%s's busiest CPU = %+v; want busy at 40", level.n.name, r.CPU)
		}
		// Every consumer's view: the paged fleet carries the same roll-up,
		// and the unmeasured node's card is unknown, not idle.
		v := fleetOf(t, level.n, "?q=blind&limit=5")
		if v.Rollup.CPUUnknown != 1 {
			t.Errorf("%s's paged roll-up: CPU unknown on %d; want 1", level.n.name, v.Rollup.CPUUnknown)
		}
		if len(v.Nodes) != 1 || v.Nodes[0].CPUAvailable || v.Nodes[0].Source == types.SummarySourceIndex {
			t.Errorf("%s's blind card = %+v; want it fetched, its CPU unavailable", level.n.name, v.Nodes)
		}
	}
}

// TestTree_DrillInThroughTheTree: a node two levels down is opened through the
// root, which asks the child that owns it; its settings likewise.
func TestTree_DrillInThroughTheTree(t *testing.T) {
	root, _, _ := threeLevels(t)

	var peer types.PeerSnapshot
	if code := getJSON(t, root.srv, "/api/fleet/b1/snapshot", &peer); code != http.StatusOK {
		t.Fatalf("drill-in on b1 through root = %d", code)
	}
	if !peer.Reachable || peer.Snapshot == nil || peer.Snapshot.Host.Hostname != "b1.lan" {
		t.Errorf("b1's snapshot through root = %+v", peer)
	}
	if code := getJSON(t, root.srv, "/api/fleet/a1/config", &types.ConfigView{}); code != http.StatusOK {
		t.Errorf("a1's settings through root = %d, want 200", code)
	}
	if rec := get(t, root.srv, "/api/fleet/nobody/snapshot"); rec.Code != http.StatusNotFound {
		t.Errorf("drill-in on an unknown name = %d, want 404", rec.Code)
	}
}

// TestTree_ALostChildTakesItsSubtree: when a child aggregator stops answering,
// the root counts it and everything below it as unreachable -- never healthy --
// and names the child with the size of what it took with it.
func TestTree_ALostChildTakesItsSubtree(t *testing.T) {
	root, _, midB := threeLevels(t)
	midB.ts.Close()

	waitFor(t, "root sees mid-b gone", func() bool {
		return fleetOf(t, root, "?limit=25").Rollup.Unreachable == 2
	})
	v := fleetOf(t, root, "?limit=25")
	for _, n := range v.Nodes {
		if n.Name == "b1" && (n.Reachable || n.Via != "mid-b" || !strings.Contains(n.Error, "unreachable through mid-b")) {
			t.Errorf("b1 = %+v; want unknown through mid-b", n)
		}
	}
	r := v.Rollup
	if r.Healthy != 4 || r.Warning != 1 || r.Nodes != 7 {
		t.Errorf("roll-up = %d nodes, %d healthy, %d warning; want 7, 4, 1", r.Nodes, r.Healthy, r.Warning)
	}
	var sub int
	for _, a := range r.Attention {
		if a.Name == "mid-b" {
			sub = a.Subtree
		}
	}
	if sub != 1 {
		t.Errorf("mid-b's attention entry carries subtree %d, want 1", sub)
	}
}

// TestTree_ALoopIsRefusedAtBothEnds: two aggregators that list each other are
// a loop no single configuration shows. Each refuses the other's subtree once
// it sees itself in it, and says so; neither view grows around the loop.
func TestTree_ALoopIsRefusedAtBothEnds(t *testing.T) {
	// Each needs the other's URL before it starts, so both are served by a
	// handler that dispatches to a server built once both URLs exist.
	var xs, ys atomic.Pointer[Server]
	xts := httptest.NewServer(bound(&xs))
	yts := httptest.NewServer(bound(&ys))
	t.Cleanup(xts.Close)
	t.Cleanup(yts.Close)
	leaf := startTreeNode(t, namedSnapshotter{name: "leaf"})
	x := &treeNode{name: "x", ts: xts}
	y := &treeNode{name: "y", ts: yts}
	xn := startTreeNode(t, namedSnapshotter{name: "x"}, y, leaf)
	yn := startTreeNode(t, namedSnapshotter{name: "y"}, x)
	xs.Store(xn.srv)
	ys.Store(yn.srv)

	waitFor(t, "both ends refuse the loop", func() bool {
		return fleetOf(t, xn, "?limit=25").Rollup.SubtreeErrors == 1 && fleetOf(t, yn, "?limit=25").Rollup.SubtreeErrors == 1
	})
	for _, n := range fleetOf(t, xn, "").Nodes {
		if n.Name == "y" && !strings.Contains(n.SubtreeError, "loop") {
			t.Errorf("x's view of y = %+v; want the loop named", n)
		}
	}
	if total := fleetOf(t, xn, "?limit=25").Page.Total; total != 3 {
		t.Errorf("x's fleet is %d nodes; want x, y and leaf, nothing duplicated around the loop", total)
	}
}

// TestFleetSubtreeAndNodesEndpoints: the two endpoints a parent reads, on an
// aggregator and on a node with no fleet, and GET /api/fleet/nodes' refusals.
func TestFleetSubtreeAndNodesEndpoints(t *testing.T) {
	root, midA, _ := threeLevels(t)

	var sub types.FleetSubtree
	if code := getJSON(t, midA.srv, "/api/fleet/subtree", &sub); code != http.StatusOK {
		t.Fatalf("GET /api/fleet/subtree = %d", code)
	}
	if len(sub.Nodes) != 3 || sub.Nodes[2].Name != "mid-a" || sub.Nodes[2].Depth != 0 || sub.Rollup == nil {
		t.Errorf("mid-a's subtree = %+v", sub)
	}

	var nodes types.FleetNodes
	if code := getJSON(t, root.srv, "/api/fleet/nodes?name=a1&name=r1&name=nobody", &nodes); code != http.StatusOK {
		t.Fatalf("GET /api/fleet/nodes = %d", code)
	}
	var got []string
	for _, n := range nodes.Nodes {
		got = append(got, n.Name)
	}
	if !reflect.DeepEqual(got, []string{"a1", "r1"}) {
		t.Errorf("nodes = %v, want [a1 r1]; an unknown name is omitted", got)
	}

	for _, q := range []string{"", "?name=", "?name=a/b", "?name=a;b"} {
		if rec := get(t, root.srv, "/api/fleet/nodes"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /api/fleet/nodes%s = %d, want 400", q, rec.Code)
		}
	}
	standalone := newServer(Config{}, nil)
	for _, path := range []string{"/api/fleet/subtree", "/api/fleet/nodes?name=a"} {
		if rec := get(t, standalone, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with no fleet = %d, want 404", path, rec.Code)
		}
	}
	// A poller with no peers is no fleet either: a leaf, which says so.
	noPeers := newServer(Config{Fleet: fleet.New(
		func() config.Fleet { return config.Fleet{} },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "leaf" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)}, nil)
	if rec := get(t, noPeers, "/api/fleet/subtree"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/fleet/subtree on a poller with no peers = %d, want 404", rec.Code)
	}
}

// TestTree_ANameTwiceIsRefusedOverHTTP: two children each polling a node
// called dup put one name on two machines. The root lists both, and refuses to
// route either: 409, never whichever answered first.
func TestTree_ANameTwiceIsRefusedOverHTTP(t *testing.T) {
	dupA := startTreeNode(t, namedSnapshotter{name: "dup"})
	dupB := startTreeNode(t, namedSnapshotter{name: "dup"})
	midA := startTreeNode(t, namedSnapshotter{name: "mid-a"}, dupA)
	midB := startTreeNode(t, namedSnapshotter{name: "mid-b"}, dupB)
	waitFor(t, "children see their leaves", func() bool {
		return reachableCount(fleetOf(t, midA, "")) == 2 && reachableCount(fleetOf(t, midB, "")) == 2
	})
	root := startTreeNode(t, namedSnapshotter{name: "root"}, midA, midB)
	waitFor(t, "root sees both", func() bool { return fleetOf(t, root, "?limit=25").Rollup.NameConflicts == 2 })

	for _, path := range []string{"/api/fleet/dup/snapshot", "/api/fleet/dup/config"} {
		if rec := get(t, root.srv, path); rec.Code != http.StatusConflict {
			t.Errorf("GET %s = %d, want 409", path, rec.Code)
		}
	}
	if root.poller.Has("dup") {
		t.Error("a conflicted name is a node history would be answered for")
	}
}

// TestTree_ServesNoAddressAtAnyLevel is #120 through the tree: nothing the root
// serves about any node, including what it relays from its children, carries a
// URL or any part of a peer's address.
func TestTree_ServesNoAddressAtAnyLevel(t *testing.T) {
	root, midA, midB := threeLevels(t)
	for _, path := range []string{
		"/api/fleet", "/api/fleet?limit=3", "/api/fleet/rollup", "/api/fleet/subtree",
		"/api/fleet/nodes?name=a1&name=b1", "/api/fleet/a1/config", "/api/fleet/b1/snapshot",
	} {
		rec := get(t, root.srv, path)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
		if strings.Contains(body, `"url"`) {
			t.Errorf("GET %s carries a url field", path)
		}
		for _, n := range []*treeNode{midA, midB} {
			for _, leak := range addressParts(t, n.url()) {
				if strings.Contains(body, leak) {
					t.Errorf("GET %s carries %q from %s's address", path, leak, n.name)
				}
			}
		}
	}
	// And the payload is JSON a parent can decode.
	var v types.Fleet
	if err := json.Unmarshal(get(t, root.srv, "/api/fleet").Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
