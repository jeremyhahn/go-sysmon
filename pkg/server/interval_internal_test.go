package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// intervalServer builds a server with a real fleet poller so the scope routing
// is exercised against the thing it actually drives.
func scopedIntervalServer(t *testing.T, peers []config.Peer) *Server {
	t.Helper()
	poller := fleet.New(
		func() config.Fleet { return config.Fleet{Peers: peers, PollIntervalMs: 10000, TimeoutMs: 1000} },
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
	return srv
}

func postInterval(t *testing.T, srv *Server, body string) (int, intervalPayload) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/interval", bytes.NewBufferString(body))
	srv.ServeHTTP(rec, req)
	var out intervalPayload
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

// TestInterval_FleetScopeDrivesThePeerPoller is #97's contract: while the view
// is a peer, the poller feeding it is the fleet poller, so that is what the
// control has to change.
func TestInterval_FleetScopeDrivesThePeerPoller(t *testing.T) {
	srv := scopedIntervalServer(t, []config.Peer{{Name: "omega", URL: deadpeer.URL(t)}})

	localBefore := srv.mon.Interval()
	code, got := postInterval(t, srv, `{"interval_ms":5000,"scope":"fleet"}`)
	if code != http.StatusOK {
		t.Fatalf("POST fleet scope: status %d", code)
	}

	if want := 5000; got.FleetIntervalMS != want {
		t.Errorf("fleet_interval_ms = %d, want %d", got.FleetIntervalMS, want)
	}
	if effective := srv.fleet.PollInterval(); effective != 5*time.Second {
		t.Errorf("poller runs at %v, want 5s -- the response and the poller disagree", effective)
	}
	// The local monitor is a different poller and must not have moved.
	if after := srv.mon.Interval(); after != localBefore {
		t.Errorf("local interval changed from %v to %v on a fleet-scoped request", localBefore, after)
	}
}

// TestInterval_FleetScopeReportsTheFlooredValue covers the clamp. A request
// below the floor is honoured as far as it can be, and the answer says what is
// actually in force -- reporting 250ms back while running at 1s would be the
// dashboard stating a cadence nothing collects at, which is the defect #97
// exists to remove.
func TestInterval_FleetScopeReportsTheFlooredValue(t *testing.T) {
	srv := scopedIntervalServer(t, []config.Peer{{Name: "omega", URL: deadpeer.URL(t)}})

	code, got := postInterval(t, srv, `{"interval_ms":250,"scope":"fleet"}`)
	if code != http.StatusOK {
		t.Fatalf("POST fleet scope: status %d", code)
	}
	if got.FleetIntervalMS != int(fleet.MinPollInterval.Milliseconds()) {
		t.Errorf("fleet_interval_ms = %d, want the floor %d", got.FleetIntervalMS, fleet.MinPollInterval.Milliseconds())
	}
	if effective := srv.fleet.PollInterval(); effective != fleet.MinPollInterval {
		t.Errorf("poller runs at %v, want the floor %v", effective, fleet.MinPollInterval)
	}
}

// TestInterval_LocalScopeIsUnchanged is the compatibility case: every existing
// client sends no scope at all and must keep driving the local monitor.
func TestInterval_LocalScopeIsUnchanged(t *testing.T) {
	srv := scopedIntervalServer(t, []config.Peer{{Name: "omega", URL: deadpeer.URL(t)}})
	fleetBefore := srv.fleet.PollInterval()

	code, got := postInterval(t, srv, `{"interval_ms":5000}`)
	if code != http.StatusOK {
		t.Fatalf("POST with no scope: status %d", code)
	}
	if got.IntervalMS != 5000 {
		t.Errorf("interval_ms = %d, want 5000", got.IntervalMS)
	}
	if srv.mon.Interval() != 5*time.Second {
		t.Errorf("local poller runs at %v, want 5s", srv.mon.Interval())
	}
	if after := srv.fleet.PollInterval(); after != fleetBefore {
		t.Errorf("fleet cadence changed from %v to %v on a local request", fleetBefore, after)
	}
}

// TestInterval_RejectsWhatItCannotHonour covers the error paths: an unknown
// scope, and a fleet request on a node with no fleet. Both must fail loudly
// rather than silently doing nothing, because a control that reports success
// and changes nothing is worse than one that errors.
func TestInterval_RejectsWhatItCannotHonour(t *testing.T) {
	withFleet := scopedIntervalServer(t, []config.Peer{{Name: "omega", URL: deadpeer.URL(t)}})
	if code, _ := postInterval(t, withFleet, `{"interval_ms":5000,"scope":"nonsense"}`); code != http.StatusBadRequest {
		t.Errorf("unknown scope: status %d, want 400", code)
	}
	if code, _ := postInterval(t, withFleet, `{"interval_ms":333,"scope":"fleet"}`); code != http.StatusBadRequest {
		t.Errorf("interval outside the allowed set: status %d, want 400", code)
	}

	mon, err := monitor.New(idleSnapshotter{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New(): %v", err)
	}
	srv, err := NewWithConfig(Config{Monitor: mon, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if code, _ := postInterval(t, srv, `{"interval_ms":5000,"scope":"fleet"}`); code != http.StatusBadRequest {
		t.Errorf("fleet scope with no fleet: status %d, want 400", code)
	}
}

// TestInterval_GetReportsBothCadences is what lets the header state the
// effective rate rather than the requested one.
func TestInterval_GetReportsBothCadences(t *testing.T) {
	srv := scopedIntervalServer(t, []config.Peer{{Name: "omega", URL: deadpeer.URL(t)}})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/interval", nil))
	var got intervalPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	if got.IntervalMS <= 0 {
		t.Errorf("interval_ms = %d, want the monitor's cadence", got.IntervalMS)
	}
	if got.FleetIntervalMS != 10000 {
		t.Errorf("fleet_interval_ms = %d, want the configured 10000", got.FleetIntervalMS)
	}
}
