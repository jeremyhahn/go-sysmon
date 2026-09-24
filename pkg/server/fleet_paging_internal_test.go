package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// pagedFleetServer serves a fleet of the named peers plus this node, "local".
// Nothing is polled: every peer is "not polled yet", which is all paging needs
// -- names to order, search and count.
func pagedFleetServer(t *testing.T, peers ...string) *Server {
	t.Helper()
	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	for i, name := range peers {
		cfg.Peers = append(cfg.Peers, config.Peer{Name: name, URL: fmt.Sprintf("https://10.0.0.%d:8081", i+1)})
	}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			return nil, &types.Snapshot{Host: types.HostInfo{Hostname: "aggregator"}, GitCommit: "a46f6cf"}, time.Second
		},
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return newServer(Config{Fleet: poller}, nil)
}

// get serves one GET through the server's handler chain. Shared with the
// history tests, which build only with the prometheus tag; this file builds in
// both shapes.
func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec
}

func fleetNames(v types.Fleet) []string {
	out := []string{}
	for _, n := range v.Nodes {
		out = append(out, n.Name)
	}
	return out
}

func TestHandleFleet_ServesTheRequestedPage(t *testing.T) {
	srv := pagedFleetServer(t, "vm-host-2", "fs", "ci-node-1", "vm-host-1", "gpu-host")

	tests := []struct {
		path      string
		want      []string
		page      types.FleetPage
		wantTotal int
	}{
		{"/api/fleet", []string{"ci-node-1", "fs", "gpu-host", "local", "vm-host-1", "vm-host-2"},
			types.FleetPage{Matched: 6, Total: 6}, 6},
		{"/api/fleet?limit=2", []string{"ci-node-1", "fs"},
			types.FleetPage{Limit: 2, Matched: 6, Total: 6}, 6},
		{"/api/fleet?offset=4&limit=2", []string{"vm-host-1", "vm-host-2"},
			types.FleetPage{Offset: 4, Limit: 2, Matched: 6, Total: 6}, 6},
		{"/api/fleet?q=VSTATION&limit=1&offset=1", []string{"vm-host-2"},
			types.FleetPage{Query: "VSTATION", Offset: 1, Limit: 1, Matched: 2, Total: 6}, 6},
		{"/api/fleet?q=dell", []string{"ci-node-1"},
			types.FleetPage{Query: "dell", Matched: 1, Total: 6}, 6},
		{"/api/fleet?q=nothing", []string{},
			types.FleetPage{Query: "nothing", Matched: 0, Total: 6}, 6},
		{"/api/fleet?offset=4&limit=9223372036854775807", []string{"vm-host-1", "vm-host-2"},
			types.FleetPage{Offset: 4, Limit: 9223372036854775807, Matched: 6, Total: 6}, 6},
		// The anchor moves the page to the one holding the named node.
		{"/api/fleet?limit=2&anchor=vm-host-1", []string{"vm-host-1", "vm-host-2"},
			types.FleetPage{Offset: 4, Limit: 2, Matched: 6, Total: 6}, 6},
		{"/api/fleet?offset=50&limit=25", []string{},
			types.FleetPage{Offset: 50, Limit: 25, Matched: 6, Total: 6}, 6},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			var v types.Fleet
			if code := getJSON(t, srv, tt.path, &v); code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", tt.path, code)
			}
			if got := fleetNames(v); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("nodes = %v, want %v", got, tt.want)
			}
			if v.Page == nil || *v.Page != tt.page {
				t.Errorf("page = %+v, want %+v", v.Page, tt.page)
			}
			// The roll-up counts the whole fleet on every page.
			if v.Rollup == nil || v.Rollup.Nodes != tt.wantTotal {
				t.Errorf("rollup = %+v, want the whole fleet of %d", v.Rollup, tt.wantTotal)
			}
			if v.Self == nil || v.Self.Name != "local" || v.Self.GitCommit != "a46f6cf" {
				t.Errorf("self = %+v, want the local node with its build", v.Self)
			}
		})
	}
}

// TestHandleFleet_RejectsMalformedParameters: 400 with the parameter named,
// never a guess -- and on a standalone node too, because the request is wrong
// either way.
func TestHandleFleet_RejectsMalformedParameters(t *testing.T) {
	servers := map[string]*Server{
		"fleet":      pagedFleetServer(t, "fs"),
		"standalone": newServer(Config{}, nil),
	}
	for shape, srv := range servers {
		// A query string that does not parse is refused as a whole, rather
		// than having its bad pair dropped and the rest answered unbounded.
		if rec := get(t, srv, "/api/fleet?limit=25;offset=0"); rec.Code != http.StatusBadRequest ||
			!strings.Contains(rec.Body.String(), "malformed query") {
			t.Errorf("%s: GET /api/fleet?limit=25;offset=0 = %d %q, want 400 malformed query", shape, rec.Code, rec.Body.String())
		}
		for _, tt := range []struct{ query, param string }{
			{"limit=0", "limit"},
			{"limit=-1", "limit"},
			{"limit=abc", "limit"},
			{"offset=-3", "offset"},
			{"offset=", "offset"},
			{"limit=5&limit=6", "limit"},
			{"q=%ff", "q"},
		} {
			t.Run(shape+"/"+tt.query, func(t *testing.T) {
				rec := get(t, srv, "/api/fleet?"+tt.query)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("GET /api/fleet?%s = %d, want 400", tt.query, rec.Code)
				}
				if body := rec.Body.String(); !strings.Contains(body, "invalid "+tt.param) {
					t.Errorf("body %q does not name the parameter %q", body, tt.param)
				}
			})
		}
	}
}

// TestHandleFleet_PagesWalkTheFleetOnceInOrder: following the pages by offset
// visits every node exactly once, in the same order as the unpaged list.
func TestHandleFleet_PagesWalkTheFleetOnceInOrder(t *testing.T) {
	var peers []string
	for i := range 57 {
		peers = append(peers, fmt.Sprintf("node-%02d", (i*37)%57)) // configured out of order
	}
	srv := pagedFleetServer(t, peers...)

	var whole types.Fleet
	getJSON(t, srv, "/api/fleet", &whole)
	var walked []string
	var firstRollup []byte
	for offset := 0; ; offset += 10 {
		var v types.Fleet
		path := fmt.Sprintf("/api/fleet?offset=%d&limit=10", offset)
		if code := getJSON(t, srv, path, &v); code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		if len(v.Nodes) == 0 {
			break
		}
		walked = append(walked, fleetNames(v)...)
		// Nothing in this fleet has been polled, so the roll-up has no
		// time-dependent figures and must be byte-identical on every page.
		rollup, err := json.Marshal(v.Rollup)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if firstRollup == nil {
			firstRollup = rollup
		} else if string(rollup) != string(firstRollup) {
			t.Errorf("page at offset %d carries a different roll-up:\n%s\nwant\n%s", offset, rollup, firstRollup)
		}
	}
	if !reflect.DeepEqual(walked, fleetNames(whole)) {
		t.Errorf("pages walked %v\nwant the unpaged order %v", walked, fleetNames(whole))
	}
}

func TestHandleFleetRollup(t *testing.T) {
	t.Run("the whole fleet, without its nodes", func(t *testing.T) {
		srv := pagedFleetServer(t, "fs", "gpu-host", "omega")
		var r types.FleetRollup
		if code := getJSON(t, srv, "/api/fleet/rollup", &r); code != http.StatusOK {
			t.Fatalf("GET /api/fleet/rollup = %d, want 200", code)
		}
		// Three peers never polled are unreachable; this node is healthy.
		if r.Nodes != 4 || r.Healthy != 1 || r.Unreachable != 3 || r.NeverAnswered != 3 {
			t.Errorf("rollup = %+v; want 4 nodes: 1 healthy, 3 unreachable that never answered", r)
		}
	})
	t.Run("no fleet is 404", func(t *testing.T) {
		if rec := get(t, newServer(Config{}, nil), "/api/fleet/rollup"); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/fleet/rollup on a standalone node = %d, want 404", rec.Code)
		}
	})
}
