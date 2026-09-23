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
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestHandleFleetSnapshot_StaleDataIsNotServedAsCurrent is the HTTP-level
// regression test for the drill-in fail-open.
//
// The poller keeps a peer's last good snapshot when the peer stops answering,
// on purpose: a blip should not blank the page. The handler used to return that
// snapshot bare, so GET /api/fleet/{name}/snapshot answered 200 with frozen
// readings and nothing to distinguish them from a fresh collection. The fleet
// card reported the node unreachable while the detail view beside it did not,
// and a dashboard polling once a second folded the same values into its history
// over and over as though it had kept measuring them.
//
// The contract asserted here: the cached snapshot still comes back, and it is
// unambiguously labelled as not current.
func TestHandleFleetSnapshot_StaleDataIsNotServedAsCurrent(t *testing.T) {
	var down atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		if down.Load() {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		snap := &types.Snapshot{Host: types.HostInfo{Hostname: "fs"}}
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			t.Errorf("encoding peer snapshot: %v", err)
		}
	}))
	defer peer.Close()

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "fs", URL: peer.URL}},
		PollIntervalMs: 10,
		TimeoutMs:      2000,
	}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	done := make(chan struct{})
	go poller.Run(done)
	defer close(done)

	srv := newServer(Config{Fleet: poller}, nil)

	get := func(t *testing.T) types.PeerSnapshot {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fleet/fs/snapshot", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/fleet/fs/snapshot = %d, want 200", rec.Code)
		}
		var got types.PeerSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding response: %v; body=%s", err, rec.Body.String())
		}
		return got
	}

	waitUntil(t, func() bool {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fleet/fs/snapshot", nil))
		return rec.Code == http.StatusOK
	})

	live := get(t)
	if live.Snapshot == nil || live.Snapshot.Host.Hostname != "fs" {
		t.Fatalf("live response did not carry the peer's snapshot: %+v", live)
	}
	if !live.Reachable {
		t.Error("a peer that answered is reported unreachable")
	}

	// The peer goes away.
	down.Store(true)
	waitUntil(t, func() bool { return !get(t).Reachable })

	stale := get(t)
	if stale.Snapshot == nil {
		t.Error("the last good snapshot was discarded; a brief blip should not blank the view")
	}
	if stale.Reachable {
		t.Fatal("stale data is still reported reachable: the detail view would show it as live")
	}
	if stale.Error == "" {
		t.Error("no error accompanies the stale snapshot; the client cannot explain why it is frozen")
	}
}

// waitUntil polls cond so the test does not depend on the poller's scheduling.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the timeout")
}
