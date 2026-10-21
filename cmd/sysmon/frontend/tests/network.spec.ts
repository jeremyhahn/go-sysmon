import { test, expect, type Page } from '@playwright/test';
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
    // The rate display uses ↑ and ↓ arrows with bit-rate values
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

/**
 * Interface rates are shown in decimal bits per second, as links are rated
 * (#257): the collector reports bytes per second, and a 1 Gbps port moving
 * 125 MB/s should read as the 1 Gbps it is. Byte totals stay in bytes.
 */
test.describe('Network rates', () => {
  test('interface rates are shown in bits per second', async ({ page }) => {
    await mockSnapshotFeed(page, {
      networks: [{ name: 'enp131s0', bytes_sent_rate: 125_000, bytes_recv_rate: 187_500_000, bytes_sent: 2048 }],
    });
    await page.goto('/');
    await openTab(page, 'Network');

    await expect(page.locator('.text-success').filter({ hasText: /↑/ }).first()).toHaveText(/↑\s*1\.0 Mbps/);
    await expect(page.locator('.text-info').filter({ hasText: /↓/ }).first()).toHaveText(/↓\s*1\.5 Gbps/);
    await expect(page.locator('div').filter({ hasText: /^Sent:/ }).first()).toContainText('2.0 KB');
  });
});

/**
 * A wifi interface shows its radio statistics from /proc/net/wireless (#259);
 * a wired one shows none.
 */
test.describe('Wireless detail', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page, {
      networks: [
        { name: 'enp131s0' },
        {
          name: 'wlp2s0',
          kind: 'wifi',
          wireless: {
            ssid: 'home-net',
            bssid: '02:00:00:00:00:02',
            link_quality: 62,
            signal_level_dbm: -48,
            noise_level_dbm: -95,
          },
        },
        {
          name: 'wlp3s0',
          kind: 'wifi',
          wireless: { link_quality: 0, signal_level_dbm: -90, noise_level_dbm: 0 },
        },
      ],
    });
    await page.goto('/');
    await openTab(page, 'Network');
  });

  const card = (page: Page, name: string) =>
    page.locator('.card').filter({ has: page.locator('h3', { hasText: new RegExp(`^\\s*${name}\\s*$`) }) });

  test('a wifi interface shows quality, signal and noise', async ({ page }) => {
    const wireless = card(page, 'wlp2s0').getByTestId('wireless');
    await expect(wireless).toBeVisible();
    await expect(wireless).toContainText('Quality: 62');
    await expect(wireless).toContainText('Signal: -48 dBm');
    await expect(wireless).toContainText('Noise: -95 dBm');
  });

  test('an associated wifi interface shows its SSID and BSSID', async ({ page }) => {
    const wireless = card(page, 'wlp2s0').getByTestId('wireless');
    await expect(wireless).toContainText('SSID: home-net');
    await expect(wireless).toContainText('BSSID: 02:00:00:00:00:02');
  });

  test('an unassociated wifi interface omits SSID, BSSID and a zero noise', async ({ page }) => {
    const wireless = card(page, 'wlp3s0').getByTestId('wireless');
    await expect(wireless).toContainText('Signal: -90 dBm');
    await expect(wireless).not.toContainText('SSID');
    await expect(wireless).not.toContainText('Noise');
  });

  test('a wired interface shows no wireless block', async ({ page }) => {
    await expect(card(page, 'enp131s0').locator('h3')).toBeVisible();
    await expect(card(page, 'enp131s0').getByTestId('wireless')).toHaveCount(0);
  });
});
