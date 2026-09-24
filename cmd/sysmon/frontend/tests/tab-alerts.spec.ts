import { test, expect, type Page } from '@playwright/test';
import { alertEveryTab, mockSnapshotFeed } from './fixtures/mock-sse';
import { serveFleet } from './fixtures/fleet';
import { dashboardTab, openTab, TAB_IDS, type TabLabel } from './fixtures/tabs';

/**
 * Tab navigation with an alert firing in every category.
 *
 * #163 was reported as "storage.spec fails on a host with a storage alert".
 * The failure turned out to be the Storage panel throwing on that host's disks
 * (storage.spec.ts has the regression), but the suspicion was fair: every spec
 * the suite had ran on a quiet CI runner, so nothing showed that navigation
 * survives a noisy host. This is that host, made deliberate: every tab exists
 * (GPU and NPU are present, fleet mode is on), every one wears an alert dot,
 * and the shared tab helper still has to land on each.
 */

const LABELS = Object.keys(TAB_IDS) as TabLabel[];

async function openNoisyDashboard(page: Page) {
  await mockSnapshotFeed(page, { gpus: 1, npus: 1, alerts: alertEveryTab() });
  // Fleet mode on, so the Fleet tab exists too. No peers are needed for that.
  await serveFleet(page, () => ({ nodes: [] }));
  await page.goto('/');
  await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
  await expect(dashboardTab(page, 'Fleet')).toBeVisible();
}

test.describe('Tabs with an alert in every category', () => {
  test.beforeEach(async ({ page }) => {
    await openNoisyDashboard(page);
  });

  test('every tab wears its alert dot', async ({ page }) => {
    for (const label of LABELS) {
      await expect(
        dashboardTab(page, label).locator(`[title="${label} has active alerts"]`),
        `${label} tab has no alert dot`
      ).toHaveCount(1);
    }
  });

  test('every tab still opens by its exact name', async ({ page }) => {
    for (const label of LABELS) {
      await openTab(page, label);
      await expect(page.locator('.tab-active'), `opening ${label}`).toHaveCount(1);
    }
  });

  test('the alerts do reproduce the collision a loose locator falls into', async ({ page }) => {
    // Without this, the tests above could pass because the fixture never
    // produced the hazard at all. The navbar's alert list names each tab, so
    // while it is open a substring match on a label finds more than the tab.
    await page.getByRole('button', { name: 'Alerts', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Storage' })).not.toHaveCount(1);
    await expect(dashboardTab(page, 'Storage')).toHaveCount(1);
  });
});
