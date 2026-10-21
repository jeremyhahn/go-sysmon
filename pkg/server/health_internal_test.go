package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// newHealthServer returns a server over mon with no listener; tests drive it
// through ServeHTTP.
func newHealthServer(t *testing.T, mon *monitor.Monitor) *Server {
	t.Helper()
	srv, err := NewWithConfig(Config{Monitor: mon, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	return srv
}

// probe GETs path on h and returns the status and body.
func probe(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
	}
	return rec.Code, rec.Body.String()
}

// fixedSnapshotter always yields a copy of the same snapshot.
type fixedSnapshotter struct {
	snap types.Snapshot
}

func (s fixedSnapshotter) Snapshot() (*types.Snapshot, error) {
	snap := s.snap
	return &snap, nil
}

// newFixedMonitor returns a started monitor that has published snap. The
// interval is short because the first collection waits for the first tick.
func newFixedMonitor(t *testing.T, snap *types.Snapshot) *monitor.Monitor {
	t.Helper()
	mon := startedMonitor(t, fixedSnapshotter{snap: *snap}, 5*time.Millisecond)
	awaitPublications(t, mon, 1)
	return mon
}

// agedSnapshot returns a snapshot taken age ago. A zero age leaves the
// timestamp unset, so the monitor dates it as collected.
func agedSnapshot(age time.Duration) *types.Snapshot {
	if age == 0 {
		return &types.Snapshot{}
	}
	return &types.Snapshot{Timestamp: time.Now().Add(-age)}
}

func TestReadyBound(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{500 * time.Millisecond, readyFloor},
		{time.Second, readyFloor},
		{5 * time.Second, 15 * time.Second},
		{60 * time.Second, 180 * time.Second},
	}
	for _, tt := range tests {
		if got := readyBound(tt.interval); got != tt.want {
			t.Errorf("readyBound(%v) = %v, want %v", tt.interval, got, tt.want)
		}
	}
}

func TestReadyz_FreshSnapshotIsReady(t *testing.T) {
	srv := newHealthServer(t, newFixedMonitor(t, agedSnapshot(0)))

	code, body := probe(t, srv, "/readyz")
	if code != http.StatusOK || body != "ready\n" {
		t.Errorf("/readyz = %d %q, want 200 \"ready\\n\"", code, body)
	}
}

// TestReadyz_StaleSnapshotIsNotReady is the stalled collector the probe
// exists for: the process still serves, but what it serves is old.
func TestReadyz_StaleSnapshotIsNotReady(t *testing.T) {
	srv := newHealthServer(t, newFixedMonitor(t, agedSnapshot(time.Hour)))

	code, body := probe(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "latest snapshot is") {
		t.Errorf("/readyz = %d %q, want 503 naming the stale snapshot", code, body)
	}
	if code, _ := probe(t, srv, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d on a stalled collector, want 200: liveness checks nothing but serving", code)
	}
}

// TestReadyz_NoSnapshotIsNotReady: a monitor that has never published has
// nothing to serve, which is not ready either.
func TestReadyz_NoSnapshotIsNotReady(t *testing.T) {
	mon, err := monitor.New(idleSnapshotter{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	srv := newHealthServer(t, mon)

	code, body := probe(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, (&types.MonitorNotRunningError{}).Error()) {
		t.Errorf("/readyz = %d %q, want 503 naming the stopped monitor", code, body)
	}
}

func TestReadyz_DrainingIsNotReady(t *testing.T) {
	srv := newHealthServer(t, newFixedMonitor(t, agedSnapshot(0)))
	if code, _ := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d before Stop, want 200", code)
	}

	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	code, body := probe(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable || body != "not ready: server is draining\n" {
		t.Errorf("/readyz = %d %q while draining, want 503 naming the drain", code, body)
	}
	if code, _ := probe(t, srv, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d while draining, want 200", code)
	}
}

// TestReadyz_IntervalMovesTheBound: a snapshot 30s old is a stall at a 5s
// stream interval and on schedule at 60s, so the bound must follow the
// interval set through the API rather than a fixed figure.
func TestReadyz_IntervalMovesTheBound(t *testing.T) {
	srv := newHealthServer(t, newFixedMonitor(t, agedSnapshot(30*time.Second)))

	if code, _ := postInterval(t, srv, `{"interval_ms":5000}`); code != http.StatusOK {
		t.Fatalf("POST /api/interval 5000: status %d", code)
	}
	if code, _ := probe(t, srv, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d at 5s with a 30s-old snapshot, want 503", code)
	}

	if code, _ := postInterval(t, srv, `{"interval_ms":60000}`); code != http.StatusOK {
		t.Fatalf("POST /api/interval 60000: status %d", code)
	}
	if code, body := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %q at 60s with a 30s-old snapshot, want 200", code, body)
	}
}

// TestHealth_RelativeToMountPoint: the probes are routes of the server's own
// handler, like every other, so an application embedding go-sysmon under a
// prefix finds them under that prefix.
func TestHealth_RelativeToMountPoint(t *testing.T) {
	srv := newHealthServer(t, newFixedMonitor(t, agedSnapshot(0)))
	mounted := http.StripPrefix("/sysmon", srv)

	for _, path := range []string{"/sysmon/healthz", "/sysmon/readyz"} {
		if code, _ := probe(t, mounted, path); code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, code)
		}
	}
}
