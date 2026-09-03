import type { Page } from "@playwright/test";
import snapshot from "./snapshot.json" with { type: "json" };

/**
 * Serves a deterministic snapshot over a mocked /api/events stream.
 *
 * The DataTable tests used to run against whatever the host happened to be
 * doing, on the assumption that "a host always has more than 25 processes".
 * That is false inside a container without --pid=host: the process list is one
 * or two entries, the pager never renders because it is behind `pageCount > 1`,
 * and the tests hang waiting for a Next page button that does not exist. They
 * passed on GitHub and failed on Gitea for that reason alone.
 *
 * Pagination, search and sort are properties of the component, not of the
 * machine, so the data they run against should be fixed.
 */

type Snapshot = typeof snapshot;

/** Builds `count` processes with predictable, searchable names. */
export function makeProcesses(count: number) {
  const template = snapshot.processes.process_list[0];
  return Array.from({ length: count }, (_, i) => ({
    ...template,
    pid: 1000 + i,
    // A third are named systemd-* so a search has a known, non-empty result
    // that is still a strict subset.
    name: i % 3 === 0 ? `systemd-unit-${i}` : `worker-${i}`,
    username: i % 2 === 0 ? "root" : "jhahn",
    cpu_percent: (count - i) / 10,
    memory_bytes: (count - i) * 1024 * 1024,
  }));
}

export interface MockOptions {
  /** How many processes to serve. Default 60: enough for 3 pages of 25. */
  processCount?: number;
  /**
   * Replaces the fixture's disk list. Given as partials merged over the first
   * fixture disk, so a spec states only the fields it is about and still gets
   * a renderable card.
   */
  disks?: Partial<Snapshot["disks"][number]>[];
}

/**
 * Intercepts the dashboard's event stream and serves one snapshot.
 *
 * The fulfilled body is a complete response rather than a live stream, so the
 * browser sees the event, then EOF, and reconnects — at which point the route
 * serves the same fixture again. The UI only needs one snapshot to render, and
 * a fixed body keeps the test independent of any real server.
 *
 * Must be called before page.goto, since the app connects on load.
 */
export async function mockSnapshotFeed(page: Page, opts: MockOptions = {}) {
  const count = opts.processCount ?? 60;

  const processes = makeProcesses(count);
  const disks =
    opts.disks == null
      ? snapshot.disks
      : opts.disks.map((d, i) => ({ ...snapshot.disks[0], index: i, ...d }));
  const payload: Snapshot = {
    ...snapshot,
    disks,
    processes: {
      ...snapshot.processes,
      total: count,
      running: count,
      sleeping: 0,
      idle: 0,
      stopped: 0,
      zombie: 0,
      process_list: processes,
    },
  } as Snapshot;

  // A "full" frame of the delta protocol, which is what the dashboard opens
  // the stream in. One frame is all the UI needs to render; the sequence number
  // matters only to a test that goes on to send deltas, which delta.spec.ts
  // does with its own feed.
  const body =
    `retry: 1000\n\n` +
    `event: full\ndata: ${JSON.stringify({ seq: 1, snapshot: payload })}\n\n`;

  // The path may carry a query string -- the dashboard opens the stream with
  // ?deltas=1 -- so the pattern must not be anchored at the end of the path. It
  // was, and the mock silently stopped matching the moment the parameter was
  // added: every test went on running against whatever the real host was doing,
  // which is precisely the fragility this fixture exists to remove.
  await page.route(/\/api\/events(\?|$)/, async (route) => {
    await route.fulfill({
      status: 200,
      headers: {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache",
        "X-Accel-Buffering": "no",
      },
      body,
    });
  });

  // The interval dropdown posts here; accept and ignore so the mocked page
  // never reaches a real server.
  await page.route(/\/api\/interval$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ interval_ms: 1000 }),
    });
  });
}

/** The number of processes whose name matches a "systemd" search. */
export function systemdMatchCount(total: number): number {
  return makeProcesses(total).filter((p) => p.name.includes("systemd")).length;
}
