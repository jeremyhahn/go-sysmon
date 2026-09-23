import { test, expect, type Locator, type Page } from '@playwright/test';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { makeContainers, mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * Tables operated from the keyboard alone (#165).
 *
 * A DataTable with selectable rows is an ARIA grid with a roving tabindex: one
 * tab stop for the whole table, arrow keys between rows, Enter or Space to
 * select. The container listing is the one table whose rows are selectable
 * today; the process table is the example of a table whose rows are not, and
 * must not become a trap of sixty tab stops.
 */

/** The container listing, which is the grid. */
function containerGrid(page: Page): Locator {
  return page.getByRole('grid', { name: 'Containers' });
}

function containerRows(page: Page): Locator {
  return containerGrid(page).locator('tbody tr');
}

/** The data-key of the focused element, or null when it is not a table row. */
async function focusedKey(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const el = document.activeElement;
    return el instanceof HTMLTableRowElement ? (el.dataset.key ?? null) : null;
  });
}

/** Presses `key` until `done` holds, failing after `max` presses. */
async function pressUntil(
  page: Page,
  key: string,
  done: () => Promise<boolean>,
  max = 30
): Promise<number> {
  for (let presses = 1; presses <= max; presses++) {
    await page.keyboard.press(key);
    if (await done()) return presses;
  }
  throw new Error(`${key} pressed ${max} times without reaching the target`);
}

async function openContainers(page: Page): Promise<void> {
  await page.goto('/');
  await openTab(page, 'Containers');
  await expect(containerRows(page).first()).toBeVisible();
}

/** The id of the fixture's i-th container, which is also its row key. */
const id = (i: number) => makeContainers(i + 1)[i].id;

test.describe('the container grid from the keyboard', () => {
  // 30 containers at 25 a page: two pages, so the pager and aria-rowcount apply.
  const COUNT = 30;

  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page, { containers: COUNT });
    await openContainers(page);
  });

  test('is one tab stop between its sort headers and its pager', async ({ page }) => {
    const grid = containerGrid(page);
    await expect(grid.locator('tbody tr[tabindex="0"]')).toHaveCount(1);
    await expect(grid.locator('tbody tr[tabindex="-1"]')).toHaveCount(24);

    // The search box is reachable, and Tab from it walks the page-size
    // selector and the sort headers into the grid.
    const search = page.getByPlaceholder(/Search name, ID or runtime/);
    await search.focus();
    await pressUntil(page, 'Tab', async () => (await focusedKey(page)) != null);
    expect(await focusedKey(page)).toBe(id(0));

    // One more Tab leaves the grid for the pager, not for the next row.
    await page.keyboard.press('Tab');
    await expect(page.getByRole('button', { name: 'Next page' })).toBeFocused();

    // And back: Shift+Tab re-enters on the row, then walks out to the search.
    await page.keyboard.press('Shift+Tab');
    expect(await focusedKey(page)).toBe(id(0));
    await pressUntil(page, 'Shift+Tab', async () =>
      search.evaluate((el) => el === document.activeElement)
    );
  });

  test('arrow, Home and End keys move between rows', async ({ page }) => {
    await containerRows(page).first().focus();

    await page.keyboard.press('ArrowDown');
    expect(await focusedKey(page)).toBe(id(1));
    await page.keyboard.press('ArrowDown');
    expect(await focusedKey(page)).toBe(id(2));
    await page.keyboard.press('ArrowUp');
    expect(await focusedKey(page)).toBe(id(1));

    await page.keyboard.press('End');
    expect(await focusedKey(page)).toBe(id(24));
    // The last row of the page is a floor, not a wrap.
    await page.keyboard.press('ArrowDown');
    expect(await focusedKey(page)).toBe(id(24));

    await page.keyboard.press('Home');
    expect(await focusedKey(page)).toBe(id(0));
    await page.keyboard.press('ArrowUp');
    expect(await focusedKey(page)).toBe(id(0));

    // The roving stop follows focus, so Tab back in returns to the same row.
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Tab');
    await page.keyboard.press('Shift+Tab');
    expect(await focusedKey(page)).toBe(id(1));
  });

  test('PageDown and PageUp turn the pager and keep the position', async ({ page }) => {
    await containerRows(page).nth(2).focus();

    await page.keyboard.press('PageDown');
    await expect(page.getByText(/Showing 26–30 of 30/)).toBeVisible();
    expect(await focusedKey(page)).toBe(id(27));

    // On the last page, PageDown goes to its last row.
    await page.keyboard.press('PageDown');
    expect(await focusedKey(page)).toBe(id(29));

    await page.keyboard.press('PageUp');
    await expect(page.getByText(/Showing 1–25 of 30/)).toBeVisible();
    expect(await focusedKey(page)).toBe(id(4));

    // On the first page, PageUp goes to its first row.
    await page.keyboard.press('PageUp');
    expect(await focusedKey(page)).toBe(id(0));
  });

  test('Enter opens the detail and Space closes it, like a click', async ({ page }) => {
    const rows = containerRows(page);
    await rows.first().focus();
    await page.keyboard.press('ArrowDown');

    await page.keyboard.press('Enter');
    await expect(rows.nth(1)).toHaveAttribute('aria-selected', 'true');
    await expect(rows.nth(0)).toHaveAttribute('aria-selected', 'false');
    await expect(page.getByText('CPU throttling')).toBeVisible();
    await expect(page.getByRole('heading', { name: /container-1\b/ })).toBeVisible();
    // Selecting does not move focus off the row.
    expect(await focusedKey(page)).toBe(id(1));

    const scrollBefore = await page.evaluate(() => window.scrollY);
    await page.keyboard.press(' ');
    await expect(rows.nth(1)).toHaveAttribute('aria-selected', 'false');
    await expect(page.getByText('CPU throttling')).toHaveCount(0);
    // Space selects; it does not also scroll the page.
    expect(await page.evaluate(() => window.scrollY)).toBe(scrollBefore);
  });

  test('a paged grid announces its full row count and each row position', async ({ page }) => {
    const grid = containerGrid(page);
    // 30 rows plus the header row.
    await expect(grid).toHaveAttribute('aria-rowcount', '31');
    await expect(grid.locator('thead tr')).toHaveAttribute('aria-rowindex', '1');
    await expect(containerRows(page).first()).toHaveAttribute('aria-rowindex', '2');

    await page.getByRole('button', { name: 'Next page' }).click();
    await expect(containerRows(page).first()).toHaveAttribute('aria-rowindex', '27');
  });

  test('a clicked row draws no focus ring but becomes the tab stop', async ({ page }) => {
    const row = containerRows(page).nth(3);
    await row.click();
    expect(await focusedKey(page)).toBe(id(3));
    expect(await row.evaluate((el) => el.matches(':focus-visible'))).toBe(false);
    await expect(row).toHaveAttribute('tabindex', '0');

    // From the keyboard the same row does show it.
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('ArrowUp');
    expect(await row.evaluate((el) => el.matches(':focus-visible'))).toBe(true);
  });
});

/**
 * A live feed whose frames rotate the containers' CPU figures, so each refresh
 * reorders the table (it sorts by CPU). A row that kept focus only because
 * nothing moved would not pass. With `auto` set, every frame rotates one step;
 * otherwise the test steps it, so it can hold the order still while it asserts
 * an exact row. Registered after the fixture's route, so it takes precedence.
 *
 * Under rotation r, container i has the (5 - (i + r) % count)-th highest CPU,
 * so it sits at index count - 1 - ((i + r) % count) of the sorted list.
 */
async function rotatingFeed(page: Page, count: number) {
  const feed = { frames: 0, rotation: 0, auto: true, count };
  await mockSnapshotFeed(page, { containers: count });
  await page.route(/\/api\/events(\?|$)/, async (route) => {
    feed.frames++;
    if (feed.auto) feed.rotation++;
    const containers = makeContainers(feed.count).map((c, i) => ({
      ...c,
      cpu_percent: (((i + feed.rotation) % feed.count) + 1) * 1.5,
    }));
    const payload = {
      ...snapshot,
      virtualization: { ...snapshot.virtualization, containers },
    };
    await route.fulfill({
      status: 200,
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' },
      body:
        `retry: 500\n\n` +
        `event: full\ndata: ${JSON.stringify({ seq: 1, snapshot: payload })}\n\n`,
    });
  });
  return feed;
}

/** Where container `i` sits in the sorted list under `rotation`. */
function sortedIndex(i: number, rotation: number, count: number): number {
  return count - 1 - ((i + rotation) % count);
}

test.describe('focus survives live refreshes', () => {
  test('the focused row keeps focus while refreshes reorder it', async ({ page }) => {
    const COUNT = 6;
    const feed = await rotatingFeed(page, COUNT);
    await openContainers(page);
    await expect(containerRows(page)).toHaveCount(COUNT);

    const target = id(2);
    const row = containerGrid(page).locator(`tbody tr[data-key="${target}"]`);
    await row.focus();
    await page.keyboard.press('Enter');
    const position = () => row.evaluate((el) => (el as HTMLTableRowElement).sectionRowIndex);
    const start = await position();
    const seen = feed.frames;

    // At least two refreshes, and the row has visibly moved.
    await expect.poll(() => feed.frames, { timeout: 15000 }).toBeGreaterThanOrEqual(seen + 2);
    await expect.poll(position, { timeout: 15000 }).not.toBe(start);

    expect(await focusedKey(page)).toBe(target);
    await expect(row).toHaveAttribute('aria-selected', 'true');
    await expect(row).toHaveAttribute('tabindex', '0');

    // And the keyboard still works from where focus was kept. Stop the
    // rotation and wait until the table shows the frozen order, so the order
    // no longer moves under the key press. Choosing the key from an earlier
    // position raced the refreshes: on a slow runner the row had reached the
    // very end the key pointed at, and focus correctly stayed put.
    feed.auto = false;
    const settled = sortedIndex(2, feed.rotation, COUNT);
    await expect.poll(position, { timeout: 15000 }).toBe(settled);
    expect(await focusedKey(page)).toBe(target);

    const [key, edge] = settled === 0 ? ['End', COUNT - 1] : ['Home', 0];
    await page.keyboard.press(key);
    await expect(containerRows(page).nth(edge)).toBeFocused();
    expect(await focusedKey(page)).not.toBe(target);
  });

  test('a row sorted onto another page takes focus with it', async ({ page }) => {
    // 30 rows at 25 a page. The feed is stepped by hand, so the row is on page
    // one when focused and on page two after exactly one refresh.
    const COUNT = 30;
    const feed = await rotatingFeed(page, COUNT);
    feed.auto = false;
    await openContainers(page);

    // Container 28 heads the list under rotation 1; one more step wraps it to
    // the bottom, index 29, which is page two.
    feed.rotation = 1;
    expect(sortedIndex(28, feed.rotation, COUNT)).toBe(0);
    const target = id(28);
    const row = containerGrid(page).locator(`tbody tr[data-key="${target}"]`);
    await expect(row).toHaveAttribute('aria-rowindex', '2');
    await row.focus();
    await page.keyboard.press('Enter');

    feed.rotation = 2;
    expect(sortedIndex(28, feed.rotation, COUNT)).toBe(COUNT - 1);
    await expect(page.getByText(/Showing 26–30 of 30/)).toBeVisible({ timeout: 15000 });
    await expect(row).toBeFocused();
    await expect(row).toHaveAttribute('aria-rowindex', '31');
    await expect(row).toHaveAttribute('aria-selected', 'true');

    // Enter still acts on the same container: it deselects it.
    await page.keyboard.press('Enter');
    await expect(row).toHaveAttribute('aria-selected', 'false');
  });

  test('a sort that keeps the focused row on the page keeps focus on that row', async ({ page }) => {
    // Focus follows the item, not the position: after a sort, Enter must act
    // on the container the user was on. dispatchEvent clicks the header
    // without moving focus, as Safari does.
    await mockSnapshotFeed(page, { containers: 6 });
    await openContainers(page);
    const row = containerGrid(page).locator(`tbody tr[data-key="${id(2)}"]`);
    await row.focus();
    expect(await row.evaluate((el) => (el as HTMLTableRowElement).sectionRowIndex)).toBe(2);

    // Sorted by CPU, largest first; one click reverses it.
    await containerGrid(page).getByRole('button', { name: 'CPU', exact: true }).dispatchEvent('click');
    await expect(page.getByRole('columnheader', { name: /^CPU/ }).first()).toHaveAttribute('aria-sort', 'ascending');
    await expect.poll(() => row.evaluate((el) => (el as HTMLTableRowElement).sectionRowIndex)).toBe(3);
    await expect(row).toBeFocused();
  });

  test('sorting from a later page returns to page one even if focus stays on a row', async ({ page }) => {
    // Safari does not move focus to a button it clicks, so a sort header can
    // be clicked while a row keeps focus. The sort resets to page one, and the
    // row that had focus now sorts onto page two; the table must stay on page
    // one rather than treat the sort as a refresh and follow the row.
    await mockSnapshotFeed(page, { containers: 30 });
    await openContainers(page);
    await page.getByRole('button', { name: 'Next page' }).click();
    await expect(page.getByText(/Showing 26–30 of 30/)).toBeVisible();

    // container-27 is third on page two by CPU, and 28th by ID.
    const row = containerGrid(page).locator(`tbody tr[data-key="${id(27)}"]`);
    await row.focus();
    const sortById = containerGrid(page).getByRole('button', { name: 'ID', exact: true });
    await sortById.dispatchEvent('click');
    expect(await focusedKey(page)).not.toBe(null);

    await expect(page.getByRole('columnheader', { name: /^ID/ })).toHaveAttribute('aria-sort', 'ascending');
    await expect(page.getByText(/Showing 1–25 of 30/)).toBeVisible();
    // Focus stays in the grid, in the position it had: the third row.
    await expect(containerRows(page).nth(2)).toBeFocused();
    expect(await focusedKey(page)).toBe(id(2));
  });

  test('a row kept through a refresh that removes its page keeps focus', async ({ page }) => {
    // Focus a row on page three of 60, then serve 40 in a new order: page
    // three no longer exists and the table clamps to page two, but the row
    // now ranks on page one. The page changed without the user turning it,
    // so focus must follow the row rather than land on whichever container
    // now holds its old position on the clamped page.
    const feed = await rotatingFeed(page, 60);
    feed.auto = false;
    feed.rotation = 1;
    await openContainers(page);

    const target = id(0);
    expect(sortedIndex(0, feed.rotation, 60)).toBe(58);
    await page.getByRole('button', { name: 'Next page' }).click();
    await page.getByRole('button', { name: 'Next page' }).click();
    const row = containerGrid(page).locator(`tbody tr[data-key="${target}"]`);
    await expect(row).toHaveAttribute('aria-rowindex', '60');
    await row.focus();

    feed.count = 40;
    feed.rotation = 20;
    expect(sortedIndex(0, feed.rotation, 40)).toBe(19);
    await expect(page.getByText(/Showing 1–25 of 40/)).toBeVisible({ timeout: 15000 });
    await expect(row).toBeFocused();
    await expect(row).toHaveAttribute('aria-rowindex', '21');

    await page.keyboard.press('Enter');
    await expect(row).toHaveAttribute('aria-selected', 'true');
  });
});

test.describe('a table without selectable rows', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page, { processCount: 60 });
    await page.goto('/');
    await openTab(page, 'Host');
    await expect(page.locator('table tbody tr').first()).toBeVisible();
  });

  test('is a plain table whose rows take no tab stops', async ({ page }) => {
    const table = page.getByRole('table', { name: 'Processes' });
    await expect(table).toBeVisible();
    await expect(page.getByRole('grid')).toHaveCount(0);
    await expect(table.locator('tbody tr[tabindex]')).toHaveCount(0);
    await expect(table.locator('tbody tr[aria-selected]')).toHaveCount(0);

    // Tab from the last sort header goes straight to the pager.
    await table.getByRole('button', { name: 'Nice' }).focus();
    await page.keyboard.press('Tab');
    await expect(page.getByRole('button', { name: 'Next page' })).toBeFocused();
  });

  test('sorts from the keyboard through header buttons', async ({ page }) => {
    const header = page.getByRole('columnheader', { name: /^PID/ });
    const button = header.getByRole('button', { name: 'PID' });

    // Reached with Tab from the header before it, and visibly focused there.
    await page.getByRole('button', { name: 'Name', exact: true }).focus();
    await page.keyboard.press('Tab');
    await expect(button).toBeFocused();
    expect(await button.evaluate((el) => getComputedStyle(el).outlineStyle)).not.toBe('none');

    await page.keyboard.press('Enter');
    await expect(header).toHaveAttribute('aria-sort', 'descending');
    await page.keyboard.press(' ');
    await expect(header).toHaveAttribute('aria-sort', 'ascending');

    const pids = (await page.locator('table tbody tr td:nth-child(2)').allInnerTexts()).map(Number);
    expect([...pids].sort((a, b) => a - b)).toEqual(pids);
  });

  test('the whole header cell, padding included, stays the sort target', async ({ page }) => {
    const header = page.getByRole('columnheader', { name: /^PID/ });
    // The corner is the cell's padding, outside the label.
    await header.click({ position: { x: 2, y: 2 } });
    await expect(header).toHaveAttribute('aria-sort', 'descending');
  });
});
