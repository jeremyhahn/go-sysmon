package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// setEntry changes one node of a fake child's subtree -- index and roll-up
// together, as a real child's next poll would.
func (c *fakeChild) setEntry(t *testing.T, name string, status types.NodeStatus) {
	t.Helper()
	var nodes []types.FleetNode
	for i := range c.subtree.Nodes {
		e := &c.subtree.Nodes[i]
		if e.Name == name {
			*e = entry(name, status)
		}
		n := types.FleetNode{Name: e.Name, Reachable: e.Reachable, Status: e.Status,
			NodeSummary: types.NodeSummary{GitCommit: e.Commit, Alerts: []types.Alert{}}}
		if !e.Reachable {
			n.Error = "connection refused"
		}
		nodes = append(nodes, n)
	}
	sortByName(nodes)
	c.subtree.Rollup = collect(nodes, types.RollupNames, 0.8)
}

// TestPage_NeverContradictsTheIndex (P2-3): a summary reused within the poll
// interval is older than the index the roll-up is counted from. The card takes
// its status, reachability and age from the index, so it never calls a node
// healthy while the roll-up above it counts it unreachable.
func TestPage_NeverContradictsTheIndex(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy), entry("b", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	if a := find(t, p.View(context.Background(), Query{Limit: 25}).Nodes, "a"); !a.Reachable {
		t.Fatalf("a = %+v; want it fetched healthy", a)
	}

	mid.setEntry(t, "a", types.NodeStatusUnreachable)
	p.refresh(cfg)
	v := p.View(context.Background(), Query{Limit: 25})
	a := find(t, v.Nodes, "a")
	if a.Reachable || a.Status != types.NodeStatusUnreachable || !strings.Contains(a.Error, "connection refused") {
		t.Errorf("a from the cache = reachable %v, status %q, error %q; want unreachable, as the roll-up counts it",
			a.Reachable, a.Status, a.Error)
	}
	if v.Rollup.Unreachable != 1 {
		t.Errorf("roll-up counts %d unreachable, want 1", v.Rollup.Unreachable)
	}
}

// TestPage_ARepointedChildsSummariesAreNotReused (P2-4): the cache is keyed by
// the child's address, so after a reload repoints the child's name at another
// machine, a page is fetched from the new one rather than served from the old.
func TestPage_ARepointedChildsSummariesAreNotReused(t *testing.T) {
	oldMid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	newMid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	for name, n := range newMid.nodes {
		n.CPUPercent = 77
		newMid.nodes[name] = n
	}
	p, cfg := treePoller(t, oldMid)
	p.refresh(cfg)
	p.View(context.Background(), Query{Limit: 25})

	stale, ok := p.pageCache.Load("a")
	if !ok {
		t.Fatal("a was not cached from the old child")
	}
	cfg.Peers[0].URL = newMid.srv.URL
	p.cfgFn = func() config.Fleet { return cfg }
	p.refresh(cfg)
	// The window between a new generation being published and the refresh
	// dropping the old child's entries: an old entry, still fresh, is in the
	// cache when a view asks.
	c := *stale.(*cachedNode)
	c.fetchedAt = time.Now()
	p.pageCache.Store("a", &c)
	a := find(t, p.View(context.Background(), Query{Limit: 25}).Nodes, "a")
	if a.CPUPercent != 77 || newMid.nodeRequests.Load() != 1 {
		t.Errorf("a after the repoint = cpu %v (%d fetches from the new child); want the new machine's 77, fetched",
			a.CPUPercent, newMid.nodeRequests.Load())
	}
}

// bigChild is a child with n healthy nodes below it.
func bigChild(t *testing.T, name string, n int) *fakeChild {
	t.Helper()
	var below []types.FleetIndexEntry
	for i := range n {
		below = append(below, entry(fmt.Sprintf("%s-%04d", name, i), types.NodeStatusHealthy))
	}
	return newFakeChild(t, name, below...)
}

// TestPage_ConcurrentColdRequestsFetchOnce (P2-6): many requests for the same
// cold page make one fetch of it, not one each.
func TestPage_ConcurrentColdRequestsFetchOnce(t *testing.T) {
	mid := bigChild(t, "mid", 50)
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	// The child holds the fetch until all 20 are waiting on it.
	release := hold(t, &mid.nodesHeld)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.View(context.Background(), Query{Limit: 25})
		}()
	}
	await.Until(t, "all 20 views to wait on the page fetch", func(context.Context) bool {
		return await.Parked("fleet.(*Poller).fetchNodes(", "fleet.(*flightGroup).do(") == 20
	})
	release()
	wg.Wait()
	if n := mid.nodeRequests.Load(); n != 1 {
		t.Errorf("20 concurrent cold views made %d fetches; want 1", n)
	}
}

// TestPage_FetchesAreBounded (P2-6): no view fetches more than
// maxFetchedPerView summaries, and no more than maxConcurrentFetches fetches
// are in flight from this node at once, however many views ask.
func TestPage_FetchesAreBounded(t *testing.T) {
	mid := bigChild(t, "mid", 3000)
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	v := p.View(context.Background(), Query{Limit: 2000})
	if asked := mid.namesAsked.Load(); asked != maxFetchedPerView {
		t.Errorf("a view of 2000 asked the child for %d summaries; want %d", asked, maxFetchedPerView)
	}
	var indexOnly int
	for _, n := range v.Nodes {
		if n.Source == types.SummarySourceIndex {
			indexOnly++
		}
	}
	if len(v.Nodes) != 2000 || indexOnly != 2000-maxFetchedPerView-2 {
		t.Errorf("%d nodes, %d from the index; want 2000, all but the fetched and the two direct nodes", len(v.Nodes), indexOnly)
	}

	// Twelve views of different pages, their fetches held at the child until
	// the bound is reached with more work than it waiting for a turn.
	release := hold(t, &mid.nodesHeld)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.View(context.Background(), Query{Offset: 250 + i*maxFetchedPerView, Limit: maxFetchedPerView})
		}()
	}
	await.Until(t, "the bound to be reached with more fetches waiting", func(context.Context) bool {
		return mid.inFlight.Load() == maxConcurrentFetches &&
			await.Parked("fleet.(*flightGroup).do.func1(") > maxConcurrentFetches
	})
	release()
	wg.Wait()
	if m := mid.maxInFlight.Load(); m > maxConcurrentFetches {
		t.Errorf("%d fetches were in flight at once; want at most %d", m, maxConcurrentFetches)
	}
}

// TestRollup_TheWholeListsShareTheBoundedPath (P2-7): the unbounded roll-up is
// the bounded one with its lists whole: nested subtree errors counted, and an
// unreachable child's entry carrying its subtree, exactly as a page's are.
func TestRollup_TheWholeListsShareTheBoundedPath(t *testing.T) {
	var below []types.FleetIndexEntry
	for i := range 7 {
		below = append(below, entry(fmt.Sprintf("w%d", i), types.NodeStatusWarning))
	}
	mid := newFakeChild(t, "mid", below...)
	mid.subtree.Rollup.SubtreeErrors = 2
	gone := newFakeChild(t, "gone", entry("g1", types.NodeStatusHealthy), entry("g2", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid, gone)
	p.refresh(cfg)
	gone.down.Store(true)
	p.refresh(cfg)

	whole := p.View(context.Background(), Query{}).Rollup
	paged := p.View(context.Background(), Query{Limit: 1}).Rollup
	if whole.SubtreeErrors != 2 || paged.SubtreeErrors != 2 {
		t.Errorf("subtree errors whole %d, paged %d; want the child's 2 in both", whole.SubtreeErrors, paged.SubtreeErrors)
	}
	var sub int
	var warned []string
	for _, a := range whole.Attention {
		if a.Name == "gone" {
			sub = a.Subtree
		}
		if a.Status == types.NodeStatusWarning {
			warned = append(warned, a.Name+" via "+a.Via)
		}
	}
	if sub != 2 {
		t.Errorf("gone's entry in the whole roll-up carries subtree %d, want 2", sub)
	}
	if len(warned) != 7 || warned[0] != "w0 via mid" {
		t.Errorf("whole attention names %v; want all seven warnings, via mid", warned)
	}
	if len(paged.Attention) != types.RollupNames || whole.Nodes != paged.Nodes || whole.Unreachable != paged.Unreachable {
		t.Errorf("paged %+v and whole %+v disagree beyond the lists' bound", paged, whole)
	}
}

// TestView_OneGenerationAtATime (P2-8): a child's poll that has failed but not
// yet been published does not reach the view half-way -- the child's state and
// its subtree are shown from one refresh, so a child is never unreachable
// beside a roll-up that merges its subtree as healthy.
func TestView_OneGenerationAtATime(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	// A refresh in progress: mid's poll has failed and been stored, and the
	// cycle has not finished.
	p.mu.Lock()
	p.peers["mid"] = &peerState{url: mid.srv.URL, err: "timed out", polledAt: time.Now()}
	p.mu.Unlock()

	v := p.View(context.Background(), Query{Limit: 25})
	m := find(t, v.Nodes, "mid")
	if !m.Reachable || v.Rollup.Unreachable != 0 || v.Rollup.Healthy != 3 {
		t.Errorf("mid reachable %v with %d unreachable, %d healthy; want the published generation throughout",
			m.Reachable, v.Rollup.Unreachable, v.Rollup.Healthy)
	}
}

// TestValidateSubtree (P2-9): a child's subtree is held to what composes, and
// its lists are clamped to their bounds.
func TestValidateSubtree(t *testing.T) {
	good := func() *types.FleetSubtree {
		return &types.FleetSubtree{
			Nodes: []types.FleetIndexEntry{
				{Name: "mid", Status: types.NodeStatusHealthy, Reachable: true},
				{Name: "a", Depth: 1, Status: types.NodeStatusHealthy, Reachable: true},
			},
			Rollup:      &types.FleetRollup{Nodes: 2, Healthy: 2, Reporting: 2},
			Aggregators: []string{newInstanceID()},
		}
	}
	for name, mutate := range map[string]func(*types.FleetSubtree){
		"no roll-up":       func(s *types.FleetSubtree) { s.Rollup = nil },
		"counts disagree":  func(s *types.FleetSubtree) { s.Rollup.Nodes = 3 },
		"no self":          func(s *types.FleetSubtree) { s.Nodes[0].Depth = 1 },
		"two selves":       func(s *types.FleetSubtree) { s.Nodes[1].Depth = 0 },
		"negative depth":   func(s *types.FleetSubtree) { s.Nodes[1].Depth = -1 },
		"an unusable name": func(s *types.FleetSubtree) { s.Nodes[1].Name = "a/b" },
		"no aggregators":   func(s *types.FleetSubtree) { s.Aggregators = nil },
		"a bad ID":         func(s *types.FleetSubtree) { s.Aggregators = []string{"nope"} },
	} {
		s := good()
		mutate(s)
		var invalid *SubtreeInvalidError
		if err := validateSubtree("mid", s); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "mid") {
			t.Errorf("%s: %v, want SubtreeInvalidError", name, err)
		}
	}

	s := good()
	many := strings.Split("a b c d e f g h", " ")
	s.Nodes[1].Reasons = many
	s.Rollup.Attention = make([]types.NodeAttention, 9)
	for i := range s.Rollup.Attention {
		s.Rollup.Attention[i].Status = types.NodeStatusWarning
		s.Rollup.Attention[i].Reasons = many
	}
	s.Rollup.Reporting = maxBuildGroups + 10
	s.Rollup.Nodes, s.Rollup.Healthy = s.Rollup.Reporting, s.Rollup.Reporting
	for i := range maxBuildGroups + 10 {
		s.Rollup.Builds = append(s.Rollup.Builds, types.FleetBuild{Commit: fmt.Sprint(i), Count: 1, Nodes: many})
		if i >= 2 {
			s.Nodes = append(s.Nodes, types.FleetIndexEntry{Name: fmt.Sprintf("n%d", i), Depth: 1, Status: types.NodeStatusHealthy, Reachable: true})
		}
	}
	s.Rollup.BuildGroups = len(s.Rollup.Builds)
	if err := validateSubtree("mid", s); err != nil {
		t.Fatalf("an oversized but valid subtree = %v", err)
	}
	r := s.Rollup
	if len(s.Nodes[1].Reasons) != types.RollupNames || len(r.Attention) != types.RollupNames ||
		len(r.Attention[0].Reasons) != types.RollupNames || len(r.Builds) != maxBuildGroups ||
		len(r.Builds[0].Nodes) != types.RollupNames {
		t.Errorf("clamped to %d reasons, %d attention, %d attention reasons, %d groups, %d names",
			len(s.Nodes[1].Reasons), len(r.Attention), len(r.Attention[0].Reasons), len(r.Builds), len(r.Builds[0].Nodes))
	}
}

// TestPollSubtree_OversizeAndInvalidAreReadFailures (P2-9): a subtree past the
// size bound, or one that does not hold together, is not composed; the last
// good one stays, stale, with every node unknown.
func TestPollSubtree_OversizeAndInvalidAreReadFailures(t *testing.T) {
	mid := bigChild(t, "mid", 200)
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)

	p.subtreeLimit = 1024
	p.refresh(cfg)
	v := p.View(context.Background(), Query{Limit: 25})
	if e := find(t, v.Nodes, "mid").SubtreeError; e == "" || v.Rollup.Unreachable != 200 {
		t.Errorf("after an oversize subtree: error %q, %d unreachable; want the last subtree, all unknown", e, v.Rollup.Unreachable)
	}

	p.subtreeLimit = maxSubtreeBytes
	mid.subtree.Rollup.Nodes = 5
	p.refresh(cfg)
	if e := find(t, p.View(context.Background(), Query{Limit: 25}).Nodes, "mid").SubtreeError; !strings.Contains(e, "invalid subtree") {
		t.Errorf("after an invalid subtree: error %q", e)
	}
}

// TestBuffers_AreKeptPerClass (P2-10): a peer's read buffers are one per
// endpoint class -- the subtree's among them, kept between cycles -- however
// many different names its pages ask for.
func TestBuffers_AreKeptPerClass(t *testing.T) {
	mid := bigChild(t, "mid", 400)
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	for page := range 4 {
		p.View(context.Background(), Query{Offset: page * 100, Limit: 100})
	}
	p.refresh(cfg)

	keys := map[string]bool{}
	p.bodies.m.Range(func(k, _ any) bool {
		keys[k.(string)] = true
		return true
	})
	if !keys[bufferKey("mid", endpointSubtree)] {
		t.Errorf("the subtree buffer was not kept: %v", keys)
	}
	if len(keys) > len(bufferClasses) {
		t.Errorf("%d buffers kept for one peer; want at most one per class (%d): %v", len(keys), len(bufferClasses), keys)
	}
}

func TestBufferClass(t *testing.T) {
	for in, want := range map[string]string{
		endpointSummary:                  endpointSummary,
		endpointNodes + "?name=a&name=b": endpointNodes,
		"/api/fleet/db-7/snapshot":       classForwardSnapshot,
		"/api/fleet/db-7/config":         classForwardConfig,
		endpointSubtree:                  endpointSubtree,
	} {
		if got := bufferClass(in); got != want {
			t.Errorf("bufferClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolve_BuildsNoView (P2-13): routing a name reads the published
// generation and the configuration, and builds no view -- a history request
// used to build the whole tree three times.
func TestResolve_BuildsNoView(t *testing.T) {
	mid := bigChild(t, "mid", 100)
	var views int
	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000, Peers: []config.Peer{{Name: "mid", URL: mid.srv.URL}}}
	p := New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			views++
			return nil, &types.Snapshot{}, time.Second
		},
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.refresh(cfg)
	views = 0

	for range 3 {
		if r := p.Resolve("mid-0042"); r.Kind != RouteSubtree || r.Peer.Name != "mid" {
			t.Fatalf("Resolve = %+v", r)
		}
		p.Has("mid")
	}
	if views != 0 {
		t.Errorf("routing built %d views; want none", views)
	}
}

// TestRelayHistory_OnlyItsOwnJSONIsServed (N1): a relayed history answer is
// decoded and served by this node, so a child answering anything but its JSON
// -- HTML above all, which would be served on this node's origin -- is
// refused, as are an oversize body, another node's range, and an error, which
// comes back as its status alone, never the child's bytes.
func TestRelayHistory_OnlyItsOwnJSONIsServed(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		ctype  string
		status int
		check  func(error) bool
	}{
		"html": {`<script>alert(1)</script>`, "text/html", 0, func(err error) bool {
			var e *RelayNotJSONError
			return errors.As(err, &e)
		}},
		"json that is not a range": {`[1,2,3]`, "", 0, func(err error) bool {
			var e *RelayNotJSONError
			return errors.As(err, &e)
		}},
		"another node's range": {`{"enabled":true,"node":"b","series":[]}`, "", 0, func(err error) bool {
			var e *RelayNotJSONError
			return errors.As(err, &e) && strings.Contains(err.Error(), "another node")
		}},
		"too large": {strings.Repeat("x", maxSnapshotBytes+1), "", 0, func(err error) bool {
			var e *RelayTooLargeError
			return errors.As(err, &e) && !strings.Contains(err.Error(), "127.0.0.1")
		}},
		"an error, with markup": {"bad range <b>x</b>", "text/html", http.StatusBadRequest, func(err error) bool {
			var e *RelayStatusError
			return errors.As(err, &e) && e.Status == http.StatusBadRequest && !strings.Contains(err.Error(), "<b>")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
			mid.historyBody, mid.historyType, mid.historyStatus = []byte(tc.body), tc.ctype, tc.status
			p, cfg := treePoller(t, mid)
			p.refresh(cfg)
			if _, err := p.RelayHistory(context.Background(), p.Resolve("a"), "a", "node=a&metric=cpu"); !tc.check(err) {
				t.Errorf("relay = %v", err)
			}
		})
	}
	p, _ := treePoller(t)
	if _, err := p.RelayHistory(context.Background(), p.Resolve("x"), "x", "node=x"); err == nil {
		t.Error("relay to a name that is not a subtree node was attempted")
	}
}

// TestRefresh_ForgetsRelayedAnswersPastTheirReuse: a relayed answer is kept
// only for its reuse. Every distinct history range and every chain a relay
// arrives with is a key no later request may ask for again, so a refresh drops
// the expired ones -- left alone they accumulated for the life of the process
// -- and keeps those still inside their reuse.
func TestRefresh_ForgetsRelayedAnswersPastTheirReuse(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.relays.done.Store("expired", &reused{expires: time.Now().Add(-time.Second)})
	p.relays.done.Store("current", &reused{expires: time.Now().Add(time.Hour)})

	p.refresh(cfg)

	if _, ok := p.relays.done.Load("expired"); ok {
		t.Error("a relayed answer past its reuse survived a refresh")
	}
	if _, ok := p.relays.done.Load("current"); !ok {
		t.Error("a relayed answer still inside its reuse was dropped")
	}
}
