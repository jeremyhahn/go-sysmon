// Package fleet aggregates several go-sysmon nodes into one view.
//
// The aggregation is deliberately server-side. A browser fanning out to each
// node would need CORS on every peer and would break the single-origin model
// that lets the whole dashboard sit behind one authenticating proxy. Here the
// browser talks only to the node it loaded, and that node polls its peers.
//
// A node with no peers configured runs none of this: the fleet view reports
// itself as disabled and the dashboard behaves exactly as a standalone node.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// maxSnapshotBytes bounds a peer response so a misbehaving or hostile peer
// cannot exhaust memory on the aggregator.
const maxSnapshotBytes = 8 << 20 // 8 MiB

// unstatedCommit is what a binary built without the -ldflags reports for its
// commit: main.gitCommit defaults to it, and GET /api/version renders an empty
// value the same way. It says "this build does not know what it is", which is
// the same non-answer as an empty field and is treated as one here.
const unstatedCommit = "unknown"

// compareBuild reports whether a node's commit is the aggregator's own.
//
// A missing commit on either side yields types.BuildUnknown rather than agreement.
// Two nodes that both declined to say what they are running have not been shown
// to match -- and a peer whose build predates the field is precisely the stale
// node this comparison exists to find, so reading its silence as "same" would
// reintroduce the defect in a new field.
func compareBuild(local, node string) types.BuildMatch {
	if local == "" || local == unstatedCommit ||
		node == "" || node == unstatedCommit {
		return types.BuildUnknown
	}
	if local == node {
		return types.BuildSame
	}
	return types.BuildDifferent
}

// peerState is the last result of polling one peer.
type peerState struct {
	// url is the address this state was read from. A reload that repoints
	// the peer's name at another URL makes everything below some other
	// machine's, and it is discarded rather than carried over.
	url string
	// polledAt is when this poll was made, successful or not. fetchedAt
	// dates the data and survives a failed poll; this dates the attempt.
	polledAt  time.Time
	summary   *types.NodeSummary
	source    types.SummarySource
	err       string
	fetchedAt time.Time
	// swapBase is the timestamp and memory of the last whole snapshot, kept
	// only for a peer too old to serve a summary: the aggregator then
	// computes that peer's paging rate itself, by differencing its counters
	// across two polls.
	swapBase *types.Snapshot
	// tree is the peer's subtree, when the peer is itself an aggregator
	// (#190); nil for a leaf.
	tree *childTree
}

// LocalFunc supplies this node's own latest snapshot, cur, the baseline of
// the one it collected immediately before, prev -- see
// monitor.Monitor.Consecutive -- and the interval its monitor collects at.
// prev may be nil; cur is nil when there is no reading at all; a zero interval
// is unknown.
type LocalFunc func() (prev, cur *types.Snapshot, interval time.Duration)

// Reading is called once for every peer history reading that actually
// arrives, with the node's name and the reading itself.
//
// It exists so the aggregator can persist the fleet's history (topology A):
// writing it from here is what lets the peers keep no storage of their own.
// The reading is the peer's history payload -- its snapshot less the process
// list and per-CPU flags, see HistorySnapshot -- or, from a peer too old to
// serve one, its whole snapshot.
//
// It is NOT called when a poll fails. The poller keeps the previous summary so
// a card shows stale data rather than emptying, but re-writing a reading under
// a new timestamp would record an observation nobody made and would hide the
// outage in the history -- and a gap that needs explaining is better than a
// number that needs distrusting.
type Reading func(node string, snap *types.Snapshot)

// HistoryGate decides whether one peer's history reading is written: record
// or not, and the status the fleet card shows for it. summary is the peer's
// latest account of itself -- nil when there is none. A peer read by its whole
// snapshot has a summary with no collection interval.
//
// It is consulted for every reading before the observer sees it, so it must
// be quick and must not wait on anything: the poll and the history workers
// call it.
type HistoryGate func(peer string, summary *types.NodeSummary) (record bool, status types.PeerHistory)

// observer is a registered Reading and how often history wants a reading.
type observer struct {
	fn      Reading
	cadence func() time.Duration
}

// ConfigFunc supplies the current fleet settings. It is called every cycle so a
// config reload adds or removes peers without restarting the process.
type ConfigFunc func() config.Fleet

// Poller keeps a cached summary per peer, refreshed on an interval.
type Poller struct {
	cfgFn       ConfigFunc
	localFn     LocalFunc
	localNameFn func() string
	logger      *slog.Logger
	client      *http.Client

	// now is the Poller's clock: every time it records or compares -- when a
	// peer was polled and its data fetched, how long a refresh took, how old
	// a drill-in is -- is read from it, so all of them agree. Always
	// time.Now outside tests, which substitute a clock they advance by hand
	// to state an age or a duration exactly instead of waiting for one.
	now func() time.Time

	// observer is read on every poll and written at most once, at wiring
	// time. An atomic pointer rather than a mutex: the poll goroutines only
	// ever load it.
	observer atomic.Pointer[observer]

	// pollOverrideNs is the dashboard's live choice of cadence, in
	// nanoseconds; zero means follow the configuration. Atomic because it is
	// written by an API request and read by the poll loop.
	pollOverrideNs atomic.Int64

	// lastRefreshNs is how long the most recent refresh took, in nanoseconds.
	// Atomic because the poll goroutine writes it and API requests read it.
	//
	// It exists because Run sleeps for the interval AFTER doing the work, so
	// the cadence peers are actually refreshed at is interval + work, not
	// interval. Zero until a refresh has completed, which is honest: nothing
	// has been observed yet.
	lastRefreshNs atomic.Int64

	// bodies holds each peer endpoint's read buffer between cycles; see
	// bodyBuffers.
	bodies bodyBuffers

	// details holds each peer's last drill-in snapshot: peer name ->
	// *detailState. See PeerSnapshot.
	details sync.Map

	// gen is what the last refresh published: every peer's state and the
	// children's subtrees, from one and the same cycle, so no view mixes a
	// child's new state with its old subtree (#190). nil before the first.
	gen atomic.Pointer[generation]

	// instanceID is this process's identity in a fleet tree; see ancestry.go.
	instanceID string
	// viewFetches, fillFetches and relays bound what this aggregator asks
	// its children for on requests' behalf: views' page fetches, parents'
	// page fills, and relayed drill-ins, settings and history. See flight.go
	// and page.go.
	viewFetches *flightGroup
	fillFetches *flightGroup
	relays      *flightGroup
	// subtreeLimit bounds a child's subtree response; maxSubtreeBytes.
	subtreeLimit int64

	// pageCache holds the summaries of subtree nodes fetched for a page from
	// their child aggregators: node name -> *cachedNode. See materialize.
	pageCache sync.Map

	// history runs the per-peer history workers; see recorder.
	history *recorder

	// gate, when set, decides per reading whether a peer's history is
	// written (see HistoryGate); set at wiring time, loaded per reading.
	gate atomic.Pointer[HistoryGate]
	// historyStatus holds each peer's last gate decision: peer name ->
	// historyDecision. It is what the fleet card shows.
	historyStatus sync.Map

	mu    sync.RWMutex
	peers map[string]*peerState
}

// SetClient replaces the HTTP client peers are polled with. Call it before Run.
// A nil client is ignored, so a caller with nothing to install need not branch.
//
// The TLS policy is resolved into a client once, at startup, rather than being
// rebuilt in the poll loop. A CA file that cannot be read or a key pair that
// does not load is a configuration mistake, and it should stop the process with
// a message naming the file -- not appear three seconds later as every peer
// being unreachable for a reason that has nothing to do with the peers.
//
// The consequence is that the TLS settings are boot-time, exactly as
// server.tls_cert and server.tls_key already are: a reload retimes and repoints
// the fleet, but changing how peers are verified takes a restart.
func (p *Poller) SetClient(c *http.Client) {
	if c == nil {
		return
	}
	p.client = pooled(c)
}

// pooled returns c with a transport of its own whose idle pool holds a
// connection to every peer, rather than the standard library's 100 across all
// hosts (#15).
//
// Every peer is polled every cycle, so each one's connection is wanted again
// within seconds. With the default pool, a fleet of more than a hundred peers
// evicted the surplus between cycles and redialled it on the next: measured at
// 300 peers, 222 new connections every cycle -- each a TLS handshake on an
// https fleet, on both ends, forever. Per host the default of two idle
// connections stays, which covers a peer's summary poll and its history worker
// at once, and the idle timeout still closes the connections of a peer that has
// been removed.
//
// The transport is cloned, never modified: without TLS settings the client
// arrives on http.DefaultTransport, which the rest of the process shares. A
// client whose transport is not an *http.Transport is used as given.
func pooled(c *http.Client) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	t, ok := base.(*http.Transport)
	if !ok {
		return c
	}
	t = t.Clone()
	t.MaxIdleConns = 0 // no cap across hosts; MaxIdleConnsPerHost still bounds each peer
	own := *c
	own.Transport = t
	return &own
}

// Observe registers fn to be called for each peer history reading that
// arrives, and cadence to say how often history wants one. Call it before Run.
// A nil fn clears it.
//
// cadence is read on every cycle and by every history worker. Zero means
// history is not being recorded, and then no history payload is fetched at
// all: the summary is all the fleet view needs, so an aggregator not writing
// history -- remote write off, or turned off by a reload -- never pays for the
// rest. A positive cadence is the interval each peer's history worker fetches
// at, floored at MinPollInterval; see recorder.
func (p *Poller) Observe(fn Reading, cadence func() time.Duration) {
	if fn == nil || cadence == nil {
		p.observer.Store(nil)
		return
	}
	p.observer.Store(&observer{fn: fn, cadence: cadence})
}

// configuredAt reports whether cfg configures a peer named name at url.
func configuredAt(cfg config.Fleet, name, url string) bool {
	for _, peer := range cfg.Peers {
		if peer.Name == name {
			return peer.URL == url
		}
	}
	return false
}

// historyDecision is one gate decision, for the peer at url. A reload that
// repoints the name makes it another machine's, and it is not shown.
type historyDecision struct {
	url    string
	status types.PeerHistory
}

// GateHistory registers gate to decide, for each peer history reading, whether
// it is written. Call it before Run. A nil gate records everything.
func (p *Poller) GateHistory(gate HistoryGate) {
	if gate == nil {
		p.gate.Store(nil)
		return
	}
	p.gate.Store(&gate)
}

// New returns a Poller. localNameFn supplies how this node names itself in the
// fleet; it is a function rather than a value so a configuration reload can
// change the name without restarting.
func New(cfgFn ConfigFunc, localFn LocalFunc, localNameFn func() string, logger *slog.Logger) *Poller {
	p := &Poller{
		cfgFn:        cfgFn,
		localFn:      localFn,
		localNameFn:  localNameFn,
		logger:       logger,
		client:       pooled(outbound.NewClient(0)),
		now:          time.Now,
		peers:        make(map[string]*peerState),
		instanceID:   newInstanceID(),
		viewFetches:  newFlightGroup(maxConcurrentFetches, 0),
		fillFetches:  newFlightGroup(maxConcurrentFetches, 0),
		relays:       newFlightGroup(maxConcurrentFetches, relayReuse),
		subtreeLimit: maxSubtreeBytes,
	}
	p.history = newRecorder(p)
	return p
}

// Enabled reports whether any peers are configured.
func (p *Poller) Enabled() bool {
	return len(p.cfgFn().Peers) > 0
}

// Run refreshes every peer until done is closed. The period is re-read from
// configuration each cycle so a reload retimes it.
//
// This sleeps for the interval AFTER the refresh rather than driving off a
// ticker, so the real cadence is interval + work. That is measurable: mon0 runs
// at 3.20s against a configured 3.00s. PollInterval reports the real figure
// rather than the configured one, so the dashboard is truthful about it.
//
// Leaving the loop this way is a DELIBERATE deferral, not an oversight. A true
// ticker would poll every 3.00s instead of every 3.20s -- 6.7% MORE aggregation
// on mon0, which is the node this is trying to relieve, and where aggregation
// was measured as 94% of the process's CPU (#76). That larger question is now
// settled: the poll carries summaries and never a process list (see
// poll). With the cycle that much cheaper the ticker costs nothing worth
// counting, and converting to one is a separate change rather than part of
// this one.
//
// The history workers are reconciled after each cycle and stopped, and waited
// for, when Run returns: no worker outlives the loop that started it.
func (p *Poller) Run(done <-chan struct{}) {
	defer p.history.stop()
	for {
		cfg := p.cfgFn()
		// Reconciled before the poll as well as after it: a peer removed or
		// repointed by a reload has its worker cancelled before the poll --
		// which can take up to the fleet timeout -- rather than after, and a
		// peer the poll has just classified gets its worker at once.
		p.history.sync(cfg)
		p.cycle(cfg)
		// Re-read: a reload that landed during the poll is acted on now,
		// not after the sleep.
		p.history.sync(p.cfgFn())
		select {
		case <-done:
			return
		case <-time.After(p.effectiveInterval(cfg)):
		}
	}
}

// cycle is one pass of Run: refresh the configured peers, or, when a reload
// has removed the last of them, release their read buffers and drill-in
// snapshots -- refresh is what prunes them, and it does not run with no peers
// to poll.
func (p *Poller) cycle(cfg config.Fleet) {
	if len(cfg.Peers) == 0 {
		p.bodies.forget(nil)
		p.details.Clear()
		p.pageCache.Clear()
		p.gen.Store(nil)
		// And the poll state, swap baselines with it: a peer added back
		// later starts afresh rather than differencing against a reading
		// from before it was removed.
		p.mu.Lock()
		clear(p.peers)
		p.mu.Unlock()
		// And the history decisions: a peer added back is judged afresh,
		// not shown under a verdict from before it was removed.
		p.historyStatus.Clear()
		return
	}
	p.refresh(cfg)
}

// MinPollInterval is the floor for the peer refresh cadence.
//
// A peer poll is an HTTP round trip to another machine, and every peer is
// fetched on every cycle, so this is not the same kind of cost as reading
// /proc locally. Below a second the aggregator would spend its time asking ten
// machines what changed in the last few hundred milliseconds, on their CPU as
// well as its own.
const MinPollInterval = time.Second

// SetPollInterval overrides the configured peer refresh cadence for as long as
// the process runs, floored at MinPollInterval.
//
// It exists because the dashboard's interval control must drive the poller that
// actually feeds what is on screen (#97): while the view is the fleet or a
// peer, that poller is this one, and a control that changed only how often the
// browser re-read the same cached figures would be reporting a cadence nobody
// was collecting at.
//
// A zero or negative duration clears the override and returns to the
// configuration, which is also what a reload means: the override is the user's
// live choice, the file is the default it started from.
func (p *Poller) SetPollInterval(d time.Duration) {
	if d <= 0 {
		p.pollOverrideNs.Store(0)
		return
	}
	if d < MinPollInterval {
		d = MinPollInterval
	}
	p.pollOverrideNs.Store(int64(d))
}

// PollInterval reports the cadence peers are actually refreshed at, which is
// what the dashboard displays. It is the resolved interval -- the override when
// one is set, the configuration otherwise, never below the floor -- PLUS the
// time the last refresh took.
//
// The addition is the point. Run does the work and THEN sleeps for the
// interval, so a configured 3s with a 200ms refresh produces a peer poll every
// 3.2s, not every 3s. On mon0 that was measured at 3.20s against a configured
// 3.00s (#76): the aggregation cycle is visible in that node's CPU trace with
// an autocorrelation peak at exactly 3.20s.
//
// Returning the configured value would restate the file rather than describe
// the service, which is the defect effectiveInterval's own comment says this
// feature exists to remove. It was reintroduced here by the shape of the loop
// rather than by the resolution logic, so it is fixed here rather than there.
//
// This reports the LAST cycle, not an average. A cycle in which a peer times
// out genuinely does take longer, and the cadence genuinely does stretch for
// it; smoothing that would trade a true jumpy number for a calm untrue one.
//
// The loop still uses effectiveInterval directly, so this changes what is
// REPORTED and nothing about what is done. Converting Run to a real ticker --
// which would make the cadence match the configuration instead of the
// configuration matching the cadence -- is deliberately not done here; see the
// note on Run.
func (p *Poller) PollInterval() time.Duration {
	return p.effectiveInterval(p.cfgFn()) + time.Duration(p.lastRefreshNs.Load())
}

// LastRefresh reports how long the most recent peer refresh took, or zero
// before the first one completes. It is the difference between the configured
// cadence and the one PollInterval reports.
func (p *Poller) LastRefresh() time.Duration {
	return time.Duration(p.lastRefreshNs.Load())
}

// effectiveInterval resolves the override, the configuration and the floor in
// one place, so the loop and the reported value cannot disagree -- a dashboard
// stating a cadence the poller is not running at is the defect this feature
// exists to remove, and having two expressions of it is how that comes back.
func (p *Poller) effectiveInterval(cfg config.Fleet) time.Duration {
	if ns := p.pollOverrideNs.Load(); ns > 0 {
		return time.Duration(ns)
	}
	if configured := cfg.PollInterval(); configured > 0 {
		return max(configured, MinPollInterval)
	}
	return MinPollInterval
}

// Peer endpoints the poller reads.
const (
	endpointSummary  = "/api/summary"
	endpointSnapshot = "/api/snapshot"
	endpointHistory  = "/api/snapshot/history"
)

// maxSummaryBytes bounds a peer's summary. A summary is a few kilobytes; the
// bound is generous for a node with many alerts and still far below what a
// snapshot is allowed, so a peer answering the summary path with something
// else entirely is refused rather than decoded.
const maxSummaryBytes = 1 << 20 // 1 MiB

// maxDrainBytes bounds how much of an error response is read to keep its
// connection reusable. An error body larger than this is not worth reading;
// that connection is simply closed.
const maxDrainBytes = 64 << 10

// refresh polls all peers concurrently. One slow peer must not delay the rest,
// so each has its own timeout and they run in parallel.
func (p *Poller) refresh(cfg config.Fleet) {
	started := p.now()
	defer func() { p.lastRefreshNs.Store(int64(p.now().Sub(started))) }()

	recording := p.historyCadence() > 0

	var wg sync.WaitGroup
	for _, peer := range cfg.Peers {
		wg.Add(1)
		go func(peer config.Peer) {
			defer wg.Done()
			p.mu.RLock()
			prev := p.peers[peer.Name]
			p.mu.RUnlock()
			if prev != nil && prev.url != peer.URL {
				prev = nil
			}

			st := p.poll(peer, cfg, prev, recording)

			p.mu.Lock()
			p.peers[peer.Name] = st
			p.mu.Unlock()
		}(peer)
	}
	wg.Wait()

	// Forget peers that have been removed from the configuration.
	keep := make(map[string]bool, len(cfg.Peers))
	buffers := make(map[string]bool, 3*len(cfg.Peers))
	for _, peer := range cfg.Peers {
		keep[peer.Name] = true
		for _, endpoint := range bufferClasses {
			buffers[bufferKey(peer.Name, endpoint)] = true
		}
	}
	p.mu.Lock()
	for name := range p.peers {
		if !keep[name] {
			delete(p.peers, name)
		}
	}
	p.mu.Unlock()
	// A removed peer's history decision goes with it, so a name added back
	// is judged afresh -- and one repointed at another address: that
	// decision was about a different machine.
	p.historyStatus.Range(func(name, d any) bool {
		if !keep[name.(string)] || !configuredAt(cfg, name.(string), d.(historyDecision).url) {
			p.historyStatus.Delete(name)
		}
		return true
	})
	// A drill-in snapshot goes with its peer, and with its peer's address: one
	// fetched from a URL a reload has replaced is another machine's, and is
	// dropped now rather than whenever that node is next opened. A detail
	// whose lock is held is mid-fetch; that request checks the mapping itself,
	// and the next refresh catches whatever it leaves.
	//
	// Reconciled against the configuration as it is now, not the one this
	// cycle polled: a drill-in made after a reload that landed mid-poll holds
	// the new address, and the old mapping would throw it away.
	current := p.cfgFn()
	urls := make(map[string]string, len(current.Peers))
	for _, peer := range current.Peers {
		urls[peer.Name] = peer.URL
	}
	p.details.Range(func(k, v any) bool {
		name := k.(string)
		url, configured := urls[name]
		if !configured {
			p.details.Delete(name)
			return true
		}
		d := v.(*detailState)
		if d.tryAcquire() {
			d.repoint(url)
			d.release()
		}
		return true
	})
	p.bodies.forget(buffers)
	// Every peer's state and the children's subtrees, published together for
	// every request until the next refresh.
	p.publish(current)
	// A page's subtree summaries go with their child, and with age: one
	// fetched longer ago than two intervals will never be reused.
	p.forgetPages(urls, 2*p.detailReuse(current))
	// And a relayed answer once its reuse is over. Each is kept under its
	// relay's key -- the child, the endpoint with its query, the chain -- so
	// every distinct history range, and every chain a drill-in arrived with,
	// is a key of its own that no later request may ever ask for again. Left
	// to be dropped only when its own key was next asked for, they
	// accumulated for the life of the process.
	p.relays.forget()
}

// historyCadence is how often history wants a reading from each peer, or zero
// when history is not being recorded.
func (p *Poller) historyCadence() time.Duration {
	obs := p.observer.Load()
	if obs == nil {
		return 0
	}
	return obs.cadence()
}

// peerSource reports how the latest poll of peer at its configured URL read
// it, or "" when no poll has read it at that URL -- a state left from an
// address a reload has replaced says nothing about the new one.
func (p *Poller) peerSource(peer config.Peer) types.SummarySource {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if st := p.peers[peer.Name]; st != nil && st.url == peer.URL {
		return st.source
	}
	return ""
}

// pollFailedSince reports whether the latest summary poll of peer, at its
// current URL, failed after t. A drill-in answered from before that failure
// would present the node as live while the fleet view calls it unreachable. A
// failure at an address a reload has replaced says nothing about the new one.
func (p *Poller) pollFailedSince(peer config.Peer, t time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := p.peers[peer.Name]
	return st != nil && st.url == peer.URL && st.err != "" && st.polledAt.After(t)
}

// poll is one peer's summary for this cycle. A peer served by its whole
// snapshot also has that snapshot recorded here when history is being
// recorded; a peer that served a summary has its history payload fetched by
// its history worker, on its own schedule (see recorder).
//
// A peer too old to serve a summary answers 404 on it, and is then polled the
// way every peer used to be: its whole snapshot, summarised here. That keeps a
// fleet working through a rollout, and it is marked on the node (Source
// "snapshot") rather than hidden -- it costs the aggregator what #76 set out
// to remove, and an operator should be able to see which peers still do.
func (p *Poller) poll(peer config.Peer, cfg config.Fleet, prev *peerState, recording bool) *peerState {
	now := p.now()
	st := &peerState{url: peer.URL, polledAt: now, fetchedAt: now}

	var summary types.NodeSummary
	err := p.get(context.Background(), peer, cfg, endpointSummary, maxSummaryBytes, &summary)
	switch {
	case err == nil:
		st.summary = &summary
		st.source = types.SummarySourceSummary
	case endpointNotServed(err):
		err = p.pollSnapshot(peer, cfg, prev, st, recording)
	}

	switch {
	case err != nil:
	case st.summary.InstanceID != "" && st.summary.InstanceID == p.instanceID:
		// The peer is this aggregator, under another name: the shortest loop
		// there is. Nothing below it is composed.
		st.tree = &childTree{refused: "this peer is this aggregator itself, listed under another name"}
	case st.summary.Aggregates > 0:
		p.pollSubtree(peer, cfg, prev, st)
	}

	if err != nil {
		st.summary = nil
		st.err = err.Error()
		// The served error is worded to name no address (PeerRequestError);
		// the log is on this host, and keeps the whole chain for the operator.
		p.logger.Debug("fleet: peer unreachable", "peer", peer.Name, "error", err, "cause", errors.Unwrap(err))
		// Keep the last good summary alongside the error so a brief blip shows
		// as stale data rather than an empty card -- and keep the time that
		// summary was actually fetched with it. fetchedAt dates the DATA, not
		// the attempt: carrying the retry's timestamp instead would reset the
		// age on every failed poll, so a node unreachable for hours would
		// report having been updated moments ago. Stale data presented as
		// fresh is worse than an obvious gap, because it is the reading an
		// operator would act on.
		if prev != nil && prev.summary != nil {
			st.summary = prev.summary
			st.source = prev.source
			st.fetchedAt = prev.fetchedAt
			st.swapBase = prev.swapBase
		}
		// A child aggregator that stopped answering keeps its last subtree,
		// so its nodes are shown as unknown with their last known ages
		// rather than vanishing from the fleet.
		if prev != nil {
			st.tree = prev.tree
		}
	}
	// Dated when the poll finished, not when it began: a failure that lands
	// after a drill-in succeeded must end that drill-in's reuse.
	st.polledAt = p.now()
	return st
}

// pollSnapshot is the path for a peer that does not serve a summary: its whole
// snapshot is fetched and summarised here, against the swap counters kept
// from its previous snapshot, and the same snapshot is the reading recorded.
func (p *Poller) pollSnapshot(peer config.Peer, cfg config.Fleet, prev, st *peerState, recording bool) error {
	var snap types.Snapshot
	if err := p.get(context.Background(), peer, cfg, endpointSnapshot, maxSnapshotBytes, &snap); err != nil {
		return err
	}
	var base *types.Snapshot
	if prev != nil {
		base = prev.swapBase
	}
	summary := Summarize(base, &snap)
	st.swapBase = swapBaseline(&snap)
	// A fleet polling faster than the peer collects fetches the same snapshot
	// twice. There is no new interval to measure then, and the rate measured
	// over the last one still stands; recomputing it against itself would
	// flicker the card to "paging unknown" until the peer published again.
	if base != nil && snap.Timestamp.Equal(base.Timestamp) && prev.summary != nil {
		summary.SwapRateAvailable = prev.summary.SwapRateAvailable
		summary.SwapInRate = prev.summary.SwapInRate
		summary.SwapOutRate = prev.summary.SwapOutRate
		st.swapBase = base
	}
	st.summary = &summary
	st.source = types.SummarySourceSnapshot
	if recording && p.admit(peer, st.summary) {
		p.deliver(peer.Name, &snap)
	}
	return nil
}

// admit asks the gate whether peer's history is written, and records the
// decision for its card. summary is what the gate judges the peer by: its
// latest account of itself. Without a gate everything is admitted.
func (p *Poller) admit(peer config.Peer, summary *types.NodeSummary) bool {
	gate := p.gate.Load()
	if gate == nil {
		return true
	}
	record, status := (*gate)(peer.Name, summary)
	p.historyStatus.Store(peer.Name, historyDecision{url: peer.URL, status: status})
	return record
}

// deliver hands one admitted reading to the observer, if there is one.
func (p *Poller) deliver(node string, snap *types.Snapshot) {
	if obs := p.observer.Load(); obs != nil {
		obs.fn(node, snap)
	}
}

// peerSummary returns the latest summary of peer at its configured URL; nil
// when no poll has read it there.
func (p *Poller) peerSummary(peer config.Peer) *types.NodeSummary {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if st := p.peers[peer.Name]; st != nil && st.url == peer.URL {
		return st.summary
	}
	return nil
}

// endpointNotServed reports whether a failed fetch of an endpoint this build
// added means the peer (or a proxy in front of it) does not serve it, rather
// than that the peer is down.
//
// A 404 is the ordinary answer. The other is a 200 whose body is not JSON: an
// older build embedded as a library without dashboard assets answers every
// unknown path from its catch-all status page, "go-sysmon is running". A peer
// that is genuinely broken fails the snapshot fetch that follows too, so
// trying it costs nothing in honesty.
func endpointNotServed(err error) bool {
	var syntax *json.SyntaxError
	return isNotFound(err) || errors.As(err, &syntax)
}

// isNotFound reports whether err is a peer answering 404.
func isNotFound(err error) bool {
	var status *PeerStatusError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

// bufferKey names the read buffer kept for one peer endpoint. Each endpoint
// has its own, because a summary is a few kilobytes and a snapshot hundreds,
// and a buffer shared between them would be resized on every cycle.
func bufferKey(peer, endpoint string) string {
	return peer + " " + endpoint
}

// Endpoint classes a peer's read buffers are kept by. A request carrying a
// query or naming a node -- a page's names, a relayed drill-in -- reads into
// its class's buffer, so the buffers kept stay one per peer per class rather
// than one per distinct request (#190).
const (
	classForwardSnapshot = "/api/fleet/*/snapshot"
	classForwardConfig   = "/api/fleet/*/config"
)

// bufferClasses is every class a peer may keep a buffer for between cycles.
var bufferClasses = []string{
	endpointSummary, endpointSnapshot, endpointHistory,
	endpointSubtree, endpointNodes, classForwardSnapshot, classForwardConfig,
}

// bufferClass is the class of an endpoint: its path without the query, and a
// node's forwarded drill-in or settings under one class each.
func bufferClass(endpoint string) string {
	path, _, _ := strings.Cut(endpoint, "?")
	if rest, ok := strings.CutPrefix(path, "/api/fleet/"); ok && strings.Contains(rest, "/") {
		switch {
		case strings.HasSuffix(rest, "/snapshot"):
			return classForwardSnapshot
		case strings.HasSuffix(rest, "/config"):
			return classForwardConfig
		}
	}
	return path
}

// drain reads what is left of a short error body before it is closed, so the
// connection goes back to the pool.
func drain(p *Poller, peer config.Peer, resp *http.Response) {
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)); err != nil {
		p.logger.Debug("fleet: draining peer error body", "peer", peer.Name, "error", err)
	}
}

// get fetches one peer endpoint and decodes it into v, bounded by limit.
//
// It takes the whole fleet config rather than just the timeout because the
// transport a peer is reached over is a property of the fleet, and deciding it
// here -- once, per peer, per request -- is what lets a reload move a peer from
// http to https without restarting.
func (p *Poller) get(parent context.Context, peer config.Peer, cfg config.Fleet, endpoint string, limit int64, v any) error {
	resp, cancel, err := p.do(parent, peer, cfg, endpoint)
	if err != nil {
		return err
	}
	defer cancel()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.logger.Debug("fleet: closing peer body", "peer", peer.Name, "error", err)
		}
	}()

	if resp.StatusCode == http.StatusLoopDetected {
		drain(p, peer, resp)
		return &FleetRefusedError{Peer: peer.Name, Reason: resp.Header.Get(HeaderRefusal)}
	}
	if resp.StatusCode != http.StatusOK {
		// Read what is left of a short error body before it is closed, so the
		// connection goes back to the pool. A peer too old to serve a summary
		// answers 404 on every cycle, and without this each of those would
		// cost the fallback fetch that follows a new connection -- and a new
		// TLS handshake on an https fleet.
		if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)); err != nil {
			p.logger.Debug("fleet: draining peer error body", "peer", peer.Name, "error", err)
		}
		return &PeerStatusError{Peer: peer.Name, Endpoint: endpoint, Status: resp.StatusCode}
	}
	if err := p.bodies.decode(bufferKey(peer.Name, bufferClass(endpoint)), resp.Body, limit, v); err != nil {
		return &PeerRequestError{Peer: peer.Name, Cause: err}
	}
	// A peer on an older build sends null for a list it has none of, and this
	// node serves what it decoded on as its own. Normalised here, before it is
	// cached and shared, so this node's wire never carries null (#151).
	types.ReplaceNilSlices(v)
	return nil
}

// do sends one GET for endpoint to peer, over the transport the fleet's TLS
// policy allows, bounded by the fleet timeout. The caller closes the body and
// then calls cancel.
func (p *Poller) do(parent context.Context, peer config.Peer, cfg config.Fleet, endpoint string) (*http.Response, context.CancelFunc, error) {
	target := strings.TrimRight(peer.URL, "/") + endpoint

	parsed, err := url.Parse(target)
	if err != nil {
		return nil, nil, &PeerURLError{Peer: peer.Name, Cause: err}
	}
	// The scheme is checked before the request is built, so a peer that should
	// not be spoken to in the clear is refused here rather than after the
	// connection has already been made.
	switch parsed.Scheme {
	case "https":
	case "http":
		if cfg.TLS.RequireTLS {
			return nil, nil, &PlaintextPeerError{Peer: peer.Name}
		}
	default:
		return nil, nil, &PeerSchemeError{Peer: peer.Name, Scheme: parsed.Scheme}
	}

	ctx, cancel := context.WithTimeout(parent, cfg.Timeout())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, nil, &PeerURLError{Peer: peer.Name, Cause: err}
	}
	p.stampRequest(ctx, req)
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, &PeerRequestError{Peer: peer.Name, Cause: err}
	}
	return resp, cancel, nil
}

// detailReuse is how long a peer's drill-in snapshot, or failed attempt, is
// served again rather than fetched anew: the fleet's effective poll interval.
//
// That interval is the cadence the dashboard reports for a selected peer and
// the one its rate control sets while a peer is shown (#97), and it is what
// governed a peer's detail when the poll fetched whole snapshots. Every open
// drill-in polls the aggregator, and several browsers -- or one whose own
// refresh is faster than the fleet's -- would otherwise each cost that peer a
// whole snapshot per tick.
//
// It is the interval as reported (PollInterval): the resolved cadence plus the
// time the last refresh took, which is the rate the dashboard is told a
// selected peer is refreshed at.
func (p *Poller) detailReuse(cfg config.Fleet) time.Duration {
	return p.effectiveInterval(cfg) + time.Duration(p.lastRefreshNs.Load())
}

// detailState is the last drill-in snapshot fetched from one peer. Its mutex is
// held across the fetch, so concurrent drill-ins of the same peer wait for one
// request rather than each making their own.
type detailState struct {
	// lock is held across the fetch, so concurrent drill-ins of one peer wait
	// for a single request. A channel rather than a mutex so waiting for it
	// honours the drill-in's deadline: a request queued behind a slow fetch
	// gives up when its budget does.
	lock chan struct{}
	// url is the peer URL the snapshot came from. A reload that repoints the
	// name at another URL makes the snapshot some other machine's.
	url       string
	snap      *types.Snapshot
	fetchedAt time.Time
	// triedAt and err are the latest attempt, successful or not. A failed
	// attempt is reused as long as a snapshot is (detailReuse), or every
	// drill-in queued behind it would wait out the peer's timeout again.
	triedAt time.Time
	err     error
}

func newDetailState() *detailState {
	return &detailState{lock: make(chan struct{}, 1)}
}

// acquire takes d's lock, or gives up when ctx is done.
func (d *detailState) acquire(ctx context.Context) error {
	select {
	case d.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryAcquire takes d's lock if it is free.
func (d *detailState) tryAcquire() bool {
	select {
	case d.lock <- struct{}{}:
		return true
	default:
		return false
	}
}

// release gives d's lock back.
func (d *detailState) release() { <-d.lock }

// answer is what d says at now: its snapshot, marked stale when the latest
// attempt failed, or that attempt's error when there is no snapshot at all.
func (d *detailState) answer(now time.Time) (types.PeerSnapshot, error) {
	if d.snap == nil {
		return types.PeerSnapshot{}, d.err
	}
	env := types.PeerSnapshot{
		Snapshot:   d.snap,
		Reachable:  d.err == nil,
		AgeSeconds: now.Sub(d.fetchedAt).Seconds(),
	}
	if d.err != nil {
		env.Error = d.err.Error()
	}
	return env, nil
}

// repoint forgets everything d holds when the peer's URL has changed, so a
// snapshot fetched from the old address is never served under the new one.
func (d *detailState) repoint(url string) {
	if d.url == url {
		return
	}
	d.url, d.snap, d.fetchedAt, d.triedAt, d.err = url, nil, time.Time{}, time.Time{}, nil
}

// PeerSnapshot returns a node's full snapshot together with the freshness of
// the fetch behind it, so the dashboard can render any node from the
// aggregator without contacting it directly.
//
// A peer's snapshot is fetched on demand: the poll cycle carries only
// summaries (#76), and the whole snapshot is needed only by someone looking at
// that node. A fetch that fails still answers with the last snapshot fetched,
// when there is one -- a brief blip should not blank the view -- but never as a
// plain success. Reachable and AgeSeconds describe the LATEST attempt, and they
// are what stop stale readings from being displayed as current ones.
//
// The error is UnknownPeerError for a node that is not configured, NoReadingError
// when there is nothing to serve, and otherwise the reason the peer could not
// be read.
func (p *Poller) PeerSnapshot(ctx context.Context, name string, budget time.Duration) (types.PeerSnapshot, error) {
	switch route := p.Resolve(name); route.Kind {
	case RouteLocal:
		if _, snap, _ := p.localFn(); snap != nil {
			// The local node is collected on demand, so it is current by
			// construction.
			return types.PeerSnapshot{Snapshot: snap, Reachable: true}, nil
		}
		return types.PeerSnapshot{}, &NoReadingError{Node: name}
	case RouteSubtree:
		// A node of a child's subtree, whose drill-in the child serves
		// (#190).
		return p.forwardSnapshot(ctx, name, route, budget)
	case RoutePeer:
	default:
		return types.PeerSnapshot{}, route.err(name)
	}

	// budget bounds the whole drill-in on the peer's behalf -- waiting for
	// another request's fetch included -- so it cannot outlive the response it
	// is for. ctx is the asker's own.
	opCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	v, _ := p.details.LoadOrStore(name, newDetailState())
	d := v.(*detailState)
	if err := d.acquire(opCtx); err != nil {
		// Not recorded: the fetch this waited for belongs to another request,
		// which records its own outcome.
		return types.PeerSnapshot{}, err
	}
	defer d.release()

	// Resolved again under the lock: a reload may have removed or repointed
	// the peer while this request waited for it.
	peer, cfg, ok := p.resolvePeer(name)
	if !ok {
		return types.PeerSnapshot{}, &UnknownPeerError{Peer: name}
	}
	d.repoint(peer.URL)
	if !d.triedAt.IsZero() && p.now().Sub(d.triedAt) < p.detailReuse(cfg) &&
		!p.pollFailedSince(peer, d.triedAt) {
		return d.answer(p.now())
	}

	var snap types.Snapshot
	err := p.get(opCtx, peer, cfg, endpointSnapshot, maxSnapshotBytes, &snap)
	if ctx.Err() != nil {
		// The asker went away, or its own deadline passed. That says nothing
		// about the peer, so it is not recorded as an attempt that others
		// would be answered from. The budget running out with the asker still
		// there is different: a peer that did not answer in time, which is
		// its failure and is recorded as one below.
		return types.PeerSnapshot{}, ctx.Err()
	}
	// And again after the fetch: if the name has moved to another address
	// meanwhile, this answer is another machine's and is neither kept nor
	// served under the name.
	if now, _, ok := p.resolvePeer(name); !ok || now.URL != peer.URL {
		return types.PeerSnapshot{}, &PeerMovedError{Peer: name}
	}
	d.err, d.triedAt = err, p.now()
	if d.err == nil {
		d.snap, d.fetchedAt = &snap, d.triedAt
	}
	return d.answer(p.now())
}

// View builds the fleet view: the page of nodes q selects, in name order, and
// the fleet-level roll-up over EVERY node -- never over the page.
//
// On an aggregator with child aggregators (#190) the page may hold nodes of
// their subtrees; those are fetched from their children now, bounded by ctx,
// unless a fetch within the poll interval already has them.
func (p *Poller) View(ctx context.Context, q Query) types.Fleet {
	g := p.published()
	if len(g.subtrees.childURL) == 0 {
		return p.flatView(g, q)
	}
	return p.treeView(ctx, g, q)
}

// treeView is the view of a fleet with child aggregators; see View.
func (p *Poller) treeView(ctx context.Context, g *generation, q Query) types.Fleet {
	t, ok := p.treeOf(g)
	if !ok {
		return types.Fleet{Enabled: false}
	}
	var self *types.FleetNode
	for i := range t.direct {
		if t.direct[i].Self {
			n := t.direct[i]
			self = &n
			break
		}
	}
	shown, matched, offset := page(t.rows, rowKey, q)
	// A whole-fleet request on a tree fetches nothing from the children: its
	// subtree nodes are served from the cache or their index entries, so an
	// unauthenticated request cannot make this node ask its children for
	// every node of the tree (#190).
	nodes := p.materialize(ctx, t, shown, q.bounded() != Query{})

	names := rollupNames(q)
	r := t.rollup(names)
	trimBuilds(r, names)
	return types.Fleet{
		Enabled: true,
		Nodes:   nodes,
		Page: &types.FleetPage{
			Query:   q.Search,
			Offset:  offset,
			Limit:   max(q.Limit, 0),
			Matched: matched,
			Total:   len(t.rows),
		},
		Self:   self,
		Rollup: r,
	}
}

// rollupNames is how many nodes the roll-up names in each list for q. Only a
// request with no parameters at all -- the whole fleet, the one shape there was
// before paging -- gets the lists whole: it is already the size of the fleet,
// and it is what every consumer written before paging, the dashboard left open
// across an upgrade included, reads its "and N more" from. Any search or bound
// is a request for less than the fleet, and its roll-up is bounded (#15); a
// search matching one node must not carry every unhealthy node's name.
func rollupNames(q Query) int {
	if q.bounded() == (Query{}) {
		return 0
	}
	return types.RollupNames
}

// Rollup is the fleet-level statement alone, over every node: what GET
// /api/fleet/rollup serves to a consumer that wants the fleet's state and none
// of its nodes. It is nil when no peers are configured.
func (p *Poller) Rollup() *types.FleetRollup {
	t, ok := p.tree()
	if !ok {
		return nil
	}
	r := t.rollup(types.RollupNames)
	trimBuilds(r, types.RollupNames)
	return r
}

// nodes is every node in the fleet, this one included, in name order, with
// the peers' states as generation g published them and each node's status
// and build agreement stamped. ok is false when no peers are configured and
// there is no fleet to describe.
func (p *Poller) nodes(g *generation) (nodes []types.FleetNode, ok bool) {
	cfg := p.cfgFn()
	if len(cfg.Peers) == 0 {
		return nil, false
	}
	self := p.selfNode()

	// Alphabetical by name (owner ruling 2026-09-01). Names are unique within a
	// fleet, so this is a total order and the list cannot reshuffle between
	// polls -- which was the original reason for sorting here at all, since map
	// and config order left equal nodes moving around.
	//
	// This used to put the worst status first so a fault led the list. That
	// reads as "unsorted" to someone looking for a host. A fleet list is used
	// far more often to find a named host than to discover an unknown fault,
	// and the fault is not hidden by name order: the roll-up above the list
	// counts the nodes by state and names every one that needs attention.
	//
	// Search and paging are applied to this order (see View), so a page is a
	// contiguous run of it and the same node can never appear on two pages.
	//
	// The peers' order and states come from the generation, which sorted them
	// once at publish; every request then builds the list in place, in order,
	// with no sort and no lookup by name. A reload that has replaced the peer
	// list since is sorted here, as the list always used to be.
	order, states := g.order, g.states
	if !samePeers(g.cfgPeers, cfg.Peers) {
		order, states = sortedPeers(cfg.Peers, g.peers)
	}
	nodes = make([]types.FleetNode, len(cfg.Peers)+1)
	at := sort.Search(len(order), func(i int) bool {
		return CompareNodeNames(cfg.Peers[order[i]].Name, self.Name) > 0
	})
	nodes[at] = self

	now := p.now()
	recording := p.historyCadence() > 0
	for k, i := range order {
		slot := k
		if k >= at {
			slot = k + 1
		}
		peerNode(&nodes[slot], cfg.Peers[i], states[k], now)
		nodes[slot].History = p.peerHistory(cfg.Peers[i], recording)
	}

	// Build agreement and status are stamped in one pass over every node,
	// which also covers the two paths that carry no summary -- the peer that
	// has not been polled yet and the peer that answered nothing -- so no node
	// leaves the view with either field unset.
	for i := range nodes {
		stamp(&nodes[i], self.GitCommit)
	}
	return nodes, true
}

// selfNode is this node as the fleet view lists it, from its latest two
// collections.
func (p *Poller) selfNode() types.FleetNode {
	self := types.FleetNode{Name: p.localNameFn(), Self: true}
	if prev, cur, interval := p.localFn(); cur != nil {
		self.Reachable = true
		self.Source = types.SummarySourceLocal
		self.NodeSummary = LocalSummary(prev, cur, interval)
		// This node's name and the name it writes under are one function,
		// resolved now. Its snapshot's copy is restamped on a reload of
		// fleet.local_name, but after the new configuration is live, and a
		// view built in between would flag the node against itself.
		self.NodeName = self.Name
	} else {
		self.Error = "no data"
	}
	return self
}

// peerHistory is the history decision a peer's card shows: the gate's latest
// for the peer at its configured address, while history is being recorded,
// and nil otherwise. Only while recording: a decision made while it was is no
// statement about a fleet that records nothing. It is read per request, not
// cached with the peer's node, because the history workers decide between
// polls.
func (p *Poller) peerHistory(peer config.Peer, recording bool) *types.PeerHistory {
	if !recording {
		return nil
	}
	d, ok := p.historyStatus.Load(peer.Name)
	if !ok || d.(historyDecision).url != peer.URL {
		return nil
	}
	h := d.(historyDecision).status
	return &h
}

// peerNode fills n with a peer as its latest poll left it.
func peerNode(n *types.FleetNode, peer config.Peer, st *peerState, now time.Time) {
	// The peer's URL stays here, in the poller that dials it. The view is
	// served without authentication, and a URL is the peer's scheme, address
	// and port on the internal network (#120); the dashboard addresses every
	// node by name.
	n.Name = peer.Name
	if st != nil && st.url != peer.URL {
		// Polled at an address a reload has since replaced: that reading is
		// another machine's, and this one has not been polled yet.
		st = nil
	}
	switch {
	case st == nil:
		n.Error = "not polled yet"
	case st.summary == nil:
		// No summary means no reading to be old, so the age stays zero rather
		// than describing the last ATTEMPT. For a peer that has NEVER answered
		// there is no date to report, and the moment of the last try -- about
		// two seconds ago, always -- would render as "unreachable for 2s" for a
		// total absence, which is #69's defect in the field meant to
		// distinguish them.
		n.Error = st.err
		if n.Error == "" {
			n.Error = "no data"
		}
	default:
		n.Reachable = st.err == ""
		n.Error = st.err
		n.Source = st.source
		n.AgeSeconds = now.Sub(st.fetchedAt).Seconds()
		n.NodeSummary = *st.summary
	}
}

// stamp sets the fields of n that the view derives rather than polls. Build
// agreement is the one that is not a property of a node on its own: it is that
// node's commit held against THIS node's.
func stamp(n *types.FleetNode, selfCommit string) {
	n.BuildMatch = compareBuild(selfCommit, n.GitCommit)
	n.NameMatch = compareName(n.Name, n.NodeName, n.Reachable)
	n.Status = nodeStatus(n.Reachable, &n.NodeSummary)
	if n.Alerts == nil {
		n.Alerts = []types.Alert{}
	}
}

// samePeers reports whether a and b are one peer list: the same slice, which
// a configuration reload replaces rather than edits.
func samePeers(a, b []config.Peer) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// sortedPeers is the indexes of peers in name order, and each one's state from
// states, aligned with the order.
func sortedPeers(peers []config.Peer, states map[string]*peerState) ([]int, []*peerState) {
	order := make([]int, len(peers))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return CompareNodeNames(peers[order[i]].Name, peers[order[j]].Name) < 0
	})
	aligned := make([]*peerState, len(order))
	for k, i := range order {
		aligned[k] = states[peers[i].Name]
	}
	return order, aligned
}

// PlaintextPeerError reports a peer configured with an http URL on a fleet that
// has switched plaintext off.
//
// It names the remedy, because the two ways out are not equivalent and an
// operator reading this at three in the morning should not have to weigh them:
// giving the peer an https URL is the fix, and turning allow_plaintext back on
// is the retreat.
type PlaintextPeerError struct {
	Peer string
}

func (e *PlaintextPeerError) Error() string {
	return fmt.Sprintf(
		"peer %s is configured with a plaintext http URL and fleet.tls.require_tls is set; "+
			"give it an https URL, or clear fleet.tls.require_tls to accept plaintext again",
		e.Peer)
}

// PeerSchemeError reports a peer URL whose scheme is neither http nor https.
//
// Without this the request fails inside the HTTP client with a message about
// an unsupported protocol and no mention of which peer caused it, which in a
// fleet of ten is most of the work of finding it.
//
// The message does not quote the scheme. It is served in the fleet view, which
// withholds every peer's address (#120), and the likeliest bad scheme is not a
// scheme at all: "fs.lan:8081" parses as scheme "fs.lan", the peer's host.
type PeerSchemeError struct {
	Peer   string
	Scheme string
}

func (e *PeerSchemeError) Error() string {
	return "peer " + e.Peer + " has a URL that is neither http nor https; " +
		"a peer written as host:port needs http:// or https:// in front"
}

// PeerURLError reports a peer URL that will not parse.
//
// The message names the peer and not the URL, nor the parse error, whose text
// quotes the URL: it is served in the fleet view, which withholds every peer's
// address (#120). The configuration holds the URL for anyone who may read it,
// and Unwrap keeps the parse error for this node's log.
type PeerURLError struct {
	Peer  string
	Cause error
}

func (e *PeerURLError) Error() string {
	return "peer " + e.Peer + " has an unparseable URL; check its url in fleet.peers"
}

func (e *PeerURLError) Unwrap() error { return e.Cause }

// PeerStatusError reports a peer that answered with a non-OK HTTP status.
//
// Endpoint is the path that was asked for, so the message can say what the
// node could not do rather than only what it returned.
type PeerStatusError struct {
	Peer     string
	Endpoint string
	Status   int
}

// endpointsAddedLater are the paths a node can legitimately not serve because
// its build predates them. A 404 on one of these is a version statement.
//
// /api/snapshot is deliberately NOT here, and the distinction is the point: it
// is the endpoint the fleet has always polled, so a 404 on it is a wrong peer
// URL or a proxy that does not forward the path -- never an old build. Telling
// an operator to "check its /api/version" in that case sends them to an
// endpoint that will fail the same way, for the same reason, and delays finding
// the real cause.
//
// Add a path here when it is introduced, not when someone reports the
// confusing message.
var endpointsAddedLater = map[string]bool{
	"/api/config":   true,
	endpointSummary: true,
	endpointHistory: true,
}

func (e *PeerStatusError) Error() string {
	// A 404 from a node that is otherwise answering CAN be a version statement
	// rather than a fault: the endpoint was added after the build it is
	// running. Saying "returned HTTP 404" leaves an operator looking for a
	// broken node; saying what it means turns a bug report into a known state
	// with an obvious remedy.
	//
	// Only for endpoints that postdate the original API, though -- see
	// endpointsAddedLater.
	//
	// Deliberately hedged even then. A reverse proxy that does not forward the
	// path produces the same 404, so this says what was observed and what
	// usually explains it rather than asserting a version.
	if e.Status == http.StatusNotFound && endpointsAddedLater[e.Endpoint] {
		endpoint := e.Endpoint
		if endpoint == "" {
			endpoint = "that endpoint"
		}
		return fmt.Sprintf(
			"peer %s does not serve %s (HTTP 404) — it is most likely running a "+
				"version older than the one that added it; check its /api/version",
			e.Peer, endpoint)
	}
	return fmt.Sprintf("peer %s returned HTTP %d", e.Peer, e.Status)
}

// LocalName returns the name this node uses for itself in the fleet.
func (p *Poller) LocalName() string { return p.localNameFn() }

// PeerConfig fetches a peer's effective configuration on demand.
//
// It is not cached with the snapshots: settings change rarely and are read only
// when someone opens the settings view, so a fresh read is both cheap and more
// truthful than a copy that could be minutes stale. Showing the aggregator's
// own settings while a peer is selected would be actively misleading, which is
// why this exists rather than the view falling back to local config.
func (p *Poller) PeerConfig(ctx context.Context, name string) (*types.ConfigView, error) {
	switch route := p.Resolve(name); route.Kind {
	case RoutePeer:
	case RouteSubtree:
		return p.forwardConfig(ctx, name, route)
	default:
		return nil, route.err(name)
	}
	cfg := p.cfgFn()
	var url string
	for _, peer := range cfg.Peers {
		if peer.Name == name {
			url = strings.TrimRight(peer.URL, "/") + "/api/config"
			break
		}
	}
	if url == "" {
		return nil, &UnknownPeerError{Peer: name}
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &PeerURLError{Peer: name, Cause: err}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.logger.Debug("fleet: peer config unreadable", "peer", name, "error", err)
		return nil, &PeerRequestError{Peer: name, Cause: err}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.logger.Debug("fleet: closing peer config body", "peer", name, "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, &PeerStatusError{Peer: name, Endpoint: "/api/config", Status: resp.StatusCode}
	}

	var view types.ConfigView
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSnapshotBytes)).Decode(&view); err != nil {
		p.logger.Debug("fleet: peer config unreadable", "peer", name, "error", err)
		return nil, &PeerRequestError{Peer: name, Cause: err}
	}
	// The same older-peer case as a snapshot's lists (#151).
	types.ReplaceNilSlices(&view)
	// Relayed with the peer's topology withheld, as this node's own settings
	// are: its bind address, and anything an older build still sends verbatim.
	view = config.RedactPeerView(view)
	return &view, nil
}

// resolvePeer finds name in the current configuration, and returns that
// configuration with it.
func (p *Poller) resolvePeer(name string) (config.Peer, config.Fleet, bool) {
	cfg := p.cfgFn()
	for _, peer := range cfg.Peers {
		if peer.Name == name {
			return peer, cfg, true
		}
	}
	return config.Peer{}, cfg, false
}

// PeerMovedError reports a drill-in whose peer a reload removed or repointed
// while it was being fetched. The answer belonged to the old address and is
// discarded; asking again reaches the new one.
type PeerMovedError struct{ Peer string }

func (e *PeerMovedError) Error() string {
	return "fleet node " + e.Peer + " was reconfigured while its snapshot was being fetched; ask again"
}

// NoReadingError reports a node that is configured but has no reading to serve:
// this node before its first collection.
type NoReadingError struct{ Node string }

func (e *NoReadingError) Error() string { return "no reading yet from fleet node " + e.Node }

// UnknownPeerError reports a request for a node that is not configured.
type UnknownPeerError struct{ Peer string }

func (e *UnknownPeerError) Error() string { return "unknown fleet node " + e.Peer }
