# CLI architecture

Two packages, split along a deliberate line:

- `cmd/sysmon/` — Cobra commands, flags, mode selection
- `pkg/cli/` — rendering, and nothing else

`pkg/cli` never collects. Every render function takes an `io.Writer` and a
`*types.Snapshot` and returns an error, which means the whole output layer is
testable against a fixture snapshot without touching hardware.

| Function            | Renders                                             |
|---------------------|-----------------------------------------------------|
| `RenderOverview`    | Everything, summarised                              |
| `RenderHost`        | Hostname, OS, kernel, BIOS, board                   |
| `RenderCPU`         | Topology, per-core usage, temperatures, voltages    |
| `RenderMemory`      | RAM and swap, plus the DIMM table                   |
| `RenderStorage`     | Per-disk detail with SMART and NVMe health          |
| `RenderNetwork`     | Per-interface detail with traffic                   |
| `RenderGPU`         | Per-GPU detail with utilisation bars                |
| `RenderContainers`  | Containers, plus the attention block                |
| `RenderVMs`         | Guests, their disks and their host NICs             |
| `RenderImages`      | Image inventory and reclaimable space               |

## Error propagation

Rendering writes a lot of lines, and checking every `Fprintf` would bury the
layout in error handling. The package uses the `errWriter` pattern from
Effective Go instead: a wrapper that records the first failure and turns
subsequent writes into no-ops. The renderer checks once, at the end.

This is not pedantry. `sysmon storage | head -5` closes the pipe partway
through, and without propagation the command exits 0 having silently failed to
print most of its output.

## Formatting helpers

In `pkg/cli/format.go`:

- `formatBytes` — B, KB, MB, GB, TB
- `formatDuration` — seconds to `Xd Xh Xm`
- `formatPercent` — a float to `X.X%`
- `formatBar` — an ASCII bar out of block characters
- `formatHours` — hours to years and days, for power-on time
- `formatDataUnits` — NVMe data units, which are 512KB each, to something readable
- `formatTable` — column-aligned output with widths measured from the content

## JSON mode

`--json` skips the renderer and encodes the snapshot straight to stdout. Both
modes go through the same `collectSnapshot()` path, so the JSON and the text
can never disagree about what was collected.

Logs go to stderr precisely so this works: `sysmon cpu --json | jq` stays clean
no matter how much the collector has to say.

## Refresh mode

`runWithRefresh` wraps any render function. With `--refresh` set it:

1. Builds one `SystemCollector` with tiering disabled, so every frame is fresh
2. Clears the terminal with `\033[H\033[2J`
3. Collects and renders
4. Sleeps for the interval *minus* what collection and rendering took, so the
   frame rate is the rate you asked for rather than that plus the work
5. Exits on `SIGINT`
