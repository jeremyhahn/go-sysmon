package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- test double --------------------------------------------------------

type stubSnapshotter struct {
	snapshot *types.Snapshot
	err      error
}

func (s *stubSnapshotter) Snapshot() (*types.Snapshot, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.snapshot, nil
}

func newMonitor(t *testing.T, hostname string) *monitor.Monitor {
	t.Helper()
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: hostname}},
	}
	m, err := monitor.New(stub, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck
	return m
}

// waitForSnapshot blocks until the monitor has produced its first snapshot.
func waitForSnapshot(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := m.Snapshot(); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("monitor never produced a snapshot")
}

// ---- New ----------------------------------------------------------------

func TestNew_ReturnsServer(t *testing.T) {
	m := newMonitor(t, "host")
	s := server.New(m, ":0", nil)
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

// ---- GET /api/snapshot --------------------------------------------------

func TestHandleSnapshot_ReturnsJSON(t *testing.T) {
	m := newMonitor(t, "snap-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var snap types.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Host.Hostname != "snap-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "snap-host")
	}
}

func TestHandleSnapshot_MonitorNotStarted(t *testing.T) {
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "h"}},
	}
	m, _ := monitor.New(stub, time.Second)
	// Do NOT start the monitor – no snapshot available yet.

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// ---- GET / (status page) ------------------------------------------------

func TestHandleStatus_NoAssets(t *testing.T) {
	m := newMonitor(t, "host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// ---- GET /api/events – server-sent events -------------------------------

func TestHandleEvents_SendsEventStreamHeaders(t *testing.T) {
	m := newMonitor(t, "header-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	if got := c.resp.StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if ct := c.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := c.resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want it to forbid caching", cc)
	}
	// Reverse proxies buffer by default, which would defeat the whole stream.
	if got := c.resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", got, "no")
	}
}

func TestHandleEvents_ReceivesSnapshot(t *testing.T) {
	m := newMonitor(t, "sse-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	ev := c.nextNamed(t, "snapshot", 2*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)

	if snap.Host.Hostname != "sse-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "sse-host")
	}
}

// TestHandleEvents_SendsRetryHint verifies the stream tells EventSource how
// long to wait before reconnecting, instead of leaving it to the browser
// default.
func TestHandleEvents_SendsRetryHint(t *testing.T) {
	m := newMonitor(t, "retry-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	// The retry field is written before any snapshot, so it is the first frame.
	ev, err := c.next()
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if ev.Retry <= 0 {
		t.Errorf("first frame retry = %d, want a positive reconnect hint", ev.Retry)
	}
}

// TestHandleEvents_SendsCurrentSnapshotImmediately is the regression test for a
// blank dashboard after reconnect: the handler must not make the client wait a
// full poll interval for its first frame.
func TestHandleEvents_SendsCurrentSnapshotImmediately(t *testing.T) {
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "immediate-host"}},
	}
	// A very slow poll: any snapshot that arrives promptly must be the cached
	// one, not a freshly polled tick.
	m, err := monitor.New(stub, time.Hour)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	// Seed the cached snapshot by driving one fast tick, then slow back down.
	if err := m.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForSnapshot(t, m)
	if err := m.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	ev := c.nextNamed(t, "snapshot", 2*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "immediate-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "immediate-host")
	}
}

func TestHandleEvents_MultipleSnapshots(t *testing.T) {
	m := newMonitor(t, "multi-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	for i := range 3 {
		ev := c.nextNamed(t, "snapshot", 3*time.Second)
		var snap types.Snapshot
		decodeSnapshot(t, ev, &snap)
		if snap.Host.Hostname != "multi-host" {
			t.Errorf("snapshot %d hostname = %q, want %q", i, snap.Host.Hostname, "multi-host")
		}
	}
}

// TestHandleEvents_GzipEncodesStream covers the compression path. Consecutive
// snapshots are near-identical, so compressing the stream is the single
// largest bandwidth saving available here.
func TestHandleEvents_GzipEncodesStream(t *testing.T) {
	m := newMonitor(t, "gzip-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", true)

	if got := c.resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if vary := c.resp.Header.Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to include Accept-Encoding", vary)
	}

	// The payload must still decode correctly through the gzip reader.
	ev := c.nextNamed(t, "snapshot", 2*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "gzip-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "gzip-host")
	}
}

// TestHandleEvents_NoGzipWhenNotOffered verifies encoding is negotiated rather
// than assumed. A client that cannot inflate, such as a minimal embedded
// reader, must still get a usable stream.
func TestHandleEvents_NoGzipWhenNotOffered(t *testing.T) {
	m := newMonitor(t, "identity-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	if got := c.resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want it unset for an identity request", got)
	}

	ev := c.nextNamed(t, "snapshot", 2*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "identity-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "identity-host")
	}
}

func TestHandleEvents_ClientDisconnect(t *testing.T) {
	m := newMonitor(t, "disc-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)
	c.nextNamed(t, "snapshot", 2*time.Second)
	c.Close()

	// The handler must unwind without taking the server with it.
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatalf("server stopped serving after a client disconnect: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

// TestHandleEvents_MonitorStopSendsBye verifies that stopping the monitor tells
// streaming clients to stop reconnecting, rather than dropping the connection
// and letting EventSource retry forever against a dead monitor.
func TestHandleEvents_MonitorStopSendsBye(t *testing.T) {
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "stop-host"}},
	}
	m, err := monitor.New(stub, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)
	c.nextNamed(t, "snapshot", 2*time.Second)

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	ev := c.nextNamed(t, "bye", 2*time.Second)
	if ev.Data == "" {
		t.Error("bye event carried no reason")
	}
}

// ---- /api/interval ------------------------------------------------------

func TestHandleGetInterval_ReportsCurrent(t *testing.T) {
	m := newMonitor(t, "interval-host")

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/interval")
	if err != nil {
		t.Fatalf("GET /api/interval: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		IntervalMS int `json:"interval_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IntervalMS != 20 {
		t.Errorf("interval_ms = %d, want 20", got.IntervalMS)
	}
}

func TestHandleSetInterval_ChangesMonitorInterval(t *testing.T) {
	m := newMonitor(t, "set-interval-host")

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/interval", "application/json",
		strings.NewReader(`{"interval_ms":500}`))
	if err != nil {
		t.Fatalf("POST /api/interval: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		IntervalMS int `json:"interval_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IntervalMS != 500 {
		t.Errorf("response interval_ms = %d, want 500", got.IntervalMS)
	}
	if m.Interval() != 500*time.Millisecond {
		t.Errorf("monitor interval = %v, want 500ms", m.Interval())
	}
}

// TestHandleSetInterval_RejectsDisallowedValue keeps the endpoint from becoming
// a way to pin the machine at an arbitrary poll rate.
func TestHandleSetInterval_RejectsDisallowedValue(t *testing.T) {
	m := newMonitor(t, "reject-host")
	before := m.Interval()

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	for _, body := range []string{
		`{"interval_ms":999}`, // not in the allowed set
		`{"interval_ms":0}`,   // zero
		`{"interval_ms":-1}`,  // negative
		`{"interval_ms":1}`,   // far below the floor
	} {
		resp, err := http.Post(ts.URL+"/api/interval", "application/json",
			strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", body, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want 400", body, resp.StatusCode)
		}
	}

	if m.Interval() != before {
		t.Errorf("monitor interval = %v, want it unchanged at %v", m.Interval(), before)
	}
}

func TestHandleSetInterval_RejectsMalformedBody(t *testing.T) {
	m := newMonitor(t, "malformed-host")

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	for _, body := range []string{
		`not json`,
		`{"interval_ms":`,
		`{"interval_ms":"500"}`, // string where a number is required
		``,
	} {
		resp, err := http.Post(ts.URL+"/api/interval", "application/json",
			strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %q: %v", body, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %q status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestHandleSetInterval_RejectsOversizedBody verifies the request body is
// capped, so a client cannot stream an unbounded payload into the decoder.
func TestHandleSetInterval_RejectsOversizedBody(t *testing.T) {
	m := newMonitor(t, "oversized-host")

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	huge := `{"interval_ms":1000,"pad":"` + strings.Repeat("x", 4096) + `"}`
	resp, err := http.Post(ts.URL+"/api/interval", "application/json",
		strings.NewReader(huge))
	if err != nil {
		t.Fatalf("POST oversized: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHandleInterval_MethodNotAllowed confirms the mux rejects verbs that are
// not registered for the route.
func TestHandleInterval_MethodNotAllowed(t *testing.T) {
	m := newMonitor(t, "verb-host")

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/interval", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/interval: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// ---- Stop ---------------------------------------------------------------

func TestStop_GracefulShutdown(t *testing.T) {
	m := newMonitor(t, "host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Stop(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop: %v", err)
	}

	ts.Close()
}

// ---- Embedded assets ----------------------------------------------------

func TestNew_WithAssets_ServesFile(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<html>hello</html>"),
		},
	}

	m := newMonitor(t, "asset-host")
	s := server.New(m, ":0", fs.FS(assets))
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/index.html")
	if err != nil {
		t.Fatalf("GET /index.html: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// ---- retired routes -----------------------------------------------------

// TestWebSocketRouteIsGone is the regression test for the transport migration:
// /ws must no longer exist, so a stale client fails loudly instead of silently
// falling back to a route that quietly stopped streaming.
func TestWebSocketRouteIsGone(t *testing.T) {
	m := newMonitor(t, "gone-host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer resp.Body.Close()

	// With no assets configured the catch-all status handler answers, so the
	// meaningful assertion is that nothing upgrades or streams.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Error("GET /ws upgraded a connection; the WebSocket route should be gone")
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("GET /ws Content-Type = %q, want no stream on the retired route", ct)
	}
}
