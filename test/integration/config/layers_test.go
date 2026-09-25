//go:build integration

package config

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestSettingsView_NamesTheLayerBehindEverySetting starts the service with a
// file, the environment and flags all supplying settings, and reads back which
// layer won each one: flag over environment over file over default.
func TestSettingsView_NamesTheLayerBehindEverySetting(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{
		Config: "server:\n  interval_ms: 5000\n" +
			"alerts:\n  memory_pct: 80\n  cpu_pct: 70\n" +
			"fleet:\n  local_name: rack-7\n  poll_interval_ms: -1\n" +
			"  peers:\n    - name: db\n      url: http://db.internal:8081\n" +
			"prometheus:\n  batch_size: 500\n",
		Env: []string{"SYSMON_ALERTS_CPU_PCT=77", "SYSMON_ALERTS_DISK_FULL_PCT=91"},
	})
	view := viewOf(t, s)

	if view.file != "(configured)" {
		t.Errorf("config_file = %q, want (configured) with the path withheld", view.file)
	}
	// --interval 200 is on the command line the harness runs: it outranks the
	// file's 5000.
	view.want(t, "server.interval_ms", "200", types.ConfigSourceFlag)
	view.want(t, "alerts.cpu_pct", "77", types.ConfigSourceEnv)
	view.want(t, "alerts.disk_full_pct", "91", types.ConfigSourceEnv)
	view.want(t, "alerts.memory_pct", "80", types.ConfigSourceFile)
	view.want(t, "fleet.local_name", "rack-7", types.ConfigSourceFile)
	view.want(t, "prometheus.batch_size", "500", types.ConfigSourceFile)
	view.want(t, "alerts.temp_crit_c", "85", types.ConfigSourceDefault)
	// A value the file asked for and could not have is repaired to the
	// default, and says so rather than crediting the file.
	view.want(t, "fleet.poll_interval_ms", "10000", types.ConfigSourceDefault)
	// A peer is listed by name; its address is another machine's and is
	// withheld.
	view.want(t, "fleet.peers.db", "(configured)", types.ConfigSourceFile)
	view.want(t, "prometheus.remote_write_url", "(not set)", types.ConfigSourceDefault)
}

// TestSettingsView_RunningOnDefaults verifies a node with no config file says
// so, and reports every setting as a default.
func TestSettingsView_RunningOnDefaults(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	view := viewOf(t, s)

	if view.file != "(not set)" {
		t.Errorf("config_file = %q, want (not set) with no file", view.file)
	}
	view.want(t, "alerts.memory_pct", "90", types.ConfigSourceDefault)
	view.want(t, "server.watch", "false", types.ConfigSourceDefault)
}

// TestSettingsView_WithholdsTopology verifies the settings that describe where
// things are -- certificate paths, the time-series database -- are reported as
// configured without saying where, whichever layer set them, while the node
// serves HTTPS on the certificate it names.
func TestSettingsView_WithholdsTopology(t *testing.T) {
	p := newPKI(t)
	s := harness.Serve(t, harness.ServeOptions{
		Config: "prometheus:\n  query_url: http://prom.internal:9090\n",
		Env:    []string{"SYSMON_FLEET_TLS_CA_FILE=" + p.caFile},
		Args:   []string{"--tls-cert", p.certFile, "--tls-key", p.keyFile},
		Client: p.client(),
	})
	var view types.ConfigView
	status, _, body := harness.Do(t, p.client(), mustGet(t, "https://"+s.Addr+"/api/config"))
	if status != 200 {
		t.Fatalf("GET /api/config over TLS = %d: %s", status, body)
	}
	harness.MustParseJSON(t, string(body), &view)
	got := settings{entries: map[string]types.ConfigEntry{}}
	for _, e := range view.Entries {
		got.entries[e.Key] = e
	}

	got.want(t, "prometheus.query_url", "(configured)", types.ConfigSourceFile)
	got.want(t, "fleet.tls.ca_file", "(configured)", types.ConfigSourceEnv)
	got.want(t, "server.tls_cert", "(configured)", types.ConfigSourceFlag)
	got.want(t, "server.tls_key", "(configured)", types.ConfigSourceFlag)
	for _, secret := range []string{p.caFile, p.certFile, p.keyFile, "prom.internal"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("GET /api/config names %q", secret)
		}
	}
}

// TestServe_RefusesAnUnreadableFleetCA verifies a peer CA that cannot be read
// is a startup error, not every peer turning unreachable later.
func TestServe_RefusesAnUnreadableFleetCA(t *testing.T) {
	stderr := failsToStart(t, []string{"SYSMON_FLEET_TLS_CA_FILE=" + filepath.Join(t.TempDir(), "absent-ca.pem")},
		"--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if !strings.Contains(stderr, "absent-ca.pem") {
		t.Errorf("stderr does not name the CA file:\n%s", stderr)
	}
}

// mustGet builds a GET request for url.
func mustGet(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	return req
}

// TestSettingsView_RepairsWhatWouldDisableASubsystem verifies values that
// would switch a subsystem off by accident -- a zero timeout, a negative batch
// -- run on the defaults instead, and the view credits the defaults rather
// than the file that asked for something else. Peers are listed in name order.
func TestSettingsView_RepairsWhatWouldDisableASubsystem(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{
		Config: "fleet:\n  timeout_ms: 0\n" +
			"  peers:\n    - name: zeta\n      url: http://zeta:8081\n    - name: alpha\n      url: http://alpha:8081\n" +
			"prometheus:\n  sample_interval_ms: -1\n  flush_interval_ms: 0\n  batch_size: -5\n  timeout_ms: 0\n",
	})
	view := viewOf(t, s)

	view.want(t, "fleet.timeout_ms", "3000", types.ConfigSourceDefault)
	view.want(t, "prometheus.sample_interval_ms", "15000", types.ConfigSourceDefault)
	view.want(t, "prometheus.flush_interval_ms", "30000", types.ConfigSourceDefault)
	view.want(t, "prometheus.batch_size", "1000", types.ConfigSourceDefault)
	view.want(t, "prometheus.timeout_ms", "10000", types.ConfigSourceDefault)
	view.want(t, "fleet.peers.alpha", "(configured)", types.ConfigSourceFile)
	view.want(t, "fleet.peers.zeta", "(configured)", types.ConfigSourceFile)
}
