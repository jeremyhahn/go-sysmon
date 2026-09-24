package server_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jeremyhahn/go-sysmon/internal/await"
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

// waitForServer returns once addr accepts a TCP connection. Start gives no
// signal when its listener is bound, so this polls the listener itself, which
// is the real condition.
func waitForServer(t *testing.T, addr string) {
	t.Helper()
	var dialer net.Dialer
	await.Until(t, "the server to accept connections on "+addr, func(ctx context.Context) bool {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close probe connection: %v", err)
		}
		return true
	})
}

// stopServer stops srv, bounded by await.Timeout, failing the test on error.
func stopServer(t *testing.T, srv *server.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// newConnTrackingServer starts an httptest server over h and returns a channel
// that receives once for every connection the server has finished with.
// net/http moves a connection to StateClosed only after the handler serving it
// has returned, so a receive proves that handler unwound -- the event a test of
// a vanished client needs, rather than a pause it hopes is long enough.
func newConnTrackingServer(t *testing.T, h http.Handler) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{}, 64)
	ts := httptest.NewUnstartedServer(h)
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, closed
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

	waitForServer(t, addr)

	// The monitor answers 503 until its first collection completes, so wait
	// for that collection rather than assuming the first request is already
	// serviceable.
	waitForSnapshot(t, m)
	resp, err := http.Get("http://" + addr + "/api/snapshot") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/snapshot status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	stopServer(t, srv)

	if err := await.Recv(t, "Start to return after Stop", errCh); err != nil {
		t.Errorf("Start() = %v, want nil after a clean Stop", err)
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
	defer func() {
		if err := l.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	}()

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

// TestStart_TLSAddressInUse covers the same bind failure on the TLS path,
// which reaches the listener through ListenAndServeTLS instead.
func TestStart_TLSAddressInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	}()

	cert, _ := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, cert)

	m := newMonitor(t, "tls-conflict-host")
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    l.Addr().String(),
		TLS:     &server.TLS{CertFile: certPath, KeyFile: keyPath},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	err = srv.Start()
	if err == nil {
		t.Fatal("Start() = nil, want an error when the TLS address is already in use")
	}

	var startErr *types.ServerStartError
	if !errors.As(err, &startErr) {
		t.Fatalf("Start() error = %T (%v), want *types.ServerStartError", err, err)
	}
}

// TestStop_BeforeStart verifies Stop on a server that never started is a no-op
// rather than a panic or a hang.
func TestStop_BeforeStart(t *testing.T) {
	m := newMonitor(t, "never-started")
	srv := server.New(m, freeAddr(t), nil)

	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop() on a non-started server = %v, want nil", err)
	}
}

// ---- stream shutdown ------------------------------------------------------

// TestEvents_ByeEventWhenMonitorStops verifies that stopping the monitor tells
// streaming clients to go away over a real listener. Without it the handler
// would sit on the subscription and Shutdown would block until its timeout.
func TestEvents_ByeEventWhenMonitorStops(t *testing.T) {
	m := newMonitor(t, "closing-host")
	addr := freeAddr(t)
	srv := server.New(m, addr, nil)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()
	waitForServer(t, addr)
	defer func() {
		stopServer(t, srv)
		if err := await.Recv(t, "Start to return after Stop", errCh); err != nil {
			t.Errorf("Start() = %v, want nil after a clean Stop", err)
		}
	}()

	c := dialEvents(t, "http://"+addr+"/api/events", false)

	// Drain the first snapshot so the handler is definitely streaming.
	c.nextNamed(t, "snapshot")

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	c.nextNamed(t, "bye")
}

// TestShutdown_CompletesWithOpenStream is the regression test for a hung
// shutdown: an event stream never ends on its own, so Shutdown can only return
// once the monitor has released its subscribers.
func TestShutdown_CompletesWithOpenStream(t *testing.T) {
	m := newMonitor(t, "shutdown-host")
	addr := freeAddr(t)
	srv := server.New(m, addr, nil)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()
	waitForServer(t, addr)

	c := dialEvents(t, "http://"+addr+"/api/events", false)
	c.nextNamed(t, "snapshot")

	// Release the streaming handler, then shut the listener down.
	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	// The context is never cancelled: a hung shutdown must show up as Stop
	// not returning, not as Stop returning a deadline error.
	done := make(chan error, 1)
	go func() { done <- srv.Stop(context.Background()) }()

	if err := await.Recv(t, "Stop to return with an event stream open", done); err != nil {
		t.Fatalf("Stop with an open stream: %v", err)
	}
	if err := await.Recv(t, "Start to return after Stop", errCh); err != nil {
		t.Errorf("Start() = %v, want nil after a clean Stop", err)
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
	waitForSnapshot(t, m)

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	w := &failingResponseWriter{}

	// Must not panic even though every write fails.
	srv.ServeHTTP(w, req)

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
}

// TestHandleEvents_UnflushableWriter covers middleware that wraps the response
// writer without forwarding Flush. Streaming is impossible, so the handler must
// answer with an error instead of silently buffering forever.
func TestHandleEvents_UnflushableWriter(t *testing.T) {
	m := newMonitor(t, "unflushable-host")
	srv := server.New(m, ":0", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := &noFlushRecorder{header: make(http.Header)}

	srv.ServeHTTP(w, req)

	if w.code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the writer cannot flush", w.code)
	}
	if !strings.Contains(w.body.String(), "streaming") {
		t.Errorf("body = %q, want it to explain the streaming failure", w.body.String())
	}
}

// noFlushRecorder records a response but deliberately omits Flush.
type noFlushRecorder struct {
	header http.Header
	body   strings.Builder
	code   int
}

func (w *noFlushRecorder) Header() http.Header         { return w.header }
func (w *noFlushRecorder) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *noFlushRecorder) WriteHeader(code int)        { w.code = code }

// TestHandleEvents_WriteToClosedConnection drives the write-failure path: the
// client vanishes while the server is still streaming, so the next write must
// fail and end the handler without taking the server down.
func TestHandleEvents_WriteToClosedConnection(t *testing.T) {
	m := newMonitor(t, "write-fail-host")
	waitForSnapshot(t, m)

	srv := server.New(m, ":0", nil)
	ts, closed := newConnTrackingServer(t, srv)

	// Speak HTTP by hand so the socket can be ripped away mid-stream.
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("GET /api/events HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// The status line arriving proves the handler is streaming.
	statusLine := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			line = "read failed: " + err.Error()
		}
		statusLine <- line
	}()
	if line := await.Recv(t, "the stream's status line", statusLine); !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("status line = %q, want a 200 stream", line)
	}

	// Drop the connection mid-stream.
	if err := conn.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}

	// The handler has to notice and unwind; the connection reaching
	// StateClosed is the proof that it returned.
	await.Recv(t, "the streaming handler to unwind after its client vanished", closed)

	// And the server survives and keeps serving.

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
		"index.html":                 &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/echarts-CJXIpdJj.js": &fstest.MapFile{Data: []byte("charts")},
		"assets/index.js":            &fstest.MapFile{Data: []byte("console.log(1)")},
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

// TestCacheControl_HashedAssetsAreImmutable verifies content-addressed chunks
// are cacheable indefinitely, which is what makes the no-cache entry point
// cheap. Since #55 that is the split-out vendor chunk only: the app bundle
// carries a fixed name and revalidates, covered by the test below.
func TestCacheControl_HashedAssetsAreImmutable(t *testing.T) {
	m := newMonitor(t, "cache-host")
	assets := fstest.MapFS{
		"index.html":                 &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/echarts-CJXIpdJj.js": &fstest.MapFile{Data: []byte("charts")},
		"assets/index.js":            &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	srv := server.New(m, ":0", assets)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/assets/echarts-CJXIpdJj.js") //nolint:noctx
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

// TestCacheControl_FixedNameBundleRevalidatesWithETag is the safety net for
// dropping the content hash from the app bundle (#55). The filename no longer
// changes when the code does, so the only thing standing between a returning
// browser and a stale bundle is the entity tag: it must be present, it must
// answer a matching conditional request with 304, and it must serve the new
// bytes when the tag does not match.
func TestCacheControl_FixedNameBundleRevalidatesWithETag(t *testing.T) {
	m := newMonitor(t, "cache-host")
	assets := fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/index.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	srv := server.New(m, ":0", assets)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/assets/index.js") //nolint:noctx
	if err != nil {
		t.Fatalf("GET bundle: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want the fixed-name bundle to revalidate", cc)
	}
	tag := resp.Header.Get("ETag")
	if tag == "" {
		t.Fatal("ETag is empty: a fixed-name bundle with no validator re-downloads on every load")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/assets/index.js", nil)
	if err != nil {
		t.Fatalf("build conditional request: %v", err)
	}
	req.Header.Set("If-None-Match", tag)
	cond, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	if err := cond.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
	if cond.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want %d for a matching entity tag", cond.StatusCode, http.StatusNotModified)
	}

	req.Header.Set("If-None-Match", `"00000000000000000000000000000000"`)
	stale, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stale conditional GET: %v", err)
	}
	if err := stale.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
	if stale.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d for a stale entity tag", stale.StatusCode, http.StatusOK)
	}
}
