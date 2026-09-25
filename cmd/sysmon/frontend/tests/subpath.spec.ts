import { test, expect } from '@playwright/test';
import { startSubpathProxy, type SubpathProxy } from './fixtures/subpath-proxy';

/**
 * The dashboard served under a sub-path by a reverse proxy.
 *
 * go-sysmon is meant to sit behind an authenticating proxy that mounts it
 * wherever the platform puts it, not at the URL root. The page used to load
 * its scripts, stylesheet and every API call by root-absolute URLs, so under
 * /sysmon/ it asked the proxy for /assets/index.js and /api/events -- paths
 * that belong to something else or to nothing -- and drew an empty page.
 *
 * These run the real server through a real proxy: nothing is mocked, so a
 * request that escapes the prefix is refused and recorded.
 */

const PREFIX = '/sysmon';

let proxy: SubpathProxy;

test.beforeEach(async ({ baseURL }) => {
  proxy = await startSubpathProxy(baseURL!, PREFIX);
});

test.afterEach(async ({ request }) => {
  await proxy.close();
  // The interval test changes the shared server's rate; put it back.
  await request.post('/api/interval', { data: { interval_ms: 500 } });
});

test.describe('Behind a reverse proxy at a sub-path', () => {
  test('the dashboard loads, streams and stays under the prefix', async ({ page }) => {
    await page.goto(proxy.url);

    await expect(page).toHaveTitle(/System Monitor/);
    // Connected means the event stream opened through the proxy.
    await expect(page.locator('nav').getByText('Connected', { exact: true })).toBeVisible({ timeout: 10000 });
    await expect(page.getByText('Load Avg')).toBeVisible();

    expect(proxy.forwarded).toContain(`${PREFIX}/assets/index.js`);
    expect(proxy.forwarded).toContain(`${PREFIX}/theme-init.js`);
    expect(proxy.forwarded.some((p) => p.startsWith(`${PREFIX}/api/events`))).toBe(true);
    expect(proxy.outside).toEqual([]);
  });

  test('changing the interval reaches the server through the prefix', async ({ page, request }) => {
    await page.goto(proxy.url);
    await expect(page.locator('nav').getByText('Connected', { exact: true })).toBeVisible({ timeout: 10000 });

    await page.getByLabel('Polling interval').selectOption({ label: '5s' });

    await expect
      .poll(async () => ((await (await request.get('/api/interval')).json()) as { interval_ms: number }).interval_ms)
      .toBe(5000);
    expect(proxy.forwarded).toContain(`${PREFIX}/api/interval`);
    expect(proxy.outside).toEqual([]);
  });
});
