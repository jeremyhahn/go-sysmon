# Integration tests

The integration suites run the real binaries end to end, the way an operator
would. Their code lives under `test/integration/`, behind the `integration`
build tag, one package per area:

| Package | What it runs |
|---|---|
| `test/integration/harness` | Shared: builds every binary once per suite, runs commands, starts `sysmon serve` on an ephemeral port and follows its JSON log, stops it with SIGTERM and requires a clean exit |
| `test/integration/cli` | Every CLI subcommand, and the server's snapshot, interval and event-stream endpoints |
| `test/integration/fleet` | Fleets of real `sysmon serve` processes and stand-in peers: dead, slow, misbehaving and TLS peers, trees, loops, reloads, drill-ins and paging |
| `test/integration/config` | Configs refused at startup, flag/env/file layering, SIGHUP and file-watch reloads (ConfigMap swaps included), logging, systemd notify and watchdog, shutdown, memory limits from real cgroups |
| `test/integration/server` | Every API endpoint, security headers, static TLS, a reverse proxy, SSE in whole and delta form (decoded with `pkg/delta`), alerts, shutdown, and `pkg/server` embedded as a library |
| `test/integration/prom` | The `prometheus,nvml` build against a Prometheus stand-in: remote write, history queries, lookback, the history cadence watch, `/metrics` |
| `test/integration/tools` | `checklatest`, `checkpins`, `buildtags` and `tsgen` against stand-ins for every release source, a Go module proxy, git mirrors and Gitea |
| `test/integration/collector` | A Docker API stand-in, real qemu and Firecracker-named processes, real container cgroups, network devices in a private namespace, install detection, `sysmon doctor` |

Each suite's `TestMain` is `os.Exit(harness.Main(m))`. The harness builds the
default `sysmon`, the `prometheus,nvml` server build and the development tools.
Under `scripts/pkgtest.sh integration` (`SYSMON_COVERDIR` set), each binary is
built with every package of the module instrumented, and every process the
suites start writes its coverage into one directory
([coverage](coverage.md)).

External services are never reached. A suite that needs one runs an
in-process stand-in for it.

## One environment: the devcontainer

The suite runs in one place only, the devcontainer built from
`build/docker/devcontainer/Dockerfile`. It never runs on the host.

- The image carries the pinned Go toolchain (the `FROM` line matches `go.mod`,
  which `make check-pins` enforces).
- It also carries the tools the collectors shell out to, `smartmontools` and
  `dmidecode`.
- The container runs `--privileged`, so the collectors see real devices, and
  nothing the suite does can reach the host.
- Its entrypoint (`build/docker/devcontainer/entrypoint.sh`) delegates the cpu,
  io, memory and pids controllers inside the container's own cgroup namespace,
  so the collector suite can create real container and VM cgroups.
- `SYSMON_DEVCONTAINER=1` is set in the image. A test that changes
  container-wide state (a socket at `/run/docker.sock`, cgroups, network
  devices) fails with the reason unless it is set.

| Target | What it does |
|---|---|
| `make integration-test` | Builds the devcontainer, runs the whole suite in it, and reports every package's integration coverage |
| `make integration-test-<pkg>` | The same run, holding one package's integration coverage to the 90% floor ([coverage](coverage.md)) |
| `make docker-build` | Builds the devcontainer image only |

To work on one suite, name it in `INTEGRATION_SUITES`, and set
`INTEGRATION_PROFILE` to keep the merged coverage profile, read file by file
with `go tool cover -func`:

```sh
docker build -t go-sysmon-test-config -f build/docker/devcontainer/Dockerfile .
docker run --rm --privileged -v "$PWD/out:/out" \
    -e INTEGRATION_SUITES=./test/integration/config -e INTEGRATION_PROFILE=/out/profile.txt \
    go-sysmon-test-config ./scripts/pkgtest.sh integration 0
```

`DOCKER_IMAGE` names the image (default `go-sysmon-test`). Override it when
several checkouts share one Docker daemon, so one does not run the image
another just built:

```sh
make integration-test DOCKER_IMAGE=go-sysmon-test-mybranch
```

CI's Integration Tests job runs `make integration-test`, so a local run and
the CI run use the same image. The suite's output is kept in
`test-logs/integration.log` ([CI failure artifacts](ci-artifacts.md)).
