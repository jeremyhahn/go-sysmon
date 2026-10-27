package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestBMC_AbsentIsOff(t *testing.T) {
	cfg := load(t, "server:\n  addr: \":8080\"\n")
	if cfg.BMC.RedfishURL != "" {
		t.Errorf("RedfishURL = %q, want empty", cfg.BMC.RedfishURL)
	}
	if got := cfg.BMC.Timeout(); got != 10*time.Second {
		t.Errorf("Timeout() = %v, want the 10s default", got)
	}
}

func TestBMC_LoadsFromFile(t *testing.T) {
	cfg := load(t, `
bmc:
  redfish_url: "https://10.0.0.5"
  username: "monitor"
  password_file: "/etc/go-sysmon/bmc-password"
  ca_file: "/etc/go-sysmon/bmc-ca.pem"
  timeout_ms: 2500
`)
	want := config.BMC{
		RedfishURL:   "https://10.0.0.5",
		Username:     "monitor",
		PasswordFile: "/etc/go-sysmon/bmc-password",
		CAFile:       "/etc/go-sysmon/bmc-ca.pem",
		TimeoutMs:    2500,
	}
	if cfg.BMC != want {
		t.Errorf("BMC = %+v, want %+v", cfg.BMC, want)
	}
	if got := cfg.BMC.Timeout(); got != 2500*time.Millisecond {
		t.Errorf("Timeout() = %v, want 2.5s", got)
	}
}

func TestBMC_NonsenseTimeoutIsRepaired(t *testing.T) {
	cfg, sources, err := config.NewLoader(writeConfig(t, "bmc:\n  timeout_ms: -1\n")).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.BMC.TimeoutMs != config.Defaults().BMC.TimeoutMs {
		t.Errorf("TimeoutMs = %d, want the default", cfg.BMC.TimeoutMs)
	}
	if sources["bmc.timeout_ms"] != types.ConfigSourceDefault {
		t.Errorf("bmc.timeout_ms source = %q, want default", sources["bmc.timeout_ms"])
	}
}

func TestBMC_UnusableURLsAreRejected(t *testing.T) {
	cases := map[string]string{
		"plain http":     "bmc:\n  redfish_url: \"http://10.0.0.5\"\n  username: monitor\n  password_file: /p\n",
		"no host":        "bmc:\n  redfish_url: \"https:///redfish/v1\"\n  username: monitor\n  password_file: /p\n",
		"does not parse": "bmc:\n  redfish_url: \"https://10.0.0.5:port\"\n  username: monitor\n  password_file: /p\n",
		"no username":    "bmc:\n  redfish_url: \"https://10.0.0.5\"\n  password_file: /p\n",
		"no password":    "bmc:\n  redfish_url: \"https://10.0.0.5\"\n  username: monitor\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := config.NewLoader(writeConfig(t, body)).Load()
			var urlErr *config.BMCURLError
			if !errors.As(err, &urlErr) {
				t.Fatalf("Load() = %v, want *BMCURLError", err)
			}
			if strings.Contains(err.Error(), "10.0.0.5") {
				t.Errorf("the rejection quoted the URL: %s", err)
			}
		})
	}
}

func TestBMC_CredentialsAreRejected(t *testing.T) {
	_, _, err := config.NewLoader(writeConfig(t,
		"bmc:\n  redfish_url: \"https://root:calvin@10.0.0.5\"\n  username: root\n  password_file: /p\n")).Load()
	var credErr *config.BMCCredentialsError
	if !errors.As(err, &credErr) {
		t.Fatalf("Load() = %v, want *BMCCredentialsError", err)
	}
	if strings.Contains(err.Error(), "calvin") {
		t.Fatalf("the rejection quoted the password: %s", err)
	}
	if credErr.Host != "10.0.0.5" {
		t.Errorf("Host = %q, want the host alone", credErr.Host)
	}
}

func TestBMC_ViewWithholdsTheConnectionDetails(t *testing.T) {
	mgr, err := config.NewManager(config.NewLoader(writeConfig(t, `
bmc:
  redfish_url: "https://bmc-02.internal"
  username: "monitor"
  password_file: "/etc/go-sysmon/bmc-password"
  ca_file: "/etc/go-sysmon/bmc-ca.pem"
`)))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	withheld := map[string]bool{
		"bmc.redfish_url":   true,
		"bmc.username":      true,
		"bmc.password_file": true,
		"bmc.ca_file":       true,
	}
	found := 0
	for _, e := range mgr.View().Entries {
		if withheld[e.Key] {
			found++
			if e.Value != config.ValueConfigured {
				t.Errorf("%s = %q, want %q", e.Key, e.Value, config.ValueConfigured)
			}
		}
		for _, secret := range []string{"bmc-02.internal", "monitor", "bmc-password", "bmc-ca.pem"} {
			if strings.Contains(e.Value, secret) {
				t.Errorf("entry %s leaked %q: %q", e.Key, secret, e.Value)
			}
		}
	}
	if found != len(withheld) {
		t.Fatalf("found %d withheld bmc entries in the settings view, want %d", found, len(withheld))
	}
}
