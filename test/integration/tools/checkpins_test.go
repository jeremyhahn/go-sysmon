//go:build integration

package tools

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// makefilePin matches one of the release pins the Makefile passes checkpins.
var makefilePin = regexp.MustCompile(`^(NODE_VERSION_FILE|DEBIAN_RELEASE|UBUNTU_RELEASE|HOSTED_RUNNER_RELEASE) := (\S+)$`)

// repoPinArgs reads the pins from this repository's Makefile and returns the
// arguments `make check-pins` passes, so the test follows the pins rather than
// repeating them.
func repoPinArgs(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(harness.RepoRoot, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	harness.CloseOnCleanup(t, "the Makefile", f)
	flags := map[string]string{
		"NODE_VERSION_FILE": "-node-file", "DEBIAN_RELEASE": "-debian",
		"UBUNTU_RELEASE": "-ubuntu", "HOSTED_RUNNER_RELEASE": "-hosted-runner",
	}
	args := []string{"-root", harness.RepoRoot}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := makefilePin.FindStringSubmatch(sc.Text()); m != nil {
			args = append(args, flags[m[1]], m[2])
			delete(flags, m[1])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(flags) != 0 {
		t.Fatalf("the Makefile does not set %v", flags)
	}
	return args
}

// TestCheckpins_ThisRepositoryAgreesWithItsPins runs `make check-pins` as the
// Makefile does, over this repository.
func TestCheckpins_ThisRepositoryAgreesWithItsPins(t *testing.T) {
	r := harness.Run(t, harness.Bins.Checkpins, nil, repoPinArgs(t)...)

	wantExit(t, r, 0)
	wantContains(t, "stdout", r.Stdout, "version references agree")
}

// pinArgs are the pins every synthetic repository below is checked against.
func pinArgs(root string) []string {
	return []string{"-root", root, "-node-file", ".node-version", "-debian", "trixie", "-ubuntu", "26.04", "-hosted-runner", "26.04"}
}

// TestCheckpins_EveryKindOfReferenceAgrees checks a repository using every
// kind of reference the check knows -- images in Dockerfiles, compose files
// and workflows, resolved build arguments, setup steps reading the pin's file
// or naming it through env, and hosted runner labels -- passes and is counted.
func TestCheckpins_EveryKindOfReferenceAgrees(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.27.1\n\ntoolchain go1.27.1\n",
		".node-version": "24.21.0\n",
		"Dockerfile": "FROM node:24.21.0-trixie-slim AS frontend\n" +
			"FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie AS builder\n" +
			"FROM debian:trixie-slim\n",
		"build/Containerfile":       "ARG GO=1.27.1\nFROM docker.io/library/golang:${GO}-trixie@sha256:0123\nFROM ubuntu:26.04\n",
		"deploy/app.Dockerfile":     "FROM ghcr.io/example/golang:0.1\n",
		"docker-compose.yml":        "services:\n  dev:\n    image: debian:trixie\n  other:\n    image: ghcr.io/example/node:1\n",
		"testdata/Dockerfile":       "FROM golang:1.0\n",
		"node_modules/x/Dockerfile": "FROM golang:1.0\n",
		// Workflows shaped every way a job can be: a container given as a
		// mapping, a job with no steps or with steps that are not a list,
		// a step running a script, and a service with no image. An empty
		// workflow holds nothing.
		".gitea/workflows/odd.yaml": `jobs:
  mapped:
    container:
      image: golang:1.27.1-trixie
    services:
      cache:
        ports: [6379]
    steps:
      - run: make test
  none:
    runs-on: ubuntu-latest
  scalar:
    steps: make
`,
		".gitea/workflows/empty.yml": "",
		".github/workflows/ci.yml": `env:
  GO: 1.27.1
jobs:
  test:
    runs-on: ${{ github.server_url == 'https://github.com' && 'ubuntu-26.04' || 'ubuntu-latest' }}
    strategy:
      matrix:
        include:
          - runner: ubuntu-26.04-arm
    container: golang:1.27.1-trixie
    services:
      builder:
        image: docker.io/library/node:24.21.0-trixie-slim
    steps:
      - uses: actions/setup-go@v7
        with:
          go-version: ${{ env.GO }}
      - uses: actions/setup-go@v7
        with:
          go-version-file: ./go.mod
      - uses: actions/setup-node@v7
        with:
          node-version-file: .node-version
      - uses: docker://ubuntu:26.04
`,
	})

	r := harness.Run(t, harness.Bins.Checkpins, nil, pinArgs(root)...)

	wantExit(t, r, 0)
	// Dockerfile 3, Containerfile 2, compose 1, workflow: container, service,
	// three setup steps, docker:// and two runner labels; the mapped
	// container.
	want := "pins: all 15 version references agree (go 1.27.1, node 24.21.0, debian trixie, ubuntu 26.04, hosted runners 26.04)\n"
	if r.Stdout != want {
		t.Errorf("stdout = %q, want %q", r.Stdout, want)
	}
}

// TestCheckpins_EveryDisagreementIsNamed checks each way a reference can
// disagree with its pin is reported, one line each, and the run exits 1.
func TestCheckpins_EveryDisagreementIsNamed(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.27.1\n",
		".node-version": "24.21.0\n",
		"Dockerfile": "FROM golang:1.27.0-trixie AS builder\nFROM node:22-bookworm-slim\nFROM debian:bookworm-slim\n" +
			"FROM golang:1.27-alpine\nFROM golang\nFROM golang:latest\nFROM golang:1.27.1\nFROM golang:${UNSET}-trixie\n" +
			"FROM ubuntu:24.04\nFROM node:24.21.0-bookworm\nFROM node:24.20.0-trixie\n",
		"compose.yaml": "services:\n  dev:\n    image: library/ubuntu:noble\n",
		".github/workflows/ci.yml": `jobs:
  a:
    runs-on: ubuntu-24.04-arm
    steps:
      - uses: actions/setup-go@v7
      - uses: actions/setup-go@v7
        with:
          go-version: '1.26.5'
      - uses: actions/setup-go@v7
        with:
          go-version: '1.27'
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
          node-version-file: ${{ env.UNSET }}
      - uses: actions/setup-node@v7
        with:
          node-version: '24'
`,
	})

	r := harness.Run(t, harness.Bins.Checkpins, nil, pinArgs(root)...)

	wantExit(t, r, 1)
	for _, want := range []string{
		`Dockerfile:1: "golang:1.27.0-trixie": the image is Go 1.27.0, go.mod says 1.27.1`,
		`Dockerfile:2: "node:22-bookworm-slim": the tag does not name an exact stable Node.js release`,
		`Dockerfile:3: "debian:bookworm-slim": the image is Debian bookworm, DEBIAN_RELEASE is trixie`,
		`Dockerfile:4: "golang:1.27-alpine": the tag does not name an exact stable Go release`,
		`Dockerfile:5: "golang": an untagged or latest image floats`,
		`Dockerfile:6: "golang:latest": an untagged or latest image floats`,
		`Dockerfile:7: "golang:1.27.1": the tag names no base variant`,
		`Dockerfile:8: "golang:${UNSET}-trixie": the tag uses a build argument with no default`,
		`Dockerfile:9: "ubuntu:24.04": the image is Ubuntu 24.04, UBUNTU_RELEASE is 26.04`,
		`Dockerfile:10: "node:24.21.0-bookworm": the image is built on bookworm, DEBIAN_RELEASE is trixie`,
		`Dockerfile:11: "node:24.20.0-trixie": the image is Node.js 24.20.0, .node-version says 24.21.0`,
		`compose.yaml:3: "library/ubuntu:noble": the image is Ubuntu noble`,
		`.github/workflows/ci.yml:3: "ubuntu-24.04-arm": the hosted runner is Ubuntu 24.04, HOSTED_RUNNER_RELEASE is 26.04`,
		`.github/workflows/ci.yml:5: "actions/setup-go": no go-version-file or go-version, so the action picks the release`,
		`"1.26.5": go-version is Go 1.26.5, go.mod says 1.27.1`,
		`"1.27": go-version does not name an exact stable release`,
		`".tool-versions": go-version-file must be go.mod`,
		`"${{ matrix.go }}": go-version cannot be resolved offline`,
		`"${{ env.UNSET }}": node-version cannot be resolved offline`,
		`"${{ env.UNSET }}": node-version-file cannot be resolved offline`,
		`"24": node-version is 24, .node-version says 24.21.0`,
	} {
		wantContains(t, "stderr", r.Stderr, want)
	}
}

// TestCheckpins_ThePinsThemselvesMustBeExact checks a pin that is not one
// exact release is reported before anything is compared with it.
func TestCheckpins_ThePinsThemselvesMustBeExact(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.27\n\ntoolchain go1.26.5\n",
		".node-version": "24.21.0\n",
	})

	r := harness.Run(t, harness.Bins.Checkpins, nil,
		"-root", root, "-node-file", ".node-version", "-debian", "Trixie", "-ubuntu", "26.4", "-hosted-runner", "25.10")

	wantExit(t, r, 1)
	for _, want := range []string{
		`go.mod: "1.27": the go directive must name an exact stable Go release`,
		`go.mod: "go1.26.5": the toolchain directive names a different release`,
		`Makefile: "Trixie": DEBIAN_RELEASE must be a Debian release codename`,
		`Makefile: "26.4": UBUNTU_RELEASE must be an Ubuntu release number`,
		`Makefile: "25.10": HOSTED_RUNNER_RELEASE must be an Ubuntu LTS release number`,
	} {
		wantContains(t, "stderr", r.Stderr, want)
	}
}

// TestCheckpins_UnreadablePinsAndSourcesFail checks what cannot be read is
// reported by path, and never passes as agreement.
func TestCheckpins_UnreadablePinsAndSourcesFail(t *testing.T) {
	tests := map[string]struct {
		files map[string]string
		want  string
	}{
		"no go.mod": {
			files: map[string]string{".node-version": "24.21.0\n"},
			want:  "go.mod: cannot read",
		},
		"a go.mod that does not parse": {
			files: map[string]string{"go.mod": "module\n", ".node-version": "24.21.0\n"},
			want:  "go.mod",
		},
		"a go.mod with no go directive": {
			files: map[string]string{"go.mod": "module example.com/m\n", ".node-version": "24.21.0\n"},
			want:  "go.mod: no go directive",
		},
		"a Dockerfile with a line too long to read": {
			files: map[string]string{
				"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
				"Dockerfile": "FROM golang:1.27.1-trixie\nRUN echo " + strings.Repeat("x", 70*1024) + "\n",
			},
			want: "Dockerfile: bufio.Scanner: token too long",
		},
		"no Node.js version file": {
			files: map[string]string{"go.mod": "module example.com/m\n\ngo 1.27.1\n"},
			want:  ".node-version: cannot read",
		},
		"a workflow that is not YAML": {
			files: map[string]string{
				"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
				".gitea/workflows/nightly.yaml": "jobs: [\n",
			},
			want: ".gitea/workflows/nightly.yaml",
		},
		"a compose file that is not YAML": {
			files: map[string]string{
				"go.mod": "module example.com/m\n\ngo 1.27.1\n", ".node-version": "24.21.0\n",
				"compose.yml": "services: {\n",
			},
			want: "compose.yml",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Checkpins, nil, pinArgs(writeTree(t, tt.files))...)
			wantExit(t, r, 1)
			wantContains(t, "stderr", r.Stderr, tt.want)
			if strings.Contains(r.Stdout, "agree") {
				t.Errorf("stdout = %q, want no agreement reported", r.Stdout)
			}
		})
	}
}

// TestCheckpins_AMissingRootOrBadFlagFails checks the command's own inputs.
func TestCheckpins_AMissingRootOrBadFlagFails(t *testing.T) {
	missing := harness.Run(t, harness.Bins.Checkpins, nil, pinArgs(filepath.Join(t.TempDir(), "absent"))...)
	wantExit(t, missing, 1)
	wantContains(t, "stderr", missing.Stderr, "absent")

	bad := harness.Run(t, harness.Bins.Checkpins, nil, "-no-such-flag")
	wantExit(t, bad, 2)
	wantContains(t, "stderr", bad.Stderr, "Usage of checkpins")
}
