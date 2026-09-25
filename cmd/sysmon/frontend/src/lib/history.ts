// The history API client.
//
// Everything here is about a date range read back from Prometheus. It is
// deliberately separate from $lib/transport, which owns the live stream: the two
// answer different questions -- "what is happening now" and "what happened
// then" -- and a node can have the first without the second.

import type {
  History,
  HistoryCatalog,
  HistoryPoint,
  HistoryUnit,
} from "$lib/types";

// Relative to the page, like every API URL: see EVENTS_URL in transport.ts.
const CATALOG_URL = "api/history/metrics";
const HISTORY_URL = "api/history";

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

/** HistoryStepError is a range whose step cannot be drawn from: absent, zero,
 * negative or not a number. The server always sends a positive step, so this
 * is something other than go-sysmon answering -- and without the step a chart
 * cannot tell an outage from the spacing between its points (see
 * lineWithBreaks), so the range is refused rather than drawn. */
export class HistoryStepError extends Error {
  readonly stepMs: unknown;
  constructor(stepMs: unknown) {
    super(`history: the range has no usable step (${String(stepMs)})`);
    this.name = "HistoryStepError";
    this.stepMs = stepMs;
  }
}

/** checkStep throws a HistoryStepError unless stepMs is a positive, finite
 * number of milliseconds. */
function checkStep(stepMs: unknown): asserts stepMs is number {
  if (typeof stepMs !== "number" || !Number.isFinite(stepMs) || stepMs <= 0) {
    throw new HistoryStepError(stepMs);
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
export async function fetchHistory(
  req: RangeRequest,
  signal?: AbortSignal
): Promise<History> {
  const params = new URLSearchParams({
    metric: req.metric,
    start: String(Math.round(req.startMs)),
    end: String(Math.round(req.endMs)),
  });
  if (req.node) params.set("node", req.node);
  const history = await getJSON<History>(
    `${HISTORY_URL}?${params.toString()}`,
    signal
  );
  // Checked here, where it arrives, so a range that cannot be drawn fails as
  // a request -- the chart says history is unavailable -- rather than
  // throwing out of a chart's render later.
  checkStep(history.step_ms);
  return history;
}

/**
 * HistoryLine is one line a history chart draws: a series and the step of the
 * range it came from. The step travels with the line rather than with the
 * chart because a chart can combine metrics -- disk read and write -- and it
 * is each line's own step that says where that line has a gap.
 */
export interface HistoryLine {
  name: string;
  points: HistoryPoint[];
  stepMs: number;
}

/**
 * LinePoint is one entry of a history line as handed to the chart: a reading,
 * or a null value where the line must break.
 */
export type LinePoint = [timeMs: number, value: number | null];

/**
 * gapSteps is how many step intervals apart two consecutive points must be
 * before the line between them is broken. See lineWithBreaks.
 */
const gapSteps = 1.5;

/**
 * lineWithBreaks turns a series' points into a chart line that breaks wherever
 * the data does.
 *
 * A range query answers on a grid: Prometheus evaluates the query at start,
 * start + step, start + 2*step and so on, and each evaluation yields a value
 * if the series has a sample within the database's lookback before it.
 * Samples are written at the collection interval, and a node is refused any
 * cadence slower than the lookback (docs/server/api.md), so an ordinary
 * cadence -- however sparse, and whatever the step -- has a value at every
 * evaluation. A missing evaluation is therefore never spacing: it is the node
 * down, unreachable or not recording. Prometheus leaves it out of the
 * response, and a chart joins whatever points it is given, so without a break
 * a two-hour outage is drawn as a plausible straight line.
 *
 * So two consecutive points more than
 *
 *	gapSteps * step  (one and a half step intervals)
 *
 * apart have at least one evaluation missing between them, and a null is
 * inserted at the first missing one. Adjacent points on the grid are exactly
 * one step apart and the nearest gap is two, so the threshold sits midway:
 * the millisecond the server's float-seconds decoding can shift a timestamp by
 * cannot tip an adjacent pair over it or a missing point under it.
 *
 * A point before the first reading or after the last is not a gap to mark:
 * the chart's axis spans the requested window, so the empty lead-in and tail
 * are already drawn as empty.
 */
export function lineWithBreaks(
  points: readonly HistoryPoint[],
  stepMs: number
): LinePoint[] {
  checkStep(stepMs);
  const threshold = gapSteps * stepMs;
  const line: LinePoint[] = [];
  points.forEach((pt, i) => {
    const prev = points[i - 1];
    if (prev != null && pt.t - prev.t > threshold) {
      line.push([prev.t + stepMs, null]);
    }
    line.push([pt.t, pt.v]);
  });
  return line;
}

/**
 * isLone reports whether line[i] is a reading with no reading either side of
 * it -- one evaluation between two gaps, or the only point in the range. A
 * line needs two points to draw a segment, so a chart drawing lines without
 * markers shows nothing for it; the chart marks it so it is not lost.
 */
export function isLone(line: readonly LinePoint[], i: number): boolean {
  const hasValue = (j: number) => line[j] != null && line[j][1] != null;
  return hasValue(i) && !hasValue(i - 1) && !hasValue(i + 1);
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

/**
 * unitLabel names a unit the way a reader says it, for the chart's accessible
 * name. Empty for a plain count, which has no unit to name.
 */
export function unitLabel(unit: HistoryUnit): string {
  return UNIT_LABELS[unit] ?? "";
}

const UNIT_LABELS: Record<HistoryUnit, string> = {
  percent: "percent",
  bytes_per_second: "bytes per second",
  celsius: "degrees Celsius",
  watts: "watts",
  count: "",
};

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
