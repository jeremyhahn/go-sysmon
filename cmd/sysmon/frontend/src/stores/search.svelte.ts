// The history search: what was typed, what matches, and what is pinned.
//
// Separate from the history store because the two answer different questions.
// That one holds the window and the series every panel needs; this one holds a
// question the user asked once, and its answers, which nothing else on the page
// depends on. Merging them would put a text box's state on the path every chart
// re-reads.

import { searchHistory, hitKey, type ResourceLabels, type Resources, type SearchHit } from "$lib/search";
import { coreKey, coreLabel, multiPackage } from "$lib/cores";
import { history } from "$stores/history.svelte";
import { monitor } from "$stores/monitor.svelte";

/** The most series charted at once. A pinned result costs one range request
 * per node per window, so this is a bound on what one search can ask of
 * Prometheus, not a layout preference. */
export const MAX_PINNED = 6;

class SearchStore {
  #query = $state("");
  #pinned = $state<SearchHit[]>([]);

  get query(): string {
    return this.#query;
  }

  set query(value: string) {
    this.#query = value;
  }

  /**
   * The resources this node has, taken from the live snapshot.
   *
   * The snapshot is the machine in front of you; Prometheus knows what this
   * node HAD, which is a different list after a disk is pulled. Searching the
   * live one means a result always corresponds to something present, and a
   * drive that has gone is absent from the search rather than offering a chart
   * that ends three days ago with no explanation.
   */
  get resources(): Resources {
    const snap = monitor.snapshot;
    if (snap == null) return {};
    return {
      device: (snap.disks ?? []).map((d) => d.name),
      interface: (snap.networks ?? []).map((n) => n.name),
      gpu: (snap.gpus ?? []).map((g) => String(g.index)),
      // A core's history is named by its package and core, not by the
      // logical CPU index, which is a different number on most hosts.
      core: (snap.sensors?.core_temps ?? []).map((t) => coreKey(t.package_id, t.core_id)),
    };
  }

  /** The names of the resources whose value is not what a person calls them. */
  get labels(): ResourceLabels {
    const temps = monitor.snapshot?.sensors?.core_temps ?? [];
    const multi = multiPackage(temps);
    return {
      core: Object.fromEntries(temps.map((t) => [coreKey(t.package_id, t.core_id), coreLabel(t, multi)])),
    };
  }

  /** The matches for the current query, best first. */
  get hits(): SearchHit[] {
    return searchHistory(this.#query, history.metrics, this.resources, this.labels);
  }

  /** The series the user chose to chart. */
  get pinned(): SearchHit[] {
    return this.#pinned;
  }

  /** available reports whether searching is possible at all: a node with no
   * Prometheus has nothing to search, and the control is not rendered. */
  get available(): boolean {
    return history.available;
  }

  /** pin adds a result to the charted set, ignoring one already there and
   * refusing to grow past MAX_PINNED. */
  pin(hit: SearchHit): void {
    const key = hitKey(hit);
    if (this.#pinned.some((p) => hitKey(p) === key)) return;
    if (this.#pinned.length >= MAX_PINNED) return;
    this.#pinned = [...this.#pinned, hit];
  }

  /** unpin removes one result. */
  unpin(hit: SearchHit): void {
    const key = hitKey(hit);
    this.#pinned = this.#pinned.filter((p) => hitKey(p) !== key);
  }

  /** clear drops the query and every pinned result, which is what leaving the
   * search means -- pinned charts outliving the search that produced them
   * would be a second, unnamed dashboard. */
  clear(): void {
    this.#query = "";
    this.#pinned = [];
  }

  /** pinnedFull reports whether the pin limit has been reached, so the UI can
   * say why a result will not pin rather than ignoring the click. */
  get pinnedFull(): boolean {
    return this.#pinned.length >= MAX_PINNED;
  }
}

export const search = new SearchStore();
