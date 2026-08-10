import type { Page } from "@playwright/test";
import snapshot from "./snapshot.json" with { type: "json" };

/**
 * Serves a deterministic snapshot over a mocked /ws connection.
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
}

/**
 * Intercepts the dashboard's WebSocket and pushes one snapshot.
 *
 * Must be called before page.goto, since the app connects on load.
 */
export async function mockSnapshotFeed(page: Page, opts: MockOptions = {}) {
  const count = opts.processCount ?? 60;

  const processes = makeProcesses(count);
  const payload: Snapshot = {
    ...snapshot,
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

  await page.routeWebSocket(/\/ws$/, (ws) => {
    // Never connect upstream: the point is to be independent of the server.
    ws.send(JSON.stringify(payload));
    // The client sends set_interval commands; accept and ignore them.
    ws.onMessage(() => ws.send(JSON.stringify(payload)));
  });
}

/** The number of processes whose name matches a "systemd" search. */
export function systemdMatchCount(total: number): number {
  return makeProcesses(total).filter((p) => p.name.includes("systemd")).length;
}
