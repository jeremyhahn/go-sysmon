import { test, expect, type Page } from "@playwright/test";
import { mockSnapshotFeed } from "./fixtures/mock-sse";
import snapshot from "./fixtures/snapshot.json" with { type: "json" };
import { expectLine, historyBody } from "./fixtures/history";

/**
 * Searching the recorded history, end to end through the dashboard.
 *
 * The owner asked for "the ability to search across historical data points".
 * The matcher is unit-tested in history-search.spec.ts; what is checked here is
 * that typing a resource name gets that resource charted for the selected
 * range, and that the states which are NOT a chart each say what they are --
 * no range, nothing recorded, nothing matching. An empty box where a chart
 * should be is the failure this feature could most easily ship with.
 */

const CATALOG = {
  enabled: true,
  metrics: [
    { key: "cpu", label: "CPU", unit: "percent", split_by: "" },
    { key: "disk_read", label: "Disk read", unit: "bytes_per_second", split_by: "device" },
    { key: "net_rx", label: "Network in", unit: "bytes_per_second", split_by: "interface" },
  ],
};

/** The device names in the fixture snapshot, which is what the search offers. */
const devices: string[] = (snapshot.disks ?? []).map((d: { name: string }) => d.name);

async function setup(page: Page, opts: { series?: string[] } = {}) {
  const requests: URLSearchParams[] = [];
  await mockSnapshotFeed(page);
  await page.route(/\/api\/history\/metrics$/, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CATALOG) })
  );
  await page.route(/\/api\/history\?/, async (route) => {
    const params = new URL(route.request().url()).searchParams;
    requests.push(params);
    const names = opts.series ?? devices;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(historyBody(params, { names })),
    });
  });
  return { requests };
}

const box = (page: Page) => page.getByLabel("Search history");
const results = (page: Page) => page.getByRole("region", { name: "History search results" });

test.describe("history search", () => {
  test("a device name charts that device over the selected range", async ({ page }) => {
    const { requests } = await setup(page);
    await page.goto("/");

    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
    await box(page).fill(devices[0]);

    const option = page.getByRole("option", { name: "Disk read \u2014 " + devices[0] });
    await expect(option).toBeVisible();
    await option.click();

    await expect(results(page).getByText("Disk read \u2014 " + devices[0])).toBeVisible();
    // The chart is drawn, not merely announced: a line across the range.
    const chart = results(page).getByRole("img", { name: /History chart/ }).first();
    await expect(chart.locator("canvas").first()).toBeVisible();
    await expectLine(chart);

    // And it asked the server for the metric the hit named.
    await expect.poll(() => requests.some((r) => r.get("metric") === "disk_read")).toBe(true);
  });

  test("with no range selected it says so instead of drawing an empty box", async ({ page }) => {
    await setup(page);
    await page.goto("/");

    await box(page).fill(devices[0]);
    await page.getByRole("option", { name: "Disk read \u2014 " + devices[0] }).click();

    await expect(results(page).getByText("Pick a date range to chart these.")).toBeVisible();
  });

  test("a resource with nothing recorded in the window says that, not an error", async ({ page }) => {
    // The metric answers; this device is simply not in it. That is a different
    // fact from a failed query and reads differently to an operator.
    await setup(page, { series: ["some-other-device"] });
    await page.goto("/");

    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
    await box(page).fill(devices[0]);
    await page.getByRole("option", { name: "Disk read \u2014 " + devices[0] }).click();

    await expect(
      results(page).getByText("Nothing recorded for this in the selected range.")
    ).toBeVisible();
  });

  test("a query matching nothing says so", async ({ page }) => {
    await setup(page);
    await page.goto("/");

    await box(page).fill("kernel entropy");
    await expect(page.getByText("Nothing recorded matches that.")).toBeVisible();
  });

  test("a pinned chart can be removed", async ({ page }) => {
    await setup(page);
    await page.goto("/");

    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
    await box(page).fill(devices[0]);
    await page.getByRole("option", { name: "Disk read \u2014 " + devices[0] }).click();

    const title = results(page).getByText("Disk read \u2014 " + devices[0]);
    await expect(title).toBeVisible();

    await page.getByRole("button", { name: "Remove Disk read \u2014 " + devices[0] }).click();
    await expect(title).toHaveCount(0);
  });

  test("a node with no history has no search box at all", async ({ page }) => {
    // Not a disabled input: there is nothing to search, and a control that
    // always answers "nothing" is worse than no control.
    await mockSnapshotFeed(page);
    await page.route(/\/api\/history\/metrics$/, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ enabled: false, metrics: null }),
      })
    );
    await page.goto("/");

    await expect(box(page)).toHaveCount(0);
  });
});
