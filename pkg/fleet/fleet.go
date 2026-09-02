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
	"sort"
	"strings"
	"sync"
	"time"

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

// peerState is the last result of polling one peer.
type peerState struct {
	snapshot  *types.Snapshot
	err       string
	fetchedAt time.Time
}

// LocalFunc supplies this node's own snapshot.
type LocalFunc func() *types.Snapshot

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

	mu    sync.RWMutex
	peers map[string]*peerState
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

// Run refreshes every peer until done is closed. The ticker period is re-read
// from configuration each cycle so a reload retimes it.
func (p *Poller) Run(done <-chan struct{}) {
	for {
		cfg := p.cfgFn()
		if len(cfg.Peers) > 0 {
			p.refresh(cfg)
		}
		select {
		case <-done:
			return
		case <-time.After(cfg.PollInterval()):
		}
	}
}

// refresh polls all peers concurrently. One slow peer must not delay the rest,
// so each has its own timeout and they run in parallel.
func (p *Poller) refresh(cfg config.Fleet) {
	var wg sync.WaitGroup
	for _, peer := range cfg.Peers {
		wg.Add(1)
		go func(peer config.Peer) {
			defer wg.Done()
			snap, err := p.fetch(peer, cfg.Timeout())
			st := &peerState{fetchedAt: time.Now()}
			if err != nil {
				st.err = err.Error()
				p.logger.Debug("fleet: peer unreachable", "peer", peer.Name, "error", err)
			} else {
				st.snapshot = snap
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
func (p *Poller) fetch(peer config.Peer, timeout time.Duration) (*types.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	url := strings.TrimRight(peer.URL, "/") + "/api/snapshot"
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
		if n.Error == "" {
			n.Error = "no data"
		}
		return n
	}
	n.Reachable = errMsg == ""
	n.Hostname = snap.Host.Hostname
	n.Version = snap.Version
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
		if d.RAID != nil && d.RAID.Degraded {
			n.RAIDDegraded = true
		}
		if d.SMARTEnabled && !d.SMARTHealthy {
			n.SMARTUnhealthy++
		}
		if d.Temperature > n.MaxTempC {
			n.MaxTempC = d.Temperature
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
