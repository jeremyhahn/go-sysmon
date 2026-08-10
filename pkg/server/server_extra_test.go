package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// ---- events – method not allowed ------------------------------------------

func TestHandleEvents_MethodNotAllowed(t *testing.T) {
	m := newMonitor(t, "events-verb-host")
	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/events", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/events: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// ---- events – no snapshot yet ---------------------------------------------

// TestHandleEvents_StreamsBeforeFirstSnapshot covers a client that connects in
// the window before the monitor has collected anything. The stream must open
// and then deliver the first snapshot when it arrives, not fail outright.
func TestHandleEvents_StreamsBeforeFirstSnapshot(t *testing.T) {
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "early-host"}},
	}
	m, err := monitor.New(stub, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	// Connect immediately, racing the monitor's first tick.
	c := dialEvents(t, ts.URL+"/api/events", false)

	ev := c.nextNamed(t, "snapshot", 3*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "early-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "early-host")
	}
}

// ---- events – multiple concurrent clients all receive snapshots ------------

func TestHandleEvents_ConcurrentClients(t *testing.T) {
	const numClients = 5

	m := newMonitor(t, "concurrent-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	errs := make(chan string, numClients)

	for i := range numClients {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			c := dialEvents(t, ts.URL+"/api/events", false)

			ev := c.nextNamed(t, "snapshot", 5*time.Second)
			var snap types.Snapshot
			if err := json.Unmarshal([]byte(ev.Data), &snap); err != nil {
				errs <- fmt.Sprintf("client %d unmarshal: %v", id, err)
				return
			}
			if snap.Host.Hostname != "concurrent-host" {
				errs <- fmt.Sprintf("client %d hostname = %q", id, snap.Host.Hostname)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Error(msg)
	}
}

// ---- events – abrupt client close does not break the server ---------------

// Connects several clients, drops them without reading to completion, and
// verifies the server continues to handle new requests correctly.
func TestHandleEvents_AbruptClientClose(t *testing.T) {
	m := newMonitor(t, "abrupt-host")
	waitForSnapshot(t, m)

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	for range 3 {
		c := dialEvents(t, ts.URL+"/api/events", false)
		c.nextNamed(t, "snapshot", 3*time.Second)
		c.Close()
	}

	// Give the server goroutines time to notice the disconnections.
	time.Sleep(100 * time.Millisecond)

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

// ---- interval affects the stream ------------------------------------------

// TestSetInterval_AppliesToActiveStream verifies the REST rate control reaches
// a stream that is already running, which is the whole point of moving the
// command off the transport.
func TestSetInterval_AppliesToActiveStream(t *testing.T) {
	stub := &stubSnapshotter{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "rate-host"}},
	}
	// Start slow enough that the default rate would not deliver in time.
	m, err := monitor.New(stub, time.Hour)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	s := server.New(m, ":0", nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	c := dialEvents(t, ts.URL+"/api/events", false)

	// Speed the monitor up over REST while the stream is open.
	resp, err := http.Post(ts.URL+"/api/interval", "application/json",
		strings.NewReader(`{"interval_ms":250}`))
	if err != nil {
		t.Fatalf("POST /api/interval: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/interval status = %d, want 200", resp.StatusCode)
	}

	// A snapshot must now arrive on the already-open stream well inside the
	// original hour-long interval.
	ev := c.nextNamed(t, "snapshot", 5*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "rate-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "rate-host")
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
