package config_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestPrometheus_AbsentIsOff is the property everything else rests on: a config
// file with no prometheus section leaves the node exactly as it was.
func TestPrometheus_AbsentIsOff(t *testing.T) {
	cfg, sources, err := config.NewLoader(writeConfig(t, "server:\n  addr: \":8081\"\n")).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if cfg.Prometheus.WriteEnabled() || cfg.Prometheus.QueryEnabled() {
		t.Fatalf("Prometheus = %+v, want both halves disabled", cfg.Prometheus)
	}
	// The cadences still have usable values, so turning it on later needs one
	// line rather than five.
	if cfg.Prometheus.SampleInterval() != 15*time.Second ||
		cfg.Prometheus.FlushInterval() != 30*time.Second ||
		cfg.Prometheus.BatchSize != 1000 ||
		cfg.Prometheus.Timeout() != 10*time.Second {
		t.Fatalf("Prometheus defaults = %+v", cfg.Prometheus)
	}
	if sources["prometheus.remote_write_url"] != types.ConfigSourceDefault {
		t.Errorf("remote_write_url source = %q, want default", sources["prometheus.remote_write_url"])
	}
}

func TestPrometheus_LoadsFromFile(t *testing.T) {
	cfg, sources, err := config.NewLoader(writeConfig(t, `
prometheus:
  remote_write_url: "http://mon0:9090/api/v1/write"
  query_url: "http://mon0:9090"
  sample_interval_ms: 30000
  flush_interval_ms: 60000
  batch_size: 500
  timeout_ms: 5000
  external_labels:
    site: rack-3
`)).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if !cfg.Prometheus.WriteEnabled() || !cfg.Prometheus.QueryEnabled() {
		t.Fatalf("Prometheus = %+v, want both halves enabled", cfg.Prometheus)
	}
	if cfg.Prometheus.SampleInterval() != 30*time.Second || cfg.Prometheus.BatchSize != 500 {
		t.Errorf("Prometheus = %+v", cfg.Prometheus)
	}
	if cfg.Prometheus.ExternalLabels["site"] != "rack-3" {
		t.Errorf("ExternalLabels = %v", cfg.Prometheus.ExternalLabels)
	}
	if sources["prometheus.remote_write_url"] != types.ConfigSourceFile {
		t.Errorf("remote_write_url source = %q, want the file", sources["prometheus.remote_write_url"])
	}
}

// TestPrometheus_NonsenseCadencesAreRepaired: a zero or negative interval would
// make a ticker panic or spin, so it falls back to the default and says so --
// the same treatment fleet.poll_interval_ms already gets.
func TestPrometheus_NonsenseCadencesAreRepaired(t *testing.T) {
	cfg, sources, err := config.NewLoader(writeConfig(t, `
prometheus:
  remote_write_url: "http://mon0:9090/api/v1/write"
  sample_interval_ms: 0
  flush_interval_ms: -1
  batch_size: 0
  timeout_ms: -5
`)).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	d := config.Defaults().Prometheus
	if cfg.Prometheus.SampleIntervalMs != d.SampleIntervalMs ||
		cfg.Prometheus.FlushIntervalMs != d.FlushIntervalMs ||
		cfg.Prometheus.BatchSize != d.BatchSize ||
		cfg.Prometheus.TimeoutMs != d.TimeoutMs {
		t.Fatalf("Prometheus = %+v, want the defaults", cfg.Prometheus)
	}
	for _, key := range []string{
		"prometheus.sample_interval_ms",
		"prometheus.flush_interval_ms",
		"prometheus.batch_size",
		"prometheus.timeout_ms",
	} {
		if sources[key] != types.ConfigSourceDefault {
			t.Errorf("%s source = %q; a value the file asked for and did not get "+
				"must not be attributed to the file", key, sources[key])
		}
	}
}

// TestPrometheus_UnusableURLsAreRejected. "mon0:9090" parses happily, with
// scheme "mon0" -- the service would start clean and every push would fail
// silently, which is the failure an operator is least likely to notice.
func TestPrometheus_UnusableURLsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no scheme on remote write", "prometheus:\n  remote_write_url: \"mon0:9090/api/v1/write\"\n"},
		{"no host on remote write", "prometheus:\n  remote_write_url: \"http://\"\n"},
		{"unsupported scheme", "prometheus:\n  remote_write_url: \"ftp://mon0/write\"\n"},
		{"no scheme on query", "prometheus:\n  query_url: \"mon0:9090\"\n"},
		{"unparseable", "prometheus:\n  query_url: \"http://[::1\"\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := config.NewLoader(writeConfig(t, tt.body)).Load()
			var urlErr *config.ErrPrometheusURL
			if !errors.As(err, &urlErr) {
				t.Fatalf("Load() = %v, want *ErrPrometheusURL", err)
			}
			if urlErr.Key == "" {
				t.Error("the error does not name the setting to fix")
			}
		})
	}
}

// TestPrometheus_CredentialsAreRejected mirrors the peer-URL rule. A secret
// accepted here would sit in memory and be one code path from the settings
// view, which has no authentication of its own -- and it would be a credential
// for the database a whole fleet writes to.
func TestPrometheus_CredentialsAreRejected(t *testing.T) {
	for _, body := range []string{
		"prometheus:\n  remote_write_url: \"http://user:hunter2@mon0:9090/api/v1/write\"\n",
		"prometheus:\n  query_url: \"http://user:hunter2@mon0:9090\"\n",
	} {
		_, _, err := config.NewLoader(writeConfig(t, body)).Load()
		var credErr *config.ErrPrometheusCredentials
		if !errors.As(err, &credErr) {
			t.Fatalf("Load() = %v, want *ErrPrometheusCredentials", err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("the rejection quoted the secret: %s", err)
		}
		if credErr.Host != "mon0:9090" {
			t.Errorf("Host = %q, want the host alone", credErr.Host)
		}
	}
}

// TestPrometheus_MisspelledKeyIsRejected: the loader decodes strictly, and a
// key that maps to nothing means the operator's setting is silently ignored.
func TestPrometheus_MisspelledKeyIsRejected(t *testing.T) {
	_, _, err := config.NewLoader(writeConfig(t,
		"prometheus:\n  remote_write_urls: \"http://mon0:9090/api/v1/write\"\n")).Load()
	if err == nil {
		t.Fatal("Load() accepted a misspelled key; the setting would be silently ignored")
	}
}

// TestPrometheus_ViewRedactsTheURLs. GET /api/config has no authentication of
// its own, and the address of the fleet's time-series database is
// reconnaissance. Whether history is on is the part an operator needs there.
func TestPrometheus_ViewRedactsTheURLs(t *testing.T) {
	mgr, err := config.NewManager(config.NewLoader(writeConfig(t, `
prometheus:
  remote_write_url: "http://mon0.internal:9090/api/v1/write"
  query_url: "http://mon0.internal:9090"
`)))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	view := mgr.View()
	found := 0
	for _, e := range view.Entries {
		switch e.Key {
		case "prometheus.remote_write_url", "prometheus.query_url":
			found++
			if e.Value != config.ValueConfigured {
				t.Errorf("%s = %q, want %q", e.Key, e.Value, config.ValueConfigured)
			}
		}
		if strings.Contains(e.Value, "mon0.internal") {
			t.Errorf("entry %s leaked the Prometheus address: %q", e.Key, e.Value)
		}
	}
	if found != 2 {
		t.Fatalf("found %d prometheus URL entries in the settings view, want 2", found)
	}
}

// TestPrometheus_ViewReportsNotSetWhenOff so the settings view distinguishes a
// standalone node from one whose configuration failed to load.
func TestPrometheus_ViewReportsNotSetWhenOff(t *testing.T) {
	mgr, err := config.NewManager(config.NewLoader(writeConfig(t, "server:\n  addr: \":8081\"\n")))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, e := range mgr.View().Entries {
		if e.Key == "prometheus.remote_write_url" && e.Value != config.ValueNotSet {
			t.Fatalf("remote_write_url = %q, want %q", e.Value, config.ValueNotSet)
		}
	}
}

// TestPrometheus_ReloadTurnsItOn is what lets an operator enable history on a
// running node. Restarting to gain the setting would drop every connected
// browser and empty the history buffers -- the reason config reload exists here
// at all.
func TestPrometheus_ReloadTurnsItOn(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\n")
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if mgr.Current().Prometheus.WriteEnabled() {
		t.Fatal("remote write is on before it was configured")
	}

	if err := os.WriteFile(path, []byte(
		"server:\n  addr: \":8081\"\nprometheus:\n  remote_write_url: \"http://mon0:9090/api/v1/write\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() = %v", err)
	}
	if !mgr.Current().Prometheus.WriteEnabled() {
		t.Fatal("remote write is still off after a reload that configured it")
	}
}

// TestPrometheus_ReloadRejectsABadURL: a reload that cannot be applied must
// leave the running configuration alone rather than going live and being
// reported afterwards.
func TestPrometheus_ReloadRejectsABadURL(t *testing.T) {
	good := "prometheus:\n  remote_write_url: \"http://mon0:9090/api/v1/write\"\n"
	path := writeConfig(t, good)
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := os.WriteFile(path, []byte("prometheus:\n  remote_write_url: \"mon0:9090\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err == nil {
		t.Fatal("Reload() accepted an unusable URL")
	}
	if got := mgr.Current().Prometheus.RemoteWriteURL; got != "http://mon0:9090/api/v1/write" {
		t.Fatalf("RemoteWriteURL = %q after a rejected reload, want the previous value", got)
	}
}

// TestPrometheus_ExternalLabelsAreValidated.
//
// A label that collides with one go-sysmon sets itself puts the same name on a
// series twice. Prometheus rejects such a series and rejects the whole write
// request with it, so one entry here would silently stop the node's entire
// history -- and only for the metrics that happen to carry that label, which is
// the hardest version of the bug to find. Rejecting at load is the difference
// between a service that will not start and one that starts and records
// nothing.
func TestPrometheus_ExternalLabelsAreValidated(t *testing.T) {
	tests := []struct {
		name  string
		label string
	}{
		{"collides with the node identity", "instance"},
		{"collides with the job", "job"},
		{"collides with a disk label", "device"},
		{"collides with an interface label", "interface"},
		{"collides with a PSI label", "resource"},
		{"reserved prefix", "__replica"},
		{"not a label name", "site-name"},
		{"starts with a digit", "9site"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "prometheus:\n  remote_write_url: \"http://mon0:9090/api/v1/write\"\n" +
				"  external_labels:\n    " + tt.label + ": x\n"
			_, _, err := config.NewLoader(writeConfig(t, body)).Load()
			if err == nil {
				t.Fatalf("Load() accepted external label %q", tt.label)
			}
			var labelErr *config.ErrPrometheusLabel
			if tt.label != "" && !errors.As(err, &labelErr) {
				t.Fatalf("Load() = %v, want *ErrPrometheusLabel", err)
			}
		})
	}

	// A label that is neither reserved nor malformed is accepted, so the check
	// is not simply refusing everything.
	cfg, _, err := config.NewLoader(writeConfig(t,
		"prometheus:\n  remote_write_url: \"http://mon0:9090/api/v1/write\"\n"+
			"  external_labels:\n    site: rack_3\n    env: prod\n")).Load()
	if err != nil {
		t.Fatalf("Load() rejected usable external labels: %v", err)
	}
	if cfg.Prometheus.ExternalLabels["site"] != "rack_3" {
		t.Errorf("ExternalLabels = %v", cfg.Prometheus.ExternalLabels)
	}
}

// Topology B (owner ruling 2026-09-04): "a node may be configured to store to
// local prometheus; when enabled, that node's GUI allows historical searching
// too -- so if the aggregator dies, the data survives on the nodes."
//
// It needs no new mechanism, and this is the check that keeps that claim true:
// a node pointing both halves at a Prometheus on its own host loads, validates
// and reports both halves enabled, which is what makes the date picker and the
// history search appear on that node (#101).
func TestPrometheus_NodeLocalStoreIsAValidTopology(t *testing.T) {
	cfg, sources, err := config.NewLoader(writeConfig(t, `
prometheus:
  remote_write_url: "http://127.0.0.1:9090/api/v1/write"
  query_url: "http://127.0.0.1:9090"
`)).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if !cfg.Prometheus.WriteEnabled() {
		t.Error("a node-local store does not record: WriteEnabled() is false")
	}
	if !cfg.Prometheus.QueryEnabled() {
		t.Error("a node-local store cannot be read back: QueryEnabled() is false")
	}
	if sources["prometheus.query_url"] != types.ConfigSourceFile {
		t.Errorf("query_url source = %q, want the file", sources["prometheus.query_url"])
	}
}

// A node that records locally and reads from somewhere else is a real
// arrangement, not a mistake to reject: a node keeping its own copy while the
// dashboard reads the fleet's aggregate is exactly the arrangement that
// survives the aggregator dying.
func TestPrometheus_HalvesMayPointAtDifferentStores(t *testing.T) {
	cfg, _, err := config.NewLoader(writeConfig(t, `
prometheus:
  remote_write_url: "http://127.0.0.1:9090/api/v1/write"
  query_url: "http://mon0:9090"
`)).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if !cfg.Prometheus.WriteEnabled() || !cfg.Prometheus.QueryEnabled() {
		t.Fatalf("Prometheus = %+v, want both halves enabled", cfg.Prometheus)
	}
}

// The write half alone is the shape every peer in topology A has: it records
// nothing locally to read back, so the dashboard must show no date picker while
// the node keeps pushing.
func TestPrometheus_WriteWithoutQueryHasNoHistoryToShow(t *testing.T) {
	cfg, _, err := config.NewLoader(writeConfig(t, `
prometheus:
  remote_write_url: "http://mon0:9090/api/v1/write"
`)).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if !cfg.Prometheus.WriteEnabled() {
		t.Error("WriteEnabled() = false with a remote_write_url set")
	}
	if cfg.Prometheus.QueryEnabled() {
		t.Error("QueryEnabled() = true with no query_url; the node would offer history it cannot read")
	}
}

// load reads a configuration body that must parse.
func load(t *testing.T, body string) *config.Config {
	t.Helper()
	cfg, _, err := config.NewLoader(writeConfig(t, body)).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	return cfg
}

// TestCheckHistoryCadence_BeyondTheLookbackIsRefused. History written more
// slowly than the database's lookback is stale between its own samples: no
// chart can draw it, and a counter's rate cannot be told from an outage.
func TestCheckHistoryCadence_BeyondTheLookbackIsRefused(t *testing.T) {
	const write = "prometheus:\n  remote_write_url: http://mon0:9090/api/v1/write\n"
	const peers = "fleet:\n  peers:\n    - name: fs\n      url: http://fs:8081\n"
	tests := []struct {
		name     string
		body     string
		lookback time.Duration
		key      string
	}{
		{"a slow exporter", write + "  sample_interval_ms: 300001\n", 5 * time.Minute,
			"prometheus.sample_interval_ms + server.interval_ms + prometheus.timeout_ms"},
		{"a slow collection", write + "server:\n  interval_ms: 600000\n", 5 * time.Minute,
			"prometheus.sample_interval_ms + server.interval_ms + prometheus.timeout_ms"},
		// Each under the limit, together over it: readings three minutes
		// apart, sampled every four, can be stored six minutes apart.
		{"a tick and a collection that add up past it",
			write + "  sample_interval_ms: 240000\nserver:\n  interval_ms: 180000\n", 5 * time.Minute,
			"prometheus.sample_interval_ms + server.interval_ms + prometheus.timeout_ms"},
		// The dashboard can switch collection to 60s at runtime, whatever
		// the file says.
		{"a tick that the slowest dashboard interval carries past it", write + "  sample_interval_ms: 250000\n",
			5 * time.Minute, "prometheus.sample_interval_ms + server.interval_ms + prometheus.timeout_ms"},
		{"a slow relay of peers", write + peers + "  poll_interval_ms: 600000\n", 5 * time.Minute,
			"fleet.poll_interval_ms + 4 x fleet.timeout_ms + prometheus.timeout_ms"},
		// The poller sleeps after each refresh, which can wait out the
		// timeout. (The peer's own collection is checked per peer, against
		// what it reports -- not assumed here.)
		{"a relay whose timeout carries it past", write + peers + "  poll_interval_ms: 295000\n  timeout_ms: 10000\n",
			5 * time.Minute, "fleet.poll_interval_ms + 4 x fleet.timeout_ms + prometheus.timeout_ms"},
		// The dashboard can override the fleet poll to 60s at runtime, so a
		// short configured poll does not bound it.
		{"a relay the runtime poll override carries past", write + peers + "  poll_interval_ms: 5000\n  timeout_ms: 245000\n",
			5 * time.Minute, "fleet.poll_interval_ms + 4 x fleet.timeout_ms + prometheus.timeout_ms"},
		// The limit is the database's lookback, not a constant: a cadence
		// fine for Prometheus' default is refused by one set lower.
		{"a database with a shorter lookback", write, time.Minute,
			"prometheus.sample_interval_ms + server.interval_ms + prometheus.timeout_ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := config.CheckHistoryCadence(load(t, tt.body), tt.lookback)
			var cadence *config.ErrHistoryCadence
			if !errors.As(err, &cadence) {
				t.Fatalf("CheckHistoryCadence() = %v, want *ErrHistoryCadence", err)
			}
			if cadence.Key != tt.key || cadence.LookbackMs != int(tt.lookback.Milliseconds()) {
				t.Errorf("error = %+v, want key %q against %v", cadence, tt.key, tt.lookback)
			}
		})
	}
}

// TestCheckHistoryCadence_Accepted. The limit is about history written into a
// database with a given lookback. A node that writes none -- or that has no
// peers to relay -- may run its dashboard and fleet poll as slowly as it
// likes; and a database whose lookback has been raised takes slower cadences.
func TestCheckHistoryCadence_Accepted(t *testing.T) {
	const write = "prometheus:\n  remote_write_url: http://mon0:9090/api/v1/write\n"
	cases := []struct {
		name     string
		body     string
		lookback time.Duration
	}{
		{"no remote write", "server:\n  interval_ms: 600000\nprometheus:\n  sample_interval_ms: 600000\n", time.Minute},
		{"writing, with a slow poll but no peers", write + "fleet:\n  poll_interval_ms: 600000\n", 5 * time.Minute},
		// No fixed allowance for the peer's collection: a relay tick well
		// inside the lookback passes, whatever the peer turns out to do.
		{"a relay tick inside the lookback", write + "fleet:\n  peers:\n    - name: fs\n      url: http://fs:8081\n" +
			"  poll_interval_ms: 205000\n  timeout_ms: 10000\n", 5 * time.Minute},
		// Two refreshes of two requests each are counted, but only those:
		// 250s + 4 x 10s + the 10s write timeout = 300s.
		{"a relay tick of exactly the lookback", write + "fleet:\n  peers:\n    - name: fs\n      url: http://fs:8081\n" +
			"  poll_interval_ms: 250000\n  timeout_ms: 10000\n", 5 * time.Minute},
		// 230s sampling + 60s collection + the 10s write timeout = 300s.
		{"writing at exactly the limit", write + "  sample_interval_ms: 230000\n", 5 * time.Minute},
		{"the defaults", write, 5 * time.Minute},
		{"a slow cadence into a database with a raised lookback",
			write + "  sample_interval_ms: 600000\nserver:\n  interval_ms: 600000\n", time.Hour},
	}
	for _, c := range cases {
		if err := config.CheckHistoryCadence(load(t, c.body), c.lookback); err != nil {
			t.Errorf("%s: CheckHistoryCadence() = %v, want it accepted", c.name, err)
		}
	}
}

// TestLoad_DoesNotCheckTheCadence. Parsing must give the same answer wherever
// it runs, and the limit is the database's lookback -- so a slow cadence
// parses, and the serve command's validator is what refuses it.
func TestLoad_DoesNotCheckTheCadence(t *testing.T) {
	load(t, "prometheus:\n  remote_write_url: http://mon0:9090/api/v1/write\n  sample_interval_ms: 600000\n")
}

// TestCheckHistoryCadence_TheWriteTimeoutCounts. The exporter flushes in
// line, and a write may take up to prometheus.timeout_ms with nothing sampled
// meanwhile: the default 15s tick and 60s collection under a 9m write timeout
// can leave 10m between stored samples.
func TestCheckHistoryCadence_TheWriteTimeoutCounts(t *testing.T) {
	body := "prometheus:\n  remote_write_url: http://mon0:9090/api/v1/write\n  timeout_ms: 540000\n"
	var cadence *config.ErrHistoryCadence
	if err := config.CheckHistoryCadence(load(t, body), 5*time.Minute); !errors.As(err, &cadence) {
		t.Fatalf("a 9m write timeout against a 5m lookback: %v, want *ErrHistoryCadence", err)
	}
}

// TestCheckHistoryCadence_TheSampleIntervalIsInWholeTicks. The exporter
// wakes once a second, so 230.5s samples every 231s: with a 60s collection
// and a 9.5s write timeout that is 300.5s, past a 5m lookback -- though the
// configured milliseconds add up to exactly 300s.
func TestCheckHistoryCadence_TheSampleIntervalIsInWholeTicks(t *testing.T) {
	body := "prometheus:\n  remote_write_url: http://mon0:9090/api/v1/write\n  sample_interval_ms: 230500\n  timeout_ms: 9500\n"
	var cadence *config.ErrHistoryCadence
	if err := config.CheckHistoryCadence(load(t, body), 5*time.Minute); !errors.As(err, &cadence) || cadence.IntervalMs != 300500 {
		t.Fatalf("CheckHistoryCadence = %v, want *ErrHistoryCadence at 300500ms", err)
	}
}
