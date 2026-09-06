package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// What an aggregation cycle costs, measured because it could not be inferred.
//
// The collector work merged in #73 halved a process walk and did nothing at all
// for mon0, whose CPU went 220 ms/s to 228 across the deploy. mon0 has 115
// processes against ci-node-1's 1410, so there is barely a process table for
// that saving to apply to -- and mon0 is the only node running this path.
//
// These are internal so they can call refresh directly: one aggregation cycle
// is the unit of work, and driving it through Run would measure the ticker.
//
//	make bench-fleet

// benchPeerCount matches the deployed fleet: mon0 polls ten peers plus itself.
// The tenth (gitea) was added 2026-09-06; the count is read from mon0's own
// sysmon.yaml rather than assumed, because it is the multiplier on everything
// below.
const benchPeerCount = 10

// benchProcesses gives each peer's snapshot a realistic body. The fleet's
// snapshots run 223 KB to 488 KB, almost all of it the process list, so a
// fixture with a handful of processes would measure the HTTP round trip and
// miss the decode entirely -- which is the part suspected of costing.
const benchProcesses = 1000

// benchSnapshot builds a peer snapshot of production size.
func benchSnapshot(host string) *types.Snapshot {
	list := make([]types.ProcessDetail, benchProcesses)
	for i := range list {
		list[i] = types.ProcessDetail{
			PID:            int32(i + 1),
			Name:           fmt.Sprintf("process-%05d", i),
			Username:       "sysmon",
			CPUPercent:     1.5,
			MemoryBytes:    1 << 20,
			ReadBytesRate:  4096,
			WriteBytesRate: 4096,
			Priority:       20,
			Status:         "sleeping",
		}
	}
	return &types.Snapshot{
		Version:    "0.1.0-alpha",
		Host:       types.HostInfo{Hostname: host, Uptime: 3600},
		CPUs:       make([]types.CPUInfo, 24),
		CPUSummary: types.CPUSummary{TotalThreads: 24},
		Memory:     types.MemoryInfo{UsedPercent: 40},
		Disks:      []types.DiskInfo{{Name: "sda", TotalBytes: 1 << 40, UsedPercent: 55}},
		Networks:   []types.NetworkInfo{{Name: "eth0"}},
		Processes:  types.ProcessSummary{Total: benchProcesses, Processes: list},
	}
}

// benchFleet starts n peers serving a pre-encoded snapshot and returns a poller
// configured against them.
//
// The body is encoded once and written verbatim: the peers exist to feed the
// aggregator, and encoding per request would put the peer's CPU inside the
// aggregator's measurement.
func benchFleet(b *testing.B, n int) (*Poller, config.Fleet) {
	b.Helper()

	body, err := json.Marshal(benchSnapshot("peer"))
	if err != nil {
		b.Fatalf("encode peer snapshot: %v", err)
	}
	b.Logf("peer snapshot is %d bytes", len(body))

	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	for i := range n {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/snapshot" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(body); err != nil {
				return
			}
		}))
		b.Cleanup(srv.Close)
		cfg.Peers = append(cfg.Peers, config.Peer{Name: fmt.Sprintf("peer-%02d", i), URL: srv.URL})
	}

	local := benchSnapshot("local")
	p := New(
		func() config.Fleet { return cfg },
		func() *types.Snapshot { return local },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return p, cfg
}

// BenchmarkPoller_Refresh is one aggregation cycle: every peer fetched
// concurrently and decoded.
//
// Its ns/op is WALL time, and the peers are fetched in parallel, so it is
// roughly the slowest round trip and not what the cycle costs a CPU. Do not
// divide it by the poll interval to get a per-second load: doing exactly that
// is how #76 concluded aggregation was too small to explain mon0, which it
// outweighed by more than two orders of magnitude. BenchmarkPoller_RefreshCPU
// reports the CPU figure directly.
func BenchmarkPoller_Refresh(b *testing.B) {
	p, cfg := benchFleet(b, benchPeerCount)

	b.ResetTimer()
	for b.Loop() {
		p.refresh(cfg)
	}
}

// BenchmarkPoller_View is what a dashboard request costs: summarising every
// cached peer snapshot into the fleet payload. It runs per request rather than
// per poll, so a browser left open on the fleet page pays it repeatedly.
func BenchmarkPoller_View(b *testing.B) {
	p, cfg := benchFleet(b, benchPeerCount)
	p.refresh(cfg)

	b.ResetTimer()
	for b.Loop() {
		if v := p.View(); len(v.Nodes) == 0 {
			b.Fatal("View returned no nodes; the benchmark is measuring nothing")
		}
	}
}
