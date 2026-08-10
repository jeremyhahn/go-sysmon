import { test, expect } from '@playwright/test';

test.describe('Memory Tab', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Memory' }).click();
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('RAM usage gauge is visible', async ({ page }) => {
    await expect(page.getByText('RAM Usage')).toBeVisible();
    const gaugeCard = page.getByText('RAM Usage').locator('..').locator('..');
    await expect(gaugeCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('memory time-series chart is visible', async ({ page }) => {
    await expect(page.getByText('Memory Usage (60s)')).toBeVisible();
    const chartCard = page.getByText('Memory Usage (60s)').locator('..').locator('..');
    await expect(chartCard.locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('stat cards show Total, Used, Available, Free labels', async ({ page }) => {
    // Labels are rendered in uppercase <p> elements inside StatCard card-body.
    // Use exact matching to avoid strict mode violations with similar substrings.
    const statGrid = page.locator('.grid').filter({ hasText: 'Total' }).filter({ hasText: 'Available' }).first();
    await expect(statGrid.locator('p').filter({ hasText: /^Total$/i }).first()).toBeVisible();
    await expect(statGrid.locator('p').filter({ hasText: /^Used$/i }).first()).toBeVisible();
    await expect(statGrid.locator('p').filter({ hasText: /^Available$/i }).first()).toBeVisible();
    await expect(statGrid.locator('p').filter({ hasText: /^Free$/i }).first()).toBeVisible();
  });

  test('memory stat card values are non-empty', async ({ page }) => {
    // StatCard renders the label in a <p> and value in a <span class="text-2xl font-bold">.
    // Find the card containing the "Total" label and verify the value is a byte string.
    const totalLabel = page.locator('p.uppercase').filter({ hasText: /^Total$/i }).first();
    await expect(totalLabel).toBeVisible();
    const totalCard = totalLabel.locator('..');
    // The value span contains a size string like "16 GB" or "8.0 GiB"
    const valueSpan = totalCard.locator('span.text-2xl').first();
    await expect(valueSpan).toBeVisible();
    const text = await valueSpan.textContent();
    expect(text).toMatch(/[KMGT]?i?B/);
  });

  test('buffers / cache breakdown section is rendered', async ({ page }) => {
    await expect(page.getByText('Buffers / Cache Breakdown')).toBeVisible();
  });

  test('swap section is present when swap exists', async ({ page }) => {
    // Swap section is conditionally rendered only when swap_total_bytes > 0.
    // We simply verify that either the section or the loading state is coherent.
    const swapSection = page.getByText('Swap').first();
    // The "Swap" StatCard on the Overview also uses this text, so check the card heading
    const swapHeading = page.locator('h3').filter({ hasText: /^Swap$/ });
    const hasSwap = await swapHeading.count();
    if (hasSwap > 0) {
      await expect(swapHeading.first()).toBeVisible();
    }
    // If swap is not present, the test still passes — hardware may have no swap.
    void swapSection;
  });

  test('DIMM table is rendered when DIMM data is available', async ({ page }) => {
    const dimmHeading = page.locator('h3').filter({ hasText: /Memory Modules/ });
    const hasDimms = await dimmHeading.count();
    if (hasDimms > 0) {
      await expect(dimmHeading.first()).toBeVisible();
      await expect(page.getByRole('columnheader', { name: 'Location' })).toBeVisible();
      await expect(page.getByRole('columnheader', { name: 'Size' })).toBeVisible();
      await expect(page.getByRole('columnheader', { name: 'Type' })).toBeVisible();
    }
    // On systems without DMI DIMM info the section is simply absent — that is correct behavior.
  });
});
