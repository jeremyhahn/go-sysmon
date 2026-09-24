package fleet

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// What the dashboard is told about the peer refresh cadence.
//
// Run does the work and then sleeps for the interval, so peers are refreshed
// every interval+work. Reporting the configured value instead restates the
// config file rather than describing the service, and on mon0 that was a 6.7%
// lie: 3.20s measured against 3.00s configured (#76).

// slowPeer serves a snapshot that takes delay to produce, by the poller's
// clock: answering advances clk by delay. Nothing else moves clk, so a refresh
// that polls only this peer takes exactly delay -- stated, not waited out.
func slowPeer(t *testing.T, clk *manualClock, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		clk.Advance(delay)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&types.Snapshot{Host: types.HostInfo{Hostname: "peer"}}); err != nil {
			t.Errorf("encoding the peer snapshot: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cadencePoller(t *testing.T, cfg config.Fleet) *Poller {
	t.Helper()
	return New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// TestPollInterval_BeforeAnyRefresh is the honest starting state: nothing has
// been observed, so the configured value is all there is to report. This is
// also what keeps every existing caller that never refreshes unchanged.
func TestPollInterval_BeforeAnyRefresh(t *testing.T) {
	p := cadencePoller(t, config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000})

	if got := p.PollInterval(); got != 3*time.Second {
		t.Errorf("PollInterval() = %v before any refresh, want the configured 3s", got)
	}
	if got := p.LastRefresh(); got != 0 {
		t.Errorf("LastRefresh() = %v before any refresh, want 0", got)
	}
}

// TestPollInterval_IncludesTheRefreshItJustDid is the defect itself. A peer
// that takes 150ms to answer makes the real cadence 3.15s, and that is the
// number the dashboard must be given.
func TestPollInterval_IncludesTheRefreshItJustDid(t *testing.T) {
	const delay = 150 * time.Millisecond
	clk := newManualClock()
	srv := slowPeer(t, clk, delay)

	cfg := config.Fleet{
		PollIntervalMs: 3000,
		TimeoutMs:      3000,
		Peers:          []config.Peer{{Name: "peer", URL: srv.URL}},
	}
	p := cadencePoller(t, cfg)
	p.now = clk.Now
	p.refresh(cfg)

	last := p.LastRefresh()
	if last != delay {
		t.Fatalf("LastRefresh() = %v, want the %v the refresh took", last, delay)
	}

	got := p.PollInterval()
	if got <= 3*time.Second {
		t.Errorf("PollInterval() = %v, want more than the configured 3s: the loop sleeps 3s AFTER a %v refresh", got, last)
	}
	if want := 3*time.Second + last; got != want {
		t.Errorf("PollInterval() = %v, want configured+last = %v", got, want)
	}
}

// TestPollInterval_TracksTheLatestCycle proves it is the current cadence and
// not a high-water mark: when the work gets cheaper the reported cadence must
// come back down, or it would be just as untrue in the other direction.
func TestPollInterval_TracksTheLatestCycle(t *testing.T) {
	clk := newManualClock()
	slow := slowPeer(t, clk, 200*time.Millisecond)
	fast := slowPeer(t, clk, 0)

	slowCfg := config.Fleet{PollIntervalMs: 1000, TimeoutMs: 3000,
		Peers: []config.Peer{{Name: "slow", URL: slow.URL}}}
	fastCfg := config.Fleet{PollIntervalMs: 1000, TimeoutMs: 3000,
		Peers: []config.Peer{{Name: "fast", URL: fast.URL}}}

	current := slowCfg
	p := New(
		func() config.Fleet { return current },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	p.now = clk.Now

	p.refresh(slowCfg)
	afterSlow := p.PollInterval()

	current = fastCfg
	p.refresh(fastCfg)
	afterFast := p.PollInterval()

	if want := time.Second + 200*time.Millisecond; afterSlow != want {
		t.Errorf("PollInterval() = %v after the slow cycle, want %v", afterSlow, want)
	}
	if afterFast != time.Second {
		t.Errorf("PollInterval() = %v after a fast cycle and %v after a slow one; it must follow the latest cycle, not hold the worst",
			afterFast, afterSlow)
	}
}

// TestPollInterval_StillHonoursTheOverrideAndFloor guards the behaviour that
// was already there. The reported cadence is built ON the resolved interval, so
// the override and the floor must still be what it starts from.
func TestPollInterval_StillHonoursTheOverrideAndFloor(t *testing.T) {
	p := cadencePoller(t, config.Fleet{PollIntervalMs: 5000, TimeoutMs: 3000})

	p.SetPollInterval(2 * time.Second)
	if got := p.PollInterval(); got != 2*time.Second {
		t.Errorf("PollInterval() = %v with a 2s override, want 2s", got)
	}

	// Below the floor the floor wins, exactly as before.
	p.SetPollInterval(10 * time.Millisecond)
	if got := p.PollInterval(); got != MinPollInterval {
		t.Errorf("PollInterval() = %v for an override under the floor, want %v", got, MinPollInterval)
	}

	// Cleared, it returns to the configuration.
	p.SetPollInterval(0)
	if got := p.PollInterval(); got != 5*time.Second {
		t.Errorf("PollInterval() = %v after clearing the override, want the configured 5s", got)
	}
}

// TestRefresh_RecordsDurationEvenWhenEveryPeerFails is the bad-input half. An
// unreachable peer still costs the timeout, and that time is still part of the
// cadence -- a failing fleet stretches the cycle rather than shortening it, and
// reporting the configured value then would be at its most misleading.
func TestRefresh_RecordsDurationEvenWhenEveryPeerFails(t *testing.T) {
	// A closed listener: connection refused immediately, no waiting on a timeout.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	cfg := config.Fleet{
		PollIntervalMs: 3000,
		TimeoutMs:      1000,
		Peers:          []config.Peer{{Name: "dead", URL: url}},
	}
	p := cadencePoller(t, cfg)
	p.client = outbound.NewClient(0)

	p.refresh(cfg)

	if got := p.LastRefresh(); got <= 0 {
		t.Errorf("LastRefresh() = %v after a refresh where every peer failed, want a positive duration", got)
	}
	if got := p.PollInterval(); got < 3*time.Second {
		t.Errorf("PollInterval() = %v, want at least the configured 3s", got)
	}
}
