MODULE := github.com/jeremyhahn/go-sysmon
VERSION := $(shell cat VERSION 2>/dev/null || echo "0.1.0")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.buildDate=$(BUILD_DATE)

# Feature flags (set to 0 to disable)
WITH_SYSTRAY ?= 1

# Build tags — desktop + systray assembled dynamically
DESKTOP_TAGS := desktop,production,webkit2_41
ifeq ($(WITH_SYSTRAY),1)
  DESKTOP_TAGS := $(DESKTOP_TAGS),systray
endif

PKG_ALL := ./...
PKG_CLI := ./pkg/cli/...
PKG_COLLECTOR := ./pkg/collector/...
PKG_MONITOR := ./pkg/monitor/...
PKG_SERVER := ./pkg/server/...
PKG_TYPES := ./pkg/types/...
PKG_CMD := ./cmd/sysmon/...

FRONTEND_DIR := cmd/sysmon/frontend
DOCKER_IMAGE := go-sysmon-test

# Tools installed via "go install" land in GOBIN (or GOPATH/bin) which is not
# on the PATH make uses, so resolve golangci-lint explicitly.
GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
  GOBIN_DIR := $(shell go env GOPATH)/bin
endif
GOLANGCI_LINT ?= $(GOBIN_DIR)/golangci-lint
ACTIONLINT ?= $(GOBIN_DIR)/actionlint

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

.PHONY: all deps build build-desktop build-server build-frontend dev serve test test-cli test-collector test-monitor \
        test-server test-types coverage-cli coverage-collector coverage-monitor coverage-server \
        bench-cli bench-collector bench-monitor bench-server integration-test integration-test-cli \
        test-e2e lint lint-workflows fmt fmt-check vet clean docker-build deploy \
        ci ci-fast vulncheck gosec audit release-build release-build-desktop clean-dist \
        install-extension-cinnamon uninstall-extension-cinnamon \
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
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o bin/sysmon-server ./cmd/sysmon

# svelte-check runs before the bundle: the TypeScript interfaces in
# frontend/src/lib/types.ts mirror the Go types by hand, and drift between them
# is otherwise invisible until a panel renders "undefined" at runtime.
build-frontend:
	cd $(FRONTEND_DIR) && npm install && npm run check && npm run build

dev:
	cd cmd/sysmon && wails dev

## Test targets

# -race is on by default: the monitor broadcasts to concurrent subscribers and
# two real data races shipped undetected while this target ran without it.
test:
	go test $(PKG_ALL) -race -count=1

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

test-cmd:
	go test $(PKG_CMD) -v -count=1

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

## Benchmark targets

bench-cli:
	go test $(PKG_CLI) -bench=. -benchmem -count=1 -run='^$$'

bench-collector:
	go test $(PKG_COLLECTOR) -bench=. -benchmem -count=1 -run='^$$'

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
test-extensions:
	node --test 'extensions/tests/*.test.js'

# test-extensions-desktop is tier 3: a real Cinnamon session under Xvfb, driven
# through the org.Cinnamon D-Bus Eval method. It needs a Cinnamon install and
# software GL, so it is slow and is not part of `make ci` -- run it nightly or
# before touching applet.js.
test-extensions-desktop: desktop-docker-build
	docker run --rm --shm-size=256m $(DESKTOP_IMAGE)

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
# is resolved from GOBIN/GOPATH because "go install"-ed tools are not on the
# default PATH that make uses.
lint: fmt-check vet lint-workflows
	@if [ ! -x "$(GOLANGCI_LINT)" ] && ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install it with:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	$(GOLANGCI_LINT) run $(PKG_ALL)

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
	@if [ ! -x "$(ACTIONLINT)" ] && ! command -v actionlint >/dev/null 2>&1; then \
		echo "actionlint not found. Install it with:"; \
		echo "  go install github.com/rhysd/actionlint/cmd/actionlint@latest"; \
		exit 1; \
	fi
	$(ACTIONLINT) .github/workflows/*.yml

# fmt-check fails when any tracked Go file is not gofmt-clean, listing offenders.
fmt-check:
	@unformatted=$$(gofmt -l cmd pkg test); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-clean:"; \
		echo "$$unformatted"; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi
	@echo "gofmt: clean"

fmt:
	gofmt -w cmd pkg test

vet:
	go vet $(PKG_ALL)

## Clean

clean-dist:
	rm -rf $(RELEASE_DIR)

clean: clean-dist
	go clean ./...
	rm -rf bin/ /tmp/coverage-*.out
	rm -rf $(FRONTEND_DIR)/dist $(FRONTEND_DIR)/node_modules

# deploy copies the server binary to another Linux host and reports how to run
# it. The frontend is embedded, so this single file is the whole application.
#
#   make deploy HOST=192.168.101.90
#   make deploy HOST=user@host DEST=/usr/local/bin
#
# The binary is dynamically linked against glibc (CGO is required by the NVIDIA
# NVML dependency), so the target needs a glibc at least as new as the build
# host's. The target is checked before anything is copied.
DEST ?= /usr/local/bin
SSH_OPTS ?= -o BatchMode=yes -o ConnectTimeout=10

deploy: build-server
	@test -n "$(HOST)" || { echo "HOST is required, e.g. make deploy HOST=192.168.101.90"; exit 1; }
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
	@echo "For a service that survives logout and reboot, see docs/server/deployment.md"

## CI/CD

# GOVULNCHECK is resolved from GOBIN like golangci-lint; "go install"-ed tools
# are not on the PATH make uses.
GOVULNCHECK ?= $(GOBIN_DIR)/govulncheck
GOSEC ?= $(GOBIN_DIR)/gosec

# vulncheck reports known vulnerabilities in dependencies *and* in the Go
# standard library the project is built against. The toolchain directive in
# go.mod pins the patched compiler, so this fails if that pin regresses.
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

gosec:
	@if [ ! -x "$(GOSEC)" ] && ! command -v gosec >/dev/null 2>&1; then \
		echo "gosec not found. Install it with:"; \
		echo "  go install github.com/securego/gosec/v2/cmd/gosec@latest"; \
		exit 1; \
	fi
	$(GOSEC) $(GOSEC_ARGS) ./...

vulncheck:
	@if [ ! -x "$(GOVULNCHECK)" ] && ! command -v govulncheck >/dev/null 2>&1; then \
		echo "govulncheck not found. Install it with:"; \
		echo "  go install golang.org/x/vuln/cmd/govulncheck@latest"; \
		exit 1; \
	fi
	$(GOVULNCHECK) ./...

# audit checks the frontend dependency tree. npm audit exits non-zero on any
# finding, which is the behaviour we want in CI.
audit:
	cd $(FRONTEND_DIR) && npm audit --audit-level=low

# ci runs the full pipeline exactly as the GitHub workflow does, so a green
# local run means a green CI run. Ordered cheapest-first: formatting and vet
# fail in seconds, the browser suite takes a minute.
#
#   make ci          full pipeline
#   make ci-fast     everything except the container and browser suites
ci: fmt-check vet lint vulncheck gosec audit test test-extensions build integration-test test-e2e
	@echo ""
	@echo "CI pipeline passed."

ci-fast: fmt-check vet lint vulncheck gosec audit test test-extensions build
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
	CGO_ENABLED=1 go build -trimpath -ldflags "$(LDFLAGS) -s -w" \
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
