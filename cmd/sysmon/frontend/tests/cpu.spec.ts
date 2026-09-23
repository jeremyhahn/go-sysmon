import { test, expect } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * Served from the fixture snapshot, not from the host running the suite: what
 * this panel draws is a property of the panel, and a spec whose outcome depends
 * on the machine it runs on tests the machine (#163).
 */
test.describe('CPU Tab', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'CPU');
    // Wait for the loading spinner to go away once snapshot data arrives
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('CPU tab shows per-core usage bars', async ({ page }) => {
    // The per-core section heading
    await expect(page.getByText('Per-Core Usage')).toBeVisible();
    // One bar per logical CPU in the fixture. Scoped to the per-core list:
    // the panel draws other progress bars (frequency, pressure) as well.
    const coreBars = page.locator('.space-y-1').filter({ hasText: /^\s*Core \d+/ }).locator('progress');
    await expect(coreBars).toHaveCount(2);
  });

  test('each core bar has Core N label', async ({ page }) => {
    // "Core 0" also appears as a cell in the CPU details table, so scope the
    // assertion to the per-core usage list to keep the locator unambiguous.
    const coreLabel = page.locator('span', { hasText: /^Core 0$/ }).first();
    await expect(coreLabel).toBeVisible();
  });

  test('CPU details table renders with required columns', async ({ page }) => {
    await expect(page.getByText('CPU Details')).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Model' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Vendor' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Cores' })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Threads' })).toBeVisible();
  });

  test('CPU details table row shows the model name', async ({ page }) => {
    const firstDataRow = page.locator('table tbody tr').first();
    await expect(firstDataRow.locator('td').first()).toHaveText('Intel(R) Core(TM) Ultra 9 285K');
  });

  test('overall CPU gauge chart is rendered', async ({ page }) => {
    await expect(page.getByText('Overall CPU')).toBeVisible();
    // ECharts renders a canvas inside the gauge card
    const gaugeCard = page.getByText('Overall CPU').locator('..').locator('..');
    await expect(gaugeCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('CPU usage time-series chart is rendered', async ({ page }) => {
    await expect(page.getByText('CPU Usage (60s)')).toBeVisible();
    const chartCard = page.getByText('CPU Usage (60s)').locator('..').locator('..');
    await expect(chartCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('per-core usage is shown as a percentage', async ({ page }) => {
    const firstCoreUsage = page.locator('.space-y-1 span.font-semibold').first();
    await expect(firstCoreUsage).toHaveText(/^\s*\d+(\.\d+)?%\s*$/);
  });
});
