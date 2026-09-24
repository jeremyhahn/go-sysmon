import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { serveFleet } from './fixtures/fleet';

/**
 * A fleet peer held under a name it does not write its history as (#162).
 *
 * `database` pointing at a node whose fleet.local_name is `db-01` passes every
 * load-time check and runs perfectly well, but splits the node's history across
 * two names, so a chart queried by either can come up empty with nothing to
 * say why. The aggregator now compares the two names and sends
 * `name_match`; the card must turn "different" into a warning that names both,
 * and keep "unknown" -- an older peer that did not say -- quiet.
 */

function node(name: string, extra: Record<string, unknown>) {
  return {
    name,
    self: false,
    reachable: true,
    error: '',
    age_seconds: 1,
    status: 'healthy',
    hostname: `${name}-host`,
    version: '1.0.0',
    uptime_seconds: 3600,
    critical_count: 0,
    warning_count: 0,
    cpu_percent: 5,
    memory_percent: 20,
    swap_percent: 0,
    load_per_core: 0.1,
    worst_disk_percent: 30,
    raid_arrays: 0,
    raid_degraded: false,
    smart_capable: 0,
    smart_unhealthy: 0,
    max_temp_c: 40,
    power_w: 0,
    net_rx_rate: 0,
    net_tx_rate: 0,
    ...extra,
  };
}

async function fleetWith(page: Page, nodes: unknown[]) {
  await mockSnapshotFeed(page);
  await serveFleet(page, { nodes: nodes as Record<string, unknown>[] });
  await page.goto('/');
  await page.getByRole('button', { name: 'Fleet', exact: true }).click();
}

/** The card for one node; see fleet-health.spec.ts for why it is anchored so. */
function card(page: Page, name: string) {
  return page.locator('button.card').filter({ has: page.getByRole('heading', { name, exact: true }) });
}

test('a peer held under a name it does not answer to is flagged with both names', async ({ page }) => {
  await fleetWith(page, [
    node('database', { node_name: 'db-01', name_match: 'different' }),
    node('web-01', { node_name: 'web-01', name_match: 'same' }),
  ]);

  const warning = card(page, 'database').getByTestId('name-mismatch');
  await expect(warning).toBeVisible();
  await expect(warning).toContainText('database');
  await expect(warning).toContainText('db-01');
  await expect(warning).toContainText('fleet.local_name');

  // The agreeing peer beside it says nothing about its name at all.
  await expect(card(page, 'web-01')).toBeVisible();
  await expect(card(page, 'web-01').getByTestId('name-mismatch')).toHaveCount(0);
  await expect(card(page, 'web-01').getByTestId('name-unverified')).toHaveCount(0);
});

test('a peer that did not report its name is marked quietly, not warned about', async ({ page }) => {
  await fleetWith(page, [node('old-peer', { name_match: 'unknown' })]);

  const c = card(page, 'old-peer');
  await expect(c.getByTestId('name-unverified')).toBeVisible();
  await expect(c.getByTestId('name-mismatch')).toHaveCount(0);
  // Quiet means it is not dressed as a warning either.
  await expect(c.locator('.alert-warning')).toHaveCount(0);
});

test('a server that predates the field shows nothing about names', async ({ page }) => {
  await fleetWith(page, [node('legacy', {})]);

  const c = card(page, 'legacy');
  await expect(c).toBeVisible();
  await expect(c.getByTestId('name-mismatch')).toHaveCount(0);
  await expect(c.getByTestId('name-unverified')).toHaveCount(0);
});
