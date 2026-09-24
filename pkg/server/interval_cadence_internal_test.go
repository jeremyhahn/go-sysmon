package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// GET /api/interval is where the fleet cadence reaches a browser, so it is
// where the truthfulness of that number actually matters. The poller sleeps for
// the interval AFTER refreshing, so the cadence is interval+work; on mon0 that
// was 3.20s against a configured 3.00s (#76).

// TestGetInterval_ReportsTheCadenceIncludingTheRefresh drives a real poller
// against a deliberately slow peer and reads the number back off the endpoint,
// rather than asserting on the poller directly -- the wiring between them is
// the part that could silently report the configured value.
func TestGetInterval_ReportsTheCadenceIncludingTheRefresh(t *testing.T) {
	// How long the refresh is held open at least. Anything that survives the
	// endpoint's rounding to whole milliseconds will do: the assertion is that
	// the refresh is counted, not how long it took.
	const minRefresh = 2 * time.Millisecond

	arrived := make(chan time.Time, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePeer := func() { releaseOnce.Do(func() { close(release) }) }
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		// The slow peer: held until the test lets it answer.
		arrived <- time.Now()
		<-release
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&types.Snapshot{Host: types.HostInfo{Hostname: "peer"}}); err != nil {
			t.Errorf("peer: encode snapshot: %v", err)
		}
	}))
	defer peer.Close()
	// Deferred after peer.Close so it runs first: a test that fails while the
	// peer is held must let the handler return, or Close waits for it forever.
	defer releasePeer()

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "peer", URL: peer.URL}},
		PollIntervalMs: 3000,
		TimeoutMs:      int(await.Timeout.Milliseconds()),
	}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	mon, err := monitor.New(idleSnapshotter{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New(): %v", err)
	}
	srv, err := NewWithConfig(Config{Monitor: mon, Addr: "127.0.0.1:0", Fleet: poller})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	// Before any cycle the endpoint reports the configured value, because that
	// is all that has been observed.
	if got := getFleetIntervalMS(t, srv); got != 3000 {
		t.Errorf("fleet_interval_ms = %d before any refresh, want the configured 3000", got)
	}

	// Run one cycle. Run() is used rather than calling refresh directly so the
	// path the deployed service takes is the path measured.
	done := make(chan struct{})
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		poller.Run(done)
	}()
	var doneOnce sync.Once
	stopPoller := func() { doneOnce.Do(func() { close(done) }) }
	// However the test ends, the poller is stopped and joined -- after the
	// peer is released (defers run last-in first-out), since the cycle it is
	// in cannot finish before the peer answers.
	defer func() {
		stopPoller()
		<-ran
	}()
	defer releasePeer()

	// The refresh is in flight, so nothing has been measured yet either.
	at := await.Recv(t, "the poller's request to the peer", arrived)
	if got := getFleetIntervalMS(t, srv); got != 3000 {
		t.Errorf("fleet_interval_ms = %d while the first refresh is in flight, want the configured 3000", got)
	}
	// Real time, because it is the peer's real response time the poller
	// measures; this re-reads the clock rather than guessing a delay.
	await.Until(t, "the peer to have been slow for "+minRefresh.String(), func(context.Context) bool {
		return time.Since(at) >= minRefresh
	})
	releasePeer()

	// Run finishes the cycle before it looks at done, so its return means the
	// refresh -- and its measurement -- is complete.
	stopPoller()
	await.Recv(t, "the poller to finish its cycle and return", ran)

	if poller.LastRefresh() < minRefresh {
		t.Fatalf("LastRefresh = %v, want at least the %v the peer was held", poller.LastRefresh(), minRefresh)
	}

	got := getFleetIntervalMS(t, srv)
	if got <= 3000 {
		t.Errorf("fleet_interval_ms = %d after a %v refresh, want more than the configured 3000 -- "+
			"the loop sleeps 3s AFTER the work, so peers are polled every 3s+work", got, poller.LastRefresh())
	}
	if want := 3000 + int(poller.LastRefresh().Milliseconds()); got != want {
		t.Errorf("fleet_interval_ms = %d, want configured+last refresh = %d", got, want)
	}
}

func getFleetIntervalMS(t *testing.T, srv *Server) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/interval", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/interval: status %d", rec.Code)
	}
	var out intervalPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return out.FleetIntervalMS
}

// TestGetInterval_NoFleetReportsZero is the bad-input half: a node with no
// peers runs no poller, and must report nothing rather than a cadence it is not
// keeping.
func TestGetInterval_NoFleetReportsZero(t *testing.T) {
	mon, err := monitor.New(idleSnapshotter{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New(): %v", err)
	}
	srv, err := NewWithConfig(Config{Monitor: mon, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if got := getFleetIntervalMS(t, srv); got != 0 {
		t.Errorf("fleet_interval_ms = %d with no fleet configured, want 0", got)
	}
}
