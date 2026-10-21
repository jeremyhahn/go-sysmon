import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * A VM's network rates are shown in decimal bits per second, as links are
 * rated; its disk rates are storage rates and stay in bytes per second.
 */

/** The value shown under a label on the VM card. */
const field = (page: Page, label: string) =>
  page.getByText(label, { exact: true }).first().locator('xpath=following-sibling::p');

test('a VM card shows network in and out as bit rates', async ({ page }) => {
  await mockSnapshotFeed(page, {
    vms: 1,
    vmOverrides: { net_rx_rate: 125_000, net_tx_rate: 187_500_000 },
  });
  await page.goto('/');
  await openTab(page, 'Virtualization');

  await expect(field(page, 'Network in')).toHaveText('1.0 Mbps');
  await expect(field(page, 'Network out')).toHaveText('1.5 Gbps');
});

test('a VM card keeps disk rates in bytes per second', async ({ page }) => {
  await mockSnapshotFeed(page, {
    vms: 1,
    vmOverrides: {
      cgroup_path: '/machine.slice/guest-0.scope',
      net_rx_rate: 125_000,
      disk_read_rate: 2048,
      disk_write_rate: 1_048_576,
    },
  });
  await page.goto('/');
  await openTab(page, 'Virtualization');

  await expect(field(page, 'Disk read')).toHaveText('2.0 KB/s');
  await expect(field(page, 'Disk write')).toHaveText('1.0 MB/s');
  await expect(field(page, 'Network in')).toHaveText('1.0 Mbps');
});
