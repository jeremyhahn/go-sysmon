import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { serveFleet } from './fixtures/fleet';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { openTab } from './fixtures/tabs';

/**
 * Regression test for stale peer data presented as live.
 *
 * The aggregator keeps a peer's last good snapshot when the peer stops
 * answering, so GET /api/fleet/{name}/snapshot keeps returning 200 with a full
 * body. Before the freshness envelope, the dashboard could not tell that body
 * apart from a fresh collection: every panel showed the frozen numbers as
 * current, and because the poll repeats on the refresh interval, the identical
 * reading was folded into the history charts again and again -- a flat line
 * that reads as "measured and steady" for a machine that had gone silent.
 *
 * What is asserted here is the visible half of the fix: an unreachable peer is
 * marked, so an operator reading the panels knows the numbers are old.
 */

const peerSnapshot = {
  ...snapshot,
  host: { ...snapshot.host, hostname: 'fs-host' },
};

function fleetNode(name: string, hostname: string, reachable: boolean) {
  return {
    name,
    self: false,
    reachable,
    error: reachable ? '' : 'dial tcp: connect: connection refused',
    age_seconds: reachable ? 1 : 754,
    status: reachable ? 'healthy' : 'unreachable',
    hostname,
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
    raid_degraded: false,
    smart_unhealthy: 0,
    max_temp_c: 40,
    power_w: 0,
    net_rx_rate: 0,
    net_tx_rate: 0,
  };
}

/**
 * setup serves a fleet whose peer starts healthy and can be killed mid-session
 * by flipping the returned flag.
 *
 * The peer must be reachable when it is SELECTED: the fleet dashboard disables
 * the card of an unreachable node, so there is no way to drill into one that is
 * already down. The failure this guards is therefore the sequence that actually
 * happens in practice -- you are watching a node and it dies underneath you --
 * not picking a dead node off the list.
 */
function setup(page: Page) {
  const state = { reachable: true };

  const body = () => ({
    error: state.reachable ? '' : 'dial tcp: connect: connection refused',
    age: state.reachable ? 1 : 754,
  });

  const install = async () => {
    await mockSnapshotFeed(page);

    await serveFleet(page, () => ({
      nodes: [fleetNode('fs', 'fs-host', state.reachable)],
    }));

    await page.route(/\/api\/fleet\/fs\/snapshot$/, async (route) => {
      const b = body();
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          snapshot: peerSnapshot,
          reachable: state.reachable,
          error: b.error,
          age_seconds: b.age,
        }),
      });
    });
  };

  return { state, install };
}

/** Opens the fleet dashboard and drills into the peer. */
async function selectPeer(page: Page) {
  await openTab(page, 'Fleet');
  await page.getByRole('button', { name: /fs/ }).first().click();
}

test.describe('Stale peer data', () => {
  test('a peer that dies while selected is visibly marked stale', async ({
    page,
  }) => {
    const { state, install } = setup(page);
    await install();
    await page.goto('/');
    await selectPeer(page);

    // While healthy the badge must be absent, or it is noise operators learn
    // to ignore.
    await expect(page.getByText(/stale/i)).toHaveCount(0);

    // The peer goes away. The aggregator keeps serving its last snapshot, so
    // every panel still shows numbers -- which is exactly why the badge has to
    // appear, and why this is the dangerous case rather than a blank page.
    state.reachable = false;

    const badge = page.getByText(/stale/i).first();
    await expect(badge).toBeVisible({ timeout: 15000 });

    // Age is rounded to whole units -- 754s reads as "13m old" -- because false
    // precision on a frozen reading invites more trust than it deserves.
    await expect(badge).toContainText(/13m old/);
  });
});
