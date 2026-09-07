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

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// maxSnapshotBytes bounds a peer response so a misbehaving or hostile peer
// cannot exhaust memory on the aggregator.
const maxSnapshotBytes = 8 << 20 // 8 MiB

// Node status values reported to the UI.
const (
	StatusHealthy     = "healthy"
	StatusWarning     = "warning"
	StatusCritical    = "critical"
	StatusUnreachable = "unreachable"
)

// Build agreement values, reported per node as FleetNode.BuildMatch.
//
// These describe BUILD IDENTITY, not health. They are kept apart from the
// Status values above and from the alert counts on purpose: during a rollout
// every node disagrees with the aggregator until it is restarted, and a fleet
// that turned amber on every deploy would train operators to ignore the colour.
const (
	BuildSame      = "same"
	BuildDifferent = "different"
	BuildUnknown   = "unknown"
)

// unstatedCommit is what a binary built without the -ldflags reports for its
// commit: main.gitCommit defaults to it, and GET /api/version renders an empty
// value the same way. It says "this build does not know what it is", which is
// the same non-answer as an empty field and is treated as one here.
const unstatedCommit = "unknown"

// compareBuild reports whether a node's commit is the aggregator's own.
//
// A missing commit on either side yields BuildUnknown rather than agreement.
// Two nodes that both declined to say what they are running have not been shown
// to match -- and a peer whose build predates the field is precisely the stale
// node this comparison exists to find, so reading its silence as "same" would
// reintroduce the defect in a new field.
func compareBuild(local, node string) string {
	if local == "" || local == unstatedCommit ||
		node == "" || node == unstatedCommit {
		return BuildUnknown
	}
	if local == node {
		return BuildSame
	}
	return BuildDifferent
}

// peerState is the last result of polling one peer.
type peerState struct {
	snapshot  *types.Snapshot
	err       string
	fetchedAt time.Time
}

// LocalFunc supplies this node's own snapshot.
type LocalFunc func() *types.Snapshot

// Reading is called once for every peer snapshot that actually arrives, with
// the node's name and the reading itself.
//
// It exists so the aggregator can persist the fleet's history: it holds every
// peer's data already, and writing it from here is what lets the peers keep no
// storage of their own.
//
// It is NOT called when a poll fails. The poller keeps the previous snapshot so
// a card shows stale data rather than emptying, but re-writing that reading
// under a new timestamp would record an observation nobody made and would hide
// the outage in the history -- and a gap that needs explaining is better than a
// number that needs distrusting.
type Reading func(node string, snap *types.Snapshot)

// ConfigFunc supplies the current fleet settings. It is called every cycle so a
// config reload adds or removes peers without restarting the process.
type ConfigFunc func() config.Fleet

// Poller keeps a cached snapshot per peer, refreshed on an interval.
type Poller struct {
	cfgFn       ConfigFunc
	localFn     LocalFunc
	localNameFn func() string
	logger      *slog.Logger
	client      *http.Client

	// reading is read on every poll and written at most once, at wiring time.
	// An atomic pointer rather than a mutex: the poll goroutines only ever
	// load it.
	reading atomic.Pointer[Reading]

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
	p.client = c
}

// Observe registers fn to be called for each peer snapshot that arrives. Call
// it before Run. Passing nil clears it.
func (p *Poller) Observe(fn Reading) {
	if fn == nil {
		p.reading.Store(nil)
		return
	}
	p.reading.Store(&fn)
}

// New returns a Poller. localNameFn supplies how this node names itself in the
// fleet; it is a function rather than a value so a configuration reload can
// change the name without restarting.
func New(cfgFn ConfigFunc, localFn LocalFunc, localNameFn func() string, logger *slog.Logger) *Poller {
	return &Poller{
		cfgFn:       cfgFn,
		localFn:     localFn,
		localNameFn: localNameFn,
		logger:      logger,
		client:      outbound.NewClient(0),
		peers:       make(map[string]*peerState),
	}
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
// was measured as 94% of the process's CPU (#76). Spending that before the
// larger question is settled -- whether the poll path should carry peer process
// lists at all, which would remove most of the load -- is paying for a
// correctness fix with the resource under pressure. Once that is decided the
// ticker costs nothing worth counting and should be done then.
func (p *Poller) Run(done <-chan struct{}) {
	for {
		cfg := p.cfgFn()
		if len(cfg.Peers) > 0 {
			p.refresh(cfg)
		}
		select {
		case <-done:
			return
		case <-time.After(p.effectiveInterval(cfg)):
		}
	}
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

// refresh polls all peers concurrently. One slow peer must not delay the rest,
// so each has its own timeout and they run in parallel.
func (p *Poller) refresh(cfg config.Fleet) {
	started := time.Now()
	defer func() { p.lastRefreshNs.Store(int64(time.Since(started))) }()

	var wg sync.WaitGroup
	for _, peer := range cfg.Peers {
		wg.Add(1)
		go func(peer config.Peer) {
			defer wg.Done()
			snap, err := p.fetch(peer, cfg)
			st := &peerState{fetchedAt: time.Now()}
			if err != nil {
				st.err = err.Error()
				p.logger.Debug("fleet: peer unreachable", "peer", peer.Name, "error", err)
			} else {
				st.snapshot = snap
				if fn := p.reading.Load(); fn != nil {
					(*fn)(peer.Name, snap)
				}
			}
			p.mu.Lock()
			// Keep the last good snapshot alongside the error so a brief blip
			// shows as stale data rather than an empty card -- and keep the time
			// that snapshot was actually fetched with it. fetchedAt dates the
			// DATA, not the attempt: carrying the retry's timestamp instead
			// would reset the age on every failed poll, so a node unreachable
			// for hours would report having been updated moments ago. Stale
			// data presented as fresh is worse than an obvious gap, because it
			// is the reading an operator would act on.
			if st.snapshot == nil {
				if prev, ok := p.peers[peer.Name]; ok && prev.snapshot != nil {
					st.snapshot = prev.snapshot
					st.fetchedAt = prev.fetchedAt
				}
			}
			p.peers[peer.Name] = st
			p.mu.Unlock()
		}(peer)
	}
	wg.Wait()

	// Forget peers that have been removed from the configuration.
	keep := make(map[string]bool, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		keep[peer.Name] = true
	}
	p.mu.Lock()
	for name := range p.peers {
		if !keep[name] {
			delete(p.peers, name)
		}
	}
	p.mu.Unlock()
}

// fetch retrieves one peer's snapshot.
//
// It takes the whole fleet config rather than just the timeout because the
// transport a peer is reached over is a property of the fleet, and deciding it
// here -- once, per peer, per cycle -- is what lets a reload move a peer from
// http to https without restarting.
func (p *Poller) fetch(peer config.Peer, cfg config.Fleet) (*types.Snapshot, error) {
	target := strings.TrimRight(peer.URL, "/") + "/api/snapshot"

	parsed, err := url.Parse(target)
	if err != nil {
		return nil, &ErrPeerURL{Peer: peer.Name, URL: peer.URL, Cause: err}
	}
	// The scheme is checked before the request is built, so a peer that should
	// not be spoken to in the clear is refused here rather than after the
	// connection has already been made.
	switch parsed.Scheme {
	case "https":
	case "http":
		if cfg.TLS.RequireTLS {
			return nil, &ErrPlaintextPeer{Peer: peer.Name}
		}
	default:
		return nil, &ErrPeerScheme{Peer: peer.Name, Scheme: parsed.Scheme}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.logger.Debug("fleet: closing peer body", "peer", peer.Name, "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, &ErrPeerStatus{Peer: peer.Name, Endpoint: "/api/snapshot", Status: resp.StatusCode}
	}

	var snap types.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSnapshotBytes)).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// PeerSnapshot returns a peer's cached snapshot together with the freshness of
// the poll behind it, so the dashboard can render any node from the aggregator
// without contacting it directly.
//
// The snapshot is returned even when the latest poll failed -- a brief blip
// should not blank the view -- but never as a plain success. Reachable and
// AgeSeconds describe the LATEST attempt, and they are what stop stale readings
// from being displayed, and re-recorded, as current ones.
func (p *Poller) PeerSnapshot(name string) (types.PeerSnapshot, bool) {
	if name == p.localNameFn() {
		if snap := p.localFn(); snap != nil {
			// The local node is collected on demand, so it is current by
			// construction.
			return types.PeerSnapshot{Snapshot: snap, Reachable: true}, true
		}
		return types.PeerSnapshot{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.peers[name]
	if !ok || st.snapshot == nil {
		return types.PeerSnapshot{}, false
	}
	return types.PeerSnapshot{
		Snapshot:   st.snapshot,
		Reachable:  st.err == "",
		Error:      st.err,
		AgeSeconds: time.Since(st.fetchedAt).Seconds(),
	}, true
}

// View builds the fleet summary: this node first, then its peers, ordered worst
// first so whatever needs attention is at the top.
func (p *Poller) View() types.Fleet {
	cfg := p.cfgFn()
	if len(cfg.Peers) == 0 {
		return types.Fleet{Enabled: false}
	}

	nodes := make([]types.FleetNode, 0, len(cfg.Peers)+1)

	local := p.localFn()
	self := summarize(p.localNameFn(), "", true, local, time.Now(), "")
	nodes = append(nodes, self)

	p.mu.RLock()
	for _, peer := range cfg.Peers {
		st := p.peers[peer.Name]
		if st == nil {
			nodes = append(nodes, types.FleetNode{
				Name: peer.Name, URL: peer.URL,
				Reachable: false, Status: StatusUnreachable, Error: "not polled yet",
			})
			continue
		}
		nodes = append(nodes, summarize(peer.Name, peer.URL, false, st.snapshot, st.fetchedAt, st.err))
	}
	p.mu.RUnlock()

	// Build agreement is stamped here rather than inside summarize because it
	// is the one field that is not a property of a node on its own: it is that
	// node's commit held against THIS node's. Doing it in one pass over every
	// node also covers the two paths summarize never reaches -- the peer that
	// has not been polled yet and the peer that answered nothing -- so no node
	// leaves the view with the field unset, which would decode as "" and read
	// as neither agreement nor its absence.
	selfCommit := self.GitCommit
	for i := range nodes {
		nodes[i].BuildMatch = compareBuild(selfCommit, nodes[i].GitCommit)
	}

	// Alphabetical by name (owner ruling 2026-09-01). Names are unique within a
	// fleet, so this is a total order and the list cannot reshuffle between
	// polls -- which was the original reason for sorting here at all, since map
	// and config order left equal nodes moving around.
	//
	// This used to put the worst status first so a fault led the list. That
	// reads as "unsorted" to someone looking for a host: a warning node sorts
	// ahead of a healthy one whose name comes first, so on this fleet `fs` and
	// `vm-host-1` appeared above `ci-node-1`. A fleet list is used far more
	// often to find a named host than to discover an unknown fault, and the
	// fault is not hidden by name order -- every node carries its Status, and
	// the UI counts unreachable and degraded nodes separately from the list.
	//
	// The UI sorts the same way. It owns its own filter/sort/page pipeline, so
	// the two must be changed together or the API and the screen disagree about
	// what "the fleet" looks like.
	sort.SliceStable(nodes, func(i, j int) bool {
		return CompareNodeNames(nodes[i].Name, nodes[j].Name) < 0
	})
	return types.Fleet{Enabled: true, Nodes: nodes}
}

// summarize reduces a full snapshot to the handful of figures the fleet view
// shows. It is total: a nil snapshot yields an unreachable node rather than a
// zeroed one, because those must not look the same.
func summarize(name, url string, self bool, snap *types.Snapshot, fetchedAt time.Time, errMsg string) types.FleetNode {
	n := types.FleetNode{
		Name: name, URL: url, Self: self, Error: errMsg,
		AgeSeconds: time.Since(fetchedAt).Seconds(),
	}
	if snap == nil {
		n.Reachable = false
		n.Status = StatusUnreachable
		// No snapshot means no reading to be old, so the age is cleared rather
		// than left describing the last ATTEMPT.
		//
		// It was set from fetchedAt above, which for a peer that has answered
		// dates its data -- deliberately, so a failing poll does not make stale
		// data look fresh. For a peer that has NEVER answered there is no such
		// date, and fetchedAt is the moment of the last try: about two seconds
		// ago, always. A consumer rendering "unreachable for 2s" for a node
		// that has never once responded is being told a five-second blip when
		// it is a total absence, which is #69's defect in the field meant to
		// distinguish them.
		//
		// Zero is the sentinel and it is the same value the never-polled path
		// above produces, so both states agree: no age means no reading, not a
		// reading taken now.
		n.AgeSeconds = 0
		if n.Error == "" {
			n.Error = "no data"
		}
		return n
	}
	n.Reachable = errMsg == ""
	n.Hostname = snap.Host.Hostname
	n.Version = snap.Version
	n.GitCommit = snap.GitCommit
	n.BuildDate = snap.BuildDate
	n.UptimeSeconds = snap.Host.Uptime
	n.MemoryPercent = snap.Memory.UsedPercent
	n.SwapPercent = snap.Memory.SwapPercent

	for _, c := range snap.CPUs {
		n.CPUPercent += c.UsagePercent
	}
	if len(snap.CPUs) > 0 {
		n.CPUPercent /= float64(len(snap.CPUs))
	}
	if snap.CPUSummary.TotalThreads > 0 {
		n.LoadPerCore = snap.LoadAvg.Load1 / float64(snap.CPUSummary.TotalThreads)
	}

	for i := range snap.Disks {
		d := &snap.Disks[i]
		if d.TotalBytes > 0 && d.UsedPercent > n.WorstDiskPercent {
			n.WorstDiskPercent = d.UsedPercent
		}
		// Counted whether or not it is degraded: a numerator with no
		// denominator cannot say whether zero means "all well" or "nothing to
		// look at", and those are different answers for an operator.
		if d.RAID != nil {
			n.RAIDArrays++
			// Degraded is not the whole of "unhealthy". An inactive, suspended,
			// broken or unreadable array is not serving I/O at all, raises a
			// CRITICAL raid_not_active alert, and had Degraded false -- so the
			// roll-up called it ok while the alert called it critical, and the
			// node page showed it red. The predicate is alert's, so there
			// cannot be a second list of states to drift from the first.
			if d.RAID.Degraded || alert.RAIDNotOperational(d.RAID.State) {
				n.RAIDUnhealthy++
			}
			if d.RAID.Degraded {
				n.RAIDDegraded = true
			}
		}
		if d.SMARTEnabled {
			n.SMARTCapable++
			if !d.SMARTHealthy {
				n.SMARTUnhealthy++
			}
		}
		// The drives behind a hardware RAID controller count too. They are not
		// block devices and never appear in the disk list, so a denominator
		// summed over that list alone would report ci-node-1 as having three
		// SMART-capable devices while six readable drives sat behind its PERC.
		// That is #44's defect surviving its own fix: a denominator that does
		// not know what it left out.
		for _, m := range d.Members {
			if !m.SMARTEnabled {
				continue
			}
			n.SMARTCapable++
			if !m.SMARTHealthy {
				n.SMARTUnhealthy++
			}
		}
		if d.Temperature > n.MaxTempC {
			n.MaxTempC = d.Temperature
		}
		// A member's temperature is the drive's own, and on a host whose only
		// spinning disks are behind a controller it is the only disk
		// temperature there is.
		for _, m := range d.Members {
			if m.Temperature > n.MaxTempC {
				n.MaxTempC = m.Temperature
			}
		}
	}
	for _, c := range snap.Sensors.CoreTemps {
		if c.TempCelsius > n.MaxTempC {
			n.MaxTempC = c.TempCelsius
		}
	}
	for _, pp := range snap.Sensors.PackagePower {
		n.PowerW += pp.PowerW
	}
	for _, g := range snap.GPUs {
		n.PowerW += g.PowerDrawW
	}
	for i := range snap.Networks {
		if snap.Networks[i].IsLoopback {
			continue
		}
		n.NetRxRate += snap.Networks[i].BytesRecvRate
		n.NetTxRate += snap.Networks[i].BytesSentRate
	}

	for _, a := range snap.Alerts {
		switch a.Severity {
		case types.SeverityCritical:
			n.CriticalCount++
		case types.SeverityWarning:
			n.WarningCount++
		}
	}

	switch {
	case !n.Reachable:
		n.Status = StatusUnreachable
	case n.CriticalCount > 0:
		n.Status = StatusCritical
	case n.WarningCount > 0:
		n.Status = StatusWarning
	default:
		n.Status = StatusHealthy
	}
	return n
}

// ErrPlaintextPeer reports a peer configured with an http URL on a fleet that
// has switched plaintext off.
//
// It names the remedy, because the two ways out are not equivalent and an
// operator reading this at three in the morning should not have to weigh them:
// giving the peer an https URL is the fix, and turning allow_plaintext back on
// is the retreat.
type ErrPlaintextPeer struct {
	Peer string
}

func (e *ErrPlaintextPeer) Error() string {
	return fmt.Sprintf(
		"peer %s is configured with a plaintext http URL and fleet.tls.require_tls is set; "+
			"give it an https URL, or clear fleet.tls.require_tls to accept plaintext again",
		e.Peer)
}

// ErrPeerScheme reports a peer URL whose scheme is neither http nor https.
//
// Without this the request fails inside the HTTP client with a message about
// an unsupported protocol and no mention of which peer caused it, which in a
// fleet of ten is most of the work of finding it.
type ErrPeerScheme struct {
	Peer   string
	Scheme string
}

func (e *ErrPeerScheme) Error() string {
	scheme := e.Scheme
	if scheme == "" {
		scheme = "(none)"
	}
	return fmt.Sprintf("peer %s has URL scheme %q; fleet peers are http or https", e.Peer, scheme)
}

// ErrPeerURL reports a peer URL that will not parse.
type ErrPeerURL struct {
	Peer  string
	URL   string
	Cause error
}

func (e *ErrPeerURL) Error() string {
	return fmt.Sprintf("peer %s has an unparseable URL %q: %v", e.Peer, e.URL, e.Cause)
}

func (e *ErrPeerURL) Unwrap() error { return e.Cause }

// ErrPeerStatus reports a peer that answered with a non-OK HTTP status.
//
// Endpoint is the path that was asked for, so the message can say what the
// node could not do rather than only what it returned.
type ErrPeerStatus struct {
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
	"/api/config": true,
}

func (e *ErrPeerStatus) Error() string {
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
func (p *Poller) PeerConfig(name string) (*types.ConfigView, error) {
	cfg := p.cfgFn()
	var url string
	for _, peer := range cfg.Peers {
		if peer.Name == name {
			url = strings.TrimRight(peer.URL, "/") + "/api/config"
			break
		}
	}
	if url == "" {
		return nil, &ErrUnknownPeer{Peer: name}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.logger.Debug("fleet: closing peer config body", "peer", name, "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, &ErrPeerStatus{Peer: name, Endpoint: "/api/config", Status: resp.StatusCode}
	}

	var view types.ConfigView
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSnapshotBytes)).Decode(&view); err != nil {
		return nil, err
	}
	return &view, nil
}

// ErrUnknownPeer reports a request for a node that is not configured.
type ErrUnknownPeer struct{ Peer string }

func (e *ErrUnknownPeer) Error() string { return "unknown fleet node " + e.Peer }
