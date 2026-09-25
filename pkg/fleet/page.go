package fleet

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Filling a page with subtree nodes (#190).
//
// A subtree node is known from polled state by its child's index entry: name,
// host name, status, age, reasons, build. Its figures are its summary, and the
// summary is fetched from the child that owns it only when a page shows it --
// the one thing a parent asks its children for on request (owner ruling
// 2026-09-23). The fetch is bounded every way an unauthenticated request can
// make it large:
//
//   - A summary fetched within the poll interval is reused, keyed by the child
//     it came from and that child's address, so a repointed child's data is
//     never served under the new address.
//   - Any one batch of names is fetched once at a time; concurrent requests
//     for the same batch wait for that fetch (flight.go), which no one asker's
//     budget or departure can fail for the others.
//   - At most maxConcurrentFetches fetches are in flight at once for views,
//     and separately for parents filling their pages, so neither can starve
//     the other.
//   - At most maxFetchedPerView subtree summaries are fetched for any one
//     view; nodes beyond that, and every subtree node of a whole-fleet view,
//     are served from the cache or from their index entries (Source "index").
//
// Whatever is shown agrees with the index -- the same polled state the
// roll-up is counted from -- and with itself: a summary whose alerts say
// something other than the index's status is older or newer than the index,
// and the node is shown from the index instead, so a card never calls a node
// healthy beside a roll-up that counts it unreachable, nor carries a status
// its own alert counts contradict.

const (
	// namesPerRequest bounds how many nodes one GET /api/fleet/nodes asks
	// for, so a page's names never make a URL longer than a proxy will pass.
	namesPerRequest = 100
	// maxNamesPerRequest bounds GET /api/fleet/nodes; a parent asks for at
	// most namesPerRequest at a time, and this refuses anything far beyond.
	maxNamesPerRequest = 1000
	// maxConcurrentFetches bounds the page fetches in flight from this node.
	maxConcurrentFetches = 8
	// maxFetchedPerView bounds the subtree summaries one view fetches: the
	// largest page the dashboard offers.
	maxFetchedPerView = 250
)

// QueryParamName is the repeated parameter of GET /api/fleet/nodes.
const QueryParamName = "name"

// cachedNode is one subtree node's summary as fetched from its child.
type cachedNode struct {
	node      types.FleetNode
	via       string
	childURL  string
	fetchedAt time.Time
}

// materialize turns rows into whole FleetNodes: direct nodes as they are,
// subtree nodes from the cache or, when fetch is set and the view is within
// its bound, from their children now.
func (p *Poller) materialize(ctx context.Context, t *tree, rows []row, fetch bool) []types.FleetNode {
	out := make([]types.FleetNode, len(rows))
	cfg := p.cfgFn()
	reuse := p.detailReuse(cfg)
	selfCommit := ""
	for _, n := range t.direct {
		if n.Self {
			selfCommit = n.GitCommit
		}
	}

	type batchKey struct{ via, childURL string }
	missing := map[batchKey][]int{}
	fetched := 0
	now := t.now
	for i, r := range rows {
		switch {
		case r.direct != nil:
			out[i] = *r.direct
			continue
		case r.sub.unknown != "":
			out[i] = unknownNode(r, r.sub.unknown, t.now)
			continue
		}
		if v, ok := p.pageCache.Load(r.name); ok {
			c := v.(*cachedNode)
			if c.via == r.sub.via && c.childURL == r.sub.childURL && now.Sub(c.fetchedAt) < reuse && agrees(c.node, r) {
				out[i] = fromChild(c.node, r, selfCommit, t.now)
				continue
			}
		}
		if !fetch || fetched >= maxFetchedPerView {
			out[i] = indexNode(r, selfCommit, t.now)
			continue
		}
		fetched++
		k := batchKey{r.sub.via, r.sub.childURL}
		missing[k] = append(missing[k], i)
	}

	var wg sync.WaitGroup
	for k, idx := range missing {
		for start := 0; start < len(idx); start += namesPerRequest {
			batch := idx[start:min(start+namesPerRequest, len(idx))]
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.fillBatch(ctx, cfg, k.via, k.childURL, rows, batch, out, selfCommit, t.now)
			}()
		}
	}
	wg.Wait()
	return out
}

// fillBatch fills out[i] for each i in batch from one fetch of the child's
// GET /api/fleet/nodes, shared with any concurrent request for the same batch.
// A node whose summary the child could not give in time -- the request
// failed, or the child's own fetch below it did -- or whose summary no longer
// agrees with the index, is shown from its index entry, which still says what
// the node is, with its figures absent and why.
func (p *Poller) fillBatch(ctx context.Context, cfg config.Fleet, child, childURL string, rows []row, batch []int, out []types.FleetNode, selfCommit string, now time.Time) {
	names := make([]string, len(batch))
	for j, i := range batch {
		names[j] = rows[i].name
	}
	got, err := p.fetchNodes(ctx, cfg, child, childURL, names)
	for _, i := range batch {
		r := rows[i]
		n, ok := got[r.name]
		reason := ""
		switch {
		case err != nil:
			reason = err.Error()
		case !ok:
			reason = "not returned by " + child
		case !agrees(n, r):
			reason = "its figures changed since the last poll; they are shown on the next"
		default:
			out[i] = fromChild(n, r, selfCommit, now)
			continue
		}
		n = indexNode(r, selfCommit, now)
		if n.Error == "" {
			n.Error = "figures not available through " + child + ": " + reason
		}
		out[i] = n
	}
}

// fetchNodes fetches names from child, once for any number of concurrent
// askers (flight.go). A request relayed from this aggregator's own parent is
// run in the fill group, anything else in the view group, so neither can
// starve the other. Only the names asked for are kept from the answer.
func (p *Poller) fetchNodes(ctx context.Context, cfg config.Fleet, child, childURL string, names []string) (map[string]types.FleetNode, error) {
	group := p.viewFetches
	if _, fromParent := ctx.Value(ancestryKey{}).([]string); fromParent {
		group = p.fillFetches
	}
	key := childURL + "\n" + strings.Join(names, ",")
	v, err := group.do(ctx, key, cfg.Timeout(), func(work context.Context) (any, error) {
		peer, _, ok := p.resolvePeer(child)
		if !ok || peer.URL != childURL {
			return nil, &UnknownPeerError{Peer: child}
		}
		q := url.Values{}
		for _, n := range names {
			q.Add(QueryParamName, n)
		}
		var got types.FleetNodes
		if err := p.get(work, peer, cfg, endpointNodes+"?"+q.Encode(), maxSnapshotBytes, &got); err != nil {
			return nil, err
		}
		asked := make(map[string]bool, len(names))
		for _, n := range names {
			asked[n] = true
		}
		kept := make(map[string]types.FleetNode, len(names))
		now := p.now()
		for _, n := range got.Nodes {
			if !asked[n.Name] {
				continue
			}
			kept[n.Name] = n
			p.pageCache.Store(n.Name, &cachedNode{node: n, via: child, childURL: childURL, fetchedAt: now})
		}
		return kept, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string]types.FleetNode), nil
}

// agrees reports whether a summary says what the index says of its node: the
// same reachability, and the status its own alert counts give. A summary that
// disagrees is from another moment than the index the roll-up counts from.
func agrees(n types.FleetNode, r row) bool {
	e := r.sub.entry
	return n.Reachable == e.Reachable && nodeStatus(e.Reachable, &n.NodeSummary) == e.Status
}

// fromChild is a subtree node as its child sent it, restated from this
// aggregator's side: reached through the child, its build compared with this
// node's, and its status, reachability and age the index's, which the roll-up
// is counted from.
func fromChild(n types.FleetNode, r row, selfCommit string, now time.Time) types.FleetNode {
	n.Self = false
	n.Via = r.sub.via
	n.NameConflict = r.conflict
	n.BuildMatch = compareBuild(selfCommit, n.GitCommit)
	overlay(&n, r, now)
	if n.Alerts == nil {
		n.Alerts = []types.Alert{}
	}
	return n
}

// overlay states a subtree node's status, reachability and age as its index
// entry has them. Where the child's index says the node cannot be seen, so
// does the card, with the child's reason.
func overlay(n *types.FleetNode, r row, now time.Time) {
	e := r.sub.entry
	n.Status = e.Status
	n.Reachable = e.Reachable
	n.AgeSeconds = r.sub.age(now)
	if !e.Reachable {
		n.Error = strings.Join(e.Reasons, "; ")
	}
}

// indexNode is a subtree node known only by its index entry: its status, age,
// host name, build and reasons, and none of its figures, which were not
// fetched for this view (Source "index").
func indexNode(r row, selfCommit string, now time.Time) types.FleetNode {
	e := r.sub.entry
	n := types.FleetNode{
		Name:         r.name,
		Source:       types.SummarySourceIndex,
		BuildMatch:   compareBuild(selfCommit, e.Commit),
		NameMatch:    types.NameUnknown,
		Via:          r.sub.via,
		NameConflict: r.conflict,
		NodeSummary:  types.NodeSummary{Hostname: r.hostname, GitCommit: e.Commit, Alerts: []types.Alert{}},
	}
	overlay(&n, r, now)
	return n
}

// unknownNode is a subtree node this aggregator cannot currently see: only its
// index entry is known. It is unreachable from here, with its last known age
// and the reason, and none of its figures.
func unknownNode(r row, reason string, now time.Time) types.FleetNode {
	return types.FleetNode{
		Name:         r.name,
		Error:        "unreachable through " + r.sub.via + ": " + reason,
		AgeSeconds:   r.sub.age(now),
		Status:       types.NodeStatusUnreachable,
		BuildMatch:   types.BuildUnknown,
		NameMatch:    types.NameUnknown,
		Via:          r.sub.via,
		NameConflict: r.conflict,
		NodeSummary:  types.NodeSummary{Hostname: r.hostname, Alerts: []types.Alert{}},
	}
}

// forgetPages drops page-cache entries of children that are no longer
// configured at the address they were fetched from, and entries older than
// keep.
func (p *Poller) forgetPages(urls map[string]string, keep time.Duration) {
	now := p.now()
	p.pageCache.Range(func(k, v any) bool {
		c := v.(*cachedNode)
		if urls[c.via] != c.childURL || now.Sub(c.fetchedAt) > keep {
			p.pageCache.Delete(k)
		}
		return true
	})
}

// Nodes is GET /api/fleet/nodes: the named nodes of this fleet, whole, for a
// parent filling a page. Names not in the fleet, and names in conflict, are
// omitted. Subtree nodes are fetched from their children, as a page's are.
func (p *Poller) Nodes(ctx context.Context, names []string) types.FleetNodes {
	out := types.FleetNodes{Nodes: []types.FleetNode{}}
	t, ok := p.tree()
	if !ok {
		return out
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var rows []row
	for _, r := range t.rows {
		if want[r.name] && !r.conflict {
			rows = append(rows, r)
		}
	}
	out.Nodes = p.materialize(ctx, t, rows, true)
	return out
}

// ParseNames reads the names GET /api/fleet/nodes asks for. Each must be a
// usable node name, and there must be at least one and at most
// maxNamesPerRequest.
func ParseNames(raw string) ([]string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, &MalformedQueryError{Raw: raw, Cause: err}
	}
	names := values[QueryParamName]
	if len(names) == 0 {
		return nil, &InvalidQueryError{Param: QueryParamName, Reason: "at least one is required"}
	}
	if len(names) > maxNamesPerRequest {
		return nil, &InvalidQueryError{Param: QueryParamName, Value: strconv.Itoa(len(names)) + " names",
			Reason: "at most " + strconv.Itoa(maxNamesPerRequest) + " per request"}
	}
	for _, n := range names {
		if err := nodename.Validate(n); err != nil {
			return nil, &InvalidQueryError{Param: QueryParamName, Value: n, Reason: nodename.Rule}
		}
	}
	return names, nil
}
