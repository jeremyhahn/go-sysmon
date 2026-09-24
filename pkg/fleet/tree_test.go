package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// fakeChild is a child aggregator serving a fixed subtree, and a leaf when
// it has no subtree. It counts what it is asked for.
type fakeChild struct {
	name    string
	subtree *types.FleetSubtree
	// nodes are the whole nodes it serves from GET /api/fleet/nodes.
	nodes map[string]types.FleetNode
	down  atomic.Bool
	// subtreeDown fails only the subtree read, the summary still answering.
	subtreeDown atomic.Bool
	// summaryAggregates overrides what the summary advertises; -1 uses the
	// subtree's size.
	summaryAggregates int

	// instanceID is what its summary says its instance ID is; id is the one
	// its subtree names, with any further aggregators in below.
	instanceID string
	id         string
	// refuse, when set, answers the subtree read 508 with this reason.
	refuse string
	// nodesHeld, while set, holds each GET /api/fleet/nodes until the
	// channel it points to is closed, or the asker gives up.
	nodesHeld atomic.Pointer[chan struct{}]
	// historyBody, historyType and historyStatus, when set, are GET
	// /api/history's answer; otherwise it answers the node's empty range.
	historyBody   []byte
	historyType   string
	historyStatus int

	// nodesExtra answers GET /api/fleet/nodes with every node, asked or not;
	// snapshotHeld holds each forwarded drill-in, as nodesHeld does.
	nodesExtra   bool
	snapshotHeld atomic.Pointer[chan struct{}]

	snapshotRequests atomic.Int64
	subtreeReads     atomic.Int64
	nodeRequests     atomic.Int64
	namesAsked       atomic.Int64
	inFlight         atomic.Int64
	maxInFlight      atomic.Int64
	lastHeaders      atomic.Pointer[http.Header]
	srv              *httptest.Server
}

func newFakeChild(t *testing.T, name string, below ...types.FleetIndexEntry) *fakeChild {
	t.Helper()
	c := &fakeChild{name: name, nodes: map[string]types.FleetNode{}, summaryAggregates: -1, id: newInstanceID()}
	if len(below) > 0 {
		entries := append([]types.FleetIndexEntry{{Name: name, Hostname: name + ".lan", Status: types.NodeStatusHealthy, Reachable: true, AgeSeconds: 1, Commit: "a46f6cf"}}, below...)
		var nodes []types.FleetNode
		for _, e := range entries {
			n := types.FleetNode{Name: e.Name, Reachable: e.Reachable, Status: e.Status, AgeSeconds: e.AgeSeconds,
				NodeSummary: types.NodeSummary{Hostname: e.Hostname, GitCommit: "a46f6cf", CPUPercent: 10, Alerts: []types.Alert{}}}
			if e.Status == types.NodeStatusWarning {
				n.WarningCount, n.Alerts = 1, []types.Alert{warn("hot")}
			}
			if !e.Reachable {
				n.Error = "connection refused"
			}
			nodes = append(nodes, n)
			if e.Depth > 0 {
				c.nodes[e.Name] = n
			}
		}
		sortByName(nodes)
		c.subtree = &types.FleetSubtree{Nodes: entries, Rollup: collect(nodes, types.RollupNames), Aggregators: []string{c.id}}
	}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeChild) serve(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Clone()
	c.lastHeaders.Store(&h)
	if c.down.Load() {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	switch {
	case r.URL.Path == endpointSummary:
		agg := c.summaryAggregates
		if agg < 0 && c.subtree != nil {
			agg = len(c.subtree.Nodes) - 1
		}
		write(types.NodeSummary{Hostname: c.name + ".lan", NodeName: c.name, GitCommit: "a46f6cf", Aggregates: max(agg, 0),
			InstanceID: c.instanceID, Alerts: []types.Alert{}})
	case r.URL.Path == endpointSubtree && c.subtree != nil:
		c.subtreeReads.Add(1)
		if c.refuse != "" {
			w.Header().Set(HeaderRefusal, c.refuse)
			http.Error(w, "refused", http.StatusLoopDetected)
			return
		}
		if c.subtreeDown.Load() {
			http.Error(w, "subtree unavailable", http.StatusInternalServerError)
			return
		}
		write(c.subtree)
	case r.URL.Path == "/api/history" && c.historyBody != nil:
		ct := c.historyType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		if c.historyStatus != 0 {
			w.WriteHeader(c.historyStatus)
		}
		if _, err := w.Write(c.historyBody); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case r.URL.Path == "/api/history":
		write(types.History{Enabled: true, Node: r.URL.Query().Get("node"), Metric: "cpu", Series: []types.HistorySeries{}})
	case strings.HasSuffix(r.URL.Path, "/config") && strings.HasPrefix(r.URL.Path, "/api/fleet/"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/fleet/"), "/config")
		switch name {
		case "gone":
			http.NotFound(w, r)
		case "twice":
			http.Error(w, "conflict", http.StatusConflict)
		default:
			write(types.ConfigView{ConfigFile: "/etc/" + name + ".yaml"})
		}
	case r.URL.Path == endpointNodes:
		c.nodeRequests.Add(1)
		n := c.inFlight.Add(1)
		defer c.inFlight.Add(-1)
		for {
			m := c.maxInFlight.Load()
			if n <= m || c.maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		c.namesAsked.Add(int64(len(r.URL.Query()[QueryParamName])))
		if !held(r, &c.nodesHeld) {
			return
		}
		out := types.FleetNodes{Nodes: []types.FleetNode{}}
		for _, name := range r.URL.Query()[QueryParamName] {
			if n, ok := c.nodes[name]; ok {
				out.Nodes = append(out.Nodes, n)
			}
		}
		if c.nodesExtra {
			out.Nodes = out.Nodes[:0]
			for _, n := range c.nodes {
				out.Nodes = append(out.Nodes, n)
			}
		}
		write(out)
	case strings.HasSuffix(r.URL.Path, "/snapshot") && strings.HasPrefix(r.URL.Path, "/api/fleet/"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/fleet/"), "/snapshot")
		c.snapshotRequests.Add(1)
		if !held(r, &c.snapshotHeld) {
			return
		}
		if _, ok := c.nodes[name]; !ok {
			http.NotFound(w, r)
			return
		}
		write(types.PeerSnapshot{Snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: name + ".lan"}}, Reachable: true})
	default:
		http.NotFound(w, r)
	}
}

func entry(name string, status types.NodeStatus) types.FleetIndexEntry {
	e := types.FleetIndexEntry{Name: name, Hostname: name + ".lan", Status: status,
		Reachable: status != types.NodeStatusUnreachable, AgeSeconds: 2, Depth: 1}
	switch status {
	case types.NodeStatusWarning:
		e.Reasons = []string{"hot"}
	case types.NodeStatusUnreachable:
		e.Reasons = []string{"connection refused"}
	}
	if e.Reachable {
		e.Commit = "a46f6cf"
	}
	return e
}

// treePoller is an aggregator named "local" polling the given peers.
func treePoller(t *testing.T, peers ...*fakeChild) (*Poller, config.Fleet) {
	t.Helper()
	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	for _, c := range peers {
		cfg.Peers = append(cfg.Peers, config.Peer{Name: c.name, URL: c.srv.URL})
	}
	snap := &types.Snapshot{Host: types.HostInfo{Hostname: "local.lan"}, GitCommit: "a46f6cf"}
	p := New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, snap, time.Second },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return p, cfg
}

func nodeNames(nodes []types.FleetNode) []string {
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func find(t *testing.T, nodes []types.FleetNode, name string) types.FleetNode {
	t.Helper()
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %q not in %v", name, nodeNames(nodes))
	return types.FleetNode{}
}

// TestTree_ListsPagesAndRollsUpASubtree: a child aggregator's nodes are part
// of this aggregator's fleet -- listed in the one name order, counted in the
// roll-up, paged -- and a page holding them fetches their summaries from the
// child once, then reuses them for the poll interval.
func TestTree_ListsPagesAndRollsUpASubtree(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy), entry("b", types.NodeStatusWarning), entry("c", types.NodeStatusHealthy))
	leaf := newFakeChild(t, "zz")
	p, cfg := treePoller(t, mid, leaf)
	p.refresh(cfg)

	whole := p.View(context.Background(), Query{})
	if got, want := nodeNames(whole.Nodes), []string{"a", "b", "c", "local", "mid", "zz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fleet = %v, want %v", got, want)
	}
	if b := find(t, whole.Nodes, "b"); b.Via != "mid" || b.Status != types.NodeStatusWarning || b.BuildMatch != types.BuildSame {
		t.Errorf("b = via %q, status %q, build %q; want via mid, warning, same build", b.Via, b.Status, b.BuildMatch)
	}
	if find(t, whole.Nodes, "mid").Via != "" || find(t, whole.Nodes, "zz").Via != "" {
		t.Error("a direct peer carries a via")
	}
	if self := whole.Self; self == nil || self.Aggregates != 5 {
		t.Errorf("self = %+v; want aggregates 5", self)
	}

	// The whole-fleet view asked the child for nothing: its subtree nodes are
	// their index entries, figures absent, not zero (P2-6).
	if n := mid.nodeRequests.Load(); n != 0 {
		t.Errorf("the whole-fleet view fetched from the child %d times; want none", n)
	}
	if a := find(t, whole.Nodes, "a"); a.Source != types.SummarySourceIndex {
		t.Errorf("a in the whole-fleet view = source %q; want %q", a.Source, types.SummarySourceIndex)
	}

	paged := p.View(context.Background(), Query{Offset: 1, Limit: 2})
	if got := nodeNames(paged.Nodes); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("page = %v, want [b c]", got)
	}
	if paged.Page.Total != 6 || paged.Page.Matched != 6 {
		t.Errorf("page counts = %+v; want 6 of 6", paged.Page)
	}
	if b := find(t, paged.Nodes, "b"); b.CPUPercent != 10 || b.Source == types.SummarySourceIndex {
		t.Errorf("b on a page = %+v; want its summary, fetched from mid", b)
	}
	// Within the poll interval the page's subtree nodes are not fetched again.
	p.View(context.Background(), Query{Offset: 1, Limit: 2})
	if n := mid.nodeRequests.Load(); n != 1 {
		t.Errorf("two views of one page fetched from the child %d times; want once", n)
	}

	r := paged.Rollup
	if r.Nodes != 6 || r.Healthy != 5 || r.Warning != 1 || r.Unreachable != 0 {
		t.Errorf("roll-up = %d nodes, %d healthy, %d warning, %d unreachable; want 6, 5, 1, 0",
			r.Nodes, r.Healthy, r.Warning, r.Unreachable)
	}
	if len(r.Attention) != 1 || r.Attention[0].Name != "b" || r.Attention[0].Via != "mid" {
		t.Errorf("attention = %+v; want b, from the child's roll-up, reached via mid", r.Attention)
	}
	// The unbounded whole-fleet roll-up, taken over every node, agrees.
	if w := whole.Rollup; w.Nodes != r.Nodes || w.Healthy != r.Healthy || w.Warning != r.Warning {
		t.Errorf("unpaged roll-up %+v disagrees with the composed %+v", w, r)
	}
}

// TestTree_SearchFindsSubtreeNodesByHostname: search runs over the polled
// index, so it reaches every node of the tree without fetching any of them.
func TestTree_SearchFindsSubtreeNodesByHostname(t *testing.T) {
	e := entry("db-7", types.NodeStatusHealthy)
	e.Hostname = "postgres.dc2"
	mid := newFakeChild(t, "mid", e, entry("web", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Search: "POSTGRES", Limit: 25})
	if got := nodeNames(v.Nodes); !reflect.DeepEqual(got, []string{"db-7"}) || v.Page.Matched != 1 {
		t.Errorf("search = %v (%d matched), want [db-7]", got, v.Page.Matched)
	}
}

// TestTree_UnreachableChildMakesItsSubtreeUnknown is #46's unreachable rule
// at tree scale: a child that stops answering takes its whole subtree with it
// -- counted as unreachable, never healthy, with how long since the subtree
// was read, and how many nodes it holds on the child's attention entry.
func TestTree_UnreachableChildMakesItsSubtreeUnknown(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy), entry("b", types.NodeStatusHealthy), entry("c", types.NodeStatusWarning))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	mid.down.Store(true)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Limit: 25})
	for _, name := range []string{"a", "b", "c"} {
		n := find(t, v.Nodes, name)
		if n.Reachable || n.Status != types.NodeStatusUnreachable || !strings.Contains(n.Error, "unreachable through mid") || n.Via != "mid" {
			t.Errorf("%s = reachable %v, status %q, error %q, via %q; want unknown through mid", name, n.Reachable, n.Status, n.Error, n.Via)
		}
	}
	if find(t, v.Nodes, "mid").SubtreeError == "" {
		t.Error("the unreachable child does not say why its subtree is unknown")
	}
	r := v.Rollup
	if r.Unreachable != 4 || r.Healthy != 1 || r.Warning != 0 || r.Nodes != 5 {
		t.Errorf("roll-up = %d nodes, %d unreachable, %d healthy, %d warning; want 5, 4 (mid and its three), 1 (local), 0",
			r.Nodes, r.Unreachable, r.Healthy, r.Warning)
	}
	if len(r.Attention) == 0 || r.Attention[0].Name != "mid" || r.Attention[0].Subtree != 3 {
		t.Errorf("attention = %+v; want mid first, carrying its subtree of 3", r.Attention)
	}
	// Drill-in to a node of the lost subtree fails at the gateway, not as an
	// unknown name: the node is known, its route is down.
	var unknown *ErrUnknownPeer
	if _, err := p.PeerSnapshot(context.Background(), "a", time.Second); err == nil || errors.As(err, &unknown) {
		t.Errorf("drill-in through a down child = %v; want a request error", err)
	}
}

// TestTree_RefusesATreeTooDeep: a subtree deeper than MaxTreeDepth is
// refused rather than composed. The child is still a node; its subtree is not
// in the view, and the roll-up says one subtree is missing.
func TestTree_RefusesATreeTooDeep(t *testing.T) {
	deepEntry := entry("far", types.NodeStatusHealthy)
	deepEntry.Depth = MaxTreeDepth
	deep := newFakeChild(t, "deep", deepEntry)
	p, cfg := treePoller(t, deep)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{})
	if got := nodeNames(v.Nodes); !reflect.DeepEqual(got, []string{"deep", "local"}) {
		t.Errorf("fleet = %v; want the child only, its subtree refused", got)
	}
	if e := find(t, v.Nodes, "deep").SubtreeError; !strings.Contains(e, "levels deep") {
		t.Errorf("deep's subtree error = %q", e)
	}
	if v.Rollup.SubtreeErrors != 1 || v.Rollup.Nodes != 2 {
		t.Errorf("roll-up = %d nodes, %d subtree errors; want 2 and 1", v.Rollup.Nodes, v.Rollup.SubtreeErrors)
	}
}

// TestTree_ARefusalOutlivesAFailedRead (P1-1): a refused subtree must never be
// composed, and a later read that fails is no reason to compose the last one
// it had. Before the fix the failed read copied the old subtree but not the
// refusal, and the refused subtree came back as an ordinary stale one.
func TestTree_ARefusalOutlivesAFailedRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child func(t *testing.T) *fakeChild
	}{
		{"too deep", func(t *testing.T) *fakeChild {
			e := entry("far", types.NodeStatusHealthy)
			e.Depth = MaxTreeDepth
			return newFakeChild(t, "mid", e, entry("x", types.NodeStatusHealthy))
		}},
		{"refused as a loop by the child", func(t *testing.T) *fakeChild {
			c := newFakeChild(t, "mid", entry("x", types.NodeStatusHealthy))
			c.refuse = RefusalLoop
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mid := tc.child(t)
			p, cfg := treePoller(t, mid)
			p.refresh(cfg)
			mid.subtreeDown.Store(true)
			p.refresh(cfg)

			v := p.View(context.Background(), Query{Limit: 25})
			if got := nodeNames(v.Nodes); !reflect.DeepEqual(got, []string{"local", "mid"}) {
				t.Errorf("fleet after a failed read = %v; want the refused subtree still left out", got)
			}
			if v.Rollup.SubtreeErrors != 1 || v.Rollup.NameConflicts != 0 || v.Self.NameConflict {
				t.Errorf("roll-up = %d subtree errors, %d conflicts, self conflicted %v; want 1, 0, false",
					v.Rollup.SubtreeErrors, v.Rollup.NameConflicts, v.Self.NameConflict)
			}
		})
	}
}

// TestTree_APeerThatIsThisAggregatorIsRefused (P1-2): the shortest loop -- a
// peer that is this very process under another name -- is known by its
// instance ID, and its subtree is never read.
func TestTree_APeerThatIsThisAggregatorIsRefused(t *testing.T) {
	alias := newFakeChild(t, "alias", entry("x", types.NodeStatusHealthy))
	p, cfg := treePoller(t, alias)
	alias.instanceID = p.InstanceID()
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Limit: 25})
	if e := find(t, v.Nodes, "alias").SubtreeError; !strings.Contains(e, "this aggregator itself") {
		t.Errorf("alias's subtree error = %q", e)
	}
	if alias.subtreeReads.Load() != 0 || v.Rollup.SubtreeErrors != 1 {
		t.Errorf("subtree read %d times, %d subtree errors; want none read, one refused", alias.subtreeReads.Load(), v.Rollup.SubtreeErrors)
	}
}

// TestTree_ALoopIsFoundByInstanceID (P1-2): a child whose subtree names this
// aggregator among its aggregators -- under whatever names -- is this
// aggregator's own subtree come round again. It is refused, and its
// aggregators are still named to this aggregator's parent, so the loop stays
// found after it is cut.
func TestTree_ALoopIsFoundByInstanceID(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("x", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	far := newInstanceID()
	mid.subtree.Aggregators = append(mid.subtree.Aggregators, far, p.InstanceID())
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Limit: 25})
	if e := find(t, v.Nodes, "mid").SubtreeError; !strings.Contains(e, "loop back to this aggregator through mid") {
		t.Errorf("mid's subtree error = %q", e)
	}
	if v.Page.Total != 2 || v.Rollup.SubtreeErrors != 1 {
		t.Errorf("fleet of %d, %d subtree errors; want 2 and 1", v.Page.Total, v.Rollup.SubtreeErrors)
	}
	sub, _ := p.Subtree()
	named := strings.Join(sub.Aggregators, ",")
	if !strings.HasPrefix(named, p.InstanceID()) || !strings.Contains(named, far) || !strings.Contains(named, mid.id) {
		t.Errorf("aggregators named upward = %v; want this node, then the refused child's", sub.Aggregators)
	}
	// And a later failed read does not lift it.
	mid.subtreeDown.Store(true)
	p.refresh(cfg)
	if p.View(context.Background(), Query{Limit: 25}).Rollup.SubtreeErrors != 1 {
		t.Error("a failed read lifted the loop's refusal")
	}
}

// TestTree_AChildThatRefusesIsNotComposed (P1-2): a child that refuses this
// aggregator's poll as a loop -- it found its own ID in the chain -- has its
// subtree refused here, named as a loop, never as a read failure.
func TestTree_AChildThatRefusesIsNotComposed(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("x", types.NodeStatusHealthy))
	mid.refuse = RefusalLoop
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Limit: 25})
	if e := find(t, v.Nodes, "mid").SubtreeError; !strings.Contains(e, "loop") {
		t.Errorf("mid's subtree error = %q; want the loop named", e)
	}
	if v.Page.Total != 2 || v.Rollup.SubtreeErrors != 1 {
		t.Errorf("fleet of %d, %d subtree errors; want 2 and 1", v.Page.Total, v.Rollup.SubtreeErrors)
	}
}

// TestTree_ADuplicateOfThisNodesNameIsAConflict (P2-11): a node elsewhere in
// the tree carrying this aggregator's own name is two machines under one name,
// not a loop -- loops are found by instance ID. Both are listed and flagged,
// the subtree is not hidden, and the name is not routed to.
func TestTree_ADuplicateOfThisNodesNameIsAConflict(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("x", types.NodeStatusHealthy), entry("local", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Limit: 25})
	if got := nodeNames(v.Nodes); !reflect.DeepEqual(got, []string{"local", "local", "mid", "x"}) {
		t.Fatalf("fleet = %v; want the subtree listed, both locals flagged", got)
	}
	if !v.Self.NameConflict || v.Rollup.NameConflicts != 2 || v.Rollup.SubtreeErrors != 0 {
		t.Errorf("self conflicted %v, %d conflicts, %d subtree errors; want true, 2, 0",
			v.Self.NameConflict, v.Rollup.NameConflicts, v.Rollup.SubtreeErrors)
	}
	var conflict *ErrNameConflict
	if _, err := p.PeerSnapshot(context.Background(), "local", time.Second); !errors.As(err, &conflict) {
		t.Errorf("drill-in on the shared name = %v, want ErrNameConflict", err)
	}
	if p.Has("local") {
		t.Error("the shared name is still answered for")
	}
}

// TestTree_ANameTwiceIsAConflictNotAMerge: names are fleet-wide identities
// (#37). A node in a child's subtree carrying a direct peer's name is two
// machines under one name: both are listed and flagged, and neither is
// routed to.
func TestTree_ANameTwiceIsAConflictNotAMerge(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("zz", types.NodeStatusHealthy), entry("ok", types.NodeStatusHealthy))
	leaf := newFakeChild(t, "zz")
	p, cfg := treePoller(t, mid, leaf)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{})
	var zz []types.FleetNode
	for _, n := range v.Nodes {
		if n.Name == "zz" {
			zz = append(zz, n)
		}
	}
	if len(zz) != 2 || !zz[0].NameConflict || !zz[1].NameConflict {
		t.Fatalf("zz listed %d times, flagged %+v; want both listed and flagged", len(zz), zz)
	}
	if v.Rollup.NameConflicts != 2 {
		t.Errorf("name conflicts = %d, want 2", v.Rollup.NameConflicts)
	}
	var conflict *ErrNameConflict
	if _, err := p.PeerSnapshot(context.Background(), "zz", time.Second); !errors.As(err, &conflict) {
		t.Errorf("drill-in on a conflicted name = %v, want ErrNameConflict", err)
	}
	if _, err := p.PeerConfig(context.Background(), "zz"); !errors.As(err, &conflict) {
		t.Errorf("config of a conflicted name = %v, want ErrNameConflict", err)
	}
	if p.Has("zz") || p.Resolve("zz").Kind == RouteSubtree {
		t.Error("a conflicted name is still treated as a node")
	}
	if p.Resolve("ok").Kind != RouteSubtree || !p.Has("ok") {
		t.Error("an unconflicted subtree node is not routed")
	}
}

// TestTree_DrillInRoutesToTheOwner: a subtree node's snapshot is asked of the
// child that owns it; a name no node carries is unknown.
func TestTree_DrillInRoutesToTheOwner(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	snap, err := p.PeerSnapshot(context.Background(), "a", time.Second)
	if err != nil || !snap.Reachable || snap.Snapshot.Host.Hostname != "a.lan" {
		t.Errorf("drill-in on a = %+v, %v; want a's snapshot through mid", snap, err)
	}
	var unknown *ErrUnknownPeer
	if _, err := p.PeerSnapshot(context.Background(), "nobody", time.Second); !errors.As(err, &unknown) {
		t.Errorf("drill-in on nobody = %v, want ErrUnknownPeer", err)
	}
}

// TestTree_AChildOnAnOlderBuildIsALeaf: a child whose summary does not say it
// aggregates is polled as the one node it says it is; its subtree is never
// asked for.
func TestTree_AChildOnAnOlderBuildIsALeaf(t *testing.T) {
	old := newFakeChild(t, "old", entry("hidden", types.NodeStatusHealthy))
	old.summaryAggregates = 0
	p, cfg := treePoller(t, old)
	p.refresh(cfg)

	if got := nodeNames(p.View(context.Background(), Query{}).Nodes); !reflect.DeepEqual(got, []string{"local", "old"}) {
		t.Errorf("fleet = %v, want local and old only", got)
	}
	if n := old.subtreeReads.Load(); n != 0 {
		t.Errorf("subtree read %d times from a child that did not say it aggregates", n)
	}
}

// TestSubtree_IsWhatAParentComposes: the index covers every node of the tree
// with its depth from here, and the roll-up keeps every build group so a
// parent can sum them.
func TestSubtree_IsWhatAParentComposes(t *testing.T) {
	below := []types.FleetIndexEntry{}
	for i := range 7 {
		below = append(below, entry(fmt.Sprintf("n%d", i), types.NodeStatusHealthy))
	}
	mid := newFakeChild(t, "mid", below...)
	// A build per node: more groups than a page names, and every one of them
	// counting a reachable node, as a real roll-up's do.
	mid.subtree.Rollup.Builds = nil
	for i := range mid.subtree.Nodes {
		mid.subtree.Rollup.Builds = append(mid.subtree.Rollup.Builds, types.FleetBuild{Commit: fmt.Sprintf("b%d", i), Count: 1, Nodes: []string{"x"}})
	}
	mid.subtree.Rollup.BuildGroups = len(mid.subtree.Rollup.Builds)
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	sub, ok := p.Subtree()
	if !ok {
		t.Fatal("Subtree reported no fleet")
	}
	depth := map[string]int{}
	for _, e := range sub.Nodes {
		depth[e.Name] = e.Depth
	}
	if depth["local"] != 0 || depth["mid"] != 1 || depth["n3"] != 2 || len(sub.Nodes) != 9 {
		t.Errorf("index depths = %v (%d entries); want local 0, mid 1, its nodes 2, 9 in all", depth, len(sub.Nodes))
	}
	if len(sub.Rollup.Builds) <= types.RollupNames {
		t.Errorf("subtree roll-up has %d build groups; want every group kept for the parent to sum", len(sub.Rollup.Builds))
	}
	if p.SubtreeSize() != 8 {
		t.Errorf("SubtreeSize = %d, want 8", p.SubtreeSize())
	}
}

func TestParseNames(t *testing.T) {
	names, err := ParseNames("name=a&name=b-2")
	if err != nil || !reflect.DeepEqual(names, []string{"a", "b-2"}) {
		t.Errorf("ParseNames = %v, %v", names, err)
	}
	many := strings.Repeat("name=n&", maxNamesPerRequest+1)
	for _, raw := range []string{"", "name=", "name=a/b", "name=%zz", many} {
		if _, err := ParseNames(raw); err == nil {
			t.Errorf("ParseNames(%.40q) accepted", raw)
		}
	}
}

// TestTree_ASubtreeThatCannotBeReadIsUnknown: the child answers, but its
// subtree cannot be read. Its last subtree stays, every node in it unknown, and
// the child's card says why.
func TestTree_ASubtreeThatCannotBeReadIsUnknown(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	mid.subtreeDown.Store(true)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{})
	if a := find(t, v.Nodes, "a"); a.Reachable || a.Status != types.NodeStatusUnreachable {
		t.Errorf("a = %+v; want unknown while its subtree cannot be read", a)
	}
	if m := find(t, v.Nodes, "mid"); !m.Reachable || m.SubtreeError == "" {
		t.Errorf("mid = reachable %v, subtree error %q; want reachable, saying its subtree is not current", m.Reachable, m.SubtreeError)
	}
}

// TestPoller_NodesServesTheNamedNodes: GET /api/fleet/nodes on an aggregator
// with a child answers direct nodes from its own state and subtree nodes from
// the child, and omits what it does not have.
func TestPoller_NodesServesTheNamedNodes(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	leaf := newFakeChild(t, "zz")
	p, cfg := treePoller(t, mid, leaf)
	p.refresh(cfg)

	got := p.Nodes(context.Background(), []string{"a", "local", "zz", "nobody"})
	if names := nodeNames(got.Nodes); !reflect.DeepEqual(names, []string{"a", "local", "zz"}) {
		t.Errorf("nodes = %v, want [a local zz]", names)
	}
	flat, _ := treePoller(t)
	if n := flat.Nodes(context.Background(), []string{"x"}); len(n.Nodes) != 0 {
		t.Errorf("a poller with no peers served %v", n.Nodes)
	}
}

// TestTree_SettingsAndRelayRouteToTheOwner: a subtree node's settings and a
// relayed request go to the child that owns it, and the child's answer about
// the node -- unknown there, or in conflict there -- becomes this node's.
func TestTree_SettingsAndRelayRouteToTheOwner(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy), entry("gone", types.NodeStatusHealthy), entry("twice", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	view, err := p.PeerConfig(context.Background(), "a")
	if err != nil || view.ConfigFile == "" {
		t.Errorf("settings of a = %+v, %v; want mid's answer", view, err)
	}
	var unknown *ErrUnknownPeer
	if _, err := p.PeerConfig(context.Background(), "gone"); !errors.As(err, &unknown) {
		t.Errorf("settings of a node its child no longer knows = %v, want ErrUnknownPeer", err)
	}
	var conflict *ErrNameConflict
	if _, err := p.PeerConfig(context.Background(), "twice"); !errors.As(err, &conflict) || !strings.Contains(err.Error(), "twice") {
		t.Errorf("settings of a node its child finds in conflict = %v, want ErrNameConflict", err)
	}

	got, err := p.RelayHistory(context.Background(), p.Resolve("a"), "a", "metric=cpu&node=a")
	if err != nil || got.Node != "a" || !got.Enabled {
		t.Errorf("relay = %+v, %v; want the child's range for a", got, err)
	}
	if _, err := p.RelayHistory(context.Background(), p.Resolve("nobody"), "nobody", "node=nobody"); !errors.As(err, &unknown) {
		t.Errorf("relay for a name no child owns = %v, want ErrUnknownPeer", err)
	}
	mid.srv.Close()
	if _, err := p.RelayHistory(context.Background(), p.Resolve("a"), "a", "node=a&metric=mem"); err == nil {
		t.Error("relay through a closed child succeeded")
	}
}

// TestTree_PagesGoWithTheirChild: a page's cached subtree summaries are dropped
// when their child leaves the configuration.
func TestTree_PagesGoWithTheirChild(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	leaf := newFakeChild(t, "zz")
	p, cfg := treePoller(t, mid, leaf)
	p.refresh(cfg)
	p.View(context.Background(), Query{Limit: 25})
	if _, ok := p.pageCache.Load("a"); !ok {
		t.Fatal("a's summary was not cached")
	}

	cfg.Peers = cfg.Peers[1:] // mid removed
	p.cfgFn = func() config.Fleet { return cfg }
	p.refresh(cfg)
	if _, ok := p.pageCache.Load("a"); ok {
		t.Error("a's summary outlived its child in the configuration")
	}
}

func TestTreeErrors(t *testing.T) {
	if !strings.Contains((&ErrNameConflict{Node: "dup"}).Error(), "dup") {
		t.Error("ErrNameConflict does not name the node")
	}
	if !strings.Contains((&ErrRelayTooLarge{Limit: 10}).Error(), "10") {
		t.Error("ErrRelayTooLarge does not say its limit")
	}
}

// hold makes every request a held pointer covers wait until release is
// called, and returns release.
func hold(t *testing.T, held *atomic.Pointer[chan struct{}]) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	held.Store(&ch)
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

// held waits while r is held, reporting false when its asker gave up first.
func held(r *http.Request, h *atomic.Pointer[chan struct{}]) bool {
	ch := h.Load()
	if ch == nil {
		return true
	}
	select {
	case <-*ch:
		return true
	case <-r.Context().Done():
		return false
	}
}
