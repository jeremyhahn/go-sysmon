import { test, expect, type Page } from "@playwright/test";
import { mockSnapshotFeed } from "./fixtures/mock-sse";
import { formatValue, unitLabel } from "../src/lib/history";
import { expectLine, gridPoints, leadInGap, stepFor, stepsIn } from "./fixtures/history";

/**
 * Charting a counter's history as a rate (#159).
 *
 * The swap paging totals are counters: pages moved since boot. Drawn raw, the
 * line only ever rises and a reboot is a cliff. The server queries them through
 * rate(), scales pages to bytes by each node's own page size, and says so with
 * a per-second unit; what is checked here is that the dashboard draws them in
 * that unit -- bytes per second, never a bare count, which would read as a
 * quantity rather than a speed.
 */

const CATALOG = {
  enabled: true,
  metrics: [
    { key: "swap", label: "Swap", unit: "percent", split_by: "" },
    { key: "swap_in", label: "Swap in", unit: "bytes_per_second", split_by: "" },
    { key: "swap_out", label: "Swap out", unit: "bytes_per_second", split_by: "" },
  ],
};

async function setup(page: Page, opts: { empty?: boolean } = {}) {
  const requests: URLSearchParams[] = [];
  await mockSnapshotFeed(page);
  await page.route(/\/api\/history\/metrics$/, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CATALOG) })
  );
  await page.route(/\/api\/history\?/, async (route) => {
    const params = new URL(route.request().url()).searchParams;
    requests.push(params);
    const start = Number(params.get("start"));
    const end = Number(params.get("end"));
    const metric = CATALOG.metrics.find((m) => m.key === params.get("metric"));
    const step = stepFor(end - start);
    const steps = stepsIn(start, end, step);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        enabled: true,
        node: "local",
        metric: metric?.key,
        label: metric?.label,
        unit: metric?.unit,
        start_ms: start,
        end_ms: end,
        step_ms: step,
        // Already a rate in bytes, as the server returns it: a burst of
        // paging that rises and falls away again, not a total that climbs.
        // Recording starts just after the window opens, which leaves the
        // chart an empty lead-in to compare its line against. A node that
        // does not export its page size answers with no series at all.
        series: opts.empty
          ? []
          : [
              {
                name: "",
                points: gridPoints({
                  startMs: start,
                  endMs: end,
                  stepMs: step,
                  value: (k) => 16_384 + 5_120_000 * Math.sin((Math.PI * k) / steps),
                  gaps: [leadInGap(steps)],
                }),
              },
            ],
      }),
    });
  });
  return { requests };
}

const results = (page: Page) => page.getByRole("region", { name: "History search results" });

async function chartSwapOut(page: Page) {
  await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
  await page.getByLabel("Search history").fill("swap out");
  await page.getByRole("option", { name: "Swap out" }).click();
}

test.describe("counter history", () => {
  test("swap out is charted in bytes per second", async ({ page }) => {
    const { requests } = await setup(page);
    await page.goto("/");
    await chartSwapOut(page);

    await expect(results(page).getByText("Swap out")).toBeVisible();
    const chart = results(page).getByRole("img", { name: "History chart in bytes per second" });
    await expect(chart).toBeVisible();
    await expect(chart.locator("canvas").first()).toBeVisible();
    await expectLine(chart);

    await expect.poll(() => requests.some((r) => r.get("metric") === "swap_out")).toBe(true);
  });

  test("a gauge beside it keeps its own unit", async ({ page }) => {
    // The rate unit belongs to the counter entries alone; the swap occupancy
    // chart is still a percentage.
    await setup(page);
    await page.goto("/");

    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
    await page.getByLabel("Search history").fill("swap");
    await page.getByRole("option", { name: "Swap", exact: true }).click();

    await expect(results(page).getByRole("img", { name: "History chart in percent" })).toBeVisible();
    await expect(
      results(page).getByRole("img", { name: "History chart in bytes per second" })
    ).toHaveCount(0);
  });

  test("a node without a page size draws no chart rather than a wrong one", async ({ page }) => {
    // An older peer exports no page size, so the server's scaled rate matches
    // nothing and comes back empty. That must read as nothing recorded -- not
    // as a flat line of zero paging.
    await setup(page, { empty: true });
    await page.goto("/");
    await chartSwapOut(page);

    await expect(
      results(page).getByText("Nothing recorded for this in the selected range.")
    ).toBeVisible();
    await expect(results(page).getByRole("img", { name: /History chart/ })).toHaveCount(0);
  });
});

test.describe("rate formatting", () => {
  test("a swap rate reads as bytes per second, scaled", () => {
    expect(formatValue(16_384, "bytes_per_second")).toBe("16.0 KB/s");
    expect(formatValue(5_120_000, "bytes_per_second")).toBe("4.9 MB/s");
    expect(unitLabel("bytes_per_second")).toBe("bytes per second");
    expect(unitLabel("count")).toBe("");
  });
});
