package fleet_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const gib = 1 << 30

// capacitySnapshot is a node a scheduler could place work on: 16 cores at
// 25%, 64 GiB with 40 GiB available, 500 GiB free on its docker root, and two
// containers using 2 cores and 3 GiB between them.
func capacitySnapshot(host string) *types.Snapshot {
	s := healthySnapshot(host)
	s.CPUs = []types.CPUInfo{{UsagePercent: 25, UsageAvailable: true}, {UsagePercent: 25, UsageAvailable: true}}
	s.CPUSummary.TotalThreads = 16
	s.LoadAvg = types.LoadAverage{Load1: 4, Load5: 3, Load15: 2}
	s.Memory.TotalBytes, s.Memory.AvailableBytes = 64*gib, 40*gib
	s.Virt.RuntimeDisk = types.RuntimeDisk{Path: "/var/lib/docker", TotalBytes: 1000 * gib, FreeBytes: 500 * gib}
	s.Virt.Capability.ContainersObservable = true
	s.Virt.Containers = []types.ContainerInfo{
		{Name: "runner-1", CPUPercent: 150, MemoryBytes: 2 * gib},
		{Name: "runner-2", CPUPercent: 50, MemoryBytes: 1 * gib},
	}
	return s
}

// TestSummarize_CarriesCapacity is #239's per-node figures: absolute cores,
// load, memory, the free disk under the container runtime, and what the
// containers are using -- their CPU in cores, from percent of one core.
func TestSummarize_CarriesCapacity(t *testing.T) {
	s := fleet.Summarize(nil, capacitySnapshot("worker"))
	if s.CPUCount != 16 || s.Load1 != 4 || s.Load5 != 3 || s.Load15 != 2 || s.LoadPerCore != 0.25 {
		t.Errorf("cpu = %d cores, load %v/%v/%v (%v per core); want 16, 4/3/2, 0.25",
			s.CPUCount, s.Load1, s.Load5, s.Load15, s.LoadPerCore)
	}
	if s.MemoryTotalBytes != 64*gib || s.MemoryAvailableBytes != 40*gib {
		t.Errorf("memory = %d total, %d available; want 64 GiB, 40 GiB", s.MemoryTotalBytes, s.MemoryAvailableBytes)
	}
	if s.DiskFreePath != "/var/lib/docker" || s.DiskFreeBytes != 500*gib || s.DiskTotalBytes != 1000*gib {
		t.Errorf("disk = %q %d free of %d; want /var/lib/docker, 500 GiB of 1000 GiB",
			s.DiskFreePath, s.DiskFreeBytes, s.DiskTotalBytes)
	}
	if !s.ContainersObservable || s.ContainersRunning != 2 || s.ContainerCPUCores != 2 || s.ContainerMemoryBytes != 3*gib {
		t.Errorf("containers = observable %v, %d running, %v cores, %d bytes; want true, 2, 2, 3 GiB",
			s.ContainersObservable, s.ContainersRunning, s.ContainerCPUCores, s.ContainerMemoryBytes)
	}
}

// TestSummarize_UnmeasuredCapacityStaysUnknown: every figure the node could
// not measure keeps its unknown form -- a zero count, an empty path, a false
// flag -- and nothing measured is presented beside it as if it were.
func TestSummarize_UnmeasuredCapacityStaysUnknown(t *testing.T) {
	snap := capacitySnapshot("worker")
	snap.CPUSummary.TotalThreads = 0
	snap.Memory.TotalBytes, snap.Memory.AvailableBytes = 0, 0
	// Free bytes with no path: a reading nothing vouches for.
	snap.Virt.RuntimeDisk = types.RuntimeDisk{FreeBytes: 123}
	// Containers listed from a partial cgroup scan cannot vouch for a count.
	snap.Virt.Capability.ContainersObservable = false

	s := fleet.Summarize(nil, snap)
	if s.CPUCount != 0 || s.MemoryTotalBytes != 0 {
		t.Errorf("cores %d, memory %d; want both unknown", s.CPUCount, s.MemoryTotalBytes)
	}
	if s.DiskFreePath != "" || s.DiskFreeBytes != 0 || s.DiskTotalBytes != 0 {
		t.Errorf("disk = %q %d of %d; an unmeasured disk carries no figures", s.DiskFreePath, s.DiskFreeBytes, s.DiskTotalBytes)
	}
	if s.ContainersObservable || s.ContainersRunning != 0 || s.ContainerCPUCores != 0 || s.ContainerMemoryBytes != 0 {
		t.Errorf("containers = observable %v, %d, %v cores, %d bytes; want unobservable with no figures",
			s.ContainersObservable, s.ContainersRunning, s.ContainerCPUCores, s.ContainerMemoryBytes)
	}
}

// TestSummarize_AnOlderPeersSnapshotHasNoCapacity: a peer too old to serve a
// summary is summarised here from its whole snapshot, which carries no
// runtime disk and no container capability -- unknown, not a full disk and
// no containers.
func TestSummarize_AnOlderPeersSnapshotHasNoCapacity(t *testing.T) {
	s := fleet.Summarize(nil, healthySnapshot("older"))
	if s.DiskFreePath != "" || s.ContainersObservable {
		t.Errorf("disk path %q, containers observable %v; an older snapshot measured neither", s.DiskFreePath, s.ContainersObservable)
	}
}

// olderSummaryPeer serves the fixture summary of a build that predates #239,
// as /api/summary. It never serves a snapshot: it is not that old.
func olderSummaryPeer(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/summary-pre-capacity.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/summary" {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("writing the older summary: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// capacityPoller is an aggregator over a current peer and one on an older
// build, at the target cfg holds, refreshed once.
func capacityPoller(t *testing.T, cfg *atomic.Pointer[config.Fleet]) *fleet.Poller {
	t.Helper()
	modern := &peer{}
	modern.set(nil, capacitySnapshot("modern"))
	base := *cfg.Load()
	base.Peers = []config.Peer{{Name: "modern", URL: startPeer(t, modern).URL}, {Name: "older", URL: olderSummaryPeer(t).URL}}
	base.TimeoutMs = 2000
	cfg.Store(&base)
	p := fleet.New(func() config.Fleet { return *cfg.Load() },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			return nil, capacitySnapshot("local"), time.Second
		},
		func() string { return "local" }, quiet())
	fleet.Refresh(p)
	return p
}

// TestView_CapacityAndHeadroomForEveryNode is the query a coordinator makes:
// one GET /api/fleet, and every node -- this one, served as self, and its
// peers -- carries its absolute capacity and its headroom at the configured
// target, while a peer on an older build carries neither and is counted out
// of the fleet's totals rather than in as zero.
func TestView_CapacityAndHeadroomForEveryNode(t *testing.T) {
	var cfg atomic.Pointer[config.Fleet]
	cfg.Store(&config.Fleet{HeadroomTargetFraction: 0.75})
	p := capacityPoller(t, &cfg)
	view := p.View(context.Background(), fleet.Query{})

	// 16 x (0.75 - 0.25) = 8 cores; 40 GiB less a quarter of 64: 24 GiB.
	want := types.NodeHeadroom{Target: 0.75, CPUCoresFreeAtTarget: 8, CPUReported: true, MemoryBytesFree: 24 * gib, MemoryReported: true}
	if view.Self == nil || view.Self.Headroom == nil || *view.Self.Headroom != want {
		t.Fatalf("self headroom = %+v, want %+v", view.Self, want)
	}
	for _, name := range []string{"local", "modern"} {
		n := node(t, view, name)
		if n.Headroom == nil || *n.Headroom != want {
			t.Errorf("%s headroom = %+v, want %+v", name, n.Headroom, want)
		}
		if n.CPUCount != 16 || n.DiskFreePath != "/var/lib/docker" || n.ContainersRunning != 2 {
			t.Errorf("%s capacity = %d cores, disk %q, %d containers", name, n.CPUCount, n.DiskFreePath, n.ContainersRunning)
		}
	}

	older := node(t, view, "older")
	if !older.Reachable || older.Source != types.SummarySourceSummary {
		t.Fatalf("older peer = reachable %v, source %q; want its summary read", older.Reachable, older.Source)
	}
	if older.Headroom != nil || older.CPUCount != 0 || older.DiskFreePath != "" || older.ContainersObservable {
		t.Errorf("older peer = headroom %+v, %d cores, disk %q, containers observable %v; want every one unknown",
			older.Headroom, older.CPUCount, older.DiskFreePath, older.ContainersObservable)
	}

	c := view.Rollup.Capacity
	if c == nil {
		t.Fatal("rollup capacity omitted")
	}
	if got := *c.CPUCount; got != (types.FleetTotal{Value: 32, Nodes: 2, Of: 3}) {
		t.Errorf("cpu_count = %+v, want 32 across 2 of 3", got)
	}
	if got := *c.DiskFreeBytes; got != (types.FleetTotal{Value: 1000 * gib, Nodes: 2, Of: 3}) {
		t.Errorf("disk_free_bytes = %+v, want 1000 GiB across 2 of 3", got)
	}
	if got := *c.Headroom.MemoryBytesFree; got != (types.FleetTotal{Value: 48 * gib, Nodes: 2, Of: 3}) {
		t.Errorf("headroom memory = %+v, want 48 GiB across 2 of 3", got)
	}

	// The wire form: an absent headroom is absent, not null or zeros.
	raw, err := json.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["headroom"]; ok {
		t.Errorf("older peer encodes a headroom: %s", fields["headroom"])
	}
	if string(fields["disk_free_path"]) != `""` || string(fields["cpu_count"]) != "0" {
		t.Errorf("older peer encodes disk_free_path %s, cpu_count %s; want the unknown forms", fields["disk_free_path"], fields["cpu_count"])
	}
}

// TestView_AReloadedHeadroomTargetIsLiveAtOnce: fleet.headroom_target is
// reloadable like the other fleet keys. The peers' part of the view is cached
// at each refresh; a new target must not wait for the next one, or the view
// would state headroom at a target the configuration no longer holds.
func TestView_AReloadedHeadroomTargetIsLiveAtOnce(t *testing.T) {
	var cfg atomic.Pointer[config.Fleet]
	cfg.Store(&config.Fleet{HeadroomTargetFraction: 0.75})
	p := capacityPoller(t, &cfg)
	if h := node(t, p.View(context.Background(), fleet.Query{}), "modern").Headroom; h == nil || h.Target != 0.75 {
		t.Fatalf("headroom before the reload = %+v, want at 0.75", h)
	}

	// The same peer list, as a reload that changed only the target leaves it.
	reloaded := *cfg.Load()
	reloaded.HeadroomTargetFraction = 0.5
	cfg.Store(&reloaded)

	view := p.View(context.Background(), fleet.Query{})
	for _, name := range []string{"local", "modern"} {
		if h := node(t, view, name).Headroom; h == nil || h.Target != 0.5 || h.CPUCoresFreeAtTarget != 4 {
			t.Errorf("%s headroom after the reload = %+v, want 4 cores at 0.5", name, h)
		}
	}
	if h := view.Rollup.Capacity.Headroom; h.Target != 0.5 || h.CPUCoresFreeAtTarget.Value != 8 {
		t.Errorf("rollup headroom = target %v, %v cores; want 8 at 0.5", h.Target, h.CPUCoresFreeAtTarget.Value)
	}
}
