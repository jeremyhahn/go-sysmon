package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// lateNode is a server whose handler is bound after its URL is known, so
// aggregators can list each other before either starts.
type lateNode struct {
	srv atomic.Pointer[Server]
	ts  *httptest.Server
	// subtrees counts the GET /api/fleet/subtree requests it has answered:
	// one per cycle of the aggregator polling it, so a test can wait for
	// its parent's cycles to pass instead of for time to.
	subtrees atomic.Int64
}

func newLateNode(t *testing.T) *lateNode {
	t.Helper()
	n := &lateNode{}
	served := bound(&n.srv)
	n.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.ServeHTTP(w, r)
		if r.URL.Path == "/api/fleet/subtree" {
			n.subtrees.Add(1)
		}
	}))
	t.Cleanup(n.ts.Close)
	return n
}

// aggregatorAt serves an aggregator called name on late, polling peers.
func aggregatorAt(t *testing.T, late *lateNode, name string, peers map[string]string) *fleet.Poller {
	t.Helper()
	mon := startedMonitor(t, namedSnapshotter{name: name}, 5*time.Millisecond)
	fc := config.Fleet{PollIntervalMs: 1000, TimeoutMs: 2000}
	for peer, url := range peers {
		fc.Peers = append(fc.Peers, config.Peer{Name: peer, URL: url})
	}
	poller := fleet.New(func() config.Fleet { return fc },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			prev, cur, err := mon.Consecutive()
			if err != nil {
				return nil, nil, 0
			}
			return prev, cur, mon.Interval()
		},
		func() string { return name }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	late.srv.Store(newServer(Config{Monitor: mon, Fleet: poller}, nil))
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go poller.Run(done)
	return poller
}

// TestTree_AnAliasLoopOfThreeIsRefusedAndStable (P1-2): X lists Y as "yy", Y
// lists Z as "zz", Z lists X as "xx" -- every name different, so no name ever
// repeats, and every file valid on its own. Instance IDs find the loop: it is
// refused where it closes, every view settles and stays settled, and no
// drill-in can go round it.
func TestTree_AnAliasLoopOfThreeIsRefusedAndStable(t *testing.T) {
	leaf := startTreeNode(t, namedSnapshotter{name: "lx"})
	x, y, z := newLateNode(t), newLateNode(t), newLateNode(t)
	px := aggregatorAt(t, x, "x", map[string]string{"yy": y.ts.URL, "lx": leaf.url()})
	aggregatorAt(t, y, "y", map[string]string{"zz": z.ts.URL})
	aggregatorAt(t, z, "z", map[string]string{"xx": x.ts.URL})

	view := func(n *lateNode) types.Fleet {
		var v types.Fleet
		getJSON(t, n.srv.Load(), "/api/fleet?limit=50", &v)
		return v
	}
	// Every aggregator on the loop finds its own ID below its child, so every
	// edge of the loop is cut: each shows its direct peers, and the loop's
	// subtree named once as refused.
	waitFor(t, "every edge of the loop is cut", func() bool {
		return view(x).Rollup.SubtreeErrors == 1 && view(y).Rollup.SubtreeErrors == 1 && view(z).Rollup.SubtreeErrors == 1
	})
	// Settled, and it stays settled: the same fleets over three more poll
	// cycles of every aggregator -- the refusal keeps its evidence, so
	// cutting the loop does not make it look gone. Each node is polled by
	// exactly one parent, once a cycle, and cycles do not overlap, so a
	// fourth subtree request answered means three whole cycles published.
	settled := func() {
		for name, n := range map[string]*lateNode{"x": x, "y": y, "z": z} {
			want := map[string]int{"x": 3, "y": 2, "z": 2}[name]
			v := view(n)
			if v.Page.Total != want || v.Rollup.Nodes != want || v.Rollup.SubtreeErrors != 1 || v.Rollup.NameConflicts != 0 {
				t.Fatalf("%s's fleet moved: %d nodes (roll-up %d, %d refused, %d conflicts); want %d, 1 refused",
					name, v.Page.Total, v.Rollup.Nodes, v.Rollup.SubtreeErrors, v.Rollup.NameConflicts, want)
			}
		}
	}
	base := map[*lateNode]int64{x: x.subtrees.Load(), y: y.subtrees.Load(), z: z.subtrees.Load()}
	await.Until(t, "three more poll cycles of every aggregator on the loop", func(context.Context) bool {
		settled()
		for n, b := range base {
			if n.subtrees.Load() < b+4 {
				return false
			}
		}
		return true
	})
	settled()

	// Every name x lists can be drilled into and answers, or is refused --
	// none goes round the loop.
	for _, n := range view(x).Nodes {
		if n.Self {
			continue
		}
		start := time.Now()
		rec := get(t, x.srv.Load(), "/api/fleet/"+n.Name+"/snapshot")
		if time.Since(start) > 5*time.Second {
			t.Errorf("drill-in on %s took %v", n.Name, time.Since(start))
		}
		if rec.Code != http.StatusOK && rec.Code != http.StatusLoopDetected {
			t.Errorf("drill-in on %s = %d %s", n.Name, rec.Code, rec.Body.String())
		}
	}

	// A request that has already been through x is refused by x, as a loop.
	req := httptest.NewRequest(http.MethodGet, "/api/fleet/lx/snapshot", nil)
	req.Header.Set(fleet.HeaderAncestry, px.InstanceID())
	rec := httptest.NewRecorder()
	x.srv.Load().ServeHTTP(rec, req)
	if rec.Code != http.StatusLoopDetected || rec.Header().Get(fleet.HeaderRefusal) != fleet.RefusalLoop {
		t.Errorf("a request carrying x's own ID = %d (%q); want 508 loop", rec.Code, rec.Header().Get(fleet.HeaderRefusal))
	}
}

// TestTreeRequests_AreAdmittedByAncestry: every endpoint that can make this
// node ask its children refuses a request that has been here before (508
// loop), one past the depth limit (508 depth), and a malformed chain (400).
func TestTreeRequests_AreAdmittedByAncestry(t *testing.T) {
	root, _, _ := threeLevels(t)
	own := root.poller.InstanceID()
	other := func() string { return fleet.New(nil, nil, nil, nil).InstanceID() }
	five := strings.Join([]string{other(), other(), other(), other(), other()}, ",")
	for _, path := range []string{
		"/api/fleet?limit=5", "/api/fleet/subtree", "/api/fleet/nodes?name=a1",
		"/api/fleet/a1/snapshot", "/api/fleet/a1/config", "/api/history?metric=cpu&node=a1",
	} {
		for chain, want := range map[string]int{own: http.StatusLoopDetected, five: http.StatusLoopDetected, "not-an-id": http.StatusBadRequest} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set(fleet.HeaderAncestry, chain)
			rec := httptest.NewRecorder()
			root.srv.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Errorf("GET %s with chain %.20q = %d, want %d", path, chain, rec.Code, want)
			}
		}
	}
}

// stalledServer serves srv, holding every request for one of paths while
// stalled is set -- a peer that has stopped answering -- until its asker gives
// up or the test ends.
func stalledServer(t *testing.T, srv http.Handler, stalled *atomic.Bool, paths ...string) *httptest.Server {
	t.Helper()
	end := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stalled.Load() && slices.Contains(paths, r.URL.Path) {
			select {
			case <-end:
			case <-r.Context().Done():
			}
			return
		}
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		close(end)
		ts.Close()
	})
	return ts
}

// TestTree_ASlowGrandchildFailsOnlyItsOwnNodes (P2-5): root -> c -> {l, g ->
// m}, with g slow to serve its page. The budget the root is given is passed
// down with a margin at each hop, so c stops waiting on g in time to answer
// the root with what it has: its own peer l whole, and g's node m from the
// index, marked as not fetched -- and the root answers inside its budget.
func TestTree_ASlowGrandchildFailsOnlyItsOwnNodes(t *testing.T) {
	l := startTreeNode(t, namedSnapshotter{name: "l", cpu: 12})
	m := startTreeNode(t, namedSnapshotter{name: "m", cpu: 34})
	g := startTreeNode(t, namedSnapshotter{name: "g"}, m)
	var stalled atomic.Bool
	gSlow := stalledServer(t, g.srv, &stalled, "/api/fleet/nodes")
	c := startTreeNode(t, namedSnapshotter{name: "c"}, l, &treeNode{name: "g", ts: gSlow})
	waitFor(t, "c sees m", func() bool { return reachableCount(fleetOf(t, c, "")) == 4 })
	root := startTreeNode(t, namedSnapshotter{name: "root"}, c)
	waitFor(t, "root sees m", func() bool { return reachableCount(fleetOf(t, root, "")) == 5 })

	stalled.Store(true)
	req := httptest.NewRequest(http.MethodGet, "/api/fleet?limit=25", nil)
	req.Header.Set(fleet.HeaderBudget, "1000")
	// As a parent aggregator sends it: a budget counts only with a chain.
	req.Header.Set(fleet.HeaderAncestry, fleet.New(nil, nil, nil, nil).InstanceID())
	rec := httptest.NewRecorder()
	start := time.Now()
	root.srv.ServeHTTP(rec, req)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("root answered in %v; want inside its 1s budget", took)
	}
	var v types.Fleet
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]types.FleetNode{}
	for _, n := range v.Nodes {
		byName[n.Name] = n
	}
	if n := byName["l"]; n.Source != types.SummarySourceSummary || n.CPUPercent != 12 {
		t.Errorf("l = source %q, cpu %v; want c's own peer answered whole", n.Source, n.CPUPercent)
	}
	if n := byName["m"]; n.Source != types.SummarySourceIndex || !n.Reachable || n.Via != "c" {
		t.Errorf("m = source %q, reachable %v, via %q; want its index entry through c, figures not fetched", n.Source, n.Reachable, n.Via)
	}
}

// TestTree_ADrillInThroughASlowChildGetsItsStaleReading (P2-5): a node whose
// peer has become slow is answered, through the root, with the last reading its
// aggregator has, marked stale -- inside the root's budget, not as a 502 when
// the root stops waiting.
func TestTree_ADrillInThroughASlowChildGetsItsStaleReading(t *testing.T) {
	l := startTreeNode(t, namedSnapshotter{name: "l2"})
	// Stalled, it answers neither a drill-in nor a poll.
	var stalled atomic.Bool
	lSlow := stalledServer(t, l.srv, &stalled, "/api/snapshot", "/api/summary")
	c := startTreeNode(t, namedSnapshotter{name: "c"}, &treeNode{name: "l2", ts: lSlow})
	waitFor(t, "c sees l2", func() bool { return reachableCount(fleetOf(t, c, "")) == 2 })
	root := startTreeNode(t, namedSnapshotter{name: "root"}, c)
	waitFor(t, "root sees l2", func() bool { return reachableCount(fleetOf(t, root, "")) == 3 })

	var first types.PeerSnapshot
	if code := getJSON(t, root.srv, "/api/fleet/l2/snapshot", &first); code != http.StatusOK || !first.Reachable {
		t.Fatalf("first drill-in = %d %+v", code, first)
	}
	stalled.Store(true)
	// c reuses that reading until a poll of l2 fails after it: then its next
	// drill-in asks l2 again, and l2 is too slow to answer in the budget.
	waitFor(t, "c's poll of the now slow l2 to fail", func() bool { return reachableCount(fleetOf(t, c, "")) == 1 })

	req := httptest.NewRequest(http.MethodGet, "/api/fleet/l2/snapshot", nil)
	req.Header.Set(fleet.HeaderBudget, "1000")
	// As a parent aggregator sends it: a budget counts only with a chain.
	req.Header.Set(fleet.HeaderAncestry, fleet.New(nil, nil, nil, nil).InstanceID())
	rec := httptest.NewRecorder()
	start := time.Now()
	root.srv.ServeHTTP(rec, req)
	var stale types.PeerSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &stale); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("drill-in through a slow peer = %d %s", rec.Code, rec.Body.String())
	}
	if stale.Reachable || stale.Snapshot == nil || time.Since(start) > 1500*time.Millisecond {
		t.Errorf("drill-in = reachable %v, snapshot %v, in %v; want the last reading, stale, in time",
			stale.Reachable, stale.Snapshot != nil, time.Since(start))
	}
}

// TestTree_AnOversizeRelayIsRefused: a child's relayed answer past the bound
// is not passed on; the root answers 502 saying why, with no address.
func TestTree_AnOversizeRelayIsRefused(t *testing.T) {
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/api/summary":
			body = types.NodeSummary{Hostname: "big", NodeName: "big", Aggregates: 1, Alerts: []types.Alert{}}
		case "/api/fleet/subtree":
			body = types.FleetSubtree{
				Nodes:       []types.FleetIndexEntry{{Name: "big", Reachable: true, Status: "healthy"}, {Name: "a", Depth: 1, Reachable: true, Status: "healthy"}},
				Rollup:      &types.FleetRollup{Nodes: 2, Healthy: 2, Reporting: 2, Attention: []types.NodeAttention{}, Builds: []types.FleetBuild{}},
				Aggregators: []string{"BIGBIGBIGBIGBIGBIGBIGBIGBI"},
			}
		case "/api/history":
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(strings.Repeat("x", 9<<20))); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(child.Close)
	root := startTreeNode(t, namedSnapshotter{name: "root"}, &treeNode{name: "big", ts: child})
	waitFor(t, "root sees a", func() bool { return fleetOf(t, root, "?limit=5").Page.Total == 3 })

	rec := get(t, root.srv, "/api/history?metric=cpu&node=a")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "larger than") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("an oversize relay = %d %.200s; want 502 saying it is too large, no address", rec.Code, rec.Body.String())
	}
}

// TestTree_ABrowsersBudgetIsIgnored (N4): only another aggregator -- a request
// with a valid chain -- may shorten this node's budget. The same header from
// anything else is ignored, so no anonymous request can make shared work give
// up early.
func TestTree_ABrowsersBudgetIsIgnored(t *testing.T) {
	l := startTreeNode(t, namedSnapshotter{name: "l3", cpu: 21})
	c := startTreeNode(t, namedSnapshotter{name: "c"}, l)
	// What the root tells c it will wait for each page fetch.
	budgets := make(chan string, 16)
	cSeen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fleet/nodes" {
			budgets <- r.Header.Get(fleet.HeaderBudget)
		}
		c.srv.ServeHTTP(w, r)
	}))
	t.Cleanup(cSeen.Close)
	waitFor(t, "c sees l3", func() bool { return reachableCount(fleetOf(t, c, "")) == 2 })
	root := startTreeNode(t, namedSnapshotter{name: "root"}, &treeNode{name: "c", ts: cSeen})
	waitFor(t, "root sees l3", func() bool { return reachableCount(fleetOf(t, root, "")) == 3 })

	req := httptest.NewRequest(http.MethodGet, "/api/fleet?limit=25", nil)
	req.Header.Set(fleet.HeaderBudget, "50")
	rec := httptest.NewRecorder()
	root.srv.ServeHTTP(rec, req)

	// The page fetch was asked for with the root's own budget, not the 50 ms
	// the browser named -- less a hop's margin, which is at least 50 ms, so
	// anything above 50 is not derived from the browser's figure.
	ms, err := strconv.Atoi(await.Recv(t, "the root's page fetch from c", budgets))
	if err != nil || ms <= 50 {
		t.Errorf("the root asked c with a budget of %d ms (%v); want its own, the browser's 50 ms ignored", ms, err)
	}
	var v types.Fleet
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, n := range v.Nodes {
		if n.Name == "l3" && (n.Source != types.SummarySourceSummary || n.CPUPercent != 21) {
			t.Errorf("l3 = source %q, cpu %v; want it fetched whole", n.Source, n.CPUPercent)
		}
	}
}

// TestTree_ARelayedHistoryIsNeverTheChildsBytes (N1): a child answering a
// relayed history request with HTML -- or anything but its JSON -- is refused.
// What the root serves is its own: an error in text, or JSON it wrote; never
// the child's markup or content type.
func TestTree_ARelayedHistoryIsNeverTheChildsBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		ctype  string
		body   string
		want   int
	}{
		"html with 200":        {http.StatusOK, "text/html", "<script>alert(1)</script>", http.StatusBadGateway},
		"html error":           {http.StatusBadRequest, "text/html", "<script>alert(2)</script>", http.StatusBadRequest},
		"a status not relayed": {http.StatusTeapot, "application/json", `{"x":1}`, http.StatusBadGateway},
		// A well-formed range, but not declared as JSON: whatever a browser
		// would make of that content type, it is not the child's to choose.
		"a range sent as html": {http.StatusOK, "text/html", `{"enabled":true,"node":"a","metric":"cpu"}`, http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			child := evilChild(t, tc.status, tc.ctype, tc.body)
			root := startTreeNode(t, namedSnapshotter{name: "root"}, &treeNode{name: "evil", ts: child})
			waitFor(t, "root sees a", func() bool { return fleetOf(t, root, "?limit=5").Page.Total == 3 })

			rec := get(t, root.srv, "/api/history?metric=cpu&node=a")
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "html") {
				t.Errorf("served with the child's content type %q", ct)
			}
			if strings.Contains(rec.Body.String(), "<script>") {
				t.Errorf("served the child's markup: %s", rec.Body.String())
			}
		})
	}
}

// evilChild is a child aggregator with one node, a, whose history answer is
// whatever the test says.
func evilChild(t *testing.T, status int, ctype, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch r.URL.Path {
		case "/api/summary":
			v = types.NodeSummary{Hostname: "evil", NodeName: "evil", Aggregates: 1, Alerts: []types.Alert{}}
		case "/api/fleet/subtree":
			v = types.FleetSubtree{
				Nodes: []types.FleetIndexEntry{
					{Name: "evil", Reachable: true, Status: "healthy"},
					{Name: "a", Depth: 1, Reachable: true, Status: "healthy"},
				},
				Rollup:      &types.FleetRollup{Nodes: 2, Healthy: 2, Reporting: 2, Attention: []types.NodeAttention{}, Builds: []types.FleetBuild{}},
				Aggregators: []string{"EVILEVILEVILEVILEVILEVILEV"},
			}
		case "/api/history":
			w.Header().Set("Content-Type", ctype)
			w.WriteHeader(status)
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
