import { expect, type Locator, type Page } from "@playwright/test";

/**
 * The dashboard's tab labels, exactly as the tab bar prints them.
 *
 * A closed set rather than a string, so a spec cannot ask for a tab that does
 * not exist and then fail on a timeout that reads like a rendering problem.
 */
export type TabLabel =
  | "Fleet"
  | "Overview"
  | "Host"
  | "CPU"
  | "GPU"
  | "NPU"
  | "Memory"
  | "Storage"
  | "Network"
  | "Virtualization"
  | "Containers";

/**
 * The tab id each label stands for. Tab ids double as alert categories, which
 * is what lets a spec fire an alert that decorates a particular tab.
 */
export const TAB_IDS: Record<TabLabel, string> = {
  Fleet: "fleet",
  Overview: "overview",
  Host: "host",
  CPU: "cpu",
  GPU: "gpu",
  NPU: "npu",
  Memory: "memory",
  Storage: "storage",
  Network: "network",
  Virtualization: "virt",
  Containers: "containers",
};

/**
 * The tab-bar button for `label`, and nothing else on the page.
 *
 * `getByRole('button', { name: 'Storage' })` is a case-insensitive substring
 * match: with the navbar's alert list open, every alert row naming the storage
 * category satisfies it as well, and a click is then refused as ambiguous or
 * lands on the wrong element. An exact name alone is not enough either, since
 * any other button that happens to be called "Memory" would satisfy it. So the
 * name is exact AND the element is one of the tab bar's own buttons.
 *
 * The alert dot a tab carries is aria-hidden, so it does not change the name.
 */
export function dashboardTab(page: Page, label: TabLabel): Locator {
  return page
    .getByRole("button", { name: label, exact: true })
    .and(page.locator("button.tab"));
}

/**
 * Opens the tab named `label` and waits until the tab bar says it is open.
 *
 * Waiting on the active class, rather than on the panel's content, keeps this
 * helper free of knowledge about any one panel: the caller asserts what the
 * panel shows. The GPU and NPU tabs exist only when the snapshot reports that
 * hardware, so a spec opening them must serve a snapshot that has it.
 */
export async function openTab(page: Page, label: TabLabel): Promise<void> {
  const tab = dashboardTab(page, label);
  await tab.click();
  await expect(tab).toHaveClass(/tab-active/);
}
