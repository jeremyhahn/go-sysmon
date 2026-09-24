package fleet

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// validChild is a subtree that validates: its child, one node below.
func validChild() *types.FleetSubtree {
	return &types.FleetSubtree{
		Nodes: []types.FleetIndexEntry{
			{Name: "mid", Status: types.NodeStatusHealthy, Reachable: true},
			{Name: "a", Depth: 1, Status: types.NodeStatusHealthy, Reachable: true},
		},
		Rollup:      &types.FleetRollup{Nodes: 2, Healthy: 2, Reporting: 2, Attention: []types.NodeAttention{}, Builds: []types.FleetBuild{}},
		Aggregators: []string{newInstanceID()},
	}
}

// TestValidateSubtree_DepthIsBounded (N2): a depth past MaxTreeDepth is not
// one a child composes, and MaxInt in particular must not wrap to a negative
// depth this node then passes on.
func TestValidateSubtree_DepthIsBounded(t *testing.T) {
	for _, depth := range []int{MaxTreeDepth + 1, math.MaxInt} {
		s := validChild()
		s.Nodes[1].Depth = depth
		var invalid *ErrSubtreeInvalid
		if err := validateSubtree("mid", s); !errors.As(err, &invalid) {
			t.Errorf("depth %d = %v, want ErrSubtreeInvalid", depth, err)
		}
	}
	s := validChild()
	s.Nodes[1].Depth = MaxTreeDepth
	if err := validateSubtree("mid", s); err != nil {
		t.Errorf("depth %d = %v, want valid (and then refused as too deep)", MaxTreeDepth, err)
	}
	if depthRefusal("mid", s) == "" {
		t.Error("a subtree at MaxTreeDepth below its child was not refused as too deep")
	}
}

// TestValidateSubtree_TheRollupMustAddUp (N3): every counter a count, every
// sum a finite non-negative sum, the states partitioning the nodes as the index
// lists them, every denominator the reachable nodes, the index's statuses
// statuses and its strings bounded.
func TestValidateSubtree_TheRollupMustAddUp(t *testing.T) {
	for name, mutate := range map[string]func(*types.FleetSubtree){
		"negative count":             func(s *types.FleetSubtree) { s.Rollup.SubtreeErrors = -1 },
		"states do not add up":       func(s *types.FleetSubtree) { s.Rollup.Healthy = 1 },
		"states are not the index's": func(s *types.FleetSubtree) { s.Rollup.Healthy, s.Rollup.Warning = 1, 1 },
		"reporting past nodes":       func(s *types.FleetSubtree) { s.Rollup.Reporting = 3 },
		"cpu unknown past reporting": func(s *types.FleetSubtree) { s.Rollup.CPUUnknown = 3 },
		"negative cpu unknown":       func(s *types.FleetSubtree) { s.Rollup.CPUUnknown = -1 },
		"never answered past unreachable": func(s *types.FleetSubtree) {
			s.Rollup.NeverAnswered = 1
		},
		"an infinite absence": func(s *types.FleetSubtree) { s.Rollup.LongestAbsenceSeconds = math.Inf(1) },
		"a NaN extreme": func(s *types.FleetSubtree) {
			s.Rollup.CPU = &types.FleetExtreme{Node: "a", Value: math.NaN()}
		},
		"an extreme on no node": func(s *types.FleetSubtree) {
			s.Rollup.CPU = &types.FleetExtreme{Node: "a/b", Value: 3}
		},
		"negative power": func(s *types.FleetSubtree) {
			s.Rollup.Power = &types.FleetSum{Value: -5, Nodes: 1, Of: 2}
		},
		"power over the wrong denominator": func(s *types.FleetSubtree) {
			s.Rollup.Power = &types.FleetSum{Value: 5, Nodes: 1, Of: 9}
		},
		"network nodes past its denominator": func(s *types.FleetSubtree) {
			s.Rollup.Network = &types.FleetNet{Nodes: 3, Of: 2}
		},
		"more unhealthy than devices": func(s *types.FleetSubtree) {
			s.Rollup.SMART = &types.FleetHealth{Unhealthy: 4, Devices: 3, Nodes: 1, Of: 2}
		},
		"an attention entry that is not a state": func(s *types.FleetSubtree) {
			s.Rollup.Attention = []types.NodeAttention{{Name: "a", Status: "bored"}}
		},
		"builds counting more than reachable": func(s *types.FleetSubtree) {
			s.Rollup.Builds = []types.FleetBuild{{Commit: "x", Count: 9}}
			s.Rollup.BuildGroups = 1
		},
		"an index status that is not one": func(s *types.FleetSubtree) { s.Nodes[1].Status = "fine" },
		"a host name longer than DNS allows": func(s *types.FleetSubtree) {
			s.Nodes[1].Hostname = strings.Repeat("h", maxHostnameBytes+1)
		},
		"a reason too long": func(s *types.FleetSubtree) {
			s.Nodes[1].Reasons = []string{strings.Repeat("r", maxReasonBytes+1)}
		},
		"a build too long": func(s *types.FleetSubtree) { s.Nodes[1].Commit = strings.Repeat("c", maxCommitBytes+1) },
	} {
		s := validChild()
		mutate(s)
		var invalid *ErrSubtreeInvalid
		if err := validateSubtree("mid", s); !errors.As(err, &invalid) {
			t.Errorf("%s: %v, want ErrSubtreeInvalid", name, err)
		}
	}
	if err := validateSubtree("mid", validChild()); err != nil {
		t.Errorf("a valid subtree = %v", err)
	}
}

// TestFlight_OneAskersBudgetDoesNotFailTheOthers (N4): shared work runs
// detached from whoever started it. An asker with a short deadline gives up
// alone; one that leaves does not cancel the work; the work runs to the
// latest deadline any asker has, within the limit.
func TestFlight_OneAskersBudgetDoesNotFailTheOthers(t *testing.T) {
	g := newFlightGroup(1, 0)
	release := make(chan struct{})
	var runs atomic.Int64
	fn := func(ctx context.Context) (any, error) {
		runs.Add(1)
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// The short asker starts the work with its own deadline.
	short, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	shortDeadline, _ := short.Deadline()
	var wg sync.WaitGroup
	var shortErr error
	shortDone := make(chan struct{})
	go func() {
		defer close(shortDone)
		_, shortErr = g.do(short, "k", time.Minute, fn)
	}()
	await.Until(t, "the short asker's work to be in flight", func(context.Context) bool { return g.pending("k") })

	// A second asker joins the same work with time to spare, which extends
	// the work's deadline to its own.
	var val any
	var err error
	wg.Add(1)
	go func() {
		defer wg.Done()
		long, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		val, err = g.do(long, "k", time.Minute, fn)
	}()
	await.Until(t, "the patient asker to extend the work's deadline", func(context.Context) bool {
		d, ok := g.deadline("k")
		return ok && d.After(shortDeadline)
	})

	// The short asker gives up alone, at its own deadline; the work goes on.
	await.Recv(t, "the short asker to give up", shortDone)
	if !g.pending("k") {
		t.Fatal("the work ended with the asker that started it")
	}
	close(release)
	wg.Wait()

	if !errors.Is(shortErr, context.DeadlineExceeded) {
		t.Errorf("the short asker = %v, want its own deadline", shortErr)
	}
	if err != nil || val != "done" || runs.Load() != 1 {
		t.Errorf("the patient asker = %v, %v after %d runs; want the one run's answer", val, err, runs.Load())
	}
}

// TestFlight_TheLimitAndReuse: the work never outlives the limit whoever asks,
// and a finished result is reused for the reuse window.
func TestFlight_TheLimitAndReuse(t *testing.T) {
	g := newFlightGroup(2, time.Minute)
	_, err := g.do(context.Background(), "slow", 20*time.Millisecond, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("work past its limit = %v, want the deadline", err)
	}
	var runs atomic.Int64
	for range 3 {
		if v, err := g.do(context.Background(), "fast", time.Second, func(context.Context) (any, error) {
			runs.Add(1)
			return 7, nil
		}); err != nil || v != 7 {
			t.Fatalf("fast = %v, %v", v, err)
		}
	}
	if runs.Load() != 1 {
		t.Errorf("three asks within the reuse ran %d times; want once", runs.Load())
	}
	g.forget()
}

// TestFlight_ConcurrencyIsBounded (L5): no more than the group's bound run at
// once, and one group's queue does not hold up another's.
func TestFlight_ConcurrencyIsBounded(t *testing.T) {
	views, fills := newFlightGroup(2, 0), newFlightGroup(2, 0)
	var running, peak atomic.Int64
	block := make(chan struct{})
	work := func(ctx context.Context) (any, error) {
		n := running.Add(1)
		for {
			m := peak.Load()
			if n <= m || peak.CompareAndSwap(m, n) {
				break
			}
		}
		defer running.Add(-1)
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, nil
	}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := views.do(context.Background(), string(rune('a'+i)), 5*time.Second, work); err != nil {
				t.Errorf("view: %v", err)
			}
		}()
	}
	await.Until(t, "the view group to fill", func(context.Context) bool { return running.Load() == 2 })
	// The view group is full; a fill still runs at once.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := fills.do(context.Background(), "fill", 5*time.Second, func(context.Context) (any, error) { return nil, nil }); err != nil {
			t.Errorf("fill: %v", err)
		}
	}()
	// A fill queued behind the full view group never runs, and fails here.
	await.Recv(t, "a fill to run while the view group is full", done)
	close(block)
	wg.Wait()
	if peak.Load() > 2 {
		t.Errorf("%d ran at once in a group of 2", peak.Load())
	}
}

// TestPage_AParentsFillIsNotQueuedBehindViews (L5): this node's own views and
// the fills its parent asks it for are bounded apart, so views that have filled
// their bound -- anonymous requests can -- never hold up the parent's fill.
func TestPage_AParentsFillIsNotQueuedBehindViews(t *testing.T) {
	busy := bigChild(t, "busy", 3000)
	calm := bigChild(t, "calm", 10)
	p, cfg := treePoller(t, busy, calm)
	// Held views outlast any wait here, so a fill queued behind them can
	// only be answered by their giving up, never by being let through.
	cfg.TimeoutMs = 10 * int(await.Timeout/time.Millisecond)
	p.cfgFn = func() config.Fleet { return cfg }
	p.refresh(cfg)

	release := hold(t, &busy.nodesHeld)
	var wg sync.WaitGroup
	for i := range maxConcurrentFetches + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.View(context.Background(), Query{Search: "busy-", Offset: i * namesPerRequest, Limit: namesPerRequest})
		}()
	}
	await.Until(t, "the views' bound to be full with more waiting", func(context.Context) bool {
		return busy.inFlight.Load() == maxConcurrentFetches &&
			await.Parked("fleet.(*flightGroup).do.func1(") > maxConcurrentFetches
	})

	filled := make(chan types.FleetNodes, 1)
	go func() {
		filled <- p.Nodes(WithAncestry(context.Background(), []string{newInstanceID()}), []string{"calm-0003"})
	}()
	got := await.Recv(t, "the parent's fill to be answered while the views wait", filled)
	release()
	wg.Wait()
	if len(got.Nodes) != 1 || got.Nodes[0].Source == types.SummarySourceIndex {
		t.Errorf("the parent's fill = %+v; want calm-0003 fetched whole", got.Nodes)
	}
}

// TestPage_ACardNeverContradictsItself (P2-3, residual): a cached summary whose
// alert counts give a different status than the index is from another moment;
// the card is the index's, never a status beside counts that say otherwise.
func TestPage_ACardNeverContradictsItself(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	p.View(context.Background(), Query{Limit: 25})

	// The child's summary of a now carries a critical alert its index, which
	// the roll-up counts, does not know yet.
	n := mid.nodes["a"]
	n.CriticalCount, n.Alerts = 1, []types.Alert{crit("disk failing")}
	mid.nodes["a"] = n
	c, _ := p.pageCache.Load("a")
	stale := *c.(*cachedNode)
	stale.node = n
	p.pageCache.Store("a", &stale)

	a := find(t, p.View(context.Background(), Query{Limit: 25}).Nodes, "a")
	if a.Status != types.NodeStatusHealthy || a.CriticalCount != 0 || a.Source != types.SummarySourceIndex {
		t.Errorf("a = status %q with %d critical (source %q); want the index's healthy, figures withheld",
			a.Status, a.CriticalCount, a.Source)
	}
}

// TestSubtree_AggregatorsAgeAndFailClosed (L2): a child's aggregators carried
// past failed reads expire; a child naming more than the bound is refused, not
// trusted in part, and its IDs are not named on.
func TestSubtree_AggregatorsAgeAndFailClosed(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	clk := newManualClock()
	p.now = clk.Now
	p.refresh(cfg)
	mid.subtreeDown.Store(true)

	// A read that fails inside the window keeps naming the child's IDs...
	clk.Advance(aggregatorAge*p.effectiveInterval(cfg) - time.Second)
	p.refresh(cfg)
	if sub, _ := p.Subtree(); len(sub.Aggregators) != 2 {
		t.Errorf("after one failed read = %v; want this node and the child's ID, still named", sub.Aggregators)
	}
	// ...and one past it names this node alone.
	clk.Advance(time.Second)
	p.refresh(cfg)
	if sub, _ := p.Subtree(); len(sub.Aggregators) != 1 {
		t.Errorf("after the child's IDs aged out = %v; want this node alone", sub.Aggregators)
	}

	many := newFakeChild(t, "many", entry("b", types.NodeStatusHealthy))
	for range maxAggregators {
		many.subtree.Aggregators = append(many.subtree.Aggregators, newInstanceID())
	}
	q, qcfg := treePoller(t, many)
	q.refresh(qcfg)
	v := q.View(context.Background(), Query{Limit: 25})
	if e := find(t, v.Nodes, "many").SubtreeError; !strings.Contains(e, "aggregators below it") || v.Page.Total != 2 {
		t.Errorf("a child naming too many = %q, fleet of %d; want refused", e, v.Page.Total)
	}
	if sub, _ := q.Subtree(); len(sub.Aggregators) != 1 {
		t.Errorf("a refused-for-size child's %d IDs were named on", len(sub.Aggregators)-1)
	}
}

// TestPage_OnlyRequestedNamesAreCached (L3): a child answering with more than
// it was asked for does not fill this node's cache with it.
func TestPage_OnlyRequestedNamesAreCached(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy), entry("b", types.NodeStatusHealthy))
	mid.nodesExtra = true
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	if _, err := p.fetchNodes(context.Background(), cfg, "mid", mid.srv.URL, []string{"a"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, ok := p.pageCache.Load("b"); ok {
		t.Error("b, not asked for, was cached")
	}
	if _, ok := p.pageCache.Load("a"); !ok {
		t.Error("a, asked for, was not cached")
	}
}

// TestRelay_IdenticalRelaysCostOne (L4): identical relays through this node
// cost the child one request, however many arrive together, and a repeat
// within the reuse window costs none.
func TestRelay_IdenticalRelaysCostOne(t *testing.T) {
	mid := newFakeChild(t, "mid", entry("a", types.NodeStatusHealthy))
	p, cfg := treePoller(t, mid)
	p.refresh(cfg)
	// The child holds the drill-in until all 20 are waiting on it.
	release := hold(t, &mid.snapshotHeld)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.PeerSnapshot(context.Background(), "a", time.Minute); err != nil {
				t.Errorf("drill-in: %v", err)
			}
		}()
	}
	await.Until(t, "all 20 drill-ins to wait on the relay", func(context.Context) bool {
		return await.Parked("fleet.(*Poller).PeerSnapshot(", "fleet.(*flightGroup).do(") == 20
	})
	release()
	wg.Wait()
	if _, err := p.PeerSnapshot(context.Background(), "a", time.Second); err != nil {
		t.Fatalf("drill-in: %v", err)
	}
	if n := mid.snapshotRequests.Load(); n != 1 {
		t.Errorf("21 identical drill-ins cost the child %d requests; want 1", n)
	}
}

// pending reports whether work for key is in flight.
func (g *flightGroup) pending(key string) bool {
	_, ok := g.calls.Load(key)
	return ok
}

// deadline is the deadline of key's work in flight, if any is.
func (g *flightGroup) deadline(key string) (time.Time, bool) {
	v, ok := g.calls.Load(key)
	if !ok {
		return time.Time{}, false
	}
	c := v.(*call)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, true
}
