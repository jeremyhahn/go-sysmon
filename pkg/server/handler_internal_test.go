package server

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- test doubles -------------------------------------------------------

// idleSnapshotter never produces data, so a monitor built on it leaves the
// event stream idle and the keepalive ticker is the only thing that fires.
type idleSnapshotter struct{}

func (idleSnapshotter) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Host: types.HostInfo{Hostname: "idle"}}, nil
}

// countingFailWriter succeeds for the first okWrites calls and fails after
// that, which lets a test choose exactly which write in a handler breaks.
type countingFailWriter struct {
	mu       sync.Mutex
	header   http.Header
	okWrites int
	writes   int
	err      error
	body     strings.Builder
}

func (w *countingFailWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *countingFailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes > w.okWrites {
		return 0, w.err
	}
	return w.body.Write(p)
}

func (w *countingFailWriter) WriteHeader(int) {}
func (w *countingFailWriter) Flush()          {}

// deadlineFailWriter reports a genuine failure from SetWriteDeadline, as
// opposed to the "not supported" answer that must be tolerated.
type deadlineFailWriter struct {
	header http.Header
	err    error
}

func (w *deadlineFailWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineFailWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *deadlineFailWriter) WriteHeader(int)             {}
func (w *deadlineFailWriter) Flush()                      {}

func (w *deadlineFailWriter) SetWriteDeadline(time.Time) error { return w.err }

// startedMonitor returns a running monitor over s with the given interval.
func startedMonitor(t *testing.T, s monitor.Snapshotter, interval time.Duration) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(s, interval)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck
	return m
}

// ---- keepalive ----------------------------------------------------------

// TestHandleEvents_KeepaliveComment covers the idle path. Without a periodic
// frame an intermediary is free to reap a connection that is merely quiet, and
// a dead peer goes unnoticed until the next snapshot.
func TestHandleEvents_KeepaliveComment(t *testing.T) {
	original := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = original })

	// A poll interval far longer than the test keeps snapshots out of the way,
	// so anything that arrives must be a keepalive.
	m := startedMonitor(t, idleSnapshotter{}, time.Hour)

	srv := New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if _, err := conn.Write([]byte(
		"GET /api/events HTTP/1.1\r\nHost: x\r\nAccept-Encoding: identity\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("no keepalive comment arrived: %v", err)
		}
		if strings.HasPrefix(line, ": keepalive") {
			return
		}
	}
}

// TestHandleEvents_KeepaliveWriteFailureEndsHandler verifies a keepalive that
// cannot be delivered ends the stream, which is the mechanism that reclaims a
// subscription held by a peer that vanished silently.
func TestHandleEvents_KeepaliveWriteFailureEndsHandler(t *testing.T) {
	original := keepaliveInterval
	keepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = original })

	m := startedMonitor(t, idleSnapshotter{}, time.Hour)
	srv := New(m, ":0", nil)

	// The retry frame is the only write allowed; the first keepalive fails.
	w := &countingFailWriter{okWrites: 1, err: errors.New("broken pipe")}
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after a failed keepalive write")
	}
}

// ---- stream write failures ----------------------------------------------

// TestHandleEvents_RetryWriteFailure covers a peer that dies before the stream
// has said anything at all.
func TestHandleEvents_RetryWriteFailure(t *testing.T) {
	m := startedMonitor(t, idleSnapshotter{}, time.Hour)
	srv := New(m, ":0", nil)

	w := &countingFailWriter{okWrites: 0, err: errors.New("connection reset by peer")}
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after a failed retry write")
	}
}

// TestHandleEvents_InitialSnapshotWriteFailure covers the cached-snapshot send
// that runs before the subscription loop begins.
func TestHandleEvents_InitialSnapshotWriteFailure(t *testing.T) {
	m := startedMonitor(t, idleSnapshotter{}, 10*time.Millisecond)

	// Wait for the monitor to cache a snapshot so the handler has one to send.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Snapshot(); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	srv := New(m, ":0", nil)

	// Allow the retry frame through, then fail the initial snapshot.
	w := &countingFailWriter{okWrites: 1, err: errors.New("connection reset by peer")}
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after a failed initial snapshot write")
	}
}

// TestHandleEvents_ByeWriteFailure covers a peer that dies in the window
// between the monitor stopping and the farewell frame being delivered.
func TestHandleEvents_ByeWriteFailure(t *testing.T) {
	m := startedMonitor(t, idleSnapshotter{}, time.Hour)
	srv := New(m, ":0", nil)

	w := &countingFailWriter{okWrites: 1, err: errors.New("broken pipe")}
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(w, req)
	}()

	// Give the handler time to subscribe, then release it via the monitor.
	time.Sleep(100 * time.Millisecond)
	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after a failed bye write")
	}
}

// ---- writeJSON ----------------------------------------------------------

// TestWriteJSON_EncodeFailure exercises the branch where a JSON response dies
// mid-write. The handler must not panic.
func TestWriteJSON_EncodeFailure(t *testing.T) {
	m := startedMonitor(t, idleSnapshotter{}, time.Hour)
	srv := New(m, ":0", nil)

	w := &countingFailWriter{okWrites: 0, err: errors.New("connection reset by peer")}
	req := httptest.NewRequest(http.MethodGet, "/api/interval", nil)

	srv.ServeHTTP(w, req)

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// ---- deadline and flush errors ------------------------------------------

// TestEventStream_DeadlineFailurePropagates distinguishes a writer that cannot
// carry a deadline, which is fine, from one that reports a real failure, which
// must end the stream.
func TestEventStream_DeadlineFailurePropagates(t *testing.T) {
	wantErr := errors.New("socket is closed")
	w := &deadlineFailWriter{err: wantErr}
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(w, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.send("snapshot", map[string]string{"host": "x"}); !errors.Is(err, wantErr) {
		t.Errorf("send() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestEventStream_DeadlineUnsupportedIsTolerated is the counterpart: a writer
// that simply does not implement deadlines must still stream.
func TestEventStream_DeadlineUnsupportedIsTolerated(t *testing.T) {
	w := &deadlineFailWriter{err: http.ErrNotSupported}
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(w, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.send("snapshot", map[string]string{"host": "x"}); err != nil {
		t.Errorf("send() = %v, want nil when deadlines are merely unsupported", err)
	}
}

// TestEventStream_GzipFlushFailure covers the compressed write path failing,
// which reaches the stream through the gzip writer rather than directly.
func TestEventStream_GzipFlushFailure(t *testing.T) {
	wantErr := errors.New("connection reset by peer")
	// Let the header probe through, then fail everything the gzip writer emits.
	w := &countingFailWriter{okWrites: 0, err: wantErr}
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	r.Header.Set("Accept-Encoding", "gzip")

	s, err := newEventStream(w, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	// Enough payload to force the deflate writer to emit bytes downstream.
	big := strings.Repeat("payload", 4096)
	if err := s.send("snapshot", map[string]string{"host": big}); !errors.Is(err, wantErr) {
		t.Errorf("send() error = %v, want it to wrap %v", err, wantErr)
	}
}

// ---- interval error path ------------------------------------------------

// TestHandleSetInterval_AcceptsEveryAllowedValue walks the whole allow-list.
// Each value must reach the monitor, which is what makes the handler's
// defensive rejection branch unreachable from outside.
func TestHandleSetInterval_AcceptsEveryAllowedValue(t *testing.T) {
	m := startedMonitor(t, idleSnapshotter{}, time.Hour)
	srv := New(m, ":0", nil)

	for ms := range allowedIntervals {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/interval",
			strings.NewReader(`{"interval_ms":`+strconv.Itoa(ms)+`}`))
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("POST interval_ms=%d status = %d, want 200", ms, rec.Code)
		}
		if got := m.Interval(); got != time.Duration(ms)*time.Millisecond {
			t.Errorf("monitor interval = %v, want %dms", got, ms)
		}
	}
}
