import { test, expect } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * Served from the fixture snapshot, not from the host running the suite: what
 * this panel draws is a property of the panel, and a spec whose outcome depends
 * on the machine it runs on tests the machine (#163).
 */
test.describe('Network Tab', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'Network');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('one card is rendered per interface', async ({ page }) => {
    // Each interface renders its name as an h3 heading.
    await expect(page.locator('h3').filter({ hasText: /^\s*lo\s*$/ })).toBeVisible();
    await expect(page.locator('h3').filter({ hasText: /^\s*enp131s0\s*$/ })).toBeVisible();
  });

  test('interface status badge is visible', async ({ page }) => {
    // Status badges are: UP, DOWN, loopback, virtual
    const statusBadge = page.locator('.badge').filter({ hasText: /UP|DOWN|loopback|virtual/ }).first();
    await expect(statusBadge).toBeVisible();
  });

  test('hardware address (MAC) or "no MAC" is shown', async ({ page }) => {
    // The fixture's ethernet port has a MAC; its loopback has none.
    await expect(page.locator('p.text-xs').filter({ hasText: '00:00:5e:00:53:01' })).toBeVisible();
    await expect(page.locator('p.text-xs').filter({ hasText: /no MAC/i })).toBeVisible();
  });

  test('IP address badges are rendered for interfaces with addresses', async ({ page }) => {
    await expect(
      page.locator('.badge.badge-outline.font-mono').filter({ hasText: '192.0.2.10/24' })
    ).toBeVisible();
  });

  test('traffic rate indicators are shown (sent and received)', async ({ page }) => {
    // The rate display uses ↑ and ↓ arrows with formatBytesRate values
    const sentRate = page.locator('.text-success').filter({ hasText: /↑/ }).first();
    const recvRate = page.locator('.text-info').filter({ hasText: /↓/ }).first();
    await expect(sentRate).toBeVisible();
    await expect(recvRate).toBeVisible();
  });

  test('total bytes sent and received counters are displayed', async ({ page }) => {
    // Counters grid: "Sent: X (Y pkts)" and "Recv: X (Y pkts)"
    const sentCounter = page.locator('div').filter({ hasText: /^Sent:/ }).first();
    const recvCounter = page.locator('div').filter({ hasText: /^Recv:/ }).first();
    await expect(sentCounter).toBeVisible();
    await expect(recvCounter).toBeVisible();
  });

  test('traffic rate time-series chart canvas is rendered', async ({ page }) => {
    // TimeSeriesChart mounts a canvas; wait for ECharts to initialise
    await expect(page.getByText('Traffic Rate (60s)').first()).toBeVisible();
    const canvas = page.locator('main canvas').first();
    await expect(canvas).toBeVisible({ timeout: 8000 });
  });

  test('MTU value is shown for each interface', async ({ page }) => {
    // The details row at the bottom of each card shows "MTU: N"
    const mtuText = page.locator('div').filter({ hasText: /^MTU: / }).first();
    await expect(mtuText).toBeVisible();
  });
});
