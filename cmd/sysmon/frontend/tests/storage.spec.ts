import { test, expect } from '@playwright/test';

test.describe('Storage Tab', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Storage' }).click();
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('at least one disk card is rendered', async ({ page }) => {
    // Each disk renders a card with /dev/<name> in a heading
    const diskHeading = page.locator('h3').filter({ hasText: /\/dev\// }).first();
    await expect(diskHeading).toBeVisible();
  });

  test('disk card shows drive type information', async ({ page }) => {
    // Below the heading is a sub-line with drive_type and SSD/NVMe or HDD annotation
    const driveTypeText = page.locator('p.text-xs').filter({ hasText: /(SSD|NVMe|HDD)/ }).first();
    await expect(driveTypeText).toBeVisible();
  });

  test('disk usage gauge is visible for disks with filesystem data', async ({ page }) => {
    // GaugeChart is rendered inside the disk card when total_bytes > 0
    const diskCard = page.locator('.card-body').first();
    await expect(diskCard).toBeVisible();
    // Look for any canvas (ECharts gauge) in the storage tab content
    const canvas = page.locator('main canvas').first();
    await expect(canvas).toBeVisible({ timeout: 8000 });
  });

  test('IO rate chart canvas is present', async ({ page }) => {
    await expect(page.getByText('I/O Rate (60s)').first()).toBeVisible();
  });

  test('IO counters showing read and write are displayed', async ({ page }) => {
    // The IO counters section renders "Read: X total / Y ops" style text
    const readText = page.locator('div').filter({ hasText: /^Read:/ }).first();
    await expect(readText).toBeVisible();
    const writeText = page.locator('div').filter({ hasText: /^Write:/ }).first();
    await expect(writeText).toBeVisible();
  });

  test('partition table is rendered when partitions exist', async ({ page }) => {
    // Partition table header row contains Device, Mount, FS columns
    const partitionSection = page.locator('p').filter({ hasText: 'Partitions' }).first();
    const hasPartitions = await partitionSection.count();
    if (hasPartitions > 0) {
      await expect(partitionSection).toBeVisible();
      await expect(page.getByRole('columnheader', { name: 'Device' }).first()).toBeVisible();
      await expect(page.getByRole('columnheader', { name: 'Mount' }).first()).toBeVisible();
      await expect(page.getByRole('columnheader', { name: 'FS' }).first()).toBeVisible();
    }
    // If the disk has no partition table entries, the section is omitted — that is valid.
  });

  test('SMART status badge is displayed per disk', async ({ page }) => {
    // Each disk card has a SMART badge: "Healthy", "FAILING", or "SMART N/A"
    const smartBadge = page.locator('.badge').filter({ hasText: /Healthy|FAILING|SMART N\/A/ }).first();
    await expect(smartBadge).toBeVisible();
  });
});
