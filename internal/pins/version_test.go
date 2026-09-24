package pins

import (
	"errors"
	"testing"
)

func TestCompareGo(t *testing.T) {
	tests := []struct {
		name, pinned, latest string
		want                 Status
	}{
		{"same release, go.mod form against go.dev form", "1.27.1", "go1.27.1", Current},
		{"same release, both prefixed", "go1.27.1", "go1.27.1", Current},
		{"patch behind", "1.27.0", "go1.27.1", Behind},
		{"minor behind", "1.26.9", "go1.27.0", Behind},
		{"language version names no patch release", "1.27", "go1.27.0", Behind},
		{"newer than the reported latest", "1.27.2", "go1.27.1", Ahead},
		{"release candidate pin", "1.28rc1", "go1.27.1", Prerelease},
		{"beta pin even when older than latest", "1.27beta1", "go1.27.1", Prerelease},
		{"vendor toolchain suffix is ignored", "go1.27.1-bigcorp", "go1.27.1", Current},
		{"vendor suffix on a stale release", "go1.27.0-bigcorp", "go1.27.1", Behind},
		{"latest with surrounding whitespace", "1.27.1", " go1.27.1\n", Current},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareGo(tt.pinned, tt.latest)
			if err != nil {
				t.Fatalf("CompareGo(%q, %q) error = %v", tt.pinned, tt.latest, err)
			}
			if got != tt.want {
				t.Errorf("CompareGo(%q, %q) = %q, want %q", tt.pinned, tt.latest, got, tt.want)
			}
		})
	}
}

func TestCompareGo_RefusesWhatIsNotAnAnswer(t *testing.T) {
	tests := []struct {
		name, pinned, latest string
	}{
		{"pin is not a version", "latest", "go1.27.1"},
		{"latest is not a version", "1.27.1", "<html>"},
		{"latest is empty", "1.27.1", ""},
		// A source reporting a release candidate as "latest stable" is
		// wrong, and trusting it would pass an rc pin as current.
		{"latest is a release candidate", "1.28rc1", "go1.28rc1"},
		{"latest is a bare language version", "1.27", "go1.27"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareGo(tt.pinned, tt.latest)
			var verr *VersionError
			if !errors.As(err, &verr) {
				t.Fatalf("CompareGo(%q, %q) = %q, %v; want *VersionError", tt.pinned, tt.latest, got, err)
			}
		})
	}
}

func TestCompareModule(t *testing.T) {
	tests := []struct {
		name, pinned, latest string
		want                 Status
	}{
		{"current", "v2.14.0", "v2.14.0", Current},
		{"minor behind", "v2.13.2", "v2.14.0", Behind},
		{"patch behind", "v1.7.7", "v1.7.12", Behind},
		{"numeric not lexical order", "v1.7.12", "v1.7.9", Ahead},
		{"retracted or unpublished pin", "v2.15.0", "v2.14.0", Ahead},
		{"release candidate pin", "v2.15.0-rc.1", "v2.14.0", Prerelease},
		{"prerelease of the latest", "v2.14.0-rc.1", "v2.14.0", Prerelease},
		{"pseudo-version pin", "v0.0.0-20260101000000-abcdefabcdef", "v1.0.0", Prerelease},
		{"build metadata does not order", "v2.0.0+incompatible", "v2.0.0+incompatible", Current},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareModule(tt.pinned, tt.latest)
			if err != nil {
				t.Fatalf("CompareModule(%q, %q) error = %v", tt.pinned, tt.latest, err)
			}
			if got != tt.want {
				t.Errorf("CompareModule(%q, %q) = %q, want %q", tt.pinned, tt.latest, got, tt.want)
			}
		})
	}
}

func TestCompareModule_InvalidVersionIsAnError(t *testing.T) {
	for _, tt := range [][2]string{
		{"2.14.0", "v2.14.0"}, // no v prefix
		{"v2.14.0", "latest"},
		{"", "v2.14.0"},
	} {
		if _, err := CompareModule(tt[0], tt[1]); err == nil {
			t.Errorf("CompareModule(%q, %q) error = nil, want *VersionError", tt[0], tt[1])
		} else if verr := (*VersionError)(nil); !errors.As(err, &verr) {
			t.Errorf("CompareModule(%q, %q) error = %T, want *VersionError", tt[0], tt[1], err)
		}
	}
}

func TestIsGoRelease(t *testing.T) {
	tests := map[string]bool{
		"1.27.1":           true,
		"go1.27.1":         true,
		"go1.27.1-bigcorp": true,
		"1.27":             false,
		"1.27rc1":          false,
		"go1.28beta1":      false,
		"latest":           false,
		"":                 false,
	}
	for v, want := range tests {
		if got := IsGoRelease(v); got != want {
			t.Errorf("IsGoRelease(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSameGo(t *testing.T) {
	if !SameGo("1.27.1", "go1.27.1") {
		t.Error("SameGo(1.27.1, go1.27.1) = false, want true")
	}
	if SameGo("1.27.0", "1.27.1") {
		t.Error("SameGo(1.27.0, 1.27.1) = true, want false")
	}
	if SameGo("junk", "junk") {
		t.Error("SameGo of an invalid version = true, want false")
	}
	if SameGo("1.27.1", "junk") {
		t.Error("SameGo against an invalid version = true, want false")
	}
}
