import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';
import { serveFleet } from './fixtures/fleet';

/**
 * The fleet list is ordered by name (owner ruling 2026-09-01).
 *
 * It used to sort worst-status-first so a fault surfaced on page one. That
 * reads as "not sorted at all" to anyone looking for a host: a warning node
 * sorts ahead of a healthy one whose name comes first, so on the real fleet
 * `fs`, `gpu-host` and `omega` were listed above `ci-node-1` and the list
 * looked shuffled.
 *
 * The ORDER is the server's since #15: it searches, sorts and pages, and
 * pkg/fleet's tests pin the order and the page boundaries cut from it. What is
 * checked here is the browser's half -- that it shows the page in the order it
 * was sent. A view that re-sorted each page could only disagree with the order
 * the pages were cut from.
 *
 * The fixture below has statuses deliberately ANTI-correlated with the
 * alphabet, because a fixture whose severity order happens to match its name
 * order passes under either ordering and proves nothing.
 */

function fleetNode(name: string, status: string) {
  const reachable = status !== 'unreachable';
  return {
    name,
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
  await serveFleet(page, {
    // The server's order: by name.
    nodes: NODES,
    rollup: {
      nodes: 4, healthy: 2, warning: 1, critical: 0, unreachable: 1,
      longest_absence_seconds: 754, never_answered: 0, reporting: 3,
      attention: [
        { name: 'zulu', status: 'unreachable', age_seconds: 754, reasons: ['connection refused'] },
        { name: 'yankee', status: 'warning', age_seconds: 0, reasons: ['cpu sustained'] },
      ],
      swap_unknown: 0, builds: [], build_groups: 0,
    },
  });
}

test('the fleet list is shown in the server\'s name order, not by severity', async ({ page }) => {
  await setup(page);
  await page.goto('/');
  await openTab(page, 'Fleet');

  // Each node is a card whose name is the only h3 inside it, so the h3 order is
  // the rendered order.
  const names = page.locator('h3');
  await expect(names).toHaveCount(EXPECTED.length);
  await expect(names).toHaveText(EXPECTED);
});

test('a degraded node is still counted even though it sorts last', async ({ page }) => {
  await setup(page);
  await page.goto('/');
  await openTab(page, 'Fleet');

  // Ordering by name only costs nothing in discoverability because the roll-up
  // counts faults, and names the nodes, independently of position. If this
  // ever stops being true, sorting by name genuinely would bury a fault and the
  // ruling needs revisiting.
  await expect(page.getByTestId('fleet-headline')).toContainText('1 warning, 0 critical, 1 unreachable');
  await expect(page.getByRole('list', { name: 'Nodes needing attention' })).toContainText('zulu');
});

/**
 * A five-second restart and a two-day outage both produce status
 * "unreachable" (#69). The word cannot distinguish them and neither can the
 * count, so the view has to say how long -- and `age_seconds` already carries
 * it, because the poller deliberately dates the DATA rather than the last
 * attempt.
 *
 * The fixture pairs a node absent for seconds with one absent for days, so an
 * implementation that rendered only the word, or only one of the two, fails.
 * Two nodes with similar ages would pass either way.
 */
test('an unreachable node says how long, and the summary reports the worst', async ({ page }) => {
  await mockSnapshotFeed(page);
  await serveFleet(page, {
    nodes: [
      // Mid-deploy: the data is nine seconds old.
      { ...fleetNode('alpha', 'unreachable'), age_seconds: 9 },
      // Gone since Tuesday: two days.
      { ...fleetNode('zulu', 'unreachable'), age_seconds: 172800 },
    ],
    rollup: {
      nodes: 2, healthy: 0, warning: 0, critical: 0, unreachable: 2,
      longest_absence_seconds: 172800, never_answered: 0, reporting: 0,
      attention: [], swap_unknown: 0, builds: [], build_groups: 0,
    },
  });

  await page.goto('/');
  await openTab(page, 'Fleet');

  // The brief one is not dressed as an outage...
  await expect(page.getByText(/Unreachable for 9s/)).toBeVisible();
  // ...and the sustained one is not dressed as a blip.
  await expect(page.getByText(/Unreachable for 2d/)).toBeVisible();

  // The summary carries the worst of them, so a reader who never opens a card
  // can still tell a deploy from a dead machine.
  await expect(page.getByTestId('fleet-headline')).toContainText('2 unreachable (no reading for 2d)');
});

/**
 * A node that has NEVER answered reports age_seconds 0 -- the sentinel meaning
 * "no reading", not "fetched just now". Rendering it through the duration
 * formatter would print "Unreachable for 0s", which reads as the freshest
 * possible failure and is the exact inversion of the truth.
 */
test('a node that never reported says so, rather than reporting a zero-second gap', async ({ page }) => {
  await mockSnapshotFeed(page);
  await serveFleet(page, {
    nodes: [{ ...fleetNode('never', 'unreachable'), age_seconds: 0, error: 'no data' }],
    rollup: {
      nodes: 1, healthy: 0, warning: 0, critical: 0, unreachable: 1,
      longest_absence_seconds: 0, never_answered: 1, reporting: 0,
      attention: [], swap_unknown: 0, builds: [], build_groups: 0,
    },
  });

  await page.goto('/');
  await openTab(page, 'Fleet');

  await expect(page.getByText(/Unreachable, never reported/)).toBeVisible();
  await expect(page.getByText(/Unreachable for 0s/)).toHaveCount(0);
  // And the summary does not claim a duration it does not have.
  await expect(page.getByText(/no reading for/)).toHaveCount(0);
  await expect(page.getByTestId('fleet-headline')).toContainText('(never answered)');
});
