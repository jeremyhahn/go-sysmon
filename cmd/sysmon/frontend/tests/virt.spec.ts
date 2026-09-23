import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { openTab } from './fixtures/tabs';

/**
 * containersTable scopes to the container listing.
 *
 * The Containers tab also renders an "Images & Storage" table whenever a
 * runtime socket is reachable, so a bare `table tbody tr` matches rows that
 * have nothing to do with containers. That is exactly how CI broke: a runner
 * with zero containers but a live docker socket sailed past the "no containers
 * here" guard on the storage table's rows, then failed looking for a Name
 * column that only the container table has.
 */
function containersTable(page: Page) {
  return page.locator('table').filter({
    has: page.getByRole('columnheader', { name: /^Name/ }),
  });
}

test.describe('Containers Tab', () => {
  test.beforeEach(async ({ page }) => {
    // The fixture, not the host: which rollups render is the panel's
    // business, whatever the machine running the suite has (#163).
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'Containers');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('rollup cards include the diagnostic counters', async ({ page }) => {
    for (const label of ['Containers', 'Total CPU', 'Total Memory', 'Total PIDs', 'Throttled', 'OOM Kills']) {
      await expect(page.getByText(label, { exact: true }).first()).toBeVisible();
    }
  });
});

/**
 * The container table, driven by a mocked snapshot rather than by whatever the
 * host happens to be running.
 *
 * These assertions used to be skipped on a host with no containers, which is a
 * test reporting success for not running -- the defect this suite exists to
 * catch (#35). Columns, sort order and the detail drawer are properties of the
 * component, so the data they run against belongs in the fixture, exactly as
 * pagination already does.
 *
 * The host-dependent half is not deleted: the empty state has its own test
 * below, so a machine with no containers is asserted rather than skipped.
 */
test.describe('Containers Tab, with containers', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page, { containers: 6 });
    await page.goto('/');
    await openTab(page, 'Containers');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('table exposes the operational columns', async ({ page }) => {
    const table = containersTable(page);
    // Headers carry a sort indicator when active, so anchor rather than
    // matching exactly. Sort state itself is asserted via aria-sort below.
    for (const col of ['Name', 'CPU', 'Limit', 'Throttled', 'Memory', 'Peak', 'PIDs', 'Uptime']) {
      await expect(
        table.getByRole('columnheader', { name: new RegExp('^' + col) }).first()
      ).toBeVisible();
    }
    // The active sort column must announce its direction to assistive tech.
    await expect(table.locator('th[aria-sort="descending"]').first()).toBeVisible();
  });

  test('selecting a row opens the detail drawer', async ({ page }) => {
    await containersTable(page).locator('tbody tr').first().click();
    await expect(page.getByText('CPU throttling')).toBeVisible();
    await expect(page.getByText('OOM kills / events')).toBeVisible();
    await expect(page.getByText(/Pressure stall/)).toBeVisible();
  });

  test('containers tab does not show virtual machines', async ({ page }) => {
    await expect(page.getByRole('heading', { name: 'Virtual Machines' })).toHaveCount(0);
  });
});

/**
 * The other half, and the half that used to be a skip: what a host with no
 * containers shows.
 *
 * A machine running none is the ordinary case on a CI runner and on most of
 * the fleet, so it is the state most users see and the one that had no
 * coverage at all. Asserting it costs one test and turns "we did not look"
 * into "we looked, and this is what it says".
 */
test.describe('Containers Tab, with none', () => {
  test('says there are none rather than rendering an empty table', async ({ page }) => {
    await mockSnapshotFeed(page, { containers: 0 });
    await page.goto('/');
    await openTab(page, 'Containers');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });

    // The rollup still renders -- a host with no containers is a fact about
    // the host, not a broken page.
    await expect(page.getByText('Containers', { exact: true }).first()).toBeVisible();

    // And no container rows are claimed. Scoped to the container table so the
    // storage summary's rows, which exist whenever a runtime socket answers,
    // cannot satisfy this.
    await expect(containersTable(page).locator('tbody tr')).toHaveCount(0);
  });
});

test.describe('Virtualization Tab', () => {
  test.beforeEach(async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto('/');
    await openTab(page, 'Virtualization');
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('renders the VM section and rollups', async ({ page }) => {
    await expect(page.getByRole('heading', { name: 'Virtual Machines' })).toBeVisible();
    for (const label of ['Virtual Machines', 'vCPUs', 'Guest RAM', 'Resident']) {
      await expect(page.getByText(label, { exact: true }).first()).toBeVisible();
    }
  });

  test('virtualization tab does not show the container table', async ({ page }) => {
    await expect(page.getByRole('heading', { name: 'Containers' })).toHaveCount(0);
  });

  test('snapshot exposes a virtualization section over the API', async ({ request }) => {
    const resp = await request.get('/api/snapshot');
    expect(resp.ok()).toBeTruthy();
    const snap = await resp.json();
    expect(snap).toHaveProperty('virtualization');
    expect(snap.virtualization).toHaveProperty('containers');
    expect(snap.virtualization).toHaveProperty('vms');
  });

  test('container entries carry the diagnostic fields', async ({ request }) => {
    const resp = await request.get('/api/snapshot');
    const snap = await resp.json();
    const containers = snap.virtualization.containers ?? [];
    // Every entry, not just the first, and no skip when there are none: this
    // asserts a property of whatever the server actually returned. On a host
    // with no containers it is vacuously true and still RUNS, which is the
    // difference between "nothing violated this" and "we did not look".
    //
    // The guarantee that a populated entry carries these fields does not rest
    // on a host happening to run a container: pkg/types has a marshalling test
    // that asserts the same field names on a filled-in ContainerInfo.
    for (const container of containers) {
      for (const field of [
        'throttled_percent', 'nr_throttled', 'oom_kills', 'memory_peak_bytes',
        'cpu_pressure', 'memory_pressure', 'io_pressure', 'uptime_seconds',
      ]) {
        expect(container).toHaveProperty(field);
      }
    }
  });

  test('vm entries carry the operational fields', async ({ request }) => {
    const resp = await request.get('/api/snapshot');
    const snap = await resp.json();
    const vms = snap.virtualization.vms ?? [];
    // Same shape as the container check above: every entry, no skip, and the
    // populated-entry guarantee lives in pkg/types where it does not depend on
    // a host running a guest.
    for (const vm of vms) {
      for (const field of ['vcpu_threads', 'thread_count', 'uptime_seconds', 'net_rx_rate', 'net_tx_rate']) {
        expect(vm).toHaveProperty(field);
      }
    }
  });
});
