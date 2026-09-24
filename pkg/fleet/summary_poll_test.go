package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// peer is a test node that serves the endpoints a current build serves, or
// only the whole snapshot when legacy is set, and counts what it was asked for.
type peer struct {
	legacy bool

	mu   sync.Mutex
	prev *types.Snapshot
	cur  *types.Snapshot
	hits map[string]int
}

func (p *peer) set(prev, cur *types.Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prev, p.cur = prev, cur
}

func (p *peer) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[path]
}

func (p *peer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if p.hits == nil {
		p.hits = map[string]int{}
	}
	p.hits[r.URL.Path]++
	prev, cur := p.prev, p.cur
	p.mu.Unlock()

	bodies := map[string]func() any{
		"/api/snapshot": func() any { return cur },
	}
	if !p.legacy {
		bodies["/api/summary"] = func() any { return fleet.Summarize(prev, cur) }
		bodies["/api/snapshot/history"] = func() any { return fleet.HistorySnapshot(cur) }
	}
	body, ok := bodies[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(body()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// startPeer serves p over real HTTP.
func startPeer(t *testing.T, p *peer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

// TestView_MixedVersionFleet is a rollout in progress: one peer serves a
// summary, one predates it. Both appear with the same figures, and the old
// one is marked, because it still costs the aggregator a whole snapshot.
func TestView_MixedVersionFleet(t *testing.T) {
	modern := &peer{}
	modern.set(nil, healthySnapshot("modern"))
	old := &peer{legacy: true}
	old.set(nil, healthySnapshot("old"))
	a, b := startPeer(t, modern), startPeer(t, old)

	cfg := config.Fleet{Peers: []config.Peer{{Name: "modern", URL: a.URL}, {Name: "old", URL: b.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	view := pollUntilReachable(t, p, "modern", "old")

	sources := map[string]types.SummarySource{"local": types.SummarySourceLocal, "modern": types.SummarySourceSummary, "old": types.SummarySourceSnapshot}
	for name, want := range sources {
		n := node(t, view, name)
		if n.Source != want {
			t.Errorf("%s: Source = %q, want %q", name, n.Source, want)
		}
		if n.Hostname != name || n.CPUPercent != 20 || n.Status != fleet.StatusHealthy {
			t.Errorf("%s: summary = host %q cpu %v status %q; want the same figures whichever way it was read",
				name, n.Hostname, n.CPUPercent, n.Status)
		}
	}
	if modern.count("/api/snapshot") != 0 {
		t.Errorf("the modern peer was asked for its whole snapshot %d times; the poll carries summaries",
			modern.count("/api/snapshot"))
	}
	if view.Rollup == nil || view.Rollup.Nodes != 3 || view.Rollup.Healthy != 3 {
		t.Errorf("rollup = %+v; want 3 healthy nodes", view.Rollup)
	}
}

// TestView_OldPeerSwapRateAcrossPolls: a peer too old to serve a summary has
// its paging rate computed by the aggregator, from its counters across two
// polls -- and a counter that goes backwards, as it does when the peer
// reboots, makes that interval unknown rather than a huge or negative rate.
func TestView_OldPeerSwapRateAcrossPolls(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := &peer{legacy: true}
	old.set(nil, pagingSnapshot(t0, 100, 1000))
	srv := startPeer(t, old)

	cfg := config.Fleet{Peers: []config.Peer{{Name: "old", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	refresh := func() types.FleetNode {
		done := make(chan struct{})
		close(done)
		p.Run(done) // one cycle, then return
		return node(t, p.View(fleet.Query{}), "old")
	}

	if n := refresh(); n.SwapRateAvailable {
		t.Errorf("first poll: SwapRateAvailable = true with nothing to difference against")
	}

	old.set(nil, pagingSnapshot(t0.Add(4*time.Second), 100, 1200))
	n := refresh()
	if !n.SwapRateAvailable || n.SwapOutRate != 200*4096/4 {
		t.Errorf("second poll: available %v, out %v; want 200 pages x 4 KiB over 4 s", n.SwapRateAvailable, n.SwapOutRate)
	}

	// The peer reboots: its counters restart from zero.
	old.set(nil, pagingSnapshot(t0.Add(60*time.Second), 3, 7))
	if n := refresh(); n.SwapRateAvailable || n.SwapOutRate != 0 {
		t.Errorf("after a reboot: available %v, out %v; a reset must read as unknown", n.SwapRateAvailable, n.SwapOutRate)
	}

	old.set(nil, pagingSnapshot(t0.Add(62*time.Second), 3, 9))
	if n := refresh(); !n.SwapRateAvailable || n.SwapOutRate != 2*4096/2 {
		t.Errorf("the poll after the reboot: available %v, out %v; the new baseline should measure again",
			n.SwapRateAvailable, n.SwapOutRate)
	}
}

// TestView_LocalSwapRateFromConsecutiveSnapshots: the aggregator's own node is
// summarised from the monitor's two latest collections, like any other node.
func TestView_LocalSwapRateFromConsecutiveSnapshots(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	prev, cur := pagingSnapshot(t0, 0, 0), pagingSnapshot(t0.Add(time.Second), 0, 256)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "p", URL: "http://127.0.0.1:1"}}, TimeoutMs: 100}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return prev, cur, 0 },
		func() string { return "local" }, quiet())
	n := node(t, p.View(fleet.Query{}), "local")
	if !n.SwapRateAvailable || n.SwapOutRate != 256*4096 {
		t.Errorf("local swap-out = available %v, %v; want 1 MiB/s", n.SwapRateAvailable, n.SwapOutRate)
	}
}

// TestErrNoReading_NamesTheNode: the 404 body says which node had nothing.
func TestErrNoReading_NamesTheNode(t *testing.T) {
	err := &fleet.ErrNoReading{Node: "mon0"}
	if got := err.Error(); got != "no reading yet from fleet node mon0" {
		t.Errorf("Error() = %q", got)
	}
}

// TestPeerSnapshot_ConcurrentDrillInsOfADeadPeerShareOneAttempt: during an
// outage, browsers queued behind a failed fetch must get its answer, not each
// wait out the peer's timeout again.
func TestPeerSnapshot_ConcurrentDrillInsOfADeadPeerShareOneAttempt(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var status *fleet.ErrPeerStatus
			if _, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); !errors.As(err, &status) {
				t.Errorf("PeerSnapshot(omega) = %v; want the peer's 503", err)
			}
		}()
	}
	wg.Wait()
	if n := attempts.Load(); n != 1 {
		t.Errorf("8 concurrent drill-ins of a dead peer made %d attempts; want 1", n)
	}
}

// TestPeerSnapshot_ARepointedPeerIsNotServedTheOldMachinesSnapshot: a reload
// that points a name at another URL must not be answered from the old one.
func TestPeerSnapshot_ARepointedPeerIsNotServedTheOldMachinesSnapshot(t *testing.T) {
	oldHost := &peer{}
	oldHost.set(nil, healthySnapshot("old-machine"))
	newHost := &peer{}
	newHost.set(nil, healthySnapshot("new-machine"))
	a, b := startPeer(t, oldHost), startPeer(t, newHost)

	var cfg atomic.Pointer[config.Fleet]
	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: a.URL}}, TimeoutMs: 2000})
	p := fleet.New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	if got, err := p.PeerSnapshot(context.Background(), "db", time.Minute); err != nil || got.Snapshot.Host.Hostname != "old-machine" {
		t.Fatalf("PeerSnapshot(db) = %+v, %v", got, err)
	}
	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: b.URL}}, TimeoutMs: 2000})
	got, err := p.PeerSnapshot(context.Background(), "db", time.Minute)
	if err != nil || got.Snapshot.Host.Hostname != "new-machine" {
		t.Errorf("after repointing, PeerSnapshot(db) = host %q, %v; want the new machine", got.Snapshot.Host.Hostname, err)
	}
}

// TestRefresh_OldPeerKeepsItsConnection: an old peer answers 404 on the
// summary every cycle. Its connection must survive that, or every cycle pays
// a new connection -- and on an https fleet a new handshake -- for the
// fallback fetch that follows.
func TestRefresh_OldPeerKeepsItsConnection(t *testing.T) {
	old := &peer{legacy: true}
	old.set(nil, healthySnapshot("old"))
	srv := httptest.NewUnstartedServer(old)
	var conns atomic.Int64
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	cfg := config.Fleet{Peers: []config.Peer{{Name: "old", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	for range 3 {
		done := make(chan struct{})
		close(done)
		p.Run(done)
	}
	if n := old.count("/api/snapshot"); n != 3 {
		t.Fatalf("whole snapshot fetched %d times in 3 cycles, want 3", n)
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("3 cycles of summary-404-then-snapshot opened %d connections; want 1 reused", n)
	}
}

// TestView_ARepointedOldPeerStartsAFreshBaseline: when a reload points a name
// at another machine, the old machine's swap counters are not a baseline for
// the new one's, and its last summary is not the new one's last known state.
func TestView_ARepointedOldPeerStartsAFreshBaseline(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	first := &peer{legacy: true}
	first.set(nil, pagingSnapshot(t0, 0, 0))
	second := &peer{legacy: true}
	second.set(nil, pagingSnapshot(t0.Add(3*time.Second), 5000, 90000))
	a, b := startPeer(t, first), startPeer(t, second)

	var cfg atomic.Pointer[config.Fleet]
	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: a.URL}}, TimeoutMs: 2000})
	p := fleet.New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	cycle := func() types.FleetNode {
		done := make(chan struct{})
		close(done)
		p.Run(done)
		return node(t, p.View(fleet.Query{}), "db")
	}

	cycle()
	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: b.URL}}, TimeoutMs: 2000})
	if n := cycle(); n.SwapRateAvailable {
		t.Errorf("first poll of the repointed peer: rate %v from another machine's counters; want unknown", n.SwapOutRate)
	}
}

// TestView_OldPeerRepeatedSnapshotKeepsItsRate: polling faster than the peer
// collects returns the same snapshot twice, which is no new interval -- the
// last measured rate stands rather than flickering to unknown.
func TestView_OldPeerRepeatedSnapshotKeepsItsRate(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := &peer{legacy: true}
	old.set(nil, pagingSnapshot(t0, 0, 0))
	srv := startPeer(t, old)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "old", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	cycle := func() types.FleetNode {
		done := make(chan struct{})
		close(done)
		p.Run(done)
		return node(t, p.View(fleet.Query{}), "old")
	}

	cycle()
	old.set(nil, pagingSnapshot(t0.Add(2*time.Second), 0, 100))
	want := cycle().SwapOutRate
	if n := cycle(); !n.SwapRateAvailable || n.SwapOutRate != want {
		t.Errorf("the same snapshot polled again: available %v, out %v; want the measured %v to stand",
			n.SwapRateAvailable, n.SwapOutRate, want)
	}
	old.set(nil, pagingSnapshot(t0.Add(4*time.Second), 0, 300))
	if n := cycle(); !n.SwapRateAvailable || n.SwapOutRate != 200*4096/2 {
		t.Errorf("the next snapshot: out %v; want 200 pages over the 2 s since the last new one", n.SwapOutRate)
	}
}

// TestView_OldLibraryPeerAnsweringPlainTextFallsBack: an older build embedded
// without dashboard assets answers /api/summary from its catch-all status
// page with a 200 and plain text. That is a peer without the endpoint, not a
// dead one.
func TestView_OldLibraryPeerAnsweringPlainTextFallsBack(t *testing.T) {
	body, err := json.Marshal(healthySnapshot("embedded"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/snapshot" {
			if _, err := w.Write(body); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if _, err := w.Write([]byte("go-sysmon is running\n")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "embedded", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	done := make(chan struct{})
	close(done)
	p.Run(done)
	if n := node(t, p.View(fleet.Query{}), "embedded"); !n.Reachable || n.Source != types.SummarySourceSnapshot {
		t.Errorf("embedded = reachable %v, source %q, error %q; want it read by snapshot", n.Reachable, n.Source, n.Error)
	}
}

// TestPeerSnapshot_ReusedForTheFleetInterval: a browser refreshing faster than
// the fleet polls is answered from the last fetch, not by fetching the peer's
// whole snapshot on every tick.
func TestPeerSnapshot_ReusedForTheFleetInterval(t *testing.T) {
	remote := &peer{}
	remote.set(nil, healthySnapshot("omega"))
	srv := startPeer(t, remote)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 60000, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	for range 5 {
		if _, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); err != nil {
			t.Fatalf("PeerSnapshot: %v", err)
		}
	}
	if n := remote.count("/api/snapshot"); n != 1 {
		t.Errorf("5 drill-ins within a 60 s fleet interval fetched the snapshot %d times, want 1", n)
	}

	// Following the dashboard's rate control, which sets the fleet cadence.
	p.SetPollInterval(time.Second)
	waitFor(t, func() bool {
		_, err := p.PeerSnapshot(context.Background(), "omega", time.Minute)
		return err == nil && remote.count("/api/snapshot") == 2
	})
}

// TestView_APeerRepointedMidPollShowsNoStaleMachine: a poll that answers from
// an address a reload has since replaced is not shown under the name.
func TestView_APeerRepointedMidPollShowsNoStaleMachine(t *testing.T) {
	oldHost := &peer{}
	oldHost.set(nil, healthySnapshot("old-machine"))
	srv := startPeer(t, oldHost)
	var cfg atomic.Pointer[config.Fleet]
	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: srv.URL}}, TimeoutMs: 2000})
	p := fleet.New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())
	done := make(chan struct{})
	close(done)
	p.Run(done)

	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: "http://127.0.0.1:1"}}, TimeoutMs: 2000})
	n := node(t, p.View(fleet.Query{}), "db")
	if n.Hostname == "old-machine" || n.Reachable {
		t.Errorf("after repointing, db = host %q reachable %v; want no reading until the new address is polled", n.Hostname, n.Reachable)
	}
}

// TestPeerSnapshot_APollFailureEndsReuse: once the summary poll has found a
// peer unreachable, a drill-in is not answered as live from before that.
func TestPeerSnapshot_APollFailureEndsReuse(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		body := map[string]any{"/api/snapshot": healthySnapshot("omega"), "/api/summary": fleet.Summarize(nil, healthySnapshot("omega"))}[r.URL.Path]
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 60000, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	if got, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); err != nil || !got.Reachable {
		t.Fatalf("first drill-in = %+v, %v", got, err)
	}
	down.Store(true)
	done := make(chan struct{})
	close(done)
	p.Run(done)
	if node(t, p.View(fleet.Query{}), "omega").Reachable {
		t.Fatal("the poll did not find omega unreachable")
	}
	if got, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); err != nil || got.Reachable {
		t.Errorf("drill-in after a failed poll = reachable %v, %v; want the stale snapshot marked unreachable", got.Reachable, err)
	}
}

// TestPeerSnapshot_AnAbandonedDrillInIsCancelledAndNotRemembered: a browser
// that goes away takes its outbound fetch with it, and its cancellation is not
// kept as the peer's answer for the next asker.
func TestPeerSnapshot_AnAbandonedDrillInIsCancelledAndNotRemembered(t *testing.T) {
	var stall atomic.Bool
	stall.Store(true)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall.Load() {
			close(entered)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		if err := json.NewEncoder(w).Encode(healthySnapshot("omega")); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 60000, TimeoutMs: 60000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.PeerSnapshot(ctx, "omega", time.Minute)
		result <- err
	}()
	<-entered
	cancel()
	<-cancelled
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned drill-in = %v; want context.Canceled", err)
	}

	stall.Store(false)
	got, err := p.PeerSnapshot(context.Background(), "omega", time.Minute)
	if err != nil || !got.Reachable || got.Snapshot.Host.Hostname != "omega" {
		t.Errorf("the next drill-in = %+v, %v; want a live fetch, not the abandoned one's cancellation", got, err)
	}
}

// TestPeerSnapshot_ARepointDuringTheFetchIsNotServed: a reload that moves the
// name while its snapshot is in flight makes that snapshot another machine's.
// It is neither served nor kept, and the next drill-in reaches the new
// address.
func TestPeerSnapshot_ARepointDuringTheFetchIsNotServed(t *testing.T) {
	newHost := &peer{}
	newHost.set(nil, healthySnapshot("new-machine"))
	b := startPeer(t, newHost)
	var cfg atomic.Pointer[config.Fleet]
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: b.URL}}, TimeoutMs: 2000, PollIntervalMs: 60000})
		if err := json.NewEncoder(w).Encode(healthySnapshot("old-machine")); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer old.Close()
	cfg.Store(&config.Fleet{Peers: []config.Peer{{Name: "db", URL: old.URL}}, TimeoutMs: 2000, PollIntervalMs: 60000})
	p := fleet.New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	var moved *fleet.ErrPeerMoved
	if got, err := p.PeerSnapshot(context.Background(), "db", time.Minute); !errors.As(err, &moved) {
		t.Fatalf("drill-in overtaken by a reload = %+v, %v; want ErrPeerMoved", got, err)
	}
	got, err := p.PeerSnapshot(context.Background(), "db", time.Minute)
	if err != nil || got.Snapshot.Host.Hostname != "new-machine" {
		t.Errorf("next drill-in = %+v, %v; want the new machine", got, err)
	}
	if (&fleet.ErrPeerMoved{Peer: "db"}).Error() == "" {
		t.Error("ErrPeerMoved has no message")
	}
}

// TestPeerSnapshot_AWriteBudgetTimeoutIsAPeerFailure: the server's own deadline
// running out on a silent peer is that peer's failure, and it is remembered --
// the next drill-in is answered from it rather than waiting out the peer again.
func TestPeerSnapshot_AWriteBudgetTimeoutIsAPeerFailure(t *testing.T) {
	var requests atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 60000, TimeoutMs: 60000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	if _, err := p.PeerSnapshot(context.Background(), "omega", 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drill-in past its budget = %v; want the deadline", err)
	}
	if _, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("next drill-in = %v; want the remembered failure", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests to the silent peer; want the failure reused, not waited out again", n)
	}
}

// TestPeerSnapshot_TheAskersOwnDeadlineIsNotRemembered: a caller whose own
// deadline passes -- a shorter timeout in middleware -- has abandoned the
// drill-in, and the next asker is not answered from its timeout.
func TestPeerSnapshot_TheAskersOwnDeadlineIsNotRemembered(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if slow.Load() {
			<-r.Context().Done()
			return
		}
		if err := json.NewEncoder(w).Encode(healthySnapshot("omega")); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 60000, TimeoutMs: 60000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.PeerSnapshot(ctx, "omega", time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drill-in past the asker's deadline = %v", err)
	}
	slow.Store(false)
	got, err := p.PeerSnapshot(context.Background(), "omega", time.Minute)
	if err != nil || !got.Reachable {
		t.Errorf("next drill-in = %+v, %v; want a fresh fetch, not the first asker's timeout", got, err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests; want the second asker to fetch", n)
	}
}

// TestView_CollectionIntervalIsReportedNeverAssumed: this node reports the
// interval its monitor collects at, a current peer reports its own, and a
// peer read by its whole snapshot -- which does not carry it -- is unknown.
func TestView_CollectionIntervalIsReportedNeverAssumed(t *testing.T) {
	modernSummary := fleet.LocalSummary(nil, healthySnapshot("modern"), 2*time.Second)
	modern := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/summary" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(modernSummary); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer modern.Close()
	old := &peer{legacy: true}
	old.set(nil, healthySnapshot("old"))
	b := startPeer(t, old)

	cfg := config.Fleet{Peers: []config.Peer{{Name: "modern", URL: modern.URL}, {Name: "old", URL: b.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			return nil, healthySnapshot("local"), 500 * time.Millisecond
		},
		func() string { return "local" }, quiet())
	view := pollUntilReachable(t, p, "modern", "old")

	want := map[string]int64{"local": 500, "modern": 2000, "old": 0}
	for name, ms := range want {
		if got := node(t, view, name).CollectionIntervalMs; got != ms {
			t.Errorf("%s: collection_interval_ms = %d, want %d", name, got, ms)
		}
	}
}

// TestPeerSnapshot_TheBudgetCoversWaitingForTheLock: a drill-in queued behind
// another request's slow fetch gives up when its own budget runs out, rather
// than waiting and then starting a full-budget fetch of its own.
func TestPeerSnapshot_TheBudgetCoversWaitingForTheLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 60000, TimeoutMs: 60000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	first := make(chan error, 1)
	go func() {
		_, err := p.PeerSnapshot(context.Background(), "omega", time.Minute)
		first <- err
	}()
	<-entered

	started := time.Now()
	if _, err := p.PeerSnapshot(context.Background(), "omega", 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued drill-in = %v; want its budget's deadline", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("queued drill-in took %v; its budget was 100ms", waited)
	}
}

// TestView_AnOldPeersThroughputIsUnknown: a peer too old to send
// rates_available cannot say its rates were measured, and is left out of the
// network total rather than assumed -- the denominator shows the gap.
func TestView_AnOldPeersThroughputIsUnknown(t *testing.T) {
	old := &peer{legacy: true}
	snap := healthySnapshot("old")
	snap.Networks = []types.NetworkInfo{{Name: "eno1", BytesRecvRate: 5000}}
	old.set(nil, snap)
	srv := startPeer(t, old)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "old", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			local := healthySnapshot("local")
			local.Networks = []types.NetworkInfo{{Name: "eth0", BytesRecvRate: 100, RatesAvailable: true}}
			return nil, local, 0
		},
		func() string { return "local" }, quiet())
	view := pollUntilReachable(t, p, "old")

	if n := node(t, view, "old"); n.NetReported {
		t.Errorf("old peer reports throughput %v it cannot vouch for", n.NetRxRate)
	}
	if got := view.Rollup.Network; got == nil || got.Nodes != 1 || got.Of != 2 || got.RxRate != 100 {
		t.Errorf("network total = %+v; want this node's 100 across 1 of 2", got)
	}
}
