package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// handleFleet serves the aggregated fleet view: the page of nodes the q,
// offset and limit parameters select, and the whole-fleet roll-up. A node with
// no peers answers with Enabled=false rather than 404, so the dashboard can
// tell "this build has no fleet support" from "this node is standalone".
//
// A malformed parameter is 400 whether or not a fleet is configured: the
// request is wrong either way, and answering it would teach a client that its
// query works.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	q, err := fleet.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view := types.Fleet{Enabled: false}
	if s.fleet != nil {
		// A page holding nodes of child aggregators' subtrees fetches their
		// summaries from those children (#190), within the same write budget a
		// drill-in has.
		ctx, budget, ok := s.treeRequest(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		view = s.fleet.View(ctx, q)
	}
	writeJSON(w, view)
}

// treeRequest admits a request that may make this node ask its children
// (#190). A request from another aggregator carries the chain of aggregators
// it has passed through and the time its sender will still wait; one that has
// already passed through this node, or through more aggregators than a tree
// may have, is refused with 508. The rest get the request's context, carrying
// the chain on, and a budget: the smaller of the sender's and this response's
// own. The budget is returned apart from the context, because a drill-in
// treats its budget running out -- the peer was too slow -- differently from
// its asker going away. ok is false when the request has been answered.
func (s *Server) treeRequest(w http.ResponseWriter, r *http.Request) (context.Context, time.Duration, bool) {
	chain, err := fleet.ParseAncestry(r.Header.Get(fleet.HeaderAncestry))
	if err == nil {
		err = s.fleet.CheckAncestry(chain)
	}
	if err != nil {
		writeFleetError(w, err)
		return nil, 0, false
	}
	// Only another aggregator's budget is honoured -- one that says, with a
	// valid chain, where the request has come from. A browser, or anything
	// else, cannot shorten this node's budget and fail work it shares with
	// other askers.
	budget := drillInBudget()
	if sent := fleet.ParseBudget(r.Header.Get(fleet.HeaderBudget)); sent > 0 && len(chain) > 0 {
		budget = min(budget, sent)
	}
	ctx := r.Context()
	if len(chain) > 0 {
		ctx = fleet.WithAncestry(ctx, chain)
	}
	return ctx, budget, true
}

// writeFleetError answers err with its status, and for a refusal, the reason a
// parent aggregator reads it by.
func writeFleetError(w http.ResponseWriter, err error) {
	var loop *fleet.FleetLoopError
	var deep *fleet.TreeTooDeepError
	var refused *fleet.FleetRefusedError
	switch {
	case errors.As(err, &loop):
		w.Header().Set(fleet.HeaderRefusal, fleet.RefusalLoop)
	case errors.As(err, &deep):
		w.Header().Set(fleet.HeaderRefusal, fleet.RefusalDepth)
	case errors.As(err, &refused):
		w.Header().Set(fleet.HeaderRefusal, refused.Reason)
	}
	http.Error(w, err.Error(), fleetSnapshotStatus(err))
}

// handleFleetSubtree serves what a parent aggregator polls from this one each
// cycle (#190): the index of every node in this fleet and the roll-up in the
// form a parent composes. 404 on a node with no fleet: it is a leaf, and says
// so in its summary.
func (s *Server) handleFleetSubtree(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	if _, _, ok := s.treeRequest(w, r); !ok {
		return
	}
	sub, ok := s.fleet.Subtree()
	if !ok {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, sub)
}

// handleFleetNodes serves the named nodes of this fleet, whole: what a parent
// aggregator fetches to fill a page with nodes of this one's subtree (#190).
func (s *Server) handleFleetNodes(w http.ResponseWriter, r *http.Request) {
	names, err := fleet.ParseNames(r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.fleet == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	ctx, budget, ok := s.treeRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	writeJSON(w, s.fleet.Nodes(ctx, names))
}

// handleFleetRollup serves the whole-fleet roll-up alone, for a consumer that
// wants the fleet's state without any of its nodes. Like the per-node fleet
// resources it is 404 on a node with no fleet: there is no fleet to roll up.
func (s *Server) handleFleetRollup(w http.ResponseWriter, r *http.Request) {
	var rollup *types.FleetRollup
	if s.fleet != nil {
		rollup = s.fleet.Rollup()
	}
	if rollup == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, rollup)
}

// handleFleetSnapshot serves one node's full snapshot through the aggregator,
// which is what lets the dashboard render any peer's detail without the browser
// making a cross-origin request. The aggregator fetches it from the peer on
// demand; the poll cycle carries only summaries.
//
// A node that is not in the fleet, or has nothing to serve, is 404. A peer
// that cannot be read and has never been read is 502: the aggregator is
// answering, the node behind it is not, and that is a gateway failure rather
// than a missing resource.
func (s *Server) handleFleetSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.fleet == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	// Bounded by the response's write budget -- or the budget a parent
	// relaying this drill-in sent, when that is shorter -- so a peer slower
	// than that is answered with its error, or its last reading marked stale,
	// rather than with a response nobody is waiting for any more.
	ctx, budget, ok := s.treeRequest(w, r)
	if !ok {
		return
	}
	peer, err := s.fleet.PeerSnapshot(ctx, name, budget)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	// The envelope is served even when the peer is unreachable. Answering 5xx
	// would throw away the last good reading, which is often the most useful
	// thing on the page when a host has just gone; answering with a bare
	// snapshot would present it as current. The client gets both the data and
	// the fact that it is stale, and decides how to show it.
	writeJSON(w, peer)
}

// drillInBudget is how long a drill-in may wait on its peer: the response
// write deadline less a margin to write the answer in. fleet.timeout_ms can be
// set longer than the write deadline; without this a slow peer would be waited
// on past the point where any answer could still be sent.
func drillInBudget() time.Duration {
	return responseWriteTimeout - responseWriteTimeout/10
}

// fleetSnapshotStatus maps a PeerSnapshot error to its HTTP status.
func fleetSnapshotStatus(err error) int {
	var unknown *fleet.UnknownPeerError
	var noReading *fleet.NoReadingError
	var moved *fleet.PeerMovedError
	var conflict *fleet.NameConflictError
	switch {
	case errors.As(err, &unknown), errors.As(err, &noReading):
		return http.StatusNotFound
	case errors.As(err, &conflict):
		return http.StatusConflict
	case isRefusal(err):
		return http.StatusLoopDetected
	case isInvalidAncestry(err):
		return http.StatusBadRequest
	case relayedStatus(err) != 0:
		return relayedStatus(err)
	case errors.As(err, &moved):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// handleConfig serves the effective configuration and each value's origin. It
// is read-only by design: the service holds elevated capabilities on some hosts
// and carries no authentication of its own, so configuration is changed on
// disk and reloaded, never through the dashboard.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeJSON(w, types.ConfigView{})
		return
	}
	view := s.cfg.View()
	if s.mon != nil {
		view = withRunningInterval(view, s.mon.Interval())
	}
	writeJSON(w, view)
}

// withRunningInterval replaces the configured polling interval with the one the
// monitor is actually using.
//
// POST /api/interval changes the monitor directly -- it is what the dashboard's
// refresh-rate selector calls -- so the configured value goes stale the moment
// anyone picks a different rate. Reporting it here would leave GET /api/config
// and GET /api/interval giving two different accounts of one running service,
// which is the confusion the settings view exists to end. The source becomes
// "runtime" so the pair stays self-consistent and an operator can see that the
// number came from the dashboard rather than the file.
func withRunningInterval(view types.ConfigView, running time.Duration) types.ConfigView {
	if running <= 0 {
		return view
	}
	ms := strconv.Itoa(int(running.Milliseconds()))
	for i, e := range view.Entries {
		if e.Key != "server.interval_ms" {
			continue
		}
		// When they agree there is no override to report, and the configured
		// provenance is the more informative answer.
		if e.Value != ms {
			view.Entries[i].Value = ms
			view.Entries[i].Source = types.ConfigSourceRuntime
		}
		break
	}
	return view
}

// handleFleetConfig serves one node's effective configuration. Without it the
// settings view would show the aggregator's own settings while a peer is
// selected, which reads as that peer's configuration and is wrong.
func (s *Server) handleFleetConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.fleet == nil {
		http.Error(w, "fleet is not configured", http.StatusNotFound)
		return
	}
	if name == s.fleet.LocalName() {
		s.handleConfig(w, r)
		return
	}
	ctx, budget, ok := s.treeRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	view, err := s.fleet.PeerConfig(ctx, name)
	if err != nil {
		writeFleetError(w, err)
		return
	}
	writeJSON(w, view)
}

// There is deliberately no reload endpoint. The service carries no
// authentication of its own and is designed to sit behind an authenticating
// proxy, so no HTTP path changes its configuration: settings are edited on disk
// and picked up by SIGHUP or the file watcher, which deliver everything a
// reload endpoint would have. POST /api/interval remains the one runtime-state
// endpoint, and it retunes a transient refresh rate rather than configuration.

// isRefusal reports a request refused as a loop or as too deep, here or by a
// child.
func isRefusal(err error) bool {
	var loop *fleet.FleetLoopError
	var deep *fleet.TreeTooDeepError
	var refused *fleet.FleetRefusedError
	return errors.As(err, &loop) || errors.As(err, &deep) || errors.As(err, &refused)
}

// isInvalidAncestry reports a malformed ancestry header.
func isInvalidAncestry(err error) bool {
	var invalid *fleet.InvalidAncestryError
	return errors.As(err, &invalid)
}

// handleHistoryRequest routes GET /api/history before either build's handler
// answers it. A node of a child aggregator's subtree is recorded by that child
// (topology A at every level, #190), so its history is the child's answer,
// relayed as it came -- whether or not this node keeps any history itself. A
// name two nodes carry is refused: its series would be two machines under one
// instance label. Everything else is this node's own to answer.
func (s *Server) handleHistoryRequest(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if s.fleet == nil || node == "" {
		s.handleHistory(w, r)
		return
	}
	route := s.fleet.Resolve(node)
	switch route.Kind {
	case fleet.RouteConflict:
		http.Error(w, (&fleet.NameConflictError{Node: node}).Error(), http.StatusConflict)
		return
	case fleet.RouteSubtree:
	default:
		s.handleHistory(w, r)
		return
	}
	ctx, budget, ok := s.treeRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	h, err := s.fleet.RelayHistory(ctx, route, node, r.URL.RawQuery)
	if err != nil {
		slog.Warn("server: history relay failed", "node", node, "error", err)
		writeFleetError(w, err)
		return
	}
	// Served by this node, as JSON it wrote: never the child's headers or
	// bytes.
	writeJSON(w, h)
}

// relayedStatuses are the child statuses a relayed error keeps; any other is
// this node's gateway failure.
var relayedStatuses = map[int]bool{
	http.StatusBadRequest: true,
	http.StatusNotFound:   true,
	http.StatusConflict:   true,
}

// relayedStatus is the status a child's relayed error keeps here, or 0 when
// err is not a relayed status.
func relayedStatus(err error) int {
	var relayed *fleet.RelayStatusError
	if !errors.As(err, &relayed) {
		return 0
	}
	if relayedStatuses[relayed.Status] {
		return relayed.Status
	}
	return http.StatusBadGateway
}
