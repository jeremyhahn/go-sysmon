package server_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// freeAddr reserves an ephemeral port and returns it, so a test can bind a
// real listener without racing a hardcoded port number.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// waitForServer polls addr until it accepts a connection or the deadline passes.
func waitForServer(addr string, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// pollUntilOK requests url until it returns 200 or the deadline passes,
// returning the last status code observed.
func pollUntilOK(t *testing.T, url string, deadline time.Duration) int {
	t.Helper()
	end := time.Now().Add(deadline)
	last := 0
	for time.Now().Before(end) {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			last = resp.StatusCode
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close body: %v", err)
			}
			if last == http.StatusOK {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

// ---- Start / Stop ---------------------------------------------------------

// TestStart_ServesUntilStopped covers the happy path of Start: it binds, serves
// real requests, and returns nil (not ErrServerClosed) after Stop.
func TestStart_ServesUntilStopped(t *testing.T) {
	m := newMonitor(t, "lifecycle-host")
	addr := freeAddr(t)
	srv := server.New(m, addr, nil)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	if !waitForServer(addr, 2*time.Second) {
		t.Fatal("server did not start listening")
	}

	// The monitor answers 503 until its first collection completes, so poll
	// rather than assuming the first request is already serviceable.
	if code := pollUntilOK(t, "http://"+addr+"/api/snapshot", 5*time.Second); code != http.StatusOK {
		t.Errorf("GET /api/snapshot status = %d, want %d", code, http.StatusOK)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start() = %v, want nil after a clean Stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

// TestStart_AddressInUse is the regression test for a silent bind failure:
// Start must surface the error as a typed *types.ServerStartError rather than
// returning nil.
func TestStart_AddressInUse(t *testing.T) {
	// Hold the port for the duration of the test.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close() //nolint:errcheck

	m := newMonitor(t, "conflict-host")
	srv := server.New(m, l.Addr().String(), nil)

	err = srv.Start()
	if err == nil {
		t.Fatal("Start() = nil, want an error when the address is already in use")
	}

	var startErr *types.ServerStartError
	if !errors.As(err, &startErr) {
		t.Fatalf("Start() error = %T (%v), want *types.ServerStartError", err, err)
	}
	if !strings.Contains(err.Error(), "server start:") {
		t.Errorf("error = %q, want it to carry the 'server start:' prefix", err)
	}
}

// TestStop_BeforeStart verifies Stop on a server that never started is a no-op
// rather than a panic or a hang.
func TestStop_BeforeStart(t *testing.T) {
	m := newMonitor(t, "never-started")
	srv := server.New(m, freeAddr(t), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop() on a non-started server = %v, want nil", err)
	}
}

// ---- WebSocket shutdown ---------------------------------------------------

// TestWebSocket_ReceivesCloseFrameWhenMonitorStops verifies that stopping the
// monitor tells streaming clients to go away. http.Server.Shutdown does not
// track hijacked connections, so this close frame is the only clean disconnect
// a WebSocket client ever gets.
func TestWebSocket_ReceivesCloseFrameWhenMonitorStops(t *testing.T) {
	m := newMonitor(t, "closing-host")
	addr := freeAddr(t)
	srv := server.New(m, addr, nil)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()
	if !waitForServer(addr, 2*time.Second) {
		t.Fatal("server did not start listening")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// Drain the first snapshot so the handler is definitely streaming.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read first snapshot: %v", err)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	// The client must observe a normal closure, not an abrupt EOF.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue // a queued snapshot; keep reading until the close frame
		}
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			return // expected
		}
		t.Fatalf("read error = %v, want a normal websocket closure", err)
	}
}

// ---- error paths ----------------------------------------------------------

// failingResponseWriter fails every Write, which makes the JSON encoder in
// handleSnapshot return an error.
type failingResponseWriter struct {
	header http.Header
	code   int
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("connection reset by peer")
}

func (w *failingResponseWriter) WriteHeader(statusCode int) { w.code = statusCode }

// TestHandleSnapshot_EncodeError exercises the branch where the response
// stream dies mid-encode. The handler must not panic and must have already
// committed a 200 header.
func TestHandleSnapshot_EncodeError(t *testing.T) {
	m := newMonitor(t, "encode-error-host")
	srv := server.New(m, ":0", nil)

	// Wait for the monitor's first snapshot so the handler reaches the encoder.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Snapshot(); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	w := &failingResponseWriter{}

	// Must not panic even though every write fails.
	srv.ServeHTTP(w, req)

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
}

// TestHandleWebSocket_UpgradeFailure covers the rejection path: a plain GET to
// /ws carries no upgrade headers, so the handshake must fail with a 4xx rather
// than hijacking the connection or panicking.
func TestHandleWebSocket_UpgradeFailure(t *testing.T) {
	m := newMonitor(t, "upgrade-failure-host")
	srv := server.New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/ws") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()

	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("status = %d, want a 4xx for a non-websocket request to /ws", resp.StatusCode)
	}
}

// TestHandleWebSocket_WriteToClosedConnection drives the writeJSON failure
// path: the client vanishes without a close handshake while the server is
// still streaming, so the next write must fail and end the handler.
func TestHandleWebSocket_WriteToClosedConnection(t *testing.T) {
	m := newMonitor(t, "write-fail-host")
	srv := server.New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}

	// Rip the socket away at the TCP level so the server's next write fails.
	if err := conn.UnderlyingConn().Close(); err != nil {
		t.Fatalf("close underlying conn: %v", err)
	}

	// Give the server time to attempt a write and unwind the handler; the test
	// passes as long as the server survives and keeps serving.
	time.Sleep(200 * time.Millisecond)

	resp, err := http.Get(ts.URL + "/api/snapshot") //nolint:noctx
	if err != nil {
		t.Fatalf("server stopped serving after a broken client: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

// ---- caching --------------------------------------------------------------

// TestCacheControl_EntryPointRevalidates is the regression test for a
// redeployed server that keeps showing the previous UI. index.html names the
// content-hashed bundle, so a cached copy pins the browser to an old build
// forever.
func TestCacheControl_EntryPointRevalidates(t *testing.T) {
	m := newMonitor(t, "cache-host")
	assets := fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/index-abc123.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	srv := server.New(m, ":0", assets)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()

	got := resp.Header.Get("Cache-Control")
	if !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q, want the entry point to revalidate", got)
	}
}

// TestCacheControl_HashedAssetsAreImmutable verifies content-addressed bundles
// are cacheable indefinitely, which is what makes the no-cache entry point
// cheap.
func TestCacheControl_HashedAssetsAreImmutable(t *testing.T) {
	m := newMonitor(t, "cache-host")
	assets := fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/index-abc123.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	srv := server.New(m, ":0", assets)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/assets/index-abc123.js") //nolint:noctx
	if err != nil {
		t.Fatalf("GET asset: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := resp.Header.Get("Cache-Control")
	if !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want hashed assets to be immutable", got)
	}
}
