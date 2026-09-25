import { test, expect, type Page } from "@playwright/test";
import { mockSnapshotFeed } from "./fixtures/mock-sse";
import { historyBody } from "./fixtures/history";
import { openTab } from "./fixtures/tabs";

/**
 * Cores on a two-socket host, on every surface of the dashboard that names
 * one. Both packages have a Core 0; named by the number alone the two shared
 * one legend entry, one history line and one search result, and a reader
 * could not tell which socket was hot. A single-package host is unchanged.
 */

const DUAL = [
  { package_id: 0, core_id: 0, label: "Core 0", temp_celsius: 50, high_celsius: 85, crit_celsius: 100 },
  { package_id: 1, core_id: 0, label: "Core 0", temp_celsius: 60, high_celsius: 85, crit_celsius: 100 },
];

const CATALOG = {
  enabled: true,
  metrics: [{ key: "cpu_temperature", label: "CPU temperature", unit: "celsius", split_by: "core" }],
};

/** chartLines reads the names a chart gives its lines. */
function chartLines(scope: ReturnType<Page["locator"]>) {
  return scope.getByRole("list", { name: "Chart lines" }).first().getByRole("listitem");
}

async function serveHistory(page: Page, names: string[]) {
  await page.route(/\/api\/history\/metrics$/, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CATALOG) })
  );
  await page.route(/\/api\/history\?/, async (route) => {
    const params = new URL(route.request().url()).searchParams;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(historyBody(params, { names, unit: "celsius" })),
    });
  });
}

const temperatures = (page: Page) =>
  page.locator(".card", { has: page.getByText("Temperature History", { exact: false }) }).last();

test.describe("core labels", () => {
  test("a two-socket host names each core by its package, live", async ({ page }) => {
    await mockSnapshotFeed(page, { coreTemps: DUAL });
    await page.goto("/");
    await openTab(page, "CPU");

    await expect(chartLines(temperatures(page))).toHaveText(["P0 · Core 0", "P1 · Core 0"]);
  });

  test("a single-package host keeps plain core names", async ({ page }) => {
    await mockSnapshotFeed(page);
    await page.goto("/");
    await openTab(page, "CPU");

    await expect(chartLines(temperatures(page))).toHaveText(["Core 0", "Core 4"]);
  });

  test("the history chart draws one line per package and core, named as live", async ({ page }) => {
    await mockSnapshotFeed(page, { coreTemps: DUAL });
    await serveHistory(page, ["0:0", "1:0"]);
    await page.goto("/");
    await openTab(page, "CPU");

    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });

    const chart = temperatures(page);
    await expect(chart.getByRole("img", { name: /History chart/ })).toBeVisible();
    await expect(chartLines(chart)).toHaveText(["P0 · Core 0", "P1 · Core 0"]);
  });

  test("history search finds a core by its package, and titles it so", async ({ page }) => {
    await mockSnapshotFeed(page, { coreTemps: DUAL });
    await serveHistory(page, ["0:0", "1:0"]);
    await page.goto("/");

    await page.getByLabel("Date range").selectOption({ label: "Last 1 hour" });
    await page.getByLabel("Search history").fill("P1");

    await expect(page.getByRole("option", { name: "CPU temperature — P1 · Core 0" })).toBeVisible();
    await expect(page.getByRole("option", { name: "CPU temperature — P0 · Core 0" })).toHaveCount(0);
  });
});
