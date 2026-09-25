package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRepo(t *testing.T, dockerfile string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.27.1\n",
		".node-version": "24.21.0\n",
		"Dockerfile":    dockerfile,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// pinArgs is the command line make check-pins passes, for the repo at root.
func pinArgs(root string) []string {
	return []string{"-root", root, "-node-file", ".node-version", "-debian", "trixie", "-ubuntu", "26.04", "-hosted-runner", "26.04"}
}

const currentDockerfile = "FROM node:24.21.0-trixie-slim\nFROM golang:1.27.1-trixie\nFROM debian:trixie-slim\nFROM ubuntu:26.04\n"

func TestRun_CurrentPinsPass(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(pinArgs(writeRepo(t, currentDockerfile)), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
	}
	if want := "pins: all 4 version references agree (go 1.27.1, node 24.21.0, debian trixie, ubuntu 26.04, hosted runners 26.04)\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_StalePinFails(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(pinArgs(writeRepo(t, "FROM golang:1.27.1-bookworm\n")), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `Dockerfile:1: "golang:1.27.1-bookworm": the image is built on bookworm, DEBIAN_RELEASE is trixie`) {
		t.Errorf("stderr = %q, want the stale base named", stderr.String())
	}
}

func TestRun_MissingPinFilesFail(t *testing.T) {
	for name, remove := range map[string]string{"go.mod": "go.mod", "node": ".node-version"} {
		t.Run(name, func(t *testing.T) {
			root := writeRepo(t, currentDockerfile)
			if err := os.Remove(filepath.Join(root, remove)); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run(pinArgs(root), &stdout, &stderr); code != 1 {
				t.Fatalf("run() = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), remove+": cannot read") {
				t.Errorf("stderr = %q, want the missing %s named", stderr.String(), remove)
			}
		})
	}
}

func TestRun_MalformedCommandLineExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"-no-such-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage of checkpins") {
		t.Errorf("stderr = %q, want the usage printed", stderr.String())
	}
}
