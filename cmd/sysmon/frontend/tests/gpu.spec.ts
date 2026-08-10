import { test, expect } from '@playwright/test';

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
    await page.goto('/');
    const gpuTab = page.getByRole('button', { name: 'GPU' });
    await expect(gpuTab).toBeVisible();
    await gpuTab.click();
    await expect(gpuTab).toHaveClass(/tab-active/);
  });

  test('GPU panel shows either GPU cards or no-GPU message after data loads', async ({ page }) => {
    await openGpuPanel(page);

    // After data arrives, the panel shows either GPU cards or the empty message.
    const content = page.locator('.card-body').first().or(page.getByText('No GPUs detected'));
    await expect(content).toBeVisible({ timeout: 5000 });
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
    await openGpuPanel(page);

    if (await page.getByText('No GPUs detected').isVisible().catch(() => false)) {
      test.skip();
      return;
    }

    await expect(page.getByText('Utilization').first()).toBeVisible();
  });

  test('GPU panel shows Memory section with MiB values when GPUs are present', async ({ page }) => {
    await openGpuPanel(page);

    if (await page.getByText('No GPUs detected').isVisible().catch(() => false)) {
      test.skip();
      return;
    }

    await expect(page.getByText(/MiB/).first()).toBeVisible();
  });

  test('GPU panel shows ECC status row in metrics footer when GPUs are present', async ({ page }) => {
    await openGpuPanel(page);

    if (await page.getByText('No GPUs detected').isVisible().catch(() => false)) {
      test.skip();
      return;
    }

    await expect(page.getByText('ECC').first()).toBeVisible();
  });

  test('GPU panel renders gauge charts when GPUs are present', async ({ page }) => {
    await openGpuPanel(page);

    if (await page.getByText('No GPUs detected').isVisible().catch(() => false)) {
      test.skip();
      return;
    }

    await expect(page.getByText('GPU Util').first()).toBeVisible();
    await expect(page.getByText('VRAM').first()).toBeVisible();
  });

  test('GPU panel renders time-series charts when GPUs are present', async ({ page }) => {
    await openGpuPanel(page);

    if (await page.getByText('No GPUs detected').isVisible().catch(() => false)) {
      test.skip();
      return;
    }

    await expect(page.getByText('GPU Usage (60s)').first()).toBeVisible();
    await expect(page.getByText('Memory Usage (60s)').first()).toBeVisible();
    await expect(page.getByText('Temperature (60s)').first()).toBeVisible();
    await expect(page.getByText('Power Draw (60s)').first()).toBeVisible();
    await expect(page.getByText('PCIe Throughput (60s)').first()).toBeVisible();
  });

  test('GPU panel shows GPU index and name in card header when GPUs are present', async ({ page }) => {
    await openGpuPanel(page);

    if (await page.getByText('No GPUs detected').isVisible().catch(() => false)) {
      test.skip();
      return;
    }

    const gpuHeader = page.locator('h3').filter({ hasText: /^GPU \d+:/ }).first();
    await expect(gpuHeader).toBeVisible();
    const headerText = await gpuHeader.textContent();
    expect(headerText).toMatch(/^GPU \d+:/);
  });
});
