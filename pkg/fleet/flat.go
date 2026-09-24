package fleet

import (
	"sort"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The view of a flat fleet: one with no child aggregators (#190).
//
// A flat fleet must pay nothing for tree support, and its view should cost what
// the page costs, not what the fleet does. Everything about the peers changes
// only when they are polled, so each refresh builds their part of the view once
// -- every peer's node, stamped, in name order, and their roll-up -- and a
// request adds only what is live:
//
//   - this node, from its own latest collections, inserted in name order and
//     merged into the roll-up (merge is exact, whatever the split);
//   - the time since the refresh, added to every age, since an age dates a
//     reading, and a reading ages whether or not anyone asks;
//   - and the page, copied out of the cache.
//
// A reload that has replaced the peer list, or a build of this node different
// from the one the peers were compared with, is served the long way until the
// next refresh (flatViewLive).

// flatCache is the peers' part of a flat fleet's view, as one refresh built it.
type flatCache struct {
	// peers is every peer's node, stamped against selfCommit, in name order,
	// with ages as at builtAt.
	peers      []types.FleetNode
	selfCommit string
	builtAt    time.Time
	// whole and bounded are the peers' roll-up with whole and with bounded
	// lists; build groups are untrimmed, for merging this node in.
	whole, bounded *types.FleetRollup
}

// buildFlat builds g's flat cache, or nil when g's fleet is not flat.
func (p *Poller) buildFlat(cfg config.Fleet, g *generation) *flatCache {
	if len(g.subtrees.childURL) > 0 || len(cfg.Peers) == 0 {
		return nil
	}
	now := p.now()
	fc := &flatCache{selfCommit: p.selfNode().GitCommit, builtAt: now, peers: make([]types.FleetNode, len(g.order))}
	for k, i := range g.order {
		peerNode(&fc.peers[k], cfg.Peers[i], g.states[k], now)
		stamp(&fc.peers[k], fc.selfCommit)
	}
	fc.whole = collect(fc.peers, 0)
	fc.bounded = collect(fc.peers, types.RollupNames)
	return fc
}

// flatView is the view of a flat fleet, from the refresh's cache when it still
// describes the fleet, and the long way when it does not.
func (p *Poller) flatView(g *generation, q Query) types.Fleet {
	cfg := p.cfgFn()
	if len(cfg.Peers) == 0 {
		return types.Fleet{Enabled: false}
	}
	fc := g.flat
	self := p.selfNode()
	if fc == nil || !samePeers(g.cfgPeers, cfg.Peers) || self.GitCommit != fc.selfCommit {
		return p.flatViewLive(g, q)
	}
	stamp(&self, self.GitCommit)
	self.Aggregates = len(fc.peers)
	aged := p.now().Sub(fc.builtAt).Seconds()

	at := sort.Search(len(fc.peers), func(i int) bool {
		return CompareNodeNames(fc.peers[i].Name, self.Name) > 0
	})
	item := func(i int) *types.FleetNode {
		switch {
		case i == at:
			return &self
		case i > at:
			return &fc.peers[i-1]
		}
		return &fc.peers[i]
	}
	idx, matched, offset := pageIndexes(len(fc.peers)+1, func(i int) (string, string) {
		n := item(i)
		return n.Name, n.Hostname
	}, q)
	recording := p.historyCadence() > 0
	nodes := make([]types.FleetNode, len(idx))
	for j, i := range idx {
		nodes[j] = *item(i)
		if nodes[j].Self {
			continue
		}
		if nodes[j].Source != "" {
			nodes[j].AgeSeconds += aged
		}
		k := i
		if i > at {
			k = i - 1
		}
		nodes[j].History = p.peerHistory(cfg.Peers[g.order[k]], recording)
	}

	names := rollupNames(q)
	base := fc.bounded
	if names == 0 {
		base = fc.whole
	}
	r := agedRollup(base, aged)
	merge(r, collect([]types.FleetNode{self}, names), names)
	trimBuilds(r, names)

	return types.Fleet{
		Enabled: true,
		Nodes:   nodes,
		Page: &types.FleetPage{
			Query:   q.Search,
			Offset:  offset,
			Limit:   max(q.Limit, 0),
			Matched: matched,
			Total:   len(fc.peers) + 1,
		},
		Self:   &self,
		Rollup: r,
	}
}

// agedRollup is a copy of a cached roll-up, safe for merge to add into, with
// its absences aged by aged seconds: the longest absence, and each unreachable
// node's in the attention list. An age of zero is no reading, and stays zero.
func agedRollup(r *types.FleetRollup, aged float64) *types.FleetRollup {
	c := *r
	if c.LongestAbsenceSeconds > 0 {
		c.LongestAbsenceSeconds += aged
	}
	c.Attention = make([]types.NodeAttention, len(r.Attention))
	for i, a := range r.Attention {
		if a.Status == types.NodeStatusUnreachable && a.AgeSeconds > 0 {
			a.AgeSeconds += aged
		}
		c.Attention[i] = a
	}
	c.Builds = append([]types.FleetBuild(nil), r.Builds...)
	if r.Power != nil {
		v := *r.Power
		c.Power = &v
	}
	if r.Network != nil {
		v := *r.Network
		c.Network = &v
	}
	if r.SMART != nil {
		v := *r.SMART
		c.SMART = &v
	}
	if r.RAID != nil {
		v := *r.RAID
		c.RAID = &v
	}
	return &c
}

// flatViewLive is the view of a flat fleet built whole from the generation:
// every node, paged and rolled up, as it was before the fleet could be a tree
// (#15).
func (p *Poller) flatViewLive(g *generation, q Query) types.Fleet {
	nodes, ok := p.nodes(g)
	if !ok {
		return types.Fleet{Enabled: false}
	}
	var self *types.FleetNode
	for i := range nodes {
		if nodes[i].Self {
			// As tree() stamps it: how much of the fleet this node aggregates.
			nodes[i].Aggregates = len(nodes) - 1
			n := nodes[i]
			self = &n
			break
		}
	}
	shown, matched, offset := page(nodes, nodeKey, q)
	return types.Fleet{
		Enabled: true,
		Nodes:   shown,
		Page: &types.FleetPage{
			Query:   q.Search,
			Offset:  offset,
			Limit:   max(q.Limit, 0),
			Matched: matched,
			Total:   len(nodes),
		},
		Self:   self,
		Rollup: rollup(nodes, rollupNames(q)),
	}
}
