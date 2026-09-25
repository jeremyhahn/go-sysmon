# The devcontainer: the one environment the integration suite runs in (make
# integration-test). The suite's code lives in test/integration; this image
# carries the toolchain and the tools the collectors shell out to.
#
# The image Go version is pinned to match go.mod's go directive exactly, so
# toolchain has to be downloaded on every build.
FROM golang:1.27.1-trixie

# Install runtime tools used by the collector (smartmontools, dmidecode), and
# iproute2, with which the collector suite builds bridges, bonds, VLANs and
# veth pairs in a network namespace of its own.
RUN apt-get update \
    && apt-get install -y --no-install-recommends smartmontools dmidecode iproute2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache module downloads separately from the source copy so that rebuilds
# only re-download modules when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree (frontend dist must already be present).
COPY . .

# CGO is required because pkg/collector imports NVIDIA go-nvml, which does not
# compile with CGO_ENABLED=0. No desktop tag means no Wails runtime is needed.
ENV CGO_ENABLED=1

# SYSMON_DEVCONTAINER marks this environment. A suite that changes
# container-wide state -- cgroups, network devices, a runtime socket -- refuses
# to run without it, so it can never do so on a host.
ENV SYSMON_DEVCONTAINER=1

# Delegates cgroup controllers within the container before the command runs;
# see the script.
ENTRYPOINT ["./build/docker/devcontainer/entrypoint.sh"]

# The suite, with every package instrumented and each package's integration
# coverage reported. make integration-test and integration-test-<pkg> pass the
# command explicitly, with the coverage floor and the packages to report.
CMD ["./scripts/pkgtest.sh", "integration", "90"]
