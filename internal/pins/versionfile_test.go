package pins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadVersionFile(t *testing.T) {
	for body, want := range map[string]string{"24.21.0\n": "24.21.0", "v24.21.0": "24.21.0", " 24.21.0 \n": "24.21.0"} {
		p := filepath.Join(t.TempDir(), ".node-version")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadVersionFile(p)
		if err != nil || got != want {
			t.Errorf("ReadVersionFile(%q) = %q, %v; want %q", body, got, err, want)
		}
	}
}

func TestReadVersionFile_Errors(t *testing.T) {
	for name, body := range map[string]string{
		"missing":      "",
		"major only":   "24\n",
		"lts alias":    "lts/*\n",
		"prerelease":   "25.0.0-rc.1\n",
		"two versions": "24.21.0\n22.23.3\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), ".node-version")
			if body != "" {
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ReadVersionFile(p)
			var perr *PinFileError
			if !errors.As(err, &perr) {
				t.Fatalf("ReadVersionFile() error = %v, want *PinFileError", err)
			}
		})
	}
}

func TestReadPins(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.27.1\n",
		".node-version": "24.21.0\n",
	})
	got, err := ReadPins(root, ".node-version", "trixie", "26.04", "26.04")
	if err != nil {
		t.Fatalf("ReadPins() error = %v", err)
	}
	if got != current {
		t.Errorf("ReadPins() = %+v, want %+v", got, current)
	}
}

func TestReadPins_MissingFilesAreErrors(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"go.mod":        {".node-version": "24.21.0\n"},
		".node-version": {"go.mod": "module example.com/m\n\ngo 1.27.1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadPins(writeTree(t, files), ".node-version", "trixie", "26.04", "26.04")
			var perr *PinFileError
			if !errors.As(err, &perr) {
				t.Fatalf("ReadPins() error = %v, want *PinFileError", err)
			}
		})
	}
}

func TestCompareRelease(t *testing.T) {
	tests := []struct {
		pinned, latest string
		want           Status
	}{
		{"24.21.0", "24.21.0", Current},
		{"24.20.1", "24.21.0", Behind},
		{"22.23.3", "24.21.0", Behind},
		{"24.9.0", "24.10.0", Behind}, // numeric, not lexical
		{"26.10.0", "24.21.0", Ahead},
	}
	for _, tt := range tests {
		if got, err := CompareRelease(tt.pinned, tt.latest); err != nil || got != tt.want {
			t.Errorf("CompareRelease(%q, %q) = %q, %v; want %q", tt.pinned, tt.latest, got, err, tt.want)
		}
	}
	for _, bad := range [][2]string{{"24", "24.21.0"}, {"24.21.0", "v24.21.0"}, {"24.21.0-rc.1", "24.21.0"}} {
		var verr *VersionError
		if _, err := CompareRelease(bad[0], bad[1]); !errors.As(err, &verr) {
			t.Errorf("CompareRelease(%q, %q) error = %v, want *VersionError", bad[0], bad[1], err)
		}
	}
}

func TestCompareUbuntu(t *testing.T) {
	tests := []struct {
		pinned, latest string
		want           Status
	}{
		{"26.04", "26.04", Current},
		{"24.04", "26.04", Behind},
		{"25.10", "26.04", Behind},
		{"26.10", "26.04", Ahead},
	}
	for _, tt := range tests {
		if got, err := CompareUbuntu(tt.pinned, tt.latest); err != nil || got != tt.want {
			t.Errorf("CompareUbuntu(%q, %q) = %q, %v; want %q", tt.pinned, tt.latest, got, err, tt.want)
		}
	}
	for _, bad := range [][2]string{{"noble", "26.04"}, {"26.04", "26.4"}, {"26.04 LTS", "26.04"}} {
		var verr *VersionError
		if _, err := CompareUbuntu(bad[0], bad[1]); !errors.As(err, &verr) {
			t.Errorf("CompareUbuntu(%q, %q) error = %v, want *VersionError", bad[0], bad[1], err)
		}
	}
}
