package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// stageEtc builds a synthetic /etc and points the platform lookup at it. The
// binary paths are staged too, because their presence is what decides whether
// gopsutil would have forked.
func stageEtc(t *testing.T, files map[string]string, lsbBinary, raspiBinary bool) {
	t.Helper()

	root := t.TempDir()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	bin := t.TempDir()
	lsb := filepath.Join(bin, "lsb_release")
	raspi := filepath.Join(bin, "raspi-config")
	for path, want := range map[string]bool{lsb: lsbBinary, raspi: raspiBinary} {
		if !want {
			continue
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// The vendor copy is redirected into the fixture too. Without it a case
	// that stages no os-release still reads the REAL /usr/lib/os-release and
	// gets this machine's distribution -- a fixture that isolates one input and
	// leaves another wired to the host, which is how "no file" first passed on
	// a Mint workstation and would have failed on Debian CI.
	oldEtc, oldUsrLib := etcRoot, usrLibOSRelease
	oldLSB, oldRaspi := lsbReleaseBinary, raspiConfigBinary
	etcRoot, usrLibOSRelease = root, filepath.Join(root, "usr-lib-os-release")
	lsbReleaseBinary, raspiConfigBinary = lsb, raspi
	t.Cleanup(func() {
		etcRoot, usrLibOSRelease = oldEtc, oldUsrLib
		lsbReleaseBinary, raspiConfigBinary = oldLSB, oldRaspi
	})
}

const debianOSRelease = "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nID=debian\nVERSION_ID=\"12\"\n"

// The case the short-circuit exists for: fs and mon0, byte for byte.
func TestDebianWithoutFork_ReproducesGopsutilOnTheAffectedHosts(t *testing.T) {
	stageEtc(t, map[string]string{
		"os-release":     debianOSRelease,
		"debian_version": "12.13\n",
	}, true, false)

	platform, version, ok := debianWithoutFork()
	if !ok {
		t.Fatal("the short-circuit did not fire on a host shaped exactly like fs")
	}
	if platform != "debian" {
		t.Errorf("platform = %q, want debian", platform)
	}
	// Verbatim from /etc/debian_version, which is what gopsutil's default
	// branch returns. fs reports 12.13 and mon0 12.15; VERSION_ID says "12",
	// so reading os-release for the version instead would change the API.
	if version != "12.13" {
		t.Errorf("version = %q, want 12.13 -- the point release, not os-release's VERSION_ID", version)
	}
}

// Every refusal is a case where gopsutil's answer would differ from ours, so
// each has to send the caller back to gopsutil rather than guess.
func TestDebianWithoutFork_RefusesEveryCaseItCannotReproduce(t *testing.T) {
	tests := []struct {
		name       string
		files      map[string]string
		lsb, raspi bool
	}{
		{
			// With the file, gopsutil reads it instead of forking, and its
			// named branches may apply -- there is nothing to avoid and its
			// answer is not ours to predict.
			name:  "lsb-release present",
			files: map[string]string{"os-release": debianOSRelease, "debian_version": "12.13\n", "lsb-release": "DISTRIB_ID=Debian\n"},
			lsb:   true,
		},
		{
			// No binary means no fork, so the expensive path never runs.
			name:  "no lsb_release binary",
			files: map[string]string{"os-release": debianOSRelease, "debian_version": "12.13\n"},
			lsb:   false,
		},
		{
			// gopsutil reports raspbian here, not debian.
			name:  "raspi-config present",
			files: map[string]string{"os-release": debianOSRelease, "debian_version": "12.13\n"},
			lsb:   true, raspi: true,
		},
		{
			// Ubuntu, Mint and the rest take a NAMED branch in gopsutil and
			// report the lsb release string, which is not derivable here.
			name:  "not debian",
			files: map[string]string{"os-release": "ID=ubuntu\nVERSION_ID=\"26.04\"\n", "debian_version": "bookworm/sid\n"},
			lsb:   true,
		},
		{
			name:  "no debian_version",
			files: map[string]string{"os-release": debianOSRelease},
			lsb:   true,
		},
		{
			name:  "empty debian_version",
			files: map[string]string{"os-release": debianOSRelease, "debian_version": "\n"},
			lsb:   true,
		},
		{
			name:  "no os-release",
			files: map[string]string{"debian_version": "12.13\n"},
			lsb:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stageEtc(t, tt.files, tt.lsb, tt.raspi)

			if _, _, ok := debianWithoutFork(); ok {
				t.Error("the short-circuit fired on a host whose answer it cannot reproduce")
			}
		})
	}
}

func TestOSReleaseID(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"unquoted", map[string]string{"os-release": "ID=debian\n"}, "debian"},
		{"quoted", map[string]string{"os-release": "ID=\"debian\"\n"}, "debian"},
		// ID_LIKE and VERSION_ID both start with text that a careless prefix
		// match would accept; only the exact key counts.
		{"not ID_LIKE", map[string]string{"os-release": "ID_LIKE=debian\nID=linuxmint\n"}, "linuxmint"},
		{"absent", map[string]string{"os-release": "NAME=\"x\"\n"}, ""},
		{"no file", map[string]string{}, ""},
		// The vendor location is consulted when /etc has none: the two
		// documented paths of one file, not a fallback for a failure.
		{"vendor copy only", map[string]string{"usr-lib-os-release": "ID=debian\n"}, "debian"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stageEtc(t, tt.files, false, false)

			if got := osReleaseID(); got != tt.want {
				t.Errorf("osReleaseID() = %q, want %q", got, tt.want)
			}
		})
	}
}
