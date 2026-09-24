import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';
import { FLEET_API, fleetNode, serveFleet, type FleetFixture } from './fixtures/fleet';
import { logRequests, settledRequests } from './fixtures/request-log';

/**
 * The fleet view at fleet scale (#15).
 *
 * GET /api/fleet used to send every node on every poll and the browser paged
 * them itself: about 9 MB a poll at ten thousand nodes, to draw twenty-five
 * cards. The server now searches and pages, and the view asks for the page it
 * shows. These specs run the view against a mocked ten-thousand-node fleet and
 * check both halves of that: what it ASKS for -- always a bounded page -- and
 * that the search, page-size and paging controls behave as they did when the
 * browser did the work.
 */

const FLEET_SIZE = 10000;

function bigFleet(size = FLEET_SIZE): FleetFixture {
  const nodes = Array.from({ length: size }, (_, i) =>
    fleetNode(`node-${String(i).padStart(5, '0')}`, i === 0 ? { self: true } : {})
  );
  return {
    nodes,
    rollup: {
      nodes: size, healthy: size - 1, warning: 1, critical: 0, unreachable: 0,
      longest_absence_seconds: 0, never_answered: 0, reporting: size,
      attention: [{ name: 'node-09999', status: 'warning', age_seconds: 0, reasons: ['cpu sustained'] }],
      swap_unknown: 0, builds: [{ commit: 'a46f6cf', count: size, nodes: ['node-00000'] }], build_groups: 1,
    },
  };
}

async function openFleet(page: Page, fixture: FleetFixture | (() => FleetFixture) = bigFleet()) {
  await mockSnapshotFeed(page);
  const requests = await serveFleet(page, fixture);
  await page.goto('/');
  await openTab(page, 'Fleet');
  return requests;
}

const cards = (page: Page) => page.locator('button.card h3');
const range = (page: Page) => page.getByText(/^Showing /).first();
const lastRequest = (requests: URL[]) => requests[requests.length - 1].searchParams;

test('the view asks for one page, never the whole fleet', async ({ page }) => {
  const requests = await openFleet(page);

  await expect(cards(page)).toHaveCount(25);
  await expect(cards(page).first()).toHaveText('node-00000');
  await expect(cards(page).last()).toHaveText('node-00024');
  await expect(range(page)).toContainText('1–25 of 10000');
  await expect(page.getByText('1 / 400')).toBeVisible();

  // Every poll, from the first, is bounded.
  expect(requests.length).toBeGreaterThan(0);
  for (const url of requests) {
    expect(url.searchParams.get('limit'), url.search).toBe('25');
  }
});

test('the roll-up describes the whole fleet on every page and every search', async ({ page }) => {
  await openFleet(page);
  const headline = page.getByTestId('fleet-headline');
  await expect(headline).toHaveText('10000 nodes: 9999 healthy, 1 warning, 0 critical, 0 unreachable');

  await page.getByRole('button', { name: 'Last page' }).click();
  await expect(cards(page).first()).toHaveText('node-09975');
  await expect(headline).toHaveText('10000 nodes: 9999 healthy, 1 warning, 0 critical, 0 unreachable');

  await page.getByRole('searchbox', { name: 'Search fleet nodes' }).fill('node-0001');
  await expect(cards(page)).toHaveCount(10);
  await expect(headline).toHaveText('10000 nodes: 9999 healthy, 1 warning, 0 critical, 0 unreachable');
});

test('paging walks the fleet through the server', async ({ page }) => {
  const requests = await openFleet(page);
  await expect(cards(page)).toHaveCount(25);

  await page.getByRole('button', { name: 'Next page' }).click();
  await expect(cards(page).first()).toHaveText('node-00025');
  expect(lastRequest(requests).get('offset')).toBe('25');
  await expect(range(page)).toContainText('26–50 of 10000');
  await expect(page.getByText('2 / 400')).toBeVisible();

  await page.getByRole('button', { name: 'Last page' }).click();
  await expect(cards(page)).toHaveCount(25);
  await expect(cards(page).last()).toHaveText('node-09999');
  expect(lastRequest(requests).get('offset')).toBe('9975');
  await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled();

  await page.getByRole('button', { name: 'First page' }).click();
  await expect(cards(page).first()).toHaveText('node-00000');
  await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled();
});

test('search is sent to the server, resets to page one, and says what it filtered', async ({ page }) => {
  const requests = await openFleet(page);
  await page.getByRole('button', { name: 'Next page' }).click();
  await expect(cards(page).first()).toHaveText('node-00025');

  await page.getByRole('searchbox', { name: 'Search fleet nodes' }).fill('NODE-0001');
  await expect(cards(page)).toHaveCount(10);
  await expect(cards(page).first()).toHaveText('node-00010');
  const sent = lastRequest(requests);
  expect(sent.get('q')).toBe('NODE-0001');
  expect(sent.get('offset')).toBe('0');
  await expect(range(page)).toContainText('1–10 of 10');
  await expect(range(page)).toContainText('(filtered from 10000)');

  await page.getByRole('searchbox', { name: 'Search fleet nodes' }).fill('no-such-node');
  await expect(page.getByText('No node matches “no-such-node”.')).toBeVisible();
  await page.getByRole('button', { name: 'Clear search' }).first().click();
  await expect(cards(page)).toHaveCount(25);
  expect(lastRequest(requests).has('q')).toBe(false);
});

test('the page size is sent to the server and remembered across a reload', async ({ page }) => {
  const requests = await openFleet(page);
  await page.getByRole('button', { name: 'Next page' }).click();
  await expect(cards(page).first()).toHaveText('node-00025');

  await page.getByRole('combobox', { name: 'Nodes per page' }).selectOption('100');
  await expect(cards(page)).toHaveCount(100);
  // A new page size is a new list: back to its first page.
  await expect(cards(page).first()).toHaveText('node-00000');
  expect(lastRequest(requests).get('limit')).toBe('100');
  await expect(page.getByText('1 / 100')).toBeVisible();

  await page.reload();
  await openTab(page, 'Fleet');
  await expect(cards(page)).toHaveCount(100);
  expect(lastRequest(requests).get('limit')).toBe('100');
});

test('a fleet that shrinks under the last page moves to the new last page', async ({ page }) => {
  let fixture = bigFleet(100);
  await openFleet(page, () => fixture);
  await page.getByRole('button', { name: 'Last page' }).click();
  await expect(cards(page).first()).toHaveText('node-00075');

  // Sixty nodes leave: page four no longer exists.
  fixture = bigFleet(40);
  await expect(cards(page).first()).toHaveText('node-00025', { timeout: 10000 });
  await expect(cards(page)).toHaveCount(15);
  await expect(page.getByText('2 / 2')).toBeVisible();
});

test('a slow answer to an old search never replaces the newer one', async ({ page }) => {
  await mockSnapshotFeed(page);
  // Registered first: Playwright runs the LAST matching route first, so the
  // delaying route below sees each request before this answers it.
  await serveFleet(page, bigFleet(200));
  // Answer "node-001" slowly and everything else at once, so the first
  // search's page would arrive after the second's.
  // The first search is held until the second's page is on screen, so it is
  // answered after it -- by the test, not by a delay the host may outrun.
  let answerOld: () => void = () => {};
  const oldMayAnswer = new Promise<void>((resolve) => (answerOld = resolve));
  await page.route(/\/api\/fleet(\?[^/]*)?$/, async (route) => {
    const params = new URL(route.request().url()).searchParams;
    if (params.get('q') !== 'node-001') return route.fallback();
    await oldMayAnswer;
    try {
      await route.fallback();
    } catch {
      // The view aborted this request when the newer search superseded it;
      // there is nothing left to answer.
    }
  });
  await logRequests(page, FLEET_API);
  await page.goto('/');
  await openTab(page, 'Fleet');
  await expect(cards(page)).toHaveCount(25);

  const box = page.getByRole('searchbox', { name: 'Search fleet nodes' });
  await box.fill('node-001');
  await box.fill('node-0019');
  const newer = Array.from({ length: 10 }, (_, i) => `node-0019${i}`);
  await expect(cards(page)).toHaveText(newer);
  // Now answer the old search, and wait until the page has finished with it
  // -- read its body or seen it aborted. Whatever it would do with that page
  // has happened, so the newer page still being on screen is conclusive.
  answerOld();
  const requests = await settledRequests(page, (url) => url.searchParams.get('q') === 'node-001');
  expect(requests.some((r) => r.get('q') === 'node-001'), 'the old search was never made').toBe(true);
  await expect(cards(page)).toHaveText(newer);
});

test('a selected node that leaves the fleet returns the view to this node', async ({ page }) => {
  await mockSnapshotFeed(page);
  await serveFleet(page, bigFleet(30));
  let release: () => void = () => {};
  const gone = new Promise<void>((resolve) => (release = resolve));
  await page.route(/\/api\/fleet\/node-00003\/snapshot$/, async (route) => {
    await gone;
    await route.fulfill({ status: 404, contentType: 'text/plain', body: 'unknown fleet node node-00003' });
  });
  await page.goto('/');
  await openTab(page, 'Fleet');

  await cards(page).nth(3).click();
  const breadcrumb = page.getByRole('button', { name: 'Return to fleet dashboard' });
  await expect(breadcrumb).toBeVisible();

  // The drill-in now answers 404: the aggregator no longer knows the node.
  // The fleet poll carries one page and cannot say a node is gone, so it is
  // the drill-in that must not leave the dashboard stranded on it.
  release();
  await expect(breadcrumb).toHaveCount(0);
});

// A tab left open across a rollback holds this bundle against a server from
// before #15: it ignores the paging parameters, sends every node with no page
// counts, the whole attention list, and build groups with no counts. The view
// must still read true -- not "undefined on a46f6cf", not a search box that
// silently does nothing.
test('against a server from before paging, the view says what it cannot do', async ({ page }) => {
  await mockSnapshotFeed(page);
  const nodes = Array.from({ length: 30 }, (_, i) =>
    fleetNode(`old-${String(i).padStart(2, '0')}`, i === 0 ? { self: true } : {})
  );
  await page.route(/\/api\/fleet(\?[^/]*)?$/, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        enabled: true,
        nodes,
        rollup: {
          nodes: 30, healthy: 22, warning: 8, critical: 0, unreachable: 0,
          longest_absence_seconds: 0, never_answered: 0, reporting: 30, swap_unknown: 0,
          attention: Array.from({ length: 8 }, (_, i) => ({
            name: `old-${String(i).padStart(2, '0')}`, status: 'warning', age_seconds: 0, reasons: ['cpu sustained'],
          })),
          builds: [
            { commit: 'a46f6cf', nodes: nodes.slice(0, 28).map((n) => n.name) },
            { commit: 'c00eb87', nodes: ['old-28', 'old-29'] },
          ],
        },
      }),
    })
  );
  await page.goto('/');
  await openTab(page, 'Fleet');

  await expect(cards(page)).toHaveCount(30);
  await expect(page.getByTestId('fleet-unpaged')).toBeVisible();
  await expect(page.getByRole('searchbox', { name: 'Search fleet nodes' })).toHaveCount(0);
  const attention = page.getByRole('list', { name: 'Nodes needing attention' }).getByRole('listitem');
  await expect(attention).toHaveCount(6);
  await expect(attention.last()).toHaveText('and 3 more');
  await expect(page.getByTestId('fleet-builds')).toHaveText('Builds: 28 on a46f6cf, 2 on c00eb87 (old-28, old-29)');

  // This node is known from the one marked self in the list, so choosing it
  // opens the local dashboard rather than treating it as a peer.
  await page.getByRole('list', { name: 'Nodes needing attention' }).getByRole('button', { name: 'old-00' }).click();
  await expect(page.getByRole('button', { name: 'Overview', exact: true })).toHaveClass(/tab-active/);
  await expect(page.getByRole('button', { name: 'Return to fleet dashboard' })).toHaveCount(0);
});

test('paging is held while a new search is on its way, so a step cannot land on the old list', async ({ page }) => {
  await mockSnapshotFeed(page);
  await serveFleet(page, bigFleet(200));
  let release: () => void = () => {};
  const answered = new Promise<void>((resolve) => (release = resolve));
  await page.route(/\/api\/fleet(\?[^/]*)?$/, async (route) => {
    if (new URL(route.request().url()).searchParams.get('q') === 'node-00') await answered;
    return route.fallback();
  });
  await page.goto('/');
  await openTab(page, 'Fleet');
  await page.getByRole('button', { name: 'Last page' }).click();
  await expect(cards(page).first()).toHaveText('node-00175');

  await page.getByRole('searchbox', { name: 'Search fleet nodes' }).fill('node-00');
  // The old page is still on screen, but its controls no longer describe
  // the list being asked for.
  await expect(page.getByRole('button', { name: 'First page' })).toBeDisabled();
  release();
  await expect(cards(page).first()).toHaveText('node-00000');
  await expect(page.getByRole('button', { name: 'Next page' })).toBeEnabled();
});

test('a failed move back into range keeps the last good page on screen', async ({ page }) => {
  let fixture = bigFleet(100);
  let failing = false;
  await mockSnapshotFeed(page);
  await serveFleet(page, () => fixture);
  await page.route(/\/api\/fleet(\?[^/]*)?$/, (route) => {
    const offset = new URL(route.request().url()).searchParams.get('offset');
    if (failing && offset !== '75') {
      return route.fulfill({ status: 500, contentType: 'text/plain', body: 'boom' });
    }
    return route.fallback();
  });
  await page.goto('/');
  await openTab(page, 'Fleet');
  await page.getByRole('button', { name: 'Last page' }).click();
  await expect(cards(page).first()).toHaveText('node-00075');

  // The fleet shrinks under page four, and the request for the new last page
  // fails: the cards must not be replaced by an empty page.
  fixture = bigFleet(40);
  failing = true;
  await expect.poll(async () => cards(page).count(), { timeout: 5000 }).toBeGreaterThan(0);
  await expect(page.getByText('No node matches')).toHaveCount(0);
  await expect(cards(page).first()).toHaveText('node-00075');
});

test('choosing an unreachable node from a later page of its own search goes to page one', async ({ page }) => {
  // "db" is a node and a prefix of thirty others, so a search for it runs to
  // two pages -- and the node itself is on the first.
  const nodes = [
    fleetNode('db', { self: false, status: 'unreachable', reachable: false, age_seconds: 600 }),
    ...Array.from({ length: 30 }, (_, i) => fleetNode(`db-${String(i).padStart(2, '0')}`)),
    fleetNode('web', { self: true }),
  ];
  await openFleet(page, {
    nodes,
    rollup: {
      nodes: 32, healthy: 31, warning: 0, critical: 0, unreachable: 1,
      longest_absence_seconds: 600, never_answered: 0, reporting: 31, swap_unknown: 0,
      attention: [{ name: 'db', status: 'unreachable', age_seconds: 600, reasons: ['connection refused'] }],
      builds: [], build_groups: 0,
    },
  });
  await page.getByRole('searchbox', { name: 'Search fleet nodes' }).fill('db');
  await expect(cards(page)).toHaveCount(25);
  await page.getByRole('button', { name: 'Next page' }).click();
  await expect(cards(page).first()).toHaveText('db-24');

  // The search is already "db": choosing it must still go back to page one,
  // where its card is.
  await page.getByRole('list', { name: 'Nodes needing attention' }).getByRole('button', { name: 'db' }).click();
  await expect(cards(page).first()).toHaveText('db');
});

test('choosing a node whose name many others contain opens the page that holds its card', async ({ page }) => {
  // Thirty nodes sort before "db" and all have "db" in their hostname, so a
  // search for "db" puts its card on page two.
  const nodes = [
    ...Array.from({ length: 30 }, (_, i) => fleetNode(`a-${String(i).padStart(2, '0')}`, { hostname: 'rack-db.lan' })),
    fleetNode('db', { status: 'unreachable', reachable: false, age_seconds: 600 }),
    fleetNode('web', { self: true }),
  ];
  const requests = await openFleet(page, {
    nodes,
    rollup: {
      nodes: 32, healthy: 31, warning: 0, critical: 0, unreachable: 1,
      longest_absence_seconds: 600, never_answered: 0, reporting: 31, swap_unknown: 0,
      attention: [{ name: 'db', status: 'unreachable', age_seconds: 600, reasons: ['connection refused'] }],
      builds: [], build_groups: 0,
    },
  });
  await page.getByRole('list', { name: 'Nodes needing attention' }).getByRole('button', { name: 'db' }).click();
  await expect(page.locator('button.card h3', { hasText: /^db$/ })).toBeVisible();
  await expect(page.getByText('2 / 2')).toBeVisible();
  // The anchor was sent once; the next poll asks for the page it landed on.
  expect(requests.some((u) => u.searchParams.get('anchor') === 'db')).toBe(true);
});
