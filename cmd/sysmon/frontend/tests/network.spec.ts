import { test, expect } from '@playwright/test';

test.describe('Network Tab', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Network' }).click();
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('at least one network interface card is rendered', async ({ page }) => {
    // Each interface renders its name as an h3 heading
    const ifaceHeading = page.locator('h3').first();
    await expect(ifaceHeading).toBeVisible();
    const name = await ifaceHeading.textContent();
    expect(name?.trim().length).toBeGreaterThan(0);
  });

  test('interface status badge is visible', async ({ page }) => {
    // Status badges are: UP, DOWN, loopback, virtual
    const statusBadge = page.locator('.badge').filter({ hasText: /UP|DOWN|loopback|virtual/ }).first();
    await expect(statusBadge).toBeVisible();
  });

  test('hardware address (MAC) or "no MAC" is shown', async ({ page }) => {
    // Each card's sub-line shows the hardware_addr or the fallback "no MAC"
    const macText = page.locator('p.text-xs').first();
    await expect(macText).toBeVisible();
    const text = await macText.textContent();
    // Either a colon-separated MAC address or the literal "no MAC"
    expect(text).toMatch(/([0-9a-f]{2}:){5}[0-9a-f]{2}|no MAC/i);
  });

  test('IP address badges are rendered for interfaces with addresses', async ({ page }) => {
    // IP badges are rendered as badge-outline items in the address flex container
    const addrBadge = page.locator('.badge.badge-outline.font-mono').first();
    const count = await addrBadge.count();
    if (count > 0) {
      await expect(addrBadge).toBeVisible();
      const addr = await addrBadge.textContent();
      // Should look like an IPv4 or IPv6 address with optional prefix length
      expect(addr?.trim().length).toBeGreaterThan(0);
    }
    // Loopback-only systems or pure bridged setups may have no non-empty address list.
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
