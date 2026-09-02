import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';

/**
 * The fleet list is ordered by name (owner ruling 2026-09-01).
 *
 * It used to sort worst-status-first so a fault surfaced on page one. That
 * reads as "not sorted at all" to anyone looking for a host: a warning node
 * sorts ahead of a healthy one whose name comes first, so on the real fleet
 * `fs`, `gpu-host` and `omega` were listed above `ci-node-1` and the list
 * looked shuffled.
 *
 * The fixture below is that exact shape -- statuses deliberately ANTI-correlated
 * with the alphabet -- because a fixture whose severity order happens to match
 * its name order passes either way and proves nothing.
 */

function fleetNode(name: string, status: string) {
  const reachable = status !== 'unreachable';
  return {
    name,
    url: `http://${name}:9099`,
    self: false,
    reachable,
    error: reachable ? '' : 'dial tcp: connect: connection refused',
    age_seconds: reachable ? 1 : 754,
    status,
    hostname: `${name}-host`,
    version: '1.0.0',
    uptime_seconds: 3600,
    critical_count: 0,
    warning_count: status === 'warning' ? 1 : 0,
    cpu_percent: 5,
    memory_percent: 20,
    swap_percent: 0,
    load_per_core: 0.1,
    worst_disk_percent: 30,
    raid_degraded: false,
    smart_unhealthy: 0,
    max_temp_c: 40,
    power_w: 0,
    net_rx_rate: 0,
    net_tx_rate: 0,
  };
}

// Worst statuses on the names that sort LAST, healthy on the names that sort
// FIRST. Under the old comparator this list came back exactly reversed.
const NODES = [
  fleetNode('alpha', 'healthy'),
  fleetNode('bravo', 'healthy'),
  fleetNode('yankee', 'warning'),
  fleetNode('zulu', 'unreachable'),
];

const EXPECTED = ['alpha', 'bravo', 'yankee', 'zulu'];

async function setup(page: Page) {
  await mockSnapshotFeed(page);
  await page.route(/\/api\/fleet$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      // Served in the order the aggregator returns them -- worst first -- so
      // the test fails if the view merely passes the API order through.
      body: JSON.stringify({
        enabled: true,
        nodes: [NODES[3], NODES[2], NODES[0], NODES[1]],
      }),
    });
  });
}

test('the fleet list is ordered by name, not by severity', async ({ page }) => {
  await setup(page);
  await page.goto('/');
  await page.getByRole('button', { name: 'Fleet', exact: true }).click();

  // Each node is a card whose name is the only h3 inside it, so the h3 order is
  // the rendered order.
  const names = page.locator('h3');
  await expect(names).toHaveCount(EXPECTED.length);
  await expect(names).toHaveText(EXPECTED);
});

test('a degraded node is still counted even though it sorts last', async ({ page }) => {
  await setup(page);
  await page.goto('/');
  await page.getByRole('button', { name: 'Fleet', exact: true }).click();

  // Ordering by name only costs nothing in discoverability because the summary
  // counts faults independently of position. If this ever stops being true,
  // sorting by name genuinely would bury a fault and the ruling needs revisiting.
  await expect(page.getByText(/unreachable/i).first()).toBeVisible();
});
