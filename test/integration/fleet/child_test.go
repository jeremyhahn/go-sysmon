//go:build integration

package fleet

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// A stand-in child aggregator: it describes a subtree of nodes it does not
// have, and answers for them, as a real child would -- or, on demand, as no
// real child would.

// instanceID is a well-formed aggregator instance ID, distinct for each i.
func instanceID(i int) string {
	return strings.Repeat(string(rune('B'+i%24)), 26)
}

// entry is one subtree node below a child.
func entry(name string, status types.NodeStatus, age float64) types.FleetIndexEntry {
	e := types.FleetIndexEntry{Name: name, Hostname: name + ".example", Status: status, Depth: 1,
		Reachable: status != types.NodeStatusUnreachable, AgeSeconds: age}
	if status != types.NodeStatusHealthy {
		e.Reasons = []string{string(status) + " on " + name}
	}
	if e.Reachable {
		e.Commit = "c0ffee"
	}
	return e
}

// validSubtree is the subtree a child named child, with instance ID id, serves
// over entries: itself at depth 0, the entries below it, and a roll-up that
// holds together.
func validSubtree(child, id string, entries []types.FleetIndexEntry) types.FleetSubtree {
	nodes := append([]types.FleetIndexEntry{{Name: child, Hostname: child + ".example",
		Status: types.NodeStatusHealthy, Reachable: true}}, entries...)
	r := &types.FleetRollup{Nodes: len(nodes), Attention: []types.NodeAttention{}, Builds: []types.FleetBuild{}}
	builds := map[string]*types.FleetBuild{}
	for _, e := range nodes {
		switch e.Status {
		case types.NodeStatusHealthy:
			r.Healthy++
		case types.NodeStatusWarning:
			r.Warning++
		case types.NodeStatusCritical:
			r.Critical++
		case types.NodeStatusUnreachable:
			r.Unreachable++
			if e.AgeSeconds == 0 {
				r.NeverAnswered++
			}
			r.LongestAbsenceSeconds = max(r.LongestAbsenceSeconds, e.AgeSeconds)
		}
		if e.Status != types.NodeStatusHealthy {
			r.Attention = append(r.Attention, types.NodeAttention{Name: e.Name, Status: e.Status,
				AgeSeconds: e.AgeSeconds, Reasons: e.Reasons})
		}
		if e.Reachable {
			r.Reporting++
			b, ok := builds[e.Commit]
			if !ok {
				b = &types.FleetBuild{Commit: e.Commit, Nodes: []string{}}
				builds[e.Commit] = b
			}
			b.Count++
			b.Nodes = append(b.Nodes, e.Name)
		}
	}
	r.SwapUnknown = r.Reporting
	for _, b := range builds {
		r.Builds = append(r.Builds, *b)
	}
	sort.Slice(r.Builds, func(i, j int) bool { return r.Builds[i].Commit < r.Builds[j].Commit })
	r.BuildGroups = len(r.Builds)
	return types.FleetSubtree{Nodes: nodes, Aggregators: []string{id}, Rollup: r}
}

// childStandIn is a stand-in child aggregator.
type childStandIn struct {
	*standIn
	name    string
	id      string
	entries []types.FleetIndexEntry

	mu sync.Mutex
	// nodes answers GET /api/fleet/nodes for each name; a name absent from
	// it is left out of the answer.
	nodes map[string]types.FleetNode
}

// newChild starts a child aggregator over entries that answers as a real one
// does: its summary, its subtree, and the summaries of the nodes it is asked
// for.
func newChild(t *testing.T, name string, i int, entries []types.FleetIndexEntry) *childStandIn {
	t.Helper()
	c := &childStandIn{standIn: newStandIn(t), name: name, id: instanceID(i), entries: entries, nodes: map[string]types.FleetNode{}}
	for _, e := range entries {
		s := leafSummary(e.Name)
		s.GitCommit = e.Commit
		if e.Status == types.NodeStatusWarning {
			s.WarningCount = 1
			s.Alerts = []types.Alert{alertOf(types.SeverityWarning, "load", e.Name)}
		}
		if e.Status == types.NodeStatusCritical {
			s.CriticalCount = 1
			s.Alerts = []types.Alert{alertOf(types.SeverityCritical, "raid", e.Name)}
		}
		c.nodes[e.Name] = types.FleetNode{Name: e.Name, Reachable: e.Reachable, Status: e.Status, NodeSummary: s}
	}
	summary := leafSummary(name)
	summary.Aggregates = len(entries)
	summary.InstanceID = c.id
	c.handle("/api/summary", jsonAnswer(summary))
	c.handle("/api/fleet/subtree", jsonAnswer(validSubtree(name, c.id, entries)))
	c.handle("/api/fleet/nodes", c.serveNodes)
	return c
}

// setNode replaces what GET /api/fleet/nodes answers for name.
func (c *childStandIn) setNode(name string, n types.FleetNode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes[name] = n
}

// dropNode leaves name out of GET /api/fleet/nodes answers.
func (c *childStandIn) dropNode(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, name)
}

func (c *childStandIn) serveNodes(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out := types.FleetNodes{Nodes: []types.FleetNode{}}
	c.mu.Lock()
	for _, name := range values["name"] {
		if n, ok := c.nodes[name]; ok {
			out.Nodes = append(out.Nodes, n)
		}
	}
	// A child answers with more than it was asked for: the extra is ignored.
	out.Nodes = append(out.Nodes, types.FleetNode{Name: "unasked", Reachable: true, Status: types.NodeStatusHealthy})
	c.mu.Unlock()
	jsonAnswer(out)(w, r)
}
