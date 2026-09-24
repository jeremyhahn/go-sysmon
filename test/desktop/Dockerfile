# Tier 3 desktop tests: a real Cinnamon session on a headless Xorg display.
#
# Cinnamon needs a display, a session bus and a GL implementation. Xorg with the
# dummy video driver and llvmpipe supply the first and last; dbus-launch
# supplies the second at run time. This is heavy and GL-in-a-container is not perfectly reliable, which is
# why the target is nightly rather than part of `make ci`.

# The server is built in its own stage so the Cinnamon image does not carry a Go
# toolchain and a C compiler. The Go version must satisfy the "go" directive in
# go.mod, otherwise the toolchain has to be downloaded on every build.
FROM golang:1.27.1-trixie AS builder

WORKDIR /src

# Cache module downloads separately from the source copy so rebuilds only
# re-download when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO stays enabled: pkg/collector imports NVIDIA go-nvml, which does not
# compile without it.
RUN CGO_ENABLED=1 go build -o /out/sysmon-server ./cmd/sysmon

FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive

# cinnamon pulls in muffin, cjs and the applet machinery. mesa-utils and the
# software rasteriser let muffin composite without a GPU. gjs is the JavaScript
# runtime the applet executes in; gir1.2-soup-3.0 is the HTTP binding it uses.
# libglib2.0-bin supplies gdbus and gsettings, which is how the tests drive
# Cinnamon.
# xserver-xorg-core and xserver-xorg-video-dummy are the display: unlike Xvfb,
# the dummy driver gives the screen a mode with real timings (see
# xorg-dummy.conf). x11-xserver-utils supplies xrandr, which the runner uses to
# prove the refresh rate is real before starting Cinnamon.
# udisks2 is the system bus service Cinnamon reaches at startup through GVFS's
# volume monitor. The runner starts it on a real system bus; without it
# Cinnamon logs a warning, and the runner fails on any warning.
# switcheroo-control is deliberately NOT installed (owner ruling 2026-09-23).
# What it reports depends on the host's GPUs, and with them masked XApp takes
# a buggy no-default-GPU path. Without it XApp takes its designed fallback;
# see run-cinnamon-tests.sh.
# whoopsie is here only for its system user. cinnamon hard-depends on
# network-manager-gnome, so NetworkManager's bus policy is installed, and it
# grants a "whoopsie" user; without that user dbus-daemon --system reports
# 'Unknown username "whoopsie"' on every start, which the runner fails on. The
# daemon itself is never started.
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
        udisks2 \
        whoopsie \
        x11-xserver-utils \
        xserver-xorg-core \
        xserver-xorg-video-dummy \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/sysmon-server /usr/local/bin/sysmon-server

# Install the applet exactly as `make install-extension-cinnamon` would, so the
# test exercises the real layout including the shared modules under lib/.
COPY extensions/cinnamon/sysmon-go@sysmon/ \
     /root/.local/share/cinnamon/applets/sysmon-go@sysmon/
COPY extensions/shared/ \
     /root/.local/share/cinnamon/applets/sysmon-go@sysmon/

COPY test/desktop/xorg-dummy.conf /etc/X11/xorg-dummy.conf
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
