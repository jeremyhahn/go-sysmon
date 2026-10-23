import { test, expect } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * Served from the fixture snapshot, not from the host running the suite: what
 * this panel draws is a property of the panel, and a spec whose outcome depends
 * on the machine it runs on tests the machine (#163).
 */
test.describe('Memory Tab', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'Memory');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('RAM usage gauge is visible', async ({ page }) => {
    await expect(page.getByText('RAM Usage')).toBeVisible();
    const gaugeCard = page.getByText('RAM Usage').locator('..').locator('..');
    await expect(gaugeCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('memory time-series chart is visible', async ({ page }) => {
    await expect(page.getByText('Memory Usage (60s)')).toBeVisible();
    const chartCard = page.getByText('Memory Usage (60s)').locator('..').locator('..');
    await expect(chartCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('stat cards show Total, Used, Available, Free labels', async ({ page }) => {
    // Labels are rendered in uppercase <p> elements inside StatCard card-body.
    // Use exact matching to avoid strict mode violations with similar substrings.
    const statGrid = page.locator('.grid').filter({ hasText: 'Total' }).filter({ hasText: 'Available' }).first();
    await expect(statGrid.locator('p').filter({ hasText: /^Total$/i }).first()).toBeVisible();
    await expect(statGrid.locator('p').filter({ hasText: /^Used$/i }).first()).toBeVisible();
    await expect(statGrid.locator('p').filter({ hasText: /^Available$/i }).first()).toBeVisible();
    await expect(statGrid.locator('p').filter({ hasText: /^Free$/i }).first()).toBeVisible();
  });

  test('memory stat card values are non-empty', async ({ page }) => {
    // StatCard renders the label in a <p> and value in a <span class="text-2xl font-bold">.
    // Find the card containing the "Total" label and verify the value is a byte string.
    const totalLabel = page.locator('p.uppercase').filter({ hasText: /^Total$/i }).first();
    await expect(totalLabel).toBeVisible();
    const totalCard = totalLabel.locator('..');
    // The value span contains a size string like "16 GB" or "8.0 GiB"
    const valueSpan = totalCard.locator('span.text-2xl').first();
    await expect(valueSpan).toBeVisible();
    const text = await valueSpan.textContent();
    expect(text).toMatch(/[KMGT]?i?B/);
  });

  test('buffers / cache breakdown section is rendered', async ({ page }) => {
    await expect(page.getByText('Buffers / Cache Breakdown')).toBeVisible();
  });

  test('swap section is present when swap exists', async ({ page }) => {
    // The fixture has swap configured, so the section must render.
    await expect(page.locator('h3').filter({ hasText: /^Swap$/ })).toBeVisible();
  });

  test('swap card shows the paging counters since boot', async ({ page }) => {
    const paging = page.getByTestId('swap-paging');
    await expect(paging).toContainText('Paged since boot:');
    await expect(paging).toContainText('1204 / 4565760 pages');
    await expect(paging).not.toContainText('unknown');
  });

  test('DIMM table lists each module', async ({ page }) => {
    await expect(page.locator('h3').filter({ hasText: /Memory Modules/ })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Location', exact: true })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Size', exact: true })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Type', exact: true })).toBeVisible();
    for (const slot of ['DIMMA1', 'DIMMA2', 'DIMMB1', 'DIMMB2']) {
      await expect(page.getByRole('cell', { name: slot, exact: true })).toBeVisible();
    }
  });
});

/**
 * Zero paged pages is also what a host that never paged reports, so a counter
 * that was not read must say unknown rather than draw 0 B.
 */
test.describe('Memory Tab, swap counters not read', () => {
  test('shows paging as unknown, not zero', async ({ page }) => {
    await mockSnapshotFeed(page, {
      memory: { swap_counters_available: false, swap_in_pages: 0, swap_out_pages: 0 },
    });
    await page.goto('/');
    await openTab(page, 'Memory');

    const paging = page.getByTestId('swap-paging');
    await expect(paging).toContainText('Paged since boot: unknown');
    await expect(paging).not.toContainText('0 B');
  });
});
