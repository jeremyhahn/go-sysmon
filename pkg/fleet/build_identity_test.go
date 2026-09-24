package fleet_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// builtSnapshot is a healthy snapshot from a named build. The release string is
// left as it is on purpose: it is identical on every node in the field, which
// is exactly why the commit has to be carried separately.
func builtSnapshot(host, commit, date string) *types.Snapshot {
	snap := healthySnapshot(host)
	snap.GitCommit = commit
	snap.BuildDate = date
	return snap
}

// node finds one node in a view by name. It fails rather than returning nil so
// a caller never dereferences a miss.
func node(t *testing.T, view types.Fleet, name string) types.FleetNode {
	t.Helper()
	for _, n := range view.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %q missing from the view", name)
	return types.FleetNode{}
}

// pollUntilReachable runs the poller until every named peer has answered once.
func pollUntilReachable(t *testing.T, p *fleet.Poller, names ...string) types.Fleet {
	t.Helper()
	done := make(chan struct{})
	go p.Run(done)
	await.Until(t, "every named peer to answer", func(context.Context) bool {
		view := p.View(fleet.Query{})
		answered := 0
		for _, n := range view.Nodes {
			for _, want := range names {
				if n.Name == want && n.Reachable {
					answered++
				}
			}
		}
		return answered == len(names)
	})
	close(done)
	return p.View(fleet.Query{})
}

// TestView_CarriesPeerBuildIdentity is the defect in #68: the aggregator
// collected only the release string, which every build of this project has
// reported as the same word, so a peer two builds behind rendered exactly like
// a current one. The commit and build date the peer already knew about itself
// must reach the fleet payload.
func TestView_CarriesPeerBuildIdentity(t *testing.T) {
	remote := builtSnapshot("omega", "73d0de5", "2026-09-03T18:10:08Z")
	srv := peerServer(t, remote)

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	local := builtSnapshot("local", "a46f6cf", "2026-09-05T03:26:50Z")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	view := pollUntilReachable(t, p, "omega")

	peer := node(t, view, "omega")
	if peer.GitCommit != "73d0de5" || peer.BuildDate != "2026-09-03T18:10:08Z" {
		t.Errorf("peer build = %q/%q, want 73d0de5/2026-09-03T18:10:08Z",
			peer.GitCommit, peer.BuildDate)
	}
	if peer.Version != remote.Version {
		t.Errorf("Version = %q, want the release string %q still carried",
			peer.Version, remote.Version)
	}
	if peer.BuildMatch != types.BuildDifferent {
		t.Errorf("BuildMatch = %q, want %q for a peer on another commit",
			peer.BuildMatch, types.BuildDifferent)
	}

	self := node(t, view, "local")
	if self.GitCommit != "a46f6cf" {
		t.Errorf("self GitCommit = %q, want the aggregator's own commit", self.GitCommit)
	}
	if self.BuildMatch != types.BuildSame {
		t.Errorf("self BuildMatch = %q, want %q -- a node always matches itself",
			self.BuildMatch, types.BuildSame)
	}
}

// TestView_MatchingCommitIsNotDivergent is the other half: an up-to-date fleet
// must not report drift, or the signal is worth nothing.
func TestView_MatchingCommitIsNotDivergent(t *testing.T) {
	srv := peerServer(t, builtSnapshot("omega", "a46f6cf", "2026-09-05T03:26:50Z"))

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	local := builtSnapshot("local", "a46f6cf", "2026-09-05T03:26:50Z")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	peer := node(t, pollUntilReachable(t, p, "omega"), "omega")
	if peer.BuildMatch != types.BuildSame {
		t.Errorf("BuildMatch = %q, want %q for a peer on the aggregator's commit",
			peer.BuildMatch, types.BuildSame)
	}
}

// TestView_DivergenceIsNotAFault holds the design decision the issue asked for
// explicitly: a node on another build is VISIBLE, not degraded. Mid-rollout
// every node disagrees with the aggregator for as long as the rollout takes,
// and a fleet that went amber on every deploy would teach operators to ignore
// the colour.
func TestView_DivergenceIsNotAFault(t *testing.T) {
	srv := peerServer(t, builtSnapshot("omega", "73d0de5", "2026-09-03T18:10:08Z"))

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	local := builtSnapshot("local", "a46f6cf", "2026-09-05T03:26:50Z")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	peer := node(t, pollUntilReachable(t, p, "omega"), "omega")
	if peer.BuildMatch != types.BuildDifferent {
		t.Fatalf("BuildMatch = %q, want the divergence this test is about",
			peer.BuildMatch)
	}
	if peer.Status != types.NodeStatusHealthy {
		t.Errorf("status = %q, want healthy -- an old build is not a fault", peer.Status)
	}
	if peer.WarningCount != 0 || peer.CriticalCount != 0 {
		t.Errorf("counts = %d warning, %d critical; divergence must not raise an alert",
			peer.WarningCount, peer.CriticalCount)
	}
}

// TestView_UnstatedBuildIsUnknownNotAgreement is the bad-input case, and it is
// the one that matters most: a peer whose binary predates these fields sends
// nothing, and a peer built without the -ldflags sends the literal "unknown".
// Reading either silence as agreement would put #68's defect back in a new
// field -- the stale node would once again look current.
func TestView_UnstatedBuildIsUnknownNotAgreement(t *testing.T) {
	// One case reports nothing at all (an older build); the other reports the
	// placeholder a local build produces.
	silent := peerServer(t, builtSnapshot("silent", "", ""))
	placeholder := peerServer(t, builtSnapshot("placeholder", "unknown", "unknown"))

	cfg := config.Fleet{
		Peers: []config.Peer{
			{Name: "silent", URL: silent.URL},
			{Name: "placeholder", URL: placeholder.URL},
		},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	local := builtSnapshot("local", "a46f6cf", "2026-09-05T03:26:50Z")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	view := pollUntilReachable(t, p, "silent", "placeholder")
	for _, name := range []string{"silent", "placeholder"} {
		if got := node(t, view, name).BuildMatch; got != types.BuildUnknown {
			t.Errorf("%s BuildMatch = %q, want %q -- a node that did not state "+
				"its build has not agreed with anything", name, got, types.BuildUnknown)
		}
	}
}

// TestView_AnUnstatedAggregatorKnowsNothing is the mirror of the case above.
// A dev build of the aggregator has no commit to compare against, so it must
// report every peer as unknown rather than declaring the whole fleet divergent
// from a build it cannot name.
func TestView_AnUnstatedAggregatorKnowsNothing(t *testing.T) {
	srv := peerServer(t, builtSnapshot("omega", "73d0de5", "2026-09-03T18:10:08Z"))

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "omega", URL: srv.URL}},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	local := builtSnapshot("local", "unknown", "unknown")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	view := pollUntilReachable(t, p, "omega")
	if got := node(t, view, "omega").BuildMatch; got != types.BuildUnknown {
		t.Errorf("BuildMatch = %q, want %q when the aggregator cannot name its own build",
			got, types.BuildUnknown)
	}
	if got := node(t, view, "local").BuildMatch; got != types.BuildUnknown {
		t.Errorf("self BuildMatch = %q, want %q -- it cannot name its own build either",
			got, types.BuildUnknown)
	}
}

// TestView_ASilentNodeHasNoBuildAgreement covers the two paths that never reach
// summarize: a peer that has not been polled yet and one that answered nothing.
// Both must carry an explicit "unknown" rather than the empty string a
// forgotten field would leave, which decodes as neither agreement nor its
// absence.
func TestView_ASilentNodeHasNoBuildAgreement(t *testing.T) {
	cfg := config.Fleet{
		// Port 1 on loopback refuses connections immediately.
		Peers:          []config.Peer{{Name: "dead", URL: "http://127.0.0.1:1"}},
		PollIntervalMs: 10000, TimeoutMs: 500,
	}
	local := builtSnapshot("local", "a46f6cf", "2026-09-05T03:26:50Z")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	// Before any poll has run at all.
	if got := node(t, p.View(fleet.Query{}), "dead").BuildMatch; got != types.BuildUnknown {
		t.Errorf("never-polled BuildMatch = %q, want %q", got, types.BuildUnknown)
	}

	done := make(chan struct{})
	go p.Run(done)
	await.Until(t, "the dead peer to be polled as unreachable", func(context.Context) bool {
		return node(t, p.View(fleet.Query{}), "dead").Status == types.NodeStatusUnreachable
	})
	close(done)

	dead := node(t, p.View(fleet.Query{}), "dead")
	if dead.BuildMatch != types.BuildUnknown {
		t.Errorf("unreachable BuildMatch = %q, want %q", dead.BuildMatch, types.BuildUnknown)
	}
	if dead.GitCommit != "" {
		t.Errorf("GitCommit = %q, want empty for a node that never answered", dead.GitCommit)
	}
}
