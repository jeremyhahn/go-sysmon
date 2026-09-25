//go:build integration

package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestPeerConfig_AnOldPeersTopologyIsWithheldOnRelay stands in for a peer
// running a build from before the settings view withheld topology: its
// /api/config names its config file, its Prometheus address and its own peers
// by URL. The aggregator relays it withholding all of that on its own
// authority, keeping only whether each is configured and how many peers there
// are.
func TestPeerConfig_AnOldPeersTopologyIsWithheldOnRelay(t *testing.T) {
	legacy := types.ConfigView{
		ConfigFile: "/etc/go-sysmon/sysmon.yaml",
		Entries: []types.ConfigEntry{
			{Section: "server", Key: "server.addr", Value: ":8081", Source: types.ConfigSourceFile},
			{Section: "alerts", Key: "alerts.cpu_pct", Value: "70", Source: types.ConfigSourceFile},
			{Section: "prometheus", Key: "prometheus.query_url", Value: "http://prom.internal:9090", Source: types.ConfigSourceFile},
			{Section: "prometheus", Key: "prometheus.remote_write_url", Value: "(not set)", Source: types.ConfigSourceDefault},
			{Section: "fleet", Key: "fleet.peers.http://10.0.0.5:8081", Value: "http://10.0.0.5:8081", Source: types.ConfigSourceFile},
			{Section: "fleet", Key: "fleet.peers.db", Value: "http://db.internal:8081", Source: types.ConfigSourceFile},
		},
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(legacy); err != nil {
			t.Errorf("encode the legacy view: %v", err)
		}
	}))
	t.Cleanup(old.Close)

	agg := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: agg\n" +
		"  peers:\n    - name: old\n      url: " + old.URL + "\n"})
	status, body := harness.Get(t, agg.URL("/api/fleet/old/config"))
	if status != http.StatusOK {
		t.Fatalf("GET /api/fleet/old/config = %d: %s", status, body)
	}
	for _, leak := range []string{"/etc/go-sysmon", "prom.internal", "10.0.0.5", "db.internal", ":8081"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the relayed view carries %q:\n%s", leak, body)
		}
	}

	var view types.ConfigView
	harness.MustParseJSON(t, string(body), &view)
	got := settings{file: view.ConfigFile, entries: map[string]types.ConfigEntry{}}
	for _, e := range view.Entries {
		got.entries[e.Key] = e
	}
	if got.file != "(configured)" {
		t.Errorf("config_file = %q, want (configured)", got.file)
	}
	got.want(t, "alerts.cpu_pct", "70", types.ConfigSourceFile)
	got.want(t, "server.addr", "(configured)", types.ConfigSourceFile)
	got.want(t, "prometheus.query_url", "(configured)", types.ConfigSourceFile)
	// Not set stays not set, rather than reading as configured.
	got.want(t, "prometheus.remote_write_url", "(not set)", types.ConfigSourceDefault)
	// The URL-keyed peer is re-keyed by position; the named one keeps its
	// name. Both say only that they are configured.
	got.want(t, "fleet.peers.(unnamed 1)", "(configured)", types.ConfigSourceFile)
	got.want(t, "fleet.peers.db", "(configured)", types.ConfigSourceFile)
}
