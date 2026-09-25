package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// platforms stands in for `go tool dist list`.
func platforms() ([]byte, error) {
	return []byte("linux/amd64\nlinux/arm64\n"), nil
}

// writeModule creates a module whose one tagged file needs the prometheus tag.
func writeModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.com/m\n\ngo 1.27.1\n",
		"a.go":    "package a\n",
		"prom.go": "//go:build linux && prometheus\n\npackage a\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRun_EveryTagSetByAShapePasses(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"-root", writeModule(t), "-shapes", "nvml prometheus,nvml"}, &stdout, &stderr, platforms)

	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
	}
	if want := "build tags: all 2 constraint tag uses are set by a build shape\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_TagNoShapeSetsFails(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"-root", writeModule(t), "-shapes", "nvml"}, &stdout, &stderr, platforms)

	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "prometheus") {
		t.Errorf("stderr = %q, want the unset tag named", stderr.String())
	}
}

func TestRun_CheckFailuresExitOne(t *testing.T) {
	tests := map[string]struct {
		root  func(t *testing.T) string
		plats func() ([]byte, error)
		want  string
	}{
		"platform list unavailable": {
			root:  writeModule,
			plats: func() ([]byte, error) { return nil, errors.New("exec: go: not found") },
			want:  "exec: go: not found",
		},
		"platform list unreadable": {
			root:  writeModule,
			plats: func() ([]byte, error) { return []byte("go: unknown command\n"), nil },
			want:  "go: unknown command",
		},
		"root missing": {
			root:  func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			plats: platforms,
			want:  "absent",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{"-root", tc.root(t), "-shapes", "prometheus"}, &stdout, &stderr, tc.plats); code != 1 {
				t.Fatalf("run() = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRun_MalformedCommandLineExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"-no-such-flag"}, &stdout, &stderr, platforms); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage of buildtags") {
		t.Errorf("stderr = %q, want the usage printed", stderr.String())
	}
}

// TestDistList_ListsThisPlatform verifies the real platform source answers
// with a list that includes the platform the test runs on.
func TestDistList_ListsThisPlatform(t *testing.T) {
	out, err := distList()
	if err != nil {
		t.Fatalf("distList() error = %v", err)
	}
	if !strings.Contains(string(out), "linux/amd64") && !strings.Contains(string(out), "linux/arm64") {
		t.Errorf("distList() = %q, want a Linux platform listed", out)
	}
}
