package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// historyPeer is a current-build peer whose summary answers at once and whose
// history endpoint runs history, which a test uses to stall or count it.
type historyPeer struct {
	summaries atomic.Int64
	snapshots atomic.Int64
	history   func(w http.ResponseWriter, r *http.Request)
}

func (h *historyPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snap := healthyPeerSnapshot()
	var body any
	switch r.URL.Path {
	case endpointSummary:
		h.summaries.Add(1)
		body = Summarize(nil, snap)
	case endpointHistory:
		h.history(w, r)
		return
	case endpointSnapshot:
		h.snapshots.Add(1)
		body = snap
	default:
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func healthyPeerSnapshot() *types.Snapshot {
	return &types.Snapshot{
		Timestamp: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		Host:      types.HostInfo{Hostname: "peer"},
		CPUs:      []types.CPUInfo{{UsagePercent: 10, UsageAvailable: true, Flags: []string{"fpu"}}},
		Processes: types.ProcessSummary{Total: 1, Processes: []types.ProcessDetail{{PID: 1}}},
	}
}

// servesHistory answers the history endpoint with the slimmed payload.
func servesHistory(w http.ResponseWriter, _ *http.Request) {
	if err := json.NewEncoder(w).Encode(HistorySnapshot(healthyPeerSnapshot())); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// recordingPoller returns a poller over the given peers that records history
// at cadence, and the readings it has observed.
func recordingPoller(t *testing.T, cfg *atomic.Pointer[config.Fleet], cadence *atomic.Int64) (*Poller, *atomic.Int64) {
	t.Helper()
	p := New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthyPeerSnapshot(), 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var readings atomic.Int64
	p.Observe(func(string, *types.Snapshot) { readings.Add(1) },
		func() time.Duration { return time.Duration(cadence.Load()) })
	t.Cleanup(p.history.stop)
	return p, &readings
}

func fleetOf(timeout time.Duration, urls ...string) *atomic.Pointer[config.Fleet] {
	cfg := config.Fleet{TimeoutMs: int(timeout.Milliseconds())}
	for i, u := range urls {
		cfg.Peers = append(cfg.Peers, config.Peer{Name: fmt.Sprintf("peer-%02d", i), URL: u})
	}
	var ptr atomic.Pointer[config.Fleet]
	ptr.Store(&cfg)
	return &ptr
}

func cadenceOf(d time.Duration) *atomic.Int64 {
	var c atomic.Int64
	c.Store(int64(d))
	return &c
}

// TestRecorder_AStalledHistoryEndpointNeverDelaysTheSummary is the owner's
// requirement on #181: a slow or failing history endpoint must never affect a
// card or the freshness of the summaries. The history handler here never
// answers, and the fleet timeout is a minute -- so a poll that waited on it in
// any way would hang the test rather than pass it.
func TestRecorder_AStalledHistoryEndpointNeverDelaysTheSummary(t *testing.T) {
	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	peer := &historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	}}
	srv := httptest.NewServer(peer)
	defer srv.Close()
	defer close(release)

	cfg := fleetOf(time.Minute, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Second))
	// A clock that stands still: a card kept current by every cycle is then
	// exactly zero seconds old, not "recent enough".
	p.now = newManualClock().Now

	p.refresh(*cfg.Load())
	p.history.sync(*cfg.Load())
	<-entered // the worker is now stuck inside the history fetch

	for i := range 5 {
		p.refresh(*cfg.Load())
		p.history.sync(*cfg.Load())
		n := p.View(Query{}).Nodes[1]
		if !n.Reachable || n.Status != types.NodeStatusHealthy || n.AgeSeconds != 0 {
			t.Fatalf("cycle %d with history stalled: reachable %v status %q age %.1fs; the card must stay current",
				i, n.Reachable, n.Status, n.AgeSeconds)
		}
	}
	if got := peer.summaries.Load(); got != 6 {
		t.Errorf("%d summaries fetched across 6 cycles; the stalled history fetch held some back", got)
	}
	if readings.Load() != 0 {
		t.Errorf("%d readings recorded from a history fetch that never answered", readings.Load())
	}

	// Stopping cancels the stuck fetch itself, rather than waiting it out.
	p.history.stop()
	<-cancelled
}

// TestRecorder_RecordsTheSlimmedPayloadAtItsCadence: a worker takes a reading
// at once and then one per cadence, each the history payload. The worker's
// timer and the poller's clock are both driven by hand: every fetch takes
// exactly 300ms by that clock, so the next turn must be armed for the cadence
// less those 300ms -- timed from the start of the turn, not its end.
func TestRecorder_RecordsTheSlimmedPayloadAtItsCadence(t *testing.T) {
	const fetchTakes = 300 * time.Millisecond
	clk := newManualClock()
	var got atomic.Pointer[types.Snapshot]
	var fetched atomic.Int64
	peer := &historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		clk.Advance(fetchTakes)
		servesHistory(w, r)
	}}
	srv := httptest.NewServer(peer)
	defer srv.Close()

	cfg := fleetOf(5*time.Second, srv.URL)
	p := New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthyPeerSnapshot(), 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.now = clk.Now
	timers, made := newManualTimers()
	p.history.newTimer = timers
	p.Observe(func(_ string, s *types.Snapshot) { got.Store(s) }, func() time.Duration { return MinPollInterval })
	defer p.history.stop()

	p.refresh(*cfg.Load())
	p.history.sync(*cfg.Load())
	worker := await.Recv(t, "the peer's worker to start its timer", made)

	for turn := int64(1); turn <= 2; turn++ {
		await.Send(t, fmt.Sprintf("turn %d", turn), worker.c, clk.Now())
		if next := await.Recv(t, fmt.Sprintf("turn %d to arm the next", turn), worker.resets); next != MinPollInterval-fetchTakes {
			t.Errorf("turn %d armed the next for %v, want the %v cadence less the %v fetch",
				turn, next, MinPollInterval, fetchTakes)
		}
		if n := fetched.Load(); n != turn {
			t.Fatalf("%d history fetches after %d turns, want one per turn", n, turn)
		}
	}

	s := got.Load()
	if s == nil || s.Host.Hostname != "peer" || len(s.Processes.Processes) != 0 || s.Processes.Total != 1 {
		t.Errorf("recorded reading = %+v; want the peer's slimmed history payload", s)
	}
	if peer.snapshots.Load() != 0 {
		t.Errorf("the whole snapshot was fetched %d times; history uses the payload", peer.snapshots.Load())
	}
}

// TestClockTimer_IsATimeTimer covers the production timer: it expires on its
// own, Reset rearms it, and Stop disarms it.
func TestClockTimer_IsATimeTimer(t *testing.T) {
	tm := newClockTimer(0)
	await.Recv(t, "an immediate timer to expire", tm.Chan())

	tm.Reset(time.Hour)
	if !tm.Stop() {
		t.Error("Stop() = false on a timer armed for an hour; Reset did not arm it")
	}
	if tm.Stop() {
		t.Error("Stop() = true on a timer already stopped")
	}
	if tm.Reset(0) {
		t.Error("Reset() = true on a stopped timer")
	}
	await.Recv(t, "the rearmed timer to expire", tm.Chan())
}

// TestRecorder_BoundsConcurrentFetches: however many peers there are, no more
// than MaxHistoryFetches history payloads are in flight at once.
func TestRecorder_BoundsConcurrentFetches(t *testing.T) {
	var inflight, peak, done atomic.Int64
	release := make(chan struct{})
	history := func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		done.Add(1)
		servesHistory(w, r)
	}
	var urls []string
	for range 10 {
		srv := httptest.NewServer(&historyPeer{history: history})
		defer srv.Close()
		urls = append(urls, srv.URL)
	}
	cfg := fleetOf(time.Minute, urls...)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))

	p.refresh(*cfg.Load())
	p.history.sync(*cfg.Load())
	await.Until(t, "the concurrency limit to fill", func(context.Context) bool { return inflight.Load() == MaxHistoryFetches })
	close(release)
	await.Until(t, "every peer's reading", func(context.Context) bool { return readings.Load() == 10 })

	if got := peak.Load(); got != MaxHistoryFetches {
		t.Errorf("peak concurrent history fetches = %d, want %d", got, MaxHistoryFetches)
	}
}

// TestRecorder_WorkersFollowTheConfiguration: a worker per configured peer
// while history is recorded; a removed peer's worker is cancelled with its
// request; a repointed peer gets a new worker; recording off stops them all;
// and when the recorder stops, every goroutine it started is gone.
func TestRecorder_WorkersFollowTheConfiguration(t *testing.T) {
	baseline := runtime.NumGoroutine()

	entered := make(chan struct{})
	cancelled := make(chan struct{})
	stalled := httptest.NewServer(&historyPeer{history: func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}})
	a := httptest.NewServer(&historyPeer{history: servesHistory})
	b := httptest.NewServer(&historyPeer{history: servesHistory})

	cfg := fleetOf(time.Minute, stalled.URL, a.URL)
	cadence := cadenceOf(time.Hour)
	p, _ := recordingPoller(t, cfg, cadence)
	step := func() {
		p.refresh(*cfg.Load())
		p.history.sync(*cfg.Load())
	}

	step()
	if n := len(p.history.workers); n != 2 {
		t.Fatalf("%d workers for 2 peers", n)
	}

	// peer-00 is removed while its fetch is stuck: the fetch is cancelled.
	<-entered
	cfg.Store(&config.Fleet{TimeoutMs: 60000, Peers: []config.Peer{{Name: "peer-01", URL: a.URL}}})
	step()
	<-cancelled
	if _, running := p.history.workers["peer-00"]; running || len(p.history.workers) != 1 {
		t.Errorf("workers after removing peer-00: %v", p.history.workers)
	}

	// peer-01 is repointed: a new worker for the new URL.
	cfg.Store(&config.Fleet{TimeoutMs: 60000, Peers: []config.Peer{{Name: "peer-01", URL: b.URL}}})
	step()
	if w := p.history.workers["peer-01"]; w == nil || w.url != b.URL {
		t.Errorf("peer-01's worker after repointing = %+v; want one for %s", w, b.URL)
	}

	// Recording switched off: no workers.
	cadence.Store(0)
	step()
	if n := len(p.history.workers); n != 0 {
		t.Errorf("%d workers with history not recorded", n)
	}

	p.history.stop()
	stalled.Close()
	a.Close()
	b.Close()
	p.client.CloseIdleConnections()
	await.Until(t, fmt.Sprintf("goroutines to return to %d", baseline), func(context.Context) bool {
		return runtime.NumGoroutine() <= baseline
	})
}

// TestRecorder_AnOldPeerIsRecordedByItsPollAlone: a peer too old to serve a
// summary is read by its whole snapshot, which the poll records. Its worker
// asks it for nothing more.
func TestRecorder_AnOldPeerIsRecordedByItsPollAlone(t *testing.T) {
	var snapshots, other atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != endpointSnapshot {
			other.Add(1)
			http.NotFound(w, r)
			return
		}
		snapshots.Add(1)
		if err := json.NewEncoder(w).Encode(healthyPeerSnapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))

	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])

	if snapshots.Load() != 1 || readings.Load() != 1 {
		t.Errorf("whole snapshot fetched %d times, %d readings; want the poll's one fetch, recorded once",
			snapshots.Load(), readings.Load())
	}
	if other.Load() != 1 {
		t.Errorf("%d requests beyond the summary probe; the worker must not ask an old peer for history", other.Load()-1)
	}
}

// TestRecorder_NothingRecordedWithoutAnObserver: with no observer, or one
// whose cadence is zero, no worker starts and no history is fetched.
func TestRecorder_NothingRecordedWithoutAnObserver(t *testing.T) {
	var fetched atomic.Int64
	srv := httptest.NewServer(&historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		servesHistory(w, r)
	}})
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p := New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer p.history.stop()

	p.refresh(*cfg.Load())
	p.history.sync(*cfg.Load())
	p.Observe(func(string, *types.Snapshot) {}, func() time.Duration { return 0 })
	p.history.sync(*cfg.Load())
	p.Observe(nil, nil)
	p.history.sync(*cfg.Load())

	if n := len(p.history.workers); n != 0 || fetched.Load() != 0 {
		t.Errorf("%d workers, %d history fetches without recording; want none", n, fetched.Load())
	}
}

// TestRun_StopsItsHistoryWorkers: Run returning leaves no worker behind.
func TestRun_StopsItsHistoryWorkers(t *testing.T) {
	srv := httptest.NewServer(&historyPeer{history: servesHistory})
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(time.Hour))

	done := make(chan struct{})
	close(done)
	p.Run(done)
	if n := len(p.history.workers); n != 0 {
		t.Errorf("%d history workers left after Run returned", n)
	}
}

// TestRecorder_ARetimedCadenceRestartsTheWorkers: a reload that shortens the
// sample interval must not leave workers waiting out timers armed for the old
// one.
func TestRecorder_ARetimedCadenceRestartsTheWorkers(t *testing.T) {
	var fetched atomic.Int64
	srv := httptest.NewServer(&historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		servesHistory(w, r)
	}})
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	cadence := cadenceOf(time.Hour)
	p, _ := recordingPoller(t, cfg, cadence)

	p.refresh(*cfg.Load())
	p.history.sync(*cfg.Load())
	await.Until(t, "the first reading", func(context.Context) bool { return fetched.Load() == 1 })
	before := p.history.workers["peer-00"]

	cadence.Store(int64(MinPollInterval))
	p.history.sync(*cfg.Load())
	if after := p.history.workers["peer-00"]; after == before {
		t.Fatal("the worker was kept across a cadence change; its timer is still armed for an hour")
	}
	await.Until(t, "a reading at the new cadence", func(context.Context) bool { return fetched.Load() >= 2 })
}

// TestRecorder_ATickWithRecordingOffFetchesNothing: between remote write being
// turned off and the next sync cancelling the worker, a turn fetches nothing
// and waits at the floor.
func TestRecorder_ATickWithRecordingOffFetchesNothing(t *testing.T) {
	var fetched atomic.Int64
	srv := httptest.NewServer(&historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		servesHistory(w, r)
	}})
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	cadence := cadenceOf(time.Hour)
	p, readings := recordingPoller(t, cfg, cadence)
	p.refresh(*cfg.Load())

	peer := cfg.Load().Peers[0]
	if next := p.history.tick(context.Background(), peer); next != time.Hour || readings.Load() != 1 {
		t.Fatalf("a recording tick = next %v, %d readings; want an hour and one reading", next, readings.Load())
	}
	cadence.Store(0)
	if next := p.history.tick(context.Background(), peer); next != MinPollInterval {
		t.Errorf("next turn with recording off = %v, want the floor %v", next, MinPollInterval)
	}
	if fetched.Load() != 1 || readings.Load() != 1 {
		t.Errorf("recording off: %d fetches, %d readings; want nothing further", fetched.Load(), readings.Load())
	}
}

// TestRecorder_AReadingFromARepointedPeerIsDropped: a reload that repoints the
// name while a fetch is in flight makes that reading another machine's, and it
// is not recorded under the name.
func TestRecorder_AReadingFromARepointedPeerIsDropped(t *testing.T) {
	other := httptest.NewServer(&historyPeer{history: servesHistory})
	defer other.Close()
	cfg := new(atomic.Pointer[config.Fleet])
	srv := httptest.NewServer(&historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		// The reload lands while this request is being answered.
		cfg.Store(&config.Fleet{TimeoutMs: 5000, Peers: []config.Peer{{Name: "peer-00", URL: other.URL}}})
		servesHistory(w, r)
	}})
	defer srv.Close()
	cfg.Store(fleetOf(5*time.Second, srv.URL).Load())
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	p.refresh(*cfg.Load())

	peer := cfg.Load().Peers[0]
	p.history.record(context.Background(), peer)
	if readings.Load() != 0 {
		t.Errorf("%d readings recorded from %s after the name moved to %s", readings.Load(), srv.URL, other.URL)
	}
	// And a worker for the old address asks for nothing more.
	p.history.record(context.Background(), peer)
	if readings.Load() != 0 {
		t.Errorf("a stale worker recorded %d readings", readings.Load())
	}
}

// TestRecorder_AHistoryPathAnsweredWithAPageFallsBack: a proxy that does not
// forward the history path but answers it with a 200 page of its own is the
// same as a 404: the whole snapshot is fetched instead, and history stays
// complete.
func TestRecorder_AHistoryPathAnsweredWithAPageFallsBack(t *testing.T) {
	peer := &historyPeer{history: func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("<html>proxy</html>")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}}
	srv := httptest.NewServer(peer)
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	p.refresh(*cfg.Load())

	p.history.record(context.Background(), cfg.Load().Peers[0])
	if readings.Load() != 1 || peer.snapshots.Load() != 1 {
		t.Errorf("%d readings, %d snapshot fetches; want the whole snapshot recorded once", readings.Load(), peer.snapshots.Load())
	}
}

// TestRecorder_AnUnpolledPeerIsRetriedAtTheFloor: a worker that starts before
// its peer's first summary does not wait a whole cadence for its first
// reading.
func TestRecorder_AnUnpolledPeerIsRetriedAtTheFloor(t *testing.T) {
	var fetched atomic.Int64
	srv := httptest.NewServer(&historyPeer{history: func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		servesHistory(w, r)
	}})
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	peer := cfg.Load().Peers[0]

	if next := p.history.tick(context.Background(), peer); next != MinPollInterval || fetched.Load() != 0 {
		t.Fatalf("before any poll: next %v, %d fetches; want the floor and no fetch", next, fetched.Load())
	}
	p.refresh(*cfg.Load())
	if next := p.history.tick(context.Background(), peer); next != time.Hour || readings.Load() != 1 {
		t.Errorf("after the first poll: next %v, %d readings; want a reading and the cadence", next, readings.Load())
	}
}

// TestRecorder_ARepointedPeerIsRetriedAtTheFloor: a worker for a peer's new
// address does not take the old address's classification for its own.
func TestRecorder_ARepointedPeerIsRetriedAtTheFloor(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != endpointSnapshot {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(healthyPeerSnapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer legacy.Close()
	modern := httptest.NewServer(&historyPeer{history: servesHistory})
	defer modern.Close()

	cfg := fleetOf(5*time.Second, legacy.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(time.Hour))
	p.refresh(*cfg.Load())

	cfg.Store(&config.Fleet{TimeoutMs: 5000, Peers: []config.Peer{{Name: "peer-00", URL: modern.URL}}})
	if next := p.history.tick(context.Background(), cfg.Load().Peers[0]); next != MinPollInterval {
		t.Errorf("first turn at the new address = %v; want the floor, not the old address's cadence", next)
	}
}

// TestRefresh_DropsADrillInFromAReplacedAddress: a drill-in snapshot fetched
// from a URL a reload has replaced is dropped at the next refresh, not held
// until that node happens to be opened again.
func TestRefresh_DropsADrillInFromAReplacedAddress(t *testing.T) {
	a := httptest.NewServer(&historyPeer{history: servesHistory})
	defer a.Close()
	b := httptest.NewServer(&historyPeer{history: servesHistory})
	defer b.Close()
	cfg := fleetOf(5*time.Second, a.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(0))

	if _, err := p.PeerSnapshot(context.Background(), "peer-00", time.Minute); err != nil {
		t.Fatalf("PeerSnapshot: %v", err)
	}
	cfg.Store(&config.Fleet{TimeoutMs: 5000, Peers: []config.Peer{{Name: "peer-00", URL: b.URL}}})
	p.refresh(*cfg.Load())

	v, ok := p.details.Load("peer-00")
	if !ok {
		t.Fatal("the peer's detail entry vanished; it is still configured")
	}
	d := v.(*detailState)
	if !d.tryAcquire() {
		t.Fatal("the detail is still locked")
	}
	defer d.release()
	if d.snap != nil || d.url != b.URL {
		t.Errorf("after repointing, the detail holds a snapshot from %s; want it dropped", d.url)
	}
}

// TestRefresh_KeepsADrillInMadeAfterAReloadDuringThePoll: a refresh reconciles
// drill-ins against the configuration as it is now, so one fetched from a
// peer's new address after a reload landed mid-poll survives.
func TestRefresh_KeepsADrillInMadeAfterAReloadDuringThePoll(t *testing.T) {
	a := httptest.NewServer(&historyPeer{history: servesHistory})
	defer a.Close()
	b := httptest.NewServer(&historyPeer{history: servesHistory})
	defer b.Close()
	cfg := fleetOf(5*time.Second, a.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(0))
	polled := *cfg.Load()

	cfg.Store(&config.Fleet{TimeoutMs: 5000, PollIntervalMs: 60000, Peers: []config.Peer{{Name: "peer-00", URL: b.URL}}})
	if _, err := p.PeerSnapshot(context.Background(), "peer-00", time.Minute); err != nil {
		t.Fatalf("PeerSnapshot: %v", err)
	}
	p.refresh(polled) // the cycle that started before the reload

	v, _ := p.details.Load("peer-00")
	d := v.(*detailState)
	if !d.tryAcquire() {
		t.Fatal("the detail is still locked")
	}
	defer d.release()
	if d.snap == nil || d.url != b.URL {
		t.Errorf("detail after a mid-poll reload = url %s, snapshot %v; want the new address's snapshot kept", d.url, d.snap != nil)
	}
}

// TestPollFailedSince_OnlyAtTheSameAddress: a poll failure at an address a
// reload has replaced does not end reuse of a drill-in at the new one.
func TestPollFailedSince_OnlyAtTheSameAddress(t *testing.T) {
	p := New(func() config.Fleet { return config.Fleet{} },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := time.Now()
	p.peers["db"] = &peerState{url: "http://old:8081", err: "refused", polledAt: before.Add(time.Second)}

	if !p.pollFailedSince(config.Peer{Name: "db", URL: "http://old:8081"}, before) {
		t.Error("a later failure at the same address did not end reuse")
	}
	if p.pollFailedSince(config.Peer{Name: "db", URL: "http://new:8081"}, before) {
		t.Error("a failure at the replaced address ended reuse at the new one")
	}
}

// TestDetailReuse_IsTheReportedPollInterval: a drill-in is reused for the
// cadence the dashboard is told a selected peer refreshes at -- the resolved
// interval plus the time the last refresh took -- so a slow fleet is not asked
// for whole snapshots faster than it reports refreshing.
func TestDetailReuse_IsTheReportedPollInterval(t *testing.T) {
	cfg := config.Fleet{PollIntervalMs: 3000}
	p := New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.lastRefreshNs.Store(int64(2 * time.Second))
	if got, want := p.detailReuse(cfg), 5*time.Second; got != want || got != p.PollInterval() {
		t.Errorf("detailReuse = %v, PollInterval = %v; want both %v", got, p.PollInterval(), want)
	}
}
