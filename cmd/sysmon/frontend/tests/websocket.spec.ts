import { test, expect } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';

test.describe('WebSocket Transport', () => {
  test('WebSocket connects successfully to /ws', async ({ page }) => {
    await page.goto('/');

    const connected = await page.evaluate((): Promise<boolean> => {
      return new Promise((resolve) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onopen = () => {
          ws.close();
          resolve(true);
        };
        ws.onerror = () => resolve(false);
        // Timeout guard
        setTimeout(() => resolve(false), 8000);
      });
    });

    expect(connected).toBe(true);
  });

  test('WebSocket receives a valid JSON snapshot on first message', async ({ page }) => {
    await page.goto('/');

    const snapshot = await page.evaluate((): Promise<unknown> => {
      return new Promise((resolve, reject) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onmessage = (ev) => {
          ws.close();
          try {
            resolve(JSON.parse(ev.data as string));
          } catch {
            reject(new Error('Failed to parse WebSocket message as JSON'));
          }
        };
        ws.onerror = () => reject(new Error('WebSocket error'));
        setTimeout(() => reject(new Error('Timeout waiting for first message')), 10000);
      });
    });

    expect(snapshot).toBeTruthy();
    const snap = snapshot as Partial<Snapshot>;
    expect(typeof snap.timestamp).toBe('string');
    expect(Array.isArray(snap.cpus)).toBe(true);
    expect(typeof snap.memory).toBe('object');
    expect(Array.isArray(snap.disks)).toBe(true);
    expect(Array.isArray(snap.networks)).toBe(true);
  });

  test('WebSocket receives at least 3 consecutive snapshots', async ({ page }) => {
    await page.goto('/');

    // With --interval 500ms we expect 3 messages within ~5 seconds comfortably.
    const messageCount = await page.evaluate((): Promise<number> => {
      return new Promise((resolve) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        let count = 0;
        ws.onmessage = () => {
          count++;
          if (count >= 3) {
            ws.close();
            resolve(count);
          }
        };
        ws.onerror = () => resolve(count);
        setTimeout(() => {
          ws.close();
          resolve(count);
        }, 10000);
      });
    });

    expect(messageCount).toBeGreaterThanOrEqual(3);
  });

  test('snapshot contains CPU data with model_name fields', async ({ page }) => {
    await page.goto('/');

    const cpus = await page.evaluate((): Promise<unknown[]> => {
      return new Promise((resolve, reject) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onmessage = (ev) => {
          ws.close();
          try {
            const data = JSON.parse(ev.data as string) as { cpus: unknown[] };
            resolve(data.cpus);
          } catch {
            reject(new Error('Parse error'));
          }
        };
        ws.onerror = () => reject(new Error('WebSocket error'));
        setTimeout(() => reject(new Error('Timeout')), 10000);
      });
    });

    expect(Array.isArray(cpus)).toBe(true);
    expect(cpus.length).toBeGreaterThan(0);
    const firstCpu = cpus[0] as { model_name?: string; usage_percent?: number };
    expect(typeof firstCpu.model_name).toBe('string');
    expect(firstCpu.model_name!.length).toBeGreaterThan(0);
    expect(typeof firstCpu.usage_percent).toBe('number');
  });

  test('snapshot contains memory data with total_bytes > 0', async ({ page }) => {
    await page.goto('/');

    const memory = await page.evaluate((): Promise<unknown> => {
      return new Promise((resolve, reject) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onmessage = (ev) => {
          ws.close();
          try {
            const data = JSON.parse(ev.data as string) as { memory: unknown };
            resolve(data.memory);
          } catch {
            reject(new Error('Parse error'));
          }
        };
        ws.onerror = () => reject(new Error('WebSocket error'));
        setTimeout(() => reject(new Error('Timeout')), 10000);
      });
    });

    expect(memory).toBeTruthy();
    const mem = memory as { total_bytes?: number; used_percent?: number };
    expect(typeof mem.total_bytes).toBe('number');
    expect(mem.total_bytes!).toBeGreaterThan(0);
    expect(typeof mem.used_percent).toBe('number');
  });

  test('snapshot contains a disks array', async ({ page }) => {
    await page.goto('/');

    const disks = await page.evaluate((): Promise<unknown[]> => {
      return new Promise((resolve, reject) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onmessage = (ev) => {
          ws.close();
          try {
            const data = JSON.parse(ev.data as string) as { disks: unknown[] };
            resolve(data.disks);
          } catch {
            reject(new Error('Parse error'));
          }
        };
        ws.onerror = () => reject(new Error('WebSocket error'));
        setTimeout(() => reject(new Error('Timeout')), 10000);
      });
    });

    expect(Array.isArray(disks)).toBe(true);
    // Any real machine has at least one block device
    expect(disks.length).toBeGreaterThan(0);
    const disk = disks[0] as { name?: string };
    expect(typeof disk.name).toBe('string');
  });

  test('snapshot contains a networks array', async ({ page }) => {
    await page.goto('/');

    const networks = await page.evaluate((): Promise<unknown[]> => {
      return new Promise((resolve, reject) => {
        const ws = new WebSocket(`ws://${location.host}/ws`);
        ws.onmessage = (ev) => {
          ws.close();
          try {
            const data = JSON.parse(ev.data as string) as { networks: unknown[] };
            resolve(data.networks);
          } catch {
            reject(new Error('Parse error'));
          }
        };
        ws.onerror = () => reject(new Error('WebSocket error'));
        setTimeout(() => reject(new Error('Timeout')), 10000);
      });
    });

    expect(Array.isArray(networks)).toBe(true);
    // Every Linux system has at least loopback
    expect(networks.length).toBeGreaterThan(0);
    const iface = networks[0] as { name?: string; is_loopback?: boolean };
    expect(typeof iface.name).toBe('string');
    expect(iface.name!.length).toBeGreaterThan(0);
  });
});
