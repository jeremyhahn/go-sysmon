//go:build integration

package fleet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// editableConfig is a config file a test rewrites while the server runs.
func editableConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// reload rewrites the config at path and has srv reload it on SIGHUP,
// returning once the reload is logged.
func reload(t *testing.T, srv *harness.Server, path, body string, reloads int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
	srv.Signal(t, syscall.SIGHUP)
	seen := 0
	srv.Logs.Await(t, "the SIGHUP reload to be applied", func(r harness.LogRecord) bool {
		if r.Msg() == "config: reloaded on SIGHUP" {
			seen++
		}
		return seen >= reloads
	})
}

// TestReload_PeersAreAddedRemovedAndRepointedOnSIGHUP checks that a reload
// changes the fleet at once: a peer removed is gone, one added is listed as
// not polled yet, and one repointed at another address is never shown with
// the old address's reading -- the view is honest before the next poll comes.
func TestReload_PeersAreAddedRemovedAndRepointedOnSIGHUP(t *testing.T) {
	a, a2, b, c := newStandIn(t), newStandIn(t), newStandIn(t), newStandIn(t)
	for name, s := range map[string]*standIn{"a": a, "a2": a2, "b": b, "c": c} {
		sum := leafSummary(name)
		s.handle("/api/summary", jsonAnswer(sum))
		s.handle("/api/snapshot", jsonAnswer(types.Snapshot{NodeName: name, Host: types.HostInfo{Hostname: name + ".example"}}))
	}
	path := editableConfig(t, fleetYAML("hub", onePoll, 1500, []peer{{"a", a.URL}, {"b", b.URL}}, ""))
	agg := harness.Serve(t, harness.ServeOptions{ConfigPath: path})
	awaitFleet(t, agg.Addr, "the one poll", polled)
	var before types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/a/snapshot", &before)
	if before.Snapshot == nil || before.Snapshot.NodeName != "a" {
		t.Fatalf("a's drill-in before the reload = %+v", before)
	}

	reload(t, agg, path, fleetYAML("hub", onePoll, 1500, []peer{{"a", a2.URL}, {"c", c.URL}}, ""), 1)

	v := fleetOf(t, agg.Addr, "")
	if got := strings.Join(names(v), ","); got != "a,c,hub" {
		t.Errorf("after the reload the fleet is %s; want a, c and hub: b removed, c added", got)
	}
	for _, name := range []string{"a", "c"} {
		if n := node(t, v, name); n.Error != "not polled yet" || n.Reachable {
			t.Errorf("%s after the reload = %+v; want not polled yet -- a's reading was another address's", name, n)
		}
	}
	var after types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/a/snapshot", &after)
	if after.Snapshot == nil || after.Snapshot.NodeName != "a2" {
		t.Errorf("a's drill-in after it was repointed = %+v; want the new address's snapshot", after.Snapshot)
	}
	if status, _, _ := request(t, agg.Addr, "/api/fleet/b/snapshot", nil); status != http.StatusNotFound {
		t.Errorf("a removed peer's drill-in = %d; want 404", status)
	}
	if v.Rollup.Nodes != 3 || v.Rollup.NeverAnswered != 2 {
		t.Errorf("roll-up after the reload = %d nodes, %d never answered; want 3 and 2", v.Rollup.Nodes, v.Rollup.NeverAnswered)
	}
}

// TestReload_TheLastPeerRemovedMakesAStandaloneNode checks that a reload
// removing every peer turns the fleet off, and adding one back turns it on
// with that peer polled afresh.
func TestReload_TheLastPeerRemovedMakesAStandaloneNode(t *testing.T) {
	a := newStandIn(t)
	a.handle("/api/summary", jsonAnswer(leafSummary("a")))
	path := editableConfig(t, fleetYAML("hub", 1000, 1500, []peer{{"a", a.URL}}, ""))
	agg := harness.Serve(t, harness.ServeOptions{ConfigPath: path})
	awaitFleet(t, agg.Addr, "a to be read", func(v types.Fleet) bool { return polled(v) && node(t, v, "a").Reachable })

	reload(t, agg, path, fleetYAML("hub", 1000, 1500, nil, ""), 1)
	if v := fleetOf(t, agg.Addr, ""); v.Enabled || len(v.Nodes) != 0 {
		t.Errorf("the fleet with no peers = %+v; want disabled", v)
	}
	for _, path := range []string{"/api/fleet/rollup", "/api/fleet/subtree"} {
		if status, _, body := request(t, agg.Addr, path, nil); status != http.StatusNotFound || !strings.Contains(string(body), "fleet is not configured") {
			t.Errorf("GET %s with no peers = %d %q; want 404, not configured", path, status, body)
		}
	}
	var none types.FleetNodes
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/nodes?name=a", &none)
	if len(none.Nodes) != 0 {
		t.Errorf("GET /api/fleet/nodes with no peers = %v; want none", none.Nodes)
	}
	var self types.NodeSummary
	harness.GetJSON(t, "http://"+agg.Addr+"/api/summary", &self)
	if self.Aggregates != 0 || self.InstanceID != "" {
		t.Errorf("a standalone node's summary = aggregates %d, instance %q; want neither: it is a leaf", self.Aggregates, self.InstanceID)
	}

	hits := a.hitsOf("/api/summary")
	reload(t, agg, path, fleetYAML("hub", 1000, 1500, []peer{{"a", a.URL}}, ""), 2)
	awaitFleet(t, agg.Addr, "a to be read again", func(v types.Fleet) bool {
		return v.Enabled && polled(v) && node(t, v, "a").Reachable && a.hitsOf("/api/summary") > hits
	})
}

// TestReload_AStandaloneNodeAnswersTheFleetRoutes checks a node with no fleet
// configured at all: the fleet view is off, and every fleet route says so, or
// answers for this node alone.
func TestReload_AStandaloneNodeAnswersTheFleetRoutes(t *testing.T) {
	srv := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: alone\n"})
	if v := fleetOf(t, srv.Addr, ""); v.Enabled {
		t.Errorf("a standalone node's fleet = %+v; want disabled", v)
	}
	for path, status := range map[string]int{
		"/api/fleet/rollup":          http.StatusNotFound,
		"/api/fleet/subtree":         http.StatusNotFound,
		"/api/fleet/other/snapshot":  http.StatusNotFound,
		"/api/fleet/other/config":    http.StatusNotFound,
		"/api/fleet?limit=nope":      http.StatusBadRequest,
		"/api/fleet/nodes?name=a%20": http.StatusBadRequest,
	} {
		if got, _, body := request(t, srv.Addr, path, nil); got != status {
			t.Errorf("GET %s on a standalone node = %d (%s); want %d", path, got, body, status)
		}
	}
	var own types.PeerSnapshot
	harness.GetJSON(t, "http://"+srv.Addr+"/api/fleet/alone/snapshot", &own)
	if !own.Reachable || own.Snapshot == nil {
		t.Errorf("a standalone node's own drill-in = %+v; want its snapshot", own)
	}
	var cfg types.ConfigView
	harness.GetJSON(t, "http://"+srv.Addr+"/api/fleet/alone/config", &cfg)
	if len(cfg.Entries) == 0 {
		t.Error("a standalone node's own settings through the fleet route are empty")
	}
}

// TestReload_ALoopBetweenTwoAggregatorsIsCut forms a loop with real processes
// -- each lists the other -- and checks that both find it and cut it, and
// that neither's fleet grows without bound.
func TestReload_ALoopBetweenTwoAggregatorsIsCut(t *testing.T) {
	leaf := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: leaf\n"})
	pathA := editableConfig(t, fleetYAML("a", 1000, 1500, []peer{{"leaf", "http://" + leaf.Addr}}, ""))
	a := harness.Serve(t, harness.ServeOptions{ConfigPath: pathA})
	b := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("b", 1000, 1500, []peer{{"a", "http://" + a.Addr}}, "")})
	awaitFleet(t, b.Addr, "b to compose a's subtree", func(v types.Fleet) bool {
		return v.Enabled && len(v.Nodes) == 3
	})

	reload(t, a, pathA, fleetYAML("a", 1000, 1500, []peer{{"leaf", "http://" + leaf.Addr}, {"b", "http://" + b.Addr}}, ""), 1)

	loop := func(v types.Fleet, child string) bool {
		for _, n := range v.Nodes {
			if n.Name == child && strings.Contains(n.SubtreeError, "loop back to this aggregator") {
				return true
			}
		}
		return false
	}
	va := awaitFleet(t, a.Addr, "a to find the loop through b", func(v types.Fleet) bool { return loop(v, "b") })
	vb := awaitFleet(t, b.Addr, "b to find the loop through a", func(v types.Fleet) bool { return loop(v, "a") })
	if len(va.Nodes) > 3 || len(vb.Nodes) > 3 {
		t.Errorf("fleets around the loop = %v and %v; want neither composing the other", names(va), names(vb))
	}
	if va.Rollup.SubtreeErrors == 0 {
		t.Errorf("a's roll-up = %+v; want the refused subtree counted", va.Rollup)
	}
	// With a's subtree cut, b no longer routes to what is below a.
	if status, _, _ := request(t, b.Addr, "/api/fleet/leaf/snapshot", nil); status != http.StatusNotFound {
		t.Errorf("leaf's drill-in through b, with the loop cut = %d; want 404", status)
	}
}

// TestReload_AChildThatStopsAnsweringLeavesItsNodesUnknown checks that the
// nodes below a child aggregator that stops answering are shown as unknown --
// never healthy -- with how long it has been, and counted so in the roll-up,
// both when the child is down and when only its subtree cannot be read.
func TestReload_AChildThatStopsAnsweringLeavesItsNodesUnknown(t *testing.T) {
	entries := []types.FleetIndexEntry{entry("u-1", types.NodeStatusHealthy, 0), entry("u-2", types.NodeStatusUnreachable, 0)}
	child := newChild(t, "mid", 0, entries)
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", 1000, 1500, []peer{{"mid", child.URL}}, "")})
	awaitFleet(t, agg.Addr, "mid's subtree to be composed", func(v types.Fleet) bool { return v.Enabled && len(v.Nodes) == 4 })

	summary := child.hitsOf("/api/summary")
	child.handle("/api/summary", rawAnswer(http.StatusServiceUnavailable, "text/plain", "restarting"))
	v := awaitFleet(t, agg.Addr, "mid's absence to be noticed", func(v types.Fleet) bool {
		return child.hitsOf("/api/summary") > summary && !node(t, v, "mid").Reachable
	})
	for _, name := range []string{"u-1", "u-2"} {
		n := node(t, v, name)
		if n.Status != types.NodeStatusUnreachable || !strings.HasPrefix(n.Error, "unreachable through mid: ") {
			t.Errorf("%s below a silent child = %+v; want unknown, through mid", name, n)
		}
	}
	if n := node(t, v, "u-1"); n.AgeSeconds <= 0 {
		t.Errorf("u-1, last read healthy, has age %v; want the time since", n.AgeSeconds)
	}
	if v.Rollup.Unreachable < 3 || v.Rollup.Healthy > 1 {
		t.Errorf("roll-up with mid silent = %+v; want mid and both its nodes unreachable", v.Rollup)
	}
	page := fleetOf(t, agg.Addr, "?limit=10")
	if n := node(t, page, "u-1"); !strings.HasPrefix(n.Error, "unreachable through mid: ") {
		t.Errorf("u-1 on a page = %+v; want it unknown there too", n)
	}
	var sub types.FleetSubtree
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/subtree", &sub)
	for _, e := range sub.Nodes {
		if e.Name == "u-1" && (e.Status != types.NodeStatusUnreachable || len(e.Reasons) == 0) {
			t.Errorf("u-1 in the subtree served upward = %+v; want unknown, with the reason", e)
		}
	}

	// The child answers again, but its subtree does not.
	child.handle("/api/summary", jsonAnswer(func() types.NodeSummary {
		s := leafSummary("mid")
		s.Aggregates, s.InstanceID = 2, child.id
		return s
	}()))
	child.handle("/api/fleet/subtree", rawAnswer(http.StatusInternalServerError, "text/plain", "index broken"))
	v = awaitFleet(t, agg.Addr, "mid to answer while its subtree does not", func(v types.Fleet) bool {
		return node(t, v, "mid").Reachable
	})
	if n := node(t, v, "mid"); !strings.Contains(n.SubtreeError, "returned HTTP 500") {
		t.Errorf("mid with an unreadable subtree = %+v; want the subtree error named", n)
	}
	if n := node(t, v, "u-1"); !strings.Contains(n.Error, "returned HTTP 500") {
		t.Errorf("u-1 while mid's subtree is unreadable = %+v; want unknown for that reason", n)
	}
}

// TestReload_ARelayThatOutlivesItsBudgetIsAnsweredInTime checks a drill-in
// through a child that never answers: every asker is answered when the fleet
// timeout runs out, with the failure, rather than waiting for ever.
func TestReload_ARelayThatOutlivesItsBudgetIsAnsweredInTime(t *testing.T) {
	child := newChild(t, "mid", 0, []types.FleetIndexEntry{entry("slow-1", types.NodeStatusHealthy, 0)})
	child.handle("/api/fleet/slow-1/snapshot", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1000, []peer{{"mid", child.URL}}, "")})
	awaitFleet(t, agg.Addr, "the one poll", polled)

	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + agg.Addr + "/api/fleet/slow-1/snapshot")
			if err != nil {
				t.Errorf("drill-in %d: %v", i, err)
				return
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Errorf("drill-in %d body: %v", i, err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Errorf("drill-in %d close: %v", i, err)
			}
			statuses[i] = resp.StatusCode
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	await.Recv(t, "every drill-in through the silent child to be answered", done)
	for i, s := range statuses {
		if s != http.StatusBadGateway {
			t.Errorf("drill-in %d through a silent child = %d; want 502", i, s)
		}
	}
}

// TestReload_TheAggregatorRecordsItsPeersHistory runs the server build with
// remote write on and checks that the peers' readings -- fetched as each peer
// can serve them -- reach the time-series database under the peers' own
// names, and that each card says whether its history is being recorded.
func TestReload_TheAggregatorRecordsItsPeersHistory(t *testing.T) {
	var mu sync.Mutex
	var written [][]byte
	tsdb := newStandIn(t)
	tsdb.handle("/api/v1/write", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		plain, err := snappy.Decode(nil, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		written = append(written, plain)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	wrote := func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range written {
			if bytes.Contains(p, []byte(name)) {
				return true
			}
		}
		return false
	}

	// A current peer, whose history payload its history worker fetches; one
	// that serves no history payload, so its whole snapshot is fetched
	// instead; and one too old for a summary, recorded from the snapshot its
	// poll fetched.
	//
	// Every reading is dated when it is served, as a live node's is: history
	// is written at the time a reading was collected, and one with no date is
	// dropped.
	reading := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			jsonAnswer(types.Snapshot{Timestamp: time.Now().UTC(), NodeName: name,
				Host: types.HostInfo{Hostname: name + ".example"}})(w, r)
		}
	}
	current, fallback, old := newStandIn(t), newStandIn(t), newStandIn(t)
	for name, s := range map[string]*standIn{"rec-current": current, "rec-fallback": fallback} {
		sum := leafSummary(name)
		sum.CollectionIntervalMs = 1000
		s.handle("/api/summary", jsonAnswer(sum))
		s.handle("/api/snapshot", reading(name))
	}
	current.handle("/api/snapshot/history", reading("rec-current"))
	old.handle("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snap := oldSnapshot()
		snap.NodeName, snap.Timestamp = "rec-old", time.Now().UTC()
		jsonAnswer(snap)(w, r)
	})

	cfg := fleetYAML("hub", 1000, 1500, []peer{{"rec-current", current.URL}, {"rec-fallback", fallback.URL}, {"rec-old", old.URL}}, "") +
		"prometheus:\n  remote_write_url: " + tsdb.URL + "/api/v1/write\n  sample_interval_ms: 1000\n  flush_interval_ms: 1000\n"
	agg := harness.Serve(t, harness.ServeOptions{Bin: harness.Bins.Server, Config: cfg})

	await.Until(t, "every peer's history to be written under its name", func(_ context.Context) bool {
		return wrote("rec-current") && wrote("rec-fallback") && wrote("rec-old")
	})
	if current.hitsOf("/api/snapshot/history") == 0 {
		t.Error("the current peer's history payload was never fetched")
	}
	if fallback.hitsOf("/api/snapshot") == 0 {
		t.Error("a peer serving no history payload did not have its snapshot fetched instead")
	}
	v := awaitFleet(t, agg.Addr, "every card to state its history", func(v types.Fleet) bool {
		for _, n := range v.Nodes {
			if !n.Self && n.History == nil {
				return false
			}
		}
		return polled(v)
	})
	for _, name := range []string{"rec-current", "rec-fallback", "rec-old"} {
		if h := node(t, v, name).History; h == nil || h.State == "" {
			t.Errorf("%s's history = %+v; want its recording stated", name, h)
		}
	}
}

// TestReload_ADrillInWhosePeerMovesIsDiscarded checks a drill-in whose peer a
// reload repoints while its snapshot is being fetched: the answer belonged to
// the old address, and it is neither served under the name nor kept.
func TestReload_ADrillInWhosePeerMovesIsDiscarded(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	old, moved := newStandIn(t), newStandIn(t)
	old.handle("/api/summary", jsonAnswer(leafSummary("a")))
	old.handle("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		jsonAnswer(types.Snapshot{NodeName: "old-address"})(w, r)
	})
	moved.handle("/api/summary", jsonAnswer(leafSummary("a")))
	moved.handle("/api/snapshot", jsonAnswer(types.Snapshot{NodeName: "new-address"}))
	path := editableConfig(t, fleetYAML("hub", onePoll, 10000, []peer{{"a", old.URL}}, ""))
	agg := harness.Serve(t, harness.ServeOptions{ConfigPath: path})
	awaitFleet(t, agg.Addr, "the one poll", polled)

	status := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + agg.Addr + "/api/fleet/a/snapshot")
		if err != nil {
			t.Errorf("drill-in: %v", err)
			status <- 0
			return
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close the drill-in: %v", err)
		}
		status <- resp.StatusCode
	}()
	await.Recv(t, "the drill-in to reach the old address", arrived)
	reload(t, agg, path, fleetYAML("hub", onePoll, 10000, []peer{{"a", moved.URL}}, ""), 1)
	close(release)

	if got := await.Recv(t, "the drill-in to be answered", status); got != http.StatusServiceUnavailable {
		t.Errorf("a drill-in whose peer moved mid-fetch = %d; want 503, ask again", got)
	}
	var again types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/a/snapshot", &again)
	if again.Snapshot == nil || again.Snapshot.NodeName != "new-address" {
		t.Errorf("asking again = %+v; want the new address's snapshot", again.Snapshot)
	}
}

// TestReload_TheSettingsViewReportsTheRunningInterval checks that a collection
// interval changed from the dashboard is what the settings view reports, with
// its source, rather than the configured value nothing is running at.
func TestReload_TheSettingsViewReportsTheRunningInterval(t *testing.T) {
	s := newStandIn(t)
	s.handle("/api/summary", jsonAnswer(leafSummary("a")))
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500, []peer{{"a", s.URL}}, "")})
	if status, body := harness.Post(t, "http://"+agg.Addr+"/api/interval", `{"interval_ms":5000}`); status != http.StatusOK {
		t.Fatalf("set the interval = %d %s", status, body)
	}
	var cfg types.ConfigView
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/hub/config", &cfg)
	for _, e := range cfg.Entries {
		if e.Key == "server.interval_ms" && (e.Value != "5000" || e.Source != types.ConfigSourceRuntime) {
			t.Errorf("server.interval_ms = %q from %q; want 5000 from the runtime", e.Value, e.Source)
		}
	}
}

// TestReload_ARemoteWriteRedirectIsNotFollowedAsAGet checks the redirect
// policy on the one request that carries a body: a 302 would have the client
// re-send the batch as a GET, dropping the samples while the receiver answers
// 200. It is refused, and said so.
func TestReload_ARemoteWriteRedirectIsNotFollowedAsAGet(t *testing.T) {
	tsdb := newStandIn(t)
	tsdb.handle("/api/v1/write", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	tsdb.handle("/login", rawAnswer(http.StatusOK, "text/html", "<html>sign in</html>"))
	cfg := "prometheus:\n  remote_write_url: " + tsdb.URL + "/api/v1/write\n  sample_interval_ms: 1000\n  flush_interval_ms: 1000\n"
	srv := harness.Serve(t, harness.ServeOptions{Bin: harness.Bins.Server, Config: cfg})

	rec := srv.Logs.Await(t, "the refused write to be logged", func(r harness.LogRecord) bool {
		return strings.HasPrefix(r.Msg(), "prom: remote write") && r["error"] != nil
	})
	if msg := fmt.Sprint(rec["error"]); !strings.Contains(msg, "redirect rewrites the POST as a GET") {
		t.Errorf("the refused redirect = %q; want the rewritten method named", msg)
	}
	if got := tsdb.hitsOf("/login"); got != 0 {
		t.Errorf("the redirect's target was sent %d requests; want none", got)
	}
}
