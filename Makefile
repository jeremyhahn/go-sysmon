MODULE := github.com/jeremyhahn/go-sysmon
VERSION := $(shell cat VERSION 2>/dev/null || echo "0.1.0")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.buildDate=$(BUILD_DATE)

# Feature flags (set to 0 to disable)
WITH_SYSTRAY ?= 1

# Build tags — desktop + systray assembled dynamically
#
# SERVER_TAGS carries `prometheus`: remote write and date-range history are
# compiled into sysmon-server and NOT into the desktop or systray builds, whose
# users want a realtime monitor with no time-series dependency (owner ruling
# 2026-09-04, #100). Both shapes are built and tested here and in CI -- a tag
# that only ever compiles one way is not tested.
# nvml is on both shipped builds, so go-sysmon's own binaries report NVIDIA GPUs
# exactly as before. It is NOT on the default build: NVML has no pure-Go
# implementation, and importing it unconditionally made CGO_ENABLED=0 impossible
# for anything embedding pkg/collector (#8). See check-cgo-clean below.
# systray compiles the tray into the desktop binary (cmd/sysmon/tray_desktop.go);
# WITH_SYSTRAY=0 leaves it out, and closing the window then quits.
SERVER_TAGS := prometheus,nvml
DESKTOP_BASE_TAGS := desktop,production,webkit2_41,nvml
DESKTOP_TAGS := $(DESKTOP_BASE_TAGS)
ifeq ($(WITH_SYSTRAY),1)
  DESKTOP_TAGS := $(DESKTOP_BASE_TAGS),systray
endif
# The published container image. The Dockerfile does not build through make, so
# it names the same tags itself; this is the copy the gates below check.
IMAGE_TAGS := nvml

# Build shapes: every tag set a go-sysmon binary or an embedding consumer is
# built with. `make vet`, `make lint` and `make test` run over each one, because
# vet and test report success over the files their tags select and say nothing
# about the rest -- the desktop build's test files stopped compiling with every
# gate green (#204). check-build-tags fails when a build constraint names a tag
# that no shape here sets, so a new tag cannot arrive without a shape that
# compiles it. Each shape is also a target: `make test-shape-desktop`.
#
#   default          what an embedder of pkg/* gets; CGO_ENABLED=0 and FIPS clean
#   image            the published container image (Dockerfile)
#   server           sysmon-server, the release and `make deploy` binary
#   desktop          sysmon, the desktop release binary, with the tray
#   desktop-notray   sysmon built with WITH_SYSTRAY=0
SHAPE_default :=
SHAPE_image := $(IMAGE_TAGS)
SHAPE_server := $(SERVER_TAGS)
SHAPE_desktop := $(DESKTOP_BASE_TAGS),systray
SHAPE_desktop-notray := $(DESKTOP_BASE_TAGS)
BUILD_SHAPES := default image server desktop desktop-notray

# Check shapes are compiled, vetted and linted but ship nothing. nomegaraid
# stands in for a target without the RAID passthrough (see megaraid_unsupported.go),
# and its unit tests run here too; integration's run in their container
# (make integration-test), so here they are vetted only.
SHAPE_nomegaraid := nomegaraid
SHAPE_integration := integration
TEST_SHAPES := $(BUILD_SHAPES) nomegaraid
ALL_SHAPES := $(BUILD_SHAPES) nomegaraid integration

PKG_ALL := ./...
PKG_CLI := ./pkg/cli/...
PKG_COLLECTOR := ./pkg/collector/...
PKG_FLEET := ./pkg/fleet/...
PKG_METRICS := ./pkg/metrics/...
PKG_MONITOR := ./pkg/monitor/...
PKG_SERVER := ./pkg/server/...
PKG_TYPES := ./pkg/types/...
PKG_TSGEN := ./internal/tsgen/...
PKG_BUILDTAGS := ./internal/buildtags/...
PKG_DEADPEER := ./internal/deadpeer/...
PKG_PINS := ./internal/pins/...
PKG_GITEA := ./internal/gitea/...
PKG_CMD := ./cmd/sysmon/...

FRONTEND_DIR := cmd/sysmon/frontend
# Defined here, beside FRONTEND_DIR, and NOT next to the rule that builds it:
# prerequisites are expanded when make READS a rule, so a := defined further down
# the file expands to nothing in `build-frontend: $(FRONTEND_STAMP)` above it --
# which silently left build-frontend with no prerequisites at all and no install
# step, so a fresh checkout ran `npm run check` against an absent node_modules.
FRONTEND_STAMP := $(FRONTEND_DIR)/node_modules/.install-stamp
DOCKER_IMAGE := go-sysmon-test

# Analysis tool versions. This is the ONLY place they are pinned: `make lint`,
# `make vulncheck` and `make gosec` refuse a binary built at any other version,
# and every workflow installs through `make tools`, so a local run and CI run
# the same tools. Pins are exact module versions, which Go checksums on install.
#
# Every pin is the latest stable release (owner rule 2026-09-22). `make
# check-latest` fails when one is not, and the daily check-latest workflow files
# an issue for it (#183).
GOLANGCI_LINT_VERSION := v2.14.0
ACTIONLINT_VERSION := v1.7.12
GOSEC_VERSION := v2.29.0
GOVULNCHECK_VERSION := v1.8.0

# The module each tool is released from, which is what check-latest asks for
# the latest version of, and the package inside it that is installed.
GOLANGCI_LINT_MOD := github.com/golangci/golangci-lint/v2
ACTIONLINT_MOD := github.com/rhysd/actionlint
GOSEC_MOD := github.com/securego/gosec/v2
GOVULNCHECK_MOD := golang.org/x/vuln

GOLANGCI_LINT_PKG := $(GOLANGCI_LINT_MOD)/cmd/golangci-lint
ACTIONLINT_PKG := $(ACTIONLINT_MOD)/cmd/actionlint
GOSEC_PKG := $(GOSEC_MOD)/cmd/gosec
GOVULNCHECK_PKG := $(GOVULNCHECK_MOD)/cmd/govulncheck

# Base releases and Node.js. Go is pinned by go.mod's go directive, which the
# workflows read through setup-go's go-version-file. Node.js is pinned in
# NODE_VERSION_FILE, which setup-node reads through node-version-file. The
# Debian and Ubuntu releases are pinned here. Dockerfiles cannot read any of
# these, so each FROM repeats the release exactly, and `make check-pins`
# fails when one disagrees. Each is the latest stable release -- for Node.js
# the latest LTS, for Ubuntu the latest LTS -- and `make check-latest` fails
# when one is not.
NODE_VERSION_FILE := .node-version
DEBIAN_RELEASE := trixie
UBUNTU_RELEASE := 26.04
# HOSTED_RUNNER_RELEASE is the Ubuntu release of every versioned GitHub-hosted
# runner label the workflows use (ubuntu-26.04, ubuntu-26.04-arm): the newest
# generally available one in actions/runner-images, which lags Ubuntu's own
# releases, so it is its own pin. ubuntu-latest is not one of these: on the
# Gitea plane it is a self-hosted runner label, which is company infra.
HOSTED_RUNNER_RELEASE := 26.04
PIN_ARGS = -node-file $(NODE_VERSION_FILE) -debian $(DEBIAN_RELEASE) -ubuntu $(UBUNTU_RELEASE) -hosted-runner $(HOSTED_RUNNER_RELEASE)

# PINNED_TOOLS names every pinned tool by its variable prefix; check-latest
# checks each one. A tool pinned above and missing here is a tool nothing keeps
# current, so add it to both.
PINNED_TOOLS := GOLANGCI_LINT ACTIONLINT GOSEC GOVULNCHECK

# TOOL verifies a binary against its pin, and installs the pin when asked.
TOOL := ./scripts/tool.sh

# Analysis tools resolve to GOBIN when a copy is there, then to PATH, and
# finally to the GOBIN path so a missing tool is reported by name.
#
# GOBIN comes first because that is where `make tools` installs a pinned version
# when the one on PATH is wrong -- on the CI runner, a stale image copy in
# /usr/local/bin. When GOBIN has no copy, the PATH lookup still finds the image's
# baked tools; pointing only at GOBIN is what broke `make lint` in CI before
# (#39/#21). Locally GOBIN is normally on PATH, so both resolve to one file.
GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
  GOBIN_DIR := $(shell go env GOPATH)/bin
endif
tool = $(or $(wildcard $(GOBIN_DIR)/$(1)),$(shell command -v $(1) 2>/dev/null),$(GOBIN_DIR)/$(1))

GOLANGCI_LINT ?= $(call tool,golangci-lint)
ACTIONLINT ?= $(call tool,actionlint)
GOSEC ?= $(call tool,gosec)
GOVULNCHECK ?= $(call tool,govulncheck)

# Cinnamon applet paths
APPLET_UUID := sysmon-go@sysmon
APPLET_SRC := extensions/cinnamon/$(APPLET_UUID)
APPLET_DEST := $(HOME)/.local/share/cinnamon/applets/$(APPLET_UUID)
# The applet's pure logic lives in extensions/shared and is installed alongside
# applet.js: Cinnamon's require() resolves against the applet directory only and
# rejects a path containing a subdirectory.
EXTENSION_SHARED := extensions/shared
DESKTOP_IMAGE := go-sysmon-desktop-test

ADDR ?= :8081
INTERVAL ?= 1000

.PHONY: all deps build build-desktop build-server build-frontend dev serve audit audit-report check-bundle-current frontend-deps test test-cli test-collector test-monitor \
        test-server test-types test-metrics coverage-cli coverage-collector coverage-monitor coverage-server coverage-metrics \
        bench-cli bench-collector bench-fleet bench-monitor bench-server integration-test integration-test-cli \
        test-e2e lint lint-workflows fmt fmt-check vet clean docker-build deploy deploy-binary \
        ci ci-fast vulncheck gosec audit check-signed check-conflicts check-cgo-clean release-build release-build-desktop clean-dist \
        build-both generate check-generated check-ts check-errcheck-nolint test-tsgen coverage-tsgen test-buildtags coverage-buildtags test-deadpeer coverage-deadpeer \
        check-build-tags test-fips check-pins check-latest test-pins coverage-pins test-gitea coverage-gitea \
        tools tool-golangci-lint tool-actionlint tool-gosec tool-govulncheck \
        install-extension-cinnamon uninstall-extension-cinnamon scan-image scan-image-sarif \
        test-extensions test-extensions-desktop desktop-docker-build \

all: build test lint

## Dependencies

deps:
	@echo "Installing system dependencies for Wails v2 desktop build..."
	@if command -v apt-get >/dev/null 2>&1; then \
		sudo apt-get install -y \
			pkg-config \
			build-essential \
			libgtk-3-dev \
			libwebkit2gtk-4.1-dev \
			libsoup-3.0-dev; \
	elif command -v dnf >/dev/null 2>&1; then \
		sudo dnf install -y \
			pkgconf-pkg-config \
			gcc \
			gtk3-devel \
			webkit2gtk4.1-devel \
			libsoup3-devel; \
	elif command -v pacman >/dev/null 2>&1; then \
		sudo pacman -S --needed \
			pkgconf \
			base-devel \
			gtk3 \
			webkit2gtk-4.1 \
			libsoup3; \
	else \
		echo "Unsupported package manager. Install manually: pkg-config, gtk3, webkit2gtk-4.1, libsoup-3.0 (dev packages)"; \
		exit 1; \
	fi

## Build targets

# build produces everything needed to run the solution: the web UI bundle that
# both binaries embed, the desktop binary, and the headless server binary.
build: build-frontend build-desktop build-server

build-desktop: build-frontend
	CGO_ENABLED=1 go build -tags "$(DESKTOP_TAGS)" -ldflags "$(LDFLAGS)" -o bin/sysmon ./cmd/sysmon

# build-server depends on build-frontend because cmd/sysmon embeds
# frontend/dist; without it the //go:embed directive fails to compile.
#
# CGO is required: pkg/collector imports NVIDIA go-nvml, which does not build
# with CGO_ENABLED=0. The result is dynamically linked and needs a matching
# glibc on the target host. No desktop tag, so no Wails runtime is pulled in.
build-server: build-frontend
	CGO_ENABLED=1 go build -tags "$(SERVER_TAGS)" -ldflags "$(LDFLAGS)" -o bin/sysmon-server ./cmd/sysmon

# svelte-check runs before the bundle: it is what catches a call site left
# behind by a regenerated interface, and it needs the generated files to be
# current -- which is why this VERIFIES them and does not rewrite them.
#
# check-generated, NOT generate, and that distinction is the whole point: the
# build must not mutate a checked-in source file. `ci` runs lint (which contains
# check-generated) and build in one target, so under `make -j` a generate here
# would race that check -- and if the generate won, check-generated would compare
# files it had just rewritten and pass over generated output that was never
# committed, which is precisely the drift the gate exists to catch. Vite could
# equally read a half-rewritten source. Verifying is idempotent and safe to run
# concurrently with anything; regenerating is not. Change a Go wire type and the
# build tells you to run `make generate` and commit the result.
#
# Depends on the shared install rather than running its own `npm install`: under
# `make -j` that raced check-bundle-current's `npm ci` in the same directory, and
# because npm ci deletes and recreates node_modules either build could observe a
# half-replaced dependency tree. One install target, keyed to the lockfile, is
# also what makes a local build match the hosted one.
build-frontend: check-generated $(FRONTEND_STAMP)
	cd $(FRONTEND_DIR) && npm run check && npm run build

# generate rewrites the dashboard's wire types and runtime schema from
# pkg/types/types.go.
#
# It is Go only -- no npm, no node -- so it runs in the same builds that must
# work without a JavaScript toolchain at all.
generate:
	go run ./cmd/tsgen

# check-generated fails when the committed TypeScript does not match what the
# current pkg/types produces.
#
# The whole value of generating these is that they cannot drift from the Go
# types, and that only holds if something checks. A regenerated file that was
# never committed is the hand-mirror problem back again, with the added
# confusion of a file that says DO NOT EDIT and is wrong.
#
# It generates into a temporary directory and diffs, rather than regenerating in
# place and asking git: a working tree with legitimate uncommitted work in it
# must not fail this, and a `git diff` check cannot tell the two apart.
#
# EVERY step is joined with `&&`, the closing echo included. With a `;` before
# it, a failing mktemp or a generator that does not compile skipped the
# comparison and then ran the echo, whose zero status became the recipe's --
# so the gate announced "current" without having compared anything, and
# build-frontend depends on it.
check-generated:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	go run ./cmd/tsgen -out "$$tmp" && \
	for f in types.generated.ts schema.generated.ts; do \
		if ! diff -u "$(FRONTEND_DIR)/src/lib/$$f" "$$tmp/$$f"; then \
			echo ""; \
			echo "$$f is out of date. Run 'make generate' and commit the result."; \
			exit 1; \
		fi; \
	done && \
	echo "Generated TypeScript is current."

dev:
	cd cmd/sysmon && wails dev

## Test targets

# -race is on by default: the monitor broadcasts to concurrent subscribers and
# two real data races shipped undetected while this target ran without it.
# Every shape, because a build tag that is only ever compiled one way is not
# tested (#100, #204): see BUILD_SHAPES at the top of this file.
test: $(addprefix test-shape-,$(TEST_SHAPES))

# The desktop shapes link GTK and WebKit, so they need the packages `make deps`
# installs; so do their vet and lint below.
test-shape-%:
	go test -tags "$(SHAPE_$*)" $(PKG_ALL) -race -count=1

# test-fips runs the suite in FIPS 140-3 mode, in the shapes a FIPS consumer
# embeds (go-quicbaas builds pkg/collector with CGO_ENABLED=0 GOFIPS140=latest,
# #8). The tagged run matters too: pkg/prom encodes protobuf and snappy by hand,
# so if anything in it reaches a non-approved primitive, this is where it shows
# (#100).
FIPS_ENV := GOFIPS140=latest GODEBUG=fips140=on
test-fips:
	$(FIPS_ENV) go build $(PKG_ALL)
	$(FIPS_ENV) go build -tags prometheus $(PKG_ALL)
	$(FIPS_ENV) go test -count=1 -timeout=900s $(PKG_ALL)
	$(FIPS_ENV) go test -tags prometheus -count=1 -timeout=900s $(PKG_ALL)

# build-both compiles the two shapes without running anything, which is the
# cheap half of the same guarantee.
build-both:
	CGO_ENABLED=1 go build ./...
	CGO_ENABLED=1 go build -tags "$(SERVER_TAGS)" ./...

test-cli:
	go test $(PKG_CLI) -v -count=1

test-collector:
	go test $(PKG_COLLECTOR) -v -count=1

test-monitor:
	go test $(PKG_MONITOR) -v -count=1

test-server:
	go test $(PKG_SERVER) -v -count=1

test-types:
	go test $(PKG_TYPES) -v -count=1

test-metrics:
	go test $(PKG_METRICS) -v -count=1

test-cmd:
	go test $(PKG_CMD) -v -count=1

test-tsgen:
	go test $(PKG_TSGEN) -v -count=1

test-buildtags:
	go test $(PKG_BUILDTAGS) -v -count=1

test-deadpeer:
	go test $(PKG_DEADPEER) -race -v -count=1

test-pins:
	go test $(PKG_PINS) -v -count=1

test-gitea:
	go test $(PKG_GITEA) -v -count=1

## Coverage targets

coverage-cli:
	go test $(PKG_CLI) -coverprofile=/tmp/coverage-cli.out -count=1
	go tool cover -func=/tmp/coverage-cli.out

coverage-collector:
	go test $(PKG_COLLECTOR) -coverprofile=/tmp/coverage-collector.out -count=1
	go tool cover -func=/tmp/coverage-collector.out

coverage-monitor:
	go test $(PKG_MONITOR) -coverprofile=/tmp/coverage-monitor.out -count=1
	go tool cover -func=/tmp/coverage-monitor.out

coverage-server:
	go test $(PKG_SERVER) -coverprofile=/tmp/coverage-server.out -count=1
	go tool cover -func=/tmp/coverage-server.out

coverage-types:
	go test $(PKG_TYPES) -coverprofile=/tmp/coverage-types.out -count=1
	go tool cover -func=/tmp/coverage-types.out

coverage-tsgen:
	go test $(PKG_TSGEN) -coverprofile=/tmp/coverage-tsgen.out -count=1
	go tool cover -func=/tmp/coverage-tsgen.out

coverage-buildtags:
	go test $(PKG_BUILDTAGS) -coverprofile=/tmp/coverage-buildtags.out -count=1
	go tool cover -func=/tmp/coverage-buildtags.out

coverage-deadpeer:
	go test $(PKG_DEADPEER) -coverprofile=/tmp/coverage-deadpeer.out -count=1
	go tool cover -func=/tmp/coverage-deadpeer.out

coverage-pins:
	go test $(PKG_PINS) -coverprofile=/tmp/coverage-pins.out -count=1
	go tool cover -func=/tmp/coverage-pins.out

coverage-gitea:
	go test $(PKG_GITEA) -coverprofile=/tmp/coverage-gitea.out -count=1
	go tool cover -func=/tmp/coverage-gitea.out

coverage-metrics:
	go test $(PKG_METRICS) -coverprofile=/tmp/coverage-metrics.out -count=1
	go tool cover -func=/tmp/coverage-metrics.out

## Benchmark targets

bench-cli:
	go test $(PKG_CLI) -bench=. -benchmem -count=1 -run='^$$'

bench-collector:
	go test $(PKG_COLLECTOR) -bench=. -benchmem -count=1 -run='^$$'

bench-fleet:
	go test $(PKG_FLEET) -bench=. -benchmem -count=1 -run='^$$'

bench-monitor:
	go test $(PKG_MONITOR) -bench=. -benchmem -count=1 -run='^$$'

bench-server:
	go test $(PKG_SERVER) -bench=. -benchmem -count=1 -run='^$$'

## E2E tests

# test-e2e drives the real server in a headless browser. "playwright install"
# is deliberately run without --with-deps: that flag apt-installs system
# packages, which a test target must never do to a developer's machine. Install
# the OS libraries once by hand if the browser fails to launch.
test-e2e: build-server build-frontend
	cd $(FRONTEND_DIR) && npx playwright install chromium && npx playwright test

## Desktop extension tests

# test-extensions is tier 1 and tier 2: the shared modules' unit tests plus a
# harness that loads applet.js against stand-in GObject Introspection objects.
# Both run in-process under Node, need no display, and are part of `make ci`.
# It also runs the tier 3 runner script against stand-in desktop tools, to prove
# its session gate fails closed.
test-extensions:
	node --test 'extensions/tests/*.test.js'

# test-extensions-desktop is tier 3: a real Cinnamon session on headless Xorg, driven
# through the org.Cinnamon D-Bus Eval method. It needs a Cinnamon install and
# software GL, so it is slow and is not part of `make ci` -- run it nightly or
# before touching applet.js.
#
# The tmpfs over /sys/class/drm hides the host's GPUs, so the container sees the
# same (empty) set of DRM devices on every host (#163). What Cinnamon logs at
# startup depends on that set, and the runner refuses any other view.
test-extensions-desktop: desktop-docker-build
	docker run --rm --shm-size=256m --mount type=tmpfs,destination=/sys/class/drm $(DESKTOP_IMAGE)

desktop-docker-build:
	docker build -t $(DESKTOP_IMAGE) -f test/desktop/Dockerfile .

## Integration tests

# integration-test-cli runs the CLI integration suite directly on the host.
# The binary is built internally by TestMain; no pre-built binary is required.
integration-test-cli:
	CGO_ENABLED=0 go test -v -tags integration -count=1 -timeout 120s ./test/integration/...

# integration-test builds the Docker image and runs the full suite inside a
# container so that smartmontools and dmidecode are available.
integration-test: docker-build
	docker run --rm --privileged $(DOCKER_IMAGE)

docker-build:
	docker build -t $(DOCKER_IMAGE) -f test/integration/Dockerfile .

# serve runs the web UI locally. Override the listen address and poll rate with
# e.g. `make serve ADDR=:9090 INTERVAL=500`.
serve: build-server
	bin/sysmon-server serve --addr $(ADDR) --interval $(INTERVAL)


## Desktop extensions

install-extension-cinnamon:
	mkdir -p $(APPLET_DEST)
	cp -r $(APPLET_SRC)/* $(APPLET_DEST)/
	cp $(EXTENSION_SHARED)/*.js $(APPLET_DEST)/

uninstall-extension-cinnamon:
	rm -rf $(APPLET_DEST)

## Lint

# lint fails the build on any finding. gofmt and vet always run; golangci-lint
# must be the pinned version (see GOLANGCI_LINT_VERSION), and `make tools`
# installs it. check-generated keeps the dashboard's TypeScript in step with
# pkg/types.
lint: fmt-check vet lint-workflows check-conflicts check-cgo-clean check-generated check-build-tags check-pins check-errcheck-nolint check-ts \
	$(addprefix lint-shape-,$(ALL_SHAPES))

# golangci-lint in one shape. Its unused check is what finds code that only one
# shape compiles and no shape calls. Every shape runs the same .golangci.yml, so
# errcheck's full test-code coverage (#196) reaches every tagged test file --
# the integration shape is what lints test/integration at all.
lint-shape-%:
	@$(TOOL) check "$(GOLANGCI_LINT)" $(GOLANGCI_LINT_PKG) $(GOLANGCI_LINT_VERSION)
	$(GOLANGCI_LINT) run --build-tags "$(SHAPE_$*)" $(PKG_ALL)

# check-errcheck-nolint fails when any Go file silences errcheck with //nolint.
# The linter checks every file in full -- tests since #196, production since
# #207 -- and a directive is the one way around that it cannot report. See the
# script for what counts.
check-errcheck-nolint:
	@./scripts/check-errcheck-nolint.sh

# check-ts type-checks the frontend TypeScript that svelte-check does not
# cover: the Playwright specs and their fixtures (tests/tsconfig.json), and the
# Node-side configuration -- vite.config.ts, svelte.config.js,
# playwright.config.ts and e2e-port.ts (tsconfig.node.json). svelte-check
# covers src/ only, so the specs had never been type-checked and one already
# carried an error nothing reported (#196); the build configuration was left
# out the same way (#209).
#
# This is what makes `lint` need node. The committed bundle exists so that
# BUILDING the Go binary never does, and that still holds; type-checking
# cannot happen without the TypeScript compiler the files are written for.
check-ts: $(FRONTEND_STAMP)
	cd $(FRONTEND_DIR) && npm run check:tests && npm run check:node

# tools installs every pinned analysis tool that is missing or at the wrong
# version, and leaves a matching one alone. The per-tool targets let a CI job
# fetch only what it runs.
tools: tool-golangci-lint tool-actionlint tool-gosec tool-govulncheck

tool-golangci-lint:
	@$(TOOL) ensure "$(GOLANGCI_LINT)" $(GOLANGCI_LINT_PKG) $(GOLANGCI_LINT_VERSION) "$(GOBIN_DIR)" "$(origin GOLANGCI_LINT)"

tool-actionlint:
	@$(TOOL) ensure "$(ACTIONLINT)" $(ACTIONLINT_PKG) $(ACTIONLINT_VERSION) "$(GOBIN_DIR)" "$(origin ACTIONLINT)"

tool-gosec:
	@$(TOOL) ensure "$(GOSEC)" $(GOSEC_PKG) $(GOSEC_VERSION) "$(GOBIN_DIR)" "$(origin GOSEC)"

tool-govulncheck:
	@$(TOOL) ensure "$(GOVULNCHECK)" $(GOVULNCHECK_PKG) $(GOVULNCHECK_VERSION) "$(GOBIN_DIR)" "$(origin GOVULNCHECK)"

# check-signed fails when a commit on this branch carries no signature.
#
# NOT part of `lint`, deliberately. It needs a base commit to have a range to
# scan, so locally it would print SKIPPED on every run -- and a line that is
# always skipped teaches people to read past it, which is how the next real
# skip goes unnoticed. CI passes the PR base and it does real work there.
check-signed:
	@SIGNED_SCAN_BASE="$(SIGNED_SCAN_BASE)" ./scripts/check-signed-commits.sh

# check-conflicts fails when a git conflict marker has been committed.
#
# It is part of lint because the class it catches is invisible to everything
# else: the committed frontend bundle is generated output that nothing compiles
# and no test reads, so a marker in it survives every other gate (#40).
#
# Set CONFLICT_SCAN_BASE to also scan each commit ahead of that base. Without
# it only the working tree is scanned, and the script says so rather than
# reporting a pass it did not earn.
check-conflicts:
	@CONFLICT_SCAN_BASE="$(CONFLICT_SCAN_BASE)" ./scripts/check-conflict-markers.sh

# check-cgo-clean asserts the default build needs no cgo, which is what lets a
# consumer embed pkg/collector in a static, cross-compiled or FIPS binary (#8).
#
# It builds the whole module rather than pkg/collector alone, because the
# property a consumer depends on is transitive: one unguarded cgo import
# anywhere in the import graph takes it away, and the failure appears in their
# build rather than in ours. `go vet` is included because it compiles the test
# binary too, which is how an nvml import in a _test.go file would slip through.
check-cgo-clean:
	CGO_ENABLED=0 go build ./...
	CGO_ENABLED=0 go vet ./...

# check-bundle-current verifies the committed dashboard bundle matches its source.
#
# cmd/sysmon/frontend/dist is generated output that is committed on purpose --
# //go:embed all:frontend/dist means a clone without it cannot `go build` -- and
# nothing checked the two copies agreed. They have disagreed three times; most
# recently `main` served a dashboard built before #130's PSI card and #132's swap
# types, with every test passing, because no test reads the bundle.
#
# NOT a prerequisite of `lint`, deliberately: it rebuilds the whole bundle,
# which is a build rather than a check of source, and as its own CI job a stale
# bundle is reported as exactly that. (`lint` does need node since #196, for
# check-ts; that type-checks the specs and the build configuration and builds
# nothing.)
# frontend-deps installs the node toolchain when it is absent OR older than the
# lockfile, so check-bundle-current can run from an aggregate target on a fresh
# checkout without paying for npm every time.
#
# Keyed to package-lock.json rather than to vite's presence: an existing checkout
# whose lockfile has moved on would otherwise build the bundle with a stale
# dependency graph, while the hosted job's clean `npm ci` uses the new one -- and
# the two would then disagree about whether the committed bundle is current, which
# is precisely the question this gate exists to answer.
# package.json as well as the lockfile: a manifest change without a lockfile
# change would otherwise leave an existing stamp in place, so a local build would
# use stale dependencies while a clean CI checkout's npm ci rejects the
# manifest/lockfile mismatch -- the two disagreeing again, which is the failure
# this stamp exists to avoid.
$(FRONTEND_STAMP): $(FRONTEND_DIR)/package-lock.json $(FRONTEND_DIR)/package.json
	cd $(FRONTEND_DIR) && npm ci
	@touch $@

frontend-deps: $(FRONTEND_STAMP)

check-bundle-current: frontend-deps
	@./scripts/check-bundle-current.sh

# lint-workflows validates the GitHub Actions files with the same expression
# parser GitHub uses. A rejected workflow fails the run before any job starts,
# which surfaces as "0 jobs" rather than as a test failure -- so it is worth
# catching here rather than after a push.
#
# actionlint shells out to shellcheck and pyflakes for `run:` blocks when they
# are on PATH, and silently skips those checks when they are not. CI runners
# ship both, so a missing shellcheck locally means `make lint` is quietly
# weaker than the same target in CI -- which is exactly how an SC2046 reached
# main. The warning below makes that visible instead of silent.
lint-workflows:
	@command -v shellcheck >/dev/null 2>&1 || \
		echo "warning: shellcheck not installed; actionlint will skip run-block checks (CI runs them)"
	@command -v pyflakes >/dev/null 2>&1 || command -v pyflakes3 >/dev/null 2>&1 || \
		echo "warning: pyflakes not installed; actionlint will skip python checks (CI runs them)"
	@$(TOOL) check "$(ACTIONLINT)" $(ACTIONLINT_PKG) $(ACTIONLINT_VERSION)
	$(ACTIONLINT) .github/workflows/*.yml

# fmt-check fails when any tracked Go file is not gofmt-clean, listing offenders.
fmt-check:
	@unformatted=$$(gofmt -l cmd internal pkg test); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-clean:"; \
		echo "$$unformatted"; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi
	@echo "gofmt: clean"

fmt:
	gofmt -w cmd internal pkg test

# vet runs in every shape, test files included. The nomegaraid shape is here
# because the hardware RAID passthrough is built only for the 64-bit ABI its
# packet layout describes, with a stub elsewhere. pkg/collector cannot be
# compiled for a 32-bit target at all (NVML, issue #8), so the stub was
# unverifiable and silently stopped compiling: two symbols that untagged files
# use lived only in the tagged ones. Selecting a file is not the same as
# building it, and this is what checks the difference.
vet: $(addprefix vet-shape-,$(ALL_SHAPES))

vet-shape-%:
	go vet -tags "$(SHAPE_$*)" $(PKG_ALL)

# check-pins fails when any version reference disagrees with the pin it
# repeats: a golang, node, debian or ubuntu image tag in a Dockerfile, compose
# file or workflow, or an actions/setup-go or actions/setup-node step. Offline,
# so it runs in lint. See docs/testing/toolchain.md.
check-pins:
	go run ./cmd/checkpins $(PIN_ARGS)

# check-latest fails when any pin is not at its latest stable release: Go,
# each pinned tool, Node.js LTS, the Debian and Ubuntu releases, and every
# action the workflows use. It needs the network, so it is NOT part of lint -- an upstream release
# must never fail an unrelated PR. The daily check-latest workflow runs it and
# files an issue instead. Exit 1 means something lags; 2 means an item could
# not be checked, which is never read as current.
CHECK_LATEST_ARGS = $(PIN_ARGS) $(foreach t,$(PINNED_TOOLS),-tool $(call tool_name,$(t))=$($(t)_MOD)@$($(t)_VERSION))
tool_name = $(notdir $($(1)_PKG))
check-latest:
	go run ./cmd/checklatest $(CHECK_LATEST_ARGS) $(CHECK_LATEST_FLAGS)

# check-build-tags fails when a build constraint names a tag no shape sets --
# a file no gate compiles -- or when the Dockerfile builds the image with tags
# other than IMAGE_TAGS, the shape that stands for it here.
check-build-tags:
	go run ./cmd/buildtags -shapes "$(foreach s,$(ALL_SHAPES),$(SHAPE_$(s)))"
	@grep -qE '^RUN go build .*-tags $(IMAGE_TAGS) ' Dockerfile || { \
		echo "Dockerfile does not build with -tags $(IMAGE_TAGS) (IMAGE_TAGS): update one to match the other."; \
		exit 1; }

## Clean

clean-dist:
	rm -rf $(RELEASE_DIR)

clean: clean-dist
	go clean ./...
	rm -rf bin/ /tmp/coverage-*.out
	rm -rf $(FRONTEND_DIR)/dist $(FRONTEND_DIR)/node_modules

## Deployment

# There are two ways to put sysmon on another host, and they are for different
# situations:
#
#   make deploy         a supervised systemd service that survives reboot
#   make deploy-binary  one file into /usr/local/bin, you arrange to run it
#
# `deploy` is the one to reach for. `deploy-binary` exists for dropping a build
# onto a machine to run by hand.
#
# Full documentation: docs/deployment/

SSH_OPTS ?= -o BatchMode=yes -o ConnectTimeout=10

# deploy builds the server binary here, copies it to the target over SSH,
# creates a dedicated service account, installs an environment file and a
# hardened systemd unit, and starts the service. The target needs systemd, SSH
# key access and sudo -- no container runtime, no registry, no Go and no Node.
#
#   make deploy HOST=host.example.com
#   make deploy HOST=host.example.com USER=deploy
#   make deploy HOST=host.example.com SYSMON_ADDR=:9090
#   make deploy HOST=host.example.com SYSMON_ADDR=10.0.0.5:8081
#
# It is idempotent: re-running converges, and it will not overwrite the
# target's /etc/go-sysmon/sysmon.env once that exists.
#
# USER defaults to the environment's $USER, which is what ssh would use anyway;
# set it explicitly when the account on the target differs.
CONF_DIR ?= /etc/go-sysmon
BIN_DIR ?= /usr/local/bin
SVC_USER ?= sysmon
SUDO ?= sudo

deploy: build-server
	@HOST='$(HOST)' \
	 SSH_USER='$(USER)' \
	 CONF_DIR='$(CONF_DIR)' \
	 BIN_DIR='$(BIN_DIR)' \
	 SVC_USER='$(SVC_USER)' \
	 SYSMON_ADDR='$(SYSMON_ADDR)' \
	 VERSION='$(VERSION)' \
	 SSH_OPTS='$(SSH_OPTS)' \
	 SUDO='$(SUDO)' \
	 bash scripts/deploy.sh

# deploy-binary copies the server binary to another Linux host and reports how
# to run it. The frontend is embedded, so this single file is the whole
# application -- but nothing supervises it and nothing starts it at boot.
#
#   make deploy-binary HOST=host.example.com
#   make deploy-binary HOST=user@host DEST=/usr/local/bin
#
# The binary is dynamically linked against glibc (CGO is required by the NVIDIA
# NVML dependency), so the target needs a glibc at least as new as the build
# host's. The target is checked before anything is copied.
DEST ?= /usr/local/bin

deploy-binary: build-server
	@test -n "$(HOST)" || { echo "HOST is required, e.g. make deploy-binary HOST=host.example.com"; exit 1; }
	@echo "==> checking $(HOST)"
	@ssh $(SSH_OPTS) $(HOST) 'test "$$(uname -m)" = x86_64 || { echo "target is not x86_64"; exit 1; }; echo "    arch  $$(uname -m)"; echo "    glibc $$(ldd --version | head -1)"'
	@echo "==> copying bin/sysmon-server to $(HOST):$(DEST)"
	@scp -q bin/sysmon-server $(HOST):/tmp/sysmon-server
	@ssh $(SSH_OPTS) $(HOST) 'sudo install -m 0755 /tmp/sysmon-server $(DEST)/sysmon-server && rm -f /tmp/sysmon-server'
	@echo "==> verifying"
	@ssh $(SSH_OPTS) $(HOST) '$(DEST)/sysmon-server version'
	@echo ""
	@echo "Installed. Start the web service on the target with:"
	@echo "    ssh $(HOST) '$(DEST)/sysmon-server serve --addr :8080'"
	@echo ""
	@echo "For a service that survives logout and reboot, use 'make deploy' instead"
	@echo "(docs/deployment/), or write a unit by hand: docs/server/deployment.md"

## CI/CD

# vulncheck reports known vulnerabilities in dependencies *and* in the Go
# standard library the project is built against. The go directive in go.mod
# pins the patched compiler (there is no separate toolchain directive), so this
# fails if that pin regresses.
# gosec rule policy.
#
# G304 (file inclusion via variable) and G115 (integer overflow on conversion)
# are excluded repo-wide, not because they are unimportant but because they do
# not discriminate here: reading a /sys or /proc path assembled from a device
# name IS what this program does, and every sysfs value arrives as a string
# that must be converted to a sized integer. Roughly 40 of the 42 findings were
# those two rules on exactly that pattern, which drowns out anything real --
# gosec's own value showed up as G112, a genuinely missing ReadHeaderTimeout.
#
# Individual exceptions to the remaining rules carry a #nosec comment naming
# the reason at the call site.
GOSEC_EXCLUDE ?= G304,G115
GOSEC_ARGS ?= -exclude-dir=cmd/sysmon/frontend -exclude=$(GOSEC_EXCLUDE) -severity medium

# gosec and govulncheck run in every shape, like vet: a finding in code only the
# desktop or server build compiles -- Wails, the tray, NVML, remote write -- is a
# finding in a binary we ship, and an untagged scan never reads that code.
gosec: $(addprefix gosec-shape-,$(ALL_SHAPES))

gosec-shape-%:
	@$(TOOL) check "$(GOSEC)" $(GOSEC_PKG) $(GOSEC_VERSION)
	$(GOSEC) -concurrency 4 -tags "$(SHAPE_$*)" $(GOSEC_ARGS) ./...

vulncheck: $(addprefix vulncheck-shape-,$(BUILD_SHAPES))

vulncheck-shape-%:
	@$(TOOL) check "$(GOVULNCHECK)" $(GOVULNCHECK_PKG) $(GOVULNCHECK_VERSION)
	$(GOVULNCHECK) -tags "$(SHAPE_$*)" ./...

# audit checks the frontend dependency tree, and distinguishes the two ways it
# can come back non-zero.
#
# npm audit exits non-zero both for "these packages have advisories" and for "the
# advisory endpoint did not answer", and only the first is ours. Classifying that
# by matching npm's error text does not work: an HTTP 429, a 5xx, a TLS or auth
# failure and npm's own "audit endpoint returned an error" all read differently,
# and the list has no end. scripts/audit-gate.py detects the SUCCESS case
# positively instead -- a report carrying auditReportVersion and
# metadata.vulnerabilities -- and exits 1 for findings, 2 for an audit that never
# completed.
#
# This target keeps a non-zero exit either way: a developer asking about
# advisories wants a status to act on. CI does not fail on it (owner ruling
# 2026-09-16) and tells the two apart by that exit code -- see the Frontend
# vulnerability scan step in .github/workflows/ci.yml.
# --no-offline is load-bearing, not tidiness. With NPM_CONFIG_OFFLINE=true in the
# environment (or offline=true in an inherited .npmrc) npm 11.9.0 emits a complete
# report schema with zero vulnerabilities and exits 0 WITHOUT contacting the
# advisory endpoint -- measured, and it does so even against a dead registry. The
# gate would then report a clean audit that never ran, which is a worse failure
# than the false alarm this target was written to remove: a security check that
# passes by not looking. With the flag, an unreachable endpoint produces no report
# and is classified as "could not run".
AUDIT_LEVEL ?= low
# AUDIT_STATUS_FILE, when set, is where the gate records its exit code. GNU make
# returns 2 for any failed recipe, so a caller cannot tell the gate's 1 from its 2
# through this target's status -- CI needs the distinction and reads the file.
AUDIT_STATUS_FILE ?=

audit:
	@tmp=$$(mktemp -d); \
	( cd $(FRONTEND_DIR) && npm audit --no-offline --audit-level=$(AUDIT_LEVEL) --json >$$tmp/audit.json 2>$$tmp/audit.err ) || true; \
	rc=0; AUDIT_STATUS_FILE="$(AUDIT_STATUS_FILE)" python3 scripts/audit-gate.py $$tmp/audit.json $(AUDIT_LEVEL) || rc=$$?; \
	if [ $$rc -eq 2 ] && [ -s $$tmp/audit.err ]; then \
		echo "--- what npm said ---"; head -5 $$tmp/audit.err; \
	fi; \
	rm -rf $$tmp; exit $$rc

# audit-report is `audit` without the power to stop a pipeline, and it is what the
# aggregate ci targets use.
#
# devops.md §4: CI runs the same targets a developer runs. The hosted workflow does
# not fail on advisories or on an unreachable advisory endpoint (owner ruling
# 2026-09-16), so `make ci` must not either -- otherwise the third party's
# availability still breaks the local pipeline and the two paths disagree about
# what a red build means. Direct `make audit` keeps its non-zero status, because a
# developer asking the question wants an answer to act on.
audit-report:
	@$(MAKE) --no-print-directory audit || true

# scan-image builds the container and scans it, plus the binaries inside it,
# for known vulnerabilities. Trivy runs from its own image rather than an
# action, so this behaves identically here, on GitHub and on a self-hosted
# runner -- an action that installs itself through a nested checkout cannot.
#
# TRIVY_SEVERITY defaults to the two levels worth failing a build over.
TRIVY_SEVERITY ?= CRITICAL,HIGH
TRIVY_IMAGE ?= go-sysmon:scan

scan-image:
	docker build -t $(TRIVY_IMAGE) .
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
	    aquasec/trivy:latest image \
	      --severity $(TRIVY_SEVERITY) \
	      --no-progress \
	      --format table \
	      $(TRIVY_IMAGE)

# scan-image-sarif emits machine-readable output for the Security tab.
scan-image-sarif:
	docker build -t $(TRIVY_IMAGE) .
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
	    aquasec/trivy:latest image \
	      --severity $(TRIVY_SEVERITY) \
	      --no-progress \
	      --format sarif \
	      $(TRIVY_IMAGE) > trivy-results.sarif

# ci runs the full pipeline exactly as the GitHub workflow does, so a green
# local run means a green CI run. Ordered cheapest-first: formatting and vet
# fail in seconds, the browser suite takes a minute.
#
#   make ci          full pipeline
#   make ci-fast     everything except the container and browser suites
# ci runs the full pipeline exactly as the GitHub workflow does, so a green local
# run means a green CI run. Ordered cheapest-first: formatting and vet fail in
# seconds, the browser suite takes a minute. One dependency graph, so shared
# prerequisites are built once -- check-bundle-current reads the bundle from HEAD
# rather than the working tree, so it does not care whether build has run yet.
ci: fmt-check vet lint vulncheck gosec audit-report test test-fips test-extensions check-bundle-current build build-both integration-test test-e2e
	@echo ""
	@echo "CI pipeline passed."

ci-fast: fmt-check vet lint vulncheck gosec audit-report test test-fips test-extensions check-bundle-current build build-both
	@echo ""
	@echo "CI (fast) passed. Container and browser suites were skipped."

# release-build produces the artifacts the release workflow publishes.
#
# CGO is mandatory (the NVIDIA NVML dependency does not build without it), so
# the server binary is dynamically linked against glibc and must be built on,
# or cross-compiled for, the target architecture. That is why this builds the
# host architecture only; the release workflow runs it on each runner.
RELEASE_DIR ?= dist
release-build: build-frontend
	@mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=1 go build -trimpath -tags "$(SERVER_TAGS)" -ldflags "$(LDFLAGS) -s -w" \
		-o $(RELEASE_DIR)/sysmon-server-$(VERSION)-linux-$(shell go env GOARCH) ./cmd/sysmon
	cd $(RELEASE_DIR) && sha256sum sysmon-server-$(VERSION)-linux-$(shell go env GOARCH) \
		> sysmon-server-$(VERSION)-linux-$(shell go env GOARCH).sha256
	@echo "Built $(RELEASE_DIR)/sysmon-server-$(VERSION)-linux-$(shell go env GOARCH)"

# release-build-desktop additionally links the Wails GUI, which requires the
# GTK and WebKit development packages from "make deps".
release-build-desktop: build-frontend
	@mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=1 go build -trimpath -tags "$(DESKTOP_TAGS)" -ldflags "$(LDFLAGS) -s -w" \
		-o $(RELEASE_DIR)/sysmon-$(VERSION)-linux-$(shell go env GOARCH) ./cmd/sysmon
	cd $(RELEASE_DIR) && sha256sum sysmon-$(VERSION)-linux-$(shell go env GOARCH) \
		> sysmon-$(VERSION)-linux-$(shell go env GOARCH).sha256
	@echo "Built $(RELEASE_DIR)/sysmon-$(VERSION)-linux-$(shell go env GOARCH)"
