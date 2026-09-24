package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// What an aggregation cycle costs, measured because it could not be inferred.
//
// #76 found aggregation was most of mon0's CPU, and that most of THAT was
// decoding peers' whole snapshots to fill cards that use a dozen fields. The
// owner's ruling was that peers serve a summary, and that an aggregator
// recording history also fetches a slimmed history payload. These benchmarks
// measure all three shapes against the same busy-host peers:
//
//	snapshot          every peer polled for its whole snapshot: the shape
//	                  before #76, and still the shape for a peer too old to
//	                  serve a summary
//	summary           the fleet view alone: each peer's summary
//	summary+history   an aggregator recording history: summary plus the
//	                  history payload
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

// benchSnapshot builds a peer snapshot of production size; see busyHost.
func benchSnapshot(host string) *types.Snapshot {
	return busyHost(host)
}

// pollShape is what the benchmark's peers serve and the poller asks for.
type pollShape struct {
	name      string
	legacy    bool // peers serve only /api/snapshot
	recording bool // the poller records history
}

var pollShapes = []pollShape{
	{name: "snapshot", legacy: true},
	{name: "summary"},
	{name: "summary+history", recording: true},
}

// benchFleet starts n peers of the given shape and returns a poller configured
// against them, and a counter of the bytes the peers have written.
//
// Each body is encoded once and written verbatim: the peers exist to feed the
// aggregator, and encoding per request would put the peer's CPU inside the
// aggregator's measurement.
func benchFleet(b testing.TB, n int, shape pollShape) (*Poller, config.Fleet, *atomic.Int64) {
	b.Helper()

	snap := benchSnapshot("peer")
	prev := *snap
	prev.Timestamp = snap.Timestamp.Add(-3e9)
	bodies := map[string][]byte{endpointSnapshot: encode(b, snap)}
	if !shape.legacy {
		bodies[endpointSummary] = encode(b, Summarize(&prev, snap))
		bodies[endpointHistory] = encode(b, HistorySnapshot(snap))
	}
	for endpoint, body := range bodies {
		b.Logf("%s: %s is %d bytes", shape.name, endpoint, len(body))
	}

	var written atomic.Int64
	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	for i := range n {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, ok := bodies[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if n, err := w.Write(body); err == nil {
				written.Add(int64(n))
			}
		}))
		b.Cleanup(srv.Close)
		cfg.Peers = append(cfg.Peers, config.Peer{Name: fmt.Sprintf("peer-%02d", i), URL: srv.URL})
	}

	local := benchSnapshot("local")
	p := New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return &prev, local, 0 },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if shape.recording {
		p.Observe(func(string, *types.Snapshot) {}, func() time.Duration { return 15 * time.Second })
	}
	return p, cfg, &written
}

// benchCycle is the unit each shape is measured in: one summary refresh, and
// for a recording aggregator one history reading from every peer -- which is
// what a sample interval costs when it coincides with a poll.
func benchCycle(p *Poller, cfg config.Fleet, shape pollShape) {
	p.refresh(cfg)
	if !shape.recording {
		return
	}
	var wg sync.WaitGroup
	for _, peer := range cfg.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.history.record(context.Background(), peer)
		}()
	}
	wg.Wait()
}

func encode(b testing.TB, v any) []byte {
	b.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		b.Fatalf("encode: %v", err)
	}
	return body
}

// BenchmarkPoller_Refresh is one aggregation cycle: every peer fetched
// concurrently and decoded, in each poll shape.
//
// Its ns/op is WALL time, and the peers are fetched in parallel, so it is
// roughly the slowest round trip and not what the cycle costs a CPU. Do not
// divide it by the poll interval to get a per-second load: doing exactly that
// is how #76 concluded aggregation was too small to explain mon0, which it
// outweighed by more than two orders of magnitude. BenchmarkPoller_RefreshCPU
// reports the CPU figure directly. KB/cycle is what the peers sent.
func BenchmarkPoller_Refresh(b *testing.B) {
	for _, shape := range pollShapes {
		b.Run(shape.name, func(b *testing.B) {
			p, cfg, written := benchFleet(b, benchPeerCount, shape)
			benchCycle(p, cfg, shape)
			written.Store(0)

			cycles := 0
			b.ResetTimer()
			for b.Loop() {
				benchCycle(p, cfg, shape)
				cycles++
			}
			b.ReportMetric(float64(written.Load())/float64(cycles)/1024, "KB/cycle")
		})
	}
}
