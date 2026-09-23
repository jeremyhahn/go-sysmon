import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed, type MockOptions } from './fixtures/mock-sse';
import { dashboardTab, openTab, type TabLabel } from './fixtures/tabs';
import type { StackDetection } from '../src/lib/types';

/**
 * The Containers tab exists only on a node with a container runtime, and the
 * Virtualization tab only on one with a hypervisor stack (#187).
 *
 * Owner ruling: a tab hides only when the node PROVES absence. "unknown" --
 * the node could not look -- and a peer too old to send the field both keep
 * the tab, because hiding it there would hide something that may exist.
 */

const present = (stack: string, path: string): StackDetection => ({
  state: 'present',
  evidence: [{ stack, kind: 'binary', path }],
  reason: '',
});
const absent: StackDetection = { state: 'absent', evidence: [], reason: '' };
const unknown = (reason: string): StackDetection => ({ state: 'unknown', evidence: [], reason });

/** Serves `opts` and waits until the dashboard has drawn a snapshot. */
async function openDashboard(page: Page, opts: MockOptions) {
  await mockSnapshotFeed(page, opts);
  await page.goto('/');
  await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
}

const sections: { tab: TabLabel; option: 'containerRuntime' | 'hypervisor'; title: string; stack: string; path: string }[] = [
  { tab: 'Containers', option: 'containerRuntime', title: 'Container runtimes', stack: 'podman', path: '/usr/bin/podman' },
  { tab: 'Virtualization', option: 'hypervisor', title: 'Hypervisor stacks', stack: 'libvirt', path: '/usr/sbin/libvirtd' },
];

for (const s of sections) {
  test.describe(`${s.tab} tab`, () => {
    test('shows, with its evidence, when the node has one', async ({ page }) => {
      await openDashboard(page, { [s.option]: present(s.stack, s.path) });

      await openTab(page, s.tab);

      const card = page.getByTestId('detected-stacks');
      await expect(card).toContainText(s.title);
      await expect(card).toContainText(s.stack);
      await expect(card).toContainText(`binary ${s.path}`);
    });

    test('is absent when the node proved it has none', async ({ page }) => {
      await openDashboard(page, { [s.option]: absent });

      await expect(dashboardTab(page, s.tab)).toHaveCount(0);
      // Tabs every node has are unaffected.
      await expect(dashboardTab(page, 'CPU')).toBeVisible();
    });

    test('shows, and says why, when the node could not tell', async ({ page }) => {
      const reason = 'could not check /usr/bin: permission denied';
      await openDashboard(page, { [s.option]: unknown(reason) });

      await openTab(page, s.tab);

      await expect(page.getByTestId('detected-stacks')).toContainText(reason);
    });

    test('shows for a peer too old to report detection', async ({ page }) => {
      // The fixture carries no detection field at all by default.
      await openDashboard(page, {});

      await expect(dashboardTab(page, s.tab)).toBeVisible();
      await openTab(page, s.tab);
      await expect(page.getByTestId('detected-stacks')).toHaveCount(0);
    });

    test('a user on it when it disappears lands on the Overview', async ({ page }) => {
      await openDashboard(page, { [s.option]: present(s.stack, s.path) });
      await openTab(page, s.tab);

      // The fixture stream ends after one frame and the page reconnects; the
      // newest route wins, so the next frame is a node without the stack.
      await mockSnapshotFeed(page, { [s.option]: absent });

      await expect(dashboardTab(page, s.tab)).toHaveCount(0, { timeout: 10000 });
      await expect(dashboardTab(page, 'Overview')).toHaveClass(/tab-active/);
      await expect(page.locator('.tab-active')).toHaveCount(1);
    });
  });
}

test('the two tabs are decided independently', async ({ page }) => {
  await openDashboard(page, {
    containerRuntime: absent,
    hypervisor: present('qemu', '/usr/bin/qemu-system-x86_64'),
  });

  await expect(dashboardTab(page, 'Containers')).toHaveCount(0);
  await expect(dashboardTab(page, 'Virtualization')).toBeVisible();
});

test('running workloads are listed as evidence without a path', async ({ page }) => {
  await openDashboard(page, {
    containers: 1,
    containerRuntime: { state: 'present', evidence: [{ stack: 'docker', kind: 'running', path: '' }], reason: '' },
  });

  await openTab(page, 'Containers');

  const card = page.getByTestId('detected-stacks');
  await expect(card).toContainText('docker');
  await expect(card).toContainText('running');
});

test('an unobservable VM list names the cause the collector gave', async ({ page }) => {
  // hidepid leaves the process list readable, so "could not be read" would
  // be wrong; the panel repeats the collector's own note instead.
  const note = "/proc is mounted hidepid=2, which hides other users' processes from sysmon; virtual machines run by other users cannot be detected";
  await openDashboard(page, {
    vms: 0,
    capability: { vms_observable: false, notes: [note] },
    hypervisor: unknown(note),
  });

  await openTab(page, 'Virtualization');

  await expect(page.getByText('Virtual machines cannot be detected on this host.')).toBeVisible();
  await expect(page.getByRole('listitem').filter({ hasText: 'hidepid=2' })).toBeVisible();
  await expect(page.getByText(/process list could not be read/)).toHaveCount(0);
});
