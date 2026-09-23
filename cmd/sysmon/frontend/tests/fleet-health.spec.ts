import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * A health indicator must not be green because there was nothing to check.
 *
 * `raid_degraded: false` is produced both by "this host has no arrays" and by
 * "its arrays are fine"; `smart_unhealthy: 0` both by "no disk reported SMART"
 * and by "every disk is healthy". The fleet card rendered each pair as a green
 * ok, so a node with no RAID advertised a healthy RAID subsystem it does not
 * have — and ci-node-1, whose PERC and SAS devices report nothing, advertised
 * healthy SMART while its own node page said N/A for every one of them.
 *
 * The cells now carry a denominator and omit themselves when it is zero, which
 * is the treatment swap, temperature and power already use in the same card.
 */

function node(name: string, health: Record<string, number | boolean>) {
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
    ...health,
  };
}

async function fleetWith(page: Page, nodes: unknown[]) {
  await mockSnapshotFeed(page);
  await page.route(/\/api\/fleet$/, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ enabled: true, nodes }),
    })
  );
  await page.goto('/');
  await openTab(page, 'Fleet');
}

/**
 * The card for one node, so a two-node fixture cannot let one card's cell
 * satisfy an assertion about the other.
 *
 * Anchored on the button that IS the card -- each node card is a `button.card`
 * containing exactly one `h3` with the node's name. A generic `div` filtered by
 * the heading matches every ancestor, and `.last()` picks an inner wrapper that
 * holds the name and none of the cells: assertions of ABSENCE then pass against
 * any implementation at all, which is how the first version of this file
 * reported a green first test while the other three failed on the same
 * locator.
 */
function card(page: Page, name: string) {
  return page.locator('button.card').filter({ has: page.getByRole('heading', { name, exact: true }) });
}

test('a node with no RAID and no SMART says nothing about either', async ({ page }) => {
  await fleetWith(page, [node('bare', {})]);

  const bare = card(page, 'bare');
  await expect(bare).toBeVisible();
  // Not "ok" in green: there is no RAID subsystem to be ok.
  await expect(bare.getByText('RAID', { exact: true })).toHaveCount(0);
  await expect(bare.getByText('SMART', { exact: true })).toHaveCount(0);
});

test('a node with arrays and readable SMART reports both, with the denominator', async ({
  page,
}) => {
  await fleetWith(page, [
    node('full', { raid_arrays: 2, raid_degraded: false, smart_capable: 6, smart_unhealthy: 0 }),
  ]);

  const full = card(page, 'full');
  await expect(full.getByText('RAID', { exact: true })).toBeVisible();
  await expect(full.getByText('SMART', { exact: true })).toBeVisible();
  // Both subsystems report their state as the value: two "ok"s, not one.
  await expect(full.getByText('ok', { exact: true })).toHaveCount(2);
  // "ok" alone cannot say how many were checked, which is the whole defect
  // #44 was filed about, so BOTH cells keep their denominator -- beside the
  // state as evidence rather than above it as a claim.
  await expect(full.getByText('6 checked')).toBeVisible();
  await expect(full.getByText('2 arrays')).toBeVisible();
});

test('a degraded array and a failing disk still read as faults', async ({ page }) => {
  await fleetWith(page, [
    node('sick', { raid_arrays: 1, raid_degraded: true, smart_capable: 4, smart_unhealthy: 2 }),
  ]);

  const sick = card(page, 'sick');
  await expect(sick.getByText('degraded', { exact: true })).toBeVisible();
  await expect(sick.getByText('2 of 4 unhealthy')).toBeVisible();
});

test('one node saying nothing does not silence another that has something to say', async ({
  page,
}) => {
  // The fixture that would have hidden a regression: with a single node, a
  // cell missing everywhere and a cell missing correctly look identical.
  await fleetWith(page, [
    node('bare', {}),
    node('full', { raid_arrays: 1, smart_capable: 3, smart_unhealthy: 1 }),
  ]);

  await expect(card(page, 'bare').getByText('SMART', { exact: true })).toHaveCount(0);
  await expect(card(page, 'full').getByText('1 of 3 unhealthy')).toBeVisible();
});

test('a fault from a server that sends no counts is still shown', async ({ page }) => {
  // The mixed-version case. The frontend is embedded in the binary that serves
  // /api/fleet, so the two normally match -- but a tab left open across a
  // ROLLBACK holds this bundle against an older server whose payload has no
  // raid_arrays and no smart_capable. Gating purely on the denominator would
  // then hide a genuinely degraded array behind a missing field.
  //
  // A missing denominator is a reason to say less about a healthy host. It is
  // never a reason to stay quiet about a broken one.
  const legacy = node('old-server', {});
  delete (legacy as Record<string, unknown>).raid_arrays;
  delete (legacy as Record<string, unknown>).smart_capable;
  legacy.raid_degraded = true;
  legacy.smart_unhealthy = 3;

  await fleetWith(page, [legacy]);

  const old = card(page, 'old-server');
  await expect(old.getByText('degraded', { exact: true })).toBeVisible();
  // The count survives without a denominator, stated as the fault it is
  // rather than as "3 of 0", which is not a fact about anything.
  await expect(old.getByText('3 unhealthy')).toBeVisible();
  await expect(old.getByText(/of 0/)).toHaveCount(0);
});

test('a healthy host from a server that sends no counts stays quiet', async ({ page }) => {
  // The other half: without a denominator there is nothing to say about a host
  // reporting no faults, and saying "ok" would be the original bug arriving by
  // a different route.
  const legacy = node('old-quiet', {});
  delete (legacy as Record<string, unknown>).raid_arrays;
  delete (legacy as Record<string, unknown>).smart_capable;

  await fleetWith(page, [legacy]);

  const quiet = card(page, 'old-quiet');
  await expect(quiet).toBeVisible();
  await expect(quiet.getByText('RAID', { exact: true })).toHaveCount(0);
  await expect(quiet.getByText('SMART', { exact: true })).toHaveCount(0);
});

test('an array that is not serving I/O is not called ok', async ({ page }) => {
  // Degraded is not the whole of unhealthy. An inactive, suspended, broken or
  // unreadable array is not serving I/O at all, raises a CRITICAL
  // raid_not_active alert, and shows red on the node page -- while this cell
  // said "ok", because the roll-up carried only `degraded`.
  await fleetWith(page, [
    node('stopped-array', { raid_arrays: 2, raid_unhealthy: 1, raid_degraded: false }),
  ]);

  const stopped = card(page, 'stopped-array');
  await expect(stopped.getByText('1 of 2 unhealthy')).toBeVisible();
  await expect(stopped.getByText('ok', { exact: true })).toHaveCount(0);
});

test('a healthy host is never labelled unhealthy', async ({ page }) => {
  // The regression the owner reported, and the assertion that would have
  // caught it: read the whole card as text and look for the word.
  //
  // Every existing test here checked the VALUE, which was correct and green
  // the entire time -- "0 of 6". None looked at what it sat under, which was
  // the heading "SMART unhealthy". A roll-up is read by its headings, so the
  // page said a fault about a machine whose six devices had all passed.
  await fleetWith(page, [
    node('healthy', { raid_arrays: 2, raid_degraded: false, smart_capable: 6, smart_unhealthy: 0 }),
  ]);

  const healthy = card(page, 'healthy');
  await expect(healthy).toBeVisible();
  await expect(healthy).not.toContainText('unhealthy');
  await expect(healthy).not.toContainText('degraded');
});

test('a healthy host says how many arrays are well, not just that they are', async ({ page }) => {
  // #44's argument applied to the RAID cell: "ok" for one array and "ok" for
  // five read identically, and an operator cannot tell a host whose single
  // array is fine from one whose five are.
  await fleetWith(page, [
    node('one', { raid_arrays: 1, raid_degraded: false }),
    node('five', { raid_arrays: 5, raid_degraded: false }),
  ]);

  await expect(card(page, 'one').getByText('1 array', { exact: true })).toBeVisible();
  await expect(card(page, 'five').getByText('5 arrays')).toBeVisible();
});

test('a degraded array still says how many arrays there are', async ({ page }) => {
  // The branch combination neither the healthy test nor the unhealthy one
  // reaches: degraded is a fault whose value carries no denominator, so the
  // count is the only thing saying whether one array of one is degraded or
  // one of five. It scopes the fault without claiming those arrays are well.
  await fleetWith(page, [node('degraded-host', { raid_arrays: 5, raid_degraded: true })]);

  const host = card(page, 'degraded-host');
  await expect(host.getByText('degraded', { exact: true })).toBeVisible();
  await expect(host.getByText('5 arrays')).toBeVisible();
  // And it is still a fault, not a count with a reassuring word next to it.
  await expect(host.getByText('ok', { exact: true })).toHaveCount(0);
});

test('the array count is not repeated next to a fault', async ({ page }) => {
  // "2 of 3 unhealthy" already carries the denominator; printing "3 arrays"
  // underneath would say it twice and push the fault up the card.
  await fleetWith(page, [node('sick', { raid_arrays: 3, raid_unhealthy: 2 })]);

  const sick = card(page, 'sick');
  await expect(sick.getByText('2 of 3 unhealthy')).toBeVisible();
  await expect(sick.getByText('3 arrays')).toHaveCount(0);
});

test('a fault with no denominator still names its count', async ({ page }) => {
  // The rollback path, matching what the SMART cell does one line down: an
  // older server sends raid_unhealthy without raid_arrays, and "unhealthy"
  // alone loses the number we were given.
  const legacy = node('old-server', {});
  delete (legacy as Record<string, unknown>).raid_arrays;
  legacy.raid_unhealthy = 2;

  await fleetWith(page, [legacy]);

  await expect(card(page, 'old-server').getByText('2 unhealthy')).toBeVisible();
  await expect(card(page, 'old-server').getByText(/of 0/)).toHaveCount(0);
});

test('an unhealthy host still says so', async ({ page }) => {
  // The other half, so the fix cannot be "remove the word". A card that never
  // says "unhealthy" would pass the test above perfectly.
  await fleetWith(page, [
    node('failing', { raid_arrays: 2, raid_degraded: false, smart_capable: 6, smart_unhealthy: 1 }),
  ]);

  const failing = card(page, 'failing');
  await expect(failing).toContainText('unhealthy');
  await expect(failing.getByText('1 of 6 unhealthy')).toBeVisible();
  // And the denominator line is not shown alongside a fault: the fault value
  // already carries it.
  await expect(failing.getByText('6 checked')).toHaveCount(0);
});
