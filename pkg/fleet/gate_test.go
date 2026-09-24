package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// gateCall is what the history gate was asked about.
type gateCall struct {
	peer     string
	summary  *types.NodeSummary
	source   types.SummarySource
	interval int64
}

// recordingGate returns a gate that answers with record and status, and the
// calls it received.
func recordingGate(record bool, status types.PeerHistory) (HistoryGate, func() []gateCall) {
	var mu sync.Mutex
	var calls []gateCall
	gate := func(peer string, summary *types.NodeSummary, source types.SummarySource) (bool, types.PeerHistory) {
		mu.Lock()
		defer mu.Unlock()
		c := gateCall{peer: peer, summary: summary, source: source}
		if summary != nil {
			c.interval = summary.CollectionIntervalMs
		}
		calls = append(calls, c)
		return record, status
	}
	return gate, func() []gateCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]gateCall(nil), calls...)
	}
}

// summaryPeer serves a current build's summary, reporting interval as its
// collection interval, and its history payload.
func summaryPeer(t *testing.T, interval time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch r.URL.Path {
		case endpointSummary:
			s := Summarize(nil, healthyPeerSnapshot())
			s.CollectionIntervalMs = interval.Milliseconds()
			body = s
		case endpointHistory:
			body = HistorySnapshot(healthyPeerSnapshot())
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGateHistory_ARefusedPeerIsNotWrittenAndSaysWhy. The gate is asked with
// the peer's own summary -- its reported collection interval -- and a refusal
// keeps the reading out of the history and puts the reason on the card.
func TestGateHistory_ARefusedPeerIsNotWrittenAndSaysWhy(t *testing.T) {
	srv := summaryPeer(t, 10*time.Minute)
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	refused := types.PeerHistory{State: types.PeerHistoryRefused, Reason: "collects every 10m0s"}
	gate, calls := recordingGate(false, refused)
	p.GateHistory(gate)

	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])

	if n := readings.Load(); n != 0 {
		t.Errorf("%d readings written for a refused peer", n)
	}
	got := calls()
	if len(got) != 1 || got[0].peer != "peer-00" || got[0].source != types.SummarySourceSummary ||
		got[0].interval != (10*time.Minute).Milliseconds() {
		t.Fatalf("gate calls = %+v, want one for peer-00 with its summary and reported interval", got)
	}
	n := p.View(context.Background(), Query{}).Nodes[1]
	if n.History == nil || *n.History != refused {
		t.Fatalf("card history = %+v, want the refusal", n.History)
	}
}

// TestGateHistory_ARecordedPeerIsWritten.
func TestGateHistory_ARecordedPeerIsWritten(t *testing.T) {
	srv := summaryPeer(t, time.Second)
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	gate, _ := recordingGate(true, types.PeerHistory{State: types.PeerHistoryRecorded})
	p.GateHistory(gate)

	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])

	if n := readings.Load(); n != 1 {
		t.Errorf("%d readings written, want 1", n)
	}
	if h := p.View(context.Background(), Query{}).Nodes[1].History; h == nil || h.State != types.PeerHistoryRecorded {
		t.Errorf("card history = %+v, want recorded", h)
	}
}

// TestGateHistory_AnOldPeerIsJudgedByItsSnapshot. A peer too old to serve a
// summary is relayed by the snapshot poll, and the gate is told so -- with a
// summary computed here, which carries no collection interval.
func TestGateHistory_AnOldPeerIsJudgedByItsSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != endpointSnapshot {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(healthyPeerSnapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	unverified := types.PeerHistory{State: types.PeerHistoryUnverified, Reason: "no collection interval"}
	gate, calls := recordingGate(true, unverified)
	p.GateHistory(gate)

	p.refresh(*cfg.Load())

	got := calls()
	if len(got) != 1 || got[0].source != types.SummarySourceSnapshot || got[0].summary == nil || got[0].interval != 0 {
		t.Fatalf("gate calls = %+v, want the snapshot source and no reported interval", got)
	}
	if readings.Load() != 1 {
		t.Errorf("%d readings, want the unverified one written", readings.Load())
	}
	if h := p.View(context.Background(), Query{}).Nodes[1].History; h == nil || *h != unverified {
		t.Errorf("card history = %+v, want unverified", h)
	}
}

// TestGateHistory_NoStatusWhileNotRecording. A decision made while history was
// recorded says nothing about a fleet that records none: the card carries no
// history status then.
func TestGateHistory_NoStatusWhileNotRecording(t *testing.T) {
	srv := summaryPeer(t, 10*time.Minute)
	cfg := fleetOf(5*time.Second, srv.URL)
	cadence := cadenceOf(time.Hour)
	p, _ := recordingPoller(t, cfg, cadence)
	gate, _ := recordingGate(false, types.PeerHistory{State: types.PeerHistoryRefused, Reason: "x"})
	p.GateHistory(gate)
	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])

	cadence.Store(0)
	if h := p.View(context.Background(), Query{}).Nodes[1].History; h != nil {
		t.Errorf("card history with recording off = %+v, want none", h)
	}
}

// TestGateHistory_NilRecordsEverything. Without a gate every reading is
// written, as before the gate existed.
func TestGateHistory_NilRecordsEverything(t *testing.T) {
	srv := summaryPeer(t, 10*time.Minute)
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	gate, _ := recordingGate(false, types.PeerHistory{State: types.PeerHistoryRefused})
	p.GateHistory(gate)
	p.GateHistory(nil)
	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])
	if readings.Load() != 1 {
		t.Errorf("%d readings with no gate, want 1", readings.Load())
	}
}

// TestGateHistory_ARefusedPeerIsNotFetched. The gate is asked before the
// history payload is fetched, from the summary already held: a refused peer
// costs neither its payload nor one of the fleet's fetch slots.
func TestGateHistory_ARefusedPeerIsNotFetched(t *testing.T) {
	var fetched atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch r.URL.Path {
		case endpointSummary:
			s := Summarize(nil, healthyPeerSnapshot())
			s.CollectionIntervalMs = (10 * time.Minute).Milliseconds()
			body = s
		case endpointHistory:
			fetched.Add(1)
			body = HistorySnapshot(healthyPeerSnapshot())
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cfg := fleetOf(5*time.Second, srv.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(time.Hour))
	gate, _ := recordingGate(false, types.PeerHistory{State: types.PeerHistoryRefused, Reason: "slow"})
	p.GateHistory(gate)

	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])

	if n := fetched.Load(); n != 0 {
		t.Errorf("the history payload of a refused peer was fetched %d times", n)
	}
	if h := p.View(context.Background(), Query{}).Nodes[1].History; h == nil || h.State != types.PeerHistoryRefused {
		t.Errorf("card history = %+v, want the refusal recorded before any fetch", h)
	}
}

// TestGateHistory_ARepointedPeerDropsTheOldDecision. A reload that points a
// peer's name at another machine must not label that machine with the decision
// made about the last one.
func TestGateHistory_ARepointedPeerDropsTheOldDecision(t *testing.T) {
	first := summaryPeer(t, 10*time.Minute)
	second := summaryPeer(t, time.Second)
	cfg := fleetOf(5*time.Second, first.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(time.Hour))
	gate, _ := recordingGate(false, types.PeerHistory{State: types.PeerHistoryRefused, Reason: "slow"})
	p.GateHistory(gate)
	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])
	if h := p.View(context.Background(), Query{}).Nodes[1].History; h == nil {
		t.Fatal("no decision recorded for the first machine")
	}

	repointed := *cfg.Load()
	repointed.Peers = []config.Peer{{Name: repointed.Peers[0].Name, URL: second.URL}}
	cfg.Store(&repointed)
	if h := p.View(context.Background(), Query{}).Nodes[1].History; h != nil {
		t.Errorf("card history after a repoint = %+v, want none until the new machine is judged", h)
	}
}

// TestGateHistory_ARemovedPeerIsForgotten. A peer removed by a reload takes
// its decision with it -- including when the last peer goes -- so a name added
// back is judged afresh rather than shown under its old verdict.
func TestGateHistory_ARemovedPeerIsForgotten(t *testing.T) {
	srv := summaryPeer(t, 10*time.Minute)
	other := summaryPeer(t, time.Second)
	cfg := fleetOf(5*time.Second, srv.URL, other.URL)
	p, _ := recordingPoller(t, cfg, cadenceOf(time.Hour))
	gate, _ := recordingGate(false, types.PeerHistory{State: types.PeerHistoryRefused, Reason: "slow"})
	p.GateHistory(gate)
	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])

	full := *cfg.Load()
	without := full
	without.Peers = full.Peers[1:]
	p.cycle(without)
	if _, ok := p.historyStatus.Load(full.Peers[0].Name); ok {
		t.Error("a removed peer's decision was kept")
	}

	p.historyStatus.Store(full.Peers[1].Name, historyDecision{url: full.Peers[1].URL})
	p.cycle(config.Fleet{})
	if _, ok := p.historyStatus.Load(full.Peers[1].Name); ok {
		t.Error("the last peer's decision was kept after every peer was removed")
	}
}

// TestGateHistory_AskedAgainBeforeDelivery. The approval given before a fetch
// can be stale by its end -- the peer re-summarised, the database changed --
// so the gate is asked again, and a refusal then keeps the reading out.
func TestGateHistory_AskedAgainBeforeDelivery(t *testing.T) {
	srv := summaryPeer(t, time.Second)
	cfg := fleetOf(5*time.Second, srv.URL)
	p, readings := recordingPoller(t, cfg, cadenceOf(time.Hour))
	var asked atomic.Int64
	p.GateHistory(func(string, *types.NodeSummary, types.SummarySource) (bool, types.PeerHistory) {
		// Admitted before the fetch, refused after it.
		if asked.Add(1) == 1 {
			return true, types.PeerHistory{State: types.PeerHistoryRecorded}
		}
		return false, types.PeerHistory{State: types.PeerHistoryRefused, Reason: "changed"}
	})
	p.refresh(*cfg.Load())
	p.history.record(context.Background(), cfg.Load().Peers[0])
	if asked.Load() != 2 || readings.Load() != 0 {
		t.Errorf("gate asked %d times, %d readings; want asked before and after the fetch, and nothing written",
			asked.Load(), readings.Load())
	}
}
