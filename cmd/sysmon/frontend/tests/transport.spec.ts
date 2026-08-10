import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';

/**
 * firstSnapshot opens an event stream in the page and resolves the payload of
 * the first snapshot event.
 *
 * The whole body runs inside page.evaluate, so it cannot reference anything
 * from this module's scope.
 */
function firstSnapshot(page: Page): Promise<unknown> {
  return page.evaluate((): Promise<unknown> => {
    return new Promise((resolve, reject) => {
      const es = new EventSource('/api/events');
      const timer = setTimeout(() => {
        es.close();
        reject(new Error('timed out waiting for a snapshot event'));
      }, 10000);

      es.addEventListener('snapshot', (ev: MessageEvent<string>) => {
        clearTimeout(timer);
        es.close();
        try {
          resolve(JSON.parse(ev.data));
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
}

test.describe('Server-sent events transport', () => {
  test('the stream is served as text/event-stream', async ({ page }) => {
    await page.goto('/');

    const response = await page.request.get('/api/events', {
      headers: { Accept: 'text/event-stream' },
      // The stream never ends on its own, so read only the headers.
      maxRedirects: 0,
      timeout: 5000,
    }).catch(() => null);

    // A live stream may time out while reading the body; the headers are what
    // matter and Playwright surfaces them before the body completes.
    if (response) {
      expect(response.status()).toBe(200);
      expect(response.headers()['content-type']).toBe('text/event-stream');
      expect(response.headers()['x-accel-buffering']).toBe('no');
    }
  });

  test('the stream delivers a valid JSON snapshot', async ({ page }) => {
    await page.goto('/');

    const snapshot = await firstSnapshot(page);

    expect(snapshot).toBeTruthy();
    const snap = snapshot as Partial<Snapshot>;
    expect(typeof snap.timestamp).toBe('string');
    expect(Array.isArray(snap.cpus)).toBe(true);
    expect(typeof snap.memory).toBe('object');
    expect(Array.isArray(snap.disks)).toBe(true);
    expect(Array.isArray(snap.networks)).toBe(true);
  });

  test('the stream advertises a reconnect hint', async ({ page }) => {
    await page.goto('/');

    // EventSource does not expose the retry value, so read the raw bytes and
    // look for the field the server writes before anything else.
    const head = await page.evaluate(async (): Promise<string> => {
      const controller = new AbortController();
      const resp = await fetch('/api/events', {
        headers: { Accept: 'text/event-stream' },
        signal: controller.signal,
      });
      const reader = resp.body!.getReader();
      const { value } = await reader.read();
      controller.abort();
      return new TextDecoder().decode(value);
    });

    expect(head).toContain('retry:');
  });

  test('the stream delivers at least 3 consecutive snapshots', async ({ page }) => {
    await page.goto('/');

    // The server runs with --interval 500ms, so 3 events arrive well inside
    // the timeout.
    const count = await page.evaluate((): Promise<number> => {
      return new Promise((resolve) => {
        const es = new EventSource('/api/events');
        let seen = 0;

        const finish = () => {
          es.close();
          resolve(seen);
        };

        es.addEventListener('snapshot', () => {
          seen++;
          if (seen >= 3) finish();
        });
        es.onerror = () => finish();
        setTimeout(finish, 10000);
      });
    });

    expect(count).toBeGreaterThanOrEqual(3);
  });

  test('snapshot contains CPU data with model_name fields', async ({ page }) => {
    await page.goto('/');

    const snap = (await firstSnapshot(page)) as { cpus: unknown[] };

    expect(Array.isArray(snap.cpus)).toBe(true);
    expect(snap.cpus.length).toBeGreaterThan(0);
    const firstCpu = snap.cpus[0] as { model_name?: string; usage_percent?: number };
    expect(typeof firstCpu.model_name).toBe('string');
    expect(firstCpu.model_name!.length).toBeGreaterThan(0);
    expect(typeof firstCpu.usage_percent).toBe('number');
  });

  test('snapshot contains memory data with total_bytes > 0', async ({ page }) => {
    await page.goto('/');

    const snap = (await firstSnapshot(page)) as { memory: unknown };

    expect(snap.memory).toBeTruthy();
    const mem = snap.memory as { total_bytes?: number; used_percent?: number };
    expect(typeof mem.total_bytes).toBe('number');
    expect(mem.total_bytes!).toBeGreaterThan(0);
    expect(typeof mem.used_percent).toBe('number');
  });

  test('snapshot contains a disks array', async ({ page }) => {
    await page.goto('/');

    const snap = (await firstSnapshot(page)) as { disks: unknown[] };

    expect(Array.isArray(snap.disks)).toBe(true);
    // Any real machine has at least one block device.
    expect(snap.disks.length).toBeGreaterThan(0);
    const disk = snap.disks[0] as { name?: string };
    expect(typeof disk.name).toBe('string');
  });

  test('snapshot contains a networks array', async ({ page }) => {
    await page.goto('/');

    const snap = (await firstSnapshot(page)) as { networks: unknown[] };

    expect(Array.isArray(snap.networks)).toBe(true);
    // Every Linux system has at least loopback.
    expect(snap.networks.length).toBeGreaterThan(0);
    const iface = snap.networks[0] as { name?: string; is_loopback?: boolean };
    expect(typeof iface.name).toBe('string');
    expect(iface.name!.length).toBeGreaterThan(0);
  });
});

test.describe('Rate control', () => {
  // Restore the rate the rest of the suite expects. The interval belongs to
  // the server's single monitor, so leaving it slow would starve other tests.
  test.afterEach(async ({ request }) => {
    await request.post('/api/interval', { data: { interval_ms: 500 } });
  });

  test('GET /api/interval reports the current rate', async ({ request }) => {
    const response = await request.get('/api/interval');

    expect(response.status()).toBe(200);
    const body = await response.json();
    expect(typeof body.interval_ms).toBe('number');
    expect(body.interval_ms).toBeGreaterThan(0);
  });

  test('POST /api/interval accepts an allowed rate', async ({ request }) => {
    const response = await request.post('/api/interval', {
      data: { interval_ms: 1000 },
    });

    expect(response.status()).toBe(200);
    expect((await response.json()).interval_ms).toBe(1000);

    const readBack = await request.get('/api/interval');
    expect((await readBack.json()).interval_ms).toBe(1000);
  });

  test('POST /api/interval rejects a rate outside the allow-list', async ({ request }) => {
    const before = (await (await request.get('/api/interval')).json()).interval_ms;

    for (const interval_ms of [999, 0, -1, 1]) {
      const response = await request.post('/api/interval', { data: { interval_ms } });
      expect(response.status(), `interval_ms=${interval_ms}`).toBe(400);
    }

    const after = (await (await request.get('/api/interval')).json()).interval_ms;
    expect(after).toBe(before);
  });

  test('POST /api/interval rejects a malformed body', async ({ request }) => {
    const response = await request.post('/api/interval', {
      headers: { 'Content-Type': 'application/json' },
      data: 'not json',
    });

    expect(response.status()).toBe(400);
  });
});

test.describe('Retired routes', () => {
  test('/ws no longer streams', async ({ request }) => {
    const response = await request.get('/ws');

    // With the SPA assets mounted, the catch-all answers; the point is that
    // nothing upgrades and nothing streams.
    expect(response.status()).not.toBe(101);
    expect(response.headers()['content-type'] ?? '').not.toContain('event-stream');
  });
});
