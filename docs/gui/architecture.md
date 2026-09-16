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
| `SetInterval(ms)` | Retunes the running monitor                      |

`SetInterval` adjusts the cadence in place. It used to **replace** the monitor —
stop it, build a new one, start it — which meant the same control also discarded
every reading the monitor had accumulated, and the binding's pointer changed
while the streaming goroutine was reading it (guarded by an `RWMutex`, one of
two real data races the race detector caught after this shipped).

The server had always retuned in place, so one control meant two different
things depending on which build was running, and only the desktop's version
cleared the charts. **A control named "polling interval" that also restarts
collection is a second, unnamed action** (#97). The `RWMutex` remains for `ctx`
and the stream's cancel function; the monitor pointer no longer changes.

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
  committed with it, or the embedded UI and the source drift apart. **This is
  not hypothetical and the rule was already written when it happened**: on
  2026-09-03 #89 changed `src/` without rebuilding, and for a day `main` carried
  a bundle that did not contain the fleet card it had just merged -- a binary
  built with a bare `go build` served a UI one commit behind, with green CI and
  a correct source tree (#55). A rule nothing checks is a rule that holds until
  someone is busy.
  **Something checks it now** (#129): `make check-bundle-current` builds the
  frontend to a scratch directory and compares it against the committed copy,
  and CI runs it as its own **Bundle Currency** job, which `CI Passed` gates on.
  It happened a third time before the gate existed -- `main` served a dashboard
  built before #130's PSI card and #132's swap types.

  Two properties worth knowing. It builds to a scratch directory rather than in
  place, because a check that rebuilds the committed copy destroys the evidence
  it was meant to report and leaves a dirty tree on the machine where the answer
  was "yes". And it compares **output**, not timestamps: a whitespace-only change
  to a Svelte template compiles to the identical bundle and is correctly not
  flagged, because the question is whether the embedded UI matches what the
  source compiles to -- not whether someone remembered to run a command.

  It is deliberately NOT part of `make lint`: it needs the node toolchain, and
  requiring node in order to lint Go would reintroduce the dependency the
  committed bundle exists to remove.
- The application chunk and its stylesheet are emitted under **fixed** names,
  `assets/index.js` and `assets/index.css`, not content-hashed ones. A hash
  renamed a *committed* file on every build, so any two branches touching the
  frontend collided on three paths at once — add/add on their new bundles,
  delete/delete on the one they both replaced, modify/modify on `index.html` —
  and none of it was resolvable by merging (#55). With fixed names `index.html`
  changes only when its own source or the vendor chunk does, and the bundle is
  a single conflict on one file whose resolution is always "rebuild". Cache
  correctness moves to the server: see `assetHeaders` in
  `pkg/server/assets.go`, and *Static assets* in
  [../server/architecture.md](../server/architecture.md).

  Two builds of an unchanged tree are byte-identical, so a rebuild you did not
  need produces no diff at all.

## Charts

`src/lib/echarts.ts` is the only module that imports ECharts. It pulls from
`echarts/core` and registers exactly what the dashboard draws — gauge and line
series, the grid and tooltip components, and the canvas renderer. The package's
default entry point registers everything it ships, which costs roughly twice the
bundle for a dashboard that draws two kinds of chart.

Anything added to a chart option later — another series type, a legend, a data
zoom — has to be registered there first, or ECharts silently renders nothing for
it. Vendor code is emitted as its own content-hashed chunk — the only remaining
hashed name in the bundle — so redeploying the app does not invalidate the
charting library in returning browsers, and it can still be served `immutable`.

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

## The date range

`src/stores/history.svelte.ts` holds the selected range for the whole
dashboard, and `MetricChart` is the only component that knows there are two
modes. A panel declares what its chart IS — which catalogued metrics its series
correspond to, and which device or interface it is drawing — and never which
mode it is in.

That is deliberate. Eight panels each holding their own range would not be
comparable with each other, and would put state into the real-time path that it
does not need: with no range selected, `MetricChart` renders exactly the
`TimeSeriesChart` it always did.

One invariant holds the control together: **whenever a range is applied, the
picker's inputs describe that range.** Choosing `Custom…` therefore carries the
applied window into the inputs and leaves it applied — nothing moves, and the
control, the banner and the charts all say the same thing. Choosing it with
nothing applied opens the inputs on the last hour and stays live, because
`Custom…` is the state of specifying a window rather than a request for one
nobody has typed. Editing the pair into something that is not a window — an
emptied field, an end before its start — **un-applies whatever was applied**:
the charts go back to live and the picker says why. Breaking that invariant has
produced a bug in each of its three directions: applying a seeded window moved
the dashboard without being asked; leaving a preset applied under a `Custom`
label had it claim to have moved when it had not; and holding an applied window
through the edit that invalidated it left the charts drawing 24 hours that no
control on the page still described.

The banner says two things: that the charts are not live, and what the rest of
the page is showing instead — because a gauge has no historical value to
display, so it keeps rendering whatever the dashboard last received. That second
sentence **follows the actual freshness** rather than asserting it. It read
"still show live values" flatly, which is false in two states this dashboard
already reports elsewhere: a dropped stream, where every panel is frozen on its
last frame, and an unreachable peer, whose last snapshot keeps rendering under a
`stale` badge. A banner whose purpose is to stop a chart being mistaken for live
printing "live" beside data that is not is the failure it exists to prevent,
self-inflicted.

**One question, asked in one place.** `monitor.showingLiveData` answers "is what
is on screen current?" for the whole dashboard, and both the Overview's
Live/Disconnected badge and this banner consult it. They did not: the banner was
made selection-aware while the badge still read the local stream, so a reachable
peer showed "Disconnected" whenever that stream was down and an unreachable one
showed "Live" whenever it was up — two answers to one question, on one screen.

**Which fact settles it depends on the selection** — on `selectedNode`, not on
whether `peerFreshness` happens to be null, since selecting a peer clears the
freshness and leaves it null until the first poll returns. A selected peer with
nothing back yet is not live, and neither is one whose poll **fails outright** —
a non-2xx from the aggregator, a refused connection, a body that is not JSON.
That last case used to update nothing at all, so the last successful reading
kept its `reachable: true`, the `stale` badge never appeared, and the page
presented a frozen snapshot as live in exactly the state the badge exists for. Combining the two facts is wrong in both
directions. This host's panels are fed by the event stream, so `connected`
is the question. A selected peer's are refreshed by an independent HTTP poll,
so the stream being down says nothing about them — requiring both would tell an
operator their reachable peer's gauges were stale while they updated in front
of them, which is the same defect inverted.

Every chart heading carries the window it draws — `CPU Usage (60s)` — and that
wording comes from `history.windowLabel` rather than from each panel. It was
written when sixty seconds was the only answer, and left fixed it became a
thirty-day series under a heading claiming a minute: a specific wrong answer,
which reads worse than none because nothing invites doubt. Sixteen headings
share the one source so they cannot drift from each other or from what is
applied. An explicit window says `custom range` rather than spelling out two
dates in a heading; the dates are already in the picker and the banner.

A range or node change **aborts** the requests it makes stale, rather than only
discarding what they return. Discarding the answer was enough for correctness —
nothing wrong was ever rendered — but the request carried on: the server went
on running a Prometheus range query for a window nobody is looking at, and the
browser's per-origin connection limit stayed occupied by it. On a panel with
nine charts, two quick range changes left the requests for what is actually on
screen queued behind eighteen abandoned ones, and the charts sat on "Loading"
for a reason that had nothing to do with Prometheus.

**Every line or none, and that is about the RESULT rather than the requests.**
A chart built from more than one metric — disk read and write, network in and
out — waits for all of them to settle, and then requires each to have actually
contributed a line. Only the first half was enforced: a metric that returned no
series matching the chart's device drew nothing while its sibling drew normally,
producing a one-line chart under a heading naming two, with nothing on the page
to say the second line was missing rather than flat. A partial chart says so
instead; a window with nothing in it at all keeps its own separate message,
because "no history here" and "half a chart" are different facts.

Charts ask for what they need through `history.need()` rather than fetching, so
a metric wanted by nine disk cards is fetched once. `history.get()` returns what
arrived. The store discards everything it holds whenever the window or the node
changes, and **replaces the range object when it does** — that replacement is
the single thing that makes every chart's effect re-run and ask again. Without
it the data would be cleared and never re-fetched, and every chart would sit on
"Loading…" looking exactly like a slow Prometheus.

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
