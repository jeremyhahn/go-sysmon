import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed, type MockOptions } from './fixtures/mock-sse';
import { dashboardTab, openTab } from './fixtures/tabs';

/**
 * The GPU and NPU tabs exist only on a node that reports the hardware (#153).
 *
 * Most of the fleet has neither, and both tabs used to open onto a "none
 * detected" message there. Whether a tab exists follows the snapshot being
 * viewed, so it can appear and disappear while the page is open.
 */

/** Serves `opts` and waits until the dashboard has drawn a snapshot. */
async function openDashboard(page: Page, opts: MockOptions) {
  await mockSnapshotFeed(page, opts);
  await page.goto('/');
  await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
}

test.describe('Accelerator tabs', () => {
  test('show when the node has a GPU and an NPU', async ({ page }) => {
    await openDashboard(page, { gpus: 1, npus: 1 });

    await expect(dashboardTab(page, 'GPU')).toBeVisible();
    await expect(dashboardTab(page, 'NPU')).toBeVisible();
  });

  test('are absent when the node has neither', async ({ page }) => {
    await openDashboard(page, { gpus: 0, npus: 0 });

    await expect(dashboardTab(page, 'GPU')).toHaveCount(0);
    await expect(dashboardTab(page, 'NPU')).toHaveCount(0);
    // The tabs every node has are unaffected.
    await expect(dashboardTab(page, 'CPU')).toBeVisible();
  });

  test('are decided independently', async ({ page }) => {
    await openDashboard(page, { gpus: 0, npus: 1 });

    await expect(dashboardTab(page, 'GPU')).toHaveCount(0);
    await expect(dashboardTab(page, 'NPU')).toBeVisible();
  });

  test('NPU tab opens the NPU panel', async ({ page }) => {
    await openDashboard(page, { gpus: 0, npus: 1 });

    await openTab(page, 'NPU');

    await expect(page.getByText('Intel AI Boost').first()).toBeVisible();
  });

  test('a user on a tab that disappears lands on the Overview', async ({ page }) => {
    await openDashboard(page, { gpus: 1 });
    await openTab(page, 'GPU');

    // The fixture stream ends after one frame and the page reconnects. The
    // newest route wins, so the next frame is a node without a GPU -- the same
    // shape as drilling into a peer that lacks one.
    await mockSnapshotFeed(page, { gpus: 0 });

    await expect(dashboardTab(page, 'GPU')).toHaveCount(0, { timeout: 10000 });
    await expect(dashboardTab(page, 'Overview')).toHaveClass(/tab-active/);
    await expect(page.locator('.tab-active')).toHaveCount(1);
  });
});

test.describe('Overview accelerator gauges', () => {
  function npuGaugeCards(page: Page) {
    return page.locator('.card').filter({ has: page.locator('p', { hasText: /^\s*NPU\s*\d*\s*$/ }) });
  }

  test('show one NPU card per device', async ({ page }) => {
    await openDashboard(page, { npus: 2 });

    await expect(npuGaugeCards(page)).toHaveCount(2);
    await expect(npuGaugeCards(page).first()).toContainText('NPU 0');
    await expect(npuGaugeCards(page).first()).toContainText('active');
  });

  test('a single NPU is not numbered', async ({ page }) => {
    await openDashboard(page, { npus: 1 });

    await expect(npuGaugeCards(page)).toHaveCount(1);
    await expect(npuGaugeCards(page).first()).not.toContainText('NPU 0');
  });

  test('draw no NPU card on a node without one', async ({ page }) => {
    await openDashboard(page, { npus: 0 });

    await expect(page.locator('p').filter({ hasText: /^CPU$/ }).first()).toBeVisible();
    await expect(npuGaugeCards(page)).toHaveCount(0);
  });
});
