package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Routing a node's requests through the tree (#190).
//
// A drill-in, a node's settings and a history range are answered by the
// aggregator that polls the node: this one for itself and its direct peers,
// the owning child for a node of a child's subtree -- which may pass it on to
// its own child in turn. Every relay carries the chain of the request it
// relays (ancestry.go), so none can go round a loop.
//
// Routing is decided from the published generation alone: one lookup, no
// view built, however many times a request asks.

// RouteKind is how a name is answered.
type RouteKind int

// Route kinds.
const (
	// RouteUnknown is a name no node of this fleet carries.
	RouteUnknown RouteKind = iota
	// RouteLocal is this node.
	RouteLocal
	// RoutePeer is one of this aggregator's own peers.
	RoutePeer
	// RouteSubtree is a node of a child aggregator's subtree.
	RouteSubtree
	// RouteConflict is a name two nodes of the tree carry; neither is routed
	// to.
	RouteConflict
)

// Route is where a name's requests go.
type Route struct {
	Kind RouteKind
	// Peer is the direct peer for RoutePeer, and the owning child for
	// RouteSubtree.
	Peer config.Peer
	cfg  config.Fleet
}

// Resolve says how requests for name are answered.
func (p *Poller) Resolve(name string) Route {
	sub := p.published().subtrees
	local := name == p.localNameFn()
	peer, cfg, isPeer := p.resolvePeer(name)
	shared := sub.count[name]
	if local || isPeer {
		shared++
	}
	if shared > 1 || (sub.count[name] > 0 && sub.flagged[name]) {
		return Route{Kind: RouteConflict}
	}
	switch {
	case local:
		return Route{Kind: RouteLocal}
	case isPeer:
		return Route{Kind: RoutePeer, Peer: peer, cfg: cfg}
	}
	child, owned := sub.owner[name]
	if !owned {
		return Route{Kind: RouteUnknown}
	}
	// Owned through a child still configured at the address its subtree was
	// read from.
	peer, cfg, found := p.resolvePeer(child)
	if !found || peer.URL != sub.childURL[child] {
		return Route{Kind: RouteUnknown}
	}
	return Route{Kind: RouteSubtree, Peer: peer, cfg: cfg}
}

// err is the error a request for name meets when it is not answered here.
func (r Route) err(name string) error {
	switch r.Kind {
	case RouteConflict:
		return &ErrNameConflict{Node: name}
	case RouteUnknown:
		return &ErrUnknownPeer{Peer: name}
	}
	return nil
}

// Has reports whether name is a node of this fleet that requests may be
// answered for: this node, a configured peer, or a node of a child
// aggregator's subtree -- and no other node carries the name.
func (p *Poller) Has(name string) bool {
	switch p.Resolve(name).Kind {
	case RouteLocal, RoutePeer, RouteSubtree:
		return true
	}
	return false
}

// relayReuse is how long a relayed answer is reused for an identical relay:
// long enough that a burst of the same request costs one at each hop, short
// enough that a drill-in stays live.
const relayReuse = time.Second

// relay runs fn for one relay to r's child, once for any number of identical
// concurrent relays and reused for relayReuse (flight.go), so that no request
// costs more than one relay's worth of work at each hop.
func (p *Poller) relay(ctx context.Context, r Route, endpoint string, fn func(context.Context) (any, error)) (any, error) {
	chain, _ := ctx.Value(ancestryKey{}).([]string)
	key := r.Peer.URL + "\n" + endpoint + "\n" + strings.Join(chain, ",")
	return p.relays.do(ctx, key, r.cfg.Timeout(), fn)
}

// forwardSnapshot is a drill-in on a subtree node: asked of the child that
// owns it, which serves it from its own peer -- or asks its own child in turn.
// The envelope is decoded and served by this node, never passed through, so
// nothing but a snapshot envelope reaches the asker; a stale reading is still
// marked stale.
func (p *Poller) forwardSnapshot(ctx context.Context, name string, r Route, budget time.Duration) (types.PeerSnapshot, error) {
	opCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	endpoint := "/api/fleet/" + url.PathEscape(name) + "/snapshot"
	v, err := p.relay(opCtx, r, endpoint, func(work context.Context) (any, error) {
		var snap types.PeerSnapshot
		if err := p.get(work, r.Peer, r.cfg, endpoint, maxSnapshotBytes, &snap); err != nil {
			return nil, relayedError(name, err)
		}
		return snap, nil
	})
	if err != nil {
		return types.PeerSnapshot{}, err
	}
	return v.(types.PeerSnapshot), nil
}

// forwardConfig is a subtree node's settings, from the child that owns it,
// decoded and served by this node. The child has already withheld the node's
// topology; it is withheld again here, so a child on an older build cannot put
// an address on this node's wire.
func (p *Poller) forwardConfig(ctx context.Context, name string, r Route) (*types.ConfigView, error) {
	endpoint := "/api/fleet/" + url.PathEscape(name) + "/config"
	v, err := p.relay(ctx, r, endpoint, func(work context.Context) (any, error) {
		var view types.ConfigView
		if err := p.get(work, r.Peer, r.cfg, endpoint, maxSnapshotBytes, &view); err != nil {
			return nil, relayedError(name, err)
		}
		view = config.RedactPeerView(view)
		return &view, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*types.ConfigView), nil
}

// relayedError restates a child's answer about one of its nodes as this
// aggregator's: a child that no longer knows the node makes it unknown here
// too, and one that finds its name in conflict makes it a conflict here.
func relayedError(name string, err error) error {
	var status *ErrPeerStatus
	if errors.As(err, &status) {
		switch status.Status {
		case http.StatusNotFound:
			return &ErrUnknownPeer{Peer: name}
		case http.StatusConflict:
			return &ErrNameConflict{Node: name}
		}
	}
	return err
}

// ErrRelayTooLarge reports a child's relayed answer larger than this node will
// pass on.
type ErrRelayTooLarge struct{ Limit int }

func (e *ErrRelayTooLarge) Error() string {
	return "relayed answer is larger than " + strconv.Itoa(e.Limit) + " bytes"
}

// ErrRelayNotJSON reports a child's relayed answer that is not the JSON this
// node asked for: a different content type, or a body that does not decode as
// it. It is refused rather than passed on -- a child answering with HTML would
// otherwise be served on this node's origin.
type ErrRelayNotJSON struct {
	Peer   string
	Reason string
}

func (e *ErrRelayNotJSON) Error() string {
	return "fleet node " + e.Peer + " answered a relayed request with " + e.Reason
}

// ErrRelayStatus reports a child that answered a relayed request with an
// error status. Only the status comes back -- the server maps it onto its own
// -- never the child's words: its body is the child's to write, and nothing a
// child writes is served on this node's origin but JSON this node decoded.
type ErrRelayStatus struct {
	Peer   string
	Status int
}

func (e *ErrRelayStatus) Error() string {
	return "fleet node " + e.Peer + " answered the relayed request with status " + strconv.Itoa(e.Status)
}

// RelayHistory asks the child aggregator r routes to for node's history range
// (rawQuery is the history request's query). A subtree node's history is
// recorded and queried by the aggregator that polls it (topology A at every
// level), so the child's answer is the answer -- but it is decoded and served
// by this node, never passed through: it must be application/json and decode
// as node's history, and an error the child gives comes back as
// ErrRelayStatus, its status only. A child that refuses the relay as a loop is
// ErrFleetRefused.
func (p *Poller) RelayHistory(ctx context.Context, r Route, node, rawQuery string) (types.History, error) {
	if r.Kind != RouteSubtree {
		return types.History{}, &ErrUnknownPeer{Peer: node}
	}
	endpoint := "/api/history?" + rawQuery
	v, err := p.relay(ctx, r, endpoint, func(work context.Context) (any, error) {
		return p.fetchHistory(work, r, node, endpoint)
	})
	if err != nil {
		return types.History{}, err
	}
	return v.(types.History), nil
}

// fetchHistory is one relayed history request.
func (p *Poller) fetchHistory(ctx context.Context, r Route, node, endpoint string) (types.History, error) {
	resp, cancel, err := p.do(ctx, r.Peer, r.cfg, endpoint)
	if err != nil {
		return types.History{}, err
	}
	defer cancel()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.logger.Debug("fleet: closing relayed body", "peer", r.Peer.Name, "error", err)
		}
	}()
	switch {
	case resp.StatusCode == http.StatusLoopDetected:
		drain(p, r.Peer, resp)
		return types.History{}, &ErrFleetRefused{Peer: r.Peer.Name, Reason: resp.Header.Get(HeaderRefusal)}
	case resp.StatusCode != http.StatusOK:
		drain(p, r.Peer, resp)
		return types.History{}, &ErrRelayStatus{Peer: r.Peer.Name, Status: resp.StatusCode}
	}
	if media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || media != "application/json" {
		drain(p, r.Peer, resp)
		return types.History{}, &ErrRelayNotJSON{Peer: r.Peer.Name, Reason: "a content type other than application/json"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return types.History{}, &PeerRequestError{Peer: r.Peer.Name, Cause: err}
	}
	if len(body) > maxSnapshotBytes {
		return types.History{}, &PeerRequestError{Peer: r.Peer.Name, Cause: &ErrRelayTooLarge{Limit: maxSnapshotBytes}}
	}
	var h types.History
	if err := json.Unmarshal(body, &h); err != nil {
		return types.History{}, &ErrRelayNotJSON{Peer: r.Peer.Name, Reason: "a body that is not a history range"}
	}
	if h.Enabled && h.Node != node {
		return types.History{}, &ErrRelayNotJSON{Peer: r.Peer.Name, Reason: "the history of another node"}
	}
	types.ReplaceNilSlices(&h)
	return h, nil
}
