import { test, expect, type Page } from '@playwright/test';
import type { DriveSlot, DriveSlots } from '../src/lib/types';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * The drive bays a node's BMC reports, beside the disks the kernel sees (#289).
 *
 * A bay's fault LED is the BMC's judgement. It can be lit while every block
 * device the kernel sees reads healthy, because the drive sits behind a RAID
 * controller that hides it. The fixture follows ci-node-2: bay 3 faulted and
 * lit, bay 2 predicting its own failure with 3% of its media life left, and
 * one empty bay.
 */

function slot(overrides: Partial<DriveSlot>): DriveSlot {
  return {
    id: '',
    slot: '',
    present: true,
    health: 'ok',
    state: 'Enabled',
    fault: false,
    failure_predicted: false,
    rebuilding: false,
    media_life_left_percent: 0,
    media_life_reported: false,
    manufacturer: '',
    model: '',
    serial_number: '',
    media_type: '',
    protocol: '',
    capacity_bytes: 0,
    ...overrides,
  };
}

const DELLEMC_BAYS: DriveSlots = {
  available: true,
  source: 'redfish',
  reason: '',
  slots: [
    slot({ id: 'Disk.Bay.0', slot: 'Bay 0', media_life_left_percent: 97, media_life_reported: true }),
    slot({
      id: 'Disk.Bay.2',
      slot: 'Bay 2',
      health: 'warning',
      failure_predicted: true,
      media_life_left_percent: 3,
      media_life_reported: true,
    }),
    slot({ id: 'Disk.Bay.3', slot: 'Bay 3', health: 'critical', state: 'Fault', fault: true, rebuilding: true }),
    slot({ id: 'Disk.Bay.5', slot: 'Bay 5', present: false, health: '', state: 'Absent' }),
  ],
};

async function openStorage(page: Page, driveSlots?: DriveSlots) {
  await mockSnapshotFeed(page, { driveSlots });
  await page.goto('/');
  await openTab(page, 'Storage');
}

/** The table row for the bay whose label is shown in its first cell. */
function bayRow(page: Page, label: string) {
  return page
    .getByTestId('drive-slots')
    .locator('tbody tr')
    .filter({ has: page.locator('td:first-child', { hasText: label }) });
}

test('each bay shows its presence, health, state, life left and predicted failure', async ({ page }) => {
  await openStorage(page, DELLEMC_BAYS);

  const card = page.getByTestId('drive-slots');
  await expect(card.locator('h3')).toHaveText('Drive bays');
  await expect(card).toContainText('via Redfish');
  await expect(card.locator('tbody tr')).toHaveCount(4);

  const healthy = bayRow(page, 'Bay 0').locator('td');
  await expect(healthy.nth(1)).toHaveText('present');
  await expect(healthy.nth(2)).toHaveText('OK');
  await expect(healthy.nth(3)).toHaveText('Enabled');
  await expect(healthy.nth(4)).toHaveText('97%');
  await expect(healthy.nth(5)).toHaveText('no');

  const predicted = bayRow(page, 'Bay 2').locator('td');
  await expect(predicted.nth(2)).toHaveText('Warning');
  await expect(predicted.nth(4)).toHaveText('3%');
  await expect(predicted.nth(5)).toHaveText('predicted');

  const faulted = bayRow(page, 'Bay 3').locator('td');
  await expect(faulted.nth(0)).toHaveAttribute('title', 'Disk.Bay.3');
  await expect(faulted.nth(2)).toContainText('FAULT');
  await expect(faulted.nth(2)).toContainText('rebuilding');
  await expect(faulted.nth(3)).toHaveText('Fault');
  // No reading is not 0%: a spinning drive has no media life to report.
  await expect(faulted.nth(4)).toHaveText('—');

  const empty = bayRow(page, 'Bay 5').locator('td');
  await expect(empty.nth(1)).toHaveText('empty');
  await expect(empty.nth(2)).toHaveText('unknown');
  await expect(empty.nth(3)).toHaveText('Absent');
});

test('an unavailable collector shows its reason like the SMART badge does', async ({ page }) => {
  const reason = 'no BMC configured: set bmc.redfish_url, or load ipmi_devintf for /dev/ipmi0';
  await openStorage(page, { available: false, source: '', reason, slots: [] });

  const card = page.getByTestId('drive-slots');
  await expect(card.locator('.badge', { hasText: 'BMC not read' })).toHaveAttribute('title', reason);
  await expect(card.getByTestId('drive-slots-reason')).toHaveText(reason);
  await expect(card.locator('table')).toHaveCount(0);
});

test('a Redfish failure answered by IPMI keeps the bays and says why', async ({ page }) => {
  const reason = 'Redfish: the BMC refused the credentials (HTTP 401); read over IPMI instead';
  await openStorage(page, {
    available: true,
    source: 'ipmi',
    reason,
    // IPMI reports a drive fault as a sensor offset and gives no health word.
    slots: [slot({ id: 'Drive 3', slot: 'Drive 3', health: '', state: 'Drive Present, Drive Fault', fault: true })],
  });

  const card = page.getByTestId('drive-slots');
  await expect(card).toContainText('via IPMI');
  await expect(card.getByTestId('drive-slots-reason')).toHaveText(reason);
  const cells = bayRow(page, 'Drive 3').locator('td');
  await expect(cells.nth(2)).toHaveText('FAULT');
  await expect(cells.nth(3)).toHaveText('Drive Present, Drive Fault');
});

test('a server that predates the collector draws no drive-bay card', async ({ page }) => {
  await openStorage(page);

  await expect(page.locator('.card-body h3', { hasText: '/dev/' }).first()).toBeVisible();
  await expect(page.getByTestId('drive-slots')).toHaveCount(0);
});
