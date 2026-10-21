//go:build integration

package server

import (
	"context"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestLifecycle_BeforeTheFirstCollection starts a service whose first
// collection is a minute away. Until it happens, every reading endpoint says
// there is nothing yet -- 503, and why -- rather than serving an empty
// reading as though it were a measurement, and a stream opened in that window
// is sent nothing until there is something, then a clean bye at shutdown.
func TestLifecycle_BeforeTheFirstCollection(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{Args: []string{"--interval", "60000"}, NoSnapshot: true})

	for _, path := range []string{"/api/snapshot", "/api/summary", "/api/snapshot/history", "/readyz"} {
		status, body := harness.Get(t, s.URL(path))
		if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "not collected a snapshot yet") {
			t.Errorf("GET %s before the first collection = %d %q, want 503 saying so", path, status, body)
		}
	}
	// The service is up regardless: the status endpoints answer.
	for _, path := range []string{"/api/version", "/healthz"} {
		if status, _ := harness.Get(t, s.URL(path)); status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 while waiting for the first collection", path, status)
		}
	}

	streams := map[string]*stream{
		"snapshot": openStream(t, s.URL("/api/events"), streamOptions{}),
		"delta":    openStream(t, s.URL("/api/events?deltas=1"), streamOptions{}),
	}
	s.Signal(t, syscall.SIGTERM)
	for name, st := range streams {
		if ev := st.next(t); ev.name != "bye" {
			t.Errorf("%s stream opened before any collection got %q first, want only the bye", name, ev.name)
		}
	}
	if err := s.Wait(t); err != nil {
		t.Errorf("shutdown before the first collection: %v, want a clean exit", err)
	}
}

// TestLifecycle_AMonitorRestartIsNeverShownAsCurrent embeds the server and
// stops and restarts its monitor under it, as an application pausing
// collection does. While stopped, a new stream is told goodbye at once
// rather than being sent the last reading as live; open streams are told too.
// After the restart, a delta client starts again from a whole snapshot, and
// its sequence carries on from where it was -- never backwards.
func TestLifecycle_AMonitorRestartIsNeverShownAsCurrent(t *testing.T) {
	mon := hostMonitor(t)
	srv, err := server.NewWithConfig(server.Config{Monitor: mon, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	base := "http://" + run(t, srv)
	harness.WaitForSnapshot(t, base+"/api/snapshot")
	if status, body := harness.Get(t, base+"/readyz"); status != http.StatusOK {
		t.Errorf("GET /readyz after the first collection = %d %q, want 200", status, body)
	}

	before := &deltaClient{t: t, st: openStream(t, base+"/api/events?deltas=1", streamOptions{})}
	before.step()
	before.step()
	whole := openStream(t, base+"/api/events", streamOptions{})
	whole.next(t)

	if err := mon.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
	for name, st := range map[string]*stream{"delta": before.st, "snapshot": whole} {
		ev := st.next(t)
		for i := 0; ev.name != "bye" && i < 10; i++ {
			ev = st.next(t)
		}
		if ev.name != "bye" {
			t.Errorf("open %s stream when the monitor stopped ended with %q, want bye", name, ev.name)
		}
	}
	for _, url := range []string{base + "/api/events", base + "/api/events?deltas=1"} {
		if ev := openStream(t, url, streamOptions{}).next(t); ev.name != "bye" {
			t.Errorf("a stream opened while the monitor is stopped got %q, want bye: the last reading is not live", ev.name)
		}
	}

	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start again: %v", err)
	}
	await.Until(t, "the restarted monitor's first collection", func(context.Context) bool {
		_, err := mon.Snapshot()
		return err == nil
	})
	after := &deltaClient{t: t, st: openStream(t, base+"/api/events?deltas=1", streamOptions{})}
	after.step()
	if after.fulls != 1 {
		t.Fatal("the first frame after a restart was a patch, against a base from the previous run")
	}
	if after.seq <= before.seq {
		t.Errorf("sequence after the restart = %d, not after %d from before it", after.seq, before.seq)
	}
	for range 3 {
		after.step()
	}
}

// TestLifecycle_ADisconnectedClientReleasesItsStream verifies clients that
// go away mid-stream, in either protocol, leave the service serving the
// clients that stayed.
func TestLifecycle_ADisconnectedClientReleasesItsStream(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	for _, url := range []string{s.URL("/api/events"), s.URL("/api/events?deltas=1")} {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		cancel()
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing a cancelled stream: %v", err)
		}
	}
	stayed := &deltaClient{t: t, st: openStream(t, s.URL("/api/events?deltas=1"), streamOptions{})}
	stayed.step()
	for start := stayed.seq; stayed.seq < start+5; {
		stayed.step()
	}
}
