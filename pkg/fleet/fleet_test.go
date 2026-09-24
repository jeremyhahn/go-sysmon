package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// healthySnapshot is a minimal but realistic snapshot.
func healthySnapshot(host string) *types.Snapshot {
	return &types.Snapshot{
		Version:    "0.1.2",
		Host:       types.HostInfo{Hostname: host, Uptime: 3600},
		CPUs:       []types.CPUInfo{{UsagePercent: 10}, {UsagePercent: 30}},
		CPUSummary: types.CPUSummary{TotalThreads: 4},
		LoadAvg:    types.LoadAverage{Load1: 2},
		Memory:     types.MemoryInfo{UsedPercent: 40},
		Disks:      []types.DiskInfo{{Name: "sda", TotalBytes: 100, UsedPercent: 55}},
	}
}

// peerServer serves a snapshot as a real HTTP peer.
func peerServer(t *testing.T, snap *types.Snapshot) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			t.Errorf("encoding peer snapshot: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestView_DisabledWithoutPeers is the guarantee that a standalone node keeps
// behaving exactly as it did before fleet mode existed.
func TestView_DisabledWithoutPeers(t *testing.T) {
	p := fleet.New(
		func() config.Fleet { return config.Fleet{} },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet(),
	)
	view := p.View(fleet.Query{})
	if view.Enabled {
		t.Error("fleet reports enabled with no peers configured")
	}
	if len(view.Nodes) != 0 {
		t.Errorf("nodes = %+v, want none", view.Nodes)
	}
	if p.Enabled() {
		t.Error("Enabled() = true with no peers")
	}
}

// TestView_SummarizesLocalAndPeer covers the happy path end to end over real
// HTTP, including that a peer's version travels with its data.
func TestView_SummarizesLocalAndPeer(t *testing.T) {
	remote := healthySnapshot("omega")
	remote.Version = "0.1.1"
	srv := peerServer(t, remote)

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		for _, n := range p.View(fleet.Query{}).Nodes {
			if n.Name == "omega" && n.Reachable {
				return true
			}
		}
		return false
	})
	close(done)

	view := p.View(fleet.Query{})
	if !view.Enabled {
		t.Fatal("fleet should be enabled with a peer configured")
	}
	if len(view.Nodes) != 2 {
		t.Fatalf("nodes = %d, want local + peer", len(view.Nodes))
	}

	var peer *types.FleetNode
	for i := range view.Nodes {
		if view.Nodes[i].Name == "omega" {
			peer = &view.Nodes[i]
		}
	}
	if peer == nil {
		t.Fatal("peer omega missing from the view")
	}
	if peer.Hostname != "omega" || peer.Version != "0.1.1" {
		t.Errorf("peer summary = %+v, want hostname omega version 0.1.1", peer)
	}
	if peer.CPUPercent != 20 {
		t.Errorf("CPUPercent = %v, want the mean of 10 and 30", peer.CPUPercent)
	}
	if peer.LoadPerCore != 0.5 {
		t.Errorf("LoadPerCore = %v, want load 2 over 4 threads", peer.LoadPerCore)
	}
	if peer.Status != fleet.StatusHealthy {
		t.Errorf("status = %q, want healthy", peer.Status)
	}
}

// TestView_UnreachablePeerIsNotHealthy guards the distinction that matters
// most: a node we cannot see must never render as a healthy node.
func TestView_UnreachablePeerIsNotHealthy(t *testing.T) {
	cfg := config.Fleet{
		// Port 1 on loopback refuses connections immediately.
		Peers:          []config.Peer{{Name: "dead", URL: "http://127.0.0.1:1"}},
		PollIntervalMs: 10000, TimeoutMs: 500,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		for _, n := range p.View(fleet.Query{}).Nodes {
			if n.Name == "dead" && n.Status == fleet.StatusUnreachable {
				return true
			}
		}
		return false
	})
	close(done)

	for _, n := range p.View(fleet.Query{}).Nodes {
		if n.Name != "dead" {
			continue
		}
		if n.Reachable {
			t.Error("an unreachable peer reports Reachable")
		}
		if n.Error == "" {
			t.Error("an unreachable peer should carry the reason")
		}
	}
}

// TestView_AlertsDriveStatus checks the mapping the fleet dashboard sorts on.
func TestView_AlertsDriveStatus(t *testing.T) {
	local := healthySnapshot("local")
	local.Alerts = []types.Alert{
		{Rule: "raid_degraded", Severity: types.SeverityCritical},
		{Rule: "disk_full", Severity: types.SeverityWarning},
	}
	cfg := config.Fleet{Peers: []config.Peer{{Name: "x", URL: "http://127.0.0.1:1"}}, PollIntervalMs: 10000, TimeoutMs: 200}
	p := fleet.New(func() config.Fleet { return cfg }, func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	for _, n := range p.View(fleet.Query{}).Nodes {
		if !n.Self {
			continue
		}
		if n.CriticalCount != 1 || n.WarningCount != 1 {
			t.Errorf("counts = %d critical, %d warning; want 1 and 1", n.CriticalCount, n.WarningCount)
		}
		if n.Status != fleet.StatusCritical {
			t.Errorf("status = %q, want critical to win", n.Status)
		}
	}
}

// TestPeerSnapshot_ServesLocalAndPeer: the drill-in fetches a peer's whole
// snapshot on demand, since the poll cycle carries only summaries (#76). It
// needs no poll to have run.
func TestPeerSnapshot_ServesLocalAndPeer(t *testing.T) {
	srv := peerServer(t, healthySnapshot("omega"))
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 10000, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	local, err := p.PeerSnapshot(context.Background(), "local", time.Minute)
	if err != nil || local.Snapshot == nil || !local.Reachable {
		t.Errorf("PeerSnapshot(local) = %+v, %v; want this node's own snapshot", local, err)
	}

	peer, err := p.PeerSnapshot(context.Background(), "omega", time.Minute)
	if err != nil || peer.Snapshot == nil || peer.Snapshot.Host.Hostname != "omega" {
		t.Fatalf("PeerSnapshot(omega) = %+v, %v", peer, err)
	}
	if !peer.Reachable || peer.Error != "" {
		t.Errorf("a peer that answered should be Reachable with no error, got reachable=%v err=%q",
			peer.Reachable, peer.Error)
	}
}

// TestPeerSnapshot_Errors: an unknown node, a local node with nothing
// collected, and a peer that has never answered are three different answers,
// and the server maps them to three different statuses.
func TestPeerSnapshot_Errors(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusInternalServerError)
	}))
	defer down.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: down.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	var unknown *fleet.ErrUnknownPeer
	if _, err := p.PeerSnapshot(context.Background(), "nope", time.Minute); !errors.As(err, &unknown) {
		t.Errorf("PeerSnapshot(nope) = %v; want ErrUnknownPeer", err)
	}
	var noReading *fleet.ErrNoReading
	if _, err := p.PeerSnapshot(context.Background(), "local", time.Minute); !errors.As(err, &noReading) {
		t.Errorf("PeerSnapshot(local) before any collection = %v; want ErrNoReading", err)
	}
	var status *fleet.ErrPeerStatus
	if _, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); !errors.As(err, &status) || status.Status != http.StatusInternalServerError {
		t.Errorf("PeerSnapshot(omega) from a failing peer never read = %v; want its HTTP 500", err)
	}
}

// TestPeerSnapshot_MarksAnUnreachablePeerStale is the fail-open guard for the
// drill-in path. The last snapshot fetched survives a failed fetch, which is
// deliberate -- a blip should not blank the page. What must NOT happen is
// serving that reading as an ordinary success: the detail view would show
// frozen numbers as live ones, and a client polling every second would
// re-record the same values into its history as though it kept measuring them.
func TestPeerSnapshot_MarksAnUnreachablePeerStale(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		if down.Load() {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(healthySnapshot("omega")); err != nil {
			t.Errorf("encoding peer snapshot: %v", err)
		}
	}))
	defer srv.Close()

	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	if peer, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); err != nil || !peer.Reachable {
		t.Fatalf("first PeerSnapshot(omega) = %+v, %v; want a live reading", peer, err)
	}

	// The peer goes away. The last reading survives; its honesty must not.
	// Waited out past the reuse window, so this is a real second fetch.
	down.Store(true)
	var peer types.PeerSnapshot
	waitFor(t, func() bool {
		got, err := p.PeerSnapshot(context.Background(), "omega", time.Minute)
		peer = got
		return err == nil && !got.Reachable
	})

	if peer.Snapshot == nil || peer.Snapshot.Host.Hostname != "omega" {
		t.Errorf("the last snapshot was discarded: %+v", peer)
	}
	if peer.Error == "" {
		t.Error("Error is empty for an unreachable peer; the client has nothing to explain the staleness with")
	}
	if peer.AgeSeconds <= 0 {
		t.Errorf("AgeSeconds = %v; a stale reading must say how old it is", peer.AgeSeconds)
	}
}

// TestPeerSnapshot_ConcurrentDrillInsShareOneFetch: several browsers open on
// one peer must not each cost that peer a whole snapshot per tick.
func TestPeerSnapshot_ConcurrentDrillInsShareOneFetch(t *testing.T) {
	var fetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if err := json.NewEncoder(w).Encode(healthySnapshot("omega")); err != nil {
			t.Errorf("encoding peer snapshot: %v", err)
		}
	}))
	defer srv.Close()
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.PeerSnapshot(context.Background(), "omega", time.Minute); err != nil {
				t.Errorf("PeerSnapshot(omega): %v", err)
			}
		}()
	}
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Errorf("8 concurrent drill-ins made %d fetches of the peer; want 1", n)
	}
}

// waitFor polls cond until it holds or the test times out. Polling keeps the
// test independent of the poller's internal scheduling.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before the deadline")
}

// TestPeerConfig_FetchesFromThePeer covers the settings view following the
// selected node. Serving the aggregator's own configuration while a peer is
// selected would read as that peer's settings and be wrong.
func TestPeerConfig_FetchesFromThePeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" {
			http.NotFound(w, r)
			return
		}
		view := types.ConfigView{
			ConfigFile: "/etc/go-sysmon/sysmon.yaml",
			Entries: []types.ConfigEntry{
				{Section: "alerts", Key: "alerts.disk_full_pct", Value: "96", Source: "config file"},
			},
		}
		if err := json.NewEncoder(w).Encode(view); err != nil {
			t.Errorf("encoding peer config: %v", err)
		}
	}))
	defer srv.Close()

	cfg := config.Fleet{Peers: []config.Peer{{Name: "fs", URL: srv.URL}}, PollIntervalMs: 10000, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	view, err := p.PeerConfig("fs")
	if err != nil {
		t.Fatalf("PeerConfig(fs): %v", err)
	}
	if len(view.Entries) != 1 || view.Entries[0].Value != "96" {
		t.Errorf("PeerConfig returned %+v, want the peer's own value 96", view.Entries)
	}
}

// TestPeerConfig_UnknownPeer covers the error path for a node that is not
// configured.
func TestPeerConfig_UnknownPeer(t *testing.T) {
	p := fleet.New(func() config.Fleet { return config.Fleet{TimeoutMs: 500} },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())
	if _, err := p.PeerConfig("ghost"); err == nil {
		t.Fatal("PeerConfig for an unconfigured node = nil error, want an error")
	}
}

// TestView_StaleSnapshotKeepsItsRealAge covers the freshness contract. A peer
// that stops answering keeps its last good snapshot so the card is not blank,
// but the age must go on measuring from when that data was fetched. Dating it
// from the failed retry instead would report a node unreachable for hours as
// having been updated moments ago, which is the reading an operator would act
// on.
func TestView_StaleSnapshotKeepsItsRealAge(t *testing.T) {
	var serving atomic.Bool
	serving.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		if !serving.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if err := json.NewEncoder(w).Encode(healthySnapshot("omega")); err != nil {
			t.Errorf("encoding: %v", err)
		}
	}))
	defer srv.Close()

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 20, TimeoutMs: 500,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	defer close(done)

	// Let one good poll land.
	waitFor(t, func() bool {
		for _, n := range p.View(fleet.Query{}).Nodes {
			if n.Name == "omega" && n.Reachable {
				return true
			}
		}
		return false
	})

	// Take the peer down and let several failed polls go by.
	serving.Store(false)
	waitFor(t, func() bool {
		for _, n := range p.View(fleet.Query{}).Nodes {
			if n.Name == "omega" && !n.Reachable {
				return true
			}
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)

	var node *types.FleetNode
	for i, n := range p.View(fleet.Query{}).Nodes {
		if n.Name == "omega" {
			node = &p.View(fleet.Query{}).Nodes[i]
			_ = n
		}
	}
	if node == nil {
		t.Fatal("peer omega missing from the view")
	}
	if node.Reachable {
		t.Fatal("peer should be reporting unreachable by now")
	}
	// The retained snapshot must still be there...
	if node.Hostname != "omega" {
		t.Errorf("stale snapshot was dropped; hostname = %q", node.Hostname)
	}
	// ...and its age must reflect the last SUCCESSFUL fetch, which was at least
	// the sleep above ago, not the most recent failed retry.
	if node.AgeSeconds < 0.2 {
		t.Errorf("AgeSeconds = %.3f, want it to keep growing from the successful fetch; "+
			"a failed retry must not reset it", node.AgeSeconds)
	}
}

// TestView_OrdersByNameNotSeverity is the owner ruling of 2026-09-01.
//
// View() used to return worst-status-first so a fault led the list. That reads
// as "unsorted" to anyone looking for a host: a warning node sorts ahead of a
// healthy one whose name comes first, so on the real fleet `fs` and
// `vm-host-1` appeared above `ci-node-1` and the list looked shuffled.
//
// The peers are configured in an order that is NEITHER alphabetical nor
// severity-ordered, and the unreachable node is given the name that sorts
// LAST. A fixture whose config order or severity order happens to match the
// alphabet would pass against the old comparator too and prove nothing.
func TestView_OrdersByNameNotSeverity(t *testing.T) {
	// zulu is never served, so it comes back unreachable -- the status that
	// used to jump to the front of the list.
	alpha := peerServer(t, healthySnapshot("alpha"))
	mike := peerServer(t, healthySnapshot("mike"))

	cfg := config.Fleet{
		Peers: []config.Peer{
			{Name: "zulu", URL: "http://127.0.0.1:1"}, // unreachable, sorts last
			{Name: "mike", URL: mike.URL},
			{Name: "alpha", URL: alpha.URL},
		},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}

	p := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("bravo"), 0 },
		func() string { return "bravo" }, quiet(),
	)

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		reachable := 0
		for _, n := range p.View(fleet.Query{}).Nodes {
			if n.Reachable {
				reachable++
			}
		}
		return reachable >= 3 // alpha, mike and the local node
	})
	close(done)

	var got []string
	for _, n := range p.View(fleet.Query{}).Nodes {
		got = append(got, n.Name)
	}

	want := []string{"alpha", "bravo", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nodes = %v, want %v (alphabetical; the unreachable node "+
				"must not be promoted to the front)", got, want)
		}
	}
}

// TestPeerConfig_OldPeerExplainsItself.
//
// The owner hit "The server returned 502 for /api/fleet/ci-node-1/config" and
// reported it as a bug. It was not one: the node was reachable, serving
// snapshots, and simply running a build from before /api/config existed, so the
// aggregator was faithfully reporting an upstream 404.
//
// "peer ci-node-1 returned HTTP 404" gives an operator nothing to act on. A
// 404 from a node that is otherwise answering is a version statement, and
// saying so turns a bug report into a known state with an obvious remedy.
func TestPeerConfig_OldPeerExplainsItself(t *testing.T) {
	// A peer that serves snapshots but has no /api/config: exactly an older
	// build, rather than a node that is down.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/snapshot" {
			if err := json.NewEncoder(w).Encode(healthySnapshot("ci-node-1")); err != nil {
				t.Errorf("encoding snapshot: %v", err)
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer old.Close()

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "ci-node-1", URL: old.URL}},
		PollIntervalMs: 10000,
		TimeoutMs:      2000,
	}
	p := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "mon0" }, quiet(),
	)

	_, err := p.PeerConfig("ci-node-1")
	if err == nil {
		t.Fatal("PeerConfig() = nil error for a peer with no /api/config")
	}

	var status *fleet.ErrPeerStatus
	if !errors.As(err, &status) {
		t.Fatalf("PeerConfig() = %v, want *ErrPeerStatus", err)
	}
	if status.Status != http.StatusNotFound || status.Endpoint != "/api/config" {
		t.Fatalf("ErrPeerStatus = %+v, want a 404 on /api/config", status)
	}

	message := err.Error()
	for _, want := range []string{"ci-node-1", "/api/config", "404", "older", "/api/version"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q, so an operator cannot act on it: %s",
				want, message)
		}
	}

	// Every other status keeps the plain wording. A 500 is a fault, and
	// telling someone it is a version problem would send them the wrong way.
	plain := (&fleet.ErrPeerStatus{Peer: "fs", Endpoint: "/api/config", Status: 500}).Error()
	if strings.Contains(plain, "older") {
		t.Errorf("a 500 is reported as a version problem: %s", plain)
	}

	// And neither does a 404 on an endpoint that has ALWAYS existed.
	//
	// /api/snapshot is what the fleet has always polled, so a 404 on it is a
	// wrong peer URL or a proxy that does not forward the path -- never an old
	// build. The version wording would send an operator to /api/version, which
	// fails the same way for the same reason, and delays finding the cause.
	core := (&fleet.ErrPeerStatus{Peer: "fs", Endpoint: "/api/snapshot", Status: 404}).Error()
	if strings.Contains(core, "older") || strings.Contains(core, "/api/version") {
		t.Errorf("a 404 on the core endpoint is reported as version skew: %s", core)
	}
	if !strings.Contains(core, "404") {
		t.Errorf("a 404 on the core endpoint does not say so: %s", core)
	}
}

// TestView_HealthCountsCarryTheirDenominator.
//
// RAIDDegraded false and SMARTUnhealthy zero are each produced by two
// different realities: "this host has none" and "this host's are fine". The
// fleet view rendered both as a green ok, so a node with no arrays advertised
// a healthy RAID subsystem it does not have, and a node whose disks report no
// SMART at all advertised healthy SMART -- which is what ci-node-1 does today
// while its own node page says N/A for every one of its devices.
//
// The per-disk model already distinguishes these; only the roll-up did not.
// The counts are what let a consumer tell them apart.
func TestView_HealthCountsCarryTheirDenominator(t *testing.T) {
	cases := []struct {
		name              string
		disks             []types.DiskInfo
		wantArrays        int
		wantRAIDUnhealthy int
		wantDegraded      bool
		wantCapable       int
		wantSMARTBad      int
	}{
		{
			// The case the owner reported: nothing to report on, and the old
			// fields say "fine" about all of it.
			name:  "no arrays and no SMART",
			disks: []types.DiskInfo{{Name: "sda", TotalBytes: 100, UsedPercent: 10}},
		},
		{
			// State matters: "" is deliberately non-operational, because an
			// array whose state could not be read is reported rather than
			// assumed well. A fixture omitting it is not a healthy array, which
			// is what the first version of this case got wrong.
			name: "arrays present and healthy",
			disks: []types.DiskInfo{
				{Name: "md0", TotalBytes: 100, RAID: &types.RAIDInfo{Level: "raid1", State: "clean"}},
			},
			wantArrays: 1,
		},
		{
			// Not degraded -- not serving I/O at all. It raises a CRITICAL
			// raid_not_active alert and the node page shows it red, while the
			// roll-up called it ok because Degraded was false.
			name: "an array that is not operational is unhealthy, not ok",
			disks: []types.DiskInfo{
				{Name: "md0", TotalBytes: 100, RAID: &types.RAIDInfo{Level: "raid1", State: "inactive"}},
			},
			wantArrays: 1, wantRAIDUnhealthy: 1,
		},
		{
			// The state sysfs gives us when it gives us nothing. Reported
			// rather than assumed well.
			name: "an array whose state cannot be read is unhealthy",
			disks: []types.DiskInfo{
				{Name: "md0", TotalBytes: 100, RAID: &types.RAIDInfo{Level: "raid1", State: ""}},
			},
			wantArrays: 1, wantRAIDUnhealthy: 1,
		},
		{
			name: "an active array is not unhealthy",
			disks: []types.DiskInfo{
				{Name: "md0", TotalBytes: 100, RAID: &types.RAIDInfo{Level: "raid1", State: "active"}},
			},
			wantArrays: 1,
		},
		{
			name: "arrays present and one degraded",
			disks: []types.DiskInfo{
				{Name: "md0", TotalBytes: 100, RAID: &types.RAIDInfo{Level: "raid1", State: "clean"}},
				{Name: "md1", TotalBytes: 100, RAID: &types.RAIDInfo{Level: "raid5", State: "active", Degraded: true}},
			},
			wantArrays: 2, wantDegraded: true, wantRAIDUnhealthy: 1,
		},
		{
			name: "SMART readable and healthy",
			disks: []types.DiskInfo{
				{Name: "sda", TotalBytes: 100, SMARTEnabled: true, SMARTHealthy: true},
				{Name: "sdb", TotalBytes: 100, SMARTEnabled: true, SMARTHealthy: true},
			},
			wantCapable: 2,
		},
		{
			name: "SMART readable and one failing",
			disks: []types.DiskInfo{
				{Name: "sda", TotalBytes: 100, SMARTEnabled: true, SMARTHealthy: true},
				{Name: "sdb", TotalBytes: 100, SMARTEnabled: true, SMARTHealthy: false},
			},
			wantCapable: 2, wantSMARTBad: 1,
		},
		{
			// ci-node-1: devices present, none of them answering SMART. The
			// unhealthy count is zero for the same reason it is zero on a
			// perfectly healthy host, which is exactly why zero alone is not
			// an answer.
			name: "devices present but none report SMART",
			disks: []types.DiskInfo{
				{Name: "sda", TotalBytes: 100, SMARTEnabled: false},
				{Name: "dm-0", TotalBytes: 100, SMARTEnabled: false},
			},
		},
		{
			// ci-node-1 once its PERC members are read. The volume itself has
			// no SMART -- SMARTEnabled is false on sda and sdb -- and all six
			// readable drives are behind it. Summing the disk list alone would
			// report a denominator of one for a host with seven answering
			// devices, which is #44's defect surviving its own fix.
			name: "drives behind a hardware RAID controller count",
			disks: []types.DiskInfo{
				{Name: "nvme0n1", TotalBytes: 100, SMARTEnabled: true, SMARTHealthy: true},
				{
					Name: "sda", TotalBytes: 100, SMARTEnabled: false,
					RAID: &types.RAIDInfo{Level: "raid1", State: "active"},
					Members: []types.DiskMember{
						{DeviceID: 0, SMARTEnabled: true, SMARTHealthy: true},
						{DeviceID: 1, SMARTEnabled: true, SMARTHealthy: true},
					},
				},
				{
					Name: "sdb", TotalBytes: 100, SMARTEnabled: false,
					RAID: &types.RAIDInfo{Level: "raid10", State: "active"},
					Members: []types.DiskMember{
						{DeviceID: 6, SMARTEnabled: true, SMARTHealthy: true},
						{DeviceID: 7, SMARTEnabled: true, SMARTHealthy: false},
						{DeviceID: 8, SMARTEnabled: true, SMARTHealthy: true},
						// A member we could not read is not counted as capable:
						// it is a device that told us nothing, not a well one.
						{DeviceID: 9, SMARTEnabled: false},
					},
				},
			},
			wantArrays: 2, wantCapable: 6, wantSMARTBad: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := healthySnapshot("local")
			snap.Disks = tc.disks

			p := fleet.New(
				func() config.Fleet {
					return config.Fleet{Peers: []config.Peer{{Name: "peer", URL: "http://127.0.0.1:1"}}}
				},
				func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, snap, 0 },
				func() string { return "local" }, quiet(),
			)

			var local *types.FleetNode
			for i := range p.View(fleet.Query{}).Nodes {
				if p.View(fleet.Query{}).Nodes[i].Self {
					local = &p.View(fleet.Query{}).Nodes[i]
					break
				}
			}
			if local == nil {
				t.Fatal("the local node is missing from the view")
			}

			if local.RAIDArrays != tc.wantArrays {
				t.Errorf("RAIDArrays = %d, want %d", local.RAIDArrays, tc.wantArrays)
			}
			if local.RAIDUnhealthy != tc.wantRAIDUnhealthy {
				t.Errorf("RAIDUnhealthy = %d, want %d", local.RAIDUnhealthy, tc.wantRAIDUnhealthy)
			}
			if local.RAIDDegraded != tc.wantDegraded {
				t.Errorf("RAIDDegraded = %v, want %v", local.RAIDDegraded, tc.wantDegraded)
			}
			if local.SMARTCapable != tc.wantCapable {
				t.Errorf("SMARTCapable = %d, want %d", local.SMARTCapable, tc.wantCapable)
			}
			if local.SMARTUnhealthy != tc.wantSMARTBad {
				t.Errorf("SMARTUnhealthy = %d, want %d", local.SMARTUnhealthy, tc.wantSMARTBad)
			}
		})
	}
}

// TestView_APeerThatNeverAnsweredHasNoAge.
//
// age_seconds is the only thing distinguishing a node mid-restart from one gone
// for days (#69), so what it says for a node that has NEVER answered decides
// whether that distinction works at all.
//
// It used to be set from the last attempt, which is a couple of seconds ago and
// always will be -- so a peer that had never once responded reported the same
// small number as one that blipped during a deploy, forever. Zero is the
// sentinel for "no reading", and it is what the never-polled path already
// produced, so the two states now agree.
func TestView_APeerThatNeverAnsweredHasNoAge(t *testing.T) {
	// A peer that is configured and refuses every connection: polled, failed,
	// and never had a snapshot to date.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer dead.Close()

	cfg := config.Fleet{
		PollIntervalMs: 10, TimeoutMs: 500,
		Peers: []config.Peer{{Name: "dead", URL: dead.URL}},
	}
	p := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 },
		func() string { return "local" }, quiet(),
	)

	done := make(chan struct{})
	go p.Run(done)
	defer close(done)

	// Wait for at least one failed poll, so this is the "polled and failed"
	// state rather than the "not polled yet" one -- they are different code
	// paths and only one of them is under test here.
	deadline := time.Now().Add(2 * time.Second)
	var node types.FleetNode
	for time.Now().Before(deadline) {
		for _, n := range p.View(fleet.Query{}).Nodes {
			if n.Name == "dead" && n.Error != "" && n.Error != "not polled yet" {
				node = n
			}
		}
		if node.Name != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if node.Name == "" {
		t.Fatal("the peer was never polled, so this tests the wrong path")
	}

	if node.Reachable {
		t.Fatal("a peer refusing every request is reachable, so this proves nothing")
	}
	if node.AgeSeconds != 0 {
		t.Errorf("age_seconds = %v for a peer that has never answered, want 0. "+
			"A non-zero age here is the time since the last ATTEMPT, which reads as "+
			"a brief gap forever and is exactly what #69 is about", node.AgeSeconds)
	}
}

// TestObserve_ReportsEveryReadingThatArrives is topology A's contract on the
// poller side: the aggregator persists the fleet, so every peer reading it
// receives has to be handed over exactly once, under that peer's name.
func TestObserve_ReportsEveryReadingThatArrives(t *testing.T) {
	remote := healthySnapshot("omega")
	srv := peerServer(t, remote)

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10, TimeoutMs: 2000,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	var mu sync.Mutex
	var names []string
	var hosts []string
	p.Observe(func(node string, snap *types.Snapshot) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, node)
		if snap != nil {
			hosts = append(hosts, snap.Host.Hostname)
		}
	}, func() time.Duration { return time.Hour })

	done := make(chan struct{})
	go p.Run(done)
	defer close(done)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(names) >= 2
	})

	mu.Lock()
	defer mu.Unlock()
	for i, got := range names {
		if got != "omega" {
			t.Errorf("reading %d reported node %q, want omega", i, got)
		}
	}
	// The snapshot handed over must be the peer's own, not the local one --
	// writing the aggregator's readings under a peer's name would fill the
	// peer's history with another machine's numbers.
	for i, got := range hosts {
		if got != "omega" {
			t.Errorf("reading %d carried hostname %q, want omega", i, got)
		}
	}
}

// TestObserve_SilentWhileAPeerIsUnreachable is the other half, and the one that
// keeps the history honest. The poller retains the last good snapshot so the
// card shows stale data rather than emptying; re-reporting it would record an
// observation nobody made and would erase the outage from the history.
func TestObserve_SilentWhileAPeerIsUnreachable(t *testing.T) {
	var serving atomic.Bool
	serving.Store(true)
	remote := healthySnapshot("omega")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		if !serving.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if err := json.NewEncoder(w).Encode(remote); err != nil {
			t.Errorf("encoding peer snapshot: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10, TimeoutMs: 2000,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	var readings atomic.Int64
	p.Observe(func(string, *types.Snapshot) { readings.Add(1) }, func() time.Duration { return time.Hour })

	done := make(chan struct{})
	go p.Run(done)
	defer close(done)

	waitFor(t, func() bool { return readings.Load() >= 2 })
	serving.Store(false)

	// Let several polls fail, then confirm the count stopped moving while the
	// peer stayed visible in the view with its retained snapshot.
	settled := readings.Load()
	time.Sleep(120 * time.Millisecond)
	after := readings.Load()

	// One reading may have been in flight when serving flipped.
	if after > settled+1 {
		t.Errorf("readings went %d -> %d while the peer was unreachable; a retained snapshot was reported as a new observation", settled, after)
	}

	var found bool
	for _, n := range p.View(fleet.Query{}).Nodes {
		if n.Name == "omega" {
			found = true
			if n.Status == "" {
				t.Error("the unreachable peer lost its card entirely")
			}
		}
	}
	if !found {
		t.Error("the unreachable peer disappeared from the view")
	}
}

// TestPeerPayloads_AnOlderPeersNullListsAreNotPassedOn covers #151 at the
// fleet's door. A peer on a build from before the fix sends null for every list
// it has none of, and this node serves that snapshot, and that configuration,
// on as its own -- so they are normalised as they arrive, or the dashboard
// would meet the null the local collector no longer produces.
func TestPeerPayloads_AnOlderPeersNullListsAreNotPassedOn(t *testing.T) {
	// Encoded from Go values with nil lists, exactly as an older build would.
	oldSnapshot, err := json.Marshal(types.Snapshot{
		Host: types.HostInfo{Hostname: "old"},
		NPUs: []types.NPUInfo{{Name: "accel0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	oldConfig, err := json.Marshal(types.ConfigView{ConfigFile: "(not set)"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(oldSnapshot), `"engines":null`) || !strings.Contains(string(oldConfig), `"entries":null`) {
		t.Fatal("the fixture no longer carries null lists, so it cannot stand for an older peer")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string][]byte{"/api/snapshot": oldSnapshot, "/api/config": oldConfig}[r.URL.Path]
		if body == nil {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("writing peer response: %v", err)
		}
	}))
	defer srv.Close()

	cfg := config.Fleet{Peers: []config.Peer{{Name: "old", URL: srv.URL}}, PollIntervalMs: 10000, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, healthySnapshot("local"), 0 }, func() string { return "local" }, quiet())

	peer, err := p.PeerSnapshot(context.Background(), "old", time.Minute)
	if err != nil {
		t.Fatalf("PeerSnapshot(old): %v", err)
	}
	assertNoNullLists(t, "peer snapshot", peer)

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool { return node(t, p.View(fleet.Query{}), "old").Reachable })
	close(done)
	assertNoNullLists(t, "fleet view", p.View(fleet.Query{}))

	view, err := p.PeerConfig("old")
	if err != nil {
		t.Fatalf("PeerConfig(old): %v", err)
	}
	assertNoNullLists(t, "peer config", view)
}

// assertNoNullLists fails if any list in v's encoding is null.
func assertNoNullLists(t *testing.T, what string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", what, err)
	}
	nulls, err := typestest.NullSlices(raw, v)
	if err != nil {
		t.Fatalf("%s: NullSlices: %v", what, err)
	}
	if len(nulls) != 0 {
		t.Errorf("%s: lists encoded as null: %v", what, nulls)
	}
}
