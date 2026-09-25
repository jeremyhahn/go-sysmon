<script lang="ts">
  /**
   * A chart that follows the dashboard's range control.
   *
   * With no range selected it is the live stream, unchanged: the same
   * TimeSeriesChart, the same sixty rolling points, the same cost. With a range
   * selected it draws that window from Prometheus instead.
   *
   * The mode lives in one store, not in this component and not in the panels.
   * A panel says only what its chart IS -- which catalogued metrics its series
   * correspond to -- and never which mode it is in. That is what keeps eight
   * panels comparable with each other, and keeps the real-time path free of
   * state it does not need.
   *
   * A chart with no historical counterpart says so rather than carrying on
   * streaming beside charts showing last week. A live line under a historical
   * heading is the same lie as a frozen peer reading, and it would be
   * self-inflicted.
   */

  import TimeSeriesChart, { type SeriesConfig } from "./TimeSeriesChart.svelte";
  import HistoryChart from "./HistoryChart.svelte";
  import { history } from "$stores/history.svelte";
  import type { HistoryLine } from "$lib/history";
  import { coreLabelForKey, multiPackageKeys } from "$lib/cores";
  import type { HistoryUnit } from "$lib/types";

  interface Props {
    /** The live series, drawn when no range is selected. */
    series: SeriesConfig[];
    /**
     * The catalog keys the series correspond to, by position. A null entry, or
     * an omitted prop, means that series has no recorded history.
     */
    metrics?: (string | null)[];
    /**
     * For a metric split by a label -- device, interface, gpu -- the value this
     * chart shows. Prometheus returns every device's series, and this is what
     * selects the one the panel is drawing.
     */
    splitValue?: string;
    height?: string;
    showXAxis?: boolean;
    smooth?: boolean;
    areaStyle?: boolean;
  }

  let {
    series,
    metrics = [],
    splitValue,
    height = "160px",
    showXAxis = false,
    smooth = true,
    areaStyle = true,
  }: Props = $props();

  const keys = $derived(metrics.filter((m): m is string => m != null && m !== ""));
  const historical = $derived(history.active);

  /**
   * Whether this chart can be drawn from history at all.
   *
   * Both halves matter. A chart may name no metric -- per-core CPU usage and
   * GPU link throughput are collected but never exported as series. And a
   * metric this build knows may be absent from the catalog the SERVER
   * published, which is what an older node looks like. Asking for one of those
   * gets a 400, and a chart that had asked would wait on it for ever: a
   * spinner that reads as a slow Prometheus and is really a request that is
   * never coming back.
   */
  const drawable = $derived(
    keys.length > 0 && keys.every((key) => history.metric(key) != null)
  );

  // Declaring the need rather than fetching here: several charts share a
  // metric (nine disks all want disk_read) and the store issues one request
  // for each rather than one per chart.
  // need() reads the range, so this effect re-runs whenever the range object is
  // replaced -- which the store does on every reset, including a node change
  // that keeps the same window. That is what makes a chart ask again after its
  // data was discarded, rather than sitting on "Loading…" for ever.
  $effect(() => {
    if (!historical || !drawable) return;
    for (const key of keys) history.need(key);
  });

  // Settled, not loaded: a request that failed has finished, and waiting on it
  // is a spinner that never ends.
  const settledAll = $derived(historical && keys.every((k) => history.settled(k)));
  const anyFailed = $derived(historical && keys.some((k) => history.failed(k)));

  /** The window actually loaded, taken from the response rather than from the
   * request: the server may have widened or resolved it. */
  const window = $derived.by(() => {
    for (const key of keys) {
      const loaded = history.get(key);
      if (loaded != null) return { startMs: loaded.start_ms, endMs: loaded.end_ms };
    }
    const range = history.range;
    return { startMs: range?.startMs ?? 0, endMs: range?.endMs ?? 0 };
  });

  const unit = $derived<HistoryUnit>(
    (keys.map((k) => history.get(k)?.unit).find((u) => u != null) as HistoryUnit) ??
      "percent"
  );

  /**
   * The historical lines.
   *
   * Each catalogued metric contributes the series matching this chart's split
   * value, labelled from the live series in the same position so a chart reads
   * the same in both modes.
   */
  const historyByMetric = $derived.by<HistoryLine[][]>(() => {
    const perMetric: HistoryLine[][] = [];
    metrics.forEach((key, index) => {
      if (key == null || key === "") return;
      const out: HistoryLine[] = [];
      perMetric.push(out);
      const loaded = history.get(key);
      if (loaded == null) return;

      const label = series[index]?.label ?? loaded.label;
      const matching =
        splitValue == null || splitValue === ""
          ? loaded.series
          : loaded.series.filter((s) => s.name === splitValue);

      // Naming: a chart drawing one line takes the label the live chart used,
      // so it reads the same in both modes. A chart drawing every element of a
      // split metric -- all the cores, all the packages -- names each line by
      // the live series of the same history name when there is one, and
      // otherwise by what the split is, which is more use in a legend than a
      // bare "0".
      const splitBy = history.metric(key)?.split_by ?? "";
      const multi = splitBy === "core" && multiPackageKeys(matching.map((s) => s.name));
      for (const s of matching) {
        const live = series.find((c) => c.historyName === s.name);
        out.push({
          name:
            matching.length === 1
              ? label
              : live != null
                ? live.label
                : splitBy === "core"
                  ? coreLabelForKey(s.name, multi)
                  : splitBy === ""
                    ? s.name
                    : `${splitBy} ${s.name}`,
          points: s.points,
          stepMs: loaded.step_ms,
        });
      }
    });
    return perMetric;
  });

  const historySeries = $derived(historyByMetric.flat());

  const hasPoints = $derived(historySeries.some((s) => s.points.length > 0));

  /**
   * Every named metric contributed a line, not merely every request finished.
   *
   * `settledAll` is about the REQUESTS; this is about the RESULT. A chart built
   * from disk_read and disk_write whose write metric returns no series for this
   * device in this window would otherwise draw the read line alone, under a
   * heading that still says both -- the comment below has always claimed "every
   * line or none" and only the first half was enforced.
   *
   * A window with nothing in it at all is a different thing and keeps its own
   * message: that is "no history here", not "half a chart".
   */
  const everyMetricDrew = $derived(
    historyByMetric.length > 0 &&
      historyByMetric.every((lines) => lines.some((l) => l.points.length > 0))
  );
</script>

{#if !historical}
  <TimeSeriesChart {series} {height} {showXAxis} {smooth} {areaStyle} />
{:else if !drawable}
  <div
    class="flex items-center justify-center text-xs text-base-content/60 text-center px-4"
    style="height:{height};"
  >
    This chart has no recorded history. Return to live to see it.
  </div>
{:else if !settledAll}
  <!-- Every line or none. A chart drawing read but not write, because one
       request came back first, is a chart whose contents are not what its
       heading says -- and if the other never arrives it stays that way. -->
  <div
    class="flex items-center justify-center text-xs text-base-content/60"
    style="height:{height};"
  >
    Loading…
  </div>
{:else if anyFailed}
  <div
    class="flex items-center justify-center text-xs text-base-content/60 text-center px-4"
    style="height:{height};"
  >
    History could not be read for this chart.
  </div>
{:else if hasPoints && !everyMetricDrew}
  <!-- Some lines and not others. Drawing what arrived would be a chart whose
       contents are not what its heading says, and nothing on the page would
       hint that a line is missing rather than flat. -->
  <div
    class="flex items-center justify-center text-xs text-base-content/60 text-center px-4"
    style="height:{height};"
  >
    Only part of this chart has history for this range.
  </div>
{:else if hasPoints}
  <HistoryChart
    series={historySeries}
    {unit}
    startMs={window.startMs}
    endMs={window.endMs}
    {height}
  />
{:else}
  <!-- An empty range is a real answer: the node may not have been running, or
       may not have been pushing yet. It is not an error and is not dressed
       as one. -->
  <div
    class="flex items-center justify-center text-xs text-base-content/60 text-center px-4"
    style="height:{height};"
  >
    No data recorded for this range.
  </div>
{/if}
