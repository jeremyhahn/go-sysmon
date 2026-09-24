import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { serveFleet } from './fixtures/fleet';

/**
 * The fleet view carries no peer URL (#120).
 *
 * GET /api/fleet is unauthenticated, and a peer's URL is its scheme, address
 * and port on the internal network, so the aggregator stopped sending it. The
 * card used to fall back to that URL for a peer that had never answered and so
 * had no host name to show; it now falls back to the peer's name, which is how
 * every part of the dashboard addresses a node.
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
    cpu_available: true,
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

test('a peer that has never answered is subtitled with its name', async ({ page }) => {
  await fleetWith(page, [
    node('db-01', {
      reachable: false,
      status: 'unreachable',
      age_seconds: 0,
      hostname: '',
      uptime_seconds: 0,
      error: 'fleet: request to peer db-01 failed: connection refused',
    }),
    node('web-01', {}),
  ]);

  const never = card(page, 'db-01');
  await expect(never.getByTestId('node-subtitle')).toHaveText('db-01');
  await expect(never).toContainText('Unreachable, never reported');
  await expect(never).toContainText('connection refused');

  // A peer that has answered is still subtitled with the host name it reports.
  await expect(card(page, 'web-01').getByTestId('node-subtitle')).toContainText('web-01-host');
});

test('the fleet page renders no address even if a server still sends one', async ({ page }) => {
  // An aggregator on a build before #120 still sends url. The page must not
  // pick it up from anywhere.
  await fleetWith(page, [
    node('db-01', {
      url: 'http://198.51.100.10:8081',
      reachable: false,
      status: 'unreachable',
      age_seconds: 0,
      hostname: '',
      uptime_seconds: 0,
      error: 'fleet: request to peer db-01 failed: timed out',
    }),
  ]);

  await expect(card(page, 'db-01').getByTestId('node-subtitle')).toHaveText('db-01');
  await expect(page.locator('body')).not.toContainText('198.51.100.10');
});
