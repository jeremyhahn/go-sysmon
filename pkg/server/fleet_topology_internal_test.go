package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestFleetEndpoints_PublishNoPeerAddress is #120 end to end: an aggregator in
// front of two real peers -- a live go-sysmon server and an address nothing
// listens on -- and every endpoint an unauthenticated caller can reach about
// them is searched for any part of either peer's address.
//
// The fleet view used to carry each peer's URL outright; a peer that failed
// carried it again in its error ("Get \"http://...\": dial tcp ...: connection
// refused"), and so did the roll-up that names it and the drill-in's 502; and
// the relayed settings view served the peer's bind address. Names are how
// every one of these addresses a node, so a name is what they still carry.
func TestFleetEndpoints_PublishNoPeerAddress(t *testing.T) {
	live := livePeer(t)
	deadURL := closedAddress(t)

	cfg := config.Fleet{
		Peers:          []config.Peer{{Name: "live", URL: live}, {Name: "dead", URL: deadURL}},
		PollIntervalMs: 10,
		TimeoutMs:      2000,
	}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "agg" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	done := make(chan struct{})
	go poller.Run(done)
	defer close(done)

	aggregator := newServer(Config{
		Monitor: startedMonitor(t, &pagingSnapshotter{}, 5*time.Millisecond),
		Fleet:   poller,
		Config:  managerFor(t, "fleet:\n  peers:\n    - name: live\n      url: "+live+"\n    - name: dead\n      url: "+deadURL+"\n"),
	}, nil)

	waitUntil(t, func() bool {
		var view types.Fleet
		if getJSON(t, aggregator, "/api/fleet", &view) != http.StatusOK {
			return false
		}
		n := nodesByName(view)
		return n["live"].Reachable && n["dead"].Error != "" && n["dead"].Error != "not polled yet"
	})

	var view types.Fleet
	getJSON(t, aggregator, "/api/fleet", &view)
	dead := nodesByName(view)["dead"]
	if !strings.Contains(dead.Error, "connection refused") {
		t.Errorf("dead peer error = %q, want it still to say the connection was refused", dead.Error)
	}
	if dead.Name != "dead" {
		t.Errorf("dead peer name = %q, want the name it is addressed by", dead.Name)
	}
	if !attentionNames(view, "dead", "connection refused") {
		t.Errorf("roll-up does not name the dead peer with its reason: %+v", view.Rollup)
	}

	var relayed types.ConfigView
	if code := getJSON(t, aggregator, "/api/fleet/live/config", &relayed); code != http.StatusOK {
		t.Fatalf("GET /api/fleet/live/config = %d, want 200", code)
	}
	if v := configValue(relayed, "server.addr"); v != config.ValueConfigured {
		t.Errorf("relayed server.addr = %q, want %q", v, config.ValueConfigured)
	}

	responses := map[string]int{
		"/api/fleet": http.StatusOK,
		// The paged view (#15), a search that matches the dead peer, and the
		// roll-up alone: every shape the fleet view is served in.
		"/api/fleet?offset=0&limit=1": http.StatusOK,
		"/api/fleet?q=dead&limit=25":  http.StatusOK,
		"/api/fleet/rollup":           http.StatusOK,
		"/api/fleet/live/config":      http.StatusOK,
		"/api/fleet/live/snapshot":    http.StatusOK,
		"/api/fleet/dead/config":      http.StatusBadGateway,
		"/api/fleet/dead/snapshot":    http.StatusBadGateway,
		"/api/summary":                http.StatusOK,
		"/api/config":                 http.StatusOK,
	}
	for path, wantCode := range responses {
		rec := httptest.NewRecorder()
		aggregator.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != wantCode {
			t.Errorf("GET %s = %d, want %d; body=%s", path, rec.Code, wantCode, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, `"url"`) {
			t.Errorf("GET %s still carries a url field: %s", path, body)
		}
		for _, peer := range []string{live, deadURL} {
			for _, leak := range addressParts(t, peer) {
				if strings.Contains(body, leak) {
					t.Errorf("GET %s carries %q from peer address %s: %s", path, leak, peer, body)
				}
			}
		}
	}
}

// livePeer starts a real go-sysmon server as a fleet peer, configured to bind
// the address it is served on, and returns its URL.
func livePeer(t *testing.T) string {
	t.Helper()
	var peer *Server
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.ServeHTTP(w, r)
	}))
	addr := ts.Listener.Addr().String()
	peer = newServer(Config{
		Monitor: startedMonitor(t, &pagingSnapshotter{}, 5*time.Millisecond),
		Addr:    addr,
		Config:  managerFor(t, "server:\n  addr: "+addr+"\n"),
	}, nil)
	ts.Start()
	t.Cleanup(ts.Close)

	// The precondition: served by the peer itself, the bind address is there
	// to be withheld.
	var own types.ConfigView
	getJSON(t, peer, "/api/config", &own)
	if v := configValue(own, "server.addr"); v != addr {
		t.Fatalf("peer's own server.addr = %q, want %q", v, addr)
	}
	return ts.URL
}

// closedAddress returns the URL of a loopback port nothing is listening on.
func closedAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + addr
}

// managerFor loads body as a configuration file.
func managerFor(t *testing.T, body string) *config.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	return mgr
}

// addressParts is every fragment of rawURL that would give the address away.
func addressParts(t *testing.T, rawURL string) []string {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatalf("splitting %q: %v", rawURL, err)
	}
	return []string{rawURL, host + ":" + port, host, ":" + port}
}

func nodesByName(view types.Fleet) map[string]types.FleetNode {
	out := make(map[string]types.FleetNode, len(view.Nodes))
	for _, n := range view.Nodes {
		out[n.Name] = n
	}
	return out
}

// attentionNames reports whether the roll-up names node with a reason saying
// want.
func attentionNames(view types.Fleet, node, want string) bool {
	if view.Rollup == nil {
		return false
	}
	for _, a := range view.Rollup.Attention {
		if a.Name != node {
			continue
		}
		for _, reason := range a.Reasons {
			if strings.Contains(reason, want) {
				return true
			}
		}
	}
	return false
}

func configValue(view types.ConfigView, key string) string {
	for _, e := range view.Entries {
		if e.Key == key {
			return e.Value
		}
	}
	return ""
}
