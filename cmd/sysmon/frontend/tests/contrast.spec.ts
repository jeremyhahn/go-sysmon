import { test, expect, type Page } from '@playwright/test';
import { chartPalette, type ThemeName } from '../src/lib/palette';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab, type TabLabel } from './fixtures/tabs';

/**
 * Readability of the semantic colours.
 *
 * The owner's words on first seeing the light theme: *"the green text is a bit
 * light and hard to read"*. He was right, and measuring showed it was not only
 * the green — every semantic colour daisyUI's light theme ships fails WCAG AA
 * on a card, some worse than the green:
 *
 *     success  #00d390  1.85:1      warning  #fcb700  1.66:1
 *     error    #ff627d  2.71:1      info     #00bafe  2.10:1
 *
 * These are not decorative. `usageColor()` and `tempColor()` return
 * `text-success` / `text-warning` / `text-error`, so the figure telling an
 * operator a disk is at 94% was rendered in the least readable colour on the
 * page, and a gauge letters its percentage in the chart palette's equivalent.
 *
 * So this measures rather than eyeballs, and it measures what the BROWSER
 * resolves rather than what the source says, because a daisyUI upgrade that
 * moves these values would otherwise undo the fix in silence.
 */

// WCAG 2.1 contrast. Standard formula, not a copy of anything in the product.
function relativeLuminance([r, g, b]: [number, number, number]): number {
  const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a: [number, number, number], b: [number, number, number]): number {
  const [hi, lo] = [relativeLuminance(a), relativeLuminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

function fromHex(hex: string): [number, number, number] {
  const h = hex.replace('#', '');
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16) / 255) as [
    number,
    number,
    number,
  ];
}

/** AA for body text. */
const TEXT_AA = 4.5;
/** AA for a graphical object — a line stroke, a gauge arc. */
const GRAPHIC_AA = 3;

/** The card background most of this dashboard's text sits on. */
const SURFACE: Record<ThemeName, string> = {
  light: '#f8f8f8', // base-200
  dark: '#191e24', // base-200
};

test.describe('the chart palette is readable on its own surface', () => {
  for (const theme of ['light', 'dark'] as ThemeName[]) {
    test(`${theme}: usage colours reach AA as text`, async () => {
      // A gauge letters its percentage in these, so they are text.
      const { usage } = chartPalette(theme);
      const surface = fromHex(SURFACE[theme]);
      for (const [name, hex] of Object.entries(usage)) {
        const ratio = contrast(fromHex(hex), surface);
        expect(
          ratio,
          `${theme} usage.${name} (${hex}) is ${ratio.toFixed(2)}:1 on ${SURFACE[theme]}`
        ).toBeGreaterThanOrEqual(TEXT_AA);
      }
    });

    test(`${theme}: series strokes reach AA as graphics`, async () => {
      const { series } = chartPalette(theme);
      const surface = fromHex(SURFACE[theme]);
      expect(series.length).toBeGreaterThan(4);
      for (const hex of series) {
        const ratio = contrast(fromHex(hex), surface);
        expect(
          ratio,
          `${theme} series colour ${hex} is ${ratio.toFixed(2)}:1 on ${SURFACE[theme]}`
        ).toBeGreaterThanOrEqual(GRAPHIC_AA);
      }
    });
  }
});

/**
 * Installs the in-page measuring helpers, window.__contrast, before any page
 * script runs. Every browser-side measurement in this file goes through them,
 * so there is one conversion, one composite and one ratio rather than a copy
 * per test.
 *
 * Colours are painted onto a canvas and read back, rather than parsed out of
 * the computed value. Modern CSS colours resolve as oklch(), and a naive "grab
 * the three numbers" parser reads 0.76 0.177 163.223 as an RGB triple and
 * produces a plausible-looking figure from nonsense -- which is exactly what
 * the first version of this test did. Letting the browser do the conversion
 * cannot be wrong about it.
 *
 * Text is composited over its background before it is measured, because
 * opacity is not free: `text-base-content` at 50% is not half as readable, it
 * is a real blend, and on a light surface it lands below AA whatever colour the
 * token holds.
 */
async function installContrastProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    type RGBA = [number, number, number, number];

    const rgba = (value: string): RGBA => {
      const c = document.createElement('canvas');
      c.width = c.height = 1;
      const ctx = c.getContext('2d')!;
      ctx.fillStyle = value;
      ctx.fillRect(0, 0, 1, 1);
      const d = ctx.getImageData(0, 0, 1, 1).data;
      return [d[0] / 255, d[1] / 255, d[2] / 255, d[3] / 255];
    };

    const lum = (c: RGBA) => {
      const l = (x: number) => (x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4);
      return 0.2126 * l(c[0]) + 0.7152 * l(c[1]) + 0.0722 * l(c[2]);
    };

    /** The painted surface under an element: the first opaque ancestor fill. */
    const surfaceOf = (el: Element): string => {
      for (let e: Element | null = el; e != null; e = e.parentElement) {
        const bg = getComputedStyle(e).backgroundColor;
        if (rgba(bg)[3] > 0) return bg;
      }
      return getComputedStyle(document.body).backgroundColor;
    };

    /** WCAG contrast of `fg`, composited over the opaque `bg`. */
    const ratio = (fg: string, bg: string): number => {
      const f = rgba(fg);
      const b = rgba(bg);
      const a = f[3];
      const mixed: RGBA = [0, 1, 2].map((i) => f[i] * a + b[i] * (1 - a)) as unknown as RGBA;
      mixed[3] = 1;
      const [hi, lo] = [lum(mixed), lum(b)].sort((x, y) => y - x);
      return (hi + 0.05) / (lo + 0.05);
    };

    /** Contrast of a text class set inside `parent`, on the surface it lands on. */
    const textIn = (parent: Element, cls: string): number => {
      const probe = document.createElement('span');
      probe.className = cls;
      probe.textContent = 'probe';
      parent.appendChild(probe);
      const r = ratio(getComputedStyle(probe).color, surfaceOf(probe));
      probe.remove();
      return r;
    };

    /** Contrast of a text class on a freshly made card of `surface`. */
    const textOn = (cls: string, surface: string): number => {
      const card = document.createElement('div');
      card.className = `card ${surface}`;
      document.body.appendChild(card);
      const r = textIn(card, cls);
      card.remove();
      return r;
    };

    Object.assign(window, { __contrast: { rgba, ratio, surfaceOf, textIn, textOn } });
  });
}

/** The measuring helpers installContrastProbe puts on the page. */
interface ContrastProbe {
  rgba(value: string): [number, number, number, number];
  ratio(fg: string, bg: string): number;
  surfaceOf(el: Element): string;
  textIn(parent: Element, cls: string): number;
  textOn(cls: string, surface: string): number;
}

declare global {
  interface Window {
    __contrast: ContrastProbe;
  }
}

/** Measures a text class on a card of the given surface, as the browser resolves both. */
async function measure(page: Page, klass: string, surface = 'bg-base-200'): Promise<number> {
  return page.evaluate(
    ([cls, bg]) => window.__contrast.textOn(cls, bg),
    [klass, surface] as const
  );
}

test.describe('the muted text ladder stays above the floor', () => {
  /**
   * Every level used for text a reader must read.
   *
   * `/50` used to be here and measured 3.35:1 in the light theme, on the
   * secondary description under about a hundred values -- the same complaint
   * the owner made about the green, one level down the hierarchy. It is
   * structural rather than a tuning error: on a light surface, 50% opacity text
   * cannot reach AA with any colour, since pure black at `/50` over #f8f8f8 is
   * about 3.96:1. So the floor is `/60` and there is no fourth level of muting
   * to be had from opacity alone.
   *
   * Anything below `/60` is decoration -- the "/" between two readable values,
   * an outline icon -- and is not measured here.
   */
  const TEXT_LEVELS = [
    'text-base-content',
    'text-base-content/80',
    'text-base-content/70',
    'text-base-content/60',
  ];

  for (const scheme of ['light', 'dark'] as const) {
    test(`${scheme} theme`, async ({ page }) => {
      await installContrastProbe(page);
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto('/');

      for (const klass of TEXT_LEVELS) {
        const ratio = await measure(page, klass);
        expect(
          ratio,
          `${scheme}: .${klass} on a card is ${ratio.toFixed(2)}:1, below AA`
        ).toBeGreaterThanOrEqual(TEXT_AA);
      }
    });
  }

  test('no muted text class below the floor is emitted at all', async ({ page, request }) => {
    // Read from the built stylesheet, not from the rendered page.
    //
    // Scanning the DOM only sees what happens to be on screen at that instant,
    // and the first version of this test did exactly that: putting `/50` back
    // on a stat card's description did not fail it, because the element was
    // not rendered on the tab the test happened to be looking at.
    //
    // Tailwind emits a utility only when it finds it in the source, so the
    // stylesheet containing `text-base-content/50` means something uses it,
    // wherever that is. That makes this complete rather than a spot check.
    await page.goto('/');
    const hrefs = await page.evaluate(() =>
      Array.from(document.querySelectorAll('link[rel="stylesheet"]')).map(
        (l) => (l as HTMLLinkElement).href
      )
    );
    expect(hrefs.length, 'no stylesheet to inspect').toBeGreaterThan(0);

    /**
     * The two levels below the floor that exist on purpose, each enumerated so
     * a new one cannot join them quietly:
     *
     *   /30  the "/" between two readable values, and nothing else
     *   /20  an outline icon on an empty-state panel
     *
     * Both are decoration: no information is lost by not reading them. Any
     * other level under 60 is muted text that cannot be read, which is the
     * defect this guards.
     */
    const DECORATIVE = new Set([30, 20]);

    const offenders = new Set<string>();
    for (const href of hrefs) {
      const css = await (await request.get(href)).text();
      // Tailwind escapes the slash in the selector: .text-base-content\/50
      for (const m of css.matchAll(/text-base-content\\\/(\d+)/g)) {
        const level = Number(m[1]);
        if (level < 60 && !DECORATIVE.has(level)) {
          offenders.add(`text-base-content/${level}`);
        }
      }
    }

    expect(
      [...offenders].sort(),
      'these render text below the readable floor; use /60, or make it ' +
        'decoration (a border or a fill) if it is not meant to be read'
    ).toEqual([]);
  });
});


/**
 * Exactly the classes usageColor(), tempColor() and the alert helpers return,
 * plus text-primary: the active tab label and several panels' accent figures
 * are painted in it, and in the dark theme daisyUI's stock value measured
 * 3.40:1 on base-100 until app.css lightened it (#156).
 */
const SEMANTIC = [
  'text-success',
  'text-warning',
  'text-error',
  'text-info',
  'text-primary',
  'text-base-content',
];

/** Waits for the theme the emulated OS preference selects to be applied. */
async function openIn(page: Page, scheme: ThemeName, path = '/'): Promise<void> {
  await installContrastProbe(page);
  await page.emulateMedia({ colorScheme: scheme });
  await page.goto(path);
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-theme')))
    .toBe(scheme);
}

test.describe('semantic text is readable on every surface it is painted on', () => {
  /**
   * The flat surfaces that carry semantic text: base-200 is the card, base-100
   * the navbar, tab strip and footer. A clickable table row has its own test
   * below, measured on the rendered row rather than on a class name.
   *
   * The ratios recorded in app.css were measured on base-200 alone, and this
   * suite used to check nothing else -- which is how a selected table row came
   * to paint status text on base-300, a surface nobody had measured (#147).
   *
   * base-300 is not in this list, by rule rather than by omission. In the
   * light theme the four status colours measure 4.20-4.26:1 on it, below AA,
   * so the owner ruled that base-300 carries no semantic text at all: the GPU,
   * NPU and SMART detail tiles that used to be filled with it now sit on
   * base-100 with a border (#156). That rule is what the next describe block
   * enforces, on the rendered tabs rather than on a class name.
   */
  const SURFACES = ['bg-base-100', 'bg-base-200'];

  for (const scheme of ['light', 'dark'] as const) {
    test(`${scheme} theme`, async ({ page }) => {
      await openIn(page, scheme);
      for (const surface of SURFACES) {
        for (const klass of SEMANTIC) {
          const ratio = await measure(page, klass, surface);
          expect(
            ratio,
            `${scheme}: .${klass} on ${surface} is ${ratio.toFixed(2)}:1, below AA`
          ).toBeGreaterThanOrEqual(TEXT_AA);
        }
      }
    });
  }
});

/** One piece of rendered text, as the base-300 walk sees it. */
interface PaintedText {
  text: string;
  /** The semantic class whose colour the text is painted in, or '' if none. */
  semantic: string;
  onBase300: boolean;
  ratio: number;
}

/**
 * Every visible text element on the page: its colour, its surface, and its
 * contrast there, measured as rendered.
 */
async function paintedText(page: Page, classes: string[]): Promise<PaintedText[]> {
  return page.evaluate((classes) => {
    const c = window.__contrast;
    const key = (value: string) => c.rgba(value).slice(0, 3).map((x) => Math.round(x * 255)).join(',');

    const colourOf = (cls: string) => {
      const probe = document.createElement('span');
      probe.className = cls;
      document.body.appendChild(probe);
      const color = getComputedStyle(probe).color;
      probe.remove();
      return color;
    };
    const semanticByColour = new Map(classes.map((cls) => [key(colourOf(cls)), cls]));
    const tile = document.createElement('div');
    tile.className = 'bg-base-300';
    document.body.appendChild(tile);
    const base300 = key(getComputedStyle(tile).backgroundColor);
    tile.remove();

    const seen = new Set<Element>();
    const out: PaintedText[] = [];
    const walker = document.createTreeWalker(document.querySelector('main')!, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n != null; n = walker.nextNode()) {
      const el = n.parentElement;
      if (el == null || seen.has(el) || n.textContent!.trim() === '') continue;
      seen.add(el);
      const style = getComputedStyle(el);
      if (el.getClientRects().length === 0 || style.visibility === 'hidden') continue;
      const surface = c.surfaceOf(el);
      out.push({
        text: n.textContent!.trim().slice(0, 40),
        semantic: semanticByColour.get(key(style.color)) ?? '',
        onBase300: key(surface) === base300,
        ratio: c.ratio(style.color, surface),
      });
    }
    return out;
  }, classes);
}

/**
 * base-300 carries no semantic text (#156).
 *
 * The owner's ruling, enforced where it can actually be broken: on the pages
 * as rendered. Every tab is opened on a fixture that fills in the panels this
 * was about -- a discrete GPU, an NPU, an NVMe disk reporting wear, spare,
 * media errors and time over its temperature thresholds -- and every visible
 * piece of text is checked for the colour it is painted in and the surface
 * under it. A tile moved back onto a base-300 fill fails here, whichever
 * panel it is in.
 *
 * base-300 still exists as the page behind the cards and as a table header;
 * the text there is base-content, which reads on it, and is measured too.
 */
test.describe('base-300 carries no semantic text', () => {
  const STATUS = SEMANTIC.filter((cls) => cls !== 'text-base-content');

  /** Every tab, with a piece of text that shows its panel has rendered. */
  const TABS: [TabLabel, RegExp][] = [
    ['Overview', /Pressure/],
    ['Host', /Load Averages/],
    ['CPU', /CPU Usage/],
    ['GPU', /Utilization/],
    ['NPU', /Residency/],
    ['Memory', /Memory Usage/],
    ['Storage', /Health & Endurance/],
    ['Network', /Traffic Rate/],
    ['Virtualization', /Virtual Machines/],
    ['Containers', /Containers?/],
  ];

  for (const scheme of ['light', 'dark'] as const) {
    test(`${scheme} theme`, async ({ page }) => {
      await mockSnapshotFeed(page, {
        gpus: 1,
        containers: 3,
        vms: 2,
        images: 2,
        smartAttrs: true,
        disks: [
          {
            life_remaining_percent: 12,
            wear_level_percent: 88,
            available_spare_percent: 4,
            spare_threshold_percent: 10,
            media_errors: 3,
            error_log_entries: 7,
            unsafe_shutdowns: 41,
            warning_temp_time_minutes: 18,
            critical_temp_time_minutes: 2,
          },
        ],
      });
      await openIn(page, scheme);

      const offenders: string[] = [];
      let measuredSemantic = 0;
      for (const [label, marker] of TABS) {
        await openTab(page, label);
        await expect(page.locator('main').getByText(marker).first()).toBeVisible();

        for (const t of await paintedText(page, STATUS)) {
          if (t.semantic !== '') measuredSemantic++;
          if (t.onBase300 && t.semantic !== '') {
            offenders.push(`${label}: "${t.text}" is .${t.semantic} on base-300`);
          }
          if (t.ratio < TEXT_AA && (t.semantic !== '' || t.onBase300)) {
            offenders.push(`${label}: "${t.text}" is ${t.ratio.toFixed(2)}:1, below AA`);
          }
        }
      }

      // Not vacuous: the fixture really does put status text on the page.
      expect(measuredSemantic, `${scheme}: no semantic text was rendered`).toBeGreaterThan(20);
      expect(offenders, `${scheme}: text on base-300 or below AA`).toEqual([]);
    });
  }
});

/** The container listing: a DataTable whose rows are clickable and selectable. */
function containerRows(page: Page) {
  return page
    .locator('table')
    .filter({ has: page.getByRole('columnheader', { name: /^Name/ }) })
    .locator('tbody tr');
}

/** What one table row paints: its surface, its state rule, and its text. */
interface RowPaint {
  surface: string;
  card: string;
  rule: string;
  ruleRatio: number;
  /** The leading shadow colour of every cell: the selection edge, cell by cell. */
  edges: string[];
  /** Contrast of each cell's own text, as rendered. */
  cells: { text: string; ratio: number }[];
  /** Contrast of each semantic class placed in the row. */
  semantic: Record<string, number>;
}

async function paintOf(page: Page, index: number): Promise<RowPaint> {
  return containerRows(page)
    .nth(index)
    .evaluate((row, classes) => {
      const c = window.__contrast;
      const first = row.querySelector('td')!;
      const surface = c.surfaceOf(first);
      // The rules are inset box-shadows; a cell's colour is the leading token.
      const edgeOf = (td: Element) => {
        const shadow = getComputedStyle(td).boxShadow;
        return shadow === 'none' ? '' : (shadow.match(/^[a-z]+\([^)]*\)/)?.[0] ?? '');
      };
      const rule = edgeOf(first);
      const semantic: Record<string, number> = {};
      for (const cls of classes) semantic[cls] = c.textIn(first, cls);
      return {
        surface,
        card: c.surfaceOf(row.closest('.card')!),
        rule,
        ruleRatio: rule === '' ? 0 : c.ratio(rule, surface),
        edges: Array.from(row.querySelectorAll('td')).map(edgeOf),
        cells: Array.from(row.querySelectorAll('td')).map((td) => ({
          text: td.textContent!.trim(),
          ratio: c.ratio(getComputedStyle(td).color, c.surfaceOf(td)),
        })),
        semantic,
      };
    }, SEMANTIC);
}

/**
 * A selected or hovered row keeps the card's surface (#147).
 *
 * It used to be filled with base-300, and the status cells of a row -- the
 * text that says a container is at 94% CPU -- are exactly the semantic
 * colours, whose light-theme values clear AA on base-200 by 0.1 or so. Owner
 * ruling: the row keeps base-200 and marks its state with a rule down the left
 * edge instead, so the tokens stay where they were measured.
 */
test.describe('a selected or hovered table row keeps a readable surface', () => {
  for (const scheme of ['light', 'dark'] as const) {
    test(`${scheme} theme`, async ({ page }) => {
      await mockSnapshotFeed(page, { containers: 6 });
      await openIn(page, scheme);
      await openTab(page, 'Containers');
      await expect(containerRows(page)).toHaveCount(6);

      // Hover the third row away from the second, then select the second.
      await containerRows(page).nth(1).click();
      await page.mouse.move(0, 0);
      const resting = await paintOf(page, 3);
      const selected = await paintOf(page, 1);
      await containerRows(page).nth(2).hover();
      const hovered = await paintOf(page, 2);
      await containerRows(page).nth(1).hover();
      const selectedHovered = await paintOf(page, 1);

      const primary = await page.evaluate(() => {
        const probe = document.createElement('span');
        probe.className = 'text-primary';
        document.body.appendChild(probe);
        const color = getComputedStyle(probe).color;
        probe.remove();
        return color;
      });

      const states = { resting, selected, hovered, 'selected and hovered': selectedHovered };
      for (const [state, paint] of Object.entries(states)) {
        // No fill: the row shows the card through it.
        expect(paint.surface, `${scheme}: a ${state} row paints its own fill`).toBe(paint.card);

        for (const [cls, ratio] of Object.entries(paint.semantic)) {
          expect(
            ratio,
            `${scheme}: .${cls} in a ${state} row is ${ratio.toFixed(2)}:1, below AA`
          ).toBeGreaterThanOrEqual(TEXT_AA);
        }
        for (const cell of paint.cells) {
          expect(
            cell.ratio,
            `${scheme}: "${cell.text}" in a ${state} row is ${cell.ratio.toFixed(2)}:1, below AA`
          ).toBeGreaterThanOrEqual(TEXT_AA);
        }
      }

      // The state is still visible, now as a rule rather than a fill.
      expect(resting.edges, `${scheme}: a resting row carries a rule`).toEqual(
        resting.edges.map(() => '')
      );
      expect(hovered.rule, `${scheme}: a hovered row shows no rule`).not.toBe('');

      // Selection is information, so its marker is held to the non-text
      // contrast floor (WCAG 1.4.11); it must also outlast the pointer and
      // differ from hover, or clicking a row would look like nothing happened.
      for (const paint of [selected, selectedHovered]) {
        expect(paint.rule, `${scheme}: the selected row's rule is not primary`).toBe(primary);
        // Every cell, not just the first: a wide table scrolled sideways takes
        // the first cell out of view, and the selection must not go with it.
        expect(
          paint.edges,
          `${scheme}: some cells of the selected row carry no primary edge`
        ).toEqual(paint.edges.map(() => primary));
        expect(
          paint.ruleRatio,
          `${scheme}: the selection rule is ${paint.ruleRatio.toFixed(2)}:1 on its row`
        ).toBeGreaterThanOrEqual(GRAPHIC_AA);
      }
      expect(hovered.rule, `${scheme}: hover and selection look the same`).not.toBe(selected.rule);
    });
  }
});

/** What a row's focus indicator paints: an outline, and its contrast on the row. */
interface FocusPaint {
  style: string;
  width: number;
  color: string;
  ratio: number;
}

async function focusOf(page: Page, index: number): Promise<FocusPaint> {
  return containerRows(page)
    .nth(index)
    .evaluate((row) => {
      const c = window.__contrast;
      const s = getComputedStyle(row);
      const surface = c.surfaceOf(row.querySelector('td')!);
      return {
        style: s.outlineStyle,
        width: parseFloat(s.outlineWidth),
        color: s.outlineColor,
        ratio: c.ratio(s.outlineColor, surface),
      };
    });
}

/**
 * A row focused from the keyboard shows where focus is (#165).
 *
 * Focus is information, so its indicator is held to the non-text contrast floor
 * (WCAG 1.4.11) on the surface the row actually has. It is an outline around
 * the whole row: a different shape from hover's left rule and selection's
 * primary edges, and it must not replace either -- a focused row that is also
 * selected still shows both.
 */
test.describe('a keyboard-focused table row shows a visible focus indicator', () => {
  for (const scheme of ['light', 'dark'] as const) {
    test(`${scheme} theme`, async ({ page }) => {
      await mockSnapshotFeed(page, { containers: 6 });
      await openIn(page, scheme);
      await openTab(page, 'Containers');
      await expect(containerRows(page)).toHaveCount(6);
      await page.mouse.move(0, 0);

      // Focus reaches the row from the keyboard, which is when the ring shows.
      await containerRows(page).nth(0).focus();
      await page.keyboard.press('ArrowDown');
      await expect(containerRows(page).nth(1)).toBeFocused();

      const resting = await focusOf(page, 3);
      const focused = await focusOf(page, 1);
      expect(resting.style, `${scheme}: an unfocused row draws an outline`).toBe('none');

      expect(focused.style, `${scheme}: a focused row draws no outline`).not.toBe('none');
      expect(focused.width, `${scheme}: the focus outline is hairline`).toBeGreaterThanOrEqual(2);
      expect(
        focused.ratio,
        `${scheme}: the focus outline is ${focused.ratio.toFixed(2)}:1 on its row`
      ).toBeGreaterThanOrEqual(GRAPHIC_AA);

      // Selecting from the keyboard keeps both marks, and they stay distinct.
      await page.keyboard.press('Enter');
      const selectedFocused = await focusOf(page, 1);
      const selected = await paintOf(page, 1);
      expect(selectedFocused.style, `${scheme}: selection hid the focus outline`).not.toBe('none');
      expect(selectedFocused.ratio).toBeGreaterThanOrEqual(GRAPHIC_AA);
      expect(selected.rule, `${scheme}: focus hid the selection rule`).not.toBe('');
      expect(
        selectedFocused.color,
        `${scheme}: focus and selection are drawn in the same colour`
      ).not.toBe(selected.rule);
    });
  }
});
