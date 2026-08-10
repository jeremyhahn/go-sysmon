# The image Go version must satisfy the "go" directive in go.mod, otherwise the
# toolchain has to be downloaded on every build.
FROM golang:1.26-bookworm

# Install runtime tools used by the collector (smartmontools, dmidecode).
RUN apt-get update \
    && apt-get install -y --no-install-recommends smartmontools dmidecode \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache module downloads separately from the source copy so that rebuilds
# only re-download modules when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree (frontend dist must already be present).
COPY . .

# Run integration tests.
# CGO is required because pkg/collector imports NVIDIA go-nvml, which does not
# compile with CGO_ENABLED=0. No desktop tag means no Wails runtime is needed.
ENV CGO_ENABLED=1
CMD ["go", "test", "-v", "-tags", "integration", "-count=1", "-timeout", "120s", "./test/integration/..."]
