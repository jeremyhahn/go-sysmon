import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed, type MockOptions } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * The Storage tab, against the fixture's two NVMe disks.
 *
 * This spec used to run against whatever the host had, with its assertions
 * loosened to fit any host ("a SMART badge of some kind", "a partition table if
 * there is one"), and it failed outright on the host that reported #163.
 * What the panel draws for a disk is a property of the panel, so the disks are
 * fixed here and the assertions say exactly what should be on the page.
 */

async function openStorage(page: Page, opts: MockOptions = {}) {
  await mockSnapshotFeed(page, opts);
  await page.goto('/');
  await openTab(page, 'Storage');
}

/** The card whose heading names /dev/<device>. */
function diskCard(page: Page, device: string) {
  return page
    .locator('.card-body')
    .filter({ has: page.locator('h3', { hasText: `/dev/${device}` }) });
}

test.describe('Storage Tab', () => {
  test.beforeEach(async ({ page }) => {
    await openStorage(page);
  });

  test('renders one card per disk', async ({ page }) => {
    await expect(page.locator('h3').filter({ hasText: /^\s*\/dev\// })).toHaveCount(2);
    await expect(diskCard(page, 'nvme0n1')).toBeVisible();
    await expect(diskCard(page, 'nvme1n1')).toBeVisible();
  });

  test('disk card shows model, drive type and controller', async ({ page }) => {
    const subline = diskCard(page, 'nvme0n1').locator('p.text-xs').first();
    await expect(subline).toContainText('CT2000T705SSD3');
    await expect(subline).toContainText('SSD');
    await expect(subline).toContainText('(SSD/NVMe)');
    await expect(subline).toContainText('NVMe');
  });

  test('disk usage gauge is drawn', async ({ page }) => {
    await expect(diskCard(page, 'nvme0n1').locator('canvas').first()).toBeVisible({ timeout: 8000 });
  });

  test('IO rate chart is labelled', async ({ page }) => {
    await expect(diskCard(page, 'nvme0n1').getByText(/^I\/O Rate \(/)).toBeVisible();
  });

  test('IO counters show read and write totals', async ({ page }) => {
    const card = diskCard(page, 'nvme0n1');
    await expect(card.getByText(/^\s*Read: .* total \/\s*3581469 ops/)).toBeVisible();
    await expect(card.getByText(/^\s*Write: .* total \/\s*8756243 ops/)).toBeVisible();
  });

  test('partition table lists each partition', async ({ page }) => {
    const card = diskCard(page, 'nvme0n1');
    await expect(card.getByText('Partitions', { exact: true })).toBeVisible();
    for (const col of ['Device', 'Mount', 'FS']) {
      await expect(card.getByRole('columnheader', { name: col, exact: true })).toBeVisible();
    }
    const root = card.getByRole('row').filter({ hasText: '/dev/nvme0n1p2' });
    await expect(root).toContainText('ext4');
    await expect(card.getByRole('row').filter({ hasText: '/dev/nvme0n1p1' })).toContainText('/boot/efi');
  });

  test('SMART status badge is displayed per disk', async ({ page }) => {
    // The fixture disks report no SMART data at all, which is the state the
    // panel labels "SMART N/A". Every other label is pinned by
    // smart-status.spec.ts, against disks built to carry each one.
    for (const device of ['nvme0n1', 'nvme1n1']) {
      await expect(
        diskCard(page, device).locator('.badge').filter({ hasText: /^\s*SMART N\/A\s*$/ })
      ).toBeVisible();
    }
  });
});

/**
 * The defect #163 was actually hiding.
 *
 * On the host that reported it, one btrfs volume is mounted at two paths, so
 * its disk lists the same device twice. The partition rows were keyed on the
 * device, Svelte threw each_key_duplicate, and the Storage tab never rendered
 * at all -- which a spec running against the live host reported as a missing
 * disk card, and which read like a locator problem.
 */
test.describe('Storage Tab, one device mounted twice', () => {
  test('lists both mounts and the page does not throw', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(e.message));

    await openStorage(page, {
      disks: [
        {
          name: 'nvme5n1',
          partitions: [
            { device: '/dev/nvme5n1', mountpoint: '/media/volumes', fstype: 'btrfs', opts: 'rw,subvol=/volumes' },
            { device: '/dev/nvme5n1', mountpoint: '/media/local', fstype: 'btrfs', opts: 'rw,subvol=/local' },
          ],
        },
      ],
    });

    const rows = diskCard(page, 'nvme5n1').getByRole('row').filter({ hasText: '/dev/nvme5n1' });
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0)).toContainText('/media/volumes');
    await expect(rows.nth(1)).toContainText('/media/local');
    expect(errors).toEqual([]);
  });
});

test.describe('Storage Tab, disk without partitions', () => {
  test('omits the partition section', async ({ page }) => {
    await openStorage(page, { disks: [{ name: 'sda', partitions: [] }] });

    const card = diskCard(page, 'sda');
    await expect(card).toBeVisible();
    await expect(card.getByText('Partitions', { exact: true })).toHaveCount(0);
  });
});
