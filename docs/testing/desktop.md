# Testing the desktop extensions

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
- a snapshot populates the panel, the tooltip and all nine popup sections
- a second snapshot refreshes the menu instead of duplicating it
- malformed data is ignored rather than crashing the applet
- end of stream, a non-200 response, a failed connect and a `bye` event each
  schedule the right reconnect
- backoff grows against a server that never delivers, and resets when data does
- removal from the panel cancels timers and closes the stream

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
inside it. The container starts a real `sysmon serve`, an Xvfb display, a D-Bus
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
- the GObject Introspection calls are real, not just plausible
- the popup builds against the actual `PopupMenu` implementation
- the applet reaches a real server over a real event stream

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
