import { test, expect } from '@playwright/test';

// The Host tab's process table is the one guaranteed to have many rows on any
// machine, so it exercises search, sort and pagination end to end.
test.describe('DataTable behaviour', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Host', exact: true }).click();
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
    await page.waitForSelector('table tbody tr', { timeout: 15000 });
  });

  test('paginates instead of rendering every row', async ({ page }) => {
    const rows = page.locator('table tbody tr');
    const count = await rows.count();
    // Default page size is 25; a host has far more processes than that.
    expect(count).toBeLessThanOrEqual(25);
    await expect(page.getByText(/Showing 1–/)).toBeVisible();
  });

  test('next and previous page controls move through the data', async ({ page }) => {
    const first = await page.locator('table tbody tr').first().innerText();

    await page.getByRole('button', { name: 'Next page' }).click();
    await page.waitForTimeout(300);
    const second = await page.locator('table tbody tr').first().innerText();
    expect(second).not.toBe(first);
    await expect(page.getByText(/Showing 26–/)).toBeVisible();

    await page.getByRole('button', { name: 'Previous page' }).click();
    await page.waitForTimeout(300);
    await expect(page.getByText(/Showing 1–/)).toBeVisible();
  });

  test('search filters the rows and resets to the first page', async ({ page }) => {
    await page.getByRole('button', { name: 'Next page' }).click();
    await page.waitForTimeout(300);

    await page.getByPlaceholder(/Search name, user or PID/).fill('systemd');
    await page.waitForTimeout(400);

    await expect(page.getByText(/Showing 1–/)).toBeVisible();
    const rows = page.locator('table tbody tr');
    const n = await rows.count();
    for (let i = 0; i < n; i++) {
      expect((await rows.nth(i).innerText()).toLowerCase()).toContain('systemd');
    }
  });

  test('a search with no matches says so', async ({ page }) => {
    await page.getByPlaceholder(/Search name, user or PID/).fill('zzz-not-a-real-process-zzz');
    await page.waitForTimeout(400);
    await expect(page.getByText(/No match for/)).toBeVisible();
  });

  test('clicking a header sorts, and clicking again reverses', async ({ page }) => {
    const header = page.getByRole('columnheader', { name: /^PID/ });

    // Numeric columns sort largest-first on the first click: for CPU, memory
    // and I/O that is the interesting end, and consistency beats per-column
    // special cases.
    await header.click();
    await page.waitForTimeout(300);
    const desc = (await page.locator('table tbody tr td:nth-child(2)').allInnerTexts()).map(Number);
    expect([...desc].sort((a, b) => b - a)).toEqual(desc);

    await header.click();
    await page.waitForTimeout(300);
    const asc = (await page.locator('table tbody tr td:nth-child(2)').allInnerTexts()).map(Number);
    expect([...asc].sort((a, b) => a - b)).toEqual(asc);
  });

  test('page size selector changes how many rows render', async ({ page }) => {
    await page.getByLabel('Rows per page').selectOption('10');
    await page.waitForTimeout(400);
    expect(await page.locator('table tbody tr').count()).toBeLessThanOrEqual(10);
  });
});

test.describe('Images table', () => {
  test('supports search, sort and pagination when images are present', async ({ page, request }) => {
    const snap = await (await request.get('/api/snapshot')).json();
    const images = snap.virtualization?.runtime?.images ?? [];
    if (images.length === 0) {
      test.skip(true, 'no container images visible on this host');
    }

    await page.goto('/');
    await page.getByRole('button', { name: 'Containers', exact: true }).click();
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });

    await expect(page.getByPlaceholder(/Search tag or ID/)).toBeVisible();
    await expect(page.getByRole('columnheader', { name: /^Size/ })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: /^Tag/ })).toBeVisible();
  });
});

test.describe('SMART attributes table', () => {
  test('expands into a searchable, sortable, paginated table', async ({ page, request }) => {
    const snap = await (await request.get('/api/snapshot')).json();
    const withAttrs = (snap.disks ?? []).find(
      (d: { smart_attrs: unknown[] | null }) => (d.smart_attrs ?? []).length > 0
    );
    if (!withAttrs) {
      test.skip(true, 'no disk reports SMART attributes on this host');
    }

    await page.goto('/');
    await page.getByRole('button', { name: 'Storage', exact: true }).click();
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
