import { test, expect, type Locator, type Page } from "@playwright/test";
import { mockSnapshotFeed } from "./fixtures/mock-sse";
import { drawn, gridPoints, readPlot } from "./fixtures/history";
import {
  HistoryStepError,
  isLone,
  lineWithBreaks,
  type LinePoint,
} from "../src/lib/history";
import type { HistoryPoint } from "../src/lib/types";

/**
 * A gap in history is drawn as a break, not a line (#179).
 *
 * A range query answers on a grid of steps, and Prometheus leaves out an
 * evaluation that found no sample -- the node was down, unreachable or not
 * recording. A chart joins whatever points it is given, so unless the missing
 * steps are marked a two-hour outage is drawn as a plausible straight line
 * across the time the node was gone. What is checked here:
 *
 *  1. lineWithBreaks marks every missing step, and nothing else -- an evenly
 *     spaced series is never broken, however far apart its points, and the
 *     millisecond of jitter in a decoded timestamp does not break one either.
 *  2. The chart really draws the break: the canvas is read back, and the
 *     column through a gap is exactly the column of an empty stretch of chart.
 *     A reading alone between two gaps is still drawn, and every line of a
 *     multi-line chart breaks at its own gaps.
 *  3. A range without a usable step is refused as a failed read, not drawn.
 */

const STEP = 6_000;

/** points is one reading per step over [fromStep, toStep), at value v. */
function points(fromStep: number, toStep: number, v: number, stepMs = STEP): HistoryPoint[] {
  return gridPoints({
    startMs: fromStep * stepMs,
    endMs: (toStep - 1) * stepMs,
    stepMs,
    value: () => v,
  });
}

// --- 1. the breaks ------------------------------------------------------------

test.describe("lineWithBreaks", () => {
  test("an unbroken series is passed through unchanged", () => {
    const pts = points(0, 10, 5);
    expect(lineWithBreaks(pts, STEP)).toEqual(pts.map((p) => [p.t, p.v]));
  });

  test("an evenly sparse series is not broken", () => {
    // A thirty-day range: the server's step is 72 minutes, so the points are
    // 72 minutes apart and every one of them is present.
    const step = 72 * 60_000;
    const pts = points(0, 600, 1, step);
    expect(lineWithBreaks(pts, step).some(([, v]) => v === null)).toBe(false);
  });

  test("a millisecond of timestamp jitter is not a gap", () => {
    // Timestamps arrive as float seconds, and a point can decode a millisecond
    // either side of its grid instant -- written out by hand, since being just
    // off the grid is the point.
    const pts: HistoryPoint[] = [
      { t: 0, v: 1 },
      { t: STEP + 1, v: 2 },
      { t: 2 * STEP - 1, v: 3 },
      { t: 3 * STEP + 1, v: 4 },
    ];
    expect(lineWithBreaks(pts, STEP).some(([, v]) => v === null)).toBe(false);
  });

  test("a single missing step is a break at that step", () => {
    const pts = gridPoints({ startMs: 0, endMs: 3 * STEP, stepMs: STEP, value: (k) => k, gaps: [[2, 3]] });
    const want: LinePoint[] = [
      [0, 0],
      [STEP, 1],
      [2 * STEP, null],
      [3 * STEP, 3],
    ];
    expect(lineWithBreaks(pts, STEP)).toEqual(want);
  });

  test("a long outage is one break, and every gap gets its own", () => {
    const pts = [...points(0, 3, 1), ...points(1200, 1202, 2), ...points(1210, 1211, 3)];
    const want: LinePoint[] = [
      [0, 1],
      [STEP, 1],
      [2 * STEP, 1],
      [3 * STEP, null],
      [1200 * STEP, 2],
      [1201 * STEP, 2],
      [1202 * STEP, null],
      [1210 * STEP, 3],
    ];
    expect(lineWithBreaks(pts, STEP)).toEqual(want);
  });

  test("an empty series is an empty line", () => {
    expect(lineWithBreaks([], STEP)).toEqual([]);
  });

  test("a step that cannot be drawn from is refused", () => {
    for (const step of [0, -STEP, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(() => lineWithBreaks(points(0, 3, 1), step)).toThrow(HistoryStepError);
    }
  });
});

test.describe("isLone", () => {
  const line: LinePoint[] = [
    [0, 1],
    [1, null],
    [2, 5],
    [3, null],
    [4, 7],
    [5, 8],
    [6, null],
    [7, 9],
  ];

  test("a reading between two breaks, or at an end beside one, is lone", () => {
    expect(isLone(line, 0)).toBe(true);
    expect(isLone(line, 2)).toBe(true);
    expect(isLone(line, 7)).toBe(true);
    expect(isLone([[0, 3]], 0)).toBe(true);
  });

  test("a reading with a neighbour, a break, or no entry at all is not", () => {
    expect(isLone(line, 4)).toBe(false);
    expect(isLone(line, 5)).toBe(false);
    expect(isLone(line, 1)).toBe(false);
    expect(isLone(line, 99)).toBe(false);
    expect(isLone([], 0)).toBe(false);
  });
});

// --- 2. the chart ---------------------------------------------------------------

const WINDOW_MS = 60 * 60_000;
const STEPS = WINDOW_MS / STEP; // 600

const CATALOG = {
  enabled: true,
  metrics: [{ key: "swap", label: "Swap", unit: "percent", split_by: "" }],
};

/** A line as the mock sends it: the grid at v, less its gaps. */
interface MockLine {
  name: string;
  gaps: Array<[number, number]>;
  v: number;
}

async function serve(page: Page, lines: MockLine[], stepMs: unknown = STEP) {
  await mockSnapshotFeed(page);
  await page.route(/\/api\/history\/metrics$/, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CATALOG) })
  );
  await page.route(/\/api\/history\?/, (route) => {
    const params = new URL(route.request().url()).searchParams;
    const start = Number(params.get("start"));
    const end = Number(params.get("end"));
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        enabled: true,
        node: "local",
        metric: "swap",
        label: "Swap",
        unit: "percent",
        start_ms: start,
        end_ms: end,
        step_ms: stepMs,
        series: lines.map((l) => ({
          name: l.name,
          points: gridPoints({ startMs: start, endMs: end, stepMs: STEP, value: () => l.v, gaps: l.gaps }),
        })),
      }),
    });
  });
}

const results = (page: Page) => page.getByRole("region", { name: "History search results" });

async function chartSwap(page: Page): Promise<Locator> {
  await page.goto("/");
  await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
  await page.getByLabel("Search history").fill("swap");
  await page.getByRole("option", { name: "Swap", exact: true }).click();
  const chart = results(page).getByRole("img", { name: "History chart in percent" });
  await expect(chart.locator("canvas").first()).toBeVisible();
  return chart;
}

/** at is the step a fraction of the way across the window. */
const at = (fraction: number) => Math.round(fraction * STEPS);

/** END is past the last evaluation, for a gap that runs to the window's end. */
const END = STEPS + 1;

test.describe("history chart gaps", () => {
  test("an outage is drawn as a break, not a line across it", async ({ page }) => {
    // Readings from 20% to 45% and from 70% to the end; nothing between. The
    // value is constant, so a line joined across the gap would run straight
    // through the middle of it. 37, not a round figure: the value axis would
    // end at a round one and put the line on the plot's top edge.
    await serve(page, [{ name: "", gaps: [[0, at(0.2)], [at(0.45), at(0.7)]], v: 37 }]);
    const chart = await chartSwap(page);

    await drawn(chart, [0.1, 0.3, 0.8, 0.575], ([empty, before, after, gap]) => {
      expect(before).not.toEqual(empty);
      expect(after).not.toEqual(empty);
      expect(gap).toEqual(empty);
    });
  });

  test("a reading alone between two gaps is still drawn", async ({ page }) => {
    // Seen once in the middle of an outage. A line needs two points, so a
    // chart without markers would draw nothing here.
    await serve(page, [
      { name: "", gaps: [[0, at(0.2)], [at(0.3), at(0.5)], [at(0.5) + 1, at(0.8)]], v: 37 },
    ]);
    const chart = await chartSwap(page);

    await drawn(chart, [0.1, 0.25, 0.4, 0.5, 0.65], ([empty, run, gap1, lone, gap2]) => {
      expect(run).not.toEqual(empty);
      expect(gap1).toEqual(empty);
      expect(lone).not.toEqual(empty);
      expect(gap2).toEqual(empty);
    });
  });

  test("every line of a multi-line chart breaks at its own gaps", async ({ page }) => {
    // b starts first and stops first; a has a gap while b is drawn, and b has
    // one while a is. Each gap strip must look exactly like the stretch where
    // only the other line exists.
    await serve(page, [
      { name: "a", gaps: [[0, at(0.25)], [at(0.35), at(0.5)], [at(0.95), END]], v: 30 },
      { name: "b", gaps: [[0, at(0.1)], [at(0.6), at(0.75)], [at(0.9), END]], v: 70 },
    ]);
    const chart = await chartSwap(page);

    await drawn(chart, [0.18, 0.93, 0.42, 0.67], ([onlyB, onlyA, gapA, gapB]) => {
      expect(onlyA).not.toEqual(onlyB);
      expect(gapA).toEqual(onlyB);
      expect(gapB).toEqual(onlyA);
    });
  });

  test("hovering a gap reports nothing for it rather than failing", async ({ page }) => {
    // The axis tooltip snaps to the nearest entry, and in a gap that is the
    // break: a line with no value there, not a zero, and not a formatter
    // failing on a null.
    const errors: Error[] = [];
    page.on("pageerror", (e) => errors.push(e));
    await serve(page, [{ name: "", gaps: [[0, at(0.2)], [at(0.45), at(0.7)]], v: 37 }]);
    const chart = await chartSwap(page);

    await drawn(chart, [0.3], () => {});
    const plot = await readPlot(chart, []);
    const box = await chart.locator("canvas").first().boundingBox();
    if (box == null) throw new Error("history chart canvas has no box");
    const hover = (f: number) =>
      page.mouse.move(box.x + plot.left + f * (plot.right - plot.left), box.y + box.height / 2);

    // A reading first, so the tooltip is known to work here at all.
    await hover(0.3);
    await expect(page.getByText("37.0%")).toBeVisible();

    await hover(0.46);
    await expect(page.getByText("37.0%")).toBeHidden();
    await expect(page.getByText(/NaN/)).toHaveCount(0);
    expect(errors).toEqual([]);
  });

  test("a range with no usable step is refused, not drawn", async ({ page }) => {
    await serve(page, [{ name: "", gaps: [], v: 50 }], 0);
    await page.goto("/");
    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
    await page.getByLabel("Search history").fill("swap");
    await page.getByRole("option", { name: "Swap", exact: true }).click();

    await expect(results(page).getByText("history: the range has no usable step (0)")).toBeVisible();
    await expect(results(page).getByRole("img", { name: /History chart/ })).toHaveCount(0);
  });
});
