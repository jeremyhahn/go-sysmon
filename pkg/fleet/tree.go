package fleet

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Hierarchical aggregation (#190).
//
// One aggregator polling every node is bounded by what one process can poll
// and, above all, record: history is about 103 KB a peer a sample, about 1 GB a
// sample at ten thousand peers. So aggregators compose into a tree. An
// aggregator can have other aggregators as peers, and each records the history
// of its own direct peers only (topology A at every level).
//
// A child aggregator is recognised by what it says of itself: its summary's
// Aggregates is the size of its subtree. Each cycle its parent polls, besides
// that summary, its subtree -- an index of every node below it and the roll-up
// over them (GET /api/fleet/subtree). From that polled state the parent
// searches, orders, counts, pages and routes, exactly as it does for its own
// peers. The summaries of the nodes on a page are the one thing fetched on
// request (GET /api/fleet/nodes), from the child that owns them, and reused
// for the poll interval like a drill-in (owner ruling 2026-09-23, option A).
// See page.go.
//
// Names are fleet-wide identities (#37): unique across the whole tree, never
// qualified by path. A name found twice is a configuration fault; both nodes
// are listed and flagged, and neither is routed to (route.go). Loops are found
// by instance ID, not by name (ancestry.go).

// MaxTreeDepth is how many hops below an aggregator its deepest node may be,
// and how many aggregators a request may pass through. Four is a root,
// regions, sites and their nodes with one level to spare; a deeper tree is
// refused rather than composed, because every level adds a poll interval of
// staleness and a hop to every drill-in.
const MaxTreeDepth = 4

// maxSubtreeBytes bounds a child's subtree response: a few hundred bytes a
// node at most, so room for well over fifty thousand nodes.
const maxSubtreeBytes = 32 << 20

// maxAggregators bounds how many aggregators a child's subtree may name. Past
// it the subtree is refused, never composed from a truncated list.
const maxAggregators = 1024

// aggregatorAge is how many poll intervals a child's aggregators are named on
// after its reads start failing.
const aggregatorAge = 3

// maxBuildGroups bounds how many build groups a child's roll-up may carry. A
// group is a distinct commit, so a real fleet has a handful; the bound is
// against a child sending a group per node.
const maxBuildGroups = 256

// Tree endpoints a parent aggregator reads from a child.
const (
	endpointSubtree = "/api/fleet/subtree"
	endpointNodes   = "/api/fleet/nodes"
)

// childTree is the last subtree read from one child aggregator.
type childTree struct {
	subtree *types.FleetSubtree
	// fetchedAt dates the subtree, not the last attempt, as peerState's does.
	fetchedAt time.Time
	// err is why the latest read failed; the subtree is then stale, and every
	// node in it is shown as unknown.
	err string
	// refused is why the subtree is never composed: it names this aggregator
	// among its own (a loop), the child refused this aggregator's request, the
	// child is this aggregator itself, or the subtree is deeper than
	// MaxTreeDepth. It is not a read failure, and a later read failure does not
	// lift it.
	refused string
	// aggregators is the child's FleetSubtree.Aggregators from its last read,
	// kept for a refused subtree too: this aggregator names them on to its own
	// parent, so a loop stays found after it has been cut. aggregatorsAt dates
	// them; carried past a failed read, they expire unless the child
	// confirms them again (see pollSubtree).
	aggregators   []string
	aggregatorsAt time.Time
}

// NameConflictError reports a request for a name that two nodes in the tree
// carry. Neither is the answer, so neither is routed to.
type NameConflictError struct{ Node string }

func (e *NameConflictError) Error() string {
	return "fleet node " + e.Node + " is named twice in the fleet tree; rename one so every name is unique"
}

// SubtreeInvalidError reports a child's subtree that does not hold together: it
// is not composed, as a read failure is not.
type SubtreeInvalidError struct {
	Peer   string
	Reason string
}

func (e *SubtreeInvalidError) Error() string {
	return "fleet node " + e.Peer + " sent an invalid subtree: " + e.Reason
}

// pollSubtree reads a child aggregator's subtree into st.
//
// A failed or invalid read keeps the last good subtree, stale, and a refusal
// is carried forward whatever the read did: a child refused as a loop is not
// composed again just because the next read failed.
func (p *Poller) pollSubtree(peer config.Peer, cfg config.Fleet, prev, st *peerState) {
	var sub types.FleetSubtree
	err := p.get(context.Background(), peer, cfg, endpointSubtree, p.subtreeLimit, &sub)
	if err == nil {
		err = validateSubtree(peer.Name, &sub)
	}
	var refused *FleetRefusedError
	switch {
	case errors.As(err, &refused):
		st.tree = &childTree{refused: refused.Error()}
	case err != nil:
		p.logger.Debug("fleet: child subtree unreadable", "peer", peer.Name, "error", err)
		st.tree = &childTree{err: err.Error()}
		if prev != nil && prev.tree != nil {
			st.tree.refused = prev.tree.refused
			st.tree.subtree = prev.tree.subtree
			st.tree.fetchedAt = prev.tree.fetchedAt
			// Named on only while the child could still confirm them: a few
			// cycles, not for as long as its reads keep failing.
			if p.now().Sub(prev.tree.aggregatorsAt) < aggregatorAge*p.effectiveInterval(cfg) {
				st.tree.aggregators = prev.tree.aggregators
				st.tree.aggregatorsAt = prev.tree.aggregatorsAt
			}
		}
	default:
		read := p.now()
		st.tree = &childTree{subtree: &sub, fetchedAt: read, aggregators: sub.Aggregators,
			aggregatorsAt: read, refused: p.refusal(peer.Name, &sub)}
		if len(sub.Aggregators) > maxAggregators {
			// Refused for its size, not as a loop: nothing of it is composed,
			// so there is no loop through it to keep found, and naming its
			// IDs on would have every ancestor refuse this node in turn.
			st.tree.aggregators = nil
		}
		if st.tree.refused != "" {
			// A refused subtree is never composed, so it is not kept.
			st.tree.subtree = nil
		}
	}
}

// refusal says why a child's subtree must not be composed, or "".
//
// A loop -- A lists B, B lists C, C lists A, under whatever names -- shows as
// this aggregator's own instance ID among the aggregators the child's subtree
// names. Every aggregator on the loop finds its own ID below its child and
// refuses, so every edge of the loop is cut; and because a refused child's
// aggregators are still named upward, the evidence survives the cut and the
// loop stays cut, the same on every cycle, until an edge of it is removed.
func (p *Poller) refusal(child string, sub *types.FleetSubtree) string {
	if len(sub.Aggregators) > maxAggregators {
		// Too many to compose from: refused, not trusted in part. Dropping
		// some would drop the evidence of a loop with them.
		return child + " names " + strconv.Itoa(len(sub.Aggregators)) + " aggregators below it, more than the " +
			strconv.Itoa(maxAggregators) + " a fleet tree may have"
	}
	for _, id := range sub.Aggregators {
		if id == p.instanceID {
			return "the fleet's peer lists loop back to this aggregator through " + child +
				"; remove one peer entry on the loop"
		}
	}
	return depthRefusal(child, sub)
}

// validateSubtree holds a child's subtree to what this aggregator composes,
// and clamps the lists in it to their bounds. A child is another process on
// the network, and nothing it sends is trusted to be the size it should be.
func validateSubtree(child string, sub *types.FleetSubtree) error {
	invalid := func(reason string) error { return &SubtreeInvalidError{Peer: child, Reason: reason} }
	if sub.Rollup == nil {
		return invalid("no roll-up")
	}
	counts := map[types.NodeStatus]int{}
	selves := 0
	for i := range sub.Nodes {
		e := &sub.Nodes[i]
		switch {
		case e.Depth == 0:
			selves++
		case e.Depth < 0 || e.Depth > MaxTreeDepth:
			// A child composes nothing deeper than MaxTreeDepth, so no depth
			// past it is honest -- and bounding it here keeps every depth
			// this node adds to far from overflow.
			return invalid("a node at depth " + strconv.Itoa(e.Depth))
		}
		if err := nodename.Validate(e.Name); err != nil {
			return invalid("a node name that is not a usable node name")
		}
		if _, ok := statusCounters[e.Status]; !ok {
			return invalid("a node status that is not one")
		}
		counts[e.Status]++
		if len(e.Hostname) > maxHostnameBytes || len(e.Commit) > maxCommitBytes {
			return invalid("a host name or build longer than any is")
		}
		if len(e.Reasons) > types.RollupNames {
			e.Reasons = e.Reasons[:types.RollupNames]
		}
		for _, reason := range e.Reasons {
			if len(reason) > maxReasonBytes {
				return invalid("a reason longer than " + strconv.Itoa(maxReasonBytes) + " bytes")
			}
		}
	}
	if selves != 1 {
		return invalid(strconv.Itoa(selves) + " nodes at depth 0; there must be exactly one, the child itself")
	}
	if len(sub.Aggregators) == 0 {
		return invalid("it names no aggregators; it must name itself")
	}
	for _, id := range sub.Aggregators {
		if !validInstanceID(id) {
			return invalid("an aggregator ID that is not one")
		}
	}
	r := sub.Rollup
	if reason := rollupInconsistency(r, len(sub.Nodes), counts); reason != "" {
		return invalid(reason)
	}
	if len(r.Attention) > types.RollupNames {
		r.Attention = r.Attention[:types.RollupNames]
	}
	for i := range r.Attention {
		if len(r.Attention[i].Reasons) > types.RollupNames {
			r.Attention[i].Reasons = r.Attention[i].Reasons[:types.RollupNames]
		}
	}
	sortBuilds(r)
	if len(r.Builds) > maxBuildGroups {
		r.Builds = r.Builds[:maxBuildGroups]
	}
	for i := range r.Builds {
		if len(r.Builds[i].Nodes) > types.RollupNames {
			r.Builds[i].Nodes = r.Builds[i].Nodes[:types.RollupNames]
		}
	}
	return nil
}

// Bounds on the strings a child's index may carry: a host name is at most 253
// bytes by DNS, a build a commit hash, a reason an alert's message.
const (
	maxHostnameBytes = 253
	maxCommitBytes   = 64
	maxReasonBytes   = 1024
)

// rollupInconsistency says why a child's roll-up cannot be merged, or "". Every
// count must be a count and every sum a finite, non-negative sum, the four
// states must be the states its own index lists -- counts, taken from index
// entries each in exactly one state, so matching them partitions the nodes --
// and every denominator must be what rollup makes it: the reachable nodes.
// merge adds these into this node's own roll-up, so a child that fails any of
// them would put a number in the fleet's statement nobody measured.
func rollupInconsistency(r *types.FleetRollup, indexed int, counts map[types.NodeStatus]int) string {
	for _, n := range []int{r.Nodes, r.Healthy, r.Warning, r.Critical, r.Unreachable, r.NeverAnswered,
		r.Reporting, r.CPUUnknown, r.SwapUnknown, r.SubtreeErrors, r.NameConflicts, r.BuildGroups} {
		if n < 0 {
			return "a negative count"
		}
	}
	switch {
	case r.Nodes != indexed:
		return "its roll-up counts " + strconv.Itoa(r.Nodes) + " nodes and its index lists " + strconv.Itoa(indexed)
	case r.Healthy != counts[types.NodeStatusHealthy] || r.Warning != counts[types.NodeStatusWarning] ||
		r.Critical != counts[types.NodeStatusCritical] || r.Unreachable != counts[types.NodeStatusUnreachable]:
		return "its roll-up's states are not its index's"
	case r.Reporting > r.Nodes, r.NeverAnswered > r.Unreachable, r.CPUUnknown > r.Reporting, r.SwapUnknown > r.Reporting, r.NameConflicts > r.Nodes:
		return "a count larger than what it counts"
	case !measure(r.LongestAbsenceSeconds):
		return "an absence that is not a duration"
	}
	for _, e := range []*types.FleetExtreme{r.CPU, r.Memory, r.Disk, r.LoadPerCore, r.Temperature, r.SwapOut} {
		if e != nil && (!measure(e.Value) || nodename.Validate(e.Node) != nil) {
			return "an extreme that is not a reading of a node"
		}
	}
	if s := r.Power; s != nil && (!measure(s.Value) || s.Nodes < 0 || s.Nodes > s.Of || s.Of != r.Reporting) {
		return "a power total whose denominator is not its reachable nodes"
	}
	if s := r.Network; s != nil && (!measure(s.RxRate) || !measure(s.TxRate) || s.Nodes < 0 || s.Nodes > s.Of || s.Of != r.Reporting) {
		return "a network total whose denominator is not its reachable nodes"
	}
	if reason := capacityInconsistency(r.Capacity, r.Reporting); reason != "" {
		return reason
	}
	for _, h := range []*types.FleetHealth{r.SMART, r.RAID} {
		if h != nil && (h.Unhealthy < 0 || h.Unhealthy > h.Devices || h.Nodes < 0 || h.Nodes > h.Of || h.Of != r.Reporting) {
			return "a device count whose denominator is not its own"
		}
	}
	for _, a := range r.Attention {
		if _, ok := attentionRank[a.Status]; !ok || !measure(a.AgeSeconds) || a.Subtree < 0 {
			return "an attention entry that is not one"
		}
	}
	built := 0
	for _, b := range r.Builds {
		if b.Count < 1 || len(b.Commit) > maxCommitBytes {
			return "a build group that is not one"
		}
		built += b.Count
	}
	switch {
	case built > r.Reporting:
		return "build groups that count more nodes than it has reachable"
	case r.BuildGroups < len(r.Builds):
		return "more build groups than it says it has"
	}
	return ""
}

// measure reports whether v is a finite, non-negative reading.
func measure(v float64) bool {
	return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
}

// depthRefusal says why a child's subtree is too deep to compose, or "".
func depthRefusal(child string, sub *types.FleetSubtree) string {
	deepest := 0
	for _, e := range sub.Nodes {
		deepest = max(deepest, e.Depth)
	}
	if deepest+1 > MaxTreeDepth {
		return "the tree below " + child + " is " + strconv.Itoa(deepest+1) +
			" levels deep, beyond the " + strconv.Itoa(MaxTreeDepth) + " an aggregator composes"
	}
	return ""
}

// generation is what one refresh publishes: every peer's state and the
// children's subtrees indexed from those same states. Every view and every
// routing decision until the next refresh is built from one generation, so a
// child whose poll has just failed is never shown unreachable beside a
// roll-up that still merges its last subtree as healthy.
type generation struct {
	peers    map[string]*peerState
	subtrees *subtrees
	// cfgPeers is the peer list the generation was published for; order is
	// its indexes in name order, and states each one's state, aligned with
	// order -- sorted once here, for every request until the next refresh.
	cfgPeers []config.Peer
	order    []int
	states   []*peerState
	// flat is the peers' part of the view when the fleet is flat; nil when
	// it has child aggregators. See flat.go.
	flat *flatCache
}

// publish snapshots the peers' states and indexes the children's subtrees
// from them.
func (p *Poller) publish(cfg config.Fleet) {
	p.mu.RLock()
	peers := make(map[string]*peerState, len(p.peers))
	for name, st := range p.peers {
		peers[name] = st
	}
	p.mu.RUnlock()
	order, states := sortedPeers(cfg.Peers, peers)
	g := &generation{peers: peers, subtrees: buildSubtrees(cfg, peers),
		cfgPeers: cfg.Peers, order: order, states: states}
	g.flat = p.buildFlat(cfg, g)
	p.gen.Store(g)
}

// published is the latest generation, or an empty one before the first
// refresh.
func (p *Poller) published() *generation {
	if g := p.gen.Load(); g != nil {
		return g
	}
	return &generation{peers: map[string]*peerState{}, subtrees: &subtrees{}}
}

// row is one node of the fleet as the view lists it: a node this aggregator
// knows directly, or a node of a child's subtree, known by its index entry.
// It is kept small -- the subtree's particulars are behind a pointer shared
// with the refresh's index -- because a request lists every node of the fleet.
type row struct {
	name     string
	hostname string
	// direct is this node or a direct peer, whole; nil for a subtree node.
	direct *types.FleetNode
	// sub is a subtree node's index entry and route; nil for a direct node.
	sub      *subRow
	conflict bool
}

// subRow is what is known of a subtree node from its child's index. It is
// built at refresh and shared by every request until the next, so it is never
// written after.
type subRow struct {
	entry *types.FleetIndexEntry
	// via is the child aggregator the node is reached through, and childURL
	// the address that child's subtree was read from.
	via      string
	childURL string
	// readAt is when the child's subtree was read, which dates entry.
	readAt time.Time
	// unknown is why the child's subtree is not current -- the child is
	// unreachable, or its subtree could not be read -- or "" when it is.
	unknown string
}

// age is how old a subtree node's last reading is at now: its age when the
// subtree was read, plus the time since. Zero -- no reading -- stays zero for
// a node that never answered.
func (s *subRow) age(now time.Time) float64 {
	if s.entry.AgeSeconds == 0 && !s.entry.Reachable {
		return 0
	}
	return s.entry.AgeSeconds + now.Sub(s.readAt).Seconds()
}

// subtrees is everything below this aggregator's direct peers, as one refresh
// left it: the rows of every child's subtree in name order, and what routing
// and the roll-up need to know about them.
//
// It is built once per refresh, not per request, because it is the large part
// of the fleet -- ten thousand rows below a hundred children -- and it changes
// only when the children are polled. A request merges this node and its direct
// peers, which it reads live, into it.
type subtrees struct {
	// rows is every subtree node, in name order.
	rows []row
	// count is how many subtree nodes carry each name.
	count map[string]int
	// owner is the child each subtree name belongs to, for names only one
	// subtree node carries.
	owner map[string]string
	// flagged is the names a child has already found in conflict below it.
	flagged map[string]bool
	// childURL is the address each child's subtree was read from; a reload
	// that repoints a child retires its subtree until it is polled again.
	childURL map[string]string
	children map[string]*childTree
	unknown  map[string]unknownSubtree
	// subtreeError is why each child's subtree is not in the view, or not
	// current, when it is not.
	subtreeError map[string]string
	// refused is the children whose subtrees are never composed.
	refused map[string]bool
	// aggregators is every aggregator below this one that a child named,
	// refused children's included.
	aggregators []string
}

// unknownSubtree is a child's last known subtree, now unknown.
type unknownSubtree struct {
	entries   []types.FleetIndexEntry
	fetchedAt time.Time
}

// buildSubtrees indexes the children's subtrees from the peers' states.
func buildSubtrees(cfg config.Fleet, peers map[string]*peerState) *subtrees {
	s := &subtrees{
		count:        map[string]int{},
		owner:        map[string]string{},
		flagged:      map[string]bool{},
		childURL:     map[string]string{},
		children:     map[string]*childTree{},
		unknown:      map[string]unknownSubtree{},
		subtreeError: map[string]string{},
		refused:      map[string]bool{},
	}
	for _, peer := range cfg.Peers {
		st := peers[peer.Name]
		if st == nil || st.tree == nil || st.url != peer.URL {
			continue
		}
		ct := st.tree
		s.childURL[peer.Name] = peer.URL
		s.aggregators = append(s.aggregators, ct.aggregators...)
		switch {
		case ct.refused != "":
			s.refused[peer.Name] = true
			s.subtreeError[peer.Name] = ct.refused
			continue
		case ct.subtree == nil:
			// Never read: nothing is known below this child, not even how
			// much.
			s.subtreeError[peer.Name] = ct.err
			continue
		}
		// The subtree is current only when both the child and the read of its
		// subtree are: a child that stopped answering keeps its last index,
		// and every node in it is then unknown, never healthy (#46).
		reason := ct.err
		if st.err != "" {
			reason = st.err
		}
		if reason != "" {
			s.subtreeError[peer.Name] = reason
			u := unknownSubtree{fetchedAt: ct.fetchedAt}
			for _, e := range ct.subtree.Nodes {
				if e.Depth > 0 {
					u.entries = append(u.entries, e)
				}
			}
			s.unknown[peer.Name] = u
		} else {
			s.children[peer.Name] = ct
		}
		for j := range ct.subtree.Nodes {
			e := &ct.subtree.Nodes[j]
			if e.Depth == 0 {
				continue // the child itself, known directly
			}
			s.count[e.Name]++
			s.owner[e.Name] = peer.Name
			if e.NameConflict {
				s.flagged[e.Name] = true
			}
			s.rows = append(s.rows, row{name: e.Name, hostname: e.Hostname, sub: &subRow{
				entry: e, via: peer.Name, childURL: peer.URL, readAt: ct.fetchedAt, unknown: reason,
			}})
		}
	}
	for name, n := range s.count {
		if n > 1 {
			delete(s.owner, name)
		}
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		return CompareNodeNames(s.rows[i].name, s.rows[j].name) < 0
	})
	return s
}

// tree is the fleet this aggregator can see: every node, in name order, and
// what the roll-up needs to know about it.
type tree struct {
	// direct is this node and its direct peers, stamped and in name order:
	// what nodes() has always returned.
	direct []types.FleetNode
	// rows is every node, direct and subtree, in name order.
	rows []row
	// children is the composed child aggregators' subtrees, by child name.
	children map[string]*childTree
	// unknown is the subtrees that are not current, by child name.
	unknown map[string]unknownSubtree
	// refused counts the children whose subtrees are not composed at all.
	refused int
	// now is when the tree was built, on the Poller's clock: every age the
	// tree states is as at this moment, so the list and the roll-up agree.
	now time.Time
	// target is this aggregator's headroom target, the one its roll-up's
	// headroom is summed at; a child's sums at any other are left out.
	target float64
}

// tree builds the view's node list: this node and its direct peers merged in
// name order into the subtrees, all from one published generation. ok is
// false when no peers are configured.
func (p *Poller) tree() (*tree, bool) {
	return p.treeOf(p.published())
}

// treeOf is tree, from generation g.
func (p *Poller) treeOf(g *generation) (*tree, bool) {
	direct, ok := p.nodes(g)
	if !ok {
		return nil, false
	}
	sub := g.subtrees
	t := &tree{
		now:      p.now(),
		target:   p.cfgFn().HeadroomTarget(),
		direct:   direct,
		children: map[string]*childTree{},
		unknown:  map[string]unknownSubtree{},
	}

	// Which children's subtrees still belong in the view: those read from the
	// address each child is configured at now.
	current := map[string]bool{}
	if len(sub.childURL) > 0 {
		for _, peer := range p.cfgFn().Peers {
			if url, ok := sub.childURL[peer.Name]; ok && url == peer.URL {
				current[peer.Name] = true
			}
		}
	}
	// A name can only be carried twice if a subtree carries it: the direct
	// peers' names are unique by configuration (and none is this node's), so
	// a flat fleet -- no subtree rows -- needs no name index at all.
	var directNames map[string]bool
	if len(sub.rows) > 0 {
		directNames = make(map[string]bool, len(direct))
	}
	for i := range direct {
		n := &direct[i]
		if directNames != nil {
			directNames[n.Name] = true
		}
		if current[n.Name] {
			n.SubtreeError = sub.subtreeError[n.Name]
		}
	}
	for child := range current {
		if ct, ok := sub.children[child]; ok {
			t.children[child] = ct
		}
		if u, ok := sub.unknown[child]; ok {
			t.unknown[child] = u
		}
		if sub.refused[child] {
			t.refused++
		}
	}

	t.rows = make([]row, 0, len(direct)+len(sub.rows))
	i, j := 0, 0
	for i < len(direct) || j < len(sub.rows) {
		if j < len(sub.rows) && !current[sub.rows[j].sub.via] {
			j++
			continue
		}
		takeDirect := j >= len(sub.rows) ||
			(i < len(direct) && CompareNodeNames(direct[i].Name, sub.rows[j].name) <= 0)
		var r row
		if takeDirect {
			n := &direct[i]
			r = row{name: n.Name, hostname: n.Hostname, direct: n}
			i++
		} else {
			r = sub.rows[j]
			j++
		}
		shared := sub.count[r.name]
		if directNames[r.name] {
			shared++
		}
		if shared > 1 || (r.sub != nil && sub.flagged[r.name]) {
			r.conflict = true
			if r.direct != nil {
				r.direct.NameConflict = true
			}
		}
		t.rows = append(t.rows, r)
	}

	// This node's own card says how much of the fleet it aggregates, as its
	// summary tells its parent.
	for i := range t.direct {
		if t.direct[i].Self {
			t.direct[i].Aggregates = t.size()
		}
	}
	return t, true
}

// rollup composes the whole tree's roll-up (#190): this node and its direct
// peers counted here, each composed child's roll-up merged in, and the nodes of
// subtrees that cannot be seen counted as unknown. Build groups are all kept;
// the caller bounds them.
//
// With names zero the per-node lists are whole, as for a flat fleet: taken
// from every row -- the direct nodes themselves and the subtree nodes' index
// entries, which carry each node's reasons and build -- since the children's
// roll-ups name only their worst few.
func (t *tree) rollup(names int) *types.FleetRollup {
	// A composed child is counted by its own roll-up, so it is left out of the
	// local pass. With none -- a flat fleet -- the direct list is used as it
	// is, rather than copied node by node.
	local := t.direct
	if len(t.children) > 0 {
		local = make([]types.FleetNode, 0, len(t.direct))
		for _, n := range t.direct {
			if _, composed := t.children[n.Name]; !composed {
				local = append(local, n)
			}
		}
	}
	r := collect(local, names, t.target)
	for _, n := range t.direct {
		ct, composed := t.children[n.Name]
		if !composed {
			continue
		}
		child := *ct.subtree.Rollup
		// Every node the child names is reached through it, from here.
		child.Attention = make([]types.NodeAttention, len(ct.subtree.Rollup.Attention))
		for i, a := range ct.subtree.Rollup.Attention {
			a.Via = n.Name
			child.Attention[i] = a
		}
		merge(r, &child, names, t.target)
	}
	for _, u := range t.unknown {
		addUnknown(r, u.entries, t.now.Sub(u.fetchedAt).Seconds())
	}
	if names == 0 && len(t.rows) > len(t.direct) {
		r.Attention, r.Builds = t.wholeLists()
		sortBuilds(r)
	}
	for i := range r.Attention {
		if u, ok := t.unknown[r.Attention[i].Name]; ok && r.Attention[i].Via == "" {
			r.Attention[i].Subtree = len(u.entries)
		}
	}
	r.SubtreeErrors += t.refused
	// Every listed node carrying a shared name, across the whole tree: the
	// rows are the whole tree, and a conflict a child found is carried on its
	// index entries, so this replaces the children's counts rather than
	// adding to them.
	r.NameConflicts = 0
	for _, row := range t.rows {
		if row.conflict {
			r.NameConflicts++
		}
	}
	return r
}

// wholeLists is every node needing attention, worst first, and every build
// group whole, over every row of the tree.
func (t *tree) wholeLists() ([]types.NodeAttention, []types.FleetBuild) {
	needing := make([][]types.NodeAttention, len(attentionRank))
	builds := map[string]*types.FleetBuild{}
	group := func(commit, name string) {
		if commit == unstatedCommit {
			commit = ""
		}
		b, ok := builds[commit]
		if !ok {
			b = &types.FleetBuild{Commit: commit, Nodes: []string{}}
			builds[commit] = b
		}
		b.Count++
		b.Nodes = append(b.Nodes, name)
	}
	for _, r := range t.rows {
		switch {
		case r.direct != nil:
			n := r.direct
			if rank, ok := attentionRank[n.Status]; ok {
				needing[rank] = append(needing[rank], attention(n))
			}
			if n.Reachable {
				group(n.GitCommit, n.Name)
			}
		case r.sub.unknown != "":
			// Unknown with its child: counted, not named one by one; the
			// child's own entry says how many.
		default:
			e := r.sub.entry
			if rank, ok := attentionRank[e.Status]; ok {
				a := types.NodeAttention{Name: e.Name, Status: e.Status, Via: r.sub.via,
					Reasons: append([]string{}, e.Reasons...)}
				if !e.Reachable {
					a.AgeSeconds = r.sub.age(t.now)
				}
				needing[rank] = append(needing[rank], a)
			}
			if e.Reachable {
				group(e.Commit, e.Name)
			}
		}
	}
	attention := []types.NodeAttention{}
	for _, rank := range needing {
		attention = append(attention, rank...)
	}
	out := make([]types.FleetBuild, 0, len(builds))
	for _, b := range builds {
		out = append(out, *b)
	}
	return attention, out
}

// index is the tree as this aggregator serves it to its own parent: every
// node, this one at depth 0, with what the parent lists, searches, pages,
// routes and names in its attention list by.
func (t *tree) index() []types.FleetIndexEntry {
	out := make([]types.FleetIndexEntry, 0, len(t.rows))
	for _, r := range t.rows {
		switch {
		case r.direct != nil:
			n := r.direct
			depth := 1
			if n.Self {
				depth = 0
			}
			e := types.FleetIndexEntry{
				Name: r.name, Hostname: r.hostname, Status: n.Status,
				Reachable: n.Reachable, AgeSeconds: n.AgeSeconds,
				Depth: depth, NameConflict: r.conflict,
			}
			if _, needs := attentionRank[n.Status]; needs {
				e.Reasons = attention(n).Reasons
				if len(e.Reasons) > types.RollupNames {
					e.Reasons = e.Reasons[:types.RollupNames]
				}
			}
			if n.Reachable && n.GitCommit != unstatedCommit {
				e.Commit = n.GitCommit
			}
			out = append(out, e)
		case r.sub.unknown != "":
			out = append(out, types.FleetIndexEntry{
				Name: r.name, Hostname: r.hostname, Status: types.NodeStatusUnreachable,
				AgeSeconds: r.sub.age(t.now), Depth: r.sub.entry.Depth + 1, NameConflict: r.conflict,
				Reasons: []string{"unreachable through " + r.sub.via + ": " + r.sub.unknown},
			})
		default:
			e := *r.sub.entry
			e.Depth++
			e.NameConflict = r.conflict
			e.AgeSeconds = r.sub.age(t.now)
			out = append(out, e)
		}
	}
	return out
}

// size is how many nodes the tree holds besides this one: what this
// aggregator advertises as its summary's Aggregates.
func (t *tree) size() int {
	return len(t.rows) - 1
}

// SubtreeSize is how many nodes this aggregator's fleet view covers besides
// itself -- zero with no peers. A node's summary carries it (Aggregates), so a
// parent can tell a child aggregator from a leaf.
func (p *Poller) SubtreeSize() int {
	t, ok := p.tree()
	if !ok {
		return 0
	}
	return t.size()
}

// Subtree is what this aggregator serves its parent: the index of its whole
// tree and the roll-up in composable form. ok is false with no peers.
func (p *Poller) Subtree() (types.FleetSubtree, bool) {
	t, ok := p.tree()
	if !ok {
		return types.FleetSubtree{}, false
	}
	return types.FleetSubtree{Nodes: t.index(), Rollup: t.rollup(types.RollupNames), Aggregators: p.aggregators()}, true
}

// aggregators is this aggregator's ID and every one its children named, each
// once. Past maxAggregators it is all of them still, and a parent refuses the
// subtree: failing closed, rather than dropping IDs a loop may hide behind.
func (p *Poller) aggregators() []string {
	seen := map[string]bool{p.instanceID: true}
	out := []string{p.instanceID}
	for _, id := range p.published().subtrees.aggregators {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
