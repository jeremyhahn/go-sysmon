<script lang="ts">
  // A line chart over absolute time, for Prometheus-backed ranges.
  //
  // Separate from TimeSeriesChart, which draws the live stream: that one plots
  // sixty points against a fixed "-59s … 0s" category axis, which is exactly
  // right for a rolling window and cannot express a date. A history range has
  // real instants, gaps where a node was down, and a step the server chose, so
  // it needs a time axis and points carrying their own timestamps.

  import { onMount, onDestroy } from "svelte";
  import {
    chartPalette,
    init,
    tooltipContent,
    type ChartOption,
    type ChartPalette,
    type ECharts,
  } from "$lib/echarts";
  import { theme } from "$lib/theme.svelte";
  import { formatTick, formatValue } from "$lib/history";
  import type { HistorySeries, HistoryUnit } from "$lib/types";

  interface Props {
    series: HistorySeries[];
    unit: HistoryUnit;
    /** The requested window, used to pick a label resolution and to keep the
     * axis honest when the data does not span the whole range. */
    startMs: number;
    endMs: number;
    height?: string;
  }

  let { series, unit, startMs, endMs, height = "220px" }: Props = $props();

  let container: HTMLDivElement;
  let chart: ECharts | null = null;

  function buildOption(
    s: HistorySeries[],
    p: ChartPalette,
    from: number,
    to: number
  ): ChartOption {
    const windowMs = Math.max(1, to - from);
    return {
      darkMode: p.darkMode,
      backgroundColor: "transparent",
      animation: false,
      grid: { top: 8, right: 12, bottom: 24, left: 56 },
      tooltip: {
        trigger: "axis",
        backgroundColor: p.surface,
        borderColor: p.border,
        textStyle: { color: p.mutedText, fontSize: 11 },
        axisPointer: {
          lineStyle: { color: p.axisPointer },
          crossStyle: { color: p.axisPointer },
        },
        formatter: (params: unknown) => {
          const items = params as Array<{
            seriesName: string;
            value: [number, number];
            color: string;
          }>;
          if (items.length === 0) return "";
          const when = new Date(items[0].value[0]).toLocaleString();
          return tooltipContent(
            items.map((i) => ({
              color: i.color,
              name: i.seriesName,
              value: formatValue(i.value[1], unit),
            })),
            when
          );
        },
      },
      xAxis: {
        // A time axis, not a category one: series can have gaps where a node
        // was down, and on a category axis a gap silently shifts every later
        // point left -- drawing an outage as though it never happened.
        type: "time",
        min: from,
        max: to,
        axisLabel: {
          color: p.axisLabel,
          fontSize: 10,
          formatter: (value: number) => formatTick(value, windowMs),
        },
        axisLine: { lineStyle: { color: p.border } },
        splitLine: { show: false },
      },
      yAxis: {
        type: "value",
        axisLabel: {
          color: p.axisLabel,
          fontSize: 10,
          formatter: (v: number) => formatValue(v, unit),
        },
        axisLine: { show: false },
        splitLine: { lineStyle: { color: p.track } },
      },
      series: s.map((line, idx) => ({
        name: line.name || "value",
        type: "line",
        // connectNulls is deliberately left off: a break in the data is a
        // break in the line. Joining across it would draw a straight run
        // through an outage and read as a machine that was idle rather than
        // one that was gone.
        data: line.points.map((pt) => [pt.t, pt.v] as [number, number]),
        smooth: false,
        symbol: "none",
        lineStyle: { color: p.series[idx % p.series.length], width: 1.5 },
        itemStyle: { color: p.series[idx % p.series.length] },
      })),
    };
  }

  const palette = $derived(chartPalette(theme.resolved));

  onMount(() => {
    chart = init(container, null, { renderer: "canvas" });
    chart.setOption(buildOption(series, palette, startMs, endMs));

    const ro = new ResizeObserver(() => chart?.resize());
    ro.observe(container);
    return () => ro.disconnect();
  });

  // notMerge is true here, unlike the live chart: the number of lines changes
  // between one range and the next -- a disk that appeared, an interface that
  // went away -- and a merging update keeps a series the new data no longer
  // has, leaving a stale line on the chart with no source.
  $effect(() => {
    chart?.setOption(buildOption(series, palette, startMs, endMs), {
      notMerge: true,
    });
  });

  onDestroy(() => {
    chart?.dispose();
    chart = null;
  });
</script>

<div bind:this={container} style="width:100%;height:{height};"></div>
