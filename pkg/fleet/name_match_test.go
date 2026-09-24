package fleet_test

import (
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// namedSnapshot is a healthy snapshot from a node that reports nodeName as the
// name it writes its history under. An empty nodeName is a node whose build
// predates the field.
func namedSnapshot(host, nodeName string) *types.Snapshot {
	snap := healthySnapshot(host)
	snap.NodeName = nodeName
	return snap
}

// namedPoller is a poller for one peer, held under alias, whose node answers
// with remote. The aggregator itself is "local" and says so.
func namedPoller(t *testing.T, alias string, remote *types.Snapshot) *fleet.Poller {
	t.Helper()
	srv := peerServer(t, remote)
	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: alias, URL: srv.URL}},
		PollIntervalMs: 10000, TimeoutMs: 2000,
	}
	local := namedSnapshot("local", "local")
	return fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())
}

// TestView_ANodeThatAgreesWithItsAliasIsSame is the ordinary fleet: every peer
// is named here exactly as it names itself.
func TestView_ANodeThatAgreesWithItsAliasIsSame(t *testing.T) {
	p := namedPoller(t, "db-01", namedSnapshot("db-01", "db-01"))

	view := pollUntilReachable(t, p, "db-01")

	peer := node(t, view, "db-01")
	if peer.NameMatch != fleet.NameSame {
		t.Errorf("NameMatch = %q, want %q", peer.NameMatch, fleet.NameSame)
	}
	if peer.NodeName != "db-01" {
		t.Errorf("NodeName = %q, want db-01", peer.NodeName)
	}
	if self := node(t, view, "local"); self.NameMatch != fleet.NameSame {
		t.Errorf("self NameMatch = %q, want %q -- a node always agrees with itself",
			self.NameMatch, fleet.NameSame)
	}
}

// TestView_AnAliasTheNodeDoesNotAnswerToIsDifferent is the defect in #162: a
// peer held as `database` that writes its history as `db-01`. One node now has
// two history identities, so the view must say so and carry both names rather
// than leave an empty chart to explain itself.
func TestView_AnAliasTheNodeDoesNotAnswerToIsDifferent(t *testing.T) {
	p := namedPoller(t, "database", namedSnapshot("db-01", "db-01"))

	view := pollUntilReachable(t, p, "database")

	peer := node(t, view, "database")
	if peer.NameMatch != fleet.NameDifferent {
		t.Errorf("NameMatch = %q, want %q", peer.NameMatch, fleet.NameDifferent)
	}
	if peer.NodeName != "db-01" {
		t.Errorf("NodeName = %q, want db-01 -- the warning has to name it", peer.NodeName)
	}
	// A naming mistake is a configuration fact, not a fault on the node.
	if peer.Status != fleet.StatusHealthy {
		t.Errorf("status = %q, want healthy -- a wrong alias is not a node fault", peer.Status)
	}
}

// TestView_ANodeThatDoesNotStateItsNameIsUnknown is the older peer: its build
// predates node_name and sends nothing. That silence has not agreed with the
// alias, so it must not read as same (the rule #68 set for build_match).
func TestView_ANodeThatDoesNotStateItsNameIsUnknown(t *testing.T) {
	p := namedPoller(t, "db-01", namedSnapshot("db-01", ""))

	view := pollUntilReachable(t, p, "db-01")

	peer := node(t, view, "db-01")
	if peer.NameMatch != fleet.NameUnknown {
		t.Errorf("NameMatch = %q, want %q for a peer that did not state its name",
			peer.NameMatch, fleet.NameUnknown)
	}
	if peer.NodeName != "" {
		t.Errorf("NodeName = %q, want empty", peer.NodeName)
	}
}

// TestView_AnUnreachableNodeHasNoNameAgreement covers the paths that never
// reach a live answer: the peer not yet polled and the peer that refuses the
// connection. Both carry an explicit "unknown" rather than an empty field.
func TestView_AnUnreachableNodeHasNoNameAgreement(t *testing.T) {
	cfg := config.Fleet{
		// Port 1 on loopback refuses connections immediately.
		Peers:          []config.Peer{{Name: "dead", URL: "http://127.0.0.1:1"}},
		PollIntervalMs: 10000, TimeoutMs: 500,
	}
	local := namedSnapshot("local", "local")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	if got := node(t, p.View(fleet.Query{}), "dead").NameMatch; got != fleet.NameUnknown {
		t.Errorf("never-polled NameMatch = %q, want %q", got, fleet.NameUnknown)
	}

	done := make(chan struct{})
	go p.Run(done)
	waitFor(t, func() bool {
		return node(t, p.View(fleet.Query{}), "dead").Status == fleet.StatusUnreachable
	})
	close(done)

	if got := node(t, p.View(fleet.Query{}), "dead").NameMatch; got != fleet.NameUnknown {
		t.Errorf("unreachable NameMatch = %q, want %q", got, fleet.NameUnknown)
	}
}

// TestView_AStalePeerLosesItsNameAgreement is a peer that answered once and
// then stopped. Its last reading is kept for the card, but whether its alias
// still matches is a question about the node as it is now, which nobody has
// asked, so the answer is unknown rather than the last one heard.
func TestView_AStalePeerLosesItsNameAgreement(t *testing.T) {
	srv := peerServer(t, namedSnapshot("db-01", "db-01"))
	cfg := config.Fleet{
		Peers: []config.Peer{{Name: "db-01", URL: srv.URL}},
		// The floor, so the poll after the peer goes away comes within the
		// wait below.
		PollIntervalMs: int(fleet.MinPollInterval.Milliseconds()), TimeoutMs: 500,
	}
	local := namedSnapshot("local", "local")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, local, 0 }, func() string { return "local" }, quiet())

	// One Run throughout: the poller runs a single loop at a time.
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		p.Run(done)
	}()
	defer func() {
		close(done)
		<-stopped
	}()

	waitFor(t, func() bool { return node(t, p.View(fleet.Query{}), "db-01").Reachable })
	if got := node(t, p.View(fleet.Query{}), "db-01").NameMatch; got != fleet.NameSame {
		t.Fatalf("NameMatch = %q, want %q before the peer goes away", got, fleet.NameSame)
	}

	srv.Close()
	waitFor(t, func() bool { return !node(t, p.View(fleet.Query{}), "db-01").Reachable })

	if got := node(t, p.View(fleet.Query{}), "db-01").NameMatch; got != fleet.NameUnknown {
		t.Errorf("stale NameMatch = %q, want %q", got, fleet.NameUnknown)
	}
}

// TestView_ThisNodeAgreesWithItselfAcrossARename is a reload of
// fleet.local_name. The snapshot this node collected before the reload still
// carries the old name until the next collection, which may be a minute away,
// but its fleet name and its history label both follow the new one at once --
// so its own card must not warn it that it disagrees with itself.
func TestView_ThisNodeAgreesWithItselfAcrossARename(t *testing.T) {
	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "db-01", URL: "http://127.0.0.1:1"}},
		PollIntervalMs: 10000, TimeoutMs: 500,
	}
	stale := namedSnapshot("host", "old-name")
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, stale, 0 }, func() string { return "new-name" }, quiet())

	self := node(t, p.View(fleet.Query{}), "new-name")
	if self.NameMatch != fleet.NameSame {
		t.Errorf("self NameMatch = %q, want %q", self.NameMatch, fleet.NameSame)
	}
	if self.NodeName != "new-name" {
		t.Errorf("self NodeName = %q, want new-name", self.NodeName)
	}
}

// TestView_ThisNodeWithNoSnapshotIsUnknown is the bad-input case for the node
// itself: with no reading of its own it is shown unreachable, and nothing about
// it -- its name included -- has been confirmed.
func TestView_ThisNodeWithNoSnapshotIsUnknown(t *testing.T) {
	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "db-01", URL: "http://127.0.0.1:1"}},
		PollIntervalMs: 10000, TimeoutMs: 500,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 }, func() string { return "local" }, quiet())

	if got := node(t, p.View(fleet.Query{}), "local").NameMatch; got != fleet.NameUnknown {
		t.Errorf("self NameMatch = %q, want %q with no snapshot", got, fleet.NameUnknown)
	}
}

// TestView_NameComesFromTheSummaryAndTheLegacyFallback is the rollout #181
// made real: a current peer is read by its summary alone, and an older one by
// its whole snapshot. Both must carry the node's own name to the comparison,
// and the current peer must not be asked for its snapshot to get it.
func TestView_NameComesFromTheSummaryAndTheLegacyFallback(t *testing.T) {
	modern := &peer{}
	modern.set(nil, namedSnapshot("db-01", "db-01"))
	old := &peer{legacy: true}
	old.set(nil, namedSnapshot("db-02", "db-02"))
	a, b := startPeer(t, modern), startPeer(t, old)

	cfg := config.Fleet{
		Peers:     []config.Peer{{Name: "database", URL: a.URL}, {Name: "db-02", URL: b.URL}},
		TimeoutMs: 2000,
	}
	p := fleet.New(func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			return nil, namedSnapshot("local", "local"), 0
		},
		func() string { return "local" }, quiet())
	view := pollUntilReachable(t, p, "database", "db-02")

	viaSummary := node(t, view, "database")
	if viaSummary.Source != types.SummarySourceSummary {
		t.Fatalf("database Source = %q, want it read by its summary", viaSummary.Source)
	}
	if viaSummary.NodeName != "db-01" || viaSummary.NameMatch != fleet.NameDifferent {
		t.Errorf("via summary: NodeName %q NameMatch %q, want db-01 / %q",
			viaSummary.NodeName, viaSummary.NameMatch, fleet.NameDifferent)
	}
	if n := modern.count("/api/snapshot"); n != 0 {
		t.Errorf("the summary-serving peer was asked for its snapshot %d times to learn its name", n)
	}

	viaSnapshot := node(t, view, "db-02")
	if viaSnapshot.Source != types.SummarySourceSnapshot {
		t.Fatalf("db-02 Source = %q, want the legacy snapshot fallback", viaSnapshot.Source)
	}
	if viaSnapshot.NodeName != "db-02" || viaSnapshot.NameMatch != fleet.NameSame {
		t.Errorf("via snapshot: NodeName %q NameMatch %q, want db-02 / %q",
			viaSnapshot.NodeName, viaSnapshot.NameMatch, fleet.NameSame)
	}
}

// TestSummarize_CarriesTheNodesName: the summary a node serves states the name
// it writes history under, and one from a build that did not state it says
// nothing rather than inventing one.
func TestSummarize_CarriesTheNodesName(t *testing.T) {
	if got := fleet.Summarize(nil, namedSnapshot("host", "db-01")).NodeName; got != "db-01" {
		t.Errorf("NodeName = %q, want db-01", got)
	}
	if got := fleet.Summarize(nil, namedSnapshot("host", "")).NodeName; got != "" {
		t.Errorf("NodeName = %q for a snapshot with none, want empty -- not the hostname", got)
	}
}
