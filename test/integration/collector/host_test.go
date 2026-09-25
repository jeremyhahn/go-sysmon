//go:build integration

package collector

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestHost_NamesTheDistribution verifies the distribution is named from the
// files each family installs: Debian's own files where the lsb_release
// command would otherwise be run, and the rest the way gopsutil reads them.
func TestHost_NamesTheDistribution(t *testing.T) {
	requireDevcontainer(t)
	debian := []string{
		`printf '#!/bin/sh\nexit 1\n' >/usr/bin/lsb_release`, "chmod +x /usr/bin/lsb_release",
		"rm -f /etc/lsb-release",
		"echo 13.1 >/etc/debian_version",
	}
	tests := []struct {
		name     string
		setup    []string
		platform string
		version  string
	}{
		{"debian, from its own files", append(append([]string{}, debian...),
			`printf 'PRETTY_NAME="Debian GNU/Linux 13 (trixie)"\nID=debian\n' >/etc/os-release`), "debian", "13.1"},
		{"debian, with os-release only under /usr/lib", append(append([]string{}, debian...),
			"rm -f /etc/os-release",
			`printf 'ID="debian"\n' >/usr/lib/os-release`), "debian", "13.1"},
		{"ubuntu, from lsb-release", []string{
			`printf 'DISTRIB_ID=Ubuntu\nDISTRIB_RELEASE=24.04\nDISTRIB_CODENAME=noble\n' >/etc/lsb-release`,
			`printf 'ID=ubuntu\nVERSION_ID="24.04"\n' >/etc/os-release`,
		}, "ubuntu", "24.04"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := sandbox{root: true, setup: tt.setup}.wrap(t, harness.Bins.Sysmon)
			r := harness.Run(t, bin, nil, "host", "--json")
			if r.Code != 0 {
				t.Fatalf("sysmon host --json exited %d:\n%s", r.Code, r.Output())
			}
			var host struct {
				OS              string `json:"os"`
				Platform        string `json:"platform"`
				PlatformVersion string `json:"platform_version"`
			}
			harness.MustParseJSON(t, r.Stdout, &host)
			if host.OS != "linux" || host.Platform != tt.platform || host.PlatformVersion != tt.version {
				t.Errorf("host = %s %s %s, want linux %s %s", host.OS, host.Platform, host.PlatformVersion, tt.platform, tt.version)
			}
		})
	}
}
