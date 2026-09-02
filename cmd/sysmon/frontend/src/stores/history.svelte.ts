// The date range, and the history it selects.
//
// There is one of these for the whole dashboard, and that is the design. The
// owner asked to "see the graphs/charts with date range searches ... in addition
// to the real-time streams (when a date range is not selected)": one surface
// with two states, chosen by a control whose absence gives the live stream.
// Not a separate destination, and not a mode carried by each panel -- eight
// panels each holding their own range would not be comparable with each other
// and would put state into the real-time path that it does not need.
//
// So: this store holds the range, every chart reads it, and clearing it puts
// the whole dashboard back on the live stream without a reload.

import {
  RANGE_PRESETS,
  fetchCatalog,
  fetchHistory,
  fromLocalInput,
  toLocalInput,
  HistoryError,
} from "$lib/history";
import type { History, HistoryMetric } from "$lib/types";

/** LIVE is the picker's value when no range is selected. */
export const LIVE = "live";

/** CUSTOM is the picker's value for an explicit start and end. */
export const CUSTOM = "custom";

export interface Range {
  startMs: number;
  endMs: number;
}

class HistoryStore {
  // The catalog decides whether the picker exists at all. A node with no
  // prometheus.query_url reports itself disabled, and the dashboard is exactly
  // what it was before any of this existed.
  #available = $state(false);
  #metrics = $state<HistoryMetric[]>([]);

  #selection = $state<string>(LIVE);
  #customStart = $state("");
  #customEnd = $state("");
  #range = $state<Range | null>(null);

  /** The node being shown, so drilling into a peer draws that peer's past. */
  #node = $state<string | null>(null);

  #loaded = $state<Record<string, History>>({});
  // Metrics whose request finished without producing data. Kept apart from
  // #loaded so a chart can tell "still waiting" from "asked and got nothing",
  // which is the difference between a spinner that will end and one that will
  // not.
  #failed = $state<Record<string, true>>({});
  #inFlight = $state(0);
  #error = $state("");

  // `requested` stops a chart asking for the same metric on every render, and
  // is not $state: nothing renders from it.
  #requested = new Set<string>();
  /** The requests in flight for the current generation, so a range or node
   * change can abort them rather than merely ignore what they return. */
  #pending = new Set<AbortController>();

  // generation makes a response that arrives after the window moved
  // recognisable as stale. Nothing renders from it, so it is not $state.
  #generation = 0;

  /** available reports whether this node can answer a range at all. */
  get available(): boolean {
    return this.#available;
  }

  /** metrics is the catalog, for a caller that wants to know what is drawable. */
  get metrics(): HistoryMetric[] {
    return this.#metrics;
  }

  /** active reports whether a range is selected. When false, everything is
   * live and nothing here is consulted. */
  get active(): boolean {
    return this.#range != null;
  }

  get range(): Range | null {
    return this.#range;
  }

  get selection(): string {
    return this.#selection;
  }

  /**
   * windowLabel is what a chart heading should say it is showing.
   *
   * Every converted chart is headed with the window it draws -- "CPU Usage
   * (60s)" -- and those headings were written when 60 seconds was the only
   * answer. With a range applied they became false: a thirty-day series under
   * a heading claiming a minute, which is a worse reading of the data than no
   * heading at all, because it is specific and wrong.
   *
   * It lives here rather than in each panel so the sixteen headings cannot
   * drift from each other or from what is applied.
   */
  get windowLabel(): string {
    if (this.#range == null) return "60s";
    const preset = RANGE_PRESETS.find((p) => p.key === this.#selection);
    if (preset != null) return preset.label;
    // An explicit window. The dates are too long for a heading and are already
    // on screen in the picker and the banner, so this says which kind of
    // window it is and leaves the numbers where they are.
    return "custom range";
  }

  get customStart(): string {
    return this.#customStart;
  }

  get customEnd(): string {
    return this.#customEnd;
  }

  get loading(): boolean {
    return this.#inFlight > 0;
  }

  get error(): string {
    return this.#error;
  }

  /** loadCatalog asks the node what it can draw. Called once, on load. */
  async loadCatalog(): Promise<void> {
    try {
      const catalog = await fetchCatalog();
      this.#available = catalog.enabled && (catalog.metrics?.length ?? 0) > 0;
      this.#metrics = catalog.metrics ?? [];
    } catch {
      // An older node, or one that cannot answer: no picker. The live
      // dashboard is unaffected, which is why this is a separate request from
      // the event stream rather than something carried on it.
      this.#available = false;
      this.#metrics = [];
    }
  }

  /** setNode follows the peer being viewed. Changing it discards what was
   * loaded: it describes a different machine. */
  setNode(name: string | null): void {
    if (this.#node === name) return;
    this.#node = name;
    this.#reset();
  }

  /** selectPreset moves to a relative window ending now, or back to live. */
  selectPreset(key: string): void {
    this.#selection = key;
    if (key === LIVE) {
      this.#range = null;
      this.#reset();
      return;
    }
    if (key === CUSTOM) {
      this.beginCustom();
      return;
    }
    const preset = RANGE_PRESETS.find((p) => p.key === key);
    if (preset == null) return;
    const endMs = Date.now();
    this.#setRange({ startMs: endMs - preset.ms, endMs });
  }

  /**
   * beginCustom switches to an explicit window without changing what is on
   * screen.
   *
   * The invariant it exists to keep: **whenever a range is applied, the inputs
   * describe that range.** Choosing "Custom…" while looking at "Last 1 hour"
   * used to leave the hour applied and the inputs showing something else, so
   * the control said one thing, the banner said a second and the charts drew a
   * third. That is the same defect as seeding-and-applying, from the other
   * direction: there, the dashboard moved without being asked; here, it
   * refused to move while claiming it had.
   *
   * So an applied range is carried into the inputs and stays applied -- nothing
   * moves, and the two now agree. With nothing applied, the inputs open on the
   * last hour and the dashboard stays live: choosing "Custom…" is starting to
   * specify a window, not asking for one nobody has typed yet.
   */
  beginCustom(): void {
    this.#selection = CUSTOM;
    if (this.#range != null) {
      this.#customStart = toLocalInput(this.#range.startMs);
      this.#customEnd = toLocalInput(this.#range.endMs);
      return;
    }
    const now = Date.now();
    this.#customStart = toLocalInput(now - 3600_000);
    this.#customEnd = toLocalInput(now);
  }

  /** setCustom records an edited endpoint and applies it once both are
   * complete. They are incomplete on every keystroke while a date is typed, so
   * an incomplete pair is held rather than rejected. */
  setCustom(startInput: string, endInput: string): void {
    this.#customStart = startInput;
    this.#customEnd = endInput;
    this.#selection = CUSTOM;
    this.#applyCustom();
  }

  /** customComplete reports whether the typed pair is a usable window. */
  get customComplete(): boolean {
    const start = fromLocalInput(this.#customStart);
    const end = fromLocalInput(this.#customEnd);
    return start != null && end != null && end > start;
  }

  /**
   * refresh re-reads the current window.
   *
   * A relative preset goes stale while it is being looked at -- "the last
   * hour" means something different a minute later -- so it is resolved
   * against the clock again. An explicit window does not move, so that is a
   * plain re-read of the same range, which is still worth having: it is how an
   * operator picks up samples that arrived after they chose the window.
   */
  refresh(): void {
    if (this.#range == null) return;
    if (this.#selection !== CUSTOM) {
      this.selectPreset(this.#selection);
      return;
    }
    this.#reset();
  }

  /** need declares that a chart wants this metric for the current range.
   *
   * Called from an effect, and safe to call on every render: a metric already
   * asked for in this generation is ignored. Several charts share a metric --
   * nine disks all want disk_read -- so the requests are collected and issued
   * once each rather than once per chart. */
  need(metric: string): void {
    if (!this.active || this.#requested.has(metric)) return;
    this.#requested.add(metric);
    this.#schedule(metric);
  }

  /** metric returns a catalog entry, for a caller that needs to know how a
   * metric's series are split. */
  metric(key: string): HistoryMetric | null {
    return this.#metrics.find((m) => m.key === key) ?? null;
  }

  /** get returns what was loaded for a metric, or null while it is in flight
   * or if it was never asked for. */
  get(metric: string): History | null {
    return this.#loaded[metric] ?? null;
  }

  /**
   * settled reports whether this metric's request has finished, successfully or
   * not.
   *
   * A chart needs the distinction. Waiting on a request that already failed is
   * a spinner that never ends, which reads as a slow Prometheus rather than as
   * the error it is -- and a chart that gave up should say so.
   */
  settled(metric: string): boolean {
    return this.#loaded[metric] != null || this.#failed[metric] === true;
  }

  /** failed reports whether this metric's request finished without data. */
  failed(metric: string): boolean {
    return this.#failed[metric] === true;
  }

  #applyCustom(): void {
    const start = fromLocalInput(this.#customStart);
    const end = fromLocalInput(this.#customEnd);
    if (start == null || end == null || end <= start) {
      // Not a window, so nothing is applied -- and that includes whatever was
      // applied before this edit.
      //
      // Entering historical mode on half a date would draw a window the user
      // did not ask for; LEAVING one applied after the edit that invalidated it
      // is the same fault seen from the other side, and the worse of the two.
      // Clearing the start of an applied 24-hour window used to keep the
      // charts and the banner on those 24 hours while the inputs described
      // something else and the picker warned the pair was incomplete -- an
      // operator reading a window that no control on the page agreed with. The
      // picker's own warning says the charts are live; this is what makes that
      // true.
      if (this.#range != null) {
        this.#range = null;
        this.#reset();
      }
      return;
    }
    this.#setRange({ startMs: start, endMs: end });
  }

  #setRange(range: Range): void {
    this.#range = range;
    this.#reset();
  }

  /**
   * reset discards everything loaded, because it stopped describing what is on
   * screen: the window moved, or the node did.
   *
   * The range object is replaced rather than mutated, and that is the single
   * mechanism that makes charts ask again. A chart's effect calls need(), which
   * reads the range, so replacing it re-runs every chart's effect. Without the
   * replacement a node change would clear the data and nothing would fetch
   * more, leaving every chart on "Loading…" for as long as the tab was open --
   * and it would look exactly like a slow Prometheus.
   */
  #reset(): void {
    // Before anything else: these describe a window or a node that is no
    // longer on screen, and leaving them running costs a Prometheus query and
    // a connection slot each.
    for (const controller of this.#pending) controller.abort();
    this.#pending.clear();

    this.#generation++;
    this.#requested.clear();
    this.#loaded = {};
    this.#failed = {};
    this.#error = "";
    this.#inFlight = 0;
    if (this.#range != null) this.#range = { ...this.#range };
  }

  #schedule(metric: string): void {
    const generation = this.#generation;
    const range = this.#range;
    const node = this.#node;
    if (range == null) return;

    // Abortable, and the abort is what matters rather than the ignoring.
    //
    // A stale response was already discarded by generation, so nothing wrong
    // was ever rendered -- but the request itself carried on: the server kept
    // running an unauthenticated Prometheus range query for a window nobody is
    // looking at, and the browser's per-origin connection limit stayed
    // occupied by it. Change the range twice on a panel with nine charts and
    // the requests for the window actually on screen queue behind eighteen
    // that have been abandoned, so the charts sit on "Loading" for a reason
    // that has nothing to do with Prometheus being slow.
    //
    // The History view this replaced aborted them; the store did not, and
    // fetchHistory has taken a signal all along.
    const controller = new AbortController();
    this.#pending.add(controller);

    this.#inFlight++;
    void fetchHistory(
      { metric, node, startMs: range.startMs, endMs: range.endMs },
      controller.signal
    )
      .then((history) => {
        if (generation !== this.#generation) return; // the range moved
        this.#loaded = { ...this.#loaded, [metric]: history };
      })
      .catch((e: unknown) => {
        // An abort needs no case of its own here, and adding one was wrong:
        // the only thing that aborts these is #reset, which bumps the
        // generation before it does, so the check above has already returned
        // by the time the rejection arrives. A guard that cannot be reached
        // reads as a hazard someone considered, and this one would have said
        // an abort might otherwise be reported as Prometheus failing.
        if (generation !== this.#generation) return;
        this.#failed = { ...this.#failed, [metric]: true };
        // One message for the view, whichever metric failed: they share a
        // Prometheus, so they fail together and for the same reason.
        this.#error =
          e instanceof HistoryError
            ? e.message
            : e instanceof Error
              ? e.message
              : "history is unavailable";
      })
      .finally(() => {
        this.#pending.delete(controller);
        if (generation === this.#generation) this.#inFlight--;
      });
  }
}

export const history = new HistoryStore();
