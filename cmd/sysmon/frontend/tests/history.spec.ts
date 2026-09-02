import { test, expect, type Page, type Route } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';

/**
 * The history view.
 *
 * It is the only part of the dashboard whose data does not come from the event
 * stream: it reads date ranges back out of the Prometheus each node pushes its
 * own metrics to. Three things are asserted, each against the way it fails
 * quietly:
 *
 *  1. A node with no Prometheus has no History tab at all -- not a tab that
 *     shows an error, which would make a standalone node look broken.
 *  2. The window actually sent upstream matches the one the user picked. A
 *     picker that changes the label and not the request draws the same hour
 *     under every heading.
 *  3. A range that resolves after the user has moved on is discarded. That is
 *     the stale-data failure this repository already guards elsewhere: numbers
 *     from one window rendered under another window's heading look measured
 *     and are wrong.
 */

const CATALOG = {
  enabled: true,
  metrics: [
    { key: 'cpu', label: 'CPU', unit: 'percent', split_by: '' },
    { key: 'memory', label: 'Memory', unit: 'percent', split_by: '' },
    { key: 'disk_used', label: 'Filesystem used', unit: 'percent', split_by: 'device' },
    { key: 'net_rx', label: 'Network in', unit: 'bytes_per_second', split_by: 'interface' },
  ],
};

/** Builds a history payload whose step is a fingerprint the tests can read off
 * the page, so one response is never mistaken for another. */
function historyBody(params: URLSearchParams, stepMs: number, seriesNames: string[] = ['']) {
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
    series: seriesNames.map((name, i) => ({
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
  /** Every /api/history request, in the order they were made. */
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
  await page.route(/\/api\/history\/metrics$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(opts.catalog ?? CATALOG),
    });
  });
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
      body: JSON.stringify(historyBody(params, 15000)),
    });
  });
  return mock;
}

async function openHistory(page: Page) {
  await page.goto('/');
  await page.getByRole('button', { name: 'History', exact: true }).click();
}

test.describe('history', () => {
  test('a node with no Prometheus has no History tab', async ({ page }) => {
    await mockHistory(page, { catalog: { enabled: false, metrics: [] } });
    await page.goto('/');

    // Present enough for the live dashboard to be up, so this is "the tab is
    // absent", not "the page never loaded".
    await expect(page.getByRole('button', { name: 'Overview', exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: 'History', exact: true })).toHaveCount(0);
  });

  test('an enabled node draws its default metrics', async ({ page }) => {
    const mock = await mockHistory(page);
    await openHistory(page);

    // Four charts by default, each with a canvas that actually rendered. A
    // chart whose axis type is not registered draws nothing at all, and the
    // container would still be here -- so the canvas is what is asserted.
    await expect(page.getByRole('heading', { name: 'CPU' })).toBeVisible();
    await expect(page.locator('canvas')).toHaveCount(4);

    const drawn = await page
      .locator('canvas')
      .first()
      .evaluate((el) => {
        const c = el as HTMLCanvasElement;
        const d = c.getContext('2d')!.getImageData(0, 0, c.width, c.height).data;
        for (let p = 3; p < d.length; p += 4) if (d[p] !== 0) return true;
        return false;
      });
    expect(drawn, 'the history chart canvas is blank; nothing was plotted').toBe(true);

    const asked = mock.requests.map((r) => r.get('metric')).sort();
    expect(asked).toEqual(['cpu', 'disk_used', 'memory', 'net_rx']);
  });

  test('choosing a range changes the window that is requested', async ({ page }) => {
    const mock = await mockHistory(page);
    await openHistory(page);
    await expect.poll(() => mock.requests.length).toBeGreaterThanOrEqual(4);

    const windowOf = (params: URLSearchParams) =>
      Number(params.get('end')) - Number(params.get('start'));

    // The default is one hour.
    expect(windowOf(mock.requests[0])).toBe(3600_000);

    mock.requests.length = 0;
    await page.getByRole('button', { name: '24 hours', exact: true }).click();
    await expect.poll(() => mock.requests.length).toBeGreaterThanOrEqual(4);
    expect(windowOf(mock.requests[0])).toBe(24 * 3600_000);

    mock.requests.length = 0;
    await page.getByRole('button', { name: '15 min', exact: true }).click();
    await expect.poll(() => mock.requests.length).toBeGreaterThanOrEqual(4);
    expect(windowOf(mock.requests[0])).toBe(15 * 60_000);
  });

  test('a custom range sends the dates that were typed', async ({ page }) => {
    const mock = await mockHistory(page);
    await openHistory(page);
    await page.getByRole('button', { name: 'Custom', exact: true }).click();

    // Local time, which is what a datetime-local input holds; the assertion is
    // made through the same conversion so it holds in any timezone.
    await page.getByLabel('Range start').fill('2026-08-01T00:00');
    await page.getByLabel('Range end').fill('2026-08-02T00:00');

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
      handler: async (route, params) => {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ ...historyBody(params, 15000), series: [] }),
        });
      },
    });
    await openHistory(page);

    // A node that was switched off for the window genuinely has no data. That
    // is an answer, not a failure, and must not be dressed as one.
    await expect(page.getByText('No data recorded for this range.').first()).toBeVisible();
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('a failure is surfaced with the reason the server gave', async ({ page }) => {
    // These are the two bodies the handlers actually send. The 502 is
    // deliberately vague -- the endpoint is unauthenticated, so the address of
    // the fleet's Prometheus stays in the server log -- while a 400 is this
    // server's own message and says what to ask for instead. Both have to
    // reach the screen: "cannot reach Prometheus" and "that range is not
    // allowed" send an operator to different places.
    for (const [status, body] of [
      [502, 'cannot read history from prometheus; see the server log\n'],
      [400, 'prom: invalid range: the range is 8760h0m0s; at most 2160h0m0s may be requested at once\n'],
    ] as const) {
      await mockHistory(page, {
        handler: async (route) => {
          await route.fulfill({ status, contentType: 'text/plain', body });
        },
      });
      await openHistory(page);

      const alert = page.getByRole('alert');
      await expect(alert).toBeVisible();
      await expect(alert).toContainText(body.trim());
    }
  });

  test('a range that resolves after the user moved on is discarded', async ({ page }) => {
    // The one-hour request hangs; the 24-hour request answers at once. If the
    // slow one is applied when it finally lands, the charts show an hour of
    // data under a heading that says 24 hours -- the stale-data failure this
    // dashboard already guards for fleet peers.
    let releaseSlow: (() => void) | null = null;
    const slowReleased = new Promise<void>((resolve) => {
      releaseSlow = resolve;
    });

    await mockHistory(page, {
      handler: async (route, params) => {
        const window = Number(params.get('end')) - Number(params.get('start'));
        const step = window === 3600_000 ? 1000 : 99000;
        if (step === 1000) await slowReleased;
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params, step)),
        });
      },
    });

    await openHistory(page);
    await page.getByRole('button', { name: '24 hours', exact: true }).click();

    // The 24h response is identified by its step.
    await expect(page.getByText('99s step').first()).toBeVisible();

    releaseSlow!();

    // Give the abandoned responses every chance to land and overwrite.
    await page.waitForTimeout(1000);
    await expect(page.getByText('1s step')).toHaveCount(0);
    await expect(page.getByText('99s step').first()).toBeVisible();
  });

  test('the previous range is not left on screen under the new heading', async ({ page }) => {
    // The heading and the axis change the instant a range is picked; the data
    // takes a round trip. If the old lines keep rendering meanwhile, they are
    // numbers that look measured under a label saying they are something else
    // -- the stale-data failure this dashboard already guards for fleet peers.
    let holdSlow = true;
    await mockHistory(page, {
      handler: async (route, params) => {
        const window = Number(params.get('end')) - Number(params.get('start'));
        if (window === 3600_000) {
          await route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify(historyBody(params, 1000)),
          });
          return;
        }
        while (holdSlow) await new Promise((r) => setTimeout(r, 25));
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(historyBody(params, 99000)),
        });
      },
    });

    await openHistory(page);
    await expect(page.getByText('1s step').first()).toBeVisible();

    await page.getByRole('button', { name: '24 hours', exact: true }).click();

    // The one-hour data must be gone straight away, not when the slow request
    // for the day finally lands.
    await expect(page.getByText('1s step')).toHaveCount(0);
    await expect(page.getByText('Loading…').first()).toBeVisible();

    holdSlow = false;
    await expect(page.getByText('99s step').first()).toBeVisible();
  });

  test('the live dashboard is untouched by the history tab', async ({ page }) => {
    await mockHistory(page);
    await openHistory(page);
    await expect(page.getByRole('heading', { name: 'CPU' }).first()).toBeVisible();

    // Back to a streaming panel: the event stream is a separate transport and
    // must still be feeding the panels after a detour through history.
    await page.getByRole('button', { name: 'Memory', exact: true }).click();
    await expect(page.getByText('RAM Usage')).toBeVisible();
    await expect(page.locator('nav').getByText('Connected')).toBeVisible();
  });
});
