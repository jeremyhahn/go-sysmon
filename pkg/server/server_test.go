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

	"github.com/gorilla/websocket"
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
	ts := httptest.NewServer(handlerFrom(t, s))
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
	ts := httptest.NewServer(handlerFrom(t, s))
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
	ts := httptest.NewServer(handlerFrom(t, s))
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

// ---- GET /ws – WebSocket ------------------------------------------------

func TestHandleWebSocket_ReceivesSnapshot(t *testing.T) {
	m := newMonitor(t, "ws-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v (resp=%v)", err, resp)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	var snap types.Snapshot
	if err := json.Unmarshal(msg, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Host.Hostname != "ws-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "ws-host")
	}
}

func TestHandleWebSocket_MultipleMessages(t *testing.T) {
	m := newMonitor(t, "multi-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	conn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck

	for i := range 3 {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: ReadMessage: %v", i, err)
		}
		var snap types.Snapshot
		if err := json.Unmarshal(msg, &snap); err != nil {
			t.Fatalf("message %d: unmarshal: %v", i, err)
		}
	}
}

func TestHandleWebSocket_ClientDisconnect(t *testing.T) {
	m := newMonitor(t, "disc-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}

	// Read one message then close abruptly.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	conn.ReadMessage()                                    //nolint:errcheck
	conn.Close()

	// Server should not panic. Give the handler goroutine time to clean up.
	time.Sleep(50 * time.Millisecond)
}

// ---- WebSocket set_interval command -------------------------------------

func TestHandleWebSocket_SetIntervalCommand(t *testing.T) {
	m := newMonitor(t, "cmd-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

	// Read the first snapshot so the connection is confirmed live.
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("initial ReadMessage: %v", err)
	}

	// Send set_interval:500ms — still within the allowed set and fast enough to
	// receive another snapshot well within the read deadline.
	cmd := `{"type":"set_interval","interval_ms":500}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		t.Fatalf("WriteMessage set_interval: %v", err)
	}

	// The server must still be alive and delivering snapshots after the command.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage after set_interval: %v", err)
	}
	var snap types.Snapshot
	if err := json.Unmarshal(msg, &snap); err != nil {
		t.Fatalf("unmarshal after set_interval: %v", err)
	}
}

func TestHandleWebSocket_SetIntervalCommand_Disallowed(t *testing.T) {
	m := newMonitor(t, "cmd-host2")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("initial ReadMessage: %v", err)
	}

	// Send a disallowed interval (999ms is not in the allowed set).
	cmd := `{"type":"set_interval","interval_ms":999}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		t.Fatalf("WriteMessage disallowed interval: %v", err)
	}

	// Connection must remain open and continue delivering snapshots.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage after disallowed interval: %v", err)
	}
	var snap types.Snapshot
	if err := json.Unmarshal(msg, &snap); err != nil {
		t.Fatalf("unmarshal after disallowed interval: %v", err)
	}
}

// ---- Stop ---------------------------------------------------------------

func TestStop_GracefulShutdown(t *testing.T) {
	m := newMonitor(t, "host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))

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
	ts := httptest.NewServer(handlerFrom(t, s))
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

// ---- Monitor stop closes WebSocket --------------------------------------

func TestHandleWebSocket_MonitorStopClosesProperly(t *testing.T) {
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

	// Wait for a snapshot so the monitor is producing data.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, e := m.Snapshot(); e == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(handlerFrom(t, s))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	// Read at least one message to confirm the connection is live.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("initial ReadMessage: %v", err)
	}

	// Stop the monitor. The server should close the WebSocket connection.
	m.Stop() //nolint:errcheck

	// The next read should yield a close frame or an error.
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("expected connection close after monitor stop, got nil error")
	}
}

// ---- helpers ------------------------------------------------------------

// handlerFrom extracts the http.Handler from a *server.Server via a fresh
// httptest.Server that uses the same mux – we wrap the server's Stop/Start
// only as a Handler source so the httptest server drives networking.
func handlerFrom(t *testing.T, s *server.Server) http.Handler {
	t.Helper()
	// server.Server embeds its own *http.Server. We need its handler.
	// Since Start() calls ListenAndServe on the internal server, we instead
	// spin up an httptest.Server with the handler extracted via a middleware
	// capture. We achieve this by having the server register its mux on a
	// ServeMux and capturing the reference.
	//
	// The cleanest approach: server.New returns a *Server whose handler is
	// exposed via ServeHTTP so it satisfies http.Handler directly.
	return s
}
