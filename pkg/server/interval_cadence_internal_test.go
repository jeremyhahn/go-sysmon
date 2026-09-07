package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	const delay = 120 * time.Millisecond

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&types.Snapshot{Host: types.HostInfo{Hostname: "peer"}})
	}))
	defer peer.Close()

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "peer", URL: peer.URL}},
		PollIntervalMs: 3000,
		TimeoutMs:      3000,
	}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() *types.Snapshot { return nil },
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
	go poller.Run(done)
	deadline := time.Now().Add(5 * time.Second)
	for poller.LastRefresh() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(done)

	if poller.LastRefresh() == 0 {
		t.Fatal("no refresh completed within 5s; the test measured nothing")
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
