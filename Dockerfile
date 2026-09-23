# Container image for sysmon-server: the CLI and the embedded web dashboard.
#
# Only the server build is containerised. The desktop build links GTK and
# WebKit for a native window, which is meaningless without a display and would
# add ~120 shared libraries to the image.
#
# See docs/server/container.md for what the container needs from the host. The
# short version: a monitor that cannot see the host is not a monitor, so this
# image is only useful with --pid=host, --network=host and /sys mounted.

# --- frontend ---------------------------------------------------------------
# The dashboard is embedded in the binary via //go:embed, so it has to exist
# before the Go build runs.
FROM node:22-bookworm-slim AS frontend

WORKDIR /src/cmd/sysmon/frontend

# package.json and the lockfile first: npm ci only re-runs when deps change,
# not on every source edit.
COPY cmd/sysmon/frontend/package.json cmd/sysmon/frontend/package-lock.json ./
RUN npm ci

COPY cmd/sysmon/frontend/ ./
RUN npm run build

# --- binary -----------------------------------------------------------------
# The image Go version must satisfy the "go" directive in go.mod, or the
# toolchain gets downloaded on every build.
FROM golang:1.27.1-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend /src/cmd/sysmon/frontend/dist ./cmd/sysmon/frontend/dist

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

# -tags nvml, and therefore CGO_ENABLED=1, because this is a PUBLISHED image.
#
# NVML moved behind a build tag so that consumers embedding pkg/collector can
# build with CGO_ENABLED=0 (#8). The default build reports NVIDIA cards from sysfs
# and leaves memory, utilisation, clocks, power and ECC empty -- correct for a
# library consumer, wrong for the image an operator runs on an NVIDIA host, who
# would get a GPU tab that silently lost half its numbers. The tag has to be named
# here as well as in the Makefile's SERVER_TAGS, since this build does not go
# through make.
#
# No desktop tag, so no GTK or WebKit is linked. go-nvml loads libnvidia-ml.so
# at runtime rather than linking it, so the slim runtime image below needs nothing
# extra: a host without the library reports no NVIDIA GPUs, exactly as before.
ENV CGO_ENABLED=1
RUN go build -trimpath -tags nvml \
      -ldflags "-X main.version=${VERSION} \
                -X main.gitCommit=${GIT_COMMIT} \
                -X main.buildDate=${BUILD_DATE} -s -w" \
      -o /out/sysmon-server ./cmd/sysmon

# --- runtime ----------------------------------------------------------------
# Debian rather than Alpine: CGO means the binary is linked against glibc, and
# a musl base would need a separate toolchain for no real gain.
FROM debian:bookworm-slim

# curl is here for the healthcheck only. The image has no shell-based tooling
# otherwise, and "is the process alive" is a weak check for a monitor: the
# failure worth catching is a server that is running but no longer serving.
# apt-get upgrade, not just install: libpcre2-8-0 arrives as a dependency at the
# version the base layer was built with, and Debian's security update for it
# (10.42-1 -> 10.42-1+deb12u1) failed the Trivy gate, which is a required check,
# on every open pull request. Upgrading here takes the next such fix
# automatically instead of blocking CI again. Determinism is unaffected: this
# stage already runs an unpinned update/install, so the layer was never
# reproducible by pinning.
RUN apt-get update \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/sysmon-server /usr/local/bin/sysmon-server

# Runs as root by default, which is what SMART ioctls and the SMBIOS table
# require. Drop to an unprivileged user if you do not need those:
#   docker run --user 65534:65534 ...
# `sysmon doctor` reports exactly what the current privileges allow.
USER root

EXPOSE 8080

# No shell form: signals reach the process directly, so docker stop is a clean
# shutdown rather than a 10-second SIGKILL wait.
ENTRYPOINT ["/usr/local/bin/sysmon-server"]
CMD ["serve", "--addr", ":8080"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/ >/dev/null || exit 1

LABEL org.opencontainers.image.title="go-sysmon" \
      org.opencontainers.image.description="Real-time Linux system monitor with an embedded web dashboard" \
      org.opencontainers.image.source="https://github.com/jeremyhahn/go-sysmon" \
      org.opencontainers.image.licenses="MIT"
