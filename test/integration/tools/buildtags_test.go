//go:build integration

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// shapesArg matches the -shapes argument `make check-build-tags` passes.
var shapesArg = regexp.MustCompile(`-shapes "([^"]*)"`)

// makeShapes returns the -shapes argument `make check-build-tags` would run
// buildtags with, read from make itself so the test follows the Makefile's
// shapes rather than repeating them.
func makeShapes(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("make", "-s", "-n", "check-build-tags")
	cmd.Dir = harness.RepoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("make -n check-build-tags: %v", err)
	}
	m := shapesArg.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("make -n check-build-tags passes no -shapes:\n%s", out)
	}
	return m[1]
}

// TestBuildtags_ThisRepositoryUsesOnlyShippedTags runs the check as `make
// check-build-tags` does: every constraint in the module names a tag a build
// shape sets, or one the toolchain sets itself.
func TestBuildtags_ThisRepositoryUsesOnlyShippedTags(t *testing.T) {
	r := harness.Run(t, harness.Bins.Buildtags, nil, "-root", harness.RepoRoot, "-shapes", makeShapes(t))

	wantExit(t, r, 0)
	wantContains(t, "stdout", r.Stdout, "constraint tag uses are set by a build shape")
}

// taggedModule is a module with constraints on a shipped tag, a platform, a
// release tag, and in a //+build line, plus trees the go command ignores.
func taggedModule(t *testing.T, extra map[string]string) string {
	t.Helper()
	files := map[string]string{
		"go.mod":                 "module example.com/m\n\ngo 1.27.1\n",
		"a.go":                   "package a\n",
		"prom.go":                "//go:build linux && prometheus && go1.21\n\npackage a\n",
		"old.go":                 "// +build amd64 cgo\n\npackage a\n",
		"testdata/ignored.go":    "//go:build neverset\n\npackage ignored\n",
		".hidden/ignored.go":     "//go:build neverset\n\npackage ignored\n",
		"_underscore/ignored.go": "//go:build neverset\n\npackage ignored\n",
		"notgo.txt":              "//go:build neverset\n",
		"nested/go.mod":          "module example.com/nested\n",
		"nested/x.go":            "//go:build neverset\n\npackage nested\n",
	}
	for name, body := range extra {
		files[name] = body
	}
	return writeTree(t, files)
}

func TestBuildtags_EveryTagSetPasses(t *testing.T) {
	r := harness.Run(t, harness.Bins.Buildtags, nil, "-root", taggedModule(t, nil), "-shapes", "nvml prometheus,nvml")

	wantExit(t, r, 0)
	// linux, prometheus, go1.21 and amd64, cgo; the ignored trees add none.
	if want := "build tags: all 5 constraint tag uses are set by a build shape\n"; r.Stdout != want {
		t.Errorf("stdout = %q, want %q", r.Stdout, want)
	}
}

// TestBuildtags_ATagNoShapeSetsIsNamed checks a file behind a tag no shape
// sets -- compiled by no gate -- fails the check with its place named.
func TestBuildtags_ATagNoShapeSetsIsNamed(t *testing.T) {
	root := taggedModule(t, map[string]string{"sub/experimental.go": "//go:build linux && (experimental || prometheus)\n\npackage sub\n"})

	r := harness.Run(t, harness.Bins.Buildtags, nil, "-root", root, "-shapes", "nvml")

	wantExit(t, r, 1)
	wantContains(t, "stderr", r.Stderr, "use(s) of a build tag that no shipped build shape sets")
	wantContains(t, "stderr", r.Stderr, "prom.go:1: prometheus")
	wantContains(t, "stderr", r.Stderr, filepath.Join("sub", "experimental.go")+":1: experimental")
}

// TestBuildtags_WhatCannotBeReadFails covers a constraint that does not parse,
// a Go file that does not, and a root that is not there.
func TestBuildtags_WhatCannotBeReadFails(t *testing.T) {
	tests := map[string]struct {
		root func(t *testing.T) string
		want string
	}{
		"a malformed constraint": {
			root: func(t *testing.T) string {
				return taggedModule(t, map[string]string{"bad.go": "//go:build linux &&\n\npackage a\n"})
			},
			want: "bad.go",
		},
		"a file with no package clause": {
			root: func(t *testing.T) string {
				return taggedModule(t, map[string]string{"broken.go": "//go:build linux\n\nfunc main() {}\n"})
			},
			want: "broken.go",
		},
		"a missing root": {
			root: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			want: "absent",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Buildtags, nil, "-root", tt.root(t), "-shapes", "prometheus")
			wantExit(t, r, 1)
			wantContains(t, "stderr", r.Stderr, tt.want)
		})
	}
}

// fakeGo puts a `go` on PATH that answers `go tool dist list` with out and
// exits with code, standing in for a broken toolchain.
func fakeGo(t *testing.T, out string, code int) []string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(out, "'", `'\''`) + "'\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir}
}

// TestBuildtags_ThePlatformListMustBeReadable covers the toolchain's platform
// list: absent, failing, or printing something that is not one.
func TestBuildtags_ThePlatformListMustBeReadable(t *testing.T) {
	tests := map[string]struct {
		env  []string
		want string
	}{
		"no go on PATH": {
			env:  []string{"PATH=" + t.TempDir()},
			want: "buildtags: go tool dist list:",
		},
		"go fails": {
			env:  fakeGo(t, "", 2),
			want: "buildtags: go tool dist list: exit status 2",
		},
		"not a platform list": {
			env:  fakeGo(t, "go: unknown command dist\n", 0),
			want: `not a GOOS/GOARCH pair: "go: unknown command dist"`,
		},
		"an empty list": {
			env:  fakeGo(t, "\n", 0),
			want: "go tool dist list printed no platforms",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Buildtags, tt.env, "-root", taggedModule(t, nil), "-shapes", "prometheus")
			wantExit(t, r, 1)
			wantContains(t, "stderr", r.Stderr, tt.want)
		})
	}
}

func TestBuildtags_AMalformedCommandLineExitsTwo(t *testing.T) {
	r := harness.Run(t, harness.Bins.Buildtags, nil, "-no-such-flag")
	wantExit(t, r, 2)
	wantContains(t, "stderr", r.Stderr, "Usage of buildtags")
}
