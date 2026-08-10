package collector

import (
	"strings"
	"testing"
)

// TestCheckDesktopLibs_ReportsEveryLib verifies every declared library is
// checked and carries the metadata needed to advise a fix.
func TestCheckDesktopLibs_ReportsEveryLib(t *testing.T) {
	t.Parallel()
	got := CheckDesktopLibs()

	if len(got) != len(desktopLibs) {
		t.Fatalf("got %d results, want %d", len(got), len(desktopLibs))
	}
	for _, lib := range got {
		if lib.SOName == "" || lib.Purpose == "" {
			t.Errorf("library entry is incomplete: %+v", lib)
		}
		if lib.Debian == "" || lib.Fedora == "" || lib.Arch == "" {
			t.Errorf("%s has no package name for some distribution: %+v", lib.SOName, lib)
		}
		if lib.Found && lib.Path == "" {
			t.Errorf("%s reported found with no path", lib.SOName)
		}
	}
}

// TestMissingDesktopLibs_SubsetOfAll verifies the missing list only ever
// contains libraries that were genuinely not found.
func TestMissingDesktopLibs_SubsetOfAll(t *testing.T) {
	t.Parallel()
	all := CheckDesktopLibs()
	missing := MissingDesktopLibs()

	if len(missing) > len(all) {
		t.Fatalf("missing (%d) exceeds the total checked (%d)", len(missing), len(all))
	}
	for _, m := range missing {
		if m.Found {
			t.Errorf("%s appears in the missing list but was found", m.SOName)
		}
	}
}

// TestInstallHint_NamesPackagesForTheDistro verifies the hint is a runnable
// command naming every missing package.
func TestInstallHint_NamesPackagesForTheDistro(t *testing.T) {
	t.Parallel()
	libs := []DesktopLib{
		{SOName: "libgtk-3.so.0", Debian: "libgtk-3-0", Fedora: "gtk3", Arch: "gtk3"},
		{SOName: "libsoup-3.0.so.0", Debian: "libsoup-3.0-0", Fedora: "libsoup3", Arch: "libsoup3"},
	}

	hint := InstallHint(libs)
	if hint == "" {
		t.Fatal("InstallHint() = empty for a non-empty list")
	}
	if !strings.HasPrefix(hint, "sudo ") {
		t.Errorf("hint is not a runnable command: %q", hint)
	}

	// Whichever family was detected, both packages must appear.
	switch {
	case strings.Contains(hint, "apt"):
		for _, want := range []string{"libgtk-3-0", "libsoup-3.0-0"} {
			if !strings.Contains(hint, want) {
				t.Errorf("apt hint missing %q: %s", want, hint)
			}
		}
	case strings.Contains(hint, "dnf"):
		for _, want := range []string{"gtk3", "libsoup3"} {
			if !strings.Contains(hint, want) {
				t.Errorf("dnf hint missing %q: %s", want, hint)
			}
		}
	case strings.Contains(hint, "pacman"):
		for _, want := range []string{"gtk3", "libsoup3"} {
			if !strings.Contains(hint, want) {
				t.Errorf("pacman hint missing %q: %s", want, hint)
			}
		}
	default:
		t.Errorf("unrecognised package manager in hint: %s", hint)
	}
}

func TestInstallHint_EmptyForNothingMissing(t *testing.T) {
	t.Parallel()
	if got := InstallHint(nil); got != "" {
		t.Errorf("InstallHint(nil) = %q, want empty", got)
	}
}

// TestLibSearchDirs_IncludesStandardPaths guards against a search that would
// miss libraries in the usual multiarch locations.
func TestLibSearchDirs_IncludesStandardPaths(t *testing.T) {
	t.Parallel()
	dirs := libSearchDirs()

	for _, want := range []string{"/usr/lib/x86_64-linux-gnu", "/lib/x86_64-linux-gnu", "/usr/lib"} {
		found := false
		for _, d := range dirs {
			if d == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("search path %q missing from %v", want, dirs)
		}
	}
}

func TestDistroFamily_KnownValue(t *testing.T) {
	t.Parallel()
	switch got := distroFamily(); got {
	case "debian", "fedora", "arch":
	default:
		t.Errorf("distroFamily() = %q, want one of debian/fedora/arch", got)
	}
}
