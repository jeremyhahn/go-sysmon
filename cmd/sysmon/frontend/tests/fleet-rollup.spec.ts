import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { serveFleet } from './fixtures/fleet';

/**
 * The fleet roll-up (#46) and the swap paging signal on the cards (#134).
 *
 * The roll-up's figures are the server's; these specs pin how they read. The
 * rule under all of it, ratified on #46: show the worst and name the node, or
 * show a sum with its denominator -- never a mean. And an unreachable node is
 * its own term, with how long it has been gone, never folded into a count of
 * the healthy.
 */

type Json = Record<string, unknown>;

function fleetNode(name: string, over: Json = {}): Json {
  const status = (over.status as string) ?? 'healthy';
  const reachable = status !== 'unreachable';
  return {
    name,
    self: false,
    reachable,
    error: reachable ? '' : 'dial tcp: connect: connection refused',
    age_seconds: reachable ? 1 : 8040,
    status,
    source: 'summary',
    hostname: `${name}-host`,
    version: '1.0.0',
    uptime_seconds: 3600,
    critical_count: 0,
    warning_count: 0,
    alerts: [],
    cpu_percent: 5,
    cpu_available: true,
    memory_percent: 20,
    swap_percent: 0,
    swap_counters_available: true,
    swap_rate_available: true,
    swap_in_rate: 0,
    swap_out_rate: 0,
    load_per_core: 0.1,
    worst_disk_percent: 30,
    raid_arrays: 0,
    raid_unhealthy: 0,
    raid_degraded: false,
    smart_capable: 0,
    smart_unhealthy: 0,
    max_temp_c: 40,
    power_w: 0,
    power_reported: false,
    net_rx_rate: 0,
    net_tx_rate: 0,
    ...over,
  };
}

function rollup(over: Json = {}): Json {
  return {
    nodes: 1,
    healthy: 1,
    warning: 0,
    critical: 0,
    unreachable: 0,
    longest_absence_seconds: 0,
    never_answered: 0,
    attention: [],
    reporting: 1,
    swap_unknown: 0, cpu_unknown: 0,
    builds: [],
    build_groups: 0,
    ...over,
  };
}

async function showFleet(page: Page, nodes: Json[], r: Json) {
  await mockSnapshotFeed(page);
  await serveFleet(page, { nodes, rollup: r });
  await page.goto('/');
  await page.getByRole('button', { name: 'Fleet', exact: true }).click();
}

function card(page: Page, name: string) {
  return page.locator('button.card').filter({ has: page.getByRole('heading', { name, exact: true }) });
}

const headline = (page: Page) => page.getByTestId('fleet-headline');

test('the headline counts every state, unreachable included, with how long it has been gone', async ({ page }) => {
  await showFleet(page, [fleetNode('alpha'), fleetNode('zulu', { status: 'unreachable' })], rollup({
    nodes: 11, healthy: 7, warning: 2, critical: 1, unreachable: 1,
    longest_absence_seconds: 8040, reporting: 10,
  }));

  await expect(headline(page)).toHaveText(
    '11 nodes: 7 healthy, 2 warning, 1 critical, 1 unreachable (no reading for 2h 14m)'
  );
});

test('a node that never answered is not given a zero-second absence', async ({ page }) => {
  await showFleet(page, [fleetNode('alpha'), fleetNode('never', { status: 'unreachable', age_seconds: 0 })], rollup({
    nodes: 2, healthy: 1, unreachable: 1, never_answered: 1,
  }));

  await expect(headline(page)).toHaveText(
    '2 nodes: 1 healthy, 0 warning, 0 critical, 1 unreachable (never answered)'
  );
  await expect(headline(page)).not.toContainText('0s');
});

// The server names at most five (types.RollupNames) and counts the rest; the
// view shows what it is sent and says how many it was not.
test('the nodes needing attention are named with their reasons, worst first, five at most', async ({ page }) => {
  const attention = [
    { name: 'ci-node-1', status: 'critical', age_seconds: 0, reasons: ['array md0 is inactive', 'CPU package at 80 °C'] },
    { name: 'gone', status: 'unreachable', age_seconds: 8040, reasons: ['connection refused'] },
    { name: 'fs', status: 'warning', age_seconds: 0, reasons: ['filesystem /srv is 92.1% full'] },
    { name: 'vm-host-2', status: 'warning', age_seconds: 0, reasons: ['cpu sustained'] },
    { name: 'vm-host-3', status: 'warning', age_seconds: 0, reasons: ['cpu sustained'] },
  ];
  await showFleet(page, [fleetNode('fs', { status: 'warning' })], rollup({
    nodes: 8, healthy: 1, warning: 5, critical: 1, unreachable: 1, attention,
  }));

  const list = page.getByRole('list', { name: 'Nodes needing attention' });
  const items = list.getByRole('listitem');
  await expect(items).toHaveCount(6);
  await expect(items.nth(0)).toContainText('ci-node-1');
  await expect(items.nth(0)).toContainText('array md0 is inactive; CPU package at 80 °C');
  await expect(items.nth(1)).toContainText('no reading for 2h 14m — connection refused');
  await expect(items.nth(2)).toContainText('filesystem /srv is 92.1% full');
  await expect(items.nth(5)).toHaveText('and 2 more');
  await expect(list).not.toContainText('vm-host-4');
});

test('choosing an unreachable node from the attention list narrows the list to its card', async ({ page }) => {
  await showFleet(page, [fleetNode('alpha'), fleetNode('gone', { status: 'unreachable' })], rollup({
    nodes: 2, healthy: 1, unreachable: 1, longest_absence_seconds: 8040,
    attention: [{ name: 'gone', status: 'unreachable', age_seconds: 8040, reasons: ['connection refused'] }],
  }));

  await page.getByRole('list', { name: 'Nodes needing attention' }).getByRole('button', { name: 'gone' }).click();
  await expect(page.locator('button.card h3')).toHaveText(['gone']);
});

test('power is a total with its denominator, and absent when no node has a sensor', async ({ page }) => {
  await showFleet(page, [fleetNode('ci-node-1', { power_w: 315, power_reported: true })], rollup({
    nodes: 10, healthy: 10, reporting: 10, power: { value: 579, nodes: 3, of: 10 },
  }));
  const power = page.getByTestId('fleet-power');
  await expect(power).toContainText('579.0 W');
  await expect(power).toContainText('across 3 of 10 nodes');
});

test('with a node unreachable, the power denominator says it covers the reachable nodes', async ({ page }) => {
  await showFleet(page, [fleetNode('ci-node-1')], rollup({
    nodes: 10, healthy: 9, unreachable: 1, reporting: 9, power: { value: 579, nodes: 3, of: 9 },
  }));
  await expect(page.getByTestId('fleet-power')).toContainText('across 3 of 9 reachable nodes');
});

test('no power sensor anywhere means no power figure, not 0 W', async ({ page }) => {
  await showFleet(page, [fleetNode('vm')], rollup());
  await expect(headline(page)).toBeVisible();
  await expect(page.getByTestId('fleet-power')).toHaveCount(0);
});

test('SMART and RAID count devices, with the nodes they were found on', async ({ page }) => {
  await showFleet(page, [fleetNode('ci-node-1')], rollup({
    nodes: 10, healthy: 10, reporting: 10,
    smart: { unhealthy: 0, devices: 25, nodes: 4, of: 10 },
    raid: { unhealthy: 1, devices: 5, nodes: 2, of: 10 },
  }));
  await expect(page.getByTestId('fleet-smart')).toContainText('0 unhealthy of 25 devices, on 4 of 10 nodes');
  await expect(page.getByTestId('fleet-raid')).toContainText('1 unhealthy of 5 arrays, on 2 of 10 nodes');
});

test('a fleet with no SMART devices or arrays says nothing about either', async ({ page }) => {
  await showFleet(page, [fleetNode('vm')], rollup());
  await expect(headline(page)).toBeVisible();
  await expect(page.getByTestId('fleet-smart')).toHaveCount(0);
  await expect(page.getByTestId('fleet-raid')).toHaveCount(0);
});

test('extremes name the node that holds them', async ({ page }) => {
  await showFleet(page, [fleetNode('fs')], rollup({
    cpu: { node: 'vm-host-4', value: 44.9 },
    disk: { node: 'fs', value: 92.1 },
    temperature: { node: 'ci-node-1', value: 83, kind: 'gpu' },
  }));
  const figures = page.getByTestId('fleet-figures');
  await expect(figures).toContainText('44.9% vm-host-4');
  await expect(figures).toContainText('92.1% fs');
  // Every component counts, and the kind is named: here a GPU.
  await expect(figures).toContainText('Hottest component');
  await expect(figures).toContainText('83.0°C ci-node-1 (GPU)');
});

test('swap paging: full swap with no paging, a paging node, and a node that cannot say', async ({ page }) => {
  await showFleet(page, [
    fleetNode('idle', { swap_percent: 98.6, swap_out_rate: 0 }),
    fleetNode('pager', { swap_percent: 51.1, swap_out_rate: 4194304, swap_in_rate: 1048576 }),
    fleetNode('old', {
      swap_percent: 100, swap_counters_available: false, swap_rate_available: false, source: 'snapshot',
    }),
  ], rollup({
    nodes: 3, healthy: 3, reporting: 3, swap_out: { node: 'pager', value: 4194304 }, swap_unknown: 1,
  }));

  await expect(card(page, 'idle').getByTestId('swap-cell')).toContainText('no paging');
  await expect(card(page, 'pager').getByTestId('swap-cell')).toContainText('out 4.0 MB/s, in 1.0 MB/s');
  // Unknown is its own answer: never rendered as "no paging".
  await expect(card(page, 'old').getByTestId('swap-cell')).toContainText('paging unknown');
  await expect(card(page, 'old').getByTestId('swap-cell')).not.toContainText('no paging');

  const swap = page.getByTestId('fleet-swap');
  await expect(swap).toContainText('4.0 MB/s pager');
  await expect(swap).toContainText('unknown on 1 node');
});

test('a node that cannot measure paging says so even with no swap in use', async ({ page }) => {
  await showFleet(page, [
    fleetNode('fresh', { swap_percent: 0, swap_rate_available: false }),
    fleetNode('noswap', { swap_percent: 0 }),
  ], rollup({ nodes: 2, healthy: 2, reporting: 2, swap_unknown: 1 }));

  await expect(card(page, 'fresh').getByTestId('swap-cell')).toContainText('paging unknown');
  // Measured, no swap, no paging: nothing to say.
  await expect(card(page, 'noswap').getByTestId('swap-cell')).toHaveCount(0);
});

test('CPU: a node whose usage was not measured shows a dash and is counted, not ranked as idle', async ({ page }) => {
  await showFleet(page, [
    fleetNode('busy', { cpu_percent: 67 }),
    fleetNode('starting', { cpu_percent: 0, cpu_available: false }),
  ], rollup({ nodes: 2, healthy: 2, reporting: 2, cpu: { node: 'busy', value: 67 }, cpu_unknown: 1 }));

  await expect(card(page, 'busy')).toContainText('67.0%');
  const unknown = card(page, 'starting').getByTestId('node-cpu-unknown');
  await expect(unknown).toHaveText('—');
  // The CPU cell holds the dash alone: no percentage, zero or otherwise.
  await expect(unknown.locator('..')).not.toContainText('%');

  const cpu = page.getByTestId('fleet-cpu');
  await expect(cpu).toContainText('67.0% busy');
  await expect(cpu).toContainText('unknown on 1 node');
});

test('CPU: a fleet with no measured node shows no busiest figure, only the count', async ({ page }) => {
  await showFleet(page, [fleetNode('starting', { cpu_percent: 0, cpu_available: false })],
    rollup({ cpu_unknown: 1 }));

  const cpu = page.getByTestId('fleet-cpu');
  await expect(cpu).toContainText('unknown on 1 node');
  await expect(cpu).not.toContainText('%');
});

test('network throughput names its coverage: nodes that measure it, of those reachable', async ({ page }) => {
  await showFleet(page, [fleetNode('alpha')], rollup({
    nodes: 10, healthy: 9, unreachable: 1, reporting: 9,
    network: { rx_rate: 1024, tx_rate: 1024, nodes: 8, of: 9 },
  }));
  await expect(page.getByTestId('fleet-network')).toContainText('across 8 of 9 reachable nodes');
});

test('swap-in with nothing left in swap still shows the paging', async ({ page }) => {
  await showFleet(page, [fleetNode('drain', { swap_percent: 0, swap_in_rate: 2048 })], rollup());
  await expect(card(page, 'drain').getByTestId('swap-cell')).toContainText('out 0 B/s, in 2.0 KB/s');
});

test('choosing this node from the attention list opens its dashboard', async ({ page }) => {
  await showFleet(page, [fleetNode('local', { self: true, status: 'warning', warning_count: 1 })], rollup({
    healthy: 0, warning: 1,
    attention: [{ name: 'local', status: 'warning', age_seconds: 0, reasons: ['cpu sustained'] }],
  }));
  await page.getByRole('list', { name: 'Nodes needing attention' }).getByRole('button', { name: 'local' }).click();
  await expect(page.getByRole('button', { name: 'Overview', exact: true })).toHaveClass(/tab-active/);
});

test('a card names the component its hottest reading came from', async ({ page }) => {
  await showFleet(page, [
    fleetNode('gpu-host', { max_temp_c: 88, max_temp_source: 'gpu' }),
    fleetNode('vm', { max_temp_c: 0 }),
  ], rollup({ nodes: 2, healthy: 2 }));
  await expect(card(page, 'gpu-host').getByTestId('max-temp')).toContainText('88.0°C GPU');
  // No thermometer: no cell, not 0 °C.
  await expect(card(page, 'vm').getByTestId('max-temp')).toHaveCount(0);
});

test('a node that cannot measure throughput has no network figure, not a zero', async ({ page }) => {
  await showFleet(page, [
    fleetNode('metal', { net_reported: true, net_rx_rate: 2048 }),
    fleetNode('ctr', { net_reported: false }),
  ], rollup({ nodes: 2, healthy: 2, reporting: 2, network: { rx_rate: 2048, tx_rate: 0, nodes: 1, of: 2 } }));
  await expect(card(page, 'metal').getByTestId('net-cell')).toContainText('2.0 KB/s');
  await expect(card(page, 'ctr').getByTestId('net-cell')).toHaveCount(0);
  await expect(page.getByTestId('fleet-network')).toContainText('across 1 of 2 nodes');
});

test('a peer polled by its whole snapshot is marked on its card', async ({ page }) => {
  await showFleet(page, [fleetNode('new'), fleetNode('old', { source: 'snapshot' })], rollup({ nodes: 2, healthy: 2 }));
  await expect(card(page, 'old').getByTestId('snapshot-source')).toBeVisible();
  await expect(card(page, 'new').getByTestId('snapshot-source')).toHaveCount(0);
});

test('a peer whose history is refused says so, with the reason', async ({ page }) => {
  const reason = 'the node collects every 10m0s, so its relayed samples can be 10m20s apart';
  await showFleet(page, [
    fleetNode('fast', { history: { state: 'recorded' } }),
    fleetNode('slow', { history: { state: 'refused', reason } }),
    fleetNode('old', { source: 'snapshot', history: { state: 'unverified', reason: 'no collection interval' } }),
  ], rollup({ nodes: 3, healthy: 3 }));

  const refused = card(page, 'slow').getByTestId('history-refused');
  await expect(refused).toBeVisible();
  await expect(refused).toHaveAttribute('title', reason);
  await expect(card(page, 'old').getByTestId('history-unverified')).toBeVisible();
  // Recorded is the normal case, and says nothing.
  await expect(card(page, 'fast').getByTestId('history-refused')).toHaveCount(0);
  await expect(card(page, 'fast').getByTestId('history-unverified')).toHaveCount(0);
});

test('a fleet recording no history shows no history badge', async ({ page }) => {
  await showFleet(page, [fleetNode('a'), fleetNode('b')], rollup({ nodes: 2, healthy: 2 }));
  await expect(page.getByTestId('history-refused')).toHaveCount(0);
  await expect(page.getByTestId('history-unverified')).toHaveCount(0);
});

test('builds are listed as identity, the stragglers named', async ({ page }) => {
  await showFleet(page, [fleetNode('gitea')], rollup({
    builds: [
      { commit: 'a46f6cf', count: 3, nodes: ['a', 'b', 'c'] },
      { commit: 'c00eb87', count: 1, nodes: ['gitea'] },
      { commit: '', count: 1, nodes: ['old'] },
    ],
    build_groups: 3,
  }));
  await expect(page.getByTestId('fleet-builds')).toHaveText(
    'Builds: 3 on a46f6cf, 1 on c00eb87 (gitea), 1 on an unstated build (old)'
  );
});

test('a tied build count names every group: no build is the common one', async ({ page }) => {
  await showFleet(page, [fleetNode('a')], rollup({
    builds: [
      { commit: 'a46f6cf', count: 1, nodes: ['a'] },
      { commit: 'c00eb87', count: 1, nodes: ['b'] },
    ],
    build_groups: 2,
  }));
  await expect(page.getByTestId('fleet-builds')).toHaveText('Builds: 1 on a46f6cf (a), 1 on c00eb87 (b)');
});

// At fleet scale the server sends the largest few groups and names the first
// few nodes of each, with the true sizes (#15). The line says what it left
// out rather than presenting five names as the whole group.
test('a large rollout reads as counts, naming a few nodes and saying how many more', async ({ page }) => {
  await showFleet(page, [fleetNode('n1')], rollup({
    builds: [
      { commit: 'a46f6cf', count: 6000, nodes: ['m1', 'm2', 'm3', 'm4', 'm5'] },
      { commit: 'c00eb87', count: 3990, nodes: ['n1', 'n2', 'n3', 'n4', 'n5'] },
      { commit: 'd1e2f3a', count: 4, nodes: ['p1', 'p2', 'p3', 'p4'] },
      { commit: 'e4f5a6b', count: 2, nodes: ['q1', 'q2'] },
      { commit: 'f7a8b9c', count: 1, nodes: ['r1'] },
    ],
    build_groups: 8,
  }));
  await expect(page.getByTestId('fleet-builds')).toHaveText(
    'Builds: 6000 on a46f6cf, 3990 on c00eb87 (n1, n2, n3, n4, n5 and 3985 more), ' +
      '4 on d1e2f3a (p1, p2, p3, p4), 2 on e4f5a6b (q1, q2), 1 on f7a8b9c (r1), and 3 other builds'
  );
});
