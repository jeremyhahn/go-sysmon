package fleet_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
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
		func() *types.Snapshot { return healthySnapshot("local") },
		func() string { return "local" }, quiet(),
	)
	view := p.View()
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
		func() *types.Snapshot { return healthySnapshot("local") }, func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		for _, n := range p.View().Nodes {
			if n.Name == "omega" && n.Reachable {
				return true
			}
		}
		return false
	})
	close(done)

	view := p.View()
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
		func() *types.Snapshot { return healthySnapshot("local") }, func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		for _, n := range p.View().Nodes {
			if n.Name == "dead" && n.Status == fleet.StatusUnreachable {
				return true
			}
		}
		return false
	})
	close(done)

	for _, n := range p.View().Nodes {
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
	p := fleet.New(func() config.Fleet { return cfg }, func() *types.Snapshot { return local }, func() string { return "local" }, quiet())

	for _, n := range p.View().Nodes {
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

// TestPeerSnapshot_ServesLocalAndPeer covers the drill-in path that lets the
// dashboard render any node without a cross-origin request.
func TestPeerSnapshot_ServesLocalAndPeer(t *testing.T) {
	srv := peerServer(t, healthySnapshot("omega"))
	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 10000, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() *types.Snapshot { return healthySnapshot("local") }, func() string { return "local" }, quiet())

	if _, ok := p.PeerSnapshot("local"); !ok {
		t.Error("PeerSnapshot(local) should return this node's own snapshot")
	}
	if _, ok := p.PeerSnapshot("nope"); ok {
		t.Error("PeerSnapshot for an unknown node should report not found")
	}

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool { _, ok := p.PeerSnapshot("omega"); return ok })
	close(done)

	peer, ok := p.PeerSnapshot("omega")
	if !ok || peer.Snapshot == nil || peer.Snapshot.Host.Hostname != "omega" {
		t.Errorf("PeerSnapshot(omega) = %+v, %v", peer, ok)
	}
	if !peer.Reachable || peer.Error != "" {
		t.Errorf("a peer that answered should be Reachable with no error, got reachable=%v err=%q",
			peer.Reachable, peer.Error)
	}
}

// TestPeerSnapshot_MarksAnUnreachablePeerStale is the fail-open guard for the
// drill-in path. The poller keeps the last good snapshot when a peer stops
// answering, which is deliberate -- a blip should not blank the page. What must
// NOT happen is serving that reading as an ordinary success: the fleet card
// would report the node unreachable while the detail view beside it showed
// frozen numbers as live ones, and a client polling every second would re-record
// the same values into its history as though it kept measuring them.
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

	cfg := config.Fleet{Peers: []config.Peer{{Name: "omega", URL: srv.URL}}, PollIntervalMs: 10, TimeoutMs: 2000}
	p := fleet.New(func() config.Fleet { return cfg },
		func() *types.Snapshot { return healthySnapshot("local") }, func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	defer close(done)

	waitFor(t, func() bool {
		peer, ok := p.PeerSnapshot("omega")
		return ok && peer.Reachable
	})

	// The peer goes away. The cached reading survives; its honesty must not.
	down.Store(true)
	waitFor(t, func() bool {
		peer, ok := p.PeerSnapshot("omega")
		return ok && !peer.Reachable
	})

	peer, ok := p.PeerSnapshot("omega")
	if !ok {
		t.Fatal("PeerSnapshot(omega) stopped returning the cached snapshot; the last good reading should survive a blip")
	}
	if peer.Snapshot == nil || peer.Snapshot.Host.Hostname != "omega" {
		t.Errorf("cached snapshot was discarded: %+v", peer)
	}
	if peer.Reachable {
		t.Error("Reachable is true for a peer whose latest poll failed; the detail view would show stale data as live")
	}
	if peer.Error == "" {
		t.Error("Error is empty for an unreachable peer; the client has nothing to explain the staleness with")
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
		func() *types.Snapshot { return healthySnapshot("local") }, func() string { return "local" }, quiet())

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
		func() *types.Snapshot { return nil }, func() string { return "local" }, quiet())
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
		func() *types.Snapshot { return healthySnapshot("local") },
		func() string { return "local" }, quiet())

	done := make(chan struct{})
	go p.Run(done)
	defer close(done)

	// Let one good poll land.
	waitFor(t, func() bool {
		for _, n := range p.View().Nodes {
			if n.Name == "omega" && n.Reachable {
				return true
			}
		}
		return false
	})

	// Take the peer down and let several failed polls go by.
	serving.Store(false)
	waitFor(t, func() bool {
		for _, n := range p.View().Nodes {
			if n.Name == "omega" && !n.Reachable {
				return true
			}
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)

	var node *types.FleetNode
	for i, n := range p.View().Nodes {
		if n.Name == "omega" {
			node = &p.View().Nodes[i]
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
		func() *types.Snapshot { return healthySnapshot("bravo") },
		func() string { return "bravo" }, quiet(),
	)

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		reachable := 0
		for _, n := range p.View().Nodes {
			if n.Reachable {
				reachable++
			}
		}
		return reachable >= 3 // alpha, mike and the local node
	})
	close(done)

	var got []string
	for _, n := range p.View().Nodes {
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
		func() *types.Snapshot { return nil },
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
				func() *types.Snapshot { return snap },
				func() string { return "local" }, quiet(),
			)

			var local *types.FleetNode
			for i := range p.View().Nodes {
				if p.View().Nodes[i].Self {
					local = &p.View().Nodes[i]
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
		func() *types.Snapshot { return healthySnapshot("local") },
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
		for _, n := range p.View().Nodes {
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
