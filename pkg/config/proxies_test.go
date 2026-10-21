package config_test

import (
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestParseTrustedProxies_ParsesAndMasksEachPrefix(t *testing.T) {
	got, err := config.ParseTrustedProxies([]string{"10.1.2.3/8", " 192.0.2.10/32 ", "2001:db8::1/32"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies(): %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.0.2.10/32"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("ParseTrustedProxies() = %v, want %v", got, want)
	}
}

func TestParseTrustedProxies_EmptyTrustsNoProxy(t *testing.T) {
	got, err := config.ParseTrustedProxies(nil)
	if err != nil || got != nil {
		t.Errorf("ParseTrustedProxies(nil) = %v, %v; want nil, nil", got, err)
	}
}

// TestParseTrustedProxies_RefusesWhatIsNotAPrefix covers the entries an
// operator is likeliest to get wrong. A lone address is among them: read as a
// single host it would trust less than a network that was meant.
func TestParseTrustedProxies_RefusesWhatIsNotAPrefix(t *testing.T) {
	for _, bad := range []string{"10.0.0.1", "10.0.0.0/33", "proxy.internal/24", "", "fe80::1%eth0/64"} {
		_, err := config.ParseTrustedProxies([]string{"10.0.0.0/8", bad})
		var proxyErr *config.TrustedProxyError
		if !errors.As(err, &proxyErr) {
			t.Errorf("ParseTrustedProxies(%q) error = %v, want a TrustedProxyError", bad, err)
			continue
		}
		if proxyErr.CIDR != bad || !strings.Contains(err.Error(), "server.trusted_proxies") {
			t.Errorf("ParseTrustedProxies(%q) error = %q, want it to name the entry and the key", bad, err)
		}
		if errors.Unwrap(err) == nil {
			t.Errorf("ParseTrustedProxies(%q) error does not carry its cause", bad)
		}
	}
}

func TestLoad_TrustedProxiesFromEachSource(t *testing.T) {
	want := []string{"10.0.0.0/8", "fd00::/8"}

	t.Run("file", func(t *testing.T) {
		path := writeConfig(t, "server:\n  trusted_proxies:\n    - 10.0.0.0/8\n    - fd00::/8\n")
		cfg, sources, err := l(path).Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if !slices.Equal(cfg.Server.TrustedProxies, want) || sources["server.trusted_proxies"] != types.ConfigSourceFile {
			t.Errorf("trusted_proxies = %v from %s, want %v from the file",
				cfg.Server.TrustedProxies, sources["server.trusted_proxies"], want)
		}
	})

	t.Run("flag", func(t *testing.T) {
		loader := l(writeConfig(t, "server:\n  trusted_proxies: [192.0.2.0/24]\n"))
		loader.SetFlag("server.trusted_proxies", "10.0.0.0/8,fd00::/8")
		cfg, sources, err := loader.Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if !slices.Equal(cfg.Server.TrustedProxies, want) || sources["server.trusted_proxies"] != types.ConfigSourceFlag {
			t.Errorf("trusted_proxies = %v from %s, want %v from the flag",
				cfg.Server.TrustedProxies, sources["server.trusted_proxies"], want)
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv(config.EnvPrefix+"_SERVER_TRUSTED_PROXIES", "10.0.0.0/8,fd00::/8")
		cfg, sources, err := l(writeConfig(t, "")).Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if !slices.Equal(cfg.Server.TrustedProxies, want) || sources["server.trusted_proxies"] != types.ConfigSourceEnv {
			t.Errorf("trusted_proxies = %v from %s, want %v from the environment",
				cfg.Server.TrustedProxies, sources["server.trusted_proxies"], want)
		}
	})
}

func TestLoad_TrustedProxiesDefaultToNone(t *testing.T) {
	cfg, sources, err := l(writeConfig(t, "alerts:\n  cpu_pct: 80\n")).Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Server.TrustedProxies) != 0 || sources["server.trusted_proxies"] != types.ConfigSourceDefault {
		t.Errorf("trusted_proxies = %v from %s, want none by default",
			cfg.Server.TrustedProxies, sources["server.trusted_proxies"])
	}
}

func TestNewManager_InvalidTrustedProxyFailsStartup(t *testing.T) {
	path := writeConfig(t, "server:\n  trusted_proxies: [10.0.0.0/8, not-a-cidr]\n")
	_, err := config.NewManager(l(path))
	var proxyErr *config.TrustedProxyError
	if !errors.As(err, &proxyErr) || proxyErr.CIDR != "not-a-cidr" {
		t.Fatalf("NewManager() error = %v, want a TrustedProxyError naming not-a-cidr", err)
	}
}

// TestView_WithholdsTrustedProxies keeps the addresses of the proxies in
// front of this node -- network topology -- off the unauthenticated settings
// view, while still saying whether any are trusted.
func TestView_WithholdsTrustedProxies(t *testing.T) {
	for body, want := range map[string]string{
		"server:\n  trusted_proxies: [10.20.30.0/24]\n": config.ValueConfigured,
		"alerts:\n  cpu_pct: 80\n":                      config.ValueNotSet,
	} {
		mgr, err := config.NewManager(l(writeConfig(t, body)))
		if err != nil {
			t.Fatalf("NewManager(): %v", err)
		}
		var saw bool
		for _, e := range mgr.View().Entries {
			if strings.Contains(e.Value, "10.20.30") {
				t.Errorf("entry %s discloses a trusted proxy: %q", e.Key, e.Value)
			}
			if e.Key == "server.trusted_proxies" {
				saw = true
				if e.Value != want {
					t.Errorf("trusted_proxies value = %q, want %q", e.Value, want)
				}
			}
		}
		if !saw {
			t.Errorf("view has no server.trusted_proxies entry")
		}
	}
}

// TestManagerReload_TrustedProxiesNeedARestart: the list is handed to the
// HTTP server when it is built, so a reload that changes it is reported as
// deferred and the running value stays in Current().
func TestManagerReload_TrustedProxiesNeedARestart(t *testing.T) {
	path := writeConfig(t, "server:\n  trusted_proxies: [10.0.0.0/8]\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := os.WriteFile(path, []byte("server:\n  trusted_proxies: [192.0.2.0/24]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deferred, err := mgr.Reload()
	if err != nil {
		t.Fatalf("Reload(): %v", err)
	}
	if !slices.Contains(deferred, "server.trusted_proxies") || !slices.Contains(mgr.BootOnly(), "server.trusted_proxies") {
		t.Errorf("deferred = %v, BootOnly() = %v; want both to name server.trusted_proxies", deferred, mgr.BootOnly())
	}
	if got := mgr.Current().Server.TrustedProxies; !slices.Equal(got, []string{"10.0.0.0/8"}) {
		t.Errorf("Current() trusted_proxies = %v, want the boot value [10.0.0.0/8]", got)
	}
}

func TestManagerReload_InvalidTrustedProxyKeepsTheRunningConfig(t *testing.T) {
	path := writeConfig(t, "server:\n  trusted_proxies: [10.0.0.0/8]\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if err := os.WriteFile(path, []byte("server:\n  trusted_proxies: [10.0.0.1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var proxyErr *config.TrustedProxyError
	if _, err := mgr.Reload(); !errors.As(err, &proxyErr) {
		t.Errorf("Reload() error = %v, want a TrustedProxyError", err)
	}
}
