//go:build unix

package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Ten thousand nodes as a tree (#190): a root polling treeChildren child
// aggregators, each polling treeLeaves leaves. Measured per aggregator, since
// the point of the tree is that no aggregator carries the whole fleet:
//
//	root    polls 100 children: summary + subtree index each cycle, and
//	        records 100 peers' history
//	child   polls 100 leaves, and records 100 peers' history -- the flat
//	        100-peer shape BenchmarkPoller_RefreshCPU already measures
//
// Against one flat aggregator polling all 10,000 (#15's measurement: about
// 0.77 s of CPU a cycle, and about 1 GB of history payload a sample).
//
//	make bench-fleet
const (
	treeChildren = 100
	treeLeaves   = 100
)

// treeFixture starts treeChildren children serving what a real child
// aggregator of treeLeaves busy leaves serves, and a root polling them.
func treeFixture(b *testing.B) (*Poller, config.Fleet, *atomic.Int64, *atomic.Int64) {
	b.Helper()
	snap := busyHost("leaf")
	prev := *snap
	prev.Timestamp = snap.Timestamp.Add(-3 * time.Second)
	summary := Summarize(&prev, snap)

	var written, pageFetches atomic.Int64
	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	for c := range treeChildren {
		child := fmt.Sprintf("region-%03d", c)
		var entries []types.FleetIndexEntry
		var nodes []types.FleetNode
		entries = append(entries, types.FleetIndexEntry{Name: child, Hostname: child, Status: types.NodeStatusHealthy, Reachable: true, AgeSeconds: 1})
		self := types.FleetNode{Name: child, Reachable: true, Status: types.NodeStatusHealthy, NodeSummary: summary}
		nodes = append(nodes, self)
		byName := map[string]types.FleetNode{}
		for l := range treeLeaves {
			name := fmt.Sprintf("node-%03d-%03d", c, l)
			s := summary
			s.Hostname = name
			n := types.FleetNode{Name: name, Reachable: true, AgeSeconds: 1, NodeSummary: s}
			n.Status = nodeStatus(true, &n.NodeSummary)
			nodes = append(nodes, n)
			byName[name] = n
			entries = append(entries, types.FleetIndexEntry{Name: name, Hostname: name, Status: n.Status, Reachable: true, AgeSeconds: 1, Depth: 1})
		}
		sub := types.FleetSubtree{Nodes: entries, Rollup: collect(nodes, types.RollupNames, 0.8), Aggregators: []string{newInstanceID()}}
		childSummary := summary
		childSummary.Hostname = child
		childSummary.Aggregates = treeLeaves
		bodies := map[string][]byte{
			endpointSummary: mustJSON(b, childSummary),
			endpointSubtree: mustJSON(b, sub),
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body []byte
			if r.URL.Path == endpointNodes {
				pageFetches.Add(1)
				out := types.FleetNodes{}
				for _, name := range r.URL.Query()[QueryParamName] {
					out.Nodes = append(out.Nodes, byName[name])
				}
				body = mustJSON(b, out)
			} else if body = bodies[r.URL.Path]; body == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if n, err := w.Write(body); err == nil {
				written.Add(int64(n))
			}
		}))
		b.Cleanup(srv.Close)
		cfg.Peers = append(cfg.Peers, config.Peer{Name: child, URL: srv.URL})
	}
	local := busyHost("root")
	p := New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return &prev, local, time.Second },
		func() string { return "root" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return p, cfg, &written, &pageFetches
}

func mustJSON(b testing.TB, v any) []byte {
	b.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		b.Fatalf("encode: %v", err)
	}
	return body
}

// BenchmarkTree_RootCycle is one root aggregation cycle over the 10,000-node
// tree: every child's summary and subtree index, in CPU and bytes. history
// KB/sample is what the root records: its 100 direct peers' history payload,
// the same as any 100-peer aggregator's.
func BenchmarkTree_RootCycle(b *testing.B) {
	p, cfg, written, _ := treeFixture(b)
	p.refresh(cfg)
	written.Store(0)
	historyBytes := len(mustJSON(b, HistorySnapshot(busyHost("peer"))))

	cycles := 0
	start := processCPU(b)
	b.ResetTimer()
	for b.Loop() {
		p.refresh(cfg)
		cycles++
	}
	b.StopTimer()
	used := processCPU(b) - start
	b.ReportMetric(float64(used)/float64(cycles)/float64(time.Millisecond), "ms_cpu/op")
	b.ReportMetric(float64(written.Load())/float64(cycles)/1024, "KB/cycle")
	b.ReportMetric(float64(treeChildren*historyBytes)/1024, "history_KB/sample")
	if v := p.View(b.Context(), Query{Limit: 1}); v.Page.Total != treeChildren*(treeLeaves+1)+1 {
		b.Fatalf("root sees %d nodes, want %d", v.Page.Total, treeChildren*(treeLeaves+1)+1)
	}
}

// BenchmarkTree_RootPage is a dashboard request to the root for one 25-node
// page of the 10,000-node tree: the index search, sort and page, the composed
// roll-up, and the page's summaries -- cold, fetched from the child that owns
// them, and warm, reused within the poll interval.
func BenchmarkTree_RootPage(b *testing.B) {
	for _, shape := range []string{"warm", "cold"} {
		b.Run(shape, func(b *testing.B) {
			p, cfg, _, fetches := treeFixture(b)
			p.refresh(cfg)
			q := Query{Offset: 5000, Limit: 25}
			p.View(b.Context(), q)
			fetches.Store(0)

			var size, ops int
			start := processCPU(b)
			b.ResetTimer()
			for b.Loop() {
				if shape == "cold" {
					p.pageCache.Clear()
				}
				body, err := json.Marshal(p.View(b.Context(), q))
				if err != nil {
					b.Fatalf("encode: %v", err)
				}
				size = len(body)
				ops++
			}
			b.StopTimer()
			used := processCPU(b) - start
			b.ReportMetric(float64(size), "bytes/response")
			b.ReportMetric(float64(used)/float64(ops)/float64(time.Millisecond), "ms_cpu/op")
			b.ReportMetric(float64(fetches.Load())/float64(ops), "child_fetches/op")
			if !strings.Contains(string(mustJSON(b, p.View(b.Context(), q).Nodes[0])), `"via":"region-`) {
				b.Fatal("the page did not come from the children")
			}
		})
	}
}
