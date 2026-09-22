import { test, expect, type Page } from '@playwright/test';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };

/**
 * Byte figures below one byte, or below zero, still carry a unit (#175).
 *
 * formatBytes picked its unit from log1024 of the value, which is negative
 * between 0 and 1: 0.8 B/s -- a disk or process that moved a few bytes over a
 * long interval -- rendered as "819.2 undefined/s".
 */

/** Serves one snapshot whose first process has the given I/O rates. */
async function serveProcessRates(page: Page, read: number, write: number) {
  const process = { ...snapshot.processes.process_list[0], cpu_percent: 99, read_bytes_rate: read, write_bytes_rate: write };
  const payload = {
    ...snapshot,
    processes: { ...snapshot.processes, total: 1, process_list: [process] },
  };
  const body = `retry: 1000\n\nevent: full\ndata: ${JSON.stringify({ seq: 1, snapshot: payload })}\n\n`;
  await page.route(/\/api\/events(\?|$)/, (route) =>
    route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body }),
  );
  await page.goto('/');
  await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
  await page.getByRole('button', { name: 'Host', exact: true }).click();
}

test.describe('Byte rate formatting', () => {
  test('a fractional rate keeps its unit', async ({ page }) => {
    await serveProcessRates(page, 0.8, 0);

    const row = page.locator('tr', { hasText: snapshot.processes.process_list[0].name }).first();
    await expect(row).toBeVisible();
    await expect(row).toContainText('0.8 B/s');
    await expect(row).not.toContainText('undefined');
    await expect(row).not.toContainText('NaN');
  });

  test('a negative rate keeps its sign and unit', async ({ page }) => {
    await serveProcessRates(page, -2048, 0);

    const row = page.locator('tr', { hasText: snapshot.processes.process_list[0].name }).first();
    await expect(row).toContainText('-2.0 KB/s');
    await expect(row).not.toContainText('undefined');
    await expect(row).not.toContainText('NaN');
  });
});
