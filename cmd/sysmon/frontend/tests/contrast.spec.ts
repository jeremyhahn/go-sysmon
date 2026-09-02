import { test, expect, type Page } from '@playwright/test';
import { chartPalette, type ThemeName } from '../src/lib/palette';

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

/** Measures a daisyUI text colour against a card, as the browser resolves it. */
async function measure(page: Page, klass: string): Promise<number> {
  return page.evaluate((cls) => {
    const card = document.createElement('div');
    card.className = 'card bg-base-200';
    const probe = document.createElement('span');
    probe.className = cls;
    probe.textContent = 'probe';
    card.appendChild(probe);
    document.body.appendChild(card);

    // Painted onto a canvas and read back, rather than parsed out of the
    // computed value. Modern CSS colours resolve as oklch(), and a naive
    // "grab the three numbers" parser reads 0.76 0.177 163.223 as an RGB
    // triple and produces a plausible-looking figure from nonsense -- which is
    // exactly what the first version of this test did. Letting the browser do
    // the conversion cannot be wrong about it.
    const toRGB = (value: string): [number, number, number] => {
      const c = document.createElement('canvas');
      c.width = c.height = 1;
      const ctx = c.getContext('2d')!;
      ctx.fillStyle = '#000';
      ctx.fillRect(0, 0, 1, 1);
      ctx.fillStyle = value;
      ctx.fillRect(0, 0, 1, 1);
      const [r, g, b] = ctx.getImageData(0, 0, 1, 1).data;
      return [r / 255, g / 255, b / 255];
    };
    const fg = toRGB(getComputedStyle(probe).color);
    const bg = toRGB(getComputedStyle(card).backgroundColor);
    card.remove();

    const lum = ([r, g, b]: [number, number, number]) => {
      const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
      return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
    };
    const [hi, lo] = [lum(fg), lum(bg)].sort((x, y) => y - x);
    return (hi + 0.05) / (lo + 0.05);
  }, klass);
}

/**
 * Measures a muted text level against a card, compositing the text over the
 * background the way the screen does. Opacity is not free: `text-base-content`
 * at 50% is not half as readable, it is a real blend, and on a light surface
 * it lands below AA whatever colour the token holds.
 */
async function measureOpacity(page: Page, klass: string): Promise<number> {
  return page.evaluate((cls) => {
    const card = document.createElement('div');
    card.className = 'card bg-base-200';
    const probe = document.createElement('span');
    probe.className = cls;
    card.appendChild(probe);
    document.body.appendChild(card);

    const toRGBA = (v: string): number[] => {
      const c = document.createElement('canvas');
      c.width = c.height = 1;
      const ctx = c.getContext('2d')!;
      ctx.fillStyle = v;
      ctx.fillRect(0, 0, 1, 1);
      const d = ctx.getImageData(0, 0, 1, 1).data;
      return [d[0] / 255, d[1] / 255, d[2] / 255, d[3] / 255];
    };
    const raw = toRGBA(getComputedStyle(probe).color);
    const bg = toRGBA(getComputedStyle(card).backgroundColor);
    card.remove();

    // Composite: the alpha is what makes a muted level muted.
    const a = raw[3];
    const fg = [0, 1, 2].map((i) => raw[i] * a + bg[i] * (1 - a));
    const lum = (c: number[]) => {
      const l = (x: number) => (x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4);
      return 0.2126 * l(c[0]) + 0.7152 * l(c[1]) + 0.0722 * l(c[2]);
    };
    const [hi, lo] = [lum(fg), lum(bg)].sort((x, y) => y - x);
    return (hi + 0.05) / (lo + 0.05);
  }, klass);
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
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto('/');

      for (const klass of TEXT_LEVELS) {
        const ratio = await measureOpacity(page, klass);
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

test.describe('daisyUI semantic text is readable in both themes', () => {
  // Exactly the classes usageColor(), tempColor() and the alert helpers return.
  const CLASSES = [
    'text-success',
    'text-warning',
    'text-error',
    'text-info',
    'text-base-content',
  ];

  for (const scheme of ['light', 'dark'] as const) {
    test(`${scheme} theme`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto('/');
      await expect.poll(() =>
        page.evaluate(() => document.documentElement.getAttribute('data-theme'))
      ).toBe(scheme);

      for (const klass of CLASSES) {
        const ratio = await measure(page, klass);
        expect(
          ratio,
          `${scheme}: .${klass} on a card is ${ratio.toFixed(2)}:1, below AA`
        ).toBeGreaterThanOrEqual(TEXT_AA);
      }
    });
  }
});
