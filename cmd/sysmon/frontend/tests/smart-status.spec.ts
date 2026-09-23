import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * A device with no SMART reading, and which of several different things that
 * means.
 *
 * The fixture is ci-node-1's storage as measured on 2026-09-02, because that
 * one machine carries every case at once and the owner reported it: a
 * device-mapper node with no medium to ask, two PERC volumes whose member
 * drives answer perfectly well behind the controller, a boot card that reports
 * no SMART support, an NVMe that reads fine, and — with CAP_SYS_RAWIO withheld
 * — a device the kernel simply refuses. All five rendered as one neutral
 * "SMART N/A", which reads as "nothing to see" for four situations where there
 * is a great deal to see.
 */

const DELLEMC_DISKS = [
  {
    name: 'dm-0',
    model: 'unknown',
    smart_enabled: false,
    smart_healthy: false,
    smart_status: 'not_applicable',
    smart_reason: 'a virtual block device: there is no physical medium to ask',
  },
  {
    name: 'sda',
    model: 'PERC_H740P_Mini',
    smart_enabled: false,
    smart_healthy: false,
    smart_status: 'behind_controller',
    smart_reason:
      'served by a LSI/Broadcom MegaRAID (Dell PERC) controller and did not answer SMART directly. If it is a volume, SMART belongs to its member drives behind the controller rather than to the volume',
  },
  {
    name: 'sdc',
    model: 'DELLBOSS_VD',
    smart_enabled: false,
    smart_healthy: false,
    smart_status: 'unsupported',
    smart_reason: 'the device answered, and reports that it does not implement the SMART feature set',
  },
  {
    name: 'sdd',
    model: 'SATA_SSD',
    smart_enabled: false,
    smart_healthy: false,
    smart_status: 'unreadable',
    smart_reason:
      'refused by the kernel: the device node opens, but the SMART passthrough itself is rejected. This needs CAP_SYS_RAWIO; group membership does not grant it',
  },
  {
    name: 'nvme0n1',
    model: 'INTEL SSDPE2KE016T8',
    smart_enabled: true,
    smart_healthy: true,
    smart_status: 'ok',
    smart_reason: '',
  },
];

async function openStorage(page: Page, disks: unknown[]) {
  await mockSnapshotFeed(page, { disks: disks as never });
  await page.goto('/');
  await openTab(page, 'Storage');
}

/** The badge inside the card whose heading names /dev/<device>. */
function smartBadge(page: Page, device: string) {
  return page
    .locator('.card-body')
    .filter({ has: page.locator('h3', { hasText: `/dev/${device}` }) })
    .locator('.badge')
    .filter({ hasText: /SMART|Healthy|FAILING/ })
    .first();
}

test('each kind of missing SMART reading says which kind it is', async ({ page }) => {
  await openStorage(page, DELLEMC_DISKS);

  const seen: string[] = [];
  for (const device of ['dm-0', 'sda', 'sdc', 'sdd', 'nvme0n1']) {
    const badge = smartBadge(page, device);
    await expect(badge).toBeVisible();
    seen.push(((await badge.textContent()) ?? '').trim());
  }

  // The assertion that matters is that they DIFFER. Any five labels would pass
  // a check that each is non-empty, which is how five identical "SMART N/A"
  // badges survived until the owner noticed.
  expect(new Set(seen).size).toBe(5);
});

test('a device we were refused is not shown as one with nothing to report', async ({ page }) => {
  await openStorage(page, DELLEMC_DISKS);

  const refused = smartBadge(page, 'sdd');
  const nothingToAsk = smartBadge(page, 'dm-0');
  await expect(refused).toBeVisible();

  // "We could not read this drive" and "this is not a drive" are the two that
  // must never collapse into each other: the first is a fault in how the
  // service was deployed and is fixable, the second is nothing at all.
  expect((await refused.textContent())?.trim()).not.toBe((await nothingToAsk.textContent())?.trim());

  // And the why is carried, because "unreadable" without a reason sends the
  // reader to the hardware when the answer is a missing capability.
  await expect(refused).toHaveAttribute('title', /CAP_SYS_RAWIO/);
});

test('a server that sends no status renders what it always did', async ({ page }) => {
  // A tab held open across a rollback: this bundle, a server with no
  // smart_status field. It must fall back rather than invent a state — and in
  // particular must not report the healthy NVMe as unclassified.
  await openStorage(
    page,
    DELLEMC_DISKS.map(({ smart_status, smart_reason, ...rest }) => rest),
  );

  await expect(smartBadge(page, 'nvme0n1')).toHaveText(/Healthy/);
  await expect(smartBadge(page, 'dm-0')).toHaveText(/SMART N\/A/);
});
