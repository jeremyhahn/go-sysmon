import { test, expect, type Locator, type Page } from '@playwright/test';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { makeContainers, mockSnapshotFeed } from './fixtures/mock-sse';
import { dashboardTab, openTab, type TabLabel } from './fixtures/tabs';

/**
 * Forced colours: Windows High Contrast and its kin (#165).
 *
 * The browser swaps author colours for a small system palette and drops
 * box-shadows and fills, so a state shown only by colour or by a shadow
 * disappears. A DataTable row marked hover and selection with inset shadows,
 * and both vanished; the active tab was marked by colour alone. Each state
 * here must still read differently from its neighbours under forced colours,
 * which is measured on what the browser renders rather than on class names.
 */

/** What distinguishes one row state from another under forced colours. */
interface Look {
  outline: string;
  outlineColor: string;
  cellBackground: string;
  cellColor: string;
}

function containerRows(page: Page): Locator {
  return page.getByRole('grid', { name: 'Containers' }).locator('tbody tr');
}

async function lookOf(row: Locator): Promise<Look> {
  return row.evaluate((tr) => {
    const s = getComputedStyle(tr);
    const td = getComputedStyle(tr.querySelector('td')!);
    return {
      outline: s.outlineStyle === 'none' ? 'none' : `${s.outlineStyle} ${s.outlineWidth}`,
      outlineColor: s.outlineStyle === 'none' ? '' : s.outlineColor,
      cellBackground: td.backgroundColor,
      cellColor: td.color,
    };
  });
}

/** Resolves a system colour as this browser paints it under forced colours. */
async function systemColor(page: Page, keyword: string): Promise<string> {
  return page.evaluate((kw) => {
    const probe = document.createElement('span');
    probe.style.forcedColorAdjust = 'none';
    probe.style.backgroundColor = kw;
    document.body.appendChild(probe);
    const color = getComputedStyle(probe).backgroundColor;
    probe.remove();
    return color;
  }, keyword);
}

async function openForced(page: Page, feed: () => Promise<void>): Promise<void> {
  await feed();
  await page.emulateMedia({ forcedColors: 'active' });
  await page.goto('/');
  expect(await page.evaluate(() => matchMedia('(forced-colors: active)').matches)).toBe(true);
}

test.describe('under forced colours', () => {
  test('hover, selection and keyboard focus on a table row stay distinct', async ({ page }) => {
    await openForced(page, () => mockSnapshotFeed(page, { containers: 6 }));
    await openTab(page, 'Containers');
    const rows = containerRows(page);
    await expect(rows).toHaveCount(6);

    // Select row 1 with the mouse, then move the pointer off the table.
    await rows.nth(1).click();
    await page.mouse.move(0, 0);
    const resting = await lookOf(rows.nth(4));
    const selected = await lookOf(rows.nth(1));

    await rows.nth(3).hover();
    const hovered = await lookOf(rows.nth(3));
    await page.mouse.move(0, 0);

    // Keyboard focus, on an unselected row and then on the selected one.
    await rows.nth(1).focus();
    await page.keyboard.press('ArrowDown');
    await expect(rows.nth(2)).toBeFocused();
    const focused = await lookOf(rows.nth(2));
    await page.keyboard.press('ArrowUp');
    await expect(rows.nth(1)).toBeFocused();
    const selectedFocused = await lookOf(rows.nth(1));

    const highlight = await systemColor(page, 'Highlight');
    const highlightText = await systemColor(page, 'HighlightText');

    // Selection is the system's own selected-item pair.
    expect(selected.cellBackground).toBe(highlight);
    expect(selected.cellColor).toBe(highlightText);
    // Hover and focus are outlines of different shapes, and neither is a fill.
    expect(hovered.outline).toBe('dashed 1px');
    expect(focused.outline).toBe('solid 3px');
    expect(hovered.cellBackground).toBe(resting.cellBackground);
    expect(focused.cellBackground).toBe(resting.cellBackground);
    // Each outline is drawn in the system colour paired with what is under
    // it: CanvasText over the page, HighlightText over a selection. Any
    // other pairing is not guaranteed to contrast.
    const canvasText = await systemColor(page, 'CanvasText');
    expect(hovered.outlineColor).toBe(canvasText);
    expect(focused.outlineColor).toBe(canvasText);
    // A focused, selected row keeps both marks.
    expect(selectedFocused).toEqual({
      ...selected,
      outline: 'solid 3px',
      outlineColor: highlightText,
    });

    const states = { resting, selected, hovered, focused, selectedFocused };
    const seen = new Map<string, string>();
    for (const [name, look] of Object.entries(states)) {
      const key = JSON.stringify(look);
      expect(seen.get(key), `${name} looks the same as ${seen.get(key)}`).toBeUndefined();
      seen.set(key, name);
    }

    // Semantic text in a selected row goes to HighlightText rather than
    // keeping a status colour that may be unreadable on Highlight.
    const inner = await rows.nth(1).evaluate((tr) =>
      Array.from(tr.querySelectorAll('td, td *')).map((el) => getComputedStyle(el).color)
    );
    expect(new Set(inner)).toEqual(new Set([highlightText]));
  });

  test('a sort header shows keyboard focus and its sort direction', async ({ page }) => {
    await openForced(page, () => mockSnapshotFeed(page, { processCount: 30 }));
    await openTab(page, 'Host');

    const header = page.getByRole('columnheader', { name: /^PID/ });
    const button = header.getByRole('button', { name: 'PID' });
    await page.getByRole('button', { name: 'Name', exact: true }).focus();
    await page.keyboard.press('Tab');
    await expect(button).toBeFocused();
    const ring = await button.evaluate((el) => getComputedStyle(el).outlineStyle);
    expect(ring).not.toBe('none');

    // The direction is a glyph, not a colour, so it survives.
    await page.keyboard.press('Enter');
    await expect(header).toHaveAttribute('aria-sort', 'descending');
    await expect(button).toContainText('▼');
  });

  test('the active tab and a tab with alerts stay distinguishable', async ({ page }) => {
    await openForced(page, async () => {
      await mockSnapshotFeed(page);
      // A snapshot carrying one alert on the Storage tab, so its dot renders.
      const payload = {
        ...snapshot,
        alerts: [
          {
            id: 'disk-usage:sda',
            rule: 'disk-usage',
            severity: 'critical',
            category: 'storage',
            resource: 'sda',
            message: 'sda is 97% full',
            value: 97,
            threshold: 90,
            since: '2026-09-22T00:00:00Z',
          },
        ],
        virtualization: { ...snapshot.virtualization, containers: makeContainers(0) },
      };
      await page.route(/\/api\/events(\?|$)/, (route) =>
        route.fulfill({
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
          body: `retry: 1000\n\nevent: full\ndata: ${JSON.stringify({ seq: 1, snapshot: payload })}\n\n`,
        })
      );
    });

    const tab = (label: TabLabel) => dashboardTab(page, label);
    await openTab(page, 'Host');

    const look = (label: TabLabel) =>
      tab(label).evaluate((el) => {
        const s = getComputedStyle(el);
        return { background: s.backgroundColor, color: s.color };
      });
    // The tab fades its colours in (transition-colors), so wait for the
    // settled state rather than reading one frame of the fade.
    // Background and text fade separately, so poll the pair together.
    const settled = {
      background: await systemColor(page, 'Highlight'),
      color: await systemColor(page, 'HighlightText'),
    };
    await expect.poll(() => look('Host')).toEqual(settled);
    expect(await look('CPU'), 'the active tab looks like any other').not.toEqual(settled);

    // Keyboard focus on the active tab is a CanvasText ring outside it, on the
    // tab bar's Canvas, not a ring that could vanish into its Highlight fill.
    await tab('Overview').focus();
    await page.keyboard.press('Tab');
    await expect(tab('Host')).toBeFocused();
    // transition-colors animates the outline colour too, so poll for the
    // settled ring.
    const ring = () =>
      tab('Host').evaluate((el) => {
        const s = getComputedStyle(el);
        return { style: s.outlineStyle, color: s.outlineColor, outside: parseFloat(s.outlineOffset) > 0 };
      });
    const canvasText = await systemColor(page, 'CanvasText');
    await expect.poll(ring).toEqual({ style: 'solid', color: canvasText, outside: true });

    // The alert dot is a fill; it must still be painted, in the text colour
    // of the surface under it: CanvasText on a tab, HighlightText on the
    // active one.
    const fill = (label: TabLabel) =>
      tab(label).locator('.severity-dot').evaluate((el) => getComputedStyle(el).backgroundColor);
    await expect(tab('Storage').locator('.severity-dot')).toBeVisible();
    expect(await fill('Storage')).toBe(await systemColor(page, 'CanvasText'));
    await openTab(page, 'Storage');
    const highlightText = await systemColor(page, 'HighlightText');
    await expect.poll(() => fill('Storage')).toBe(highlightText);
  });
});
