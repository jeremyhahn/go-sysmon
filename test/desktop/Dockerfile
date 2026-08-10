# Tier 3 desktop tests: a real Cinnamon session under Xvfb.
#
# Cinnamon needs a display, a session bus and a GL implementation. Xvfb and
# llvmpipe supply the first and last; dbus-launch supplies the second at run
# time. This is heavy and GL-in-a-container is not perfectly reliable, which is
# why the target is nightly rather than part of `make ci`.

# The server is built in its own stage so the Cinnamon image does not carry a Go
# toolchain and a C compiler. The Go version must satisfy the "go" directive in
# go.mod, otherwise the toolchain has to be downloaded on every build.
FROM golang:1.26-bookworm AS builder

WORKDIR /src

# Cache module downloads separately from the source copy so rebuilds only
# re-download when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO stays enabled: pkg/collector imports NVIDIA go-nvml, which does not
# compile without it.
RUN CGO_ENABLED=1 go build -o /out/sysmon-server ./cmd/sysmon

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# cinnamon pulls in muffin, cjs and the applet machinery. mesa-utils and the
# software rasteriser let muffin composite without a GPU. gjs is the JavaScript
# runtime the applet executes in; gir1.2-soup-3.0 is the HTTP binding it uses.
# libglib2.0-bin supplies gdbus and gsettings, which is how the tests drive
# Cinnamon.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        cinnamon \
        cinnamon-common \
        curl \
        dbus-x11 \
        dconf-gsettings-backend \
        gir1.2-soup-3.0 \
        gjs \
        libgl1-mesa-dri \
        libglib2.0-bin \
        mesa-utils \
        procps \
        xvfb \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/sysmon-server /usr/local/bin/sysmon-server

# Install the applet exactly as `make install-extension-cinnamon` would, so the
# test exercises the real layout including the shared modules under lib/.
COPY extensions/cinnamon/sysmon-go@sysmon/ \
     /root/.local/share/cinnamon/applets/sysmon-go@sysmon/
COPY extensions/shared/ \
     /root/.local/share/cinnamon/applets/sysmon-go@sysmon/

COPY test/desktop/run-cinnamon-tests.sh /usr/local/bin/run-cinnamon-tests.sh
RUN chmod +x /usr/local/bin/run-cinnamon-tests.sh

# Software rendering: there is no GPU in the container.
ENV LIBGL_ALWAYS_SOFTWARE=1
ENV GALLIUM_DRIVER=llvmpipe
ENV DISPLAY=:99

# muffin refuses to start with "Unsupported session type" unless the session is
# declared. There is no logind seat in a container, so state it explicitly.
ENV XDG_SESSION_TYPE=x11
ENV XDG_CURRENT_DESKTOP=X-Cinnamon
ENV GDK_BACKEND=x11
ENV XDG_RUNTIME_DIR=/run/user/0

CMD ["/usr/local/bin/run-cinnamon-tests.sh"]
