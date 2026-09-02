# Desktop GUI

A Wails v2 application wrapping the same web frontend the server ships. Same
data as the CLI, same data as the dashboard — one codebase, three front doors.

## Build tags

GUI code compiles only under the `desktop` tag. Stubs stand in otherwise, so
the server build has no reference to GTK or WebKit at all:

| File              | Tag        | Purpose                            |
|-------------------|------------|------------------------------------|
| `gui_desktop.go`  | `desktop`  | Wails setup and lifecycle          |
| `gui_stub.go`     | `!desktop` | Returns `GUIUnavailableError`      |
| `emit_desktop.go` | `desktop`  | Wails event emitter                |
| `emit_stub.go`    | `!desktop` | No-op emitter                      |

This split is the whole reason there are two binaries. The dynamic linker
resolves GTK and WebKit before `main()` runs, so a desktop binary on a headless
host dies with a loader error that no Go code of ours can catch and explain.
The server build links three shared libraries and runs anywhere.

## Lifecycle

1. `launchGUI()` builds a `SystemCollector` and a `Monitor`.
2. A `MonitorBinding` is bound into Wails.
3. Wails calls `OnStartup`, which stores the Wails context, starts the monitor,
   launches `streamToFrontend` in a goroutine, and starts the tray unless
   `--no-tray` was passed.
4. `streamToFrontend` subscribes and emits a `sysmon:snapshot` event per frame.
5. `OnBeforeClose` hides the window rather than quitting, when the tray is up.
6. `OnShutdown` stops the tray, then the monitor.

## Bindings

| Method            | What it does                                     |
|-------------------|--------------------------------------------------|
| `GetSnapshot()`   | One snapshot, on demand                          |
| `GetVersion()`    | Version, commit and build date                   |
| `SetInterval(ms)` | Changes the rate and restarts the monitor        |

`SetInterval` replaces the monitor rather than mutating it, which means the
binding's pointer to it changes while the streaming goroutine is reading it.
That is guarded by an `RWMutex` — one of two real data races the race detector
caught after this shipped.

## Assets

Built from `cmd/sysmon/frontend/` and embedded at compile time via
`//go:embed frontend/dist`. The window is 1400x900 and titled "System Monitor".

Because the embed directive needs `dist/` to exist in a fresh clone, the build
output is committed rather than ignored. Two consequences follow, and both have
already caused bugs:

- Tailwind's automatic source detection skips gitignored paths, so a tracked
  `dist/` would otherwise be scanned as source and turn class-like strings in
  the last minified bundle into real utilities in the next one. `src/app.css`
  excludes it with `@source not "../dist"`. Without that the CSS grows on every
  rebuild and a clean build and an incremental build disagree on content hashes.
- A change to the frontend is not complete until `dist/` is rebuilt and
  committed with it, or the embedded UI and the source drift apart.

## Charts

`src/lib/echarts.ts` is the only module that imports ECharts. It pulls from
`echarts/core` and registers exactly what the dashboard draws — gauge and line
series, the grid and tooltip components, and the canvas renderer. The package's
default entry point registers everything it ships, which costs roughly twice the
bundle for a dashboard that draws two kinds of chart.

Anything added to a chart option later — another series type, a legend, a data
zoom — has to be registered there first, or ECharts silently renders nothing for
it. Vendor code is emitted as its own content-hashed chunk, so redeploying the
app does not invalidate the charting library in returning browsers.

The stock `dark` theme is not used: it ships as a UMD bundle that re-imports the
full ECharts build, which would undo the tree shaking. Instead `chartPalette()`
in the same module returns the surface colours — axes, gridlines, tooltip
chrome, the gauge track — for the current theme, and the chart components read
it rather than spelling hex codes inline. Series colours are not in the palette:
they encode meaning (green healthy, red saturated) and are legible on either
surface.

## Theming

`src/lib/theme.svelte.ts` owns the choice; the control is the `Theme` select in
the navbar. Three modes, not two — `system`, `light`, `dark` — because "follow
the operating system" is a state the user can be in, and a two-way switch has
nowhere to put it: once flipped, there is no way back to following the OS. In
`system` mode the preference is tracked live, so an OS that flips at sunset
flips the dashboard with it.

Two things about it are easy to break:

- **It is applied before first paint.** The bootstrap that sets `data-theme`
  is an inline script in the `<head>` of `index.html`, because a module script
  runs after first paint and would flash the wrong theme on every load. That
  script duplicates the storage key and the resolution rule from
  `theme.svelte.ts`; the two have to change together.
- **Canvases inherit nothing from the stylesheet.** A theme switch repaints
  every daisyUI surface and leaves an ECharts axis exactly as it was, so the
  chart components take the palette as an input and rebuild their option when
  it changes.

`tests/theme.spec.ts` covers both, including the timing of the first
`data-theme` write, which is the part that regresses invisibly: applying the
theme from the app's module script also ends up correct, just one painted frame
later.

## Tables

Every table that can grow without bound uses the shared `DataTable` component,
so search, sorting and pagination behave the same everywhere they appear:
processes, containers, container images, SMART attributes.

A caller supplies columns (each with an optional `sortValue`) and a snippet that
renders one row's cells. The component owns filtering, ordering and paging.
Controls are a search box, a 10/25/50/100 page-size selector, and a
`«  n / m  »` pager with a "Showing 1-25 of 84" range.

Behaviour worth knowing:

- Numeric columns sort largest-first on the first click. For CPU, memory and
  size, that is the end you are looking for.
- Searching or changing the page size resets to page one, and a page that would
  land past the end of a filtered list is clamped.
- The active sort column carries `aria-sort`, so the ordering reaches assistive
  technology and not just the eye.
- Rows the collector dropped upstream are disclosed in the footer, so a
  truncated list is never mistaken for the whole set.

Tables bounded by physical hardware — DIMMs, GPUs, disks, CPU cores, network
interfaces — deliberately do not use it. A search box over four rows costs more
than it returns, and the per-core CPU bars are meant to be read together as a
shape rather than paged through.
