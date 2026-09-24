import type { Page } from "@playwright/test";

/**
 * A stand-in for GET /api/fleet that answers the way the server does (#15):
 * the nodes whose name or hostname contains `q` (A-Z folded), from `offset`,
 * at most `limit` of them, with the page's counts, the serving node, and the
 * whole-fleet roll-up whatever the page.
 *
 * The fixture's nodes are taken to be in the server's order already: ordering
 * is the server's, pinned by pkg/fleet's tests, and the view shows what it is
 * sent. What these specs test is the browser's side -- what it asks for and
 * how it draws the answer.
 */

type Json = Record<string, unknown>;

export interface FleetFixture {
  /** Every node, in the order the server sorts them. */
  nodes: Json[];
  rollup?: Json | null;
  /** The serving node; defaults to the node marked self. */
  self?: Json | null;
}

/** Matches /api/fleet with or without a query, and nothing below it. */
export const FLEET_API = /\/api\/fleet(\?[^/]*)?$/;

function foldASCII(s: string): string {
  return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
}

/** The response the server would give for these parameters. */
export function fleetResponse(f: FleetFixture, params: URLSearchParams): Json {
  const q = (params.get("q") ?? "").trim();
  let offset = Number(params.get("offset") ?? 0);
  const limit = Number(params.get("limit") ?? 0);
  const anchor = params.get("anchor") ?? "";
  const needle = foldASCII(q);
  const matched = f.nodes.filter(
    (n) =>
      needle === "" ||
      foldASCII(String(n.name)).includes(needle) ||
      foldASCII(String(n.hostname ?? "")).includes(needle)
  );
  const rank = matched.findIndex((n) => n.name === anchor);
  if (anchor !== "" && limit > 0 && rank >= 0) offset = rank - (rank % limit);
  return {
    enabled: true,
    nodes: matched.slice(offset, limit > 0 ? offset + limit : undefined),
    page: { q, offset, limit, matched: matched.length, total: f.nodes.length },
    self: f.self ?? f.nodes.find((n) => n.self === true) ?? null,
    rollup: f.rollup ?? null,
  };
}

/**
 * serveFleet routes GET /api/fleet to the fixture, re-read on every request
 * when it is a function, and returns the log of request URLs so a spec can
 * assert what the browser asked for.
 */
export async function serveFleet(
  page: Page,
  fixture: FleetFixture | (() => FleetFixture)
): Promise<URL[]> {
  const requests: URL[] = [];
  await page.route(FLEET_API, (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const f = typeof fixture === "function" ? fixture() : fixture;
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(fleetResponse(f, url.searchParams)),
    });
  });
  return requests;
}

/** A healthy, reachable fleet node carrying every field the view reads. */
export function fleetNode(name: string, over: Json = {}): Json {
  return {
    name,
    self: false,
    reachable: true,
    error: "",
    age_seconds: 1,
    status: "healthy",
    source: "summary",
    build_match: "same",
    name_match: "same",
    node_name: name,
    hostname: `${name}.lan`,
    version: "1.0.0",
    git_commit: "a46f6cf",
    uptime_seconds: 3600,
    critical_count: 0,
    warning_count: 0,
    alerts: [],
    cpu_percent: 5,
    cpu_available: true,
    memory_percent: 20,
    load_per_core: 0.1,
    swap_percent: 0,
    swap_counters_available: true,
    swap_rate_available: true,
    swap_in_rate: 0,
    swap_out_rate: 0,
    worst_disk_percent: 30,
    raid_arrays: 0,
    raid_unhealthy: 0,
    raid_degraded: false,
    smart_capable: 0,
    smart_unhealthy: 0,
    max_temp_c: 40,
    power_w: 0,
    power_reported: false,
    net_rx_rate: 0,
    net_tx_rate: 0,
    net_reported: false,
    ...over,
  };
}
