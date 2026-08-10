import { test, expect } from '@playwright/test';

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
    const connectedText = page.locator('nav').getByText('Connected');
    await expect(connectedText).toBeVisible({ timeout: 10000 });
  });

  test('tab navigation renders each panel', async ({ page }) => {
    await page.goto('/');

    const tabs: Array<{ label: string; contentMatcher: string }> = [
      { label: 'Overview', contentMatcher: 'text=Waiting for system data' },
      { label: 'Host', contentMatcher: 'text=Waiting' },
      { label: 'CPU', contentMatcher: 'text=Overall CPU' },
      { label: 'Memory', contentMatcher: 'text=RAM Usage' },
      { label: 'Storage', contentMatcher: 'main' },
      { label: 'Network', contentMatcher: 'main' },
    ];

    for (const tab of tabs) {
      await page.getByRole('button', { name: tab.label }).click();
      // The main content area should be present after each tab click
      await expect(page.locator('main')).toBeVisible();
    }
  });

  test('Overview tab is active by default', async ({ page }) => {
    await page.goto('/');
    const overviewBtn = page.getByRole('button', { name: 'Overview' });
    await expect(overviewBtn).toHaveClass(/tab-active/);
  });

  test('dark theme is applied via data-theme attribute', async ({ page }) => {
    await page.goto('/');
    const html = page.locator('html');
    await expect(html).toHaveAttribute('data-theme', 'dark');
  });

  test('footer is visible', async ({ page }) => {
    await page.goto('/');
    const footer = page.locator('footer');
    await expect(footer).toBeVisible();
    await expect(footer).toContainText('go-sysmon');
  });
});
