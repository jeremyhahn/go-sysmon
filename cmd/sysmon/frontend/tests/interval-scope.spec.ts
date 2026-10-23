import { test, expect, type Page } from '@playwright/test';
import { serveFleet } from './fixtures/fleet';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { openTab } from './fixtures/tabs';

/**
 * The interval control has to drive the poller that is actually feeding the
 * page (#97).
 *
 * While a peer is selected, that poller is the aggregator's peer refresh, not
 * this node's monitor: changing the local rate from a peer's page would alter a
 * cadence nobody is looking at and leave the displayed data arriving exactly as
 * before. Worse, the dropdown would then read 500ms over figures collected once
 * a second -- a control reporting a rate nothing runs at.
 */

const peerSnapshot = {
  ...snapshot,
  host: { ...snapshot.host, hostname: 'fs-host' },
};

function fleetNode(name: string, hostname: string) {
  return {
    name, self: false, reachable: true, error: '',
    age_seconds: 1, status: 'healthy', hostname, version: '1.0.0',
    uptime_seconds: 3600, critical_count: 0, warning_count: 0,
    cpu_percent: 5, cpu_available: true, memory_percent: 20, swap_percent: 0, load_per_core: 0.1,
    worst_disk_percent: 30, raid_degraded: false, smart_unhealthy: 0,
    max_temp_c: 40, power_w: 0, net_rx_rate: 0, net_tx_rate: 0,
  };
}

/** posts records every scope the page asked for, and answers the way the
 * server does: with the cadence actually in force, floored. */
function setup(page: Page) {
  const posts: Array<{ interval_ms: number; scope?: string }> = [];

  const install = async () => {
    await mockSnapshotFeed(page);

    await page.route(/\/api\/interval$/, async (route) => {
      if (route.request().method() === 'POST') {
        const body = route.request().postDataJSON() as { interval_ms: number; scope?: string };
        posts.push(body);
        // The floor the aggregator applies to peer polling.
        const fleetMs = body.scope === 'fleet' ? Math.max(body.interval_ms, 1000) : 1000;
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ interval_ms: 500, fleet_interval_ms: fleetMs }),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ interval_ms: 500, fleet_interval_ms: 1000 }),
      });
    });

    await serveFleet(page, () => ({ nodes: [fleetNode('fs', 'fs-host')] }));

    await page.route(/\/api\/fleet\/fs\/snapshot$/, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ snapshot: peerSnapshot, reachable: true, error: '', age_seconds: 1 }),
      });
    });
  };

  return { posts, install };
}

async function selectInterval(page: Page, label: string) {
  await page.getByLabel('Polling interval').selectOption({ label });
}

test.describe('Interval scope', () => {
  test('the local monitor is driven while this node is shown', async ({ page }) => {
    const { posts, install } = setup(page);
    await install();
    await page.goto('/');

    await selectInterval(page, '5s');
    await expect.poll(() => posts.length).toBeGreaterThan(0);

    expect(posts.at(-1)?.scope ?? 'local').toBe('local');
  });

  test('the fleet poller is driven while a peer is shown', async ({ page }) => {
    const { posts, install } = setup(page);
    await install();
    await page.goto('/');

    await openTab(page, 'Fleet');
    await page.getByRole('button', { name: /fs/ }).first().click();

    await selectInterval(page, '5s');
    await expect.poll(() => posts.length).toBeGreaterThan(0);

    expect(posts.at(-1)?.scope).toBe('fleet');
    expect(posts.at(-1)?.interval_ms).toBe(5000);
  });

  test('a request the server floors is reported at the rate actually served', async ({ page }) => {
    const { install } = setup(page);
    await install();
    await page.goto('/');

    await openTab(page, 'Fleet');
    await page.getByRole('button', { name: /fs/ }).first().click();

    // 500ms is below the aggregator's floor, so peers still refresh every
    // second and the page must say so rather than showing the request back.
    await selectInterval(page, '500ms');

    await expect(page.getByText('peers every 1s')).toBeVisible();
  });
});
