# House style

The visual decisions the go-sysmon dashboard makes, with the measurements
behind them, written so a sibling project can implement the same thing rather
than approximate it.

The stack is Tailwind 4 plus daisyUI 5. Most of this is not daisyUI-specific:
the palette values, the ratios and the traps apply to any implementation.

---

## 1. Colour

### The rule

**Every colour that carries text meets WCAG AA — 4.5:1 against the surface it
sits on.** Graphical objects that are not text — a line stroke, a gauge arc —
meet 3:1. Measure; do not choose by eye.

This is not a formality. `usageColor()` and `tempColor()` return
`text-success` / `text-warning` / `text-error`, so the number telling an
operator a disk is at 94% is rendered in a semantic colour. If that colour is
unreadable, the most important figure on the page is the least legible one.

### daisyUI's stock light theme fails all four

Measured against `base-200` (`#f8f8f8`), the card background most text sits on:

| token | stock | contrast | verdict |
|---|---|---|---|
| `success` | `#00d390` | **1.85:1** | fail |
| `warning` | `#fcb700` | **1.66:1** | fail |
| `error` | `#ff627d` | **2.71:1** | fail |
| `info` | `#00bafe` | **2.10:1** | fail |

The amber is worse than the green, which is the one a user noticed. **Check all
of them; the one that gets complained about is rarely the worst.** These values
are chosen to sit on a coloured chip, not to be read as text on white.

### The corrected light palette

Hue and chroma preserved, lightness reduced until each passes. A reading still
reads as green, amber or red.

| token | value | hex | on `base-200` | on `base-100` |
|---|---|---|---|---|
| `success` | `oklch(52% 0.15 163.223)` | `#00824f` | 4.60:1 | 4.88:1 |
| `warning` | `oklch(55% 0.189 84.429)` | `#a36200` | 4.61:1 | 4.88:1 |
| `error` | `oklch(57% 0.194 13.428)` | `#d03155` | 4.67:1 | 4.95:1 |
| `info` | `oklch(53.5% 0.136 232.661)` | `#0078ae` | 4.61:1 | 4.88:1 |

The matching `-content` tokens go to white. They ship dark, for dark text on a
light chip; against these they would be dark on dark.

### Which surfaces the guarantee covers

The margins above are thin, 0.1 to 0.17 over AA, so a darker surface undoes
them. The contrast suite measures the tokens on every surface the guarantee
covers, not on `base-200` alone:

| surface | where | light |
|---|---|---|
| `base-100` | navbar, tab strip, footer | 4.88:1 and up |
| `base-200` | cards | 4.59:1 and up |
| a clickable table row, hovered or selected | `DataTable` | same as its card |
| `base-300` | the page behind the cards, a table header | **4.20:1, fails**: carries no semantic text |

**Mark row state with a rule, not a fill.** A `DataTable` row used to take a
`base-300` fill when hovered or selected, which took its status cells below AA
the moment a user clicked it (#147). A row now stays on its card's surface.
Hover shows a 3px `base-content`/40 rule down the left edge, only on devices
that can hover. Selection draws a 3px `primary` rule on the left and a `primary`
hairline along the top and bottom of every cell, so the row stays visibly
selected even when a wide table is scrolled sideways. The selection marker is
held to the 3:1 non-text floor.

Keyboard focus on a row is a third shape: a 2px `base-content` outline around
the whole row, drawn only under `:focus-visible`, so a click does not add it on
top of the selection. It measures 16.7:1 on a light card and 15.6:1 on a dark
one, and the contrast suite holds it to 3:1. A focused, selected row shows both
the outline and the primary edges (#165).

**Forced colours (Windows High Contrast) restate every state in system
colours.** The browser drops box-shadows, fills and author colours there. That
took the hover and selection rules and the active tab's colour with it, and
turned the alert dots into nothing. Under `@media (forced-colors: active)`:

| state | how it shows |
|---|---|
| row hover | 1px dashed `CanvasText` outline |
| row selected | `Highlight` fill, `HighlightText` for every cell's text |
| row keyboard focus | 3px solid outline over either of the above: `CanvasText`, or `HighlightText` on a selected row |
| sort header focus | the browser's own focus ring; direction is the ▲/▼ glyph |
| active tab | `Highlight` fill, `HighlightText` label; focus is a `CanvasText` ring outside it |
| alert dot | `CanvasText` fill, `HighlightText` on the active tab |

`forced-color-adjust: none` is set only where a system pair is painted on
purpose (a selected row's cells, the active tab, the dot), so the browser does
not force it back to `Canvas`. `tests/forced-colors.spec.ts` emulates the mode
and asserts that each state renders differently from its neighbours.

**`base-300` carries no semantic text.** The four status colours measure
4.20-4.26:1 on it in the light theme. A tile nested inside a card — the GPU,
NPU and SMART detail tiles, the Overview pressure tiles — sits on `base-100`
with a `base-300` border, not on a `base-300` fill (#156). What text does remain
on `base-300` (a `DataTable` header, the DNS line on the page background) is
`base-content` at `/70` or stronger: `/60` is 4.45:1 there. The contrast suite
walks every tab of a fully populated fixture and fails on any semantic colour
painted over `base-300`, or any text there below AA.

**`primary` is a text colour in both themes.** daisyUI's dark `primary`,
`#605dff`, measures 3.40:1 on dark `base-100` and 3.60:1 on `base-200`, and the
active tab label is `text-primary`. The dark theme's `primary` is lightened to
`oklch(68% 0.233 277.117)`, `#7b7fff` (hue and chroma kept), and its
`primary-content` goes dark so a primary badge stays readable:

| token | value | hex | on `base-100` | on `base-200` |
|---|---|---|---|---|
| `primary` (dark) | `oklch(68% 0.233 277.117)` | `#7b7fff` | 4.77:1 | 5.05:1 |
| `primary-content` (dark) | `oklch(22% 0.07 277.117)` | `#14153a` | 5.28:1 on `primary` | |
| `primary` (light, stock) | `oklch(45% 0.24 277.023)` | `#422ad5` | 8.32:1 | 7.84:1 |

The charts draw nothing in `primary`, so the chart palette is unaffected.

### The dark theme is already correct

Do not "fix" it. Against dark `base-200` (`#191e24`):

| token | hex | contrast |
|---|---|---|
| `success` | `#00d390` | 8.53:1 |
| `warning` | `#fcb700` | 9.51:1 |
| `error` | `#ff627d` | 5.84:1 |
| `info` | `#00bafe` | 7.54:1 |
| `base-content` | `#ecf9ff` | 15.62:1 |

These are the same values that fail on white. **A palette is only readable
relative to a surface**, which is why each theme carries its own rather than
sharing one set.

### Text hierarchy by opacity — and its limit

One text colour, several levels of emphasis:

| class | role | light | dark |
|---|---|---|---|
| `text-base-content` | primary values, table cells | 16.68:1 | 15.62:1 |
| `text-base-content/70` | card and section titles | 6.40:1 | 8.19:1 |
| `text-base-content/60` | labels, units, descriptions, empty states | **4.58:1** | 6.36:1 |
| `text-base-content/30` | decoration only — the `/` between two values | 1.95:1 | 2.62:1 |

**`/60` is the floor for anything a reader must read.** Below it is decoration:
a separator, an outline icon. There is no level in between.

That is a limit, not a preference. Measured by compositing the text over the
card the way the screen does, `/50` is **3.35:1** in the light theme — and no
colour rescues it, because pure black at `/50` over `#f8f8f8` is about 3.96:1.
**Opacity gives a light theme fewer usable levels than a dark one.** If you need
a fourth level of muting, take it from size, weight or letterform instead.

go-sysmon shipped `/50` for secondary descriptions and `/40` for empty-state
messages — about 130 places — until this was measured. Both are now `/60`, and
labels stay distinguishable from descriptions by being uppercase rather than by
being darker, which is the same principle `StatCard` uses below.

---

## 2. Charts

**A canvas inherits nothing from the stylesheet.** A theme switch repaints
every daisyUI surface and leaves an ECharts axis exactly the colour it was
compiled with. So every colour a chart draws lives in a palette keyed by theme
(`src/lib/palette.ts`) and the components take it as an input.

That includes colours the stylesheet appears to own. A gauge letters its own
percentage: `usageEchartsColor()` is text, and no CSS rule can reach it.

### Two bars, because there are two kinds of colour

| | bar | why |
|---|---|---|
| usage colours (gauge value, its arc) | **4.5:1** | the gauge letters its number in them |
| series strokes (lines) | **3:1** | a stroke is a graphical object, not text |

Light values, measured on `#f8f8f8`:

```
usage    ok #1d805b  warn #906b0f  crit #b65252     4.60:1 each   (was 1.81 / 1.59 / 2.59)
series   #27a073 #2b97c7 #cf7830 #977de3
         #da65a2 #aa8a0a #e26767 #1f9f8e           3.10:1 each   (was 1.44 - 2.56)
```

Dark keeps the original values; they pass comfortably on a dark surface.

### Surface colours

Axes, gridlines, tooltip chrome and the gauge track are theme-keyed too. They
are the parts a stock "dark theme" would have supplied, and are set explicitly
instead so the chart bundle stays tree-shaken.

---

## 3. Type and spacing

The failure to avoid is **five kinds of information rendered at one weight and
one colour**. A reader then has to parse the layout to work out what is a
label, what is a measurement and what is commentary.

`StatCard` is the unit of composition, and it separates four kinds of
information on **four independent axes** — size, weight, opacity and letterform
— so no two are told apart by only one of them:

| role | classes | axis it uses |
|---|---|---|
| label | `text-xs text-base-content/60 uppercase tracking-wider font-medium` | small, muted, uppercase |
| value | `text-2xl font-bold` + semantic colour | large, bold, coloured |
| unit | `text-sm text-base-content/60`, baseline-aligned with the value | smaller, muted, adjacent |
| description | `text-xs text-base-content/50` | small, more muted, on its own line |

The unit sits in `flex items-baseline gap-1` with the value, so "52.6" and "%"
read as one measurement rather than two facts.

### Scale

Four sizes carry the whole dashboard. Adding a fifth usually means a hierarchy
problem, not a size problem.

| size | role | rough frequency |
|---|---|---|
| `text-2xl` | the one headline number on a stat card | rare |
| `text-lg` | a panel's single most important figure | rare |
| `text-base` | body, table cells, most content | default |
| `text-sm` | card titles, units, secondary controls | common |
| `text-xs` | labels, captions, badges, chart legends | very common |

Card titles are uniformly `card-title text-sm text-base-content/70`.

### Spacing

A short scale, used consistently:

| token | role |
|---|---|
| `p-4` | card body |
| `p-3` | nested card body |
| `space-y-4` | between the sections of a page |
| `space-y-1` | between rows inside a group |
| `gap-4` / `gap-3` | grid and flex gutters |
| `gap-2` / `gap-1` | items that belong together (value + unit) |
| `mb-3` / `mb-2` | title to content |

Surfaces are three deep and no deeper: `base-300` page, `base-200` card,
`base-100` nested tile with a `base-300` border. A fourth level stops reading
as depth. The nested tile is not filled with `base-300`: that surface carries no
semantic text (§1).

---

## 4. Theming

### Two states, light and dark

Matching go-netmon exactly: one icon button, a sun and a moon, one click
between them. The two tools are run by the same person and their theme controls
should not disagree about what a theme control is.

**There was a third state — "follow the operating system" — and it was removed
even though it worked.** It read `prefers-color-scheme` and tracked OS changes
live. The reason it went is the one that decides such things: *a user cannot
tell which state they are in without changing their operating system.* On a
light-mode machine "system" and "light" render identically, so a third of the
control was invisible, unverifiable, and indistinguishable from a bug. The
owner asked whether it was even wired up — which is the question an invisible
state earns.

The useful part survives without being a mode: **with nothing stored, the first
load takes its theme from `prefers-color-scheme`**, so a dark-mode machine opens
dark. The first explicit choice replaces that for good.

If you are building this in another project, keep the icon meaning the state.
One icon for two states says nothing about which is active.

### The mechanism

- `data-theme` on `<html>`; every surface derives from it.
- The choice is persisted under one key. Anything that is not `light` or `dark`
  is treated as no choice — including `system`, which an earlier build stored.
  A vocabulary that shrinks has to tolerate its own old values.
- Charts read the theme directly; they draw to a canvas and inherit nothing.

### The pre-paint bootstrap

**Applying the theme from the application alone flashes the wrong one on every
load.** A module script runs after first paint, so a user who chose dark gets a
white frame every time.

The theme is therefore set by a small classic script in `<head>`,
`public/theme-init.js`, before `<body>` is parsed. It is a file, not inline,
because the server's Content-Security-Policy allows no inline script. It
necessarily duplicates the storage key and the resolution rule; that
duplication is the price and the two must be changed together.

Its `try`/`catch` wraps **the storage read alone**. `localStorage` throws
outright in a sandboxed frame or with cookies blocked, and a catch around the
whole block that defaults to dark disagrees with the application — which treats
the same failure as "nothing stored" and follows the OS. On a light-preferring
machine that is the same flash, in the other direction.

Test the *timing*, not just the outcome. Applying the theme late is also
eventually correct, so an assertion on the final state passes against the bug.
Hooking `setAttribute` and asserting the first write happens while
`document.body` is still `null` is what catches it.

---

## 5. Two traps

Both produced a **convincing wrong answer**, which is what makes them worth
writing down. Anyone doing this work in another repository will hit them.

### The override that was in the stylesheet and did nothing

Overriding daisyUI's theme tokens through `@plugin "daisyui/theme"` emitted the
new values into the built CSS — and `getComputedStyle` still returned the stock
ones, because daisyUI's own default-theme rule beat the plugin block's
selector at runtime.

The built CSS contained the fix. A diff would have proved the fix was made. The
page was unchanged.

Use plain CSS with a selector you control:

```css
:root[data-theme="light"] {
  --color-success: oklch(52% 0.15 163.223);
  /* ... */
}
```

**Verify a colour change by reading it back from a rendered page**, not by
reading the stylesheet.

### The contrast test that measured nonsense

The obvious way to measure contrast is to read `getComputedStyle(el).color` and
pull the three numbers out of it. Modern CSS resolves these as `oklch()`, so
that parser reads

```
oklch(0.76 0.177 163.223)   ->   r=0.76  g=0.177  b=163.223
```

and returns a **plausible** ratio — 1.53:1, which looks exactly like a real
failing measurement rather than a broken parser.

Paint the colour onto a 1×1 canvas and read the pixel back. The browser does
the conversion, and it cannot be wrong about its own colour space:

```js
const ctx = document.createElement('canvas').getContext('2d');
ctx.fillStyle = getComputedStyle(el).color;
ctx.fillRect(0, 0, 1, 1);
const [r, g, b] = ctx.getImageData(0, 0, 1, 1).data;
```

### The test that shipped the thing it forbade

A test asserting no banned utility is emitted has to name the banned utility to
explain itself. Tailwind scans for class names, found `text-base-content/50` in
that comment, and generated the class — so the test failed against clean source,
correctly reporting a class that existed only because the test mentioned it.

```css
@source not "../tests";   /* tests are not UI source */
```

Worth excluding regardless: a test file should never be able to add CSS to the
product.

### The lesson under all three

**Measure through something you do not control.** A stylesheet you read, a
parser you wrote and a scanner that sees your test comments all share your
assumptions and will confirm them. The browser's own colour conversion, a real
Prometheus, the reference protobuf encoder — those can tell you that you are
wrong.

---

## Where this lives

| | |
|---|---|
| light palette and dark `primary` overrides | `src/app.css` |
| chart palette, both themes | `src/lib/palette.ts` |
| theme state and bootstrap | `src/lib/theme.svelte.ts`, `public/theme-init.js` |
| the control | `src/components/ThemeToggle.svelte` |
| composition unit | `src/components/StatCard.svelte` |
| contrast tests | `tests/contrast.spec.ts` |
| theme and pre-paint tests | `tests/theme.spec.ts` |

`tests/contrast.spec.ts` measures both the palette data and what the browser
actually renders, so a daisyUI upgrade that moves these values cannot quietly
undo the fix.
