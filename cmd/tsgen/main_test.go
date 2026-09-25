package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/tsgen"
)

// source is the real wire-type file, so the test generates what make
// generate does.
var source = filepath.Join("..", "..", "pkg", "types", "types.go")

func TestRun_WritesTheTypesAndTheSchema(t *testing.T) {
	var stderr bytes.Buffer
	out := t.TempDir()

	if code := run([]string{"-source", source, "-out", out}, &stderr); code != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, name := range []string{tsgen.TypesFile, tsgen.SchemaFile} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(data), "Snapshot") {
			t.Errorf("%s does not declare Snapshot", name)
		}
	}
}

func TestRun_MissingSourceExitsOne(t *testing.T) {
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "absent.go")

	if code := run([]string{"-source", missing, "-out", t.TempDir()}, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "absent.go") {
		t.Errorf("stderr = %q, want the missing source named", stderr.String())
	}
}

func TestRun_MalformedCommandLineExitsTwo(t *testing.T) {
	var stderr bytes.Buffer

	if code := run([]string{"-no-such-flag"}, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage of tsgen") {
		t.Errorf("stderr = %q, want the usage printed", stderr.String())
	}
}
