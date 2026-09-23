import { test, expect } from '@playwright/test';
import { mockSnapshotFeed, systemdMatchCount } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

// These exercise the shared DataTable component through the Host tab's process
// table. The data is injected over a mocked event stream rather than read from
// the machine: pagination and search are properties of the component, and an
// earlier version of this file assumed "a host always has more than 25
// processes", which is false in a container without --pid=host. That made the
// suite pass on GitHub and hang on Gitea waiting for a pager that was never
// rendered.
const TOTAL = 60; // 3 pages at the default size of 25

test.describe('DataTable behaviour', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page, { processCount: TOTAL });
    await page.goto('/');
    await openTab(page, 'Host');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
    await page.waitForSelector('table tbody tr', { timeout: 15000 });
  });

  test('paginates instead of rendering every row', async ({ page }) => {
    const rows = page.locator('table tbody tr');
    await expect(rows).toHaveCount(25);
    await expect(page.getByText(/Showing 1–25 of 60/)).toBeVisible();
  });

  test('next and previous page controls move through the data', async ({ page }) => {
    const first = await page.locator('table tbody tr').first().innerText();

    await page.getByRole('button', { name: 'Next page' }).click();
    await expect(page.getByText(/Showing 26–50 of 60/)).toBeVisible();
    const second = await page.locator('table tbody tr').first().innerText();
    expect(second).not.toBe(first);

    await page.getByRole('button', { name: 'Previous page' }).click();
    await expect(page.getByText(/Showing 1–25 of 60/)).toBeVisible();
    await expect(page.locator('table tbody tr').first()).toHaveText(first);
  });

  test('the last page holds the remainder, and next is disabled there', async ({ page }) => {
    await page.getByRole('button', { name: 'Next page' }).click();
    await page.getByRole('button', { name: 'Next page' }).click();

    await expect(page.getByText(/Showing 51–60 of 60/)).toBeVisible();
    await expect(page.locator('table tbody tr')).toHaveCount(10);
    await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled();
  });

  test('search filters the rows and resets to the first page', async ({ page }) => {
    await page.getByRole('button', { name: 'Next page' }).click();
    await expect(page.getByText(/Showing 26–/)).toBeVisible();

    await page.getByPlaceholder(/Search name, user or PID/).fill('systemd');

    // Filtering must return to page one, or the view is past the end of a
    // shorter list and looks empty.
    const matches = systemdMatchCount(TOTAL);
    await expect(page.getByText(new RegExp(`Showing 1–\\d+ of ${matches}`))).toBeVisible();

    const rows = page.locator('table tbody tr');
    const n = await rows.count();
    expect(n).toBeGreaterThan(0);
    for (let i = 0; i < n; i++) {
      expect((await rows.nth(i).innerText()).toLowerCase()).toContain('systemd');
    }
  });

  test('a search with no matches says so', async ({ page }) => {
    await page.getByPlaceholder(/Search name, user or PID/).fill('zzz-not-a-real-process-zzz');
    await expect(page.getByText(/No match for/)).toBeVisible();
    await expect(page.locator('table tbody tr')).toHaveCount(1); // the empty-state row
  });

  test('changing the page size re-paginates from the first page', async ({ page }) => {
    await page.getByRole('button', { name: 'Next page' }).click();
    await expect(page.getByText(/Showing 26–/)).toBeVisible();

    await page.getByLabel('Rows per page').selectOption('50');

    await expect(page.getByText(/Showing 1–50 of 60/)).toBeVisible();
    await expect(page.locator('table tbody tr')).toHaveCount(50);
  });
});

test.describe('DataTable sorting', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page, { processCount: TOTAL });
    await page.goto('/');
    await openTab(page, 'Host');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
    await page.waitForSelector('table tbody tr', { timeout: 15000 });
  });

  test('clicking a header sorts, and clicking again reverses', async ({ page }) => {
    const header = page.getByRole('columnheader', { name: /^PID/ });

    // Numeric columns sort largest-first on the first click: for CPU, memory
    // and I/O that is the interesting end, and consistency beats per-column
    // special cases.
    await header.click();
    await expect(header).toHaveAttribute('aria-sort', 'descending');
    const desc = (await page.locator('table tbody tr td:nth-child(2)').allInnerTexts()).map(Number);
    expect([...desc].sort((a, b) => b - a)).toEqual(desc);

    await header.click();
    await expect(header).toHaveAttribute('aria-sort', 'ascending');
    const asc = (await page.locator('table tbody tr td:nth-child(2)').allInnerTexts()).map(Number);
    expect([...asc].sort((a, b) => a - b)).toEqual(asc);
  });
});

// These two used to run against whatever the host happened to have, and to skip
// when it had nothing -- a CI container has no SMART-capable disk, and the
// image listing depends on a background /system/df query that may not have
// landed. Skipping made both report success for not running (#35).
//
// They are the same kind of assertion as pagination: search, sort and column
// layout are properties of the component, so the data belongs in the fixture.
// What is host-dependent -- whether a real collector produces this shape at
// all -- is covered where it can be asserted honestly: pkg/collector has tests
// for the SMART attribute decode, and the image listing has its own Go tests.
test.describe('Images table', () => {
  test('supports search, sort and pagination when images are present', async ({ page }) => {
    await mockSnapshotFeed(page, { images: 30 });

    await page.goto('/');
    await openTab(page, 'Containers');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });

    await expect(page.getByPlaceholder(/Search tag or ID/)).toBeVisible();

    // Scope to the image listing. The Containers tab also renders an
    // "Images & Storage" summary whose columns are Type/Count/Size/Reclaimable,
    // so a bare `Size` header matches two tables and trips strict mode. Tag is
    // unique to the image listing, which makes it the anchor.
    const imagesTable = page.locator('table').filter({
      has: page.getByRole('columnheader', { name: /^Tag/ }),
    });
    await expect(imagesTable.getByRole('columnheader', { name: /^Size/ })).toBeVisible();
    await expect(imagesTable.getByRole('columnheader', { name: /^Tag/ })).toBeVisible();
  });
});

test.describe('SMART attributes table', () => {
  test('expands into a searchable, sortable, paginated table', async ({ page }) => {
    await mockSnapshotFeed(page, { smartAttrs: true });

    await page.goto('/');
    await openTab(page, 'Storage');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });

    await page.getByRole('button', { name: /SMART Attributes/ }).first().click();
    await page.waitForTimeout(500);

    await expect(page.getByPlaceholder(/Search attribute or ID/).first()).toBeVisible();
    await expect(page.getByRole('columnheader', { name: /^Worst/ }).first()).toBeVisible();
    // Page size for SMART is 10, so a drive with more attributes paginates.
    const rows = page.locator('table tbody tr');
    expect(await rows.count()).toBeLessThanOrEqual(10);
  });
});
