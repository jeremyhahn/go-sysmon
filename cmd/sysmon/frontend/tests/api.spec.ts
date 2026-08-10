import { test, expect } from '@playwright/test';
import type { Snapshot } from '../src/lib/types';

test.describe('REST API', () => {
  // The server answers 503 until the monitor completes its first collection.
  // Wait for readiness once so the suite does not race a cold start.
  test.beforeAll(async ({ request }) => {
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline) {
      const probe = await request.get('/api/snapshot');
      if (probe.status() === 200) return;
      await new Promise((r) => setTimeout(r, 200));
    }
    throw new Error('server never produced a snapshot within 20s');
  });

  test('GET /api/snapshot returns HTTP 200', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    expect(response.status()).toBe(200);
  });

  test('GET /api/snapshot returns valid JSON Content-Type', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const contentType = response.headers()['content-type'] ?? '';
    expect(contentType).toContain('application/json');
  });

  test('snapshot has all required top-level fields', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Partial<Snapshot>;

    expect(typeof snap.timestamp).toBe('string');
    expect(snap.timestamp!.length).toBeGreaterThan(0);
    expect(typeof snap.host).toBe('object');
    expect(Array.isArray(snap.cpus)).toBe(true);
    expect(typeof snap.memory).toBe('object');
    expect(Array.isArray(snap.disks)).toBe(true);
    expect(Array.isArray(snap.networks)).toBe(true);
    expect(typeof snap.load_avg).toBe('object');
    expect(typeof snap.processes).toBe('object');
  });

  test('CPU data is populated with at least one core', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(snap.cpus.length).toBeGreaterThan(0);
    const cpu = snap.cpus[0];
    expect(typeof cpu.model_name).toBe('string');
    expect(cpu.model_name.length).toBeGreaterThan(0);
    expect(typeof cpu.usage_percent).toBe('number');
    expect(cpu.usage_percent).toBeGreaterThanOrEqual(0);
    expect(cpu.usage_percent).toBeLessThanOrEqual(100);
  });

  test('memory total_bytes is greater than zero', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(snap.memory.total_bytes).toBeGreaterThan(0);
    expect(snap.memory.used_percent).toBeGreaterThanOrEqual(0);
    expect(snap.memory.used_percent).toBeLessThanOrEqual(100);
  });

  test('host info contains a non-empty hostname', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(typeof snap.host.hostname).toBe('string');
    expect(snap.host.hostname.length).toBeGreaterThan(0);
    expect(typeof snap.host.os).toBe('string');
    expect(snap.host.os.length).toBeGreaterThan(0);
    expect(typeof snap.host.kernel_version).toBe('string');
  });

  test('disks array contains at least one entry with a name', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(snap.disks.length).toBeGreaterThan(0);
    const disk = snap.disks[0];
    expect(typeof disk.name).toBe('string');
    expect(disk.name.length).toBeGreaterThan(0);
  });

  test('networks array contains at least one interface', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(snap.networks.length).toBeGreaterThan(0);
    const iface = snap.networks[0];
    expect(typeof iface.name).toBe('string');
    expect(iface.name.length).toBeGreaterThan(0);
  });

  test('load_avg has load1, load5, load15 fields', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(typeof snap.load_avg.load1).toBe('number');
    expect(typeof snap.load_avg.load5).toBe('number');
    expect(typeof snap.load_avg.load15).toBe('number');
    expect(snap.load_avg.load1).toBeGreaterThanOrEqual(0);
  });

  test('processes summary has total and running fields', async ({ request }) => {
    const response = await request.get('/api/snapshot');
    const snap = (await response.json()) as Snapshot;

    expect(typeof snap.processes.total).toBe('number');
    expect(snap.processes.total).toBeGreaterThan(0);
    expect(typeof snap.processes.running).toBe('number');
  });

  test('GET / returns HTML content', async ({ request }) => {
    const response = await request.get('/');
    expect(response.status()).toBe(200);
    const contentType = response.headers()['content-type'] ?? '';
    expect(contentType).toContain('text/html');
    const body = await response.text();
    expect(body).toContain('<html');
  });
});
