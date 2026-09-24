package buildtags

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// writeTree creates files under a temporary root and returns the root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// tagsFound returns "file:line:tag" for each use, relative to root.
func tagsFound(t *testing.T, root string, uses []Use) []string {
	t.Helper()
	got := make([]string, 0, len(uses))
	for _, u := range uses {
		rel, err := filepath.Rel(root, u.File)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.ToSlash(rel)+":"+strconv.Itoa(u.Line)+":"+u.Tag)
	}
	return got
}

func TestScan_ReadsEveryTagInHeaderConstraints(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go":         "//go:build linux && (amd64 || arm64) && !nomegaraid\n\npackage a\n",
		"b_test.go":    "// Copyright notice.\n\n//go:build desktop && systray\n\npackage a\n",
		"plus.go":      "// +build legacy\n\npackage a\n",
		"none.go":      "package a\n",
		"sub/c.go":     "//go:build prometheus\n\npackage sub\n",
		"body.go":      "package a\n\n//go:build notaconstraint\n",
		"README.md":    "//go:build markdown\n",
		"sub/d.go.txt": "//go:build text\n",
	})

	uses, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	want := []string{
		"a.go:1:linux", "a.go:1:amd64", "a.go:1:arm64", "a.go:1:nomegaraid",
		"b_test.go:3:desktop", "b_test.go:3:systray",
		"plus.go:1:legacy",
		"sub/c.go:1:prometheus",
	}
	if got := tagsFound(t, root, uses); !reflect.DeepEqual(got, want) {
		t.Errorf("Scan() =\n  %v\nwant\n  %v", got, want)
	}
}

// TestScan_SkipsWhatTheGoCommandSkips verifies the walk matches ./...: a tag
// in a tree the go command never compiles is not a tag any shape must set.
func TestScan_SkipsWhatTheGoCommandSkips(t *testing.T) {
	root := writeTree(t, map[string]string{
		"pkg/testdata/x.go": "//go:build fixture\n\npackage x\n",
		"vendor/v/v.go":     "//go:build vendored\n\npackage v\n",
		".hidden/h.go":      "//go:build hidden\n\npackage h\n",
		"_scratch/s.go":     "//go:build scratch\n\npackage s\n",
		"pkg/_ignored.go":   "//go:build underscore\n\npackage pkg\n",
		"nested/go.mod":     "module nested\n",
		"nested/n.go":       "//go:build othermodule\n\npackage n\n",
		"pkg/kept.go":       "//go:build kept\n\npackage pkg\n",
	})

	uses, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if got, want := tagsFound(t, root, uses), []string{"pkg/kept.go:1:kept"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Scan() = %v, want %v", got, want)
	}
}

func TestScan_MalformedConstraintIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"bad.go": "//go:build desktop &&\n\npackage a\n",
	})

	_, err := Scan(root)
	var srcErr *SourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("Scan() error = %v, want *SourceError", err)
	}
	if !strings.HasSuffix(srcErr.Path, "bad.go") {
		t.Errorf("SourceError.Path = %q, want the malformed file", srcErr.Path)
	}
}

func TestScan_UnparsableFileIsAnError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"broken.go": "//go:build desktop\n\nthis is not go\n",
	})

	_, err := Scan(root)
	var srcErr *SourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("Scan() error = %v, want *SourceError", err)
	}
}

func TestScan_MissingRootIsAnError(t *testing.T) {
	_, err := Scan(filepath.Join(t.TempDir(), "absent"))
	var srcErr *SourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("Scan() error = %v, want *SourceError", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Scan() error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestNewKnown_ShapesPlatformsAndToolchain(t *testing.T) {
	known := NewKnown(
		[]string{"", "prometheus,nvml", " desktop , systray "},
		Platforms{OS: []string{"linux"}, Arch: []string{"amd64"}},
	)

	for _, tag := range []string{"prometheus", "nvml", "desktop", "systray", "linux", "amd64", "unix", "cgo", "gc", "gccgo", "go1.1"} {
		if !known[tag] {
			t.Errorf("NewKnown() does not know %q", tag)
		}
	}
	for _, tag := range []string{"", "ignore", "windows", "integration"} {
		if known[tag] {
			t.Errorf("NewKnown() knows %q, which nothing set", tag)
		}
	}
}

func TestCheck_NilWhenEveryTagIsKnown(t *testing.T) {
	uses := []Use{{Tag: "desktop", File: "a.go", Line: 1}, {Tag: "linux", File: "b.go", Line: 1}}
	if err := Check(uses, Known{"desktop": true, "linux": true}); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
	if err := Check(nil, Known{}); err != nil {
		t.Errorf("Check(nil) = %v, want nil", err)
	}
}

func TestCheck_ReportsEveryUnknownUse(t *testing.T) {
	uses := []Use{
		{Tag: "desktop", File: "a.go", Line: 1},
		{Tag: "bogus", File: "b.go", Line: 3},
		{Tag: "ignore", File: "c.go", Line: 1},
	}

	err := Check(uses, Known{"desktop": true})
	var tagErr *UnknownTagError
	if !errors.As(err, &tagErr) {
		t.Fatalf("Check() = %v, want *UnknownTagError", err)
	}
	if want := uses[1:]; !reflect.DeepEqual(tagErr.Uses, want) {
		t.Errorf("UnknownTagError.Uses = %v, want %v", tagErr.Uses, want)
	}
	msg := err.Error()
	for _, want := range []string{"2 use(s) of a build tag", "BUILD_SHAPES", "b.go:3: bogus", "c.go:1: ignore"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

func TestParsePlatforms(t *testing.T) {
	got, err := ParsePlatforms([]byte("linux/amd64\nlinux/arm64\n\ndarwin/arm64\n"))
	if err != nil {
		t.Fatalf("ParsePlatforms() error = %v", err)
	}
	want := Platforms{OS: []string{"darwin", "linux"}, Arch: []string{"amd64", "arm64"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePlatforms() = %+v, want %+v", got, want)
	}
}

func TestParsePlatforms_RejectsWhatIsNotAPlatformList(t *testing.T) {
	for _, out := range []string{"", "\n\n", "linux\n", "linux/\n", "/amd64\n", "linux/amd64/extra\n", "go: unknown command\n"} {
		_, err := ParsePlatforms([]byte(out))
		var listErr *PlatformListError
		if !errors.As(err, &listErr) {
			t.Errorf("ParsePlatforms(%q) error = %v, want *PlatformListError", out, err)
		}
	}
}

func TestPlatformListError_Messages(t *testing.T) {
	if msg := (&PlatformListError{}).Error(); !strings.Contains(msg, "no platforms") {
		t.Errorf("empty-output message = %q", msg)
	}
	if msg := (&PlatformListError{Line: "linux"}).Error(); !strings.Contains(msg, `"linux"`) {
		t.Errorf("bad-line message = %q, want the line quoted", msg)
	}
	cause := errors.New("exit status 2")
	err := &PlatformListError{Cause: cause}
	if msg := err.Error(); !strings.Contains(msg, "exit status 2") {
		t.Errorf("failed-command message = %q, want the cause", msg)
	}
	if !errors.Is(err, cause) {
		t.Error("PlatformListError does not unwrap to its cause")
	}
}
