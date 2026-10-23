package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/spf13/cobra"
)

// withServeConfig points the serve command at a temporary config file and
// returns the flag-backed package variables -- which persist across tests -- to
// the defaults they were registered with, which is how "no flag was supplied"
// is spelled when loadServeConfig is called with no cobra command.
func withServeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	oldPath, oldAddr, oldInterval := serveConfigPath, serveAddr, serveIntervalMs
	oldCert, oldKey, oldProxies := serveTLSCert, serveTLSKey, serveTrustedProxies
	t.Cleanup(func() {
		serveConfigPath, serveAddr, serveIntervalMs = oldPath, oldAddr, oldInterval
		serveTLSCert, serveTLSKey, serveTrustedProxies = oldCert, oldKey, oldProxies
	})

	d := config.Defaults()
	serveConfigPath = path
	serveAddr, serveTLSCert, serveTLSKey = d.Server.Addr, d.Server.TLSCert, d.Server.TLSKey
	serveTrustedProxies = d.Server.TrustedProxies
	serveIntervalMs = d.Server.IntervalMs

	return path
}

// TestLoadServeConfig_UnsuppliedAddrLeavesTheFileInCharge is the regression
// this issue is really about (#17). The flag layer outranks the config file, so
// a flag default that reaches it is not merely redundant -- it wins. The
// aggregator's sysmon.yaml asks for :8081; a serve loop that handed the
// built-in ":8080" to the flag layer would bind that instead and report it as
// having come from a flag nobody typed.
func TestLoadServeConfig_UnsuppliedAddrLeavesTheFileInCharge(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n")

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := mgr.Current().Server.Addr; got != ":8081" {
		t.Errorf("server.addr = %q, want %q from the config file", got, ":8081")
	}
	if got := mgr.Sources()["server.addr"]; got != types.ConfigSourceFile {
		t.Errorf("source = %q, want %q", got, types.ConfigSourceFile)
	}
}

// TestLoadServeConfig_SuppliedAddrOutranksTheFile is the other half: a flag the
// operator actually gave still wins, which is what makes `--addr` worth having.
func TestLoadServeConfig_SuppliedAddrOutranksTheFile(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n")
	serveAddr = "127.0.0.1:9099"

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := mgr.Current().Server.Addr; got != "127.0.0.1:9099" {
		t.Errorf("server.addr = %q, want %q from the flag", got, "127.0.0.1:9099")
	}
	if got := mgr.Sources()["server.addr"]; got != types.ConfigSourceFlag {
		t.Errorf("source = %q, want %q", got, types.ConfigSourceFlag)
	}
}

// TestLoadServeConfig_EmptyAddrFlagIsNotAValue covers the bad input: `--addr ""`
// names no address, so it must not displace the file and leave normalize to put
// the default back under a "flag" label.
func TestLoadServeConfig_EmptyAddrFlagIsNotAValue(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n")
	serveAddr = ""

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := mgr.Current().Server.Addr; got != ":8081" {
		t.Errorf("server.addr = %q, want %q from the config file", got, ":8081")
	}
}

// TestLoadServeConfig_RejectsPeerNamedLikeThisNode proves the collision check is
// actually WIRED UP, not merely available. The check itself lives in pkg/config
// and is tested there; what this covers is that serve registers it, which is the
// part that silently stops working if the registration is dropped.
//
// Startup is the path that matters most: the fleet handlers resolve the local
// name before the peer cache, so a node that boots with this config never polls
// the configured host while showing a card that looks like it.
func TestLoadServeConfig_RejectsPeerNamedLikeThisNode(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: aggregator\n      url: http://10.0.0.1:8081\n")

	var clash *config.PeerIsLocalNameError
	if _, err := loadServeConfig(nil); !errors.As(err, &clash) {
		t.Fatalf("loadServeConfig() = %v, want a *config.PeerIsLocalNameError", err)
	} else if clash.Name != "aggregator" {
		t.Errorf("PeerIsLocalNameError.Name = %q, want %q", clash.Name, "aggregator")
	}
}

// TestLoadServeConfig_RejectsCollisionIntroducedByReload covers the other half
// of the wiring: the same check has to survive a reload, and rejecting has to
// leave the running configuration in place rather than reporting a config that
// already went live.
func TestLoadServeConfig_RejectsCollisionIntroducedByReload(t *testing.T) {
	path := withServeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig() with a valid fleet: %v", err)
	}

	// Rename this node onto its own peer. Only the incoming name reveals the
	// collision -- the outgoing "aggregator" clashes with nothing.
	bad := "fleet:\n  local_name: fs\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	var clash *config.PeerIsLocalNameError
	if _, err := mgr.Reload(); !errors.As(err, &clash) {
		t.Fatalf("Reload() into a collision = %v, want a *config.PeerIsLocalNameError", err)
	}
	if got := mgr.Current().LocalName(); got != "aggregator" {
		t.Errorf("after a rejected reload LocalName() = %q, want the previous %q", got, "aggregator")
	}
}

// TestLoadServeConfig_RejectsUnusableLocalName proves serve refuses to start
// on a fleet.local_name that cannot be an instance label (owner ruling on #37).
func TestLoadServeConfig_RejectsUnusableLocalName(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: 'agg regator'\n")

	var bad *config.LocalNameError
	if _, err := loadServeConfig(nil); !errors.As(err, &bad) {
		t.Fatalf("loadServeConfig() = %v, want a *config.LocalNameError", err)
	}
}

// TestLoadServeConfig_AcceptsAnOrdinaryFleet is the no-regression guard: the
// validator must not reject a fleet whose peers are simply other machines.
func TestLoadServeConfig_AcceptsAnOrdinaryFleet(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n    - name: gpu-host\n      url: http://10.0.0.2:8081\n")

	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig() with an ordinary fleet = %v, want it accepted", err)
	}
	if n := len(mgr.Current().Fleet.Peers); n != 2 {
		t.Errorf("peers = %d, want 2", n)
	}
}

// TestLoadServeConfig_OnlyChangedFlagsReachTheFlagLayer drives the command
// path: with a cobra command, a flag counts as supplied only when the user
// changed it, so a value the command was merely registered with never
// outranks the config file.
func TestLoadServeConfig_OnlyChangedFlagsReachTheFlagLayer(t *testing.T) {
	withServeConfig(t, "server:\n  addr: \":8081\"\n  interval_ms: 3000\n")
	d := config.Defaults()
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().StringVar(&serveAddr, "addr", ":9999", "")
	cmd.Flags().IntVar(&serveIntervalMs, "interval", d.Server.IntervalMs, "")
	cmd.Flags().StringVar(&serveTLSCert, "tls-cert", "", "")
	cmd.Flags().StringVar(&serveTLSKey, "tls-key", "", "")
	for flag, value := range map[string]string{"interval": "2000", "tls-cert": "/etc/sysmon/cert.pem", "tls-key": "/etc/sysmon/key.pem"} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}

	mgr, err := loadServeConfig(cmd)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}

	got := mgr.Current().Server
	if got.Addr != ":8081" {
		t.Errorf("server.addr = %q, want the file's :8081 -- --addr was never typed", got.Addr)
	}
	if got.IntervalMs != 2000 || got.TLSCert != "/etc/sysmon/cert.pem" || got.TLSKey != "/etc/sysmon/key.pem" {
		t.Errorf("server = %+v, want the three typed flags applied", got)
	}
	for _, key := range []string{"server.interval_ms", "server.tls_cert", "server.tls_key"} {
		if src := mgr.Sources()[key]; src != types.ConfigSourceFlag {
			t.Errorf("%s source = %q, want %q", key, src, types.ConfigSourceFlag)
		}
	}
	if tls := serveTLSConfig(mgr.Current()); tls == nil || tls.CertFile != got.TLSCert || tls.KeyFile != got.TLSKey {
		t.Errorf("serveTLSConfig() = %+v, want the flags' certificate and key", tls)
	}
}

// intervalCommand is a serve command with only --interval, given value.
func intervalCommand(t *testing.T, value string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().IntVar(&serveIntervalMs, "interval", config.Defaults().Server.IntervalMs, "")
	if err := cmd.Flags().Set("interval", value); err != nil {
		t.Fatalf("set --interval: %v", err)
	}
	return cmd
}

// TestLoadServeConfig_IntervalFlagHonoursTheFloor: --interval goes through the
// same floor check as the config file; 250 is refused and 500 accepted.
func TestLoadServeConfig_IntervalFlagHonoursTheFloor(t *testing.T) {
	withServeConfig(t, "")

	var floor *config.IntervalBelowFloorError
	if _, err := loadServeConfig(intervalCommand(t, "250")); !errors.As(err, &floor) {
		t.Fatalf("loadServeConfig(--interval 250) = %v, want a *config.IntervalBelowFloorError", err)
	}

	mgr, err := loadServeConfig(intervalCommand(t, "500"))
	if err != nil {
		t.Fatalf("loadServeConfig(--interval 500): %v", err)
	}
	if got := mgr.Current().Server.IntervalMs; got != 500 {
		t.Errorf("server.interval_ms = %d, want 500", got)
	}
}

// trustedProxiesCommand is a serve command with only --trusted-proxies,
// given values.
func trustedProxiesCommand(t *testing.T, values ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().StringSliceVar(&serveTrustedProxies, "trusted-proxies", nil, "")
	for _, value := range values {
		if err := cmd.Flags().Set("trusted-proxies", value); err != nil {
			t.Fatalf("set --trusted-proxies: %v", err)
		}
	}
	return cmd
}

// TestLoadServeConfig_TrustedProxiesFlag: the flag may be repeated or
// comma-separated, and outranks the file.
func TestLoadServeConfig_TrustedProxiesFlag(t *testing.T) {
	withServeConfig(t, "server:\n  trusted_proxies: [192.0.2.0/24]\n")

	mgr, err := loadServeConfig(trustedProxiesCommand(t, "10.0.0.0/8,fd00::/8", "172.16.0.0/12"))
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	want := []string{"10.0.0.0/8", "fd00::/8", "172.16.0.0/12"}
	if got := mgr.Current().Server.TrustedProxies; !slices.Equal(got, want) {
		t.Errorf("server.trusted_proxies = %v, want %v", got, want)
	}
	if src := mgr.Sources()["server.trusted_proxies"]; src != types.ConfigSourceFlag {
		t.Errorf("server.trusted_proxies source = %q, want %q", src, types.ConfigSourceFlag)
	}
}

func TestLoadServeConfig_InvalidTrustedProxyFailsStartup(t *testing.T) {
	withServeConfig(t, "")

	_, err := loadServeConfig(trustedProxiesCommand(t, "10.0.0.1"))
	var proxyErr *config.TrustedProxyError
	if !errors.As(err, &proxyErr) {
		t.Fatalf("loadServeConfig() error = %v, want a TrustedProxyError", err)
	}
}

// TestServeTLSConfig_NilWithoutACertificate verifies plain HTTP is served
// when neither TLS setting is given.
func TestServeTLSConfig_NilWithoutACertificate(t *testing.T) {
	d := config.Defaults()
	if tls := serveTLSConfig(&d); tls != nil {
		t.Errorf("serveTLSConfig(defaults) = %+v, want nil", tls)
	}
}

// TestLocalNodeName_ConfiguredNameWins verifies the fleet name comes from
// fleet.local_name when set, and falls back to the hostname without a config.
func TestLocalNodeName_ConfiguredNameWins(t *testing.T) {
	withServeConfig(t, "fleet:\n  local_name: rack-7\n")
	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if got := localNodeName(mgr); got != "rack-7" {
		t.Errorf("localNodeName() = %q, want the configured rack-7", got)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if got := localNodeName(nil); got != host {
		t.Errorf("localNodeName(nil) = %q, want the hostname %q", got, host)
	}
}

// TestRunServeCmd_BrokenConfigIsAStartupError verifies a config file that
// does not parse stops the service before anything starts.
func TestRunServeCmd_BrokenConfigIsAStartupError(t *testing.T) {
	previous := listenHook
	t.Cleanup(func() { listenHook = previous })
	listenHook = func(net.Addr) { t.Error("the service bound its listener despite a broken config") }

	if err := runServeWithConfig(t, "server: [\n"); err == nil {
		t.Fatal("runServeCmd() = nil, want the unparsable config reported")
	}
}
