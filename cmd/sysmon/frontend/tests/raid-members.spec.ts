import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * The drives behind a hardware RAID controller.
 *
 * The fixture is ci-node-1's PERC as measured on 2026-09-02, because the
 * shape is the point: a volume the kernel presents as one disk, made of drives
 * that have no device node and appear nowhere else on the page. Before this,
 * the owner's report was that the machine "has raid, but they are hardware
 * raid" and the dashboard showed neither the arrays nor the six drives in them.
 */

const PERC_VOLUME = {
  name: 'sdb',
  model: 'PERC_H740P_Mini',
  smart_enabled: false,
  smart_healthy: false,
  smart_status: 'behind_controller',
  smart_reason:
    'a raid10 volume on a megaraid_sas controller: SMART belongs to its 4 member drives, which are reported under this device',
  raid: {
    level: 'raid10',
    state: 'active',
    degraded: false,
    total_devices: 4,
    active_devices: 4,
    failed_devices: 0,
    spare_devices: 0,
    // What the collector actually sends for a hardware volume: the controller
    // is never asked about background operations, so the action is EMPTY --
    // not 'idle', which would claim a rebuild had finished. This fixture said
    // 'idle' until the collector stopped doing so, which is a fixture
    // encoding the behaviour under test rather than the behaviour expected.
    sync_action: '',
    sync_progress_percent: 0,
    mismatch_count: 0,
    members: [],
  },
  members: [
    member(6, 'L0J4VK8J', 33, 'ok'),
    member(7, 'L0J4Y3AJ', 34, 'ok'),
    member(8, 'L0J4HNUJ', 33, 'ok'),
    member(9, 'L0J4HTUJ', 33, 'ok'),
  ],
};

function member(id: number, serial: string, temp: number, status: string) {
  return {
    serial,
    model: 'DKR5E-J1R2SS',
    vendor: 'HITACHI',
    device_id: id,
    enclosure: 64,
    slot: id,
    temperature_celsius: temp,
    smart_enabled: status === 'ok' || status === 'failing',
    smart_healthy: status === 'ok',
    smart_status: status,
    smart_reason: status === 'failing' ? 'the drive reports an informational exception' : '',
  };
}

async function openStorage(page: Page, disks: unknown[]) {
  await mockSnapshotFeed(page, { disks: disks as never });
  await page.goto('/');
  await openTab(page, 'Storage');
}

function volumeCard(page: Page, device: string) {
  return page
    .locator('.card-body')
    .filter({ has: page.locator('h3', { hasText: `/dev/${device}` }) });
}

test('a controller volume lists the drives it is made of', async ({ page }) => {
  await openStorage(page, [PERC_VOLUME]);

  const card = volumeCard(page, 'sdb');
  await expect(card.getByText('Member drives (4)')).toBeVisible();

  // Each drive is identified by the thing that survives a reboot, and by the
  // bay to pull. Every serial must be on the page: a table that rendered one
  // row would pass a "the section exists" check.
  for (const serial of ['L0J4VK8J', 'L0J4Y3AJ', 'L0J4HNUJ', 'L0J4HTUJ']) {
    await expect(card.getByText(serial)).toBeVisible();
  }
  await expect(card.getByText('64:6')).toBeVisible();
  await expect(card.getByText('64:9')).toBeVisible();
});

test('a failing member is visible without opening anything', async ({ page }) => {
  const failing = {
    ...PERC_VOLUME,
    members: [
      member(6, 'L0J4VK8J', 33, 'ok'),
      member(7, 'L0J4Y3AJ', 34, 'failing'),
      member(8, 'L0J4HNUJ', 33, 'ok'),
      member(9, 'L0J4HTUJ', 33, 'ok'),
    ],
  };
  await openStorage(page, [failing]);

  const card = volumeCard(page, 'sdb');
  // The whole point of listing the members: one bad drive in a healthy array
  // is the thing that has to be seen, and the array above it still says
  // "active" because it is.
  await expect(card.getByText('FAILING')).toBeVisible();
  const healthy = card.getByText('Healthy');
  await expect(healthy).toHaveCount(3);
});

test('a disk with no members shows no member section', async ({ page }) => {
  // Every other disk on every other host. An empty "Member drives (0)" table
  // on four NVMe would be the same noise this change removed elsewhere.
  await openStorage(page, [
    { name: 'nvme0n1', model: 'INTEL SSDPE2KE016T8', smart_enabled: true, smart_healthy: true, smart_status: 'ok', members: [], raid: null },
  ]);

  await expect(volumeCard(page, 'nvme0n1')).toBeVisible();
  await expect(page.getByText(/Member drives/)).toHaveCount(0);
});

test('a hardware volume shows no consistency-check result it never had', async ({ page }) => {
  // "Mismatch 0" reads as a check that found nothing. A controller volume is
  // never asked about consistency checks, so zero means the check never ran --
  // the opposite claim about a redundant array, and the reassuring one.
  await openStorage(page, [PERC_VOLUME]);

  const card = volumeCard(page, 'sdb');
  await expect(card.getByText('Member drives (4)')).toBeVisible();
  await expect(card.getByText('Mismatch')).toHaveCount(0);
});

test('an md array still shows its consistency-check result', async ({ page }) => {
  // The fix must not have bought silence by hiding the cell for everyone.
  await openStorage(page, [
    {
      name: 'md0',
      model: 'Linux md raid1',
      smart_enabled: false,
      smart_healthy: false,
      members: [],
      raid: {
        level: 'raid1', state: 'clean', degraded: false,
        total_devices: 2, active_devices: 2, failed_devices: 0, spare_devices: 0,
        sync_action: 'idle', sync_progress_percent: 100, mismatch_count: 0, members: [],
      },
    },
  ]);

  await expect(volumeCard(page, 'md0').getByText('Mismatch')).toBeVisible();
});
