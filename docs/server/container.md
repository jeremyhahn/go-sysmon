# Running in a container

```bash
docker compose up -d      # then open http://localhost:8080/
```

That works, but it is worth understanding what it is doing, because a monitor
in a container has a problem no other containerised service has: containers
exist to hide the host, and this program's entire job is to see it.

## What a naive run gets you

Without host access the container reports the container:

| | Plain `docker run` | With host access |
|---|---|---|
| Processes | **1** | 606 |
| Hostname | `856d52b68c46` | `godlike` |
| Interfaces | the container's veth | all 17, with bridges and bonds |
| Containers | 0 | 7 |
| Core temperatures | 0 | 24 |

Those are real numbers from the same image on the same machine. The CPU list
looks right either way, which makes this easy to miss — `/proc/cpuinfo` is not
namespaced, so you get 24 cores and a process list of 1, and everything
*looks* plausible.

## What each setting buys

| Setting | Without it |
|---|---|
| `pid: host` | Process list is just this container. No VM is discoverable, because guests are identified from the hypervisor's `/proc/<pid>/cmdline` |
| `network_mode: host` | You see the container's veth pair instead of real NICs, bridges, bonds and wifi |
| `-v /sys:/sys:ro` | No CPU topology or frequencies, no hwmon temperatures, no fans, no thermal zones, no RAPL power, no disk queue stats |
| `-v /sys/fs/cgroup:/sys/fs/cgroup:ro` | No container metrics. **This is a separate mount from `/sys`** — mounting `/sys` alone is not enough, which is the easiest thing here to get wrong |
| `-v /etc/os-release:ro` | Distribution shows as Debian (the image), not the host |
| `-v /var/run/docker.sock:ro` | No image inventory. Still only queried with `--docker`, since the socket grants control of the daemon |

Run `docker compose exec sysmon sysmon-server doctor` at any time and it will
tell you exactly what the current settings allow.

## SMART needs privileged, and capabilities are not enough

This was measured on a 6-NVMe host, not inferred:

| Configuration | Disks reporting SMART |
|---|---|
| `--cap-add SYS_RAWIO --device=/dev/nvme0n1` | **0 of 6** |
| `--cap-add SYS_ADMIN --cap-add SYS_RAWIO --device=...` | **0 of 6** |
| `--privileged` | **6 of 6** |

The SMART ioctl needs unrestricted access to the block device; granting the
device plus the obvious capabilities does not get there. So it is privileged or
nothing.

Worth noting the flip side: the container running privileged reads SMART on all
six disks, where the *host* binary running as an ordinary user reads none.
Containerising can give you **more** hardware visibility than running it
directly, not less.

Whether that trade is right depends on the machine. Privileged is close to root
on the host. Where you already trust what runs there, it is reasonable. On a
shared host it is not, and losing disk health is the better outcome. The
compose file ships **unprivileged** and documents the one-line change.

## At that point, why containerise at all?

Fair question. With `pid: host`, `network_mode: host` and `/sys` mounted, the
isolation is mostly gone — you are getting packaging, not sandboxing.

That is still worth having, and it is exactly how node_exporter, cAdvisor and
netdata ship. `docker compose up -d` beats copying a binary and hand-writing a
systemd unit, it version-pins cleanly, it drops into a Kubernetes DaemonSet,
and it upgrades by changing a tag.

If you want isolation rather than convenience, run the binary directly under
systemd with `ProtectSystem=strict` and a `SupplementaryGroups=disk` — see
[deployment.md](deployment.md). That gives real confinement, which the
container arrangement above does not pretend to.

## Ports

`network_mode: host` means the container binds the host's network directly, so
there is no port mapping — `--addr` decides the port and a `ports:` block would
do nothing. If something already holds 8080 the container fails to start with
`bind: address already in use`; change the `command:` rather than adding a
mapping.

## The image

- `ghcr.io/jeremyhahn/go-sysmon:0.1.0`, and `:latest`
- linux/amd64 and linux/arm64
- ~100 MB, Debian-based

Debian rather than Alpine because CGO is mandatory — `pkg/collector` imports
NVIDIA's `go-nvml`, which does not build with `CGO_ENABLED=0` — so the binary
links glibc and a musl base would need a separate toolchain for no real gain.

Only the server build is containerised. The desktop build links GTK and WebKit
for a native window, which is meaningless without a display and would add
around 120 shared libraries.

## Kubernetes

The same requirements apply, as a DaemonSet:

```yaml
spec:
  hostPID: true
  hostNetwork: true
  containers:
    - name: sysmon
      image: ghcr.io/jeremyhahn/go-sysmon:0.1.0
      args: ["serve", "--addr", ":8080"]
      securityContext:
        privileged: true     # only if you want SMART; see above
      volumeMounts:
        - { name: sys,    mountPath: /sys,          readOnly: true }
        - { name: cgroup, mountPath: /sys/fs/cgroup, readOnly: true }
  volumes:
    - { name: sys,    hostPath: { path: /sys } }
    - { name: cgroup, hostPath: { path: /sys/fs/cgroup } }
```

## Building it yourself

```bash
docker build -t go-sysmon:dev --build-arg VERSION=dev .
```

The Dockerfile builds the frontend and the binary from source in separate
stages, so it needs no prebuilt artifacts.

## Security

The dashboard has no authentication, and
`network_mode: host` means it is bound wherever `--addr` says on the real host.
It publishes hostnames, the full process list, disk serial numbers and network
configuration. Bind it to a private interface (`--addr 10.0.0.5:8080`) or put
an authenticating proxy in front of it.
