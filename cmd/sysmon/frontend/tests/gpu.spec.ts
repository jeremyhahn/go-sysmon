import { test, expect } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';

test.describe('GPU Panel', () => {
  // Helper: navigate to the GPU tab and wait for snapshot data.
  async function openGpuPanel(page: import('@playwright/test').Page) {
    await page.goto('/');
    // Wait for the app's event stream to connect and deliver at least one
    // snapshot by polling for the "Updated:" label, which only renders once
    // snapshot != null.
    await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });
    // Switch to the GPU tab.
    await page.getByRole('button', { name: 'GPU' }).click();
    // Give Svelte a moment to re-render the tab content.
    await page.waitForTimeout(500);
  }

  test('GPU tab is visible and clickable', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await page.goto('/');
    const gpuTab = page.getByRole('button', { name: 'GPU' });
    await expect(gpuTab).toBeVisible();
    await gpuTab.click();
    await expect(gpuTab).toHaveClass(/tab-active/);
  });

  test('event stream snapshot contains a gpus array', async ({ page }) => {
    await page.goto('/');

    const gpus = await page.evaluate((): Promise<unknown[]> => {
      return new Promise((resolve, reject) => {
        const es = new EventSource('/api/events');
        const timer = setTimeout(() => {
          es.close();
          reject(new Error('timed out waiting for a snapshot event'));
        }, 15000);

        es.addEventListener('snapshot', (ev: MessageEvent<string>) => {
          clearTimeout(timer);
          es.close();
          try {
            const data = JSON.parse(ev.data) as { gpus: unknown[] };
            resolve(data.gpus ?? []);
          } catch {
            reject(new Error('snapshot event data was not valid JSON'));
          }
        });

        es.onerror = () => {
          clearTimeout(timer);
          es.close();
          reject(new Error('event stream error'));
        };
      });
    });

    expect(Array.isArray(gpus)).toBe(true);
  });

  test('GPU snapshot entries have expected fields when GPUs are present', async ({ page }) => {
    await page.goto('/');

    const gpus = await page.evaluate((): Promise<unknown[]> => {
      return new Promise((resolve, reject) => {
        const es = new EventSource('/api/events');
        const timer = setTimeout(() => {
          es.close();
          reject(new Error('timed out waiting for a snapshot event'));
        }, 15000);

        es.addEventListener('snapshot', (ev: MessageEvent<string>) => {
          clearTimeout(timer);
          es.close();
          try {
            const data = JSON.parse(ev.data) as { gpus: unknown[] };
            resolve(data.gpus ?? []);
          } catch {
            reject(new Error('snapshot event data was not valid JSON'));
          }
        });

        es.onerror = () => {
          clearTimeout(timer);
          es.close();
          reject(new Error('event stream error'));
        };
      });
    });

    expect(Array.isArray(gpus)).toBe(true);
    if (gpus.length === 0) {
      return;
    }

    const gpu = gpus[0] as Record<string, unknown>;
    expect(typeof gpu.index).toBe('number');
    expect(typeof gpu.name).toBe('string');
    expect(typeof gpu.ecc_enabled).toBe('boolean');
    expect(typeof gpu.gpu_util_percent).toBe('number');
    expect(typeof gpu.memory_total_mib).toBe('number');
    expect(typeof gpu.pci_bus_id).toBe('string');
  });

  test('GPU panel shows Utilization section when GPUs are present', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await openGpuPanel(page);

    await expect(page.getByText('Utilization').first()).toBeVisible();
  });

  test('GPU panel shows Memory section with MiB values when GPUs are present', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await openGpuPanel(page);

    await expect(page.getByText(/MiB/).first()).toBeVisible();
  });

  test('GPU panel shows ECC status row in metrics footer when GPUs are present', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await openGpuPanel(page);

    await expect(page.getByText('ECC').first()).toBeVisible();
  });

  test('GPU panel renders gauge charts when GPUs are present', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await openGpuPanel(page);

    await expect(page.getByText('GPU Util').first()).toBeVisible();
    await expect(page.getByText('VRAM').first()).toBeVisible();
  });

  test('GPU panel renders time-series charts when GPUs are present', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await openGpuPanel(page);

    await expect(page.getByText('GPU Usage (60s)').first()).toBeVisible();
    await expect(page.getByText('Memory Usage (60s)').first()).toBeVisible();
    await expect(page.getByText('Temperature (60s)').first()).toBeVisible();
    await expect(page.getByText('Power Draw (60s)').first()).toBeVisible();
    await expect(page.getByText('PCIe Throughput (60s)').first()).toBeVisible();
  });

  test('GPU panel shows GPU index and name in card header when GPUs are present', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 1 });
    await openGpuPanel(page);

    const gpuHeader = page.locator('h3').filter({ hasText: /^GPU \d+:/ }).first();
    await expect(gpuHeader).toBeVisible();
    const headerText = await gpuHeader.textContent();
    expect(headerText).toMatch(/^GPU \d+:/);
  });
});

/**
 * The state that used to be a skip condition, now asserted.
 *
 * A host without a GPU is the ordinary case on most of this fleet. It used to
 * get a tab that opened onto "No GPUs detected"; it now gets no tab at all
 * (#153), and accelerator-tabs.spec.ts covers the tab coming and going.
 */
test.describe('GPU Panel, with no GPU', () => {
  test('has no GPU tab to open', async ({ page }) => {
    await mockSnapshotFeed(page, { gpus: 0 });

    await page.goto('/');
    await expect(page.locator('nav').getByText(/Updated:/)).toBeVisible({ timeout: 30000 });

    await expect(page.getByRole('button', { name: 'GPU', exact: true })).toHaveCount(0);
  });
});
