// The history API client.
//
// Everything here is about a date range read back from Prometheus. It is
// deliberately separate from $lib/transport, which owns the live stream: the two
// answer different questions -- "what is happening now" and "what happened
// then" -- and a node can have the first without the second.

import type { History, HistoryCatalog, HistoryUnit } from "$lib/types";

const CATALOG_URL = "/api/history/metrics";
const HISTORY_URL = "/api/history";

/** A named window offered by the picker, in milliseconds. */
export interface RangePreset {
  key: string;
  label: string;
  ms: number;
}

/**
 * The presets, coarsest last. They stop at 30 days because the server refuses
 * a window wider than 90 and a month is already beyond what a machine-level
 * chart resolves usefully -- past that the step is minutes and a spike is
 * averaged out of existence.
 */
export const RANGE_PRESETS: RangePreset[] = [
  { key: "15m", label: "15 min", ms: 15 * 60_000 },
  { key: "1h", label: "1 hour", ms: 60 * 60_000 },
  { key: "6h", label: "6 hours", ms: 6 * 60 * 60_000 },
  { key: "24h", label: "24 hours", ms: 24 * 60 * 60_000 },
  { key: "7d", label: "7 days", ms: 7 * 24 * 60 * 60_000 },
  { key: "30d", label: "30 days", ms: 30 * 24 * 60 * 60_000 },
];

/** HistoryError carries what the server said, so the view can show it rather
 * than a generic failure. A node that cannot reach its Prometheus is a
 * different problem from a range that was asked for wrongly. */
export class HistoryError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "HistoryError";
    this.status = status;
  }
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(url, { signal });
  if (!response.ok) {
    // The handlers answer with a plain-text reason; anything else means
    // something other than go-sysmon replied.
    const detail = (await response.text()).trim();
    throw new HistoryError(response.status, detail || response.statusText);
  }
  return (await response.json()) as T;
}

/** fetchCatalog asks what history this node can draw. */
export function fetchCatalog(signal?: AbortSignal): Promise<HistoryCatalog> {
  return getJSON<HistoryCatalog>(CATALOG_URL, signal);
}

export interface RangeRequest {
  metric: string;
  /** The node to read. Omitted means this one. */
  node?: string | null;
  startMs: number;
  endMs: number;
}

/**
 * fetchHistory reads one metric over one window.
 *
 * The step is left to the server on purpose. It owns the ceiling that makes a
 * step necessary -- Prometheus returns at most 11000 points -- so a client that
 * chose its own would be guessing at a limit it cannot see, and would start
 * failing on exactly the widest ranges.
 */
export function fetchHistory(
  req: RangeRequest,
  signal?: AbortSignal
): Promise<History> {
  const params = new URLSearchParams({
    metric: req.metric,
    start: String(Math.round(req.startMs)),
    end: String(Math.round(req.endMs)),
  });
  if (req.node) params.set("node", req.node);
  return getJSON<History>(`${HISTORY_URL}?${params.toString()}`, signal);
}

/** formatValue renders a history value for its unit. */
export function formatValue(value: number, unit: HistoryUnit): string {
  switch (unit) {
    case "percent":
      return `${value.toFixed(1)}%`;
    case "celsius":
      return `${value.toFixed(1)}°C`;
    case "watts":
      return `${value.toFixed(1)} W`;
    case "bytes_per_second":
      return formatRate(value);
    default:
      return formatCount(value);
  }
}

const RATE_UNITS = ["B/s", "KB/s", "MB/s", "GB/s", "TB/s"];

function formatRate(bytesPerSecond: number): string {
  let value = Math.abs(bytesPerSecond);
  let unit = 0;
  while (value >= 1024 && unit < RATE_UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  const sign = bytesPerSecond < 0 ? "-" : "";
  return `${sign}${value.toFixed(unit === 0 ? 0 : 1)} ${RATE_UNITS[unit]}`;
}

function formatCount(value: number): string {
  // Counts here are alert totals and load averages: small numbers where a
  // trailing ".0" is noise but a fraction is meaningful.
  return Number.isInteger(value) ? String(value) : value.toFixed(2);
}

/**
 * formatTick labels a time axis at a resolution the window justifies. A 15
 * minute range wants seconds and a 30 day range wants dates; showing both at
 * once makes every label too long to read at chart width.
 */
export function formatTick(timeMs: number, windowMs: number): string {
  const d = new Date(timeMs);
  if (windowMs <= 6 * 60 * 60_000) {
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  if (windowMs <= 3 * 24 * 60 * 60_000) {
    return d.toLocaleString([], {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}

/**
 * toLocalInput renders an instant for a datetime-local input, which takes
 * "YYYY-MM-DDTHH:mm" in local time and rejects anything else -- including the
 * Z-suffixed ISO string toISOString produces.
 */
export function toLocalInput(timeMs: number): string {
  const d = new Date(timeMs);
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    `T${pad(d.getHours())}:${pad(d.getMinutes())}`
  );
}

/** fromLocalInput parses what toLocalInput produced, or null if it is not a
 * complete instant -- which is every keystroke while one is being typed. */
export function fromLocalInput(value: string): number | null {
  if (!value) return null;
  const ms = new Date(value).getTime();
  return Number.isFinite(ms) ? ms : null;
}
