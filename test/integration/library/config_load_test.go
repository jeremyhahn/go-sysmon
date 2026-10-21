//go:build integration

package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// loadFile writes body as the config file and loads it through a Manager.
func loadFile(t *testing.T, body string) (*config.Manager, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, body)
	return config.NewManager(config.NewLoader(path))
}

// asA reports whether err is, or wraps, a *T.
func asA[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// TestConfigLoad_RefusesTheFilesAnOperatorGetsWrong covers each way a file can
// be wrong, from a misspelt key to a peer that cannot be polled: the load
// stops, with an error of the type that says which, and an error message that
// is not empty.
func TestConfigLoad_RefusesTheFilesAnOperatorGetsWrong(t *testing.T) {
	tests := []struct {
		name string
		body string
		is   func(error) bool
	}{
		{"a misspelt key", "alerts:\n  cpu_percent: 80\n", asA[*config.ConfigParseError]},
		{"yaml that is not a mapping", "alerts: [oops\n", asA[*config.ConfigParseError]},
		{"a retired key", "alerts:\n  swap_pct: 80\n", asA[*config.RetiredConfigKeyError]},
		{"a peer without a name", "fleet:\n  peers:\n    - url: http://a:8080\n", asA[*config.PeerUnnamedError]},
		{"a peer without a url", "fleet:\n  peers:\n    - name: a\n", asA[*config.PeerURLError]},
		{"a peer url without a scheme", "fleet:\n  peers:\n    - name: a\n      url: a:8080\n", asA[*config.PeerURLError]},
		{"a peer url with a foreign scheme", "fleet:\n  peers:\n    - name: a\n      url: ftp://a:21\n", asA[*config.PeerURLError]},
		{"a peer url with no host", "fleet:\n  peers:\n    - name: a\n      url: \"http://\"\n", asA[*config.PeerURLError]},
		{"a peer url that does not parse", "fleet:\n  peers:\n    - name: a\n      url: \"http://a:80:80/%zz\"\n", asA[*config.PeerURLError]},
		{"a peer url carrying a credential", "fleet:\n  peers:\n    - name: a\n      url: http://user:hunter2@a:8080\n", asA[*config.PeerCredentialsError]},
		{"a peer name that cannot be a label", "fleet:\n  peers:\n    - name: \"a b/c\"\n      url: http://a:8080\n", asA[*config.PeerNameError]},
		{"two peers with one name", "fleet:\n  peers:\n    - name: a\n      url: http://a:8080\n    - name: a\n      url: http://b:8080\n", asA[*config.DuplicatePeerError]},
		{"a local name that cannot be a label", "fleet:\n  local_name: \"not a name\"\n", asA[*config.LocalNameError]},
		{"a query url without a scheme", "prometheus:\n  query_url: mon0:9090\n", asA[*config.PrometheusURLError]},
		{"a remote write url with a credential", "prometheus:\n  remote_write_url: http://user:hunter2@mon0:9090/api/v1/write\n", asA[*config.PrometheusCredentialsError]},
		{"an external label Prometheus reserves", "prometheus:\n  external_labels:\n    __name__: x\n", asA[*config.PrometheusLabelError]},
		{"an external label that is not a label name", "prometheus:\n  external_labels:\n    \"my-site\": x\n", asA[*config.PrometheusLabelError]},
		{"an external label go-sysmon sets itself", "prometheus:\n  external_labels:\n    instance: x\n", asA[*config.PrometheusLabelError]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFile(t, tt.body)
			if err == nil {
				t.Fatal("the file loaded, want it refused")
			}
			if !tt.is(err) {
				t.Errorf("error %T (%v) is not of the type that names this mistake", err, err)
			}
			if strings.TrimSpace(err.Error()) == "" {
				t.Error("the error has an empty message")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error %q echoes a credential from the file", err)
			}
		})
	}
}

// TestConfigLoad_APathThroughAFileIsUnreadableNotAbsent is a config path whose
// parent is a file: the settings exist and cannot be read, so the load fails
// rather than starting on defaults.
func TestConfigLoad_APathThroughAFileIsUnreadableNotAbsent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "afile"), "x")
	_, err := config.NewManager(config.NewLoader(filepath.Join(dir, "afile", "sysmon.yaml")))
	var unreadable *config.ConfigUnreadableError
	if !errors.As(err, &unreadable) {
		t.Fatalf("NewManager = %v, want a ConfigUnreadableError", err)
	}
	if !errors.Is(err, errors.Unwrap(unreadable)) || errors.Unwrap(unreadable) == nil {
		t.Errorf("ConfigUnreadableError does not wrap the cause: %v", err)
	}
}

// TestConfigLoad_AFileThatIsADirectoryIsAParseFailure reads a path that exists
// and cannot be read as a file.
func TestConfigLoad_AFileThatIsADirectoryIsAParseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := config.NewManager(config.NewLoader(path))
	var parse *config.ConfigParseError
	if !errors.As(err, &parse) {
		t.Fatalf("NewManager = %v, want a ConfigParseError", err)
	}
	if errors.Unwrap(parse) == nil {
		t.Error("ConfigParseError does not wrap the read failure")
	}
}

// TestConfigLoad_ZeroedIntervalsFallBackToTheDefaultsAndSayWhere is a file that
// writes zero for each interval and size: the defaults are used and the
// settings view attributes them to the defaults, not to the file.
func TestConfigLoad_ZeroedIntervalsFallBackToTheDefaultsAndSayWhere(t *testing.T) {
	body := "server:\n  interval_ms: 0\nfleet:\n  poll_interval_ms: 0\n  timeout_ms: 0\n" +
		"prometheus:\n  sample_interval_ms: 0\n  flush_interval_ms: 0\n  batch_size: 0\n  timeout_ms: 0\n"
	mgr, err := loadFile(t, body)
	if err != nil {
		t.Fatal(err)
	}
	def, got := config.Defaults(), mgr.Current()
	for name, pair := range map[string][2]int{
		"server.interval_ms":            {got.Server.IntervalMs, def.Server.IntervalMs},
		"fleet.poll_interval_ms":        {got.Fleet.PollIntervalMs, def.Fleet.PollIntervalMs},
		"fleet.timeout_ms":              {got.Fleet.TimeoutMs, def.Fleet.TimeoutMs},
		"prometheus.sample_interval_ms": {got.Prometheus.SampleIntervalMs, def.Prometheus.SampleIntervalMs},
		"prometheus.flush_interval_ms":  {got.Prometheus.FlushIntervalMs, def.Prometheus.FlushIntervalMs},
		"prometheus.batch_size":         {got.Prometheus.BatchSize, def.Prometheus.BatchSize},
		"prometheus.timeout_ms":         {got.Prometheus.TimeoutMs, def.Prometheus.TimeoutMs},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d, want the default %d", name, pair[0], pair[1])
		}
		if src := mgr.Sources()[name]; src != types.ConfigSourceDefault {
			t.Errorf("%s is attributed to %q, want the defaults", name, src)
		}
	}
	if got.Fleet.PollInterval() != time.Duration(def.Fleet.PollIntervalMs)*time.Millisecond {
		t.Errorf("Fleet.PollInterval() = %v, want the default", got.Fleet.PollInterval())
	}
	if got.Fleet.Timeout() != time.Duration(def.Fleet.TimeoutMs)*time.Millisecond {
		t.Errorf("Fleet.Timeout() = %v, want the default", got.Fleet.Timeout())
	}
}

// TestConfigLoad_FlagOutranksEnvironmentOutranksFile loads one setting from all
// three layers and checks the winner and its attribution at each step.
func TestConfigLoad_FlagOutranksEnvironmentOutranksFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, "alerts:\n  cpu_pct: 42\n")
	const key = "alerts.cpu_pct"

	cpu := func(l *config.Loader) (float64, types.ConfigSource) {
		t.Helper()
		cfg, src, err := l.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg.Alerts.CPUPct, src[key]
	}

	if got, src := cpu(config.NewLoader(path)); got != 42 || src != types.ConfigSourceFile {
		t.Errorf("file layer: %v from %q, want 42 from the file", got, src)
	}
	t.Setenv(config.EnvPrefix+"_ALERTS_CPU_PCT", "55")
	if got, src := cpu(config.NewLoader(path)); got != 55 || src != types.ConfigSourceEnv {
		t.Errorf("environment layer: %v from %q, want 55 from the environment", got, src)
	}
	flagged := config.NewLoader(path)
	flagged.SetFlag(key, "66")
	if got, src := cpu(flagged); got != 66 || src != types.ConfigSourceFlag {
		t.Errorf("flag layer: %v from %q, want 66 from the flag", got, src)
	}
}

// TestConfigReload_AValidatorRefusalLeavesTheRunningConfigAlone is a setting
// this process cannot use: it parses, the validator rejects it, and nothing is
// published.
func TestConfigReload_AValidatorRefusalLeavesTheRunningConfigAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("this host cannot honour that threshold")
	mgr.OnValidate(func(c *config.Config) error {
		if c.Alerts.CPUPct > 50 {
			return refusal
		}
		return nil
	})

	cfg := config.Defaults()
	cfg.Alerts.CPUPct = 99
	if err := mgr.Validate(&cfg); !errors.Is(err, refusal) {
		t.Errorf("Validate(99) = %v, want the validator's refusal", err)
	}
	cfg.Alerts.CPUPct = 10
	if err := mgr.Validate(&cfg); err != nil {
		t.Errorf("Validate(10) = %v, want nil", err)
	}

	writeFile(t, path, newConfig)
	if _, err := mgr.Reload(); !errors.Is(err, refusal) {
		t.Fatalf("Reload = %v, want the validator's refusal", err)
	}
	if got := mgr.Current().Alerts.CPUPct; got != 42 {
		t.Errorf("cpu_pct = %v after a refused reload, want the running 42", got)
	}
}

// TestConfigReload_BootOnlySettingsAreDeferredNotApplied changes a listener
// setting in the file: the reload reports it as needing a restart and the
// running value stands.
func TestConfigReload_BootOnlySettingsAreDeferredNotApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, "server:\n  addr: 127.0.0.1:8081\n")
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, "server:\n  addr: 127.0.0.1:9090\n  tls_cert: /etc/cert.pem\n  tls_key: /etc/key.pem\n  trusted_proxies: [10.0.0.0/8]\n  watch: true\n"+
		"fleet:\n  tls:\n    ca_file: /etc/ca.pem\n    cert_file: /etc/c.pem\n    key_file: /etc/k.pem\n    insecure_skip_verify: true\n")
	deferred, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	for _, key := range mgr.BootOnly() {
		if !contains(deferred, key) {
			t.Errorf("Reload did not report %s as needing a restart; reported %v", key, deferred)
		}
	}
	if got := mgr.Current().Server.Addr; got != "127.0.0.1:8081" {
		t.Errorf("server.addr = %q after a reload, want the running 127.0.0.1:8081", got)
	}
	if got := mgr.Current().Server.TrustedProxies; len(got) != 0 {
		t.Errorf("server.trusted_proxies = %v after a reload, want the running (empty) list", got)
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// TestConfigView_WithholdsWhatAnUnauthenticatedReaderMustNotLearn builds the
// settings payload for a configuration full of paths, URLs and secrets-adjacent
// values: they are reported as configured, never verbatim, and the peers are
// listed by name only.
func TestConfigView_WithholdsWhatAnUnauthenticatedReaderMustNotLearn(t *testing.T) {
	body := "server:\n  addr: 127.0.0.1:8081\n  tls_cert: /etc/sysmon/cert.pem\n  tls_key: /etc/sysmon/key.pem\n" +
		"prometheus:\n  remote_write_url: http://mon0.internal:9090/api/v1/write\n  query_url: http://mon0.internal:9090\n" +
		"fleet:\n  peers:\n    - name: zeta\n      url: http://zeta.internal:8081\n    - name: alpha\n      url: http://alpha.internal:8081\n"
	mgr, err := loadFile(t, body)
	if err != nil {
		t.Fatal(err)
	}
	view := mgr.View()

	if view.ConfigFile != config.ValueConfigured {
		t.Errorf("ConfigFile = %q, want %q: a file was loaded", view.ConfigFile, config.ValueConfigured)
	}
	byKey := map[string]types.ConfigEntry{}
	var peers []string
	for _, e := range view.Entries {
		byKey[e.Key] = e
		if strings.HasPrefix(e.Key, "fleet.peers.") {
			peers = append(peers, e.Key)
		}
		for _, secret := range []string{"/etc/sysmon", "mon0.internal", "alpha.internal", "zeta.internal"} {
			if strings.Contains(e.Value, secret) || strings.Contains(e.Key, "internal") {
				t.Errorf("entry %s = %q reveals %q", e.Key, e.Value, secret)
			}
		}
	}
	for _, key := range []string{"server.tls_cert", "server.tls_key", "prometheus.remote_write_url", "prometheus.query_url", "fleet.peers.alpha"} {
		if got := byKey[key].Value; got != config.ValueConfigured {
			t.Errorf("%s = %q, want %q", key, got, config.ValueConfigured)
		}
	}
	if got := byKey["fleet.tls.ca_file"].Value; got != config.ValueNotSet {
		t.Errorf("fleet.tls.ca_file = %q, want %q: none is set", got, config.ValueNotSet)
	}
	if got := byKey["server.addr"].Value; got != "127.0.0.1:8081" {
		t.Errorf("server.addr = %q, want it shown: this node's own view", got)
	}
	if len(peers) != 2 || peers[0] != "fleet.peers.alpha" || peers[1] != "fleet.peers.zeta" {
		t.Errorf("peers listed as %v, want alpha then zeta", peers)
	}
}

// TestConfigView_OnAHostWithNoFileSaysSo is the view of a node running purely on
// defaults.
func TestConfigView_OnAHostWithNoFileSaysSo(t *testing.T) {
	mgr, err := config.NewManager(config.NewLoader(filepath.Join(t.TempDir(), "absent.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.View().ConfigFile; got != config.ValueNotSet {
		t.Errorf("ConfigFile = %q, want %q: no file was loaded", got, config.ValueNotSet)
	}
}

// TestConfigView_ARelayedPeerViewIsWithheldAgain is what an aggregator does to
// the settings of a peer it relays: a build that served paths verbatim is
// redacted on the aggregator's own authority, and a peer keyed by a URL is
// re-keyed by position.
func TestConfigView_ARelayedPeerViewIsWithheldAgain(t *testing.T) {
	relayed := types.ConfigView{
		ConfigFile: "/etc/sysmon/sysmon.yaml",
		Entries: []types.ConfigEntry{
			{Section: "server", Key: "server.addr", Value: "10.0.0.7:8081"},
			{Section: "server", Key: "server.tls_key", Value: "/etc/sysmon/key.pem"},
			{Section: "server", Key: "server.tls_cert", Value: types.ConfigView{}.ConfigFile},
			{Section: "alerts", Key: "alerts.cpu_pct", Value: "90"},
			{Section: "fleet", Key: "fleet.peers.http://10.0.0.9:8081", Value: "http://10.0.0.9:8081"},
			{Section: "fleet", Key: "fleet.peers.web1", Value: config.ValueNotSet},
		},
	}
	out := config.RedactPeerView(relayed)

	if out.ConfigFile != config.ValueConfigured {
		t.Errorf("ConfigFile = %q, want %q", out.ConfigFile, config.ValueConfigured)
	}
	want := map[string]string{
		"server.addr":             config.ValueConfigured,
		"server.tls_key":          config.ValueConfigured,
		"server.tls_cert":         config.ValueNotSet,
		"alerts.cpu_pct":          "90",
		"fleet.peers.(unnamed 1)": config.ValueConfigured,
		"fleet.peers.web1":        config.ValueNotSet,
	}
	got := map[string]string{}
	for _, e := range out.Entries {
		got[e.Key] = e.Value
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("relayed %s = %q, want %q (all: %v)", key, got[key], value, got)
		}
	}
}

// TestConfig_LocalNameIsConfiguredHostnameOrAConstant checks the name a node
// publishes, and the collision check against its own peers.
func TestConfig_LocalNameIsConfiguredHostnameOrAConstant(t *testing.T) {
	cfg := config.Defaults()
	cfg.Fleet.LocalName = "edge-1"
	if got := cfg.LocalName(); got != "edge-1" {
		t.Errorf("LocalName() = %q, want the configured edge-1", got)
	}
	cfg.Fleet.LocalName = ""
	if host, err := os.Hostname(); err == nil && host != "" {
		if got := cfg.LocalName(); got != host {
			t.Errorf("LocalName() = %q, want the hostname %q", got, host)
		}
	}

	if err := cfg.ValidateLocalName(""); err != nil {
		t.Errorf("ValidateLocalName(\"\") = %v, want nil", err)
	}
	if err := cfg.ValidateLocalName("edge-1"); err != nil {
		t.Errorf("ValidateLocalName(edge-1) = %v, want nil", err)
	}
	var bad *config.LocalNameError
	if err := cfg.ValidateLocalName("not a name"); !errors.As(err, &bad) {
		t.Fatalf("ValidateLocalName of an invalid name = %v, want a LocalNameError", err)
	} else if !bad.FromHostname {
		t.Error("LocalNameError.FromHostname is false for a name that was not configured")
	}
	cfg.Fleet.Peers = []config.Peer{{Name: "edge-1", URL: "http://edge-1:8081"}}
	if err := cfg.ValidateLocalName("edge-1"); !asA[*config.PeerIsLocalNameError](err) {
		t.Errorf("ValidateLocalName of a peer's name = %v, want a PeerIsLocalNameError", err)
	}
}
