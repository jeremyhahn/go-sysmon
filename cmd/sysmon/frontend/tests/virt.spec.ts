import { test, expect } from '@playwright/test';

test.describe('Containers Tab', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Containers', exact: true }).click();
    await page.waitForSelector('.loading.loading-spinner', { state: 'detached', timeout: 15000 });
  });

  test('rollup cards include the diagnostic counters', async ({ page }) => {
    for (const label of ['Containers', 'Total CPU', 'Total Memory', 'Total PIDs', 'Throttled', 'OOM Kills']) {
      await expect(page.getByText(label, { exact: true }).first()).toBeVisible();
    }
  });

  test('table exposes the operational columns', async ({ page }) => {
    const rows = page.locator('table tbody tr');
    if ((await rows.count()) === 0) {
      test.skip(true, 'no containers running on this host');
    }
    // Headers carry a sort indicator when active, so anchor rather than
    // matching exactly. Sort state itself is asserted via aria-sort below.
    for (const col of ['Name', 'CPU', 'Limit', 'Throttled', 'Memory', 'Peak', 'PIDs', 'Uptime']) {
      await expect(
        page.getByRole('columnheader', { name: new RegExp('^' + col) }).first()
      ).toBeVisible();
    }
    // The active sort column must announce its direction to assistive tech.
    await expect(page.locator('th[aria-sort="descending"]').first()).toBeVisible();
  });

  test('selecting a row opens the detail drawer', async ({ page }) => {
    const rows = page.locator('table tbody tr');
    if ((await rows.count()) === 0) {
      test.skip(true, 'no containers running on this host');
    }
    await rows.first().click();
    await expect(page.getByText('CPU throttling')).toBeVisible();
    await expect(page.getByText('OOM kills / events')).toBeVisible();
    await expect(page.getByText(/Pressure stall/)).toBeVisible();
  });

  test('containers tab does not show virtual machines', async ({ page }) => {
    await expect(page.getByRole('heading', { name: 'Virtual Machines' })).toHaveCount(0);
  });
});

test.describe('Virtualization Tab', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Virtualization' }).click();
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
    if (containers.length === 0) {
      test.skip(true, 'no containers running on this host');
    }
    for (const field of [
      'throttled_percent', 'nr_throttled', 'oom_kills', 'memory_peak_bytes',
      'cpu_pressure', 'memory_pressure', 'io_pressure', 'uptime_seconds',
    ]) {
      expect(containers[0]).toHaveProperty(field);
    }
  });

  test('vm entries carry the operational fields', async ({ request }) => {
    const resp = await request.get('/api/snapshot');
    const snap = await resp.json();
    const vms = snap.virtualization.vms ?? [];
    if (vms.length === 0) {
      test.skip(true, 'no VMs running on this host');
    }
    for (const field of ['vcpu_threads', 'thread_count', 'uptime_seconds', 'net_rx_rate', 'net_tx_rate']) {
      expect(vms[0]).toHaveProperty(field);
    }
  });
});
