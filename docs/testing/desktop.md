# Testing the desktop extensions

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of the desktop extension tests.

A panel applet cannot be tested the way a Go package can: it runs inside a
compositor, against a JavaScript binding to a C toolkit, on a display. Testing
it is a question of how much of that stack you are willing to stand up.

The answer here is three tiers. The first two are fast and hermetic and run on
every commit. The third stands up a real Cinnamon session and runs nightly.

| Tier | What it proves                                  | Cost      | Where it runs |
|------|-------------------------------------------------|-----------|---------------|
| 1    | Formatting, parsing and the section model        | ~50ms     | `make ci`     |
| 2    | `applet.js` loads and behaves against a stand-in platform | ~50ms | `make ci` |
| 3    | Cinnamon really loads and drives the applet      | minutes   | nightly       |

## Tier 1 — pure logic

```bash
make test-extensions
```

Everything that does not need a desktop lives in `extensions/shared/`: byte and
rate formatting, snapshot aggregation, the server-sent events parser, and the
popup's section model. These are plain CommonJS modules with no GObject
Introspection imports, so `node --test` runs them directly.

This is where most of the value is. The bugs that actually reach users are
things like a rate formatted in the wrong unit, a field that renders as
`undefined` on hardware that does not report it, or a stream frame parsed
incorrectly — and every one of those is reachable here.

The section tests double as the parity tests. `sections.test.js` asserts that
each subsystem the CLI reports has a section, that the rows carrying the
debugging detail are present, and that no value ever renders as `undefined`,
`null` or `NaN`. A field silently dropped from the popup is exactly the
regression a screenshot cannot catch.

## Tier 2 — the applet against a stand-in platform

Same command; the tests live in `extensions/tests/applet.test.js`.

`applet.js` is GObject Introspection glue, so its failure mode is a typo or a
misused API — which normally only appears when Cinnamon loads it, in
`~/.xsession-errors`, after a restart.

`extensions/tests/harness/cinnamon.js` supplies stand-ins for `St`, `Soup`,
`Gio`, `GLib`, `PopupMenu`, `Mainloop` and `AppletSettings`, then executes the
real `applet.js` against them with Node's `vm` module. Every line of the applet
runs for real; only the toolkit underneath is substituted.

That is enough to drive the things that matter:

- the applet constructs and opens the stream
- a snapshot populates the panel and the tooltip, and all ten popup sections once the popup opens
- a second snapshot refreshes the menu instead of duplicating it
- malformed data is ignored rather than crashing the applet
- end of stream, a non-200 response, a failed connect and a `bye` event each
  schedule the right reconnect
- backoff grows against a server that never delivers, and resets when data does
- removal from the panel cancels timers and closes the stream

The harness also models Clutter's paint rule. Its drawing area has no natural
width, takes its height from the panel box, and `repaint()` does nothing while
either dimension is zero. The applet used to set its width only inside its own
repaint handler, and Clutter never paints a zero-width actor, so on a real
desktop the gauge was never drawn (#184). The fake used to call the handler
whatever the size, so no test saw it. The applet now sets the width from the
gauge's segments whenever the snapshot or a gauge setting changes, before any
paint.

It also models the main loop's idle sources, with their priorities, because
when the applet does its work matters as much as what it does. The applet used
to apply each snapshot inside the stream's read callback, at GLib's default
priority. Clutter paints at `CLUTTER_PRIORITY_REDRAW` (`G_PRIORITY_HIGH_IDLE +
50`), a lower one, so on a host that took longer than the interval to apply a
snapshot the main loop never got to a frame, and the whole desktop stopped
painting (#232). The tests now require a snapshot to be applied from an idle
source below that priority, only the newest of several to be applied, and the
last snapshot of an ended stream to be shown before the disconnect.

The harness menus follow Cinnamon's where the applet depends on them: menus
construct through `_init`, `toggle()` goes through `open()`, `open()` records
what the menu held as it opened (Cinnamon sizes the popup there), and an item's
`destroy()` removes it from its menu. The popup tests require a closed popup to
be left alone as snapshots arrive, an opening one to be filled first, and an
open one to update its rows in place (#233).

The harness emulates one thing carefully: Cinnamon's applet base classes come
from GJS's legacy class system, where construction dispatches to `_init` rather
than to a `constructor`. Subclasses rely on that, so the stand-in does the same.
Getting this wrong is why the first version of the harness loaded the applet
without ever running its initialiser.

These are test doubles for a desktop toolkit that cannot run headless, not
stand-ins for go-sysmon's own code.

## Tier 3 — a real Cinnamon session

```bash
make test-extensions-desktop
```

Builds `test/desktop/Dockerfile` and runs `test/desktop/run-cinnamon-tests.sh`
inside it. The container starts a real `sysmon serve`, a headless Xorg display (the dummy video driver), a D-Bus
session bus and Cinnamon itself under llvmpipe, with the applet installed
exactly as `make install-extension-cinnamon` would install it.

Assertions go through the **`org.Cinnamon` D-Bus `Eval` method**, which runs
JavaScript inside the running Cinnamon process and returns the result:

```bash
gdbus call --session \
    --dest org.Cinnamon \
    --object-path /org/Cinnamon \
    --method org.Cinnamon.Eval \
    "String(applet.snapshot !== null)"
```

This is not a novel trick — published applets in `cinnamon-spices-applets` use
D-Bus Eval assertions as regression locks.

What only this tier can prove:

- Cinnamon's `require()` resolves the shared modules
- the applet takes its settings from its settings file, and the server ends up
  polling at the interval set there. The runner writes the file where Cinnamon
  6.4 reads it, `~/.config/cinnamon/spices/<uuid>/<uuid>.json` (a
  single-instance applet's file is named after its UUID), with a 500 ms
  interval, and starts the server at 2000 ms. The schema default is 1000 ms,
  so the server reports 500 ms only if Cinnamon read the file and the applet
  pushed what it read. The runner used to write `0.json`, which Cinnamon never
  read, and the applet quietly ran on its defaults (#234)
- the GObject Introspection calls are real, not just plausible
- the popup builds against the actual `PopupMenu` implementation, opened by a
  click as a user would open it
- a closed popup is left alone while snapshots arrive: the same row actors and
  no text change on any of them. Opening it then brings it up to date before
  Cinnamon lays it out; the runner marks a row stale and checks it was
  rewritten by the time `open-state-changed` fired (#233)
- the panel gauge gets a non-zero allocation and Clutter actually paints it
- the gauge keeps being painted while every snapshot takes twice the interval
  to apply. The runner slows the applet's popup rebuild down on purpose, so the
  starved frame clock of #232 is produced on every run rather than once in
  fifty on a loaded CI runner. Each paint check logs the gauge's paints, the
  stage's frames, and whether the gauge is mapped and visible, so a failure
  says whether Clutter painted nothing or painted around the gauge.
- the applet reaches a real server over a real event stream

### The session gate

No applet assertion may pass until the run has proven the desktop is there to
be asserted on. Every setup step either proves it worked or ends the run: the
server has to answer, Xorg has to report its display ready (`-displayfd`, not a
sleep), RandR has to report a real refresh rate for the current mode, and
`gsettings` has to enable the applet. After that, in order:

1. Cinnamon finished starting: `Main.runState` is `RUNNING`, which Cinnamon
   sets only after `AppletManager.init()` has resolved
2. mutter accepted the display mode: the log has no `Invalid mode` or `(-nan)`
   line
3. the panel exists
4. the sysmon applet is in the loaded set
5. `Main.xlet_startup_error` is false
6. the log says `Loaded applet sysmon-go@sysmon` and has no error tagged with
   the UUID
7. the log has no warning or error of any kind: GLib `WARNING`/`CRITICAL`/`ERROR`,
   GJS `JS WARNING`/`JS ERROR`, LookingGlass error or warning, or Xlib. The
   same check runs again over the whole log after the applet checks.

Two lines are exempt. The first, by owner ruling (2026-09-22), is matched as a whole
line, byte for byte, display included:

```
Xlib:  extension "DPMS" missing on display ":99".
```

muffin calls `DPMSCapable()` unconditionally
(`meta-monitor-manager-xrandr.c:150` in muffin 6.0.1), and libXext prints this
line when the server lacks DPMS. Xorg only registers DPMS when a screen's
driver implements it (`Xext/dpms.c`, `DPMSSupported()`). No headless server
that runs in a container does: not xf86-video-dummy (up to its latest source),
not Xvfb, not Xvnc. Getting it would take a kernel DRM device such as vkms on
the host. Any other Xlib line, this line on another display, or a warning next
to it still fails the run; the runner tests pin each case.

The second, XApp's fallback when switcheroo-control is absent, is described
under "GPUs" below.

The system bus's own stderr is held to the same standard: anything
`dbus-daemon --system` prints at startup aborts the run. cinnamon hard-depends
on network-manager-gnome, and NetworkManager's bus policy names a `whoopsie`
user. So the image installs `whoopsie` for its system user (the daemon is
never started), which removes the bus's `Unknown username "whoopsie"` warning
at its source.

Each step runs only if the one before it passed. When one fails, every later
check prints as `n/a ... (not run)` instead of being evaluated, and the run
exits non-zero even if the counters say otherwise. A desktop that never came up
reports one failure and a list of checks that were never made.

This replaced a one-shot check made as soon as Cinnamon answered D-Bus. Eval
answers before the panel exists and before applets load, so that check raced
startup. Job 42277 (2026-09-03) lost the race by 55 ms: "an applet is
registered" failed, Cinnamon logged "Loaded applet" right after, and every later
check passed. Waiting for Cinnamon's own run state removes the race.

`extensions/tests/desktop-runner.test.js` runs the real script against
stand-in executables for the desktop stack, one scenario per startup shape. It
runs under `make test-extensions`. Healthy, slow and broken starts are covered.
In every broken one the stand-in `gdbus` would pass any applet assertion put to
it, so a single `ok` after a failed gate step fails the test.

### The display: Xorg with the dummy driver, not Xvfb

The suite used to run on Xvfb, and every run logged this:

```
mutter-WARNING: Failed to use linear monitor configuration:
                Invalid mode 1280x1024 (-nan) for monitor 'unknown unknown'
```

Xvfb's only RandR mode has no timings. `xrandr --verbose` shows dot clock,
htotal and vtotal all at 0, so the refresh rate mutter works out is 0/0. mutter
rejected its own layouts and fell back to the X server's, so the desktop still
came up, but on a display mode nobody had configured. The warning did not cause
the 2026-09-03 failure; the startup race above did. Adding a timed mode later
with `xrandr --newmode` does not work either, because Xvfb frees the mode as
soon as the `xrandr` client that created it exits.

`test/desktop/xorg-dummy.conf` runs Xorg with `xserver-xorg-video-dummy`
instead, the standard way to get real mode timings from a headless X server.
Its Modeline is the VESA 1280x1024 @ 60 Hz timing (108 MHz / (1688 x 1066) =
60.02 Hz), so the mode has timings from the moment the server starts. The
runner checks this twice. Before Cinnamon starts, the current mode's RandR
refresh rate must be a positive number. Once Cinnamon is running, the log must
have no `Invalid mode` line. Either one failing fails the run.

### The system bus and its services

At startup Cinnamon reaches udisks2 on the system bus, through GVFS's volume
monitor. A container has no init to start it. Without it, every run logged a
GVFS warning. Before starting Cinnamon, the runner starts a real system bus
(`dbus-daemon --system`) and `udisksd`, and waits for
`org.freedesktop.UDisks2` to be owned. polkitd is started by bus activation.
Anything the bus itself prints at startup aborts the run.

### GPUs: one view on every host, and no switcheroo-control

What the desktop stack logs at startup depends on the DRM devices it can see.
switcheroo-control 2.6 lists only cards whose driver is `nvidia` or that udev
tagged with `ID_PATH_TAG`, and a container has no udev database. On a
workstation with an NVIDIA card it reported a default GPU and the log was
clean. On a CI runner with only a Matrox card it reported none. XApp 2.8.2 then
logged `No default GPU exists` and returned its GTask twice, which is a second
CRITICAL.

- `make test-extensions-desktop` mounts an empty tmpfs over `/sys/class/drm`,
  so every host shows the container the same zero DRM devices. The runner
  refuses to start on any other view.
- By owner ruling (2026-09-23), switcheroo-control is not installed. The
  runner aborts if its bus name is owned. XApp then takes its designed
  fallback: it finds no name owner, logs one warning, and continues with a
  synthetic default GPU. That warning is the second exempt line:

  ```
  (cinnamon:<pid>): XApp-WARNING **: <hh:mm:ss.mmm>: Switcheroo-control doesn't appear to be running, checking for Ubuntu support...
  ```

  GLib stamps the process id and the time into the line, so those two fields
  are the only ones not compared. The rest is compared byte for byte. Any
  other XApp line fails the run, including the same text at another level or
  with another message.

### The session's logs

The runner writes every session log to `/var/log/sysmon-desktop` in the
container (`LOG_DIR`): `cinnamon.log`, `xorg.log`, `system-bus.log` and
`udisksd.log`. `make test-extensions-desktop` copies that directory to
`test-logs/desktop/` on the host, with the runner's own output beside it as
`run.log`, whether the run passed or failed. When the nightly workflow fails it
uploads the directory as the `desktop-session-logs` artifact; see
[CI failure artifacts](ci-artifacts.md).

### Why it is not in `make ci`

It needs a Cinnamon install and software GL. The image is large, the run takes
minutes, and GL-in-a-container is not perfectly reliable. Run it nightly, and
before touching `applet.js`.

Both the session bus and the script's `gdbus` calls have to share one bus, so
the script uses `dbus-launch` and exports the address rather than
`dbus-run-session`, which would give Cinnamon a private bus nothing else could
reach.

### What it has already caught

The first runs of this tier found three things nothing else could:

- `require("./lib/format.js")` does not work. Cinnamon resolves an applet's
  `require()` against the applet directory and rejects a path containing a
  subdirectory, reporting "Path does not exist" against the applet root. The
  shared modules had to be installed flat. Tier 2 could not catch it because
  the harness's own `require` was more permissive than Cinnamon's — it now
  enforces the same restriction.
- The assertions themselves were passing vacuously. `gdbus` prints Eval results
  as `(true, '<value>')`, where the leading boolean is the *success* flag.
  Matching the expected text against the whole reply meant any expression that
  threw still satisfied an assertion expecting `true` or `false`. The value is
  now parsed out of the tuple before comparison.
- Calling the applet's `_onRepaint` handler directly from Eval takes the whole
  Cinnamon process down: `get_context()` is only valid inside a paint cycle.
  The script queues a repaint and lets the compositor drive it instead. The
  Cairo drawing itself is asserted operation by operation in tier 2, where a
  fake context makes that safe.

One environment note that cost a while: where Cinnamon keeps loaded applet
instances differs by release. Older ones index them in
`appletManager.appletObj`; the version in the container hangs them off
`appletManager.definitions[id].applet`, leaving `appletObj` empty even after
"Loaded applet ... in 69 ms" appears in the log. The probe collects from both.

## What stays manual

Whether the gauge actually looks right in a 22-pixel panel. Nothing above
checks pixels, and a screenshot diff on a software rasteriser would be more
maintenance than it is worth. Install it and look at it.

## Other desktop environments

The same shape generalises. GNOME Shell exposes an equivalent `Eval` and is
normally driven with `dbus-run-session gnome-shell --devkit --wayland`, wrapped
in `xvfb-run -a dbus-run-session` for CI; `gjsify/unit` offers BDD-style GJS
tests. KDE plasmoids are a different stack again.

If a second desktop is ever added, tiers 1 and 2 carry over unchanged — that is
the point of keeping the logic in `extensions/shared/`. Only the glue file and
the tier 3 container would be new.
