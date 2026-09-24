import { expect, type Locator } from "@playwright/test";
import type { HistoryPoint } from "../../src/lib/types";

/**
 * History as a real range query returns it, and a way to see what the chart
 * drew from it.
 *
 * A range query answers on a grid: an evaluation at start, start + step,
 * start + 2*step and so on up to end, each with a value if the series had a
 * sample within the database's lookback, and simply absent if not. The chart
 * reads the space between two points against the step to find an outage
 * (#179), so a mock that hand-writes three points a half-hour apart under a
 * fifteen-second step is not a sparse line -- it is three readings separated
 * by outages, drawn as three dots. Every history mock is built here instead,
 * so none of them can drift off the grid again.
 */

/** targetPoints is pkg/prom.TargetPoints, the resolution the server draws at. */
const targetPoints = 600;

/**
 * stepFor is the step the server chooses for a window (pkg/prom.StepFor): the
 * window over 600 points, rounded to whole seconds, never under a second. A
 * mock answering with it keeps its responses the size the real ones are --
 * a thirty-day window at a fifteen-second step would be 172,801 points.
 */
export function stepFor(windowMs: number): number {
  if (windowMs <= 0) return 1000;
  const step = windowMs / targetPoints;
  if (step < 1000) return 1000;
  return Math.round(step / 1000) * 1000;
}

/** GridOptions describes one series on a range's grid. */
export interface GridOptions {
  startMs: number;
  endMs: number;
  stepMs: number;
  /** The reading at evaluation k, the k-th step from start. */
  value: (k: number) => number;
  /**
   * Evaluations with no reading, each [from, to) in step indices: the node
   * was down, or not yet recording. Omitted means the series is contiguous.
   */
  gaps?: Array<[number, number]>;
}

/**
 * gridPoints builds a series the way a range query answers: a point at every
 * evaluation from start to end inclusive, except the ones inside a gap.
 */
export function gridPoints({ startMs, endMs, stepMs, value, gaps = [] }: GridOptions): HistoryPoint[] {
  const out: HistoryPoint[] = [];
  const last = Math.floor((endMs - startMs) / stepMs);
  for (let k = 0; k <= last; k++) {
    if (gaps.some(([from, to]) => k >= from && k < to)) continue;
    out.push({ t: startMs + k * stepMs, v: value(k) });
  }
  return out;
}

/** stepsIn is how many steps of stepMs a window spans. */
export function stepsIn(startMs: number, endMs: number, stepMs: number): number {
  return Math.floor((endMs - startMs) / stepMs);
}

/** BodyOptions shapes a mocked GET /api/history answer. */
export interface BodyOptions {
  /** One series per name; [""] -- one unnamed series -- by default. */
  names?: string[];
  /** The step answered with; the server's own choice by default. */
  stepMs?: number;
  unit?: string;
  /** The reading of series i at evaluation k; a gentle wave by default. */
  value?: (k: number, i: number) => number;
  /**
   * The gaps, in step indices, of every series; by default the lead-in
   * alone, so any chart drawn from it can be checked with expectLine.
   */
  gaps?: (steps: number) => Array<[number, number]>;
}

/**
 * historyBody answers GET /api/history for the request in params: the window
 * it asked for, on the grid a range query would return for it.
 */
export function historyBody(params: URLSearchParams, opts: BodyOptions = {}) {
  const start = Number(params.get("start"));
  const end = Number(params.get("end"));
  const stepMs = opts.stepMs ?? stepFor(end - start);
  const steps = stepsIn(start, end, stepMs);
  const value = opts.value ?? ((k: number, i: number) => 23 + 10 * i + 4 * Math.sin((2 * Math.PI * k) / steps));
  const gaps = (opts.gaps ?? ((n: number) => [leadInGap(n)]))(steps);
  return {
    enabled: true,
    node: params.get("node") ?? "local",
    metric: params.get("metric"),
    label: params.get("metric"),
    unit: opts.unit ?? "percent",
    start_ms: start,
    end_ms: end,
    step_ms: stepMs,
    series: (opts.names ?? [""]).map((name, i) => ({
      name,
      points: gridPoints({ startMs: start, endMs: end, stepMs, value: (k) => value(k, i), gaps }),
    })),
  };
}

/** Plot is a history chart's plot area as painted, and strips read from it. */
export interface Plot {
  /** The plot's left and right edges, in CSS pixels from the canvas' left. */
  left: number;
  right: number;
  /** Each requested strip as raw RGBA; empty if the plot is not painted. */
  cols: number[][];
}

/**
 * readPlot finds a history chart's plot area and reads vertical strips of it,
 * each at a fraction of the window across it, as raw RGBA.
 *
 * The plot area is found on the canvas rather than assumed from the grid
 * option: ECharts moves the grid's edges to keep the axis labels inside the
 * chart, so where it ends depends on how wide the labels at the ends happen
 * to be -- that is, on the time of day. The value axis' split lines, and the
 * time axis' line beneath them, run across the plot area, so the widest
 * unbroken painted rows are those lines: the top one bounds the plot on both
 * sides, and the strips are taken between it and the axis line, clear of
 * both.
 *
 * They are read in one go, from one frame: the chart repaints whenever its
 * option is set again, and a strip read mid-repaint is blank.
 */
export async function readPlot(chart: Locator, fractions: number[]): Promise<Plot> {
  return chart.locator("canvas").first().evaluate((el, fs) => {
    const canvas = el as HTMLCanvasElement;
    const ctx = canvas.getContext("2d");
    if (ctx == null) throw new Error("history chart canvas has no 2d context");
    const { width, height } = canvas;
    const pixels = ctx.getImageData(0, 0, width, height).data;
    const painted = (x: number, y: number) => pixels[(y * width + x) * 4 + 3] !== 0;

    // Each row's longest unbroken painted run. A label beside a split line
    // is separated from it by the axis' margin, so it never joins the run.
    const runs: Array<{ y: number; left: number; right: number }> = [];
    for (let y = 0; y < height; y++) {
      let best = { y, left: 0, right: -1 };
      let from = -1;
      for (let x = 0; x <= width; x++) {
        if (x < width && painted(x, y)) {
          if (from < 0) from = x;
          continue;
        }
        if (from >= 0 && x - 1 - from > best.right - best.left) best = { y, left: from, right: x - 1 };
        from = -1;
      }
      runs.push(best);
    }
    const widest = Math.max(...runs.map((r) => r.right - r.left));
    // The split lines and the axis line span the plot -- the axis line by a
    // pixel more -- and nothing else on the chart comes close. The first is
    // the top split line, and bounds the plot exactly.
    const lines = runs.filter((r) => widest > 0 && r.right - r.left >= 0.9 * widest);
    if (lines.length < 2) return { left: 0, right: 0, cols: fs.map(() => []) };
    const { left, right } = lines[0];
    const top = lines[0].y + 2;
    const bottom = lines[lines.length - 1].y - 2;
    const ratio = width / canvas.clientWidth;

    return {
      left: left / ratio,
      right: right / ratio,
      cols: fs.map((f) => {
        const x = Math.round(left + f * (right - left));
        return Array.from(ctx.getImageData(x, top, 1, bottom - top).data);
      }),
    };
  }, fractions);
}

/**
 * drawn waits for a painted frame and hands its strips to check. The split
 * lines cross every strip, so a strip with nothing on it at all -- or none,
 * before the plot is painted -- is a frame not drawn yet, not an empty
 * stretch of chart.
 */
export async function drawn(chart: Locator, fractions: number[], check: (cols: number[][]) => void) {
  await expect(async () => {
    const { cols } = await readPlot(chart, fractions);
    for (const col of cols) expect(col.some((c) => c !== 0)).toBe(true);
    check(cols);
  }).toPass();
}

/**
 * LEAD_IN is the stretch at the start of a window a line-checking mock leaves
 * empty, as a fraction of the window: the node began recording a little
 * after the window opens. It gives expectLine a strip of chart with nothing
 * drawn on it to compare against.
 */
export const LEAD_IN = 0.1;

/**
 * leadInGap is the gap, in step indices, that leaves LEAD_IN of a window of
 * `steps` steps empty.
 */
export function leadInGap(steps: number): [number, number] {
  return [0, Math.ceil(LEAD_IN * steps)];
}

/**
 * expectLine asserts the chart draws a continuous line from just after its
 * lead-in to the end of the window: every strip across that stretch differs
 * from the empty lead-in. A chart handed points off the grid draws dots with
 * nothing between them, and the strips between the dots match the lead-in.
 */
export async function expectLine(chart: Locator) {
  const across = [0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9];
  await drawn(chart, [LEAD_IN / 2, ...across], ([empty, ...strips]) => {
    for (const strip of strips) expect(strip).not.toEqual(empty);
  });
}
