import { test, expect, type Locator } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { dashboardTab, openTab, type TabLabel } from './fixtures/tabs';

test.describe('Core Application', () => {
  test('page loads successfully with correct title', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveTitle(/System Monitor/);
  });

  test('navbar is visible with app name', async ({ page }) => {
    await page.goto('/');
    const navbar = page.locator('nav.navbar');
    await expect(navbar).toBeVisible();
    await expect(navbar.getByText('System Monitor')).toBeVisible();
  });

  test('connection status indicator is present', async ({ page }) => {
    await page.goto('/');
    // The connection status badge shows "Connected" or "Disconnected"
    // and the animated pulse dot is always in the navbar
    const statusDot = page.locator('nav .rounded-full.animate-pulse').first();
    await expect(statusDot).toBeVisible();
  });

  test('connection status transitions to Connected once the event stream opens', async ({ page }) => {
    await page.goto('/');
    // Wait for the "Connected" text to appear in the navbar status area
    const connectedText = page.locator('nav').getByText('Connected', { exact: true });
    await expect(connectedText).toBeVisible({ timeout: 10000 });
  });

  test('tab navigation renders each panel', async ({ page }) => {
    // Served from the fixture, which has every panel's hardware, so each tab
    // is asserted on what it draws rather than on "main is still there" --
    // which held for any tab and any host, and so tested neither (#163).
    await mockSnapshotFeed(page, { gpus: 1, npus: 1, containers: 1 });
    await page.goto('/');

    const tabs: Array<{ label: TabLabel; content: Locator }> = [
      { label: 'Host', content: page.getByRole('heading', { name: 'fixture-host' }) },
      { label: 'CPU', content: page.getByText('Overall CPU') },
      { label: 'GPU', content: page.getByText('GPU Util').first() },
      { label: 'NPU', content: page.getByText('Intel AI Boost').first() },
      { label: 'Memory', content: page.getByText('RAM Usage') },
      { label: 'Storage', content: page.locator('h3').filter({ hasText: '/dev/nvme0n1' }) },
      { label: 'Network', content: page.locator('h3').filter({ hasText: /^\s*enp131s0\s*$/ }) },
      { label: 'Virtualization', content: page.getByRole('heading', { name: 'Virtual Machines' }) },
      { label: 'Containers', content: page.getByText('container-0').first() },
      { label: 'Overview', content: page.getByText('Load Avg') },
    ];

    for (const tab of tabs) {
      await openTab(page, tab.label);
      await expect(tab.content, `${tab.label} panel`).toBeVisible();
    }
  });

  test('a landing tab is active by default', async ({ page }) => {
    await page.goto('/');
    // Which tab lands depends on configuration: a node with fleet peers opens
    // on Fleet, a standalone node on Overview. Assert the invariant (exactly
    // one tab is active, and it is the expected one for this server) rather
    // than hard-coding Overview, which silently couples this test to the
    // harness running without peers.
    const fleetBtn = dashboardTab(page, 'Fleet');
    const expected = (await fleetBtn.count()) > 0 ? fleetBtn : dashboardTab(page, 'Overview');
    await expect(expected).toHaveClass(/tab-active/);
    await expect(page.locator('.tab-active')).toHaveCount(1);
  });

  // Which theme is chosen, and when it is applied, is tests/theme.spec.ts.
  // What survives here is the invariant every daisyUI surface depends on: the
  // attribute is always present and always names a theme that exists in the
  // bundle. Without it the page renders with no theme variables at all.
  test('a daisyUI theme is always applied via data-theme', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('html')).toHaveAttribute(
      'data-theme',
      /^(light|dark)$/
    );
  });

  test('footer is visible', async ({ page }) => {
    await page.goto('/');
    const footer = page.locator('footer');
    await expect(footer).toBeVisible();
    await expect(footer).toContainText('go-sysmon');
  });
});
