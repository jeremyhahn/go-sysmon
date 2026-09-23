package config_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// writeConfig lays down a config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoad_MissingFileUsesDefaults verifies a host with no config file still
// gets a working configuration: absence is not an error.
func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	l := config.NewLoader(filepath.Join(t.TempDir(), "absent.yaml"))
	cfg, sources, err := l.Load()
	if err != nil {
		t.Fatalf("Load() with no file: %v", err)
	}
	if cfg.Alerts.DiskFullPct != config.Defaults().Alerts.DiskFullPct {
		t.Errorf("disk_full_pct = %v, want the default", cfg.Alerts.DiskFullPct)
	}
	if sources["alerts.disk_full_pct"] != config.SourceDefault {
		t.Errorf("source = %q, want %q", sources["alerts.disk_full_pct"], config.SourceDefault)
	}
}

// TestLoad_FileOverridesDefault covers the config file layer, including the
// peer list the fleet view depends on.
func TestLoad_FileOverridesDefault(t *testing.T) {
	path := writeConfig(t, `
alerts:
  disk_full_pct: 75
fleet:
  peers:
    - name: fs
      url: http://10.0.0.1:8081
`)
	cfg, sources, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Alerts.DiskFullPct != 75 {
		t.Errorf("disk_full_pct = %v, want 75", cfg.Alerts.DiskFullPct)
	}
	if sources["alerts.disk_full_pct"] != config.SourceFile {
		t.Errorf("source = %q, want %q", sources["alerts.disk_full_pct"], config.SourceFile)
	}
	if len(cfg.Fleet.Peers) != 1 || cfg.Fleet.Peers[0].Name != "fs" {
		t.Fatalf("peers = %+v, want one named fs", cfg.Fleet.Peers)
	}
}

// TestLoad_EnvOutranksFile checks the documented precedence and that the
// settings view can attribute the value to the environment.
func TestLoad_EnvOutranksFile(t *testing.T) {
	path := writeConfig(t, "alerts:\n  disk_full_pct: 75\n")
	t.Setenv("SYSMON_ALERTS_DISK_FULL_PCT", "60")

	cfg, sources, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Alerts.DiskFullPct != 60 {
		t.Errorf("disk_full_pct = %v, want 60 from the environment", cfg.Alerts.DiskFullPct)
	}
	if sources["alerts.disk_full_pct"] != config.SourceEnv {
		t.Errorf("source = %q, want %q", sources["alerts.disk_full_pct"], config.SourceEnv)
	}
}

// TestLoad_FlagOutranksEnv checks the top of the precedence chain.
func TestLoad_FlagOutranksEnv(t *testing.T) {
	path := writeConfig(t, "alerts:\n  disk_full_pct: 75\n")
	t.Setenv("SYSMON_ALERTS_DISK_FULL_PCT", "60")

	ld := l(path)
	ld.SetFlag("alerts.disk_full_pct", "50")
	cfg, sources, err := ld.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Alerts.DiskFullPct != 50 {
		t.Errorf("disk_full_pct = %v, want 50 from the flag", cfg.Alerts.DiskFullPct)
	}
	if sources["alerts.disk_full_pct"] != config.SourceFlag {
		t.Errorf("source = %q, want %q", sources["alerts.disk_full_pct"], config.SourceFlag)
	}
}

// TestLoad_ServerAddrClimbsEveryLayer walks the listen address up the whole
// precedence ladder in one test, because it is the ladder that matters here
// rather than any single rung: the deployments this setting exists for -- the
// fleet aggregator and its peers -- each reach it from a different layer
// (#17), and a rung that quietly stopped outranking the one below it would
// bind the wrong port on a live host while the settings view agreed with the
// file it was ignoring.
//
// Each step adds exactly one layer and asserts both the effective value and
// the layer credited with it, so a break says which rung gave way.
func TestLoad_ServerAddrClimbsEveryLayer(t *testing.T) {
	// No file, no environment, no flag: the built-in address, and nothing
	// claiming to have supplied it.
	absent := filepath.Join(t.TempDir(), "absent.yaml")
	cfg, sources, err := config.NewLoader(absent).Load()
	if err != nil {
		t.Fatalf("Load() with no file: %v", err)
	}
	if want := config.Defaults().Server.Addr; cfg.Server.Addr != want {
		t.Errorf("server.addr = %q, want the default %q", cfg.Server.Addr, want)
	}
	if got := sources["server.addr"]; got != config.SourceDefault {
		t.Errorf("source = %q, want %q", got, config.SourceDefault)
	}

	// The config file outranks the default.
	path := writeConfig(t, "server:\n  addr: \":8081\"\n")
	cfg, sources, err = l(path).Load()
	if err != nil {
		t.Fatalf("Load() with a file: %v", err)
	}
	if cfg.Server.Addr != ":8081" {
		t.Errorf("server.addr = %q, want %q from the file", cfg.Server.Addr, ":8081")
	}
	if got := sources["server.addr"]; got != config.SourceFile {
		t.Errorf("source = %q, want %q", got, config.SourceFile)
	}

	// The environment outranks the file.
	t.Setenv("SYSMON_SERVER_ADDR", "127.0.0.1:9090")
	cfg, sources, err = l(path).Load()
	if err != nil {
		t.Fatalf("Load() with an environment override: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:9090" {
		t.Errorf("server.addr = %q, want %q from the environment", cfg.Server.Addr, "127.0.0.1:9090")
	}
	if got := sources["server.addr"]; got != config.SourceEnv {
		t.Errorf("source = %q, want %q", got, config.SourceEnv)
	}

	// The flag outranks everything.
	ld := l(path)
	ld.SetFlag("server.addr", "10.0.0.5:8443")
	cfg, sources, err = ld.Load()
	if err != nil {
		t.Fatalf("Load() with a flag: %v", err)
	}
	if cfg.Server.Addr != "10.0.0.5:8443" {
		t.Errorf("server.addr = %q, want %q from the flag", cfg.Server.Addr, "10.0.0.5:8443")
	}
	if got := sources["server.addr"]; got != config.SourceFlag {
		t.Errorf("source = %q, want %q", got, config.SourceFlag)
	}
}

// TestLoad_EmptyServerAddrIsRepairedToTheDefault is the bad-input half: an
// `addr:` line with nothing after it parses fine and yields "", which
// net.Listen reads as "every interface, port 0" -- a kernel-chosen port that
// nothing knows how to reach. The default is restored instead, and credited to
// the defaults so the settings view does not point at a file saying otherwise.
func TestLoad_EmptyServerAddrIsRepairedToTheDefault(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \"\"\n")
	cfg, sources, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if want := config.Defaults().Server.Addr; cfg.Server.Addr != want {
		t.Errorf("server.addr = %q, want the default %q", cfg.Server.Addr, want)
	}
	if got := sources["server.addr"]; got != config.SourceDefault {
		t.Errorf("source = %q, want %q -- the file did not supply the effective value", got, config.SourceDefault)
	}
}

// TestLoad_MalformedFileIsAnError distinguishes a broken file from a missing
// one: a typo must be reported, not silently ignored.
func TestLoad_MalformedFileIsAnError(t *testing.T) {
	path := writeConfig(t, "alerts:\n  disk_full_pct: [not a number\n")
	if _, _, err := l(path).Load(); err == nil {
		t.Fatal("Load() on a malformed file = nil error, want a parse error")
	}
}

// TestManagerReload_PicksUpChanges is the behaviour the whole reload path
// exists for: a peer added on disk becomes visible without restarting.
func TestManagerReload_PicksUpChanges(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers: []\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if len(mgr.Current().Fleet.Peers) != 0 {
		t.Fatalf("expected no peers initially")
	}

	if err := os.WriteFile(path, []byte("fleet:\n  peers:\n    - name: omega\n      url: http://10.0.0.2:8081\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}
	peers := mgr.Current().Fleet.Peers
	if len(peers) != 1 || peers[0].Name != "omega" {
		t.Errorf("after reload peers = %+v, want one named omega", peers)
	}
}

// TestManagerReload_ReportsRestartOnlySettings verifies that changing the
// listen address is reported as needing a restart rather than pretending to
// apply: the socket was bound at startup.
func TestManagerReload_ReportsRestartOnlySettings(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := os.WriteFile(path, []byte("server:\n  addr: \":9090\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deferred, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload(): %v", err)
	}
	if len(deferred) == 0 {
		t.Error("changing server.addr should be reported as needing a restart")
	}
}

// TestManagerReload_KeepsConfigOnError proves a broken edit cannot take the
// running settings down with it.
func TestManagerReload_KeepsConfigOnError(t *testing.T) {
	path := writeConfig(t, "alerts:\n  disk_full_pct: 75\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := os.WriteFile(path, []byte("alerts:\n  disk_full_pct: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err == nil {
		t.Fatal("Reload() on a broken file = nil, want an error")
	}
	if got := mgr.Current().Alerts.DiskFullPct; got != 75 {
		t.Errorf("after a failed reload disk_full_pct = %v, want the previous 75", got)
	}
}

// TestView_ReportsEveryKeyWithSource covers the read-only settings payload.
func TestView_ReportsEveryKeyWithSource(t *testing.T) {
	path := writeConfig(t, "alerts:\n  temp_crit_c: 88\nfleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	view := mgr.View()
	// The file is reported as in use, not by name -- see
	// TestView_DoesNotDiscloseTheConfigFilePath.
	if view.ConfigFile != config.ValueConfigured {
		t.Errorf("ConfigFile = %q, want %q", view.ConfigFile, config.ValueConfigured)
	}

	var sawTemp, sawPeer bool
	for _, e := range view.Entries {
		if e.Key == "alerts.temp_crit_c" {
			sawTemp = true
			if e.Value != "88" || e.Source != config.SourceFile {
				t.Errorf("temp_crit_c entry = %+v, want value 88 from the config file", e)
			}
		}
		// The peer is present by NAME, with its URL withheld -- see
		// TestView_DoesNotDisclosePeerURLs.
		if e.Key == "fleet.peers.fs" {
			sawPeer = true
			if e.Value != config.ValueConfigured {
				t.Errorf("peer entry value = %q, want %q", e.Value, config.ValueConfigured)
			}
		}
	}
	if !sawTemp || !sawPeer {
		t.Errorf("view is missing entries (temp=%v peer=%v)", sawTemp, sawPeer)
	}
}

// TestView_DoesNotDiscloseTheConfigFilePath is the read-side half of #16: GET
// /api/config has no authentication of its own, and it used to hand back the
// absolute path of the configuration file. An operator needs to know whether a
// file is in force -- otherwise every value in the dialog is unexplained -- but
// not where it lives, which is answered by the unit and by --config, both of
// which require being on the machine.
func TestView_DoesNotDiscloseTheConfigFilePath(t *testing.T) {
	path := writeConfig(t, "alerts:\n  temp_crit_c: 88\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	view := mgr.View()
	if strings.Contains(view.ConfigFile, path) || strings.Contains(view.ConfigFile, "/") {
		t.Errorf("ConfigFile = %q, want no filesystem path", view.ConfigFile)
	}
	if view.ConfigFile != config.ValueConfigured {
		t.Errorf("ConfigFile = %q, want %q -- a file IS in use", view.ConfigFile, config.ValueConfigured)
	}
}

// TestView_ReportsNoConfigFileWhenThereIsNone is the other state: a host
// running on defaults and environment must be distinguishable from one reading
// a file, or the settings dialog cannot explain where its values came from.
func TestView_ReportsNoConfigFileWhenThereIsNone(t *testing.T) {
	mgr, err := config.NewManager(config.NewLoader(filepath.Join(t.TempDir(), "absent.yaml")))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if got := mgr.View().ConfigFile; got != config.ValueNotSet {
		t.Errorf("ConfigFile = %q, want %q", got, config.ValueNotSet)
	}
}

// TestView_ConfigFileStateFollowsAReload guards the plumbing rather than the
// redaction: the state is captured with the generation it describes, so a host
// that gains a config file and reloads reports the file, and one whose file is
// removed stops claiming to have one. Reading it from the loader on demand
// would also race the next reload.
func TestView_ConfigFileStateFollowsAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if got := mgr.View().ConfigFile; got != config.ValueNotSet {
		t.Fatalf("ConfigFile before the file exists = %q, want %q", got, config.ValueNotSet)
	}

	if err := os.WriteFile(path, []byte("alerts:\n  temp_crit_c: 88\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}
	if got := mgr.View().ConfigFile; got != config.ValueConfigured {
		t.Errorf("ConfigFile after the file appears = %q, want %q", got, config.ValueConfigured)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() after removing the file: %v", err)
	}
	if got := mgr.View().ConfigFile; got != config.ValueNotSet {
		t.Errorf("ConfigFile after the file is removed = %q, want %q", got, config.ValueNotSet)
	}
}

// TestView_DoesNotDisclosePeerURLs is the other half of #16. The peer list is
// the fleet's topology: scheme, host and port of every other machine. The name
// is kept because it is how everything downstream addresses a node and is what
// the dialog is for; the URL is not, and nothing in the browser needs it -- a
// peer is reached through /api/fleet/{name}/... on this node.
func TestView_DoesNotDisclosePeerURLs(t *testing.T) {
	path := writeConfig(t, `
fleet:
  peers:
    - name: fs
      url: http://198.51.100.10:8081
    - name: gpu-host
      url: https://198.51.100.91:9443
`)
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	view := mgr.View()
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	// Asserted over the whole serialised payload, because a redaction that only
	// covers the entry a test happens to read is not a redaction.
	for _, secret := range []string{"198.51.100.10", "198.51.100.91", "8081", "9443", "http://", "https://"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("/api/config payload still contains %q: %s", secret, body)
		}
	}

	var names []string
	for _, e := range view.Entries {
		if name, ok := strings.CutPrefix(e.Key, "fleet.peers."); ok {
			names = append(names, name)
			if e.Value != config.ValueConfigured {
				t.Errorf("peer %q value = %q, want %q", name, e.Value, config.ValueConfigured)
			}
		}
	}
	if !slices.Equal(names, []string{"fs", "gpu-host"}) {
		t.Errorf("peer names = %v, want both peers still listed by name", names)
	}
}

// TestView_PeersAreKeyedByNameWithoutTheirURL covers the peer half of the
// settings view. Every peer is keyed by its name and its URL is withheld.
//
// An unnamed peer used to take its whole URL as its name (#16), and later its
// URL's host (#37), and so leaked the address through the entry's KEY while
// its value read "(configured)". Every peer is now named by the operator, so
// no part of either address may appear.
func TestView_PeersAreKeyedByNameWithoutTheirURL(t *testing.T) {
	path := writeConfig(t, `
fleet:
  peers:
    - name: fs
      url: http://198.51.100.10:8081
    - name: omega
      url: https://198.51.100.90:9443
`)
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	view := mgr.View()
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"198.51.100.10", "198.51.100.90", "8081", "9443", "http://", "https://"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("/api/config payload still contains %q: %s", secret, body)
		}
	}

	var keys []string
	for _, e := range view.Entries {
		if name, ok := strings.CutPrefix(e.Key, "fleet.peers."); ok {
			keys = append(keys, name)
			if e.Value != config.ValueConfigured {
				t.Errorf("peer %q value = %q, want %q", name, e.Value, config.ValueConfigured)
			}
		}
	}
	if !slices.Equal(keys, []string{"fs", "omega"}) {
		t.Errorf("peer keys = %v, want each peer keyed by its name", keys)
	}
}

// TestNormalize_RepairsUnusableNumbers checks that a partial config file cannot
// disable a subsystem by accident: a zero or negative cadence falls back to the
// default rather than becoming a tight loop or a stalled one.
//
// This test used to also assert that a peer with no URL was silently DROPPED.
// That was the fail-open behaviour, not a feature: `- name: db-prod` with the
// url line missing started the service with a quietly smaller fleet. The peer
// cases now live in TestLoad_PeerWithNoURLIsRejected and its siblings, which
// assert the opposite. A repaired NUMBER and a discarded PEER are not the same
// kind of leniency -- one substitutes a sane default for a meaningless value,
// the other throws away something the operator asked to monitor.
func TestNormalize_RepairsUnusableNumbers(t *testing.T) {
	path := writeConfig(t, "server:\n  interval_ms: 0\nfleet:\n  poll_interval_ms: -5\n")
	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Server.IntervalMs <= 0 {
		t.Error("interval_ms of 0 should fall back to the default")
	}
	if cfg.Fleet.PollIntervalMs <= 0 {
		t.Error("negative poll_interval_ms should fall back to the default")
	}
}

// TestManagerReload_AppliesIntervalToRuntime is the point of the applier
// mechanism: server.interval_ms is consumed once when the monitor is built, so
// without an applier a reload would change what the settings view reports while
// the collection loop kept its original cadence.
func TestManagerReload_AppliesIntervalToRuntime(t *testing.T) {
	path := writeConfig(t, "server:\n  interval_ms: 1000\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	applied := 0
	mgr.OnReload(func(c *config.Config) error {
		applied = c.Server.IntervalMs
		return nil
	})

	if err := os.WriteFile(path, []byte("server:\n  interval_ms: 250\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}

	if applied != 250 {
		t.Errorf("applier saw interval_ms = %d, want 250", applied)
	}
	if got := mgr.Current().Server.IntervalMs; got != 250 {
		t.Errorf("Current() interval_ms = %d, want 250", got)
	}
	// Guard the pairing: an interval listed as boot-only would be pinned back
	// to its boot value and the applier would push a stale number forever.
	if slices.Contains(mgr.BootOnly(), "server.interval_ms") {
		t.Error("server.interval_ms is reloadable and must not be listed as boot-only")
	}
}

// TestManagerReload_BootOnlySettingsKeepBootValues guards the invariant that
// makes the settings view trustworthy: a value that is not in force must never
// be reported as if it were. These settings are read once, when the process
// starts and the listener binds, so the file can change them but the running
// service cannot.
func TestManagerReload_BootOnlySettingsKeepBootValues(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\n  watch: false\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	changed := "server:\n  addr: \":9090\"\n  watch: true\n"
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	deferred, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload(): %v", err)
	}

	for _, key := range []string{"server.addr", "server.watch"} {
		if !slices.Contains(deferred, key) {
			t.Errorf("deferred = %v, want it to name %s", deferred, key)
		}
	}

	cfg := mgr.Current()
	if cfg.Server.Addr != ":8081" {
		t.Errorf("Current() addr = %q, want the bound %q", cfg.Server.Addr, ":8081")
	}
	if cfg.Server.Watch {
		t.Error("Current() watch = true, want the boot value false")
	}
}

// TestView_RedactsTLSPaths keeps the settings endpoint from handing out the
// location of the private key. The service carries no authentication of its own
// -- it sits behind an authenticating proxy -- so anything it publishes should
// be assumed readable by whoever reaches it.
func TestView_RedactsTLSPaths(t *testing.T) {
	const certPath = "/etc/ssl/private/sysmon.crt"
	const keyPath = "/etc/ssl/private/sysmon.key"
	path := writeConfig(t, "server:\n  tls_cert: "+certPath+"\n  tls_key: "+keyPath+"\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	var sawCert, sawKey bool
	for _, e := range mgr.View().Entries {
		if strings.Contains(e.Value, certPath) || strings.Contains(e.Value, keyPath) {
			t.Errorf("entry %s discloses a TLS path: %q", e.Key, e.Value)
		}
		switch e.Key {
		case "server.tls_cert":
			sawCert = true
			if e.Value != config.ValueConfigured {
				t.Errorf("tls_cert value = %q, want %q", e.Value, config.ValueConfigured)
			}
		case "server.tls_key":
			sawKey = true
			if e.Value != config.ValueConfigured {
				t.Errorf("tls_key value = %q, want %q", e.Value, config.ValueConfigured)
			}
		}
	}
	if !sawCert || !sawKey {
		t.Fatalf("view is missing TLS entries (cert=%v key=%v)", sawCert, sawKey)
	}
}

// TestView_ReportsUnsetTLSAsNotSet checks the other half of redaction: an
// operator still has to be able to tell whether TLS is configured at all,
// otherwise hiding the path would hide the setting.
func TestView_ReportsUnsetTLSAsNotSet(t *testing.T) {
	mgr, err := config.NewManager(l(writeConfig(t, "alerts:\n  cpu_pct: 80\n")))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	for _, e := range mgr.View().Entries {
		if e.Key == "server.tls_cert" && e.Value != config.ValueNotSet {
			t.Errorf("tls_cert with no path = %q, want %q", e.Value, config.ValueNotSet)
		}
	}
}

// TestManagerReload_ApplierErrorDoesNotDiscardTheReload documents the contract
// the reload callers depend on: a component rejecting one setting is a partial
// application, not a failed reload. The configuration is already live for
// everything that accepted it, and the deferred list is still valid, so a
// caller must not report that the previous settings were kept.
func TestManagerReload_ApplierErrorDoesNotDiscardTheReload(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\nalerts:\n  cpu_pct: 90\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	mgr.OnReload(func(*config.Config) error {
		return &types.InvalidIntervalError{Message: "rejected by the runtime component"}
	})

	changed := "server:\n  addr: \":9090\"\nalerts:\n  cpu_pct: 70\n"
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	deferred, err := mgr.Reload()

	var applyErr *config.ErrApply
	if !errors.As(err, &applyErr) {
		t.Fatalf("Reload() error = %v, want a *config.ErrApply", err)
	}
	if len(applyErr.Reasons) != 1 {
		t.Errorf("ErrApply reasons = %v, want the one rejection", applyErr.Reasons)
	}
	if !slices.Contains(deferred, "server.addr") {
		t.Errorf("deferred = %v, want it to still name server.addr alongside the apply error", deferred)
	}
	if got := mgr.Current().Alerts.CPUPct; got != 70 {
		t.Errorf("after an apply error cpu_pct = %v, want the new 70 -- the reload is live", got)
	}
}

// unreadablePath returns a config path that cannot be examined at all: its
// parent is a regular file, so os.Stat fails with ENOTDIR. That is chosen over
// an unreadable directory because it fails the same way for every user --
// including root, which is what the test suite and the daemon often run as.
func unreadablePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	notADir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(notADir, []byte("this is a regular file"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(notADir, "sysmon.yaml")
}

// TestLoad_UnreadablePathIsAnError is the fail-open guard. Only a missing file
// means "this host runs on defaults"; a path that cannot be examined means the
// operator's settings exist and cannot be seen. Booting on defaults there would
// silently drop their TLS, listener, fleet and alert configuration while
// looking like a perfectly healthy start.
func TestLoad_UnreadablePathIsAnError(t *testing.T) {
	path := unreadablePath(t)

	cfg, _, err := l(path).Load()
	if err == nil {
		t.Fatalf("Load() on an unreadable path = nil error, want one; got config %+v", cfg)
	}
	var unreadable *config.ErrConfigUnreadable
	if !errors.As(err, &unreadable) {
		t.Fatalf("Load() error = %v (%T), want a *config.ErrConfigUnreadable", err, err)
	}
	if unreadable.Path != path {
		t.Errorf("error path = %q, want %q", unreadable.Path, path)
	}
	if cfg != nil {
		t.Errorf("Load() returned a config alongside the error: %+v", cfg)
	}
}

// TestLoad_AbsentFileIsNotAnUnreadableOne pins the other side of that
// distinction. Tightening the stat check must not turn the supported
// no-config-file deployment into a startup failure.
func TestLoad_AbsentFileIsNotAnUnreadableOne(t *testing.T) {
	cfg, _, err := l(filepath.Join(t.TempDir(), "absent.yaml")).Load()
	if err != nil {
		t.Fatalf("Load() with a genuinely missing file = %v, want defaults and no error", err)
	}
	if cfg.Server.Addr != config.Defaults().Server.Addr {
		t.Errorf("addr = %q, want the default %q", cfg.Server.Addr, config.Defaults().Server.Addr)
	}
}

// TestManagerReload_KeepsConfigWhenFileBecomesUnreadable extends the
// malformed-file guarantee to the I/O case: whatever goes wrong with the file,
// a reload must never downgrade a running daemon to defaults.
func TestManagerReload_KeepsConfigWhenFileBecomesUnreadable(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "conf.d")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "sysmon.yaml")
	if err := os.WriteFile(path, []byte("alerts:\n  disk_full_pct: 75\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	// Replace the containing directory with a regular file, so the same path
	// now fails to stat rather than parsing badly.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("no longer a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.Reload(); err == nil {
		t.Fatal("Reload() with an unreadable file = nil, want an error")
	}
	if got := mgr.Current().Alerts.DiskFullPct; got != 75 {
		t.Errorf("after a failed reload disk_full_pct = %v, want the previous 75", got)
	}
}

// TestLoad_DuplicatePeerNameIsAnError guards the fleet's identity model. A
// peer's name keys the poller's result map, titles its card in the fleet view
// and selects its settings, so two peers sharing one name produce a single
// displayed node whose snapshot came from whichever host answered last and
// whose configuration came from the other. Rejecting beats de-duplicating:
// dropping one silently would leave a real host unmonitored while the
// dashboard looked complete.
func TestLoad_DuplicatePeerNameIsAnError(t *testing.T) {
	path := writeConfig(t, `
fleet:
  peers:
    - name: fs
      url: http://10.0.0.1:8081
    - name: fs
      url: http://10.0.0.2:8081
`)
	cfg, _, err := l(path).Load()
	if err == nil {
		t.Fatalf("Load() with duplicate peer names = nil error, want one; got %+v", cfg)
	}
	var dup *config.ErrDuplicatePeer
	if !errors.As(err, &dup) {
		t.Fatalf("Load() error = %v (%T), want a *config.ErrDuplicatePeer", err, err)
	}
	if dup.Name != "fs" {
		t.Errorf("ErrDuplicatePeer.Name = %q, want %q", dup.Name, "fs")
	}
	// The message has to name both addresses or the operator cannot tell which
	// two lines of their file collide.
	for _, want := range []string{"http://10.0.0.1:8081", "http://10.0.0.2:8081"} {
		if !strings.Contains(dup.Error(), want) {
			t.Errorf("error %q does not name the conflicting URL %s", dup.Error(), want)
		}
	}
}

// TestLoad_UnnamedPeerIsRejectedByPosition is the owner ruling of 2026-09-23
// under #120: a peer with no name is a startup error. Defaulting it to its
// URL's host made the host its routing key on the unauthenticated fleet view,
// which is the exposure #120 removed.
//
// The error names the peer by its place in fleet.peers and tells the operator
// what to add. It quotes nothing from the entry -- not the URL, the host, the
// port, a path or a credential -- whatever else is wrong with it, because the
// missing name is reported before anything else about the peer is read.
func TestLoad_UnnamedPeerIsRejectedByPosition(t *testing.T) {
	tests := []struct {
		name     string
		peers    string
		position int
	}{
		{"the only peer", "    - url: http://198.51.100.10:8081\n", 1},
		{"after a named one", "    - name: fs\n      url: http://10.0.0.1:8081\n    - url: https://db-01.example.com:9443/\n", 2},
		{"an IPv6 literal", "    - name: fs\n      url: http://10.0.0.1:8081\n    - name: gpu\n      url: http://10.0.0.2:8081\n    - url: http://[fd00::10]:8081/secret-path\n", 3},
		{"with credentials", "    - url: https://u:another-secret@10.0.0.2:8081\n", 1},
		{"with no url either", "    - {}\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "fleet:\n  peers:\n"+tt.peers)
			_, _, err := l(path).Load()
			var unnamed *config.PeerUnnamedError
			if !errors.As(err, &unnamed) {
				t.Fatalf("Load() = %v, want a *config.PeerUnnamedError", err)
			}
			if unnamed.Position != tt.position {
				t.Errorf("Position = %d, want %d", unnamed.Position, tt.position)
			}
			msg := err.Error()
			if !strings.Contains(msg, "add name:") || !strings.Contains(msg, "fleet.peers") {
				t.Errorf("error %q does not tell the operator what to add, or where", msg)
			}
			for _, leak := range []string{"http", "192.168", "10.0.0", "db-01", "fd00", "8081", "9443", "secret"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error %q quotes %q from the peer entry", msg, leak)
				}
			}
		})
	}
}

// TestLoad_ReloadKeepsRunningConfigOnUnnamedPeer checks the rule holds on a
// reload: a file edited to add an unnamed peer is refused, and the fleet keeps
// running on the peers it had.
func TestLoad_ReloadKeepsRunningConfigOnUnnamedPeer(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: db-01\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	edited := "fleet:\n  peers:\n    - name: db-01\n      url: http://10.0.0.1:8081\n    - url: http://10.0.0.2:8081\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	var unnamed *config.PeerUnnamedError
	if _, err := mgr.Reload(); !errors.As(err, &unnamed) {
		t.Fatalf("Reload() = %v, want a *config.PeerUnnamedError", err)
	}
	peers := mgr.Current().Fleet.Peers
	if len(peers) != 1 || peers[0].Name != "db-01" {
		t.Errorf("running peers = %+v after a refused reload, want only db-01", peers)
	}
}

// TestLoad_RejectsPeerNameThatCannotBeAnInstance is the owner ruling on #37: a
// peer name that breaks the node-name rule is a startup error naming the peer
// and the rule, not a history request that 400s later or a chart left empty.
func TestLoad_RejectsPeerNameThatCannotBeAnInstance(t *testing.T) {
	for _, name := range []string{
		"http://db-01:8081", // a URL typed into name:, which was the old default
		"db 01",
		"-db01",
		"db01/",
		`"db01"`,
	} {
		path := writeConfig(t, "fleet:\n  peers:\n    - name: '"+name+"'\n      url: http://10.0.0.1:8081\n")
		_, _, err := l(path).Load()
		var bad *config.ErrPeerName
		if !errors.As(err, &bad) {
			t.Errorf("Load() with peer name %q = %v, want a *config.ErrPeerName", name, err)
			continue
		}
		if bad.Name != name {
			t.Errorf("ErrPeerName = %+v, want Name %q", bad, name)
		}
		var invalid *nodename.ErrInvalid
		if !errors.As(err, &invalid) {
			t.Errorf("ErrPeerName does not unwrap to *nodename.ErrInvalid: %v", err)
		}
		if !strings.Contains(err.Error(), nodename.Rule) {
			t.Errorf("error %q does not state the rule it broke", err)
		}
	}
}

// TestLoad_ReloadKeepsRunningConfigOnBadPeerName checks the rule holds on a
// reload as well as at startup: a file edited to carry an unusable name is
// refused with the running peers left in place.
func TestLoad_ReloadKeepsRunningConfigOnBadPeerName(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: db-01\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := os.WriteFile(path, []byte("fleet:\n  peers:\n    - name: 'db 01'\n      url: http://10.0.0.1:8081\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var bad *config.ErrPeerName
	if _, err := mgr.Reload(); !errors.As(err, &bad) {
		t.Fatalf("Reload() = %v, want a *config.ErrPeerName", err)
	}
	if got := mgr.Current().Fleet.Peers[0].Name; got != "db-01" {
		t.Errorf("running peer name = %q after a refused reload, want db-01", got)
	}
}

// TestLoad_DistinctPeerNamesStillLoad is the regression guard for the check
// itself: the ordinary multi-peer fleet must be unaffected.
func TestLoad_DistinctPeerNamesStillLoad(t *testing.T) {
	path := writeConfig(t, `
fleet:
  peers:
    - name: fs
      url: http://10.0.0.1:8081
    - name: gpu
      url: http://10.0.0.2:8081
    - name: edge
      url: http://10.0.0.3:8081
`)
	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load() with three distinct peers: %v", err)
	}
	if len(cfg.Fleet.Peers) != 3 {
		t.Errorf("peers = %+v, want all three", cfg.Fleet.Peers)
	}
}

// TestManagerReload_KeepsPeersWhenDuplicatesAppear extends the reload
// guarantee to this class of error: a bad edit to the peer list must not take
// the running fleet down with it.
func TestManagerReload_KeepsPeersWhenDuplicatesAppear(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	broken := "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n    - name: fs\n      url: http://10.0.0.9:8081\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.Reload(); err == nil {
		t.Fatal("Reload() onto duplicate peer names = nil, want an error")
	}
	peers := mgr.Current().Fleet.Peers
	if len(peers) != 1 || peers[0].URL != "http://10.0.0.1:8081" {
		t.Errorf("after a failed reload peers = %+v, want the previous single fs peer", peers)
	}
}

// TestManagerReload_DeferredUsesBootOnlyVocabulary keeps the two lists from
// drifting apart. Whatever Reload reports as deferred has to be a key
// BootOnly() also names, and a key the settings view renders, or a caller
// cannot join them -- a combined "a/b" entry matches nothing in either.
func TestManagerReload_DeferredUsesBootOnlyVocabulary(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\n  tls_cert: /a/old.crt\n  tls_key: /a/old.key\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	changed := "server:\n  addr: \":9090\"\n  tls_cert: /b/new.crt\n  tls_key: /b/new.key\n  watch: true\n"
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	deferred, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload(): %v", err)
	}

	for _, key := range []string{"server.tls_cert", "server.tls_key"} {
		if !slices.Contains(deferred, key) {
			t.Errorf("deferred = %v, want it to name %s as its own entry", deferred, key)
		}
	}

	boot := mgr.BootOnly()
	viewKeys := make(map[string]bool)
	for _, e := range mgr.View().Entries {
		viewKeys[e.Key] = true
	}
	for _, key := range deferred {
		if !slices.Contains(boot, key) {
			t.Errorf("deferred entry %q is not a BootOnly() key: the two lists disagree", key)
		}
		if !viewKeys[key] {
			t.Errorf("deferred entry %q is not a key the settings view renders", key)
		}
	}
}

// TestWatch_ApplierErrorStillReportsTheReload covers the file-watcher reload
// path, which had the same defect as the other two: an applier rejecting a
// setting must not be mistaken for a failed read and skip the callback, or an
// operator editing the file sees "keeping previous settings" for a change that
// is already live.
func TestWatch_ApplierErrorStillReportsTheReload(t *testing.T) {
	path := writeConfig(t, "alerts:\n  cpu_pct: 90\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	mgr.OnReload(func(*config.Config) error {
		return &types.InvalidIntervalError{Message: "rejected by the runtime component"}
	})

	done := make(chan struct{})
	defer close(done)

	reloaded := make(chan []string, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := mgr.Watch(done, logger, func(deferred []string) {
		select {
		case reloaded <- deferred:
		default:
		}
	}); err != nil {
		t.Fatalf("Watch(): %v", err)
	}

	if err := os.WriteFile(path, []byte("alerts:\n  cpu_pct: 70\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait on the callback rather than sleeping for the debounce: the timeout
	// is a generous upper bound, not the expected duration.
	select {
	case <-reloaded:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher never reported the reload; an apply error swallowed it")
	}

	if live := mgr.Current().Alerts.CPUPct; live != 70 {
		t.Errorf("cpu_pct = %v, want the new 70 to be live", live)
	}
}

// viewEntry returns the settings-view entry for key, so a test can assert on
// the value and the source an operator actually sees together.
func viewEntry(t *testing.T, mgr *config.Manager, key string) types.ConfigEntry {
	t.Helper()
	for _, e := range mgr.View().Entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("view has no entry for %q", key)
	return types.ConfigEntry{}
}

// TestManagerReload_BootOnlyKeepsBootSourceWithBootValue is the pair invariant:
// the settings view must report the value in force AND the layer that supplied
// THAT value. Pinning only the value leaves ":8080, from the config file" next
// to a file that says :9090 -- a self-contradicting pair, and worse than either
// half alone because the settings view is exactly where an operator goes to
// resolve that confusion.
func TestManagerReload_BootOnlyKeepsBootSourceWithBootValue(t *testing.T) {
	// Boot with no addr in the file at all, so the boot value AND its source
	// are both the built-in default.
	path := writeConfig(t, "alerts:\n  cpu_pct: 90\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	bootAddr := mgr.Current().Server.Addr
	if got := viewEntry(t, mgr, "server.addr").Source; got != config.SourceDefault {
		t.Fatalf("addr source at boot = %q, want %q", got, config.SourceDefault)
	}

	// Now the file sets it: the value changes AND the source would change.
	if err := os.WriteFile(path, []byte("server:\n  addr: \":9090\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deferred, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload(): %v", err)
	}

	if !slices.Contains(deferred, "server.addr") {
		t.Errorf("deferred = %v, want it to name server.addr", deferred)
	}
	if got := mgr.Current().Server.Addr; got != bootAddr {
		t.Errorf("Current() addr = %q, want the bound %q", got, bootAddr)
	}
	entry := viewEntry(t, mgr, "server.addr")
	if entry.Value != bootAddr {
		t.Errorf("view value = %q, want the bound %q", entry.Value, bootAddr)
	}
	if entry.Source != config.SourceDefault {
		t.Errorf("view source = %q, want %q -- the file's value was not applied, so the file did not supply it",
			entry.Source, config.SourceDefault)
	}
}

// TestManagerReload_ReloadableKeyUpdatesValueAndSource is the other half: the
// fix must not freeze provenance for settings that genuinely did change, or the
// view would go stale in the opposite direction.
func TestManagerReload_ReloadableKeyUpdatesValueAndSource(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if got := viewEntry(t, mgr, "alerts.disk_full_pct").Source; got != config.SourceDefault {
		t.Fatalf("disk_full_pct source at boot = %q, want %q", got, config.SourceDefault)
	}

	if err := os.WriteFile(path, []byte("alerts:\n  disk_full_pct: 55\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}

	entry := viewEntry(t, mgr, "alerts.disk_full_pct")
	if entry.Value != "55" {
		t.Errorf("view value = %q, want 55", entry.Value)
	}
	if entry.Source != config.SourceFile {
		t.Errorf("view source = %q, want %q", entry.Source, config.SourceFile)
	}
}

// TestLoad_RepairedValueIsAttributedToTheDefaults covers the same invariant on
// the load path. normalize replaces an unusable value with the built-in
// default, so the file did not supply the value in force and must not be shown
// as its source -- otherwise the view says "1000, from the config file" about a
// file that says 0.
func TestLoad_RepairedValueIsAttributedToTheDefaults(t *testing.T) {
	path := writeConfig(t, "server:\n  interval_ms: 0\nfleet:\n  timeout_ms: -5\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	for _, key := range []string{"server.interval_ms", "fleet.timeout_ms"} {
		entry := viewEntry(t, mgr, key)
		if entry.Source != config.SourceDefault {
			t.Errorf("%s source = %q, want %q -- the configured value was rejected",
				key, entry.Source, config.SourceDefault)
		}
	}
	if got := mgr.Current().Server.IntervalMs; got != config.Defaults().Server.IntervalMs {
		t.Errorf("interval_ms = %d, want the default", got)
	}
}

// TestView_NeverPairsValuesFromDifferentGenerations hammers the settings view
// while reloads run underneath it. The config alternates between two states in
// which the value and its source change TOGETHER, so exactly two pairs are
// legitimate; any other combination means a reader observed a value from one
// generation beside the provenance of another. Run under -race this also
// covers unsynchronised access to the published map.
func TestView_NeverPairsValuesFromDifferentGenerations(t *testing.T) {
	// Present in the file: value 55, source "config file".
	// Absent from the file: the default, source "default".
	const fromFile = "alerts:\n  disk_full_pct: 55\n"
	const fromDefault = "alerts:\n  cpu_pct: 88\n"
	defaultValue := num(config.Defaults().Alerts.DiskFullPct)

	path := writeConfig(t, fromFile)
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// One writer: it rewrites the file and reloads, so the file is never read
	// mid-write. Everything racing here races through the Manager, which is the
	// thing under test.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 300; i++ {
			body := fromFile
			if i%2 == 1 {
				body = fromDefault
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Errorf("writing config: %v", err)
				return
			}
			if _, err := mgr.Reload(); err != nil {
				t.Errorf("Reload(): %v", err)
				return
			}
		}
	}()

	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, e := range mgr.View().Entries {
					if e.Key != "alerts.disk_full_pct" {
						continue
					}
					fileGen := e.Value == "55" && e.Source == config.SourceFile
					defaultGen := e.Value == defaultValue && e.Source == config.SourceDefault
					if !fileGen && !defaultGen {
						t.Errorf("torn read: value %q with source %q -- the pair is from two different generations",
							e.Value, e.Source)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
}

// num mirrors the package's threshold formatting so the test compares against
// the same rendering the view produces.
func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// TestLoad_PeerCredentialsAreRejected is a secrets test as much as a config
// one. A peer URL is handed straight to net/http, so an operator can write
// credentials into it -- and the settings view copies peer URLs verbatim into
// an endpoint that has no authentication of its own. Rejecting at load means
// the secret is never stored, never rendered and never logged; redaction would
// leave it live in memory.
func TestLoad_PeerCredentialsAreRejected(t *testing.T) {
	const user = "svc-monitor"
	const token = "s3cr3t-token-value"
	path := writeConfig(t, fmt.Sprintf(`
fleet:
  peers:
    - name: fs
      url: https://%s:%s@10.0.0.1:8081
`, user, token))

	cfg, _, err := l(path).Load()
	if err == nil {
		t.Fatalf("Load() with credentials in a peer url = nil error, want one; got %+v", cfg)
	}
	var creds *config.ErrPeerCredentials
	if !errors.As(err, &creds) {
		t.Fatalf("Load() error = %v (%T), want a *config.ErrPeerCredentials", err, err)
	}
	if creds.Name != "fs" {
		t.Errorf("ErrPeerCredentials.Name = %q, want %q", creds.Name, "fs")
	}

	// The message reaches logs, which is precisely where a credential must not
	// end up. It may name the peer and its host; it may not echo either half of
	// the userinfo.
	msg := creds.Error()
	if strings.Contains(msg, token) {
		t.Errorf("error message leaks the token: %q", msg)
	}
	if strings.Contains(msg, user) {
		t.Errorf("error message leaks the username: %q", msg)
	}
	if !strings.Contains(msg, "10.0.0.1:8081") {
		t.Errorf("error message should name the host so the operator can find the peer: %q", msg)
	}
}

// TestLoad_PeerBareUsernameIsRejected covers the half that looks harmless.
// Userinfo is userinfo: a bare username is still an attempt at authentication
// this service does not support, and accepting it would mean the check passes
// whatever a password-less form of the same mistake looks like.
func TestLoad_PeerBareUsernameIsRejected(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: fs\n      url: https://onlyuser@10.0.0.1:8081\n")
	var creds *config.ErrPeerCredentials
	if _, _, err := l(path).Load(); !errors.As(err, &creds) {
		t.Fatalf("Load() with a bare username = %v, want a *config.ErrPeerCredentials", err)
	}
}

// TestLoad_OrdinaryPeerURLsStillLoad is the no-regression guard: the check must
// not make a normal fleet unconfigurable.
func TestLoad_OrdinaryPeerURLsStillLoad(t *testing.T) {
	path := writeConfig(t, `
fleet:
  peers:
    - name: fs
      url: http://10.0.0.1:8081
    - name: gpu
      url: https://gpu.internal:8443
`)
	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load() with ordinary peer urls: %v", err)
	}
	if len(cfg.Fleet.Peers) != 2 {
		t.Errorf("peers = %+v, want both", cfg.Fleet.Peers)
	}
}

// TestLoad_UnparseablePeerURLIsAnError stops a broken URL from becoming a
// mystery connection failure on every poll cycle instead of one clear error at
// the point the mistake was made.
func TestLoad_UnparseablePeerURLIsAnError(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: fs\n      url: \"://nohost\"\n")

	var bad *config.ErrPeerURL
	if _, _, err := l(path).Load(); !errors.As(err, &bad) {
		t.Fatalf("Load() with an unparseable url = %v, want a *config.ErrPeerURL", err)
	}
	if bad.Reason == "" {
		t.Error("ErrPeerURL.Reason is empty; the operator needs to know what is wrong")
	}
	// The url.Error wrapper would have embedded the raw URL, which can itself
	// hold a credential. Only the parser's explanation is reported.
	if strings.Contains(bad.Error(), "://nohost") {
		t.Errorf("error echoes the raw url: %q", bad.Error())
	}
}

// TestManagerReload_KeepsPeersWhenCredentialsAppear extends the reload-safety
// guarantee to this class: a bad edit must not take the running fleet down.
func TestManagerReload_KeepsPeersWhenCredentialsAppear(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	bad := "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n    - name: gpu\n      url: https://u:tok@10.0.0.2:8081\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.Reload(); err == nil {
		t.Fatal("Reload() onto a credentialed peer url = nil, want an error")
	}
	peers := mgr.Current().Fleet.Peers
	if len(peers) != 1 || peers[0].Name != "fs" {
		t.Errorf("after a failed reload peers = %+v, want the previous single fs peer", peers)
	}
}

// TestLoad_PeerURLMustBePollable covers the gap between "parses" and "can be
// fetched". Each rejected shape below is accepted by url.Parse and is useless
// to the poller, which would leave the node permanently unreachable while the
// service reported a clean start -- the failure an operator is least likely to
// notice, because nothing looks broken at boot.
func TestLoad_PeerURLMustBePollable(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "no scheme, host:port looks like one", url: "localhost:8081", wantErr: true},
		{name: "relative path", url: "/relative/path", wantErr: true},
		{name: "scheme we do not speak", url: "ftp://host", wantErr: true},
		{name: "empty host", url: "http:///missing-host", wantErr: true},
		{name: "plain http", url: "http://host:8081"},
		{name: "https", url: "https://host:8081"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "fleet:\n  peers:\n    - name: fs\n      url: \""+tc.url+"\"\n")
			cfg, _, err := l(path).Load()

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Load(%q) = %v, want it to load", tc.url, err)
				}
				if len(cfg.Fleet.Peers) != 1 {
					t.Errorf("peers = %+v, want the one peer retained", cfg.Fleet.Peers)
				}
				return
			}

			var bad *config.ErrPeerURL
			if !errors.As(err, &bad) {
				t.Fatalf("Load(%q) = %v (%T), want a *config.ErrPeerURL", tc.url, err, err)
			}
			if bad.Reason == "" {
				t.Error("ErrPeerURL.Reason is empty; the operator needs to know what is wrong")
			}
		})
	}
}

// TestValidateLocalName_PeerNamedLikeThisNodeIsRejected covers the
// explicit case. The fleet handlers resolve the local name before consulting the
// peer cache, so such a peer answers with THIS machine's snapshot and
// configuration: the configured host is never polled, and the dashboard shows a
// card that looks like it.
func TestValidateLocalName_PeerNamedLikeThisNodeIsRejected(t *testing.T) {
	path := writeConfig(t, `
fleet:
  local_name: aggregator
  peers:
    - name: aggregator
      url: http://10.0.0.1:8081
`)
	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	var clash *config.ErrPeerIsLocalName
	if err := cfg.ValidateLocalName(cfg.LocalName()); !errors.As(err, &clash) {
		t.Fatalf("ValidateLocalName() = %v, want a *config.ErrPeerIsLocalName", err)
	} else if clash.Name != "aggregator" {
		t.Errorf("ErrPeerIsLocalName.Name = %q, want %q", clash.Name, "aggregator")
	}
}

// TestLoad_DoesNotDependOnTheHostname is the regression guard for a real CI
// break: the collision check once lived in normalize, which reached for
// os.Hostname, so a fixture naming a peer "omega" loaded fine everywhere except
// on the build agent that happened to be called omega. Loading configuration
// must be a pure function of the inputs -- the same file has to parse the same
// way on every machine. The collision is a property of where the config is
// RUNNING, so it is checked separately, against a name the caller supplies.
func TestLoad_DoesNotDependOnTheHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname(): %v", err)
	}
	path := writeConfig(t, "fleet:\n  peers:\n    - name: "+host+"\n      url: http://10.0.0.1:8081\n")

	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load() with a peer named after this host = %v, want it to parse; "+
			"parsing must not consult the machine it runs on", err)
	}
	if len(cfg.Fleet.Peers) != 1 || cfg.Fleet.Peers[0].Name != host {
		t.Fatalf("peers = %+v, want the one peer named %q", cfg.Fleet.Peers, host)
	}

	// The collision is still caught -- just at the layer that knows the name.
	var clash *config.ErrPeerIsLocalName
	if err := cfg.ValidateLocalName(host); !errors.As(err, &clash) {
		t.Errorf("ValidateLocalName(%q) = %v, want a *config.ErrPeerIsLocalName", host, err)
	}
}

// TestValidateLocalName_UnknownLocalNameChecksNothing guards the
// degenerate input: with no name resolved there is nothing to collide with, and
// treating "" as a name would reject every unnamed peer.
func TestValidateLocalName_UnknownLocalNameChecksNothing(t *testing.T) {
	path := writeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if err := cfg.ValidateLocalName(""); err != nil {
		t.Errorf("ValidateLocalName(\"\") = %v, want no error", err)
	}
}

// TestLoad_RejectsLocalNameThatCannotBeAnInstance is the owner ruling of
// 2026-09-22 on #37: fleet.local_name is this node's fleet-wide identity and
// the instance label its history is written under, so it is held to the same
// rule as a peer name. A configured value depends only on the file, so it is
// checked while parsing and fails the same way on every machine.
func TestLoad_RejectsLocalNameThatCannotBeAnInstance(t *testing.T) {
	for _, name := range []string{"db 01", "http://db-01:8081", "-db01", "db01/"} {
		path := writeConfig(t, "fleet:\n  local_name: '"+name+"'\n")
		_, _, err := l(path).Load()
		var bad *config.ErrLocalName
		if !errors.As(err, &bad) {
			t.Errorf("Load() with local_name %q = %v, want a *config.ErrLocalName", name, err)
			continue
		}
		if bad.Name != name || bad.FromHostname {
			t.Errorf("ErrLocalName = %+v, want Name %q from the file", bad, name)
		}
		var invalid *nodename.ErrInvalid
		if !errors.As(err, &invalid) {
			t.Errorf("ErrLocalName does not unwrap to *nodename.ErrInvalid: %v", err)
		}
		if msg := err.Error(); !strings.Contains(msg, nodename.Rule) || !strings.Contains(msg, "fleet.local_name") {
			t.Errorf("error %q does not name the setting and the rule", msg)
		}
	}
}

// TestLoad_ReloadKeepsRunningConfigOnBadLocalName: a file edited to carry an
// unusable local_name is refused on reload, with the running name kept.
func TestLoad_ReloadKeepsRunningConfigOnBadLocalName(t *testing.T) {
	path := writeConfig(t, "fleet:\n  local_name: aggregator\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := os.WriteFile(path, []byte("fleet:\n  local_name: 'agg regator'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var bad *config.ErrLocalName
	if _, err := mgr.Reload(); !errors.As(err, &bad) {
		t.Fatalf("Reload() = %v, want a *config.ErrLocalName", err)
	}
	if got := mgr.Current().LocalName(); got != "aggregator" {
		t.Errorf("running local name = %q after a refused reload, want aggregator", got)
	}
}

// TestValidateLocalName_RejectsUnusableHostname covers the fallback. With
// fleet.local_name unset the node is named after its hostname, and a hostname
// that breaks the rule would be written as an instance label nothing can query.
// It depends on the machine, so it is caught by ValidateLocalName rather than
// the parse, and the message tells the operator to set fleet.local_name.
func TestValidateLocalName_RejectsUnusableHostname(t *testing.T) {
	cfg := &config.Config{}
	err := cfg.ValidateLocalName("my host")
	var bad *config.ErrLocalName
	if !errors.As(err, &bad) {
		t.Fatalf("ValidateLocalName(\"my host\") = %v, want a *config.ErrLocalName", err)
	}
	if !bad.FromHostname || bad.Name != "my host" {
		t.Errorf("ErrLocalName = %+v, want the hostname %q", bad, "my host")
	}
	if msg := err.Error(); !strings.Contains(msg, "set fleet.local_name") || !strings.Contains(msg, nodename.Rule) {
		t.Errorf("error %q does not say to set fleet.local_name and state the rule", msg)
	}
}

// TestValidateLocalName_AcceptsUsableNames is the no-regression guard for the
// fallback: an ordinary hostname, and the constant used when there is none,
// must pass.
func TestValidateLocalName_AcceptsUsableNames(t *testing.T) {
	cfg := &config.Config{}
	for _, name := range []string{"vm-host-1", "host.example.com", "local"} {
		if err := cfg.ValidateLocalName(name); err != nil {
			t.Errorf("ValidateLocalName(%q) = %v, want nil", name, err)
		}
	}
}

// TestLoad_PeerNamedLikeAnotherNodeStillLoads is the no-regression guard: only
// THIS node's name collides. An ordinary peer must not be rejected because the
// check is too eager.
func TestLoad_PeerNamedLikeAnotherNodeStillLoads(t *testing.T) {
	path := writeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Fleet.Peers) != 1 {
		t.Errorf("peers = %+v, want the one peer", cfg.Fleet.Peers)
	}
	if cfg.LocalName() != "aggregator" {
		t.Errorf("LocalName() = %q, want the configured %q", cfg.LocalName(), "aggregator")
	}
}

// TestManagerReload_KeepsConfigWhenPeerTakesTheLocalName proves a registered
// validator is consulted on the reload path, and -- the part that matters --
// that rejecting means the previous good configuration stays live rather than
// the invalid one taking effect. A validator runs BEFORE the new config is
// published, which is why this is not an applier: appliers run afterwards, so
// the bad config would already be current by the time one complained.
func TestManagerReload_KeepsConfigWhenPeerTakesTheLocalName(t *testing.T) {
	path := writeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	mgr.OnValidate(func(c *config.Config) error {
		return c.ValidateLocalName(c.LocalName())
	})

	var applied int
	mgr.OnReload(func(*config.Config) error { applied++; return nil })

	bad := "fleet:\n  local_name: aggregator\n  peers:\n    - name: aggregator\n      url: http://10.0.0.2:8081\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	var clash *config.ErrPeerIsLocalName
	if _, err := mgr.Reload(); !errors.As(err, &clash) {
		t.Fatalf("Reload() = %v, want a *config.ErrPeerIsLocalName", err)
	}
	peers := mgr.Current().Fleet.Peers
	if len(peers) != 1 || peers[0].Name != "fs" {
		t.Errorf("after a rejected reload peers = %+v, want the previous fs peer", peers)
	}
	if applied != 0 {
		t.Errorf("appliers ran %d times for a rejected reload, want 0: "+
			"a config that fails validation must never reach the running components", applied)
	}
}

// TestManagerReload_ValidatesAgainstTheIncomingLocalName guards the trap in
// wiring the check as a validator: fleet.local_name is reloadable, so during a
// reload the manager still holds the PREVIOUS generation. A validator that asks
// the manager for the local name would check the incoming peers against the
// outgoing name -- accepting a rename onto an existing peer, and rejecting a
// peer that merely inherits the name being vacated. It must use the config it
// is handed.
func TestManagerReload_ValidatesAgainstTheIncomingLocalName(t *testing.T) {
	path := writeConfig(t, "fleet:\n  local_name: aggregator\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	mgr.OnValidate(func(c *config.Config) error {
		return c.ValidateLocalName(c.LocalName())
	})

	// Renaming this node onto the name of an existing peer is a collision that
	// only the INCOMING name reveals; the outgoing name "aggregator" clashes
	// with nothing.
	renameOntoPeer := "fleet:\n  local_name: fs\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n"
	if err := os.WriteFile(path, []byte(renameOntoPeer), 0o644); err != nil {
		t.Fatal(err)
	}
	var clash *config.ErrPeerIsLocalName
	if _, err := mgr.Reload(); !errors.As(err, &clash) {
		t.Fatalf("renaming this node onto peer \"fs\": Reload() = %v, want a *config.ErrPeerIsLocalName", err)
	}

	// The mirror image: a node renaming ITSELF away is free to hand its old
	// name to a real peer. Checking against the outgoing name would refuse this
	// perfectly valid config.
	vacateName := "fleet:\n  local_name: node-a\n  peers:\n    - name: aggregator\n      url: http://10.0.0.2:8081\n"
	if err := os.WriteFile(path, []byte(vacateName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("handing the vacated name %q to a real peer: Reload() = %v, want it accepted", "aggregator", err)
	}
	if got := mgr.Current().LocalName(); got != "node-a" {
		t.Errorf("LocalName() = %q, want the reloaded %q", got, "node-a")
	}
}

// l is a tiny helper so the tests read as one line per case.
func l(path string) *config.Loader { return config.NewLoader(path) }

// TestManagerReload_ConcurrentReloadsStayConsistent forces the interleaving the
// review described: without serialisation, one reload can publish its config
// while another applies a different one, leaving Current() describing settings
// the running components were never given.
//
// The invariant asserted here is the one that matters: whenever an applier
// runs, the configuration it is handed is the one Current() reports.
func TestManagerReload_ConcurrentReloadsStayConsistent(t *testing.T) {
	path := writeConfig(t, "server:\n  interval_ms: 1000\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	var mu sync.Mutex
	var mismatches int

	// A deliberately slow applier widens the window an unserialised reload
	// would interleave in.
	mgr.OnReload(func(c *config.Config) error {
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if live := mgr.Current(); live.Server.IntervalMs != c.Server.IntervalMs {
			mismatches++
			t.Errorf("applier got interval_ms=%d while Current() reports %d",
				c.Server.IntervalMs, live.Server.IntervalMs)
		}
		return nil
	})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Each goroutine rewrites the file and reloads, so the loads race
			// as well as the applies.
			body := fmt.Sprintf("server:\n  interval_ms: %d\n", 1000+n*100)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return
			}
			if _, err := mgr.Reload(); err != nil {
				t.Errorf("Reload(): %v", err)
			}
		}(i)
	}
	wg.Wait()

	if mismatches > 0 {
		t.Errorf("%d applier calls saw a configuration other than the published one", mismatches)
	}

	// Whatever won the race, the published config and the last file read must
	// agree on a value the loader could actually have produced.
	if got := mgr.Current().Server.IntervalMs; got < 1000 || got > 1700 {
		t.Errorf("final interval_ms = %d, outside the range any reload wrote", got)
	}
}

// TestLoad_PeerWithNoURLIsRejected is the fail-open guard. `- name: db-prod`
// with the url line missing or misindented is an ordinary YAML typo, and
// skipping it started the service cleanly with db-prod absent: the operator
// asked for a fleet of N and silently got N-1, with every remaining card
// healthy. A monitoring tool that quietly monitors less than it was told to is
// worse than one that refuses to start, because nothing about the dashboard
// looks wrong.
func TestLoad_PeerWithNoURLIsRejected(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: db-prod\n    - name: fs\n      url: http://10.0.0.1:8081\n")

	var bad *config.ErrPeerURL
	if _, _, err := l(path).Load(); !errors.As(err, &bad) {
		t.Fatalf("Load() with a peer missing its url = %v, want a *config.ErrPeerURL", err)
	} else if bad.Name != "db-prod" {
		t.Errorf("ErrPeerURL.Name = %q, want the peer named %q so the operator can find it", bad.Name, "db-prod")
	}
}

// TestManagerReload_KeepsConfigWhenPeerLosesItsURL proves the rejection holds on
// the reload path too: an edit that drops a url must leave the running fleet
// intact rather than shrinking it under a running service.
func TestManagerReload_KeepsConfigWhenPeerLosesItsURL(t *testing.T) {
	path := writeConfig(t, "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n    - name: db\n      url: http://10.0.0.2:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	bad := "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n    - name: db\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	var perr *config.ErrPeerURL
	if _, err := mgr.Reload(); !errors.As(err, &perr) {
		t.Fatalf("Reload() = %v, want a *config.ErrPeerURL", err)
	}
	if n := len(mgr.Current().Fleet.Peers); n != 2 {
		t.Errorf("after a rejected reload the fleet has %d peers, want the previous 2", n)
	}
}

// TestLoad_UnknownKeysAreRejected is the typo guard. Viper's ordinary Unmarshal
// accepts keys it cannot map, which makes a one-character mistake silently
// change what the daemon monitors: `peer:` for `peers:` leaves the fleet empty,
// and a misspelled threshold keeps the default while the file plainly says
// otherwise. In both cases the service starts clean and reports health for a
// configuration nobody wrote.
func TestLoad_UnknownKeysAreRejected(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "singular peer instead of peers",
			yaml: "fleet:\n  peer:\n    - name: fs\n      url: http://10.0.0.1:8081\n",
		},
		{
			name: "misspelled alert threshold",
			yaml: "alerts:\n  disk_full_pcnt: 91\n",
		},
		{
			name: "unknown top-level section",
			yaml: "fleets:\n  poll_interval_ms: 3000\n",
		},
		{
			name: "unknown key inside a peer entry",
			yaml: "fleet:\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n      timeout: 5\n",
		},
		{
			name: "unknown key in the server section",
			yaml: "server:\n  addr: \":8081\"\n  listen: \":9090\"\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			if _, _, err := l(path).Load(); err == nil {
				t.Fatal("Load() accepted an unmappable key; want it rejected so the typo is visible")
			}
		})
	}
}

// TestLoad_ValidConfigStillLoadsUnderStrictDecoding is the no-regression guard
// for the change above: strictness must reject typos without rejecting a
// complete, correct file.
func TestLoad_ValidConfigStillLoadsUnderStrictDecoding(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: \":8081\"\n  interval_ms: 2000\n  watch: true\nalerts:\n  disk_full_pct: 91\n  cpu_pct: 95\nfleet:\n  local_name: netmon\n  poll_interval_ms: 3000\n  timeout_ms: 3000\n  peers:\n    - name: fs\n      url: http://10.0.0.1:8081\n")

	cfg, _, err := l(path).Load()
	if err != nil {
		t.Fatalf("Load() of a complete valid config = %v, want it accepted", err)
	}
	if cfg.Server.Addr != ":8081" || cfg.Fleet.LocalName != "netmon" || len(cfg.Fleet.Peers) != 1 {
		t.Errorf("config did not round-trip: addr=%q local=%q peers=%d",
			cfg.Server.Addr, cfg.Fleet.LocalName, len(cfg.Fleet.Peers))
	}
	if cfg.Alerts.DiskFullPct != 91 {
		t.Errorf("alerts.disk_full_pct = %v, want 91", cfg.Alerts.DiskFullPct)
	}
}

// TestLoad_RetiredSwapKeyNamesItsReplacement covers the upgrade path for a key
// this build removed. Strict decoding cannot tell it from a typo -- both arrive
// as viper's "invalid keys" -- so the operator who wrote a key we later retired
// would get a message saying the key is wrong and not what to write instead.
func TestLoad_RetiredSwapKeyNamesItsReplacement(t *testing.T) {
	path := writeConfig(t, "alerts:\n  swap_pct: 50\n")

	_, _, err := config.NewLoader(path).Load()
	if err == nil {
		t.Fatal("a retired key loaded cleanly; the file would state a threshold the daemon does not apply")
	}
	var retired *config.ErrRetiredConfigKey
	if !errors.As(err, &retired) {
		t.Fatalf("err = %#v, want *config.ErrRetiredConfigKey", err)
	}
	if retired.Key != "alerts.swap_pct" {
		t.Errorf("Key = %q, want alerts.swap_pct", retired.Key)
	}
	// The whole point is that the message is actionable.
	if !strings.Contains(retired.Guidance, "swap_out_pages_per_sec") {
		t.Errorf("guidance does not name the replacement: %q", retired.Guidance)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %q", err.Error())
	}
}

// TestLoad_ShippedExampleLoads is the test that would have caught this: the
// example is a file operators copy, and it carried the retired key, so the
// documented upgrade path started a daemon that refused to boot.
func TestLoad_ShippedExampleLoads(t *testing.T) {
	// Copied rather than loaded in place, because Loader records the path it
	// was given and nothing should depend on the repository layout at runtime.
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "sysmon.yaml.example"))
	if err != nil {
		t.Fatalf("reading the shipped example: %v", err)
	}
	cfg, _, err := config.NewLoader(writeConfig(t, string(body))).Load()
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	if cfg.Alerts.SwapOutPagesPerSec <= 0 {
		t.Errorf("SwapOutPagesPerSec = %v, want the example's rate", cfg.Alerts.SwapOutPagesPerSec)
	}
}
