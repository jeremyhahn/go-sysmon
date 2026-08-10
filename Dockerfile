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
FROM golang:1.26-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend /src/cmd/sysmon/frontend/dist ./cmd/sysmon/frontend/dist

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

# CGO is mandatory: pkg/collector imports NVIDIA go-nvml, which does not build
# with CGO_ENABLED=0. No desktop tag, so no GTK or WebKit is linked.
ENV CGO_ENABLED=1
RUN go build -trimpath \
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
RUN apt-get update \
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
