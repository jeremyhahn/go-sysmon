//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// A fleet tree of real `sysmon serve` processes (#190): two leaves, a child
// aggregator polling them, and a root polling the child -- each process
// collecting from this host, every hop over HTTP.

// startServerWithConfig launches `sysmon serve` with a configuration file
// holding body, and returns the address it bound, ready to serve.
func startServerWithConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return startServe(t, "--config", path)
}

func fleetConfig(local string, peers map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet:\n  local_name: %s\n  poll_interval_ms: 1000\n  timeout_ms: 3000\n  peers:\n", local)
	for name, addr := range peers {
		fmt.Fprintf(&b, "    - name: %s\n      url: http://%s\n", name, addr)
	}
	return b.String()
}

type fleetView struct {
	Nodes []struct {
		Name      string `json:"name"`
		Via       string `json:"via"`
		Reachable bool   `json:"reachable"`
	} `json:"nodes"`
	Rollup struct {
		Nodes       int `json:"nodes"`
		Unreachable int `json:"unreachable"`
	} `json:"rollup"`
}

func getFleet(t *testing.T, ctx context.Context, addr, query string) (fleetView, bool) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/fleet"+query, nil)
	if err != nil {
		t.Fatalf("build GET /api/fleet: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fleetView{}, false
	}
	var v fleetView
	err = json.NewDecoder(resp.Body).Decode(&v)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Errorf("close GET /api/fleet: %v", cerr)
	}
	if err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	return v, true
}

func TestFleetTree_ThreeLevelsOfRealProcesses(t *testing.T) {
	leafA := startServerWithConfig(t, "fleet:\n  local_name: leaf-a\n")
	leafB := startServerWithConfig(t, "fleet:\n  local_name: leaf-b\n")
	mid := startServerWithConfig(t, fleetConfig("mid", map[string]string{"leaf-a": leafA, "leaf-b": leafB}))
	root := startServerWithConfig(t, fleetConfig("root", map[string]string{"mid": mid}))

	var v fleetView
	await.Until(t, "the root to list the whole tree, every node reachable", func(ctx context.Context) bool {
		got, ok := getFleet(t, ctx, root, "")
		v = got
		return ok && len(v.Nodes) == 4 && v.Rollup.Unreachable == 0
	})
	via := map[string]string{}
	for _, n := range v.Nodes {
		via[n.Name] = n.Via
	}
	if len(v.Nodes) != 4 || via["leaf-a"] != "mid" || via["leaf-b"] != "mid" || via["mid"] != "" {
		t.Fatalf("root's fleet = %+v; want root, mid, and both leaves via mid", v.Nodes)
	}

	// A drill-in two levels down, routed by the root to mid.
	resp, err := http.Get("http://" + root + "/api/fleet/leaf-b/snapshot")
	if err != nil {
		t.Fatalf("drill-in: %v", err)
	}
	closeOnCleanup(t, "the drill-in response", resp.Body)
	var peer struct {
		Reachable bool `json:"reachable"`
		Snapshot  struct {
			NodeName string `json:"node_name"`
		} `json:"snapshot"`
	}
	err = json.NewDecoder(resp.Body).Decode(&peer)
	if err != nil || resp.StatusCode != http.StatusOK || !peer.Reachable || peer.Snapshot.NodeName != "leaf-b" {
		t.Errorf("leaf-b through root = %d %+v (%v); want its live snapshot", resp.StatusCode, peer, err)
	}

	// A page one node long, across the tree.
	if p, ok := getFleet(t, context.Background(), root, "?offset=1&limit=1"); !ok || len(p.Nodes) != 1 || p.Nodes[0].Name != "leaf-b" || p.Rollup.Nodes != 4 {
		t.Errorf("page = %+v; want leaf-b, with the whole tree rolled up", p)
	}
}
