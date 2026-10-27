# CLI — implementation contract

> **Status: NORMATIVE** · Scope: `cmd/sysmon`, `pkg/cli` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

Design and the rendering split are in [architecture.md](architecture.md); this
page is the contract a change must keep.

## Exported surface

`cmd/sysmon` is `package main` and exports nothing. The public Go surface is
`pkg/cli`, whose render functions all have the shape
`func(w io.Writer, snap *types.Snapshot) error`: `RenderOverview`, `RenderHost`,
`RenderCPU`, `RenderMemory`, `RenderStorage`, `RenderNetwork`, `RenderGPU`,
`RenderContainers`, `RenderVMs`, `RenderImages` (list in
[architecture.md](architecture.md)). Formatting helpers in `pkg/cli/format.go`
are unexported.

## Invariants

- `pkg/cli` never collects; it reads the snapshot it is handed.
- Text and `--json` output come from the same snapshot path, so they cannot
  disagree about what was collected.
- Logs go to stderr (or `--log-file`), never stdout, so `--json` stays pipeable.
- A rate (CPU, network, disk, swap paging) is the difference of two readings.
  One-shot mode takes two readings 500 ms apart (`oneShotSampleInterval`,
  `cmd/sysmon/snapshot.go`) and prints the second. A rate that could not be
  measured prints `-`, never `0`. The second reading also waits for the
  drive-slot poll the first one started (`WaitDriveSlots`, bounded at two
  minutes), so it carries the drive bays.
- `--refresh` builds one `SystemCollector` with tiering off, so every frame is
  re-read, and sleeps for the interval minus the work done, until `SIGINT`.
- The first write error is remembered by `errWriter` and returned once at the
  end of the render, so `sysmon storage | head -5` exits non-zero.
- `serve` installs its `SIGINT`/`SIGTERM` handler before starting anything or
  binding the socket (#106), so a stop during startup shuts down cleanly.

## Error semantics

| Situation | Behaviour |
|---|---|
| Any command returns an error | `main` prints `Error: <message>` to stderr and exits 1 |
| Log file cannot be closed | The same report, exit 1, even if the command succeeded |
| `serve --interval` ≤ 0 | `types.InvalidIntervalError` |
| `serve` config or TLS pair unusable | The error from the config manager / `server.NewWithConfig` (`types.TLSConfigError`) |
| `sysmon` with a display but a non-`desktop` build | `launchGUI` returns `types.GUIUnavailableError`; the root command falls back to `overview`. Any other GUI error is returned |

Every command exits 0 on success and 1 on error; there are no other exit codes.

## Configuration

Global flags and the `serve` flags are listed in [cli.md](cli.md). `serve`
reads its defaults from `config.Defaults()` so the flag, the file and the
environment layers share one copy of each default. Precedence for `serve`
settings is flag, then `SYSMON_*` environment, then the YAML file
(`/etc/go-sysmon/sysmon.yaml`, or `--config`), then defaults — see
[server/engineering.md](../server/engineering.md#configuration).

## Test strategy

- `pkg/cli` is tested against fixture snapshots with no hardware
  (`renderer_test.go`, `virt_renderer_test.go`, `format_test.go`,
  `errwriter_test.go`).
- `cmd/sysmon/*_test.go` drive each command through Cobra with a fake
  collector; the `gui_stub`/`tray_stub` files have their own tests for the
  non-desktop build.
- `make test-cli` and `make test-cmd-sysmon` run one package each;
  `make integration-test-cli` and `make integration-test-cmd-sysmon` run the
  integration suite in the devcontainer
  ([testing/integration.md](../testing/integration.md)).

## Observability

Structured `slog` records on stderr; level and format set by `--log-level` and
`--log-format`, destination by `--log-file`. `serve` logs `starting sysmon web
server` and, once bound, the address actually in use.
