package fleet

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// largeFleet returns a poller whose cache already holds n peers, as a refresh
// against n real peers would have left it, without standing up n servers.
//
// The fleet view is served from that cache, so this is exactly what a
// dashboard request reads at fleet scale. The mix is deliberate: every tenth
// peer unreachable with a stale reading, every seventh raising warnings, the
// rest healthy -- so the roll-up's attention list and build groups are as
// large as they get in a real fleet rather than empty.
func largeFleet(tb testing.TB, n int) *Poller {
	tb.Helper()

	snap := busyHost("peer")
	prev := *snap
	prev.Timestamp = snap.Timestamp.Add(-3 * time.Second)
	warning := Summarize(&prev, snap)
	healthy := warning
	healthy.Alerts = []types.Alert{}
	healthy.WarningCount = 0

	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	now := time.Now()
	p := New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return &prev, snap, 3 * time.Second },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	for i := range n {
		peer := config.Peer{Name: fmt.Sprintf("node-%05d", i), URL: fmt.Sprintf("https://10.%d.%d.%d:8081", i>>16&255, i>>8&255, i&255)}
		cfg.Peers = append(cfg.Peers, peer)

		s := healthy
		if i%7 == 0 {
			s = warning
		}
		s.Hostname = peer.Name
		// Two builds, as mid-rollout: a third of the fleet already updated.
		if i%3 == 0 {
			s.GitCommit = "b57e0d1"
		}
		st := &peerState{url: peer.URL, summary: &s, source: types.SummarySourceSummary, fetchedAt: now, polledAt: now}
		if i%10 == 0 {
			st.err = "context deadline exceeded"
			st.fetchedAt = now.Add(-time.Duration(i) * time.Second)
		}
		p.peers[peer.Name] = st
	}
	return p
}
