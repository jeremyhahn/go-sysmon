//go:build integration

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestServe_RefusesAConfigItCannotUse runs the service against every kind of
// configuration it must refuse, and checks each is refused at startup --
// before anything binds -- with an error naming what is wrong. Starting on
// defaults instead would drop the operator's settings while looking healthy.
func TestServe_RefusesAConfigItCannotUse(t *testing.T) {
	// Longer than the rule allows and carrying a control character, written
	// as a YAML escape: the refusal quotes it shortened and made printable.
	longName := "x" + `\x01` + strings.Repeat("x", 300)
	tests := []struct {
		name   string
		config string
		want   []string
	}{
		{"yaml that does not parse", "server: [\n", []string{"config: cannot load"}},
		{"a key that maps to nothing", "fleet:\n  peer:\n    - name: a\n      url: http://a:8081\n",
			[]string{"config: cannot load", "peer"}},
		{"a retired key", "alerts:\n  swap_pct: 50\n",
			[]string{"sets alerts.swap_pct, which this build no longer has", "alerts.swap_out_pages_per_sec"}},
		{"a peer with no name", "fleet:\n  peers:\n    - url: http://a:8081\n",
			[]string{"fleet peer 1 in fleet.peers has no name"}},
		{"a peer with no url", "fleet:\n  peers:\n    - name: db-prod\n",
			[]string{`fleet peer "db-prod" has an unusable url: no url`}},
		{"a peer url with no scheme", "fleet:\n  peers:\n    - name: db\n      url: \"db:8081\"\n",
			[]string{`fleet peer "db" has an unusable url`, `scheme "db" is not supported`}},
		{"a peer url that does not parse", "fleet:\n  peers:\n    - name: db\n      url: \"http://[::1\"\n",
			[]string{`fleet peer "db" has an unusable url`}},
		{"a peer url with no host", "fleet:\n  peers:\n    - name: db\n      url: \"http:///x\"\n",
			[]string{`fleet peer "db" has an unusable url: no host`}},
		{"a peer url with no scheme at all", "fleet:\n  peers:\n    - name: db\n      url: \"/x\"\n",
			[]string{`fleet peer "db" has an unusable url: no scheme`}},
		{"credentials in a peer url", "fleet:\n  peers:\n    - name: db\n      url: \"http://admin:hunter2@db:8081\"\n",
			[]string{`fleet peer "db"`, "credentials"}},
		{"a peer name that cannot be a node name", "fleet:\n  peers:\n    - name: \"db prod\"\n      url: http://db:8081\n",
			[]string{"has an unusable name", "a node name must start with a letter or digit"}},
		{"two peers with one name", "fleet:\n  peers:\n    - name: db\n      url: http://db1:8081\n    - name: db\n      url: http://db2:8081\n",
			[]string{`fleet peer name "db" is used by more than one peer`}},
		{"a local name that cannot be a node name", "fleet:\n  local_name: \"" + longName + "\"\n",
			[]string{"fleet.local_name", "is unusable", `"x.` + strings.Repeat("x", 62) + `..."`, "a node name must start"}},
		{"a peer named like this node", "fleet:\n  local_name: rack-7\n  peers:\n    - name: rack-7\n      url: http://other:8081\n",
			[]string{`fleet peer "rack-7" is also this node's own name`}},
		{"a remote-write url with no scheme", "prometheus:\n  remote_write_url: \"mon0:9090\"\n",
			[]string{"prometheus.remote_write_url is unusable"}},
		{"a query url that does not parse", "prometheus:\n  query_url: \"http://[::1\"\n",
			[]string{"prometheus.query_url is unusable"}},
		{"credentials in a remote-write url", "prometheus:\n  remote_write_url: \"https://u:p@mon0:9090/api/v1/write\"\n",
			[]string{"prometheus.remote_write_url must not carry credentials"}},
		{"a reserved Prometheus label", "prometheus:\n  external_labels:\n    __tenant: a\n",
			[]string{`prometheus.external_labels "__tenant" is unusable`, "reserved by Prometheus"}},
		{"a label Prometheus cannot accept", "prometheus:\n  external_labels:\n    1site: a\n",
			[]string{`"1site" is unusable`, "must match"}},
		{"a label go-sysmon sets itself", "prometheus:\n  external_labels:\n    instance: a\n",
			[]string{`"instance" is unusable`, "go-sysmon sets this label itself"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := harness.WriteFile(t, "sysmon.yaml", tt.config)
			stderr := failsToStart(t, nil, "--config", path)
			for _, want := range tt.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, stderr)
				}
			}
			// A credential is never echoed back, not even in the refusal.
			if strings.Contains(stderr, "hunter2") {
				t.Errorf("the refusal repeats the peer's password:\n%s", stderr)
			}
		})
	}
}

// TestServe_RefusesAConfigPathItCannotRead verifies a config path that exists
// but cannot be read -- here, a path through a regular file -- stops the
// service, where an absent file would have meant "run on defaults".
func TestServe_RefusesAConfigPathItCannotRead(t *testing.T) {
	file := harness.WriteFile(t, "not-a-dir", "x")
	stderr := failsToStart(t, nil, "--config", filepath.Join(file, "sysmon.yaml"))
	if !strings.Contains(stderr, "config: cannot read") {
		t.Errorf("stderr does not say the config cannot be read:\n%s", stderr)
	}
}

// TestServe_RefusesAConfigFileThatCannotBeOpened verifies a config file that is
// there but cannot be read -- a directory in its place -- is refused rather
// than treated as absent.
func TestServe_RefusesAConfigFileThatCannotBeOpened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stderr := failsToStart(t, nil, "--config", dir)
	if !strings.Contains(stderr, "config: cannot load "+dir) {
		t.Errorf("stderr does not name the unreadable file:\n%s", stderr)
	}
}

// TestServe_RefusesABadEnvironmentOverride verifies an environment variable is
// held to the same rules as the file it overrides.
func TestServe_RefusesABadEnvironmentOverride(t *testing.T) {
	stderr := failsToStart(t, []string{"SYSMON_PROMETHEUS_REMOTE_WRITE_URL=ftp://mon0/write"},
		"--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if !strings.Contains(stderr, `scheme "ftp" is not supported`) {
		t.Errorf("stderr does not reject the environment's url:\n%s", stderr)
	}
}

// TestServe_RefusesANonPositiveInterval verifies --interval 0 and below is a
// usage error, not a server polling as fast as it can.
func TestServe_RefusesANonPositiveInterval(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		stderr := failsToStart(t, nil, "--interval", v, "--config", filepath.Join(t.TempDir(), "absent.yaml"))
		if !strings.Contains(stderr, "interval must be greater than zero") {
			t.Errorf("--interval %s: stderr does not say why:\n%s", v, stderr)
		}
	}
}

// TestServe_RefusesAHostnameThatCannotBeANodeName runs the service on a host
// whose name cannot be a node name, with no fleet.local_name to stand in: it
// refuses to start and says to set fleet.local_name. The hostname is changed
// in a UTS namespace of the service's own, never the container's.
func TestServe_RefusesAHostnameThatCannotBeANodeName(t *testing.T) {
	script := `printf '%s' "$1" > /proc/sys/kernel/hostname && shift && exec "$@"`
	r := harness.Run(t, "unshare", nil, "--uts", "/bin/sh", "-c", script, "sh", "db prod",
		harness.Bins.Sysmon, "serve", "--addr", "127.0.0.1:0",
		"--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if r.Code != 1 {
		t.Fatalf("exit %d, want 1\nstderr:\n%s", r.Code, r.Stderr)
	}
	for _, want := range []string{"fleet.local_name is not set", `"db prod"`} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, r.Stderr)
		}
	}
}
