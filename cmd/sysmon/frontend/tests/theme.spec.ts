import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * Light/dark theming.
 *
 * Three things have to hold, and each is tested against the way it can
 * silently not hold:
 *
 *  1. With no stored choice the dashboard follows the operating system, and
 *     keeps following it while the page is open.
 *  2. A stored choice wins over the operating system and survives a reload.
 *  3. The theme is applied BEFORE the body renders. This is the one that
 *     regresses invisibly: applying it from the app's module script also
 *     ends up correct, just one painted frame later, so the assertions below
 *     are about *when* the attribute was first written, and about every value
 *     it was ever given -- not only what it says once the page has settled.
 *
 * The ECharts canvases get their own test, because they inherit nothing from
 * the stylesheet: a theme switch that repaints every daisyUI surface can leave
 * the charts exactly as they were.
 */

const THEME_KEY = 'sysmon.theme';

/**
 * The theme control is one icon button that flips, matching go-netmon's.
 *
 * It is found by role and accessible name rather than by a test attribute: the
 * name is "<current> theme. Switch to: <other> theme", which is the only thing
 * on screen that names the state, so asserting on it is asserting on what a
 * user (or a screen reader) is actually told.
 */
const themeButton = (page: Page) => page.getByRole('button', { name: /Switch to:/ });

/** The theme the control says it is in. */
async function shownMode(page: Page): Promise<string> {
  const label = (await themeButton(page).getAttribute('aria-label')) ?? '';
  return label.split('.')[0].trim().toLowerCase();
}

/** Clicks until the control reports the wanted theme, at most one flip. */
async function chooseMode(page: Page, wanted: string) {
  for (let i = 0; i < 2; i++) {
    if ((await shownMode(page)) === wanted) return;
    await themeButton(page).click();
  }
  throw new Error(`could not reach ${wanted}`);
}

interface ThemeWrite {
  value: string;
  bodyExisted: boolean;
}

/** Reads the theme actually applied to the document. */
function appliedTheme(page: Page) {
  return page.evaluate(() => document.documentElement.getAttribute('data-theme'));
}

/**
 * Records every write of data-theme, synchronously, with the value written and
 * whether <body> existed at that instant.
 *
 * It hooks setAttribute rather than using a MutationObserver because an
 * observer's callback is asynchronous: a batch of records is delivered at a
 * later microtask, by which time the attribute holds only its final value and
 * body may since have been parsed. An early wrong value corrected before
 * delivery would be invisible. This sees each call as it happens.
 *
 * The pre-paint bootstrap lives in <head>, so its write lands while body is
 * still null; any later write -- module script, component mount, effect --
 * lands after body exists.
 */
async function recordThemeWrites(page: Page) {
  await page.addInitScript(() => {
    const w = window as unknown as { __themeWrites: ThemeWrite[] };
    w.__themeWrites = [];
    const setAttribute = Element.prototype.setAttribute;
    Element.prototype.setAttribute = function (name: string, value: string) {
      if (name === 'data-theme' && this === document.documentElement) {
        w.__themeWrites.push({ value, bodyExisted: document.body !== null });
      }
      return setAttribute.call(this, name, value);
    };
  });
}

function themeWrites(page: Page): Promise<ThemeWrite[]> {
  return page.evaluate(
    () => (window as unknown as { __themeWrites: ThemeWrite[] }).__themeWrites
  );
}

/** Seeds a stored preference before any page script runs. */
async function storeMode(page: Page, mode: string) {
  await page.addInitScript(
    ([key, value]) => localStorage.setItem(key, value),
    [THEME_KEY, mode] as const
  );
}

/** Makes every read of localStorage throw, as it does in a sandboxed frame or
 * with cookies blocked entirely. */
async function breakStorageReads(page: Page) {
  await page.addInitScript(() => {
    Storage.prototype.getItem = () => {
      throw new DOMException('denied', 'SecurityError');
    };
  });
}

test.describe('theme', () => {
  test('the first load follows the OS when nothing is stored', async ({ page }) => {
    // There is no "system" mode any more -- a user could not tell which of the
    // three states they were in without changing their operating system, so a
    // third of the control was invisible. What survives is the sensible part:
    // a machine in dark mode opens dark, without that being a mode.
    await page.emulateMedia({ colorScheme: 'light' });
    await page.goto('/');
    await expect.poll(() => appliedTheme(page)).toBe('light');

    await page.emulateMedia({ colorScheme: 'dark' });
    await page.reload();
    await expect.poll(() => appliedTheme(page)).toBe('dark');
  });

  test('an explicit choice overrides the OS and survives a reload', async ({ page }) => {
    // The OS says dark throughout, so a "light" result can only come from the
    // stored choice being honoured.
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.goto('/');
    await expect.poll(() => appliedTheme(page)).toBe('dark');

    await chooseMode(page, 'light theme');
    await expect.poll(() => appliedTheme(page)).toBe('light');

    await page.reload();
    await expect.poll(() => appliedTheme(page)).toBe('light');
    expect(await shownMode(page)).toBe('light theme');

    // And the choice keeps winning: the OS is only consulted when nothing is
    // stored, so a light choice survives a dark machine indefinitely.
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.reload();
    await expect.poll(() => appliedTheme(page)).toBe('light');
  });

  test('the control flips between two states and shows which it is in', async ({ page }) => {
    // One icon button, two states, one click between them -- the same control
    // go-netmon has, so the two products do not disagree about what a theme
    // toggle is.
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.goto('/');

    expect(await shownMode(page)).toBe('dark theme');
    await expect.poll(() => appliedTheme(page)).toBe('dark');
    const darkIcon = await themeButton(page).innerHTML();

    await themeButton(page).click();
    expect(await shownMode(page)).toBe('light theme');
    await expect.poll(() => appliedTheme(page)).toBe('light');
    const lightIcon = await themeButton(page).innerHTML();

    await themeButton(page).click();
    expect(await shownMode(page)).toBe('dark theme');
    await expect.poll(() => appliedTheme(page)).toBe('dark');

    // The icon is the state. One icon for two states would leave the control
    // saying nothing about which one is active.
    expect(darkIcon, 'both themes show the same icon').not.toBe(lightIcon);
  });

  /**
   * Every route into the pre-paint bootstrap, because it duplicates the
   * resolution rule from theme.svelte.ts and each branch can drift on its own.
   * The last case is the one a catch-all around the whole bootstrap gets
   * wrong: a storage failure is not a preference for dark, and answering it
   * that way flashes dark on a light machine -- the very thing the script
   * exists to prevent, in the other direction.
   */
  const preflight: {
    name: string;
    stored?: string;
    os: 'light' | 'dark';
    brokenStorage?: boolean;
    want: string;
  }[] = [
    { name: 'a stored choice beats the OS', stored: 'light', os: 'dark', want: 'light' },
    { name: 'a stored choice beats the OS, the other way', stored: 'dark', os: 'light', want: 'dark' },
    { name: 'no choice follows a dark OS', os: 'dark', want: 'dark' },
    { name: 'no choice follows a light OS', os: 'light', want: 'light' },
    // Both OS directions, deliberately. A bootstrap that trusted any stored
    // string would resolve "chartreuse" to light and only the dark-OS case
    // would catch it; a bootstrap that always chose dark would only be caught
    // by the light-OS case.
    { name: 'an unreadable value follows a dark OS', stored: 'chartreuse', os: 'dark', want: 'dark' },
    // "system" was a real mode in an earlier build. A browser that still has
    // it stored must fall back to the OS, not to a mode that no longer exists.
    { name: 'a stored "system" from an older build follows a dark OS', stored: 'system', os: 'dark', want: 'dark' },
    { name: 'a stored "system" from an older build follows a light OS', stored: 'system', os: 'light', want: 'light' },
    { name: 'an unreadable value follows a light OS', stored: 'chartreuse', os: 'light', want: 'light' },
    { name: 'unreadable storage follows the OS', os: 'light', brokenStorage: true, want: 'light' },
  ];

  for (const c of preflight) {
    test(`applied before the body renders: ${c.name}`, async ({ page }) => {
      await recordThemeWrites(page);
      if (c.stored != null) await storeMode(page, c.stored);
      if (c.brokenStorage) await breakStorageReads(page);
      await page.emulateMedia({ colorScheme: c.os });
      await page.goto('/');
      await expect.poll(() => appliedTheme(page)).toBe(c.want);

      const writes = await themeWrites(page);
      expect(writes.length).toBeGreaterThan(0);
      expect(writes[0].value).toBe(c.want);
      expect(
        writes[0].bodyExisted,
        'the first data-theme write must happen in <head>, before <body> exists; ' +
          'a write after body means the wrong theme was painted for a frame'
      ).toBe(false);

      // No write ever disagreed, so there was no flash that a later correction
      // papered over.
      expect(writes.map((w) => w.value)).toEqual(writes.map(() => c.want));
    });
  }

  /**
   * The charts.
   *
   * The assertion is on exact palette colours rather than "the picture
   * changed": both charts fill large areas with ChartPalette.track (the gauge's
   * unfilled arc, the line chart's gridlines) and the gauge letters its caption
   * in ChartPalette.mutedText. Requiring the right colour to be present AND the
   * other theme's to be absent means a chart that repaints for an unrelated
   * reason, or that updates only one of the two, cannot pass.
   */
  const TRACK = { dark: [42, 50, 60], light: [229, 231, 235] };
  const MUTED = { dark: [166, 173, 187], light: [75, 85, 99] };

  /** Reports whether an opaque pixel of exactly this colour is on the canvas. */
  async function hasColor(page: Page, index: number, rgb: number[]) {
    return page.evaluate(
      ([i, r, g, b]) => {
        const c = document.querySelectorAll('canvas')[i] as HTMLCanvasElement;
        const d = c
          .getContext('2d')!
          .getImageData(0, 0, c.width, c.height).data;
        for (let p = 0; p < d.length; p += 4) {
          if (d[p] === r && d[p + 1] === g && d[p + 2] === b && d[p + 3] === 255) {
            return true;
          }
        }
        return false;
      },
      [index, rgb[0], rgb[1], rgb[2]] as const
    );
  }

  test('the charts are drawn in the palette of the current theme', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'dark' });
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'Memory');

    // The RAM gauge and the memory line chart, in DOM order. Both are fed by
    // the fixed snapshot the mock serves.
    const GAUGE = 0;
    const SERIES = 1;
    await expect(page.locator('canvas')).toHaveCount(2);

    const drawnIn = async (theme: 'dark' | 'light') => {
      const other = theme === 'dark' ? 'light' : 'dark';
      return (
        (await hasColor(page, GAUGE, TRACK[theme])) &&
        (await hasColor(page, GAUGE, MUTED[theme])) &&
        (await hasColor(page, SERIES, TRACK[theme])) &&
        !(await hasColor(page, GAUGE, TRACK[other])) &&
        !(await hasColor(page, GAUGE, MUTED[other])) &&
        !(await hasColor(page, SERIES, TRACK[other]))
      );
    };

    // The gauge animates in, so poll rather than sampling one frame of the
    // entrance animation.
    await expect
      .poll(() => drawnIn('dark'), {
        message: 'both charts must be drawn in the dark palette',
        timeout: 10000,
      })
      .toBe(true);

    await chooseMode(page, 'light theme');
    await expect.poll(() => appliedTheme(page)).toBe('light');

    await expect
      .poll(() => drawnIn('light'), {
        message: 'both charts must repaint in the light palette',
        timeout: 10000,
      })
      .toBe(true);
  });
});
