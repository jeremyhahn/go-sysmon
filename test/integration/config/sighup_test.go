//go:build integration

package config

import (
	"slices"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestSIGHUP_AppliesTheFileAndNamesWhatNeedsARestart edits the file of a
// running service and sends SIGHUP: a reloadable setting takes effect at once,
// and the ones consumed at startup -- the TLS certificate and key -- are named
// as needing a restart while the view keeps reporting what is in use. A
// changed listen address is not among them: --addr is on the command line and
// outranks the file, so the effective address did not change.
func TestSIGHUP_AppliesTheFileAndNamesWhatNeedsARestart(t *testing.T) {
	path := configDir(t, "alerts:\n  memory_pct: 80\n")
	s := harness.Serve(t, harness.ServeOptions{ConfigPath: path})
	viewOf(t, s).want(t, "alerts.memory_pct", "80", types.ConfigSourceFile)

	rewrite(t, path, "server:\n  addr: \"127.0.0.1:1\"\n  tls_cert: /etc/sysmon/cert.pem\n  tls_key: /etc/sysmon/key.pem\n"+
		"alerts:\n  memory_pct: 65\n")
	s.Signal(t, syscall.SIGHUP)

	s.Logs.AwaitMsg(t, msgSIGHUPReloaded)
	restart := s.Logs.AwaitMsg(t, msgNeedsRestart)
	if got := settingsOf(restart); !slices.Equal(got, []string{"server.tls_cert", "server.tls_key"}) {
		t.Errorf("settings needing a restart = %q, want [server.tls_cert server.tls_key]", got)
	}
	view := viewOf(t, s)
	view.want(t, "alerts.memory_pct", "65", types.ConfigSourceFile)
	// Still plain HTTP, as it started: the view says so, and not what the
	// file now asks for.
	view.want(t, "server.tls_cert", "(not set)", types.ConfigSourceDefault)
	view.want(t, "server.addr", "127.0.0.1:0", types.ConfigSourceFlag)
}

// TestSIGHUP_KeepsTheSettingsWhenTheFileIsBroken verifies a reload of a file
// that no longer parses leaves the running configuration in force, and says
// so at warning level.
func TestSIGHUP_KeepsTheSettingsWhenTheFileIsBroken(t *testing.T) {
	path := configDir(t, "alerts:\n  memory_pct: 80\n")
	s := harness.Serve(t, harness.ServeOptions{ConfigPath: path})

	rewrite(t, path, "alerts: [\n")
	s.Signal(t, syscall.SIGHUP)

	if r := s.Logs.AwaitMsg(t, msgSIGHUPFailed); r.Level() != "WARN" {
		t.Errorf("a failed reload is logged at %s, want WARN", r.Level())
	}
	viewOf(t, s).want(t, "alerts.memory_pct", "80", types.ConfigSourceFile)
}

// TestSIGHUP_RefusesAPeerNamedLikeThisNode verifies a reload is held to the
// checks startup applies: a peer that takes this node's own name is refused
// and the fleet stays as it was.
func TestSIGHUP_RefusesAPeerNamedLikeThisNode(t *testing.T) {
	path := configDir(t, "fleet:\n  local_name: rack-7\n")
	s := harness.Serve(t, harness.ServeOptions{ConfigPath: path})

	rewrite(t, path, "fleet:\n  local_name: rack-7\n  peers:\n    - name: rack-7\n      url: http://other:8081\n")
	s.Signal(t, syscall.SIGHUP)

	r := s.Logs.AwaitMsg(t, msgSIGHUPFailed)
	if errText, _ := r["error"].(string); errText == "" {
		t.Errorf("the refusal carries no error: %v", r)
	}
	if _, ok := viewOf(t, s).entries["fleet.peers.rack-7"]; ok {
		t.Error("the refused peer is in the settings view")
	}
}

// TestSIGHUP_ReappliesTheConfiguredInterval verifies a reload makes the
// configuration true again: an interval chosen in the dashboard is replaced by
// the configured one -- here the --interval 200 the service was started with,
// which outranks the file's 1000.
func TestSIGHUP_ReappliesTheConfiguredInterval(t *testing.T) {
	path := configDir(t, "server:\n  interval_ms: 1000\n")
	s := harness.Serve(t, harness.ServeOptions{ConfigPath: path})
	if status, body := harness.Post(t, s.URL("/api/interval"), `{"interval_ms":5000}`); status/100 != 2 {
		t.Fatalf("POST /api/interval = %d: %s", status, body)
	}
	viewOf(t, s).want(t, "server.interval_ms", "5000", types.ConfigSourceRuntime)

	s.Signal(t, syscall.SIGHUP)
	s.Logs.AwaitMsg(t, msgSIGHUPReloaded)

	viewOf(t, s).want(t, "server.interval_ms", "200", types.ConfigSourceFlag)
}

// TestSIGHUP_PeerTransportNeedsARestart verifies every fleet.tls setting that
// is baked into the peer client at startup is named as needing a restart when
// a reload changes it, and keeps its running value in the view.
func TestSIGHUP_PeerTransportNeedsARestart(t *testing.T) {
	p := newPKI(t)
	path := configDir(t, "fleet:\n  local_name: rack-7\n")
	s := harness.Serve(t, harness.ServeOptions{ConfigPath: path})

	rewrite(t, path, "fleet:\n  local_name: rack-7\n  tls:\n    ca_file: "+p.caFile+
		"\n    cert_file: "+p.certFile+"\n    key_file: "+p.keyFile+"\n    insecure_skip_verify: true\n")
	s.Signal(t, syscall.SIGHUP)

	restart := s.Logs.AwaitMsg(t, msgNeedsRestart)
	want := []string{"fleet.tls.ca_file", "fleet.tls.cert_file", "fleet.tls.key_file", "fleet.tls.insecure_skip_verify"}
	if got := settingsOf(restart); !slices.Equal(got, want) {
		t.Errorf("settings needing a restart = %q, want %q", got, want)
	}
	view := viewOf(t, s)
	view.want(t, "fleet.tls.ca_file", "(not set)", types.ConfigSourceDefault)
	view.want(t, "fleet.tls.insecure_skip_verify", "false", types.ConfigSourceDefault)
}

// TestSIGHUP_AListenAddressFromTheFileNeedsARestart runs the service with its
// listen address and interval taken from the file alone -- no flags -- so a
// reload that changes the address is a change to the effective setting, and
// is named as needing a restart. The file's non-positive interval is repaired
// to the default, and the view credits the default.
func TestSIGHUP_AListenAddressFromTheFileNeedsARestart(t *testing.T) {
	path := configDir(t, "server:\n  addr: \"127.0.0.1:0\"\n  interval_ms: 0\n")
	s := harness.Start(t, harness.Bins.Sysmon, nil, "serve", "--log-format", "json", "--config", path)
	addr := s.Logs.AwaitMsg(t, harness.ListeningMsg)["addr"].(string)
	s.Addr = addr
	harness.WaitForSnapshot(t, s.URL("/api/snapshot"))
	view := viewOf(t, s)
	view.want(t, "server.addr", "127.0.0.1:0", types.ConfigSourceFile)
	view.want(t, "server.interval_ms", "1000", types.ConfigSourceDefault)

	rewrite(t, path, "server:\n  addr: \"127.0.0.1:1\"\n")
	s.Signal(t, syscall.SIGHUP)

	restart := s.Logs.AwaitMsg(t, msgNeedsRestart)
	if got := settingsOf(restart); !slices.Equal(got, []string{"server.addr"}) {
		t.Errorf("settings needing a restart = %q, want [server.addr]", got)
	}
	viewOf(t, s).want(t, "server.addr", "127.0.0.1:0", types.ConfigSourceFile)
}
