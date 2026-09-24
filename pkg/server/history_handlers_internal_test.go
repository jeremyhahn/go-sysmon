//go:build prometheus

// Tagged because the real history handlers exist only in the prometheus build (#100).
package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/metrics"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// fakePrometheus answers range queries and records what was asked of it.
type fakePrometheus struct {
	srv    *httptest.Server
	seen   url.Values
	status int
	body   string
}

func newFakePrometheus(t *testing.T) *fakePrometheus {
	t.Helper()
	f := &fakePrometheus{status: http.StatusOK}
	f.body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"device":"sdb"},"values":[[100,"2"]]},
	  {"metric":{"device":"sda"},"values":[[100,"1"],[115,"1.5"]]}
	]}}`
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen = r.URL.Query()
		w.WriteHeader(f.status)
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Errorf("fake Prometheus: writing the response: %v", err)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// historyConfig is a manager naming this node mon0 with one peer, fs. The
// history endpoint answers only for nodes it knows, so the tests need a
// configuration that says which those are.
func historyConfig(t *testing.T) *config.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	body := "fleet:\n  local_name: mon0\n  peers:\n    - name: fs\n      url: http://fs:8081\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	return mgr
}

// historyServer returns a Server wired to prom, or to nothing when url is "".
func historyServer(t *testing.T, url string) *Server {
	t.Helper()
	return newServer(Config{
		Config: historyConfig(t),
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: url, Timeout: 5 * time.Second}
		}),
	}, nil)
}

// TestHandleHistory_DisabledIsNotAnError. A node with no Prometheus is a
// standalone node, not a broken one: answering 404 would be indistinguishable
// from an old build without the endpoint, and the dashboard could not tell
// whether to hide the date picker or show a failure.
func TestHandleHistory_DisabledIsNotAnError(t *testing.T) {
	s := historyServer(t, "")

	for _, path := range []string{"/api/history/metrics", "/api/history?metric=cpu"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if body.Enabled {
			t.Errorf("GET %s reported enabled with no query URL", path)
		}
	}
}

// TestHandleHistoryMetrics_ListsTheCatalog.
func TestHandleHistoryMetrics_ListsTheCatalog(t *testing.T) {
	f := newFakePrometheus(t)
	rec := get(t, historyServer(t, f.srv.URL), "/api/history/metrics")

	var got types.HistoryCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || len(got.Metrics) != len(prom.Catalog()) {
		t.Fatalf("catalog = %+v, want every entry with enabled=true", got)
	}
	for _, m := range got.Metrics {
		if m.Key == "" || m.Label == "" || m.Unit == "" {
			t.Errorf("catalog entry %+v is missing a field the UI needs", m)
		}
	}
}

// TestHandleHistory_ServesARange checks the query built for Prometheus and the
// payload built for the browser, since a mistake in either is invisible in the
// other.
func TestHandleHistory_ServesARange(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	rec := get(t, s, "/api/history?metric=disk_used&node=mon0&start=1788300000000&end=1788300600000&step=15000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if got, want := f.seen.Get("query"), `sysmon_disk_used_percent{instance="mon0"}`; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}
	if got := f.seen.Get("step"); got != "15" {
		t.Errorf("upstream step = %q, want 15", got)
	}

	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Node != "mon0" || got.Metric != "disk_used" || got.Unit != types.HistoryUnitPercent {
		t.Fatalf("history envelope = %+v", got)
	}
	if got.StepMs != 15000 || got.StartMs != 1788300000000 || got.EndMs != 1788300600000 {
		t.Fatalf("window = %d..%d step %d, want it echoed back", got.StartMs, got.EndMs, got.StepMs)
	}

	// Prometheus returned sdb first. A legend that reshuffles between requests
	// is the same defect the fleet list had, so the order is imposed here.
	if len(got.Series) != 2 || got.Series[0].Name != "sda" || got.Series[1].Name != "sdb" {
		t.Fatalf("series = %+v, want sda then sdb", got.Series)
	}
	if len(got.Series[0].Points) != 2 || got.Series[0].Points[0].TimeMs != 100000 {
		t.Fatalf("sda points = %+v, want two points in milliseconds", got.Series[0].Points)
	}
}

// TestHandleHistory_LabelsAreNotForwarded. Everything on a Prometheus label set
// other than the splitting label is either constant across the response or the
// operator's own labelling, and forwarding it would publish the fleet's
// Prometheus configuration to any browser that can reach the dashboard.
func TestHandleHistory_LabelsAreNotForwarded(t *testing.T) {
	f := newFakePrometheus(t)
	f.body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"device":"sda","instance":"mon0","job":"go-sysmon","datacenter":"rack-3"},
	   "values":[[100,"1"]]}]}}`

	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=disk_used&node=mon0")
	if body := rec.Body.String(); strings.Contains(body, "rack-3") || strings.Contains(body, "datacenter") {
		t.Fatalf("the response carried the operator's external labels: %s", body)
	}
}

// TestHandleHistory_UnknownMetricIsRejected is the allowlist. Anything that is
// not a catalog key must be a 400, never an expression forwarded upstream.
func TestHandleHistory_UnknownMetricIsRejected(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	for _, metric := range []string{
		"",
		"sysmon_cpu_usage_percent",
		"up",
		`up} or vector(1) #`,
		"nonsense",
	} {
		f.seen = nil
		// A valid node is supplied deliberately. Without one the request is
		// rejected for the node rather than the metric, and this would pass
		// against a handler with no allowlist at all.
		rec := get(t, s, "/api/history?node=mon0&metric="+url.QueryEscape(metric))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("metric=%q gave %d, want 400", metric, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("metric=%q reached Prometheus as %q; the allowlist did not hold",
				metric, f.seen.Get("query"))
		}
	}
}

// TestHandleHistory_InjectionInTheNodeIsRejected. The node name is the only
// caller-supplied part of the expression; if it can close the matcher, this
// endpoint becomes an open PromQL proxy on an unauthenticated port.
func TestHandleHistory_InjectionInTheNodeIsRejected(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	for _, node := range []string{
		`mon0"} or on() group_left() up{`,
		`mon0" or foo="`,
		"mon0\nup",
		"mon0 # x",
		`.*`,
	} {
		f.seen = nil
		rec := get(t, s, "/api/history?metric=cpu&node="+url.QueryEscape(node))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("node=%q gave %d, want 400", node, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("node=%q reached Prometheus as %q", node, f.seen.Get("query"))
		}
	}
}

// TestHandleHistory_BadWindowsAreRejected covers the window arithmetic, and
// separates the caller's mistake (400) from the receiver's failure (502).
func TestHandleHistory_BadWindowsAreRejected(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	tests := []struct {
		name  string
		query string
	}{
		{"end before start", "start=2000&end=1000"},
		{"empty window", "start=1000&end=1000"},
		{"non-numeric start", "start=yesterday&end=1000"},
		{"non-numeric step", "step=fast"},
		// A year, when the ceiling is 90 days.
		{"window past the ceiling", "start=0&end=31536000000"},
		// 20000 points at a one-second step, when Prometheus returns 11000.
		{"resolution past the ceiling", "start=0&end=20000000&step=1000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, s, "/api/history?metric=cpu&node=mon0&"+tt.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleHistory_UpstreamFailureIsABadGateway. The distinction matters to
// whoever is reading the error: 502 says the dashboard reached go-sysmon and
// go-sysmon could not reach Prometheus.
//
// What it must NOT say is where that Prometheus is, or what it replied. This
// endpoint has no authentication of its own, and the address of the fleet's
// time-series database is reconnaissance -- the same reason the settings view
// redacts it. The detail belongs in the server log.
func TestHandleHistory_UpstreamFailureIsABadGateway(t *testing.T) {
	f := newFakePrometheus(t)
	f.status = http.StatusInternalServerError
	f.body = `{"status":"error","errorType":"internal","error":"tsdb at /var/lib/prometheus is compacting"}`

	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=cpu&node=mon0")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{f.srv.URL, "/var/lib/prometheus", "tsdb"} {
		if strings.Contains(body, secret) {
			t.Errorf("the 502 body disclosed %q: %s", secret, body)
		}
	}
}

// TestHandleHistory_OnlyKnownNodes. Several nodes write into one Prometheus,
// and this endpoint is unauthenticated. A name that merely looks like a
// hostname is not enough: forwarding it would let anyone able to reach this
// dashboard read the history of every machine in the database.
func TestHandleHistory_OnlyKnownNodes(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	// This node and its configured peer are served.
	for _, node := range []string{"mon0", "fs"} {
		f.seen = nil
		if rec := get(t, s, "/api/history?metric=cpu&node="+node); rec.Code != http.StatusOK {
			t.Errorf("node=%s gave %d, want 200 (body %s)", node, rec.Code, rec.Body.String())
		}
		if f.seen == nil {
			t.Errorf("node=%s never reached Prometheus", node)
		}
	}

	// A well-formed name belonging to some other machine is not.
	for _, node := range []string{"someone-elses-database-host", "prometheus", "10.0.0.5"} {
		f.seen = nil
		rec := get(t, s, "/api/history?metric=cpu&node="+node)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("node=%s gave %d, want 400", node, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("node=%s was queried upstream as %q", node, f.seen.Get("query"))
		}
	}
}

// TestHandleHistory_DefaultsToTheLastHourOfThisNode: the dashboard's first
// request carries a metric and nothing else.
func TestHandleHistory_DefaultsToTheLastHourOfThisNode(t *testing.T) {
	f := newFakePrometheus(t)
	before := time.Now()
	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=cpu&node=mon0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if window := time.Duration(got.EndMs-got.StartMs) * time.Millisecond; window != time.Hour {
		t.Errorf("default window = %v, want 1h", window)
	}
	if got.EndMs < before.UnixMilli() {
		t.Errorf("default end = %d, want roughly now", got.EndMs)
	}
	if got.StepMs <= 0 {
		t.Errorf("step = %d; the server must choose one when the caller does not", got.StepMs)
	}
}

// TestHandleMetrics_ExporterHealthOnlyWhenPushing.
//
// A node with no prometheus section must produce exactly the exposition it
// always did -- that is the promise the whole feature rests on -- and a node
// that IS pushing must report whether it is succeeding, because history that
// silently stops being recorded looks the same as history that has not happened
// yet.
func TestHandleMetrics_ExporterHealthOnlyWhenPushing(t *testing.T) {
	snap := &types.Snapshot{Host: types.HostInfo{Uptime: 1}}
	mon := newStaticMonitor(t, snap)

	enabled := ""
	exporter := prom.NewExporter(func() prom.Options {
		return prom.Options{RemoteWriteURL: enabled, SampleInterval: time.Hour, FlushInterval: time.Hour}
	}, func() (prom.Measurement, error) { return prom.Measurement{}, nil }, quietServerLogger())

	s := newServer(Config{Monitor: mon, Exporter: exporter}, nil)

	body := get(t, s, "/metrics").Body.String()
	if strings.Contains(body, "sysmon_remote_write") {
		t.Fatalf("a node that is not pushing exported remote-write series:\n%s", body)
	}

	// Nothing here starts the exporter's loop; Stats() reports what it holds,
	// and what it holds when disabled is "disabled".
	plain := &metrics.TextWriter{}
	metrics.Write(plain, snap)
	if body != plain.String() {
		t.Fatal("the exposition of a standalone node is no longer what pkg/metrics alone produces")
	}
}

// staticSnapshotter always yields the same snapshot.
type staticSnapshotter struct {
	snap *types.Snapshot
}

func (s staticSnapshotter) Snapshot() (*types.Snapshot, error) { return s.snap, nil }

// newStaticMonitor returns a started monitor serving one fixed snapshot.
//
// The interval is short because the poll loop collects on its first tick
// rather than on Start, so a long interval would leave Snapshot answering "not
// running" for the whole test. Re-collecting is harmless: every tick stores the
// same value.
func newStaticMonitor(t *testing.T, snap *types.Snapshot) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(staticSnapshotter{snap: snap}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("monitor.Stop: %v", err)
		}
	})

	// Start returns before the first collection has landed, and a handler that
	// reads too early gets "no snapshot available" instead of the fixture.
	awaitPublications(t, m, 1)
	return m
}

func quietServerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestHandleHistory_ServesTheLocalNodeWithNoFleetOrConfig.
//
// server.Config makes History independent of Fleet and of the configuration
// manager, so a caller embedding this package can wire up history on its own.
// localName() returned "" when neither was supplied, and an empty name is not
// a node, so every history request for the local node was rejected as unknown
// -- the feature was unusable in exactly the configuration the API invites.
//
// The name falls back to the same resolution the configuration makes, which is
// the hostname, and never to an empty string.
func TestHandleHistory_ServesTheLocalNodeWithNoFleetOrConfig(t *testing.T) {
	f := newFakePrometheus(t)
	s := newServer(Config{
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: f.srv.URL, Timeout: 5 * time.Second}
		}),
	}, nil)

	// No node parameter: the request is for whatever this node calls itself.
	rec := get(t, s, "/api/history?metric=disk_used&start=1788300000000&end=1788300600000&step=15000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; a server with History and nothing else "+
			"cannot answer for its own node", rec.Code, rec.Body.String())
	}

	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this machine: %v", err)
	}
	if got, want := f.seen.Get("query"),
		`sysmon_disk_used_percent{instance="`+host+`"}`; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}

	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Node != host {
		t.Errorf("history is labelled %q, want the local hostname %q", got.Node, host)
	}
}

// TestHandleHistory_KnowsTheFleetWithoutAConfigManager: a server embedded as
// a library can be given a fleet and no configuration manager. The fleet then
// says which nodes history may be asked about -- this node and its configured
// peers, polled or not -- and nothing else is forwarded to Prometheus.
func TestHandleHistory_KnowsTheFleetWithoutAConfigManager(t *testing.T) {
	prometheus := newFakePrometheus(t)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "fs", URL: "https://10.0.0.1:8081"}}, PollIntervalMs: 3000, TimeoutMs: 3000}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "mon0" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv := newServer(Config{
		Fleet: poller,
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: prometheus.srv.URL, Timeout: 5 * time.Second}
		}),
	}, nil)

	for node, want := range map[string]int{
		"mon0":  http.StatusOK,
		"fs":    http.StatusOK,
		"other": http.StatusBadRequest,
	} {
		if rec := get(t, srv, "/api/history?metric=cpu&node="+node); rec.Code != want {
			t.Errorf("history for %q = %d, want %d: %s", node, rec.Code, want, rec.Body.String())
		}
	}
}
