import { test, expect, type Locator, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { fleetNode, serveFleet } from './fixtures/fleet';
import { openTab, type TabLabel } from './fixtures/tabs';

/**
 * The dashboard under the server's Content-Security-Policy (#200).
 *
 * The policy allows no inline script and no inline style. A violation does not
 * fail anything by itself: the browser drops the offending script or style and
 * carries on, so a regression shows up -- if at all -- as a theme that flashes,
 * a tooltip swatch that lost its colour, or a panel that never mounted. These
 * specs therefore listen for the violations themselves, on every surface the
 * dashboard draws, and require there to be none.
 *
 * Mocked API responses carry no policy of their own, and need none: a policy
 * governs the document it arrives with, and the document -- index.html -- is
 * served by the real server in this suite.
 */

interface Violation {
  directive: string;
  blocked: string;
  sample: string;
  source: string;
  line: number;
}

/**
 * Records every securitypolicyviolation event from the first script onward.
 * An init script runs before any of the page's own, so a violation by the
 * pre-paint bootstrap in <head> is caught as well as a late one.
 */
async function recordViolations(page: Page) {
  await page.addInitScript(() => {
    const w = window as unknown as { __cspViolations: Violation[] };
    w.__cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => {
      w.__cspViolations.push({
        directive: e.effectiveDirective,
        blocked: e.blockedURI,
        sample: e.sample,
        source: e.sourceFile,
        line: e.lineNumber,
      });
    });
  });
}

function violations(page: Page): Promise<Violation[]> {
  return page.evaluate(() => (window as unknown as { __cspViolations: Violation[] }).__cspViolations);
}

/** A history catalog and range answers, so the date range control exists. */
async function mockHistory(page: Page) {
  await page.route(/\/api\/history\/metrics$/, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        enabled: true,
        metrics: [
          { key: 'cpu', label: 'CPU', unit: 'percent', split_by: '' },
          { key: 'memory', label: 'Memory', unit: 'percent', split_by: '' },
          { key: 'net_rx', label: 'Network in', unit: 'bytes_per_second', split_by: 'interface' },
          { key: 'net_tx', label: 'Network out', unit: 'bytes_per_second', split_by: 'interface' },
          { key: 'disk_read', label: 'Disk read', unit: 'bytes_per_second', split_by: 'device' },
          { key: 'disk_write', label: 'Disk write', unit: 'bytes_per_second', split_by: 'device' },
        ],
      }),
    })
  );
  await page.route(/\/api\/history\?/, (route) => {
    const params = new URL(route.request().url()).searchParams;
    const start = Number(params.get('start'));
    const end = Number(params.get('end'));
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        enabled: true,
        node: 'local',
        metric: params.get('metric'),
        label: params.get('metric'),
        unit: 'percent',
        start_ms: start,
        end_ms: end,
        step_ms: 15000,
        series: [{ name: '', points: [{ t: start, v: 10 }, { t: (start + end) / 2, v: 40 }, { t: end, v: 20 }] }],
      }),
    });
  });
}

/**
 * Serves a node with every optional surface present -- a fleet, a GPU, an
 * NPU, a container runtime, a hypervisor and history -- so that every tab the
 * dashboard can draw exists.
 */
async function openFullDashboard(page: Page) {
  await recordViolations(page);
  await mockSnapshotFeed(page, {
    gpus: 1,
    npus: 1,
    containers: 3,
    vms: 2,
    containerRuntime: { state: 'present', evidence: [{ stack: 'podman', kind: 'binary', path: '/usr/bin/podman' }], reason: '' },
    hypervisor: { state: 'present', evidence: [{ stack: 'libvirt', kind: 'binary', path: '/usr/sbin/libvirtd' }], reason: '' },
  });
  await serveFleet(page, { nodes: [fleetNode('alpha', { self: true }), fleetNode('bravo')] });
  await mockHistory(page);

  const response = await page.goto('/');
  // Guard the guard: with no policy on the document there could be no
  // violations at all, and every assertion below would pass by proving nothing.
  const policy = response?.headers()['content-security-policy'] ?? '';
  expect(policy).toContain("script-src 'self'");
  expect(policy).toContain("style-src 'self'");
  expect(policy).not.toContain('unsafe-inline');

  await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
}

/** Hovers the middle of every chart canvas on screen, which draws its tooltip. */
async function hoverCharts(page: Page) {
  const canvases = page.locator('main canvas');
  const count = await canvases.count();
  for (let i = 0; i < count; i++) {
    const canvas = canvases.nth(i);
    if (await canvas.isVisible()) {
      await canvas.hover();
    }
  }
}

/**
 * Hovers `chart` and asserts its tooltip swatch carries a colour of its own,
 * set on the element and distinct from the text around it.
 */
async function expectColouredSwatch(page: Page, chart: Locator) {
  await page.mouse.move(0, 0);
  await chart.hover();
  const swatch = page.locator('main span').filter({ hasText: /^●$/ }).locator('visible=true').first();
  await expect(swatch).toBeVisible();

  const colours = await swatch.evaluate((el) => ({
    inline: (el as HTMLElement).style.color,
    swatch: getComputedStyle(el).color,
    text: getComputedStyle(el.parentElement as Element).color,
  }));
  expect(colours.inline).not.toBe('');
  expect(colours.swatch).not.toBe(colours.text);
}

const EVERY_TAB: TabLabel[] = [
  'Fleet',
  'Overview',
  'Host',
  'CPU',
  'GPU',
  'NPU',
  'Memory',
  'Storage',
  'Network',
  'Virtualization',
  'Containers',
];

test.describe('Content-Security-Policy', () => {
  test('every tab renders with no policy violation', async ({ page }) => {
    // Chromium reports a Permissions-Policy feature it does not recognise as a
    // console warning on every load. The server lists only recognised ones;
    // this holds it to that.
    const policyWarnings: string[] = [];
    page.on('console', (m) => {
      if (/Permissions-Policy|Content Security Policy/.test(m.text())) policyWarnings.push(m.text());
    });

    await openFullDashboard(page);

    for (const tab of EVERY_TAB) {
      await openTab(page, tab);
      await hoverCharts(page);
    }

    expect(await violations(page)).toEqual([]);
    expect(policyWarnings).toEqual([]);
  });

  test('the date range charts, the settings dialog, the alert list and the theme switch add none', async ({ page }) => {
    await openFullDashboard(page);

    await openTab(page, 'CPU');
    await page.getByLabel('Date range').selectOption('1h');
    await expect(page.getByRole('status')).toBeVisible();
    await hoverCharts(page);

    await page.getByRole('button', { name: 'Configuration' }).click();
    await page.getByRole('button', { name: 'Close settings' }).first().click();

    await page.getByRole('button', { name: 'Alerts' }).click();
    await page.keyboard.press('Escape');

    await page.getByRole('button', { name: /Switch to:/ }).click();
    await page.getByRole('button', { name: /Switch to:/ }).click();

    expect(await violations(page)).toEqual([]);
  });

  test('a tooltip swatch keeps its series colour, live and over a date range', async ({ page }) => {
    // The swatch colour is set through the CSSOM because the policy refuses a
    // style attribute parsed from markup. Were it markup, the swatch would
    // silently lose its colour -- this fails then even if the listener above
    // somehow missed the violation.
    await openFullDashboard(page);
    await openTab(page, 'Network');

    // The first traffic chart: a live TimeSeriesChart.
    await expectColouredSwatch(page, page.locator('main canvas').first());

    // The CPU chart's history is one unsplit series, which the mock serves;
    // over a range it is a HistoryChart.
    await openTab(page, 'CPU');
    await page.getByLabel('Date range').selectOption('1h');
    await expect(page.getByRole('status')).toBeVisible();
    const history = page.locator('main .card').filter({ hasText: 'CPU Usage (1 hour)' }).locator('canvas').first();
    await expectColouredSwatch(page, history);
  });

  test('the listener sees a violation when there is one', async ({ page }) => {
    // The canary for the listener itself: a style attribute parsed from markup
    // is exactly what the policy refuses, so it must be reported.
    await openFullDashboard(page);

    await page.evaluate(() => {
      const probe = document.createElement('div');
      probe.innerHTML = '<span style="color: red">probe</span>';
      document.body.append(probe);
    });

    await expect.poll(async () => (await violations(page)).map((v) => v.directive)).toContain('style-src-attr');
  });
});
