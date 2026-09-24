package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// pagingSnapshotter is a node that pages out 256 pages between every two
// collections, a second apart, with a busy process list and CPU flags.
type pagingSnapshotter struct {
	n atomic.Uint64
}

func (p *pagingSnapshotter) Snapshot() (*types.Snapshot, error) {
	n := p.n.Add(1)
	return &types.Snapshot{
		Timestamp: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Second),
		Host:      types.HostInfo{Hostname: "pager"},
		CPUs:      []types.CPUInfo{{UsagePercent: 50, UsageAvailable: true, Flags: []string{"fpu", "avx2"}}},
		Memory: types.MemoryInfo{
			SwapPercent: 98.6, SwapOutPages: 256 * n, PageSizeBytes: 4096, SwapCountersAvailable: true,
		},
		Processes: types.ProcessSummary{Total: 2, Processes: []types.ProcessDetail{{PID: 1}, {PID: 2}}},
	}, nil
}

// pagingMonitor runs a monitor over s until it has published two snapshots,
// so a summary has a previous reading to take its rate against.
func pagingMonitor(t *testing.T, s monitor.Snapshotter) *monitor.Monitor {
	t.Helper()
	mon := startedMonitor(t, s, 5*time.Millisecond)
	awaitPublications(t, mon, 2)
	return mon
}

func getJSON(t *testing.T, srv *Server, path string, v any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("GET %s: decoding: %v; body=%s", path, err, rec.Body.String())
		}
	}
	return rec.Code
}

// TestHandleSummary_ServesThisNodesSummaryWithItsPagingRate: GET /api/summary
// is what an aggregator polls, and its swap rate is taken over the node's own
// last collection interval (#134).
func TestHandleSummary_ServesThisNodesSummaryWithItsPagingRate(t *testing.T) {
	srv := newServer(Config{Monitor: pagingMonitor(t, &pagingSnapshotter{})}, nil)

	var got types.NodeSummary
	if code := getJSON(t, srv, "/api/summary", &got); code != http.StatusOK {
		t.Fatalf("GET /api/summary = %d, want 200", code)
	}
	if got.Hostname != "pager" || got.CPUPercent != 50 {
		t.Errorf("summary = %+v; want this node's figures", got)
	}
	if !got.SwapRateAvailable || got.SwapOutRate != 256*4096 {
		t.Errorf("swap-out = available %v, %v B/s; want 1 MiB/s over one interval", got.SwapRateAvailable, got.SwapOutRate)
	}
	// The interval the monitor actually collects at: pagingMonitor runs it at
	// 5 ms.
	if got.CollectionIntervalMs != 5 {
		t.Errorf("collection_interval_ms = %d, want the monitor's 5", got.CollectionIntervalMs)
	}
}

// TestHandleHistorySnapshot_OmitsWhatHistoryDoesNotRecord: the history payload
// is the snapshot less its process list and CPU flags.
func TestHandleHistorySnapshot_OmitsWhatHistoryDoesNotRecord(t *testing.T) {
	srv := newServer(Config{Monitor: pagingMonitor(t, &pagingSnapshotter{})}, nil)

	var got types.Snapshot
	if code := getJSON(t, srv, "/api/snapshot/history", &got); code != http.StatusOK {
		t.Fatalf("GET /api/snapshot/history = %d, want 200", code)
	}
	if got.Host.Hostname != "pager" || got.Processes.Total != 2 || got.Memory.SwapOutPages == 0 {
		t.Errorf("history payload lost what history records: %+v", got)
	}
	if len(got.Processes.Processes) != 0 || len(got.CPUs[0].Flags) != 0 {
		t.Errorf("history payload kept %d processes and %d flags; want neither",
			len(got.Processes.Processes), len(got.CPUs[0].Flags))
	}
}

// TestSummaryEndpoints_UnavailableBeforeTheFirstCollection: a node that has
// collected nothing answers 503 on both, not an empty summary that would read
// as a node with nothing wrong.
func TestSummaryEndpoints_UnavailableBeforeTheFirstCollection(t *testing.T) {
	mon, err := monitor.New(&pagingSnapshotter{}, time.Hour)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	srv := newServer(Config{Monitor: mon}, nil)
	for _, path := range []string{"/api/summary", "/api/snapshot/history"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with no reading = %d, want 503", path, rec.Code)
		}
	}
}

// TestHandleFleetSnapshot_StatusSaysWhoFailed: a node not in the fleet is 404;
// a peer that cannot be read and never has been is 502 -- the aggregator is
// answering, the node behind it is not.
func TestHandleFleetSnapshot_StatusSaysWhoFailed(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer dead.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "fs", URL: dead.URL}}, TimeoutMs: 2000}
	poller := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := newServer(Config{Fleet: poller}, nil)

	cases := map[string]int{
		"/api/fleet/nope/snapshot":  http.StatusNotFound,
		"/api/fleet/local/snapshot": http.StatusNotFound,
		"/api/fleet/fs/snapshot":    http.StatusBadGateway,
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

// TestHandleFleetSnapshot_AnswersWithinTheWriteBudget: a peer slower than the
// response's write deadline -- here with fleet.timeout_ms far above it -- is
// answered with its error in time, not waited on until no answer can be sent.
func TestHandleFleetSnapshot_AnswersWithinTheWriteBudget(t *testing.T) {
	original := responseWriteTimeout
	responseWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { responseWriteTimeout = original })

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()
	defer close(release)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "fs", URL: slow.URL}}, TimeoutMs: 60000}
	poller := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := newServer(Config{Fleet: poller}, nil)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fleet/fs/snapshot", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("GET a peer slower than the write budget = %d, want 502", rec.Code)
	}
}

// TestFleetSnapshotStatus_ARepointedPeerIsWorthRetrying: a drill-in overtaken
// by a reload is 503 -- ask again -- not a missing node or a dead one.
func TestFleetSnapshotStatus_ARepointedPeerIsWorthRetrying(t *testing.T) {
	if got := fleetSnapshotStatus(&fleet.ErrPeerMoved{Peer: "fs"}); got != http.StatusServiceUnavailable {
		t.Errorf("ErrPeerMoved -> %d, want 503", got)
	}
}
