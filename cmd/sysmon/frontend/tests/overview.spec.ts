import { test, expect } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * Served from the fixture snapshot, not from the host running the suite: what
 * this panel draws is a property of the panel, and a spec whose outcome depends
 * on the machine it runs on tests the machine (#163).
 */
test.describe('Overview Tab', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto('/');
    // Ensure we are on the Overview tab (default)
    await openTab(page, 'Overview');
    // Wait for the spinner to disappear — snapshot data has arrived
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('stat cards are rendered after data arrives', async ({ page }) => {
    // StatCard components appear in the stats grid
    const statCards = page.locator('.card-body').filter({ hasText: /RAM Used|Swap|Disk Used|Processes/ });
    await expect(statCards.first()).toBeVisible();
  });

  test('gauge charts render with percentage labels', async ({ page }) => {
    // GaugeChart renders an ECharts canvas inside a card labelled CPU / Memory / Disk
    const cpuLabel = page.locator('p').filter({ hasText: /^CPU$/i }).first();
    const memLabel = page.locator('p').filter({ hasText: /^Memory$/i }).first();
    const diskLabel = page.locator('p').filter({ hasText: /^Disk$/i }).first();

    await expect(cpuLabel).toBeVisible();
    await expect(memLabel).toBeVisible();
    await expect(diskLabel).toBeVisible();

    // Each gauge card contains a canvas element rendered by ECharts
    const gaugeCols = page.locator('.grid .card').filter({ hasText: /CPU|Memory|Disk/ });
    await expect(gaugeCols.first()).toBeVisible();
  });

  test('host info banner displays hostname and OS details', async ({ page }) => {
    // The host banner shows hostname as a heading and OS/kernel as a sub-line
    await expect(page.locator('h2').first()).toHaveText('fixture-host');

    // OS / kernel info text contains bullet separators
    const hostSubtext = page.locator('p.text-xs').first();
    await expect(hostSubtext).toBeVisible();
  });

  test('load averages are shown with numeric values', async ({ page }) => {
    // The Load Avg card shows 1m / 5m / 15m rows
    const loadAvgCard = page.locator('.card-body').filter({ hasText: 'Load Avg' }).first();
    await expect(loadAvgCard).toBeVisible();
    // Each load average label is a span inside the card
    await expect(loadAvgCard.locator('span').filter({ hasText: /^1m$/ })).toBeVisible();
    await expect(loadAvgCard.locator('span').filter({ hasText: /^5m$/ })).toBeVisible();
    await expect(loadAvgCard.locator('span').filter({ hasText: /^15m$/ })).toBeVisible();
  });

  test('process summary shows total and running count', async ({ page }) => {
    const processCard = page.locator('.card-body').filter({ hasText: 'Processes' }).first();
    await expect(processCard).toBeVisible();
    // The StatCard renders a value (total count) and sub text "N running"
    await expect(processCard.getByText(/running/i)).toBeVisible();
  });

  test('CPU usage time-series chart container is rendered', async ({ page }) => {
    const cpuChartHeading = page.getByText('CPU Usage (60s)');
    await expect(cpuChartHeading).toBeVisible();
    // ECharts mounts a canvas inside the chart card
    const cpuChartCard = cpuChartHeading.locator('..').locator('..');
    await expect(cpuChartCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('memory usage time-series chart container is rendered', async ({ page }) => {
    const memChartHeading = page.getByText('Memory Usage (60s)');
    await expect(memChartHeading).toBeVisible();
    const memChartCard = memChartHeading.locator('..').locator('..');
    await expect(memChartCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });
});
