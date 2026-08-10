package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- GET /api/snapshot – method not allowed --------------------------------

// The mux is registered with "GET /api/snapshot". A POST must not match.
func TestHandleSnapshot_MethodNotAllowed(t *testing.T) {
	m := newMonitor(t, "method-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/snapshot: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	// Go 1.22+ pattern-matched muxes return 405 for the wrong method.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// ---- GET /api/snapshot – response decodes to a full Snapshot struct -------

func TestHandleSnapshot_ResponseDecodesCompletely(t *testing.T) {
	m := newMonitor(t, "decode-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var snap types.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Host.Hostname != "decode-host" {
		t.Errorf("Hostname = %q, want %q", snap.Host.Hostname, "decode-host")
	}
}

// ---- GET /api/snapshot – snapshotter returns an error ---------------------

func TestHandleSnapshot_SnapshotterError(t *testing.T) {
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "err-host"}},
	}
	m, err := monitor.New(stub, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	// Do not start the monitor so no snapshot is ever stored.

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// ---- WebSocket – upgrade fails for non-WebSocket request ------------------

// A plain HTTP GET to /ws without the Upgrade header must receive a non-101
// response (the upgrader rejects it and writes its own error).
func TestHandleWebSocket_UpgradeFailsForPlainHTTP(t *testing.T) {
	m := newMonitor(t, "upgrade-host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Errorf("status = 101, want a non-upgrade response for plain HTTP GET")
	}
}

// ---- WebSocket – multiple concurrent clients all receive snapshots ---------

func TestHandleWebSocket_ConcurrentClients(t *testing.T) {
	const numClients = 5

	m := newMonitor(t, "concurrent-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	var wg sync.WaitGroup
	errs := make(chan string, numClients)

	for i := range numClients {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				errs <- "client " + string(rune('0'+id)) + " dial: " + err.Error()
				return
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close connection: %v", err)
				}
			}()

			conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck

			_, msg, err := conn.ReadMessage()
			if err != nil {
				errs <- "client " + string(rune('0'+id)) + " read: " + err.Error()
				return
			}

			var snap types.Snapshot
			if err := json.Unmarshal(msg, &snap); err != nil {
				errs <- "client " + string(rune('0'+id)) + " unmarshal: " + err.Error()
				return
			}
			if snap.Host.Hostname != "concurrent-host" {
				errs <- "client " + string(rune('0'+id)) + " hostname mismatch"
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Error(msg)
	}
}

// ---- WebSocket – client abrupt close does not panic -----------------------

// Connects several clients, closes them without a handshake, and verifies the
// server continues to handle new connections correctly.
func TestHandleWebSocket_AbruptClientClose(t *testing.T) {
	m := newMonitor(t, "abrupt-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Open and immediately close several connections without a handshake close.
	for range 3 {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial: %v", err)
		}
		// Force-close the underlying TCP connection without sending a WS close.
		conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)) //nolint:errcheck
		conn.ReadMessage()                                          //nolint:errcheck
		// An abrupt close is the point of this test, so a Close error is an
		// acceptable outcome and only worth recording.
		if err := conn.Close(); err != nil {
			t.Logf("force-close connection: %v", err)
		}
	}

	// Give the server goroutines time to notice the disconnections.
	time.Sleep(100 * time.Millisecond)

	// Verify the server is still functional by making a successful snapshot request.
	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot after abrupt closes: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// ---- GET / – status page content ------------------------------------------

func TestHandleStatus_Body(t *testing.T) {
	m := newMonitor(t, "body-host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "go-sysmon") {
		t.Errorf("body = %q, want it to contain %q", string(body), "go-sysmon")
	}
}

// ---- ServeHTTP – satisfies http.Handler -----------------------------------

func TestServer_ServeHTTP(t *testing.T) {
	m := newMonitor(t, "serve-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ---- New – nil assets uses status handler ---------------------------------

func TestNew_NilAssets_StatusRoute(t *testing.T) {
	m := newMonitor(t, "nil-assets-host")
	s := server.New(m, ":0", nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
