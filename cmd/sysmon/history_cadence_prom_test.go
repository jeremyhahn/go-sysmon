//go:build prometheus && !desktop

package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// database is a time-series database's status endpoint that a test can take
// down, bring up, and give a lookback.
type database struct {
	srv      *httptest.Server
	mu       sync.Mutex
	up       bool
	lookback string
}

func newDatabase(t *testing.T) *database {
	t.Helper()
	d := &database{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		up, lookback := d.up, d.lookback
		d.mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte(`{"status":"error","error":"starting"}`)); err != nil {
				t.Errorf("write response: %v", err)
			}
			return
		}
		if _, err := w.Write([]byte(`{"status":"success","data":{"query.lookback-delta":"` + lookback + `"}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *database) set(up bool, lookback string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.up, d.lookback = up, lookback
}

// cadenceConfig writes a configuration that pushes history to db every
// sampleMs, and returns its path.
func cadenceConfig(t *testing.T, path string, db *database, sampleMs int) {
	t.Helper()
	body := "fleet:\n  local_name: mon0\nprometheus:\n  remote_write_url: " + db.srv.URL +
		"/api/v1/write\n  query_url: " + db.srv.URL + "\n  timeout_ms: 2000\n  sample_interval_ms: " +
		strconv.Itoa(sampleMs) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// watched is a running watch on a fast schedule, with its results delivered
// to a channel as they are applied.
type watched struct {
	w       *cadenceWatch
	mgr     *config.Manager
	alerts  *alert.Evaluator
	status  *serviceStatus
	results chan cadenceResult
	cancel  context.CancelFunc
}

func startWatched(t *testing.T, path string) *watched {
	t.Helper()
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	log, _ := capturedLogger()
	// The lookback cache on the same fast schedule as the watch, so each
	// check asks the database afresh -- as it does in production, where both
	// run at the cache's own lifetimes.
	q := prom.NewQuerier(func() prom.QueryOptions { return prom.QueryOptions{Timeout: 2 * time.Second} },
		prom.WithLookbackCache(50*time.Millisecond, 10*time.Millisecond))
	x := &watched{
		mgr: mgr, alerts: alert.New(), status: &serviceStatus{},
		results: make(chan cadenceResult, 64),
	}
	x.w = newCadenceWatch(mgr, q, x.alerts, x.status, log)
	x.w.retry, x.w.recheck = 10*time.Millisecond, 50*time.Millisecond
	x.w.observe = func(r cadenceResult) { x.results <- r }
	ctx, cancel := context.WithCancel(context.Background())
	x.cancel = cancel
	if err := x.w.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		// Also the goroutine-leak check: Run has returned.
		await.Recv(t, "the watch to stop when its context ended", x.w.stopped)
	})
	return x
}

// until waits for a result in state, discarding others on the way.
func (x *watched) until(t *testing.T, state cadenceState) cadenceResult {
	t.Helper()
	for {
		if r := await.Recv(t, "a "+state.String()+" result", x.results); r.state == state {
			return r
		}
	}
}

// firing returns the history-cadence alerts the evaluator reports.
func (x *watched) firing() []types.Alert {
	var out []types.Alert
	for _, a := range x.alerts.Evaluate(config.Alerts{}, &types.Snapshot{}, time.Now()) {
		if a.Rule == alert.RuleHistoryCadence {
			out = append(out, a)
		}
	}
	return out
}

// TestCadenceWatch_StartsWithTheDatabaseDownThenFindsTheViolation is the
// ruling end to end: the node starts while its database is down, with a
// cadence that database cannot chart; the check is pending, not skipped; when
// the database comes up the violation is found, the node reports itself
// degraded and the alert fires; a reload that fixes the cadence clears both.
func TestCadenceWatch_StartsWithTheDatabaseDownThenFindsTheViolation(t *testing.T) {
	db := newDatabase(t)
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	// 280s sampling plus a 60s collection plus the 2s write timeout: 342s,
	// past a 5m lookback.
	cadenceConfig(t, path, db, 280000)

	// Down at start: accepted, and pending.
	x := startWatched(t, path)
	x.until(t, cadencePending)
	if s := x.status.Text(); !strings.HasPrefix(s, "history cadence check pending") {
		t.Errorf("status while the database is down = %q, want it pending", s)
	}
	if got := x.firing(); len(got) != 0 {
		t.Errorf("alerts while pending = %+v, want none: nothing has been measured", got)
	}

	// Up, with a lookback the cadence exceeds: degraded, and the alert.
	db.set(true, "5m")
	x.until(t, cadenceViolated)
	if s := x.status.Text(); !strings.HasPrefix(s, "degraded: ") || !strings.Contains(s, "300000ms") {
		t.Errorf("status = %q, want degraded naming the bound", s)
	}
	fired := x.firing()
	if len(fired) != 1 {
		t.Fatalf("alerts = %+v, want the history-cadence alert", fired)
	}
	a := fired[0]
	if a.Category != alert.CategoryHost || a.Severity != types.SeverityWarning ||
		!strings.Contains(a.Message, "prometheus.sample_interval_ms") || !strings.Contains(a.Message, "5m0s") ||
		a.Value != 342 || a.Threshold != 300 {
		t.Errorf("alert = %+v, want it to name the settings, the 342s cadence and the 5m bound", a)
	}

	// A reload that fixes the cadence clears both, at once.
	cadenceConfig(t, path, db, 15000)
	if _, err := x.mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	x.until(t, cadenceOK)
	if s := x.status.Text(); s != "" {
		t.Errorf("status after the fix = %q, want it cleared", s)
	}
	if got := x.firing(); len(got) != 0 {
		t.Errorf("alerts after the fix = %+v, want none", got)
	}
}

// TestCadenceWatch_AReachableViolationIsFoundAtOnce. The database answering
// at startup does not make startup wait on it: the configuration is accepted
// and the watch, running at once, finds the violation in its first check.
// Once the lookback is known, a reload that would reintroduce the violation is
// refused before it goes live.
func TestCadenceWatch_AReachableViolationIsFoundAtOnce(t *testing.T) {
	db := newDatabase(t)
	db.set(true, "2m")
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	cadenceConfig(t, path, db, 90000) // 150s: past a 2m lookback

	x := startWatched(t, path)
	x.until(t, cadenceViolated)
	if len(x.firing()) != 1 {
		t.Fatal("the violation found at startup did not fire")
	}

	// Fix it by reload; then try to reload the violation back in.
	cadenceConfig(t, path, db, 15000) // 75s
	if _, err := x.mgr.Reload(); err != nil {
		t.Fatalf("Reload to a passing cadence: %v", err)
	}
	x.until(t, cadenceOK)
	cadenceConfig(t, path, db, 90000)
	var refused *config.HistoryCadenceError
	if _, err := x.mgr.Reload(); !errors.As(err, &refused) {
		t.Fatalf("Reload back to the violation = %v, want *HistoryCadenceError", err)
	}
}

// TestCadenceWatch_KeepsCheckingOnceSettled. A passed check is repeated on
// the re-check schedule -- the lookback cache's lifetime in production -- so a
// database restarted with a shorter lookback is noticed without anything
// changing on this node.
func TestCadenceWatch_KeepsCheckingOnceSettled(t *testing.T) {
	db := newDatabase(t)
	db.set(true, "5m")
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	cadenceConfig(t, path, db, 15000)
	x := startWatched(t, path)
	for range 3 {
		x.until(t, cadenceOK)
	}
}

// TestCadenceWatch_AnUnreachableDatabaseDoesNotClearAViolation. The database
// going away after a violation was measured fixes nothing: with the
// configuration unchanged the node stays degraded and the alert stands. A
// reload is a new configuration, whose check is genuinely pending.
func TestCadenceWatch_AnUnreachableDatabaseDoesNotClearAViolation(t *testing.T) {
	log, _ := capturedLogger()
	alerts, status := alert.New(), &serviceStatus{}
	w := &cadenceWatch{alerts: alerts, status: status, log: log}
	cfg := &config.Config{}
	violation := &config.HistoryCadenceError{Key: "prometheus.sample_interval_ms + server.interval_ms",
		IntervalMs: 340000, LookbackMs: 300000}

	w.apply(cfg, cadenceResult{state: cadenceViolated, reason: violation.Error(), violation: violation})
	w.apply(cfg, cadenceResult{state: cadencePending, reason: "the time-series database could not be reached"})
	if s := status.Text(); !strings.HasPrefix(s, "degraded: ") || !strings.Contains(s, "re-check pending") {
		t.Errorf("status = %q, want still degraded, with the re-check pending", s)
	}
	firing := alerts.Evaluate(config.Alerts{}, &types.Snapshot{}, time.Now())
	if len(firing) != 1 || firing[0].Rule != alert.RuleHistoryCadence {
		t.Errorf("alerts = %+v, want the violation still firing", firing)
	}

	reloaded := &config.Config{}
	w.apply(reloaded, cadenceResult{state: cadencePending, reason: "the time-series database could not be reached"})
	if s := status.Text(); !strings.HasPrefix(s, "history cadence check pending") {
		t.Errorf("status after a reload = %q, want the new configuration's check pending", s)
	}
	if firing := alerts.Evaluate(config.Alerts{}, &types.Snapshot{}, time.Now()); len(firing) != 0 {
		t.Errorf("alerts after a reload = %+v, want the old configuration's violation withdrawn", firing)
	}
}

// TestCadenceWatch_StopsOnShutdown. A watch waiting on a database that never
// answers stops when the server does, rather than holding shutdown for the
// request timeout -- and leaves no goroutine behind.
func TestCadenceWatch_StopsOnShutdown(t *testing.T) {
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hung.Close)
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	body := "prometheus:\n  remote_write_url: " + hung.URL + "/api/v1/write\n  timeout_ms: 3600000\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	log, _ := capturedLogger()
	q := prom.NewQuerier(func() prom.QueryOptions { return prom.QueryOptions{} })
	w := newCadenceWatch(mgr, q, alert.New(), &serviceStatus{}, log)

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx) // the validator is skipped here: it would wait the same way
	cancel()
	await.Recv(t, "the watch to stop on shutdown", w.stopped)
}

// TestCadenceWatch_KickNeverBlocks. Reloads arrive on the config manager's
// goroutine; asking for a re-check must never hold one up, however many
// arrive before the watch gets to them.
func TestCadenceWatch_KickNeverBlocks(t *testing.T) {
	w := &cadenceWatch{kick: make(chan struct{}, 1)}
	for range 100 {
		w.Kick()
	}
	if len(w.kick) != 1 {
		t.Errorf("%d kicks queued, want 1", len(w.kick))
	}
}

// gateFixture is the peer history gate over a configuration writing history
// to a database with the given lookback, already read.
func gateFixture(t *testing.T, lookback string, extra string) fleet.HistoryGate {
	t.Helper()
	db, _ := flagsServer(t, http.StatusOK, lookback)
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	body := "fleet:\n  local_name: mon0\n  timeout_ms: 5000\n" + extra + "prometheus:\n  remote_write_url: " +
		db.URL + "/api/v1/write\n  query_url: " + db.URL + "\n  sample_interval_ms: 15000\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	q := newCadenceQuerier()
	if _, err := q.LookbackAt(context.Background(), db.URL); err != nil {
		t.Fatal(err)
	}
	return peerHistoryGate(mgr, q)
}

func withInterval(d time.Duration) *types.NodeSummary {
	return &types.NodeSummary{CollectionIntervalMs: d.Milliseconds()}
}

// TestPeerHistoryGate is the per-peer rule, against a 5m lookback: a peer's
// relayed samples can be a relay tick plus its collection interval apart.
func TestPeerHistoryGate(t *testing.T) {
	gate := gateFixture(t, "5m", "")

	// 15s sampling + 2 x 5s fetch timeout + 10s write timeout + 60s
	// collection = 95s: recorded.
	if ok, h := gate("fs", withInterval(time.Minute)); !ok || h.State != types.PeerHistoryRecorded {
		t.Errorf("a peer collecting every minute: %v %+v, want recorded", ok, h)
	}

	// A peer collecting every ten minutes: 15s + 10s + 10s + 10m, past 5m.
	// Refused, and the card says why, with the figures.
	ok, h := gate("slow", withInterval(10*time.Minute))
	if ok || h.State != types.PeerHistoryRefused {
		t.Fatalf("a peer collecting every 10m: %v %+v, want refused", ok, h)
	}
	for _, want := range []string{"10m0s", "10m35s", "5m0s"} {
		if !strings.Contains(h.Reason, want) {
			t.Errorf("reason %q does not say %q", h.Reason, want)
		}
	}

	// Just past the bound: 15s + 10s + 10s + 4m26s = 5m1s.
	if ok, _ := gate("edge", withInterval(4*time.Minute+26*time.Second)); ok {
		t.Error("a gap of 5m1s against a 5m lookback was recorded")
	}
	// Exactly at it: 15s + 10s + 10s + 4m25s = 5m.
	if ok, _ := gate("edge", withInterval(4*time.Minute+25*time.Second)); !ok {
		t.Error("a gap of exactly 5m against a 5m lookback was refused")
	}
}

// TestPeerHistoryGate_AnOldPeerIsUnverified. A peer that does not report its
// collection interval -- an older build, read by its whole snapshot -- is
// written, and flagged: nothing is assumed in place of the figure it does not
// give.
func TestPeerHistoryGate_AnOldPeerIsUnverified(t *testing.T) {
	gate := gateFixture(t, "5m", "")
	for _, s := range []*types.NodeSummary{nil, withInterval(0)} {
		ok, h := gate("old", s)
		if !ok || h.State != types.PeerHistoryUnverified || !strings.Contains(h.Reason, "collection interval") {
			t.Errorf("an old peer (%+v): %v %+v, want written and unverified", s, ok, h)
		}
	}
}

// TestPeerHistoryGate_AnUnknownLookbackIsUnverified. Before the cadence
// watch has read the database's lookback, the gate has nothing to judge by:
// the reading is written, and flagged.
func TestPeerHistoryGate_AnUnknownLookbackIsUnverified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	body := "prometheus:\n  remote_write_url: http://127.0.0.1:1/api/v1/write\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	gate := peerHistoryGate(mgr, newCadenceQuerier())
	ok, h := gate("fs", withInterval(10*time.Minute))
	if !ok || h.State != types.PeerHistoryUnverified || !strings.Contains(h.Reason, "not been read yet") {
		t.Errorf("%v %+v, want written and unverified until the lookback is known", ok, h)
	}
}

// TestPeerHistoryGate_FetchQueueingCounts. History fetches run
// fleet.MaxHistoryFetches at a time, each up to the timeout, so with nine
// peers a worker can wait out two rounds before its own fetch -- each fetch
// two requests -- so its relay tick is the sample interval plus six fetch
// timeouts, and the write timeout. A peer collecting every 4m20s: 15s + 10s +
// 10s + 4m20s = 4m55s alone, recorded; 15s + 30s + 10s + 4m20s = 5m15s among
// nine, refused.
func TestPeerHistoryGate_FetchQueueingCounts(t *testing.T) {
	var peers strings.Builder
	peers.WriteString("  peers:\n")
	for i := range 9 {
		peers.WriteString("    - name: p" + strconv.Itoa(i) + "\n      url: http://p" + strconv.Itoa(i) + ":8081\n")
	}
	crowded := gateFixture(t, "5m", peers.String())
	if ok, h := crowded("p0", withInterval(4*time.Minute+20*time.Second)); ok {
		t.Errorf("with nine peers queueing: %+v, want refused at 5m15s", h)
	}
	alone := gateFixture(t, "5m", "")
	if ok, _ := alone("p0", withInterval(4*time.Minute+20*time.Second)); !ok {
		t.Error("with no queue, 4m55s against 5m was refused")
	}
}

// TestPeerHistoryGate_HoldsARefusalWhileTheDatabaseIsDown. A refusal does not
// lapse when the database's lookback cannot be refreshed: the gate judges by
// the last lookback the database gave, so a slow peer is not relayed again for
// as long as nobody can confirm the refusal.
func TestPeerHistoryGate_HoldsARefusalWhileTheDatabaseIsDown(t *testing.T) {
	db := newDatabase(t)
	db.set(true, "5m")
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	cadenceConfig(t, path, db, 15000)
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	q := prom.NewQuerier(func() prom.QueryOptions { return prom.QueryOptions{Timeout: 2 * time.Second} },
		prom.WithLookbackCache(time.Nanosecond, time.Nanosecond))
	if _, err := q.LookbackAt(context.Background(), db.srv.URL); err != nil {
		t.Fatal(err)
	}
	gate := peerHistoryGate(mgr, q)
	if ok, _ := gate("slow", withInterval(10*time.Minute)); ok {
		t.Fatal("a 10m peer was recorded against a 5m lookback")
	}

	db.set(false, "5m")
	if _, err := q.LookbackAt(context.Background(), db.srv.URL); err == nil {
		t.Fatal("the refresh of a database that is down succeeded")
	}
	if ok, h := gate("slow", withInterval(10*time.Minute)); ok {
		t.Errorf("with the refresh failing: %+v, want the refusal to stand", h)
	}
}

// TestPeerHistoryGate_ReasonsNameNoAddress. The fleet endpoint has no
// authentication of its own, and publishes no peer's address (#120). A
// history reason is shown on that endpoint, so no reason the gate can give --
// refused, unverified for any cause, recorded -- may carry a peer's or the
// database's address, host or port.
func TestPeerHistoryGate_ReasonsNameNoAddress(t *testing.T) {
	db := newDatabase(t)
	db.set(true, "5m")
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	body := "fleet:\n  local_name: mon0\n  peers:\n    - name: fs\n      url: http://10.9.8.7:18081\n" +
		"prometheus:\n  remote_write_url: " + db.srv.URL + "/api/v1/write\n  query_url: " + db.srv.URL + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	unread := peerHistoryGate(mgr, newCadenceQuerier())
	q := newCadenceQuerier()
	if _, err := q.LookbackAt(context.Background(), db.srv.URL); err != nil {
		t.Fatal(err)
	}
	read := peerHistoryGate(mgr, q)

	var reasons []string
	for _, c := range []struct {
		gate    fleet.HistoryGate
		summary *types.NodeSummary
	}{
		{read, withInterval(10 * time.Minute)}, // refused
		{read, withInterval(0)},                // no interval
		{read, nil},                            // no summary
		{unread, withInterval(time.Minute)},    // lookback unread
		{read, withInterval(time.Second)},      // recorded
	} {
		_, h := c.gate("fs", c.summary)
		reasons = append(reasons, h.Reason)
	}

	dbHost := strings.TrimPrefix(db.srv.URL, "http://")
	dbPort := dbHost[strings.LastIndex(dbHost, ":")+1:]
	for _, reason := range reasons {
		for _, leak := range []string{"10.9.8.7", "18081", "http", dbHost, "127.0.0.1", ":" + dbPort} {
			if strings.Contains(reason, leak) {
				t.Errorf("history reason %q carries %q", reason, leak)
			}
		}
	}
}

// TestPeerHistoryGate_AMalformedIntervalCannotWrap. A peer reporting an
// interval too large to convert -- math.MaxInt64 milliseconds would wrap to
// about -1ms and, plus a relay tick, look like a small gap -- is refused,
// not recorded.
func TestPeerHistoryGate_AMalformedIntervalCannotWrap(t *testing.T) {
	gate := gateFixture(t, "5m", "")
	for _, ms := range []int64{math.MaxInt64, math.MaxInt64 / 1000, math.MaxInt64 / int64(time.Millisecond), (5 * time.Minute).Milliseconds() + 1} {
		ok, h := gate("bad", &types.NodeSummary{CollectionIntervalMs: ms})
		if ok || h.State != types.PeerHistoryRefused {
			t.Errorf("interval %dms: %v %+v, want refused", ms, ok, h)
		}
	}
}

// TestCadenceState_String names each state in the log, and a value outside
// the set by its number rather than as one of the three.
func TestCadenceState_String(t *testing.T) {
	for state, want := range map[cadenceState]string{
		cadenceOK: "ok", cadencePending: "pending", cadenceViolated: "violated", cadenceState(42): "cadenceState(42)",
	} {
		if got := state.String(); got != want {
			t.Errorf("cadenceState(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}
