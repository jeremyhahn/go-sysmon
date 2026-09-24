import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';
import { fleetNode, serveFleet } from './fixtures/fleet';

/**
 * The fleet view over a tree of aggregators (#190).
 *
 * The root lists every node of the tree; a node of a child aggregator's
 * subtree says which child it is reached through, and opening it asks the root,
 * which routes the drill-in to that child. A child that stopped answering takes
 * its subtree with it -- unknown, never healthy -- and a subtree that cannot be
 * composed, or a name two nodes share, is said, never hidden.
 */

function tree() {
  return {
    nodes: [
      fleetNode('a1', { via: 'mid-a', hostname: 'a1.lan' }),
      fleetNode('b1', {
        via: 'mid-b', reachable: false, status: 'unreachable', age_seconds: 600,
        error: 'unreachable through mid-b: fleet: request to peer mid-b failed: connection refused',
      }),
      fleetNode('dup', { name_conflict: true }),
      fleetNode('dup', { via: 'mid-a', name_conflict: true }),
      fleetNode('loop', { aggregates: 3, subtree_error: 'the fleet loops back to root through loop; remove one of the two peer entries' }),
      fleetNode('mid-a', { aggregates: 2 }),
      fleetNode('mid-b', {
        aggregates: 1, reachable: false, status: 'unreachable', age_seconds: 600,
        error: 'fleet: request to peer mid-b failed: connection refused',
        subtree_error: 'fleet: request to peer mid-b failed: connection refused',
      }),
      fleetNode('root', { self: true, aggregates: 6 }),
    ],
    rollup: {
      nodes: 8, healthy: 6, warning: 0, critical: 0, unreachable: 2,
      longest_absence_seconds: 600, never_answered: 0, reporting: 6, swap_unknown: 0,
      subtree_errors: 1, name_conflicts: 2,
      attention: [{ name: 'mid-b', status: 'unreachable', age_seconds: 600, subtree: 1, reasons: ['connection refused'] }],
      builds: [], build_groups: 0,
    },
  };
}

async function open(page: Page) {
  await mockSnapshotFeed(page);
  await serveFleet(page, tree());
  await page.goto('/');
  await openTab(page, 'Fleet');
}

function card(page: Page, name: string) {
  return page.locator('button.card').filter({ has: page.getByRole('heading', { name, exact: true }) });
}

test('a subtree node says which aggregator it is reached through', async ({ page }) => {
  await open(page);
  await expect(card(page, 'a1').getByTestId('node-via')).toHaveText('via mid-a');
  await expect(card(page, 'mid-a').getByTestId('node-via')).toHaveCount(0);
  await expect(card(page, 'mid-a').getByTestId('node-aggregates')).toHaveText('aggregates 2');
  await expect(card(page, 'root').getByTestId('node-aggregates')).toHaveText('aggregates 6');
});

test('opening a subtree node asks the root, which routes it', async ({ page }) => {
  await open(page);
  const asked = page.waitForRequest((r) => r.url().endsWith('/api/fleet/a1/snapshot'));
  await card(page, 'a1').click();
  await asked;
  await expect(page.getByRole('button', { name: 'Return to fleet dashboard' })).toBeVisible();
});

test('a lost child takes its subtree with it, and says how many nodes', async ({ page }) => {
  await open(page);
  await expect(card(page, 'b1')).toBeDisabled();
  await expect(card(page, 'b1')).toContainText('unreachable through mid-b');
  await expect(card(page, 'mid-b').getByTestId('subtree-error')).toContainText('connection refused');
  await expect(page.getByRole('list', { name: 'Nodes needing attention' })).toContainText(
    'no reading for 10m; 1 node below it unknown — connection refused'
  );
  await expect(page.getByTestId('fleet-headline')).toContainText('8 nodes: 6 healthy, 0 warning, 0 critical, 2 unreachable');
});

test('a refused subtree and a shared name are said, not hidden', async ({ page }) => {
  await open(page);
  await expect(card(page, 'loop').getByTestId('subtree-error')).toContainText('loops back to root');
  const dups = card(page, 'dup');
  await expect(dups).toHaveCount(2);
  for (const d of await dups.all()) {
    await expect(d).toBeDisabled();
    await expect(d.getByTestId('name-conflict')).toBeVisible();
  }
  const notes = page.getByTestId('fleet-tree-note');
  await expect(notes).toHaveText([
    "1 aggregator's fleet is not shown: see its card",
    '2 nodes share a name with another node and cannot be opened',
  ]);
});

// A node whose summary was not fetched for this view -- a whole-fleet view on
// a tree, or a child too slow to answer in time -- is shown by its index entry:
// its state, and a note that its figures are not loaded, never zeros.
test('a node known only by its index entry says its figures are not loaded', async ({ page }) => {
  await mockSnapshotFeed(page);
  const t = tree();
  t.nodes[0] = fleetNode('a1', {
    via: 'mid-a', source: 'index', cpu_percent: 0, memory_percent: 0,
    error: 'figures not available through mid-a: timed out',
  });
  await serveFleet(page, t);
  await page.goto('/');
  await openTab(page, 'Fleet');
  const a1 = card(page, 'a1');
  await expect(a1.getByTestId('figures-not-loaded')).toContainText('timed out');
  await expect(a1.getByText('CPU', { exact: true })).toHaveCount(0);
});
