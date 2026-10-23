import { test, expect, type Page } from '@playwright/test';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';
import { drawn } from './fixtures/history';

/**
 * Served from the fixture snapshot, not from the host running the suite: what
 * this panel draws is a property of the panel, and a spec whose outcome depends
 * on the machine it runs on tests the machine (#163).
 */
test.describe('CPU Tab', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'CPU');
    // Wait for the loading spinner to go away once snapshot data arrives
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('CPU tab shows per-core usage bars', async ({ page }) => {
    // The per-core section heading
    await expect(page.getByText('Per-Core Usage')).toBeVisible();
    // One bar per logical CPU in the fixture. Scoped to the per-core list:
    // the panel draws other progress bars (frequency, pressure) as well.
    const coreBars = page.locator('.space-y-1').filter({ hasText: /^\s*Core \d+/ }).locator('progress');
    await expect(coreBars).toHaveCount(2);
  });

  test('each core bar has Core N label', async ({ page }) => {
    // "Core 0" also appears as a cell in the CPU details table, so scope the
    // assertion to the per-core usage list to keep the locator unambiguous.
    const coreLabel = page.locator('span', { hasText: /^Core 0$/ }).first();
    await expect(coreLabel).toBeVisible();
  });

  test('CPU details table renders with required columns', async ({ page }) => {
    await expect(page.getByText('CPU Details')).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Model', exact: true })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Vendor', exact: true })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Cores', exact: true })).toBeVisible();
    await expect(page.getByRole('columnheader', { name: 'Threads', exact: true })).toBeVisible();
  });

  test('CPU details table row shows the model name', async ({ page }) => {
    const firstDataRow = page.locator('table tbody tr').first();
    await expect(firstDataRow.locator('td').first()).toHaveText('Intel(R) Core(TM) Ultra 9 285K');
  });

  test('overall CPU gauge chart is rendered', async ({ page }) => {
    await expect(page.getByText('Overall CPU')).toBeVisible();
    // ECharts renders a canvas inside the gauge card
    const gaugeCard = page.getByText('Overall CPU').locator('..').locator('..');
    await expect(gaugeCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('CPU usage time-series chart is rendered', async ({ page }) => {
    await expect(page.getByText('CPU Usage (60s)')).toBeVisible();
    const chartCard = page.getByText('CPU Usage (60s)').locator('..').locator('..');
    await expect(chartCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('per-core usage is shown as a percentage', async ({ page }) => {
    const firstCoreUsage = page.locator('.space-y-1 span.font-semibold').first();
    await expect(firstCoreUsage).toHaveText(/^\s*\d+(\.\d+)?%\s*$/);
  });
});

/**
 * A core whose usage was not measured -- every core on a node's first
 * collection, or one whose /proc/stat line could not be read -- arrives with
 * usage_available false and usage_percent 0. That zero is not an idle core,
 * and nothing on the page may draw it as one.
 */
test.describe('CPU usage not measured', () => {
  /** Serves the fixture with the given cores' usage marked unmeasured. */
  async function serveUnmeasured(page: Page, unmeasured: number[], tab: 'CPU' | 'Host' = 'CPU') {
    const cpus = snapshot.cpus.map((cpu, i) =>
      unmeasured.includes(i) ? { ...cpu, usage_percent: 0, usage_available: false } : cpu
    );
    const payload = { ...snapshot, cpus };
    const body = `retry: 1000\n\nevent: full\ndata: ${JSON.stringify({ seq: 1, snapshot: payload })}\n\n`;
    await page.route(/\/api\/events(\?|$)/, (route) =>
      route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body })
    );
    await page.goto('/');
    await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
    await openTab(page, tab);
  }

  const gauge = (page: Page) => page.getByRole('img', { name: /^CPU:/ }).first();

  test('an unmeasured core shows a dash and no bar; the gauge averages the measured one', async ({ page }) => {
    await serveUnmeasured(page, [0]);

    await expect(page.getByTestId('core-usage-unknown')).toHaveCount(1);
    await expect(page.getByTestId('core-usage-unknown')).toHaveText('—');
    const coreBars = page.locator('.space-y-1').filter({ hasText: /^\s*Core \d+/ }).locator('progress');
    await expect(coreBars).toHaveCount(1);
    // Core 1 alone, at 37.5%: core 0's zero is left out, not averaged in.
    await expect(gauge(page)).toHaveAttribute('aria-label', 'CPU: 38%');
  });

  test('with no core measured the gauge reads not measured, never 0%', async ({ page }) => {
    await serveUnmeasured(page, [0, 1]);

    await expect(page.getByTestId('core-usage-unknown')).toHaveCount(2);
    await expect(gauge(page)).toHaveAttribute('aria-label', 'CPU: not measured');
  });

  test('the Host tab CPU figure is a dash, not 0.0%', async ({ page }) => {
    await serveUnmeasured(page, [0, 1], 'Host');

    const stat = page
      .locator('.card')
      .filter({ has: page.locator('p', { hasText: /^\s*CPU\s*$/ }) })
      .filter({ has: page.locator('span.text-2xl') });
    await expect(stat).toContainText('—');
    await expect(stat).not.toContainText('0.0%');
  });

  test('the first measured reading after unmeasured ones is drawn, not lost between breaks', async ({ page }) => {
    // One frame: the live history is 59 unmeasured samples -- breaks, by
    // #216's rule -- and then this reading. A line needs two points, so
    // without a marker the reading would draw nothing at all.
    // Core 0 unmeasured leaves core 1's 37.5%: not a round figure, so the
    // value axis ends above it rather than putting the reading on the plot's
    // top edge, outside the strips drawn reads.
    await serveUnmeasured(page, [0]);
    const chart = page.locator('.card').filter({ hasText: /CPU Usage \(/ }).first();

    // The category axis centres each of its 60 samples in its own band, so
    // the last one sits half a band in from the right edge.
    // Compared by how many pixels are painted, not pixel for pixel: the
    // split lines crossing both strips antialias differently at different x.
    const painted = (col: number[]) => col.filter((v, i) => i % 4 === 3 && v !== 0).length;
    await drawn(chart, [0.5, 59.5 / 60], ([gap, reading]) => {
      expect(painted(reading)).toBeGreaterThan(painted(gap));
    });
  });
});

/**
 * A core's live frequency is shown when the host reports it. Without cpufreq
 * or a cpuinfo line it is unknown, and zero must not read as a stopped core.
 */
test.describe('CPU per-core current frequency', () => {
  async function serveCurMhz(page: Page, available: boolean) {
    const cpus = snapshot.cpus.map((cpu) =>
      available
        ? { ...cpu, cur_mhz: 3200, cur_mhz_available: true, cur_mhz_source: 'cpufreq' }
        : { ...cpu, cur_mhz: 0, cur_mhz_available: false, cur_mhz_source: '' }
    );
    const payload = { ...snapshot, cpus };
    const body = `retry: 1000\n\nevent: full\ndata: ${JSON.stringify({ seq: 1, snapshot: payload })}\n\n`;
    await page.route(/\/api\/events(\?|$)/, (route) =>
      route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body })
    );
    await page.goto('/');
    await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
    await openTab(page, 'CPU');
  }

  test('an available frequency is shown with its source', async ({ page }) => {
    await serveCurMhz(page, true);

    const cells = page.getByTestId('core-cur-mhz');
    await expect(cells).toHaveCount(snapshot.cpus.length);
    await expect(cells.first()).toHaveText('3.20 GHz');
    await expect(cells.first()).toHaveAttribute('title', 'Current frequency (cpufreq)');
  });

  test('an unavailable frequency reads unknown, never 0 MHz', async ({ page }) => {
    await serveCurMhz(page, false);

    const cells = page.getByTestId('core-cur-mhz');
    await expect(cells).toHaveCount(snapshot.cpus.length);
    await expect(cells.first()).toHaveText('unknown');
    await expect(cells.first()).not.toContainText('MHz');
  });
});
