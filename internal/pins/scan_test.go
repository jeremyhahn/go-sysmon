package pins

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// writeTree creates files under a temporary root and returns the root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// mismatchesOf returns "file:line: found" for each mismatch in err, failing
// the test when err is not a *MismatchError.
func mismatchesOf(t *testing.T, err error) []string {
	t.Helper()
	var merr *MismatchError
	if !errors.As(err, &merr) {
		t.Fatalf("error = %v, want *MismatchError", err)
	}
	got := make([]string, 0, len(merr.Mismatches))
	for _, m := range merr.Mismatches {
		got = append(got, m.File+":"+strconv.Itoa(m.Line)+": "+m.Found)
	}
	return got
}

var current = Pins{Go: "1.27.1", Node: "24.21.0", NodeFile: ".node-version", Debian: "trixie", Ubuntu: "26.04", HostedRunner: "26.04"}

const currentWorkflow = `name: CI
env:
  GO_VERSION_FILE: go.mod
  NODE_VERSION_FILE: .node-version
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version-file: ${{ env.GO_VERSION_FILE }}
      - uses: actions/setup-node@v7
        with:
          node-version-file: ${{ env.NODE_VERSION_FILE }}
  test:
    runs-on: ${{ github.server_url == 'https://github.com' && 'ubuntu-26.04' || 'ubuntu-latest' }}
    strategy:
      matrix:
        include:
          - runner: ubuntu-26.04-arm
    env:
      GO: 1.27.1
    container: golang:1.27.1-trixie
    services:
      builder:
        image: docker.io/library/node:24.21.0-trixie-slim
      db:
        image: postgres:16
    steps:
      - uses: actions/setup-go@0aaccfd150d50ccaeb58ebd88d36e91967a5f35b # v7
        with:
          go-version: ${{ env.GO }}
      - uses: actions/setup-node@v7
        with:
          node-version: 24.21.0
      - uses: docker://ubuntu:26.04
      - run: go test ./...
`

func TestCheckAgreement_CurrentPinsPass(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Dockerfile": "FROM node:24.21.0-trixie-slim AS frontend\n" +
			"FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie AS builder\n" +
			"FROM debian:trixie-slim\n",
		"test/integration/Dockerfile": "ARG GO_VERSION=1.27.1\nFROM golang:${GO_VERSION}-trixie\n",
		"test/desktop/Dockerfile":     "FROM golang:1.27.1-trixie@sha256:0123456789abcdef AS b\nFROM ubuntu:26.04\n",
		".github/workflows/ci.yml":    currentWorkflow,
		"docker-compose.yml":          "services:\n  dev:\n    image: debian:trixie\n  app:\n    image: ghcr.io/example/golang:0.1.2\n",
	})

	n, err := CheckAgreement(root, current)
	if err != nil {
		t.Fatalf("CheckAgreement() error = %v", err)
	}
	// Dockerfiles: node, golang, debian, golang, golang, ubuntu. Workflow:
	// 4 setup steps, the container, the node service, the docker:// step.
	// Compose: debian (ghcr.io/example/golang is not the official image).
	// Runners: ubuntu-26.04 inside the runs-on expression, and the matrix's
	// ubuntu-26.04-arm; ubuntu-latest is not a versioned label.
	if n != 16 {
		t.Errorf("CheckAgreement() examined %d references, want 16", n)
	}
}

func TestCheckAgreement_StalePinsFail(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Dockerfile":                  "FROM golang:1.27.0-trixie AS builder\nFROM node:22-bookworm-slim\nFROM debian:bookworm-slim\n",
		"test/integration/Dockerfile": "FROM golang:1.27-alpine\nFROM golang\nFROM golang:latest\nFROM golang:1.27.1\n",
		"test/desktop/Dockerfile":     "FROM golang:${GO_VERSION}-trixie\nFROM ubuntu:24.04\nFROM node:24.21.0-bookworm\n",
		".github/workflows/ci.yml": `jobs:
  a:
    steps:
      - uses: actions/setup-go@v7
      - uses: actions/setup-go@v7
        with:
          go-version: '1.26.5'
      - uses: actions/setup-go@v7
        with:
          go-version-file: .tool-versions
      - uses: actions/setup-go@v7
        with:
          go-version: ${{ matrix.go }}
      - uses: actions/setup-node@v7
        with:
          node-version: ${{ env.UNSET }}
      - uses: actions/setup-node@v7
        with:
          node-version: '24'
      - uses: actions/setup-node@v7
        with:
          node-version-file: package.json
`,
		".gitea/workflows/nightly.yaml": "jobs:\n  b:\n    container:\n      image: golang:1.26.5-trixie\n" +
			"  c:\n    # ubuntu-22.04 in a comment is not a label\n    runs-on: ubuntu-24.04-arm\n",
		"compose.yaml": "services:\n  dev:\n    image: library/ubuntu:noble\n",
	})

	_, err := CheckAgreement(root, current)
	got := mismatchesOf(t, err)
	want := []string{
		".gitea/workflows/nightly.yaml:4: golang:1.26.5-trixie",
		".gitea/workflows/nightly.yaml:7: ubuntu-24.04-arm",
		".github/workflows/ci.yml:4: actions/setup-go",
		".github/workflows/ci.yml:7: 1.26.5",
		".github/workflows/ci.yml:10: .tool-versions",
		".github/workflows/ci.yml:13: ${{ matrix.go }}",
		".github/workflows/ci.yml:16: ${{ env.UNSET }}",
		".github/workflows/ci.yml:19: 24",
		".github/workflows/ci.yml:22: package.json",
		"Dockerfile:1: golang:1.27.0-trixie",
		"Dockerfile:2: node:22-bookworm-slim",
		"Dockerfile:3: debian:bookworm-slim",
		"compose.yaml:3: library/ubuntu:noble",
		"test/desktop/Dockerfile:1: golang:${GO_VERSION}-trixie",
		"test/desktop/Dockerfile:2: ubuntu:24.04",
		"test/desktop/Dockerfile:3: node:24.21.0-bookworm",
		"test/integration/Dockerfile:1: golang:1.27-alpine",
		"test/integration/Dockerfile:2: golang",
		"test/integration/Dockerfile:3: golang:latest",
		"test/integration/Dockerfile:4: golang:1.27.1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatches =\n%q\nwant\n%q", got, want)
	}
}

func TestCheckAgreement_ThePinsThemselvesMustBeExact(t *testing.T) {
	root := writeTree(t, map[string]string{"README.md": "nothing to scan\n"})
	bad := Pins{Go: "1.27", Toolchain: "go1.27.1", Node: "24", NodeFile: ".node-version", Debian: "Trixie", Ubuntu: "26.4", HostedRunner: "25.10"}

	_, err := CheckAgreement(root, bad)
	got := mismatchesOf(t, err)
	want := []string{".node-version:0: 24", "Makefile:0: Trixie", "Makefile:0: 26.4", "Makefile:0: 25.10", "go.mod:0: 1.27", "go.mod:0: go1.27.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatches = %q, want %q", got, want)
	}
}

func TestCheckAgreement_MatchingToolchainDirectivePasses(t *testing.T) {
	root := writeTree(t, map[string]string{"README.md": "FROM golang:1.0\n"})
	p := current
	p.Toolchain = "go1.27.1"

	n, err := CheckAgreement(root, p)
	if err != nil || n != 0 {
		t.Fatalf("CheckAgreement() = %d, %v; want 0, nil", n, err)
	}
}

func TestCheckAgreement_SkipsTreesThatAreNotThisBuild(t *testing.T) {
	root := writeTree(t, map[string]string{
		"node_modules/pkg/Dockerfile":           "FROM golang:1.10\n",
		"vendor/x/Dockerfile":                   "FROM golang:1.10\n",
		"internal/pins/testdata/Dockerfile":     "FROM golang:1.10\n",
		"docs/workflows/ci.yml":                 "jobs:\n  a:\n    container: golang:1.10\n",
		".github/workflows/sub/nested.yml":      "jobs:\n  a:\n    container: golang:1.10\n",
		".github/workflows/README.md":           "container: golang:1.10\n",
		".github/workflows/empty.yml":           "",
		".github/workflows/no-jobs.yml":         "name: x\n",
		".github/workflows/scalar-steps.yml":    "jobs:\n  a:\n    steps: none\n",
		".github/workflows/run-only-steps.yml":  "jobs:\n  a:\n    steps:\n      - run: echo\n",
		"deploy/other-service-compose-file.yml": "services:\n  a:\n    image: golang:1.10\n",
		"Dockerfile":                            "FROM scratch\nFROM ghcr.io/org/node:1\nFROM alpine:3.22\n",
	})

	n, err := CheckAgreement(root, current)
	if err != nil || n != 0 {
		t.Fatalf("CheckAgreement() = %d, %v; want 0, nil", n, err)
	}
}

func TestCheckAgreement_UnparsableYAMLIsAnError(t *testing.T) {
	for name, file := range map[string]string{
		"workflow": ".github/workflows/ci.yml",
		"compose":  "docker-compose.yml",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{file: "jobs: [unclosed\n"})
			_, err := CheckAgreement(root, current)
			var serr *SourceError
			if !errors.As(err, &serr) {
				t.Fatalf("CheckAgreement() error = %v, want *SourceError", err)
			}
		})
	}
}

func TestCheckAgreement_MissingRootIsAnError(t *testing.T) {
	_, err := CheckAgreement(filepath.Join(t.TempDir(), "absent"), current)
	var serr *SourceError
	if !errors.As(err, &serr) {
		t.Fatalf("CheckAgreement() error = %v, want *SourceError", err)
	}
}

func TestCheckAgreement_ErrorListsEveryMismatch(t *testing.T) {
	root := writeTree(t, map[string]string{"Dockerfile": "FROM golang:1.27.0-trixie\n"})

	_, err := CheckAgreement(root, current)
	if err == nil {
		t.Fatal("CheckAgreement() error = nil, want a mismatch")
	}
	want := "pins: 1 version reference(s) disagree with their pin. Make each one name exactly the pinned release:\n" +
		"  Dockerfile:1: \"golang:1.27.0-trixie\": the image is Go 1.27.0, go.mod says 1.27.1"
	if err.Error() != want {
		t.Errorf("Error() =\n%s\nwant\n%s", err, want)
	}
}

func TestSplitImage(t *testing.T) {
	tests := map[string][2]string{
		"golang:1.27.1-trixie":                  {"golang", "1.27.1-trixie"},
		"golang":                                {"golang", ""},
		"localhost:5000/library/golang":         {"localhost:5000/library/golang", ""},
		"localhost:5000/library/golang:1.27.1":  {"localhost:5000/library/golang", "1.27.1"},
		"golang:1.27.1@sha256:abc":              {"golang", "1.27.1"},
		"docker.io/library/node:24.21.0-trixie": {"docker.io/library/node", "24.21.0-trixie"},
	}
	for ref, want := range tests {
		name, tag := splitImage(ref)
		if name != want[0] || tag != want[1] {
			t.Errorf("splitImage(%q) = %q, %q; want %q, %q", ref, name, tag, want[0], want[1])
		}
	}
}

func TestOfficialImage(t *testing.T) {
	tests := map[string]string{
		"golang":                         "golang",
		"library/node":                   "node",
		"docker.io/library/debian":       "debian",
		"index.docker.io/library/ubuntu": "ubuntu",
		"ghcr.io/org/golang":             "",
		"localhost:5000/library/golang":  "",
		"someone/ubuntu":                 "",
	}
	for name, want := range tests {
		if got := officialImage(name); got != want {
			t.Errorf("officialImage(%q) = %q, want %q", name, got, want)
		}
	}
}
