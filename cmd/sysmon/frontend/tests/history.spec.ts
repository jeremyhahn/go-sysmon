import { test, expect, type Page, type Route } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { openTab, type TabLabel } from './fixtures/tabs';

/**
 * The date range control.
 *
 * The owner asked to "see the graphs/charts with date range searches ... in
 * addition to the real-time streams (when a date range is not selected)". The
 * parenthetical is the design: one surface with two states, chosen by a control
 * whose absence gives the live stream — not a separate destination, and not a
 * mode carried by each panel.
 *
 * So these tests are about the whole dashboard rather than one view:
 *
 *  1. A node with no Prometheus has no picker at all, and is the dashboard it
 *     always was.
 *  2. Selecting a range puts the charts on screen into that window, together.
 *  3. It is unmistakable that the charts are not live, and clearing is one
 *     click and no reload. A chart that has silently stopped updating is the
 *     same lie as a frozen peer reading, and here it would be self-inflicted.
 */

const CATALOG = {
  enabled: true,
  metrics: [
    { key: 'cpu', label: 'CPU', unit: 'percent', split_by: '' },
    { key: 'memory', label: 'Memory', unit: 'percent', split_by: '' },
    { key: 'load1', label: 'Load (1m)', unit: 'count', split_by: '' },
    { key: 'disk_read', label: 'Disk read', unit: 'bytes_per_second', split_by: 'device' },
    { key: 'disk_write', label: 'Disk write', unit: 'bytes_per_second', split_by: 'device' },
    { key: 'net_rx', label: 'Network in', unit: 'bytes_per_second', split_by: 'interface' },
    { key: 'net_tx', label: 'Network out', unit: 'bytes_per_second', split_by: 'interface' },
    { key: 'cpu_temperature', label: 'CPU temperature', unit: 'celsius', split_by: 'core' },
    { key: 'psi_cpu_some', label: 'CPU pressure (some)', unit: 'percent', split_by: '' },
    { key: 'psi_memory_some', label: 'Memory pressure (some)', unit: 'percent', split_by: '' },
    { key: 'psi_io_some', label: 'IO pressure (some)', unit: 'percent', split_by: '' },
    { key: 'cpu_power', label: 'CPU package power', unit: 'watts', split_by: 'source' },
  ],
};

/** A history payload whose step is a fingerprint the tests can read back. */
function historyBody(params: URLSearchParams, stepMs = 15000, names: string[] = ['']) {
  const start = Number(params.get('start'));
  const end = Number(params.get('end'));
  return {
    enabled: true,
    node: params.get('node') ?? 'local',
    metric: params.get('metric'),
    label: params.get('metric'),
    unit: 'percent',
    start_ms: start,
    end_ms: end,
    step_ms: stepMs,
    series: names.map((name, i) => ({
      name,
      points: [
        { t: start, v: 10 + i },
        { t: (start + end) / 2, v: 40 + i },
        { t: end, v: 20 + i },
      ],
    })),
  };
}

interface HistoryMock {
  requests: URLSearchParams[];
}

async function mockHistory(
  page: Page,
  opts: {
    catalog?: unknown;
    handler?: (route: Route, params: URLSearchParams) => Promise<void>;
  } = {}
): Promise<HistoryMock> {
  const mock: HistoryMock = { requests: [] };

  await mockSnapshotFeed(page);
  await page.route(/\/api\/history\/metrics$/, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(opts.catalog ?? CATALOG),
    })
  );
  await page.route(/\/api\/history\?/, async (route) => {
    const params = new URL(route.request().url()).searchParams;
    mock.requests.push(params);
    if (opts.handler) {
      await opts.handler(route, params);
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(historyBody(params)),
    });
  });
  return mock;
}

const picker = (page: Page) => page.getByLabel('Date range');
const banner = (page: Page) => page.getByRole('status');
/**
 * The history error, which renders above the panels. A bare
 * `getByRole('alert')` also matched the alert banner a panel draws for a firing
 * alert, so these tests passed only on a quiet snapshot (#163).
 */
const historyError = (page: Page) => page.locator('[role="alert"]:not(main [role="alert"])');

async function openPanel(page: Page, tab: TabLabel) {
  await page.goto('/');
  await openTab(page, tab);
}

test.describe('the date range control', () => {
  test('a node with no Prometheus has no range control at all', async ({ page }) => {
    await mockHistory(page, { catalog: { enabled: false, metrics: [] } });
    await openPanel(page, 'CPU');

    // Enough of the dashboard is up that this is "the control is absent", not
    // "the page never loaded".
    await expect(page.getByText('Overall CPU')).toBeVisible();
    await expect(picker(page)).toHaveCount(0);
    await expect(banner(page)).toHaveCount(0);
    // And there is no separate destination either: the range is a control on
    // the view you are already looking at.
    await expect(page.getByRole('button', { name: 'History', exact: true })).toHaveCount(0);
  });

  test('the control offers live plus ranges, and starts live', async ({ page }) => {
    await mockHistory(page);
    await openPanel(page, 'CPU');

    await expect(picker(page)).toBeVisible();
    await expect(picker(page)).toHaveValue('live');
    await expect(banner(page)).toHaveCount(0);
  });

  test('selecting a range puts the charts on screen into that window', async ({ page }) => {
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');

    // Nothing is asked for while the dashboard is live: the real-time path
    // costs exactly what it did.
    expect(mock.requests.length).toBe(0);

    await picker(page).selectOption('1h');

    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);
    const asked = new Set(mock.requests.map((r) => r.get('metric')));
    expect(asked, 'the CPU panel asked for the metrics its charts draw').toContain('cpu');

    const window = mock.requests.map((r) => Number(r.get('end')) - Number(r.get('start')));
    for (const w of window) expect(w).toBe(3600_000);
  });

  test('one request per metric however many charts want it', async ({ page }) => {
    // The storage panel draws a chart per disk, all from disk_read and
    // disk_write. Fetching per chart would multiply the load on Prometheus by
    // the number of devices for no gain.
    const mock = await mockHistory(page, {
      handler: (route, params) =>
        route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params, 15000, ['nvme0n1', 'nvme1n1'])),
        }),
    });
    await openPanel(page, 'Storage');
    await picker(page).selectOption('1h');

    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);
    await page.waitForTimeout(500);

    const counts = new Map<string, number>();
    for (const r of mock.requests) {
      const key = r.get('metric') ?? '';
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    // Both of the panel's metrics, named: counting only what happened to be
    // asked for would pass against an implementation that requested one and
    // forgot the other.
    for (const metric of ['disk_read', 'disk_write']) {
      expect(counts.get(metric), `${metric} was not requested exactly once`).toBe(1);
    }
    for (const [metric, count] of counts) {
      expect(count, `${metric} was requested ${count} times`).toBe(1);
    }
  });

  test('it is unmistakable that the charts are not live', async ({ page }) => {
    await mockHistory(page);
    await openPanel(page, 'CPU');
    await picker(page).selectOption('6h');

    const bar = banner(page);
    await expect(bar).toBeVisible();
    await expect(bar).toContainText('Historical view');
    await expect(bar).toContainText('charts are not live');
    // And the part that is easy to omit: the gauges beside the charts have no
    // historical value and are still streaming.
    await expect(bar).toContainText(/gauges/i);
  });

  test('a chart heading says which window it is drawing', async ({ page }) => {
    // Every converted heading carries the window -- "CPU Usage (60s)" -- and
    // those were written when 60 seconds was the only answer. Left fixed, a
    // thirty-day series sat under a heading claiming a minute: a specific
    // wrong answer, which reads worse than none because nothing invites doubt.
    await mockHistory(page);
    await openPanel(page, 'CPU');

    const heading = page.getByText(/^CPU Usage \(/);
    await expect(heading).toHaveText('CPU Usage (60s)');

    await picker(page).selectOption('30d');
    await expect(banner(page)).toBeVisible();
    await expect(heading).toHaveText('CPU Usage (30 days)');

    // Other panels, to show the wording is not per-chart: it follows what is
    // applied, for every heading at once. Host is here because it phrases the
    // window differently -- "— 60s history" rather than "(60s)" -- and was
    // missed by the first pass for exactly that reason.
    await openTab(page, 'Memory');
    await expect(page.getByText(/^Memory Usage \(/)).toHaveText('Memory Usage (30 days)');

    await openTab(page, 'Host');
    await expect(page.getByText(/^Load Average \(1m\) —/)).toHaveText(
      'Load Average (1m) — 30 days history'
    );

    await openTab(page, 'Memory');

    // Custom says so rather than spelling out two dates in a heading; the
    // dates are already in the picker and the banner.
    await picker(page).selectOption('custom');
    await page.getByLabel('Range start').fill('2026-08-01T00:00');
    await page.getByLabel('Range start').blur();
    await page.getByLabel('Range end').fill('2026-08-02T00:00');
    await page.getByLabel('Range end').blur();
    await expect(page.getByText(/^Memory Usage \(/)).toHaveText('Memory Usage (custom range)');

    // And back: live is 60 seconds again.
    await picker(page).selectOption('live');
    await expect(page.getByText(/^Memory Usage \(/)).toHaveText('Memory Usage (60s)');
  });

  test('returning to live is one click and no reload', async ({ page }) => {
    await mockHistory(page);
    await openPanel(page, 'CPU');

    const loadedOnce = await page.evaluate(() => performance.now());

    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();

    await page.getByRole('button', { name: 'Return to live' }).click();
    await expect(banner(page)).toHaveCount(0);
    await expect(picker(page)).toHaveValue('live');

    // The same document: a reload would reset the clock this reads.
    const still = await page.evaluate(() => performance.now());
    expect(still).toBeGreaterThan(loadedOnce);

    // And the live stream is feeding the panels again.
    await expect(page.locator('nav').getByText('Connected')).toBeVisible();
    await expect(page.locator('canvas').first()).toBeVisible();
  });

  test('the range follows you between panels', async ({ page }) => {
    // One control for the whole dashboard: changing panel keeps the window, so
    // the panels stay comparable with each other.
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);

    await openTab(page, 'Memory');
    await expect(banner(page)).toBeVisible();
    await expect(picker(page)).toHaveValue('1h');

    await expect.poll(() => mock.requests.some((r) => r.get('metric') === 'memory')).toBe(true);

    // And coming back does not fetch it again. The range and what it loaded
    // belong to the dashboard, not to a panel that remounts: if each panel
    // held its own, every tab change would re-ask Prometheus for what it
    // already had.
    await openTab(page, 'CPU');
    await page.waitForTimeout(400);
    const cpuRequests = mock.requests.filter((r) => r.get('metric') === 'cpu').length;
    expect(cpuRequests, 'cpu was re-fetched on returning to its panel').toBe(1);
  });

  test('the banner does not claim live values while the stream is down', async ({ page }) => {
    // The banner says what the charts are NOT showing, and then what the rest
    // of the page IS. That second sentence read "Gauges, tables and counters
    // still show live values" unconditionally -- and it is false in the two
    // states this dashboard already reports elsewhere: a dropped stream, where
    // every panel is frozen on its last frame, and an unreachable peer, whose
    // last snapshot keeps rendering under a "stale" badge.
    //
    // A banner whose whole purpose is to stop a chart being mistaken for live,
    // printing the word "live" beside data that is not, is the failure it
    // exists to prevent, self-inflicted.
    //
    // The mocked feed serves one frame and closes, so the page renders and is
    // then genuinely disconnected -- which is this state exactly, and is why
    // no extra setup is needed to reach it. delta.spec.ts asserts the other
    // half, where a stream held open makes the claim true.
    await mockHistory(page);
    await openPanel(page, 'CPU');
    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();

    await expect(page.locator('nav').getByText('Disconnected')).toBeVisible();
    await expect(banner(page)).not.toContainText('still show live values');
    await expect(banner(page)).toContainText('last snapshot received');
  });

  test('a reachable peer\'s gauges are live even while the stream is down', async ({ page }) => {
    // The inverted defect, and the reason the banner asks a different question
    // depending on the selection. This host's panels are fed by the event
    // stream; a selected PEER's are refreshed by an independent HTTP poll. So
    // the stream being down says nothing about a peer that is answering, and a
    // banner requiring both would tell an operator their peer's gauges were
    // stale while they updated in front of them.
    //
    // The mocked feed is disconnected by construction -- it serves one frame
    // and closes -- so this is exactly that combination: no stream, reachable
    // peer.
    const mock = await mockHistory(page);

    await page.route(/\/api\/fleet$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          enabled: true,
          nodes: [
            {
              name: 'alpha', url: 'http://alpha:8081', self: false, reachable: true,
              error: '', age_seconds: 1, status: 'healthy', hostname: 'alpha-host',
              version: '1.0.0', uptime_seconds: 1, critical_count: 0, warning_count: 0,
              cpu_percent: 1, memory_percent: 1, swap_percent: 0, load_per_core: 0,
              worst_disk_percent: 0, raid_degraded: false, smart_unhealthy: 0,
              max_temp_c: 0, power_w: 0, net_rx_rate: 0, net_tx_rate: 0,
            },
          ],
        }),
      })
    );
    await page.route(/\/api\/fleet\/alpha\/snapshot$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          snapshot: { ...snapshot, host: { ...snapshot.host, hostname: 'alpha-host' } },
          reachable: true,
          age_seconds: 0,
        }),
      })
    );

    await page.goto('/');
    await openTab(page, 'Fleet');
    await page.getByRole('button', { name: /alpha/ }).first().click();
    await expect(page.locator('nav').getByText('alpha', { exact: true })).toBeVisible();
    // The stream really is down; this is not a test of the connected path.
    await expect(page.locator('nav').getByText('Disconnected')).toBeVisible();

    await openTab(page, 'CPU');
    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);

    await expect(banner(page)).toContainText('still show live values');
    await expect(banner(page)).not.toContainText('last snapshot received');
  });

  test('a peer that goes unreachable while selected stops being called live', async ({ page }) => {
    // The other half of the peer path, and the one the finding named: a peer
    // is selected and reachable, then stops answering. App.svelte keeps
    // rendering its last snapshot under a "stale" badge -- deliberately, since
    // blanking the page would be worse -- so the banner must not go on calling
    // those gauges live.
    //
    // Selected FIRST and made unreachable AFTER, because that is the real
    // sequence. An already-unreachable peer cannot be drilled into from the
    // fleet list at all, so testing it that way tests nothing.
    let reachable = true;
    const mock = await mockHistory(page);

    await page.route(/\/api\/fleet$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          enabled: true,
          nodes: [
            {
              name: 'alpha', url: 'http://alpha:8081', self: false, reachable: true,
              error: '', age_seconds: 1, status: 'healthy', hostname: 'alpha-host',
              version: '1.0.0', uptime_seconds: 1, critical_count: 0, warning_count: 0,
              cpu_percent: 1, memory_percent: 1, swap_percent: 0, load_per_core: 0,
              worst_disk_percent: 0, raid_degraded: false, smart_unhealthy: 0,
              max_temp_c: 0, power_w: 0, net_rx_rate: 0, net_tx_rate: 0,
            },
          ],
        }),
      })
    );
    await page.route(/\/api\/fleet\/alpha\/snapshot$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          snapshot: { ...snapshot, host: { ...snapshot.host, hostname: 'alpha-host' } },
          reachable,
          error: reachable ? '' : 'dial tcp: connect: connection refused',
          age_seconds: reachable ? 0 : 754,
        }),
      })
    );

    await page.goto('/');
    await openTab(page, 'Fleet');
    await page.getByRole('button', { name: /alpha/ }).first().click();
    await expect(page.locator('nav').getByText('alpha', { exact: true })).toBeVisible();

    await openTab(page, 'CPU');
    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);
    await expect(banner(page)).toContainText('still show live values');

    // Now it stops answering. The next poll carries reachable:false.
    reachable = false;
    await expect(page.getByText(/stale/).first()).toBeVisible();
    await expect(banner(page)).toContainText('last snapshot received');
    await expect(banner(page)).not.toContainText('still show live values');
  });

  test('a peer whose poll fails outright is reported stale, not live', async ({ page }) => {
    // The other way a peer goes away, and the one that reported NOTHING. An
    // envelope carrying reachable:false was handled; a poll that fails
    // outright -- a 500 from the aggregator, a refused connection, a body that
    // is not JSON -- left the last successful reading in place with its
    // reachable:true.
    //
    // So the page kept a frozen snapshot on screen with no stale badge and,
    // in historical mode, a banner calling those gauges live. The comment in
    // the store said "the fleet list and the stale badge are what report it
    // unreachable" -- and the badge is driven by the very field that path did
    // not touch.
    let failing = false;
    const mock = await mockHistory(page);

    await page.route(/\/api\/fleet$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          enabled: true,
          nodes: [
            {
              name: 'alpha', url: 'http://alpha:8081', self: false, reachable: true,
              error: '', age_seconds: 1, status: 'healthy', hostname: 'alpha-host',
              version: '1.0.0', uptime_seconds: 1, critical_count: 0, warning_count: 0,
              cpu_percent: 1, memory_percent: 1, swap_percent: 0, load_per_core: 0,
              worst_disk_percent: 0, raid_degraded: false, smart_unhealthy: 0,
              max_temp_c: 0, power_w: 0, net_rx_rate: 0, net_tx_rate: 0,
            },
          ],
        }),
      })
    );
    await page.route(/\/api\/fleet\/alpha\/snapshot$/, (route) => {
      if (failing) {
        return route.fulfill({ status: 500, contentType: 'text/plain', body: 'boom' });
      }
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          snapshot: { ...snapshot, host: { ...snapshot.host, hostname: 'alpha-host' } },
          reachable: true,
          age_seconds: 0,
        }),
      });
    });

    await page.goto('/');
    await openTab(page, 'Fleet');
    await page.getByRole('button', { name: /alpha/ }).first().click();
    await expect(page.locator('nav').getByText('alpha', { exact: true })).toBeVisible();

    await openTab(page, 'CPU');
    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);
    await expect(banner(page)).toContainText('still show live values');

    // The aggregator starts failing. Nothing about the peer changes in any
    // envelope, because no envelope arrives at all.
    failing = true;
    await expect(page.getByText(/stale/).first()).toBeVisible();
    await expect(banner(page)).toContainText('last snapshot received');


    // The age must MEASURE, not accumulate. Consecutive failures each add the
    // whole elapsed time to an age that already contained it, so failures at
    // +1s, +2s and +3s reported 1s, 3s and 6s. Read twice across several poll
    // cycles: the second reading must be close to the elapsed time, not a
    // multiple of it.
    const badge = page.getByText(/stale/).first();
    const first = await badge.innerText();
    await page.waitForTimeout(3000);
    const second = await badge.innerText();
    const secs = (t: string) => Number(/(\d+)s/.exec(t)?.[1] ?? NaN);
    if (!Number.isNaN(secs(first)) && !Number.isNaN(secs(second))) {
      expect(
        secs(second) - secs(first),
        `age went ${first} -> ${second} over ~3s; an age that grows faster than ` +
          'the clock is accumulating rather than measuring'
      ).toBeLessThanOrEqual(6);
    }
  });

  test('drilling into a peer draws that peer\'s past', async ({ page }) => {
    // The range is one control for the dashboard, but the dashboard shows one
    // node at a time. Keeping a peer's window while showing another node's
    // history would be a chart labelled with a machine whose data it is not.
    const mock = await mockHistory(page);
    await page.route(/\/api\/fleet$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          enabled: true,
          nodes: [
            {
              name: 'alpha', url: 'http://alpha:8081', self: false, reachable: true,
              error: '', age_seconds: 1, status: 'healthy', hostname: 'alpha-host',
              version: '1.0.0', uptime_seconds: 1, critical_count: 0, warning_count: 0,
              cpu_percent: 1, memory_percent: 1, swap_percent: 0, load_per_core: 0,
              worst_disk_percent: 0, raid_degraded: false, smart_unhealthy: 0,
              max_temp_c: 0, power_w: 0, net_rx_rate: 0, net_tx_rate: 0,
            },
          ],
        }),
      })
    );
    // The peer endpoint returns a freshness envelope, not a bare snapshot. It
    // has to be reachable and well formed, or the store declines the peer and
    // drops the selection -- correctly -- and this test would pass for the
    // wrong reason, never having selected anything.
    await page.route(/\/api\/fleet\/alpha\/snapshot$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          snapshot: { ...snapshot, host: { ...snapshot.host, hostname: 'alpha-host' } },
          reachable: true,
          age_seconds: 0,
        }),
      })
    );

    await page.goto('/');
    await openTab(page, 'CPU');
    await picker(page).selectOption('1h');
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);
    for (const r of mock.requests) {
      expect(r.get('node'), 'the local node asked with no node parameter').toBeNull();
    }

    mock.requests.length = 0;
    await openTab(page, 'Fleet');
    await page.getByRole('button', { name: /alpha/ }).first().click();
    await expect(page.locator('nav').getByText('alpha', { exact: true })).toBeVisible();

    // Not merely "some request mentions alpha": cpu was already loaded for the
    // local node, and it is the one that would otherwise be reused. Showing
    // this machine's CPU under alpha's heading is the failure being guarded.
    await expect
      .poll(() =>
        mock.requests.some((r) => r.get('metric') === 'cpu' && r.get('node') === 'alpha')
      )
      .toBe(true);
  });

  test('choosing Custom keeps the window that is on screen', async ({ page }) => {
    // The invariant: whenever a range is applied, the inputs describe THAT
    // range. Choosing Custom while looking at "Last 1 hour" used to leave the
    // hour applied and the inputs showing something else, so the control said
    // one thing, the banner a second and the charts a third.
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');

    // Deliberately NOT the one-hour preset. The fallback for "no range
    // applied" seeds the last hour, so testing this with 1h passes whether the
    // applied window is carried across or not -- which is exactly how an
    // earlier version of this test survived deleting the carry.
    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);
    const applied = mock.requests[0];
    const start = Number(applied.get('start'));
    const end = Number(applied.get('end'));

    mock.requests.length = 0;
    await picker(page).selectOption('custom');

    // Still historical, still the same window: nothing moved, and nothing was
    // re-fetched, because the window did not change.
    await expect(banner(page)).toBeVisible();
    await expect(picker(page)).toHaveValue('custom');
    expect(end - start, 'the fixture must not be the one-hour window').toBe(24 * 3600_000);

    // The inputs now describe what is actually on screen, to the minute the
    // datetime-local control can express.
    const toLocal = (ms: number) => {
      const d = new Date(ms);
      const pad = (n: number) => String(n).padStart(2, '0');
      return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
        `T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    };
    await expect(page.getByLabel('Range start')).toHaveValue(toLocal(start));
    await expect(page.getByLabel('Range end')).toHaveValue(toLocal(end));

    await page.waitForTimeout(300);
    expect(mock.requests.length, 'switching to Custom re-fetched an unchanged window').toBe(0);
  });

  test('invalidating an applied custom range takes it off the screen', async ({ page }) => {
    // The invariant from the third direction, and the only one of the three
    // that leaves a wrong number rendered as a right one. Carrying an applied
    // window into Custom is correct; keeping it applied after the edit that
    // made the pair unusable is not. The picker says "Enter a start and an end"
    // -- and used to say it beside charts still drawing the old 24 hours.
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');

    // 24 hours, not 1: the fallback for "nothing applied" seeds the last hour,
    // so a one-hour fixture cannot tell "still applied" from "re-seeded".
    await picker(page).selectOption('24h');
    await expect(banner(page)).toBeVisible();
    await expect.poll(() => mock.requests.length).toBeGreaterThan(0);

    await picker(page).selectOption('custom');
    await expect(banner(page)).toBeVisible();
    mock.requests.length = 0;

    // The edit that invalidates the pair. A datetime-local input reports an
    // incomplete value as an empty one, so this is what clearing a field or
    // deleting a segment actually delivers.
    await page.getByLabel('Range start').fill('');
    await page.getByLabel('Range start').blur();

    // The window is gone from the charts as well as from the controls: the
    // banner is what tells an operator they are not looking at live data, and
    // it must not be claiming a window the picker no longer describes.
    await expect(banner(page)).toHaveCount(0);
    await expect(page.getByText(/Enter a start and an end/)).toBeVisible();

    // And nothing was fetched for a window that was never asked for.
    await page.waitForTimeout(300);
    expect(mock.requests.length, 'an invalid pair requested history anyway').toBe(0);

    // Completing the pair applies it, so this is not a way of getting stuck
    // out of historical mode.
    await page.getByLabel('Range start').fill('2026-08-01T00:00');
    await page.getByLabel('Range start').blur();
    await page.getByLabel('Range end').fill('2026-08-02T00:00');
    await page.getByLabel('Range end').blur();

    await expect(banner(page)).toBeVisible();
    await expect
      .poll(() => {
        const last = mock.requests[mock.requests.length - 1];
        return last == null ? 0 : Number(last.get('end')) - Number(last.get('start'));
      })
      .toBe(24 * 3600_000);
  });

  test('choosing Custom from live does not apply a window nobody asked for', async ({ page }) => {
    // The same defect from the other direction, and the one fixed earlier:
    // seeding a window AND applying it drops the dashboard into a range the
    // user never chose, then moves it again on each keystroke.
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');

    await picker(page).selectOption('custom');

    await expect(banner(page)).toHaveCount(0);
    expect(mock.requests.length).toBe(0);
    // The inputs are open and usable, just not applied.
    await expect(page.getByLabel('Range start')).not.toHaveValue('');
    await expect(page.getByLabel('Range end')).not.toHaveValue('');
  });

  test('refreshing re-reads the same window', async ({ page }) => {
    // A range that does not move still has to be re-readable: it is how an
    // operator picks up samples that landed after they chose the window, and
    // for a relative preset it is the only way to advance "the last hour"
    // without leaving the range and coming back.
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');

    await picker(page).selectOption('custom');
    await page.getByLabel('Range start').fill('2026-08-01T00:00');
    await page.getByLabel('Range start').blur();
    await page.getByLabel('Range end').fill('2026-08-02T00:00');
    await page.getByLabel('Range end').blur();

    // Both endpoints: filling the start alone already produced a window with
    // this start and a different end, so matching on the start would count
    // that one too.
    const wantedStart = String(new Date('2026-08-01T00:00').getTime());
    const wantedEnd = String(new Date('2026-08-02T00:00').getTime());
    const forWindow = () =>
      mock.requests.filter(
        (r) =>
          r.get('metric') === 'cpu' &&
          r.get('start') === wantedStart &&
          r.get('end') === wantedEnd
      );

    await expect.poll(() => forWindow().length).toBe(1);

    await page.getByRole('button', { name: 'Refresh' }).click();

    // Asked again for the very same window: nothing about the request should
    // have changed except that it happened twice.
    await expect.poll(() => forWindow().length).toBe(2);
    const [before, after] = forWindow();
    expect(after.get('start')).toBe(before.get('start'));
    expect(after.get('end')).toBe(before.get('end'));
  });

  test('a metric the server does not publish is never requested', async ({ page }) => {
    // An older node publishes a shorter catalog. Asking it for a metric it
    // does not have gets a 400, and a chart that had asked would wait on it
    // for ever — a spinner that reads as a slow Prometheus and is really a
    // request that is not coming back.
    const mock = await mockHistory(page, {
      catalog: { enabled: true, metrics: CATALOG.metrics.filter((m) => m.key !== 'cpu') },
    });
    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');

    await expect(page.getByText('This chart has no recorded history.').first()).toBeVisible();
    await page.waitForTimeout(400);
    expect(
      mock.requests.some((r) => r.get('metric') === 'cpu'),
      'a metric absent from the catalog was requested anyway'
    ).toBe(false);
  });

  test('a chart whose second metric returns nothing draws neither line', async ({ page }) => {
    // The other half of "every line or none", and the half that was not
    // enforced. The first test holds one REQUEST open; this one lets both
    // finish and has the second return no matching series.
    //
    // disk_read comes back with points for the fixture's devices and
    // disk_write comes back with a series for a device this host does not
    // have, so nothing matches the chart's split value. Every request settled,
    // one metric contributed, and the card would draw a one-line chart under a
    // heading that says read AND write -- with nothing on the page to say the
    // second line is missing rather than flat.
    await mockHistory(page, {
      handler: async (route, params) => {
        const names =
          params.get('metric') === 'disk_write' ? ['a-device-this-host-does-not-have'] : ['nvme0n1', 'nvme1n1'];
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params, 15000, names)),
        });
      },
    });
    await openPanel(page, 'Storage');
    await picker(page).selectOption('1h');

    // Not "Loading…": both requests finished. Not the empty-window message
    // either, because there IS history here -- just not all of it.
    await expect(
      page.getByText('Only part of this chart has history for this range.').first()
    ).toBeVisible();
    await expect(page.getByText('Loading…')).toHaveCount(0);
  });

  test('a chart draws every line it names, or none', async ({ page }) => {
    // Disk read and write are one chart from two metrics. Drawing whichever
    // came back first is a chart whose contents are not what its heading says,
    // and if the other never arrives it stays that way.
    let releaseWrite: (() => void) | null = null;
    const writeHeld = new Promise<void>((resolve) => {
      releaseWrite = resolve;
    });

    await mockHistory(page, {
      handler: async (route, params) => {
        if (params.get('metric') === 'disk_write') await writeHeld;
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          // The device names the fixture host actually has, so the chart's
          // splitValue matches and it really would draw if it were allowed to.
          body: JSON.stringify(historyBody(params, 15000, ['nvme0n1', 'nvme1n1'])),
        });
      },
    });
    await openPanel(page, 'Storage');
    await picker(page).selectOption('1h');

    // Read has landed; write has not. The I/O chart is not drawn -- it says so
    // rather than showing the one line it has. (The gauges on the same panel
    // are canvases too, and are still live, so the count is not the assertion.)
    await expect(page.getByText('Loading…').first()).toBeVisible();
    const beforeRelease = await page.locator('main canvas').count();

    releaseWrite!();
    await expect(page.getByText('Loading…')).toHaveCount(0);
    await expect
      .poll(() => page.locator('main canvas').count())
      .toBeGreaterThan(beforeRelease);
  });

  test('a chart whose history failed says so instead of waiting for ever', async ({ page }) => {
    await mockHistory(page, {
      handler: (route) =>
        route.fulfill({ status: 502, contentType: 'text/plain', body: 'upstream is down\n' }),
    });
    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');

    await expect(page.getByText('History could not be read for this chart.').first()).toBeVisible();
    await expect(page.getByText('Loading…')).toHaveCount(0);
  });

  test('a chart draws its own device, not every device', async ({ page }) => {
    // Prometheus returns every disk's series for disk_read; the chart on a
    // given disk's card has to select that disk. Drawing all of them under one
    // disk's heading would be a chart that is not what it says it is.
    await mockHistory(page, {
      handler: (route, params) =>
        route.fulfill({
          status: 200,
          contentType: 'application/json',
          // A device this host does not have, so a chart that filtered
          // correctly finds nothing and says so.
          body: JSON.stringify(historyBody(params, 15000, ['not-a-real-device'])),
        }),
    });
    await openPanel(page, 'Storage');
    await picker(page).selectOption('1h');

    await expect(page.getByText('No data recorded for this range.').first()).toBeVisible();
  });

  test('a chart with no recorded history says so rather than streaming on', async ({ page }) => {
    // Per-core CPU usage is collected but not exported as a series. Left
    // streaming beside charts showing last week it would be a live line under a
    // historical heading, which is the failure this whole banner exists for.
    await mockHistory(page);
    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');

    await expect(page.getByText('This chart has no recorded history.').first()).toBeVisible();
  });

  test('a custom range sends the dates that were typed', async ({ page }) => {
    const mock = await mockHistory(page);
    await openPanel(page, 'CPU');

    await picker(page).selectOption('custom');
    // Choosing Custom opens the inputs and leaves the dashboard live: dropping
    // into a seeded window nobody asked for would be a surprise.
    await expect(banner(page)).toHaveCount(0);
    expect(mock.requests.length).toBe(0);

    await page.getByLabel('Range start').fill('2026-08-01T00:00');
    await page.getByLabel('Range start').blur();
    await page.getByLabel('Range end').fill('2026-08-02T00:00');
    await page.getByLabel('Range end').blur();

    await expect
      .poll(() => {
        const last = mock.requests[mock.requests.length - 1];
        return last == null ? 0 : Number(last.get('end')) - Number(last.get('start'));
      })
      .toBe(24 * 3600_000);

    const last = mock.requests[mock.requests.length - 1];
    expect(Number(last.get('start'))).toBe(new Date('2026-08-01T00:00').getTime());
  });

  test('an empty range says so instead of showing an error', async ({ page }) => {
    await mockHistory(page, {
      handler: (route, params) =>
        route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ ...historyBody(params), series: [] }),
        }),
    });
    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');

    // A node that was switched off for the window genuinely has no data. That
    // is an answer, not a failure.
    await expect(page.getByText('No data recorded for this range.').first()).toBeVisible();
    await expect(historyError(page)).toHaveCount(0);
  });

  test('a failure is surfaced with the reason the server gave', async ({ page }) => {
    await mockHistory(page, {
      handler: (route) =>
        route.fulfill({
          status: 502,
          contentType: 'text/plain',
          body: 'cannot read history from prometheus; see the server log\n',
        }),
    });
    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');

    const alert = historyError(page);
    await expect(alert).toBeVisible();
    await expect(alert).toContainText('cannot read history from prometheus');
  });

  test('a request for a window nobody is looking at is cancelled, not just ignored', async ({
    page,
  }) => {
    // Discarding a stale ANSWER is not the same as stopping the stale
    // REQUEST, and only the first was happening. The server went on running an
    // unauthenticated Prometheus range query for a window nobody is looking
    // at, and the browser's per-origin connection limit stayed occupied by it
    // -- so on a panel with nine charts, two quick range changes leave the
    // requests for what is actually on screen queued behind eighteen that have
    // been abandoned, and the charts sit on "Loading" for a reason that has
    // nothing to do with Prometheus.
    //
    // Observed from the SERVER side, as an abort is: the request is held open
    // and the connection is watched for the client hanging up.
    const aborted: string[] = [];
    page.on('requestfailed', (request) => {
      if (!request.url().includes('/api/history')) return;
      const text = request.failure()?.errorText ?? '';
      if (/abort/i.test(text)) aborted.push(request.url());
    });

    let releaseHeld: (() => void) | null = null;
    const held = new Promise<void>((resolve) => {
      releaseHeld = resolve;
    });

    await mockHistory(page, {
      handler: async (route, params) => {
        const window = Number(params.get('end')) - Number(params.get('start'));
        if (window === 3600_000) {
          await held;
        }
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params)),
        });
      },
    });

    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');
    // Deliberately not waited on: the hour's requests must still be open.
    await picker(page).selectOption('24h');

    await expect
      .poll(() => aborted.length, {
        message: 'the requests for the abandoned window were left running',
      })
      .toBeGreaterThan(0);

    releaseHeld!();

    // And the window that IS on screen still draws, so this cancels the stale
    // requests rather than all of them.
    await expect(page.locator('canvas').first()).toBeVisible();
    await expect(page.getByText(/history is unavailable/)).toHaveCount(0);
  });

  test('a response that arrives after the range moved is discarded', async ({ page }) => {
    // The dangerous shape, and the one the previous version of this test did
    // not have: the OLD request is still outstanding when the range changes,
    // and lands afterwards. Applied, it would put the first window's answer
    // under the second window's heading.
    //
    // The two answers are made distinguishable by their content rather than
    // their values: the stale one is empty, so applying it would replace a
    // drawn chart with "No data recorded for this range."
    let releaseStale: (() => void) | null = null;
    const staleHeld = new Promise<void>((resolve) => {
      releaseStale = resolve;
    });

    await mockHistory(page, {
      handler: async (route, params) => {
        const window = Number(params.get('end')) - Number(params.get('start'));
        if (window === 3600_000) {
          await staleHeld;
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ ...historyBody(params), series: [] }),
          });
          return;
        }
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params)),
        });
      },
    });

    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');
    // Deliberately not waited on: it must still be in flight.
    await picker(page).selectOption('24h');

    await expect(page.locator('canvas').first()).toBeVisible();

    releaseStale!();
    await page.waitForTimeout(800);

    // The hour's empty answer must have been dropped on arrival.
    await expect(page.getByText('No data recorded for this range.')).toHaveCount(0);
    await expect(page.locator('canvas').first()).toBeVisible();
  });

  test('the previous range is not left on screen under the new one', async ({ page }) => {
    // The banner changes the instant a range is picked; the data takes a round
    // trip. If the old lines kept rendering meanwhile they would be numbers
    // that look measured under a label saying they are something else.
    let holdSlow = true;
    await mockHistory(page, {
      handler: async (route, params) => {
        const window = Number(params.get('end')) - Number(params.get('start'));
        if (window !== 3600_000) {
          while (holdSlow) await new Promise((r) => setTimeout(r, 25));
        }
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params, window === 3600_000 ? 1000 : 99000)),
        });
      },
    });

    await openPanel(page, 'CPU');
    await picker(page).selectOption('1h');
    await expect(page.locator('canvas').first()).toBeVisible();

    const firstWindow = await page.evaluate(() => {
      const c = document.querySelector('canvas') as HTMLCanvasElement;
      return c.toDataURL().length;
    });
    expect(firstWindow).toBeGreaterThan(0);

    await picker(page).selectOption('24h');
    // The hour's chart must be gone straight away, not when the day's slow
    // request finally lands.
    await expect(page.getByText('Loading…').first()).toBeVisible();

    holdSlow = false;
    await expect.poll(() => page.locator('canvas').count()).toBeGreaterThan(0);
  });
});
