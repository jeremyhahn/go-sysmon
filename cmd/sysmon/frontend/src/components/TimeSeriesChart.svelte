<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import {
    chartPalette,
    init,
    graphic,
    type ChartOption,
    type ChartPalette,
    type ECharts,
  } from "$lib/echarts";
  import { theme } from "$lib/theme.svelte";

  export interface SeriesConfig {
    label: string;
    data: number[];
    color?: string;
    /** Optional formatter applied to Y-axis labels and tooltip. */
    formatter?: (v: number) => string;
  }

  interface Props {
    series: SeriesConfig[];
    height?: string;
    /** Show X-axis tick labels */
    showXAxis?: boolean;
    smooth?: boolean;
    areaStyle?: boolean;
  }

  let {
    series,
    height = "160px",
    showXAxis = false,
    smooth = true,
    areaStyle = true,
  }: Props = $props();

  let container: HTMLDivElement;
  let chart: ECharts | null = null;

  // Build 60-point X-axis labels: "-59s" … "0s"
  const xLabels = Array.from({ length: 60 }, (_, i) => `${i - 59}s`);

  function buildOption(s: SeriesConfig[], p: ChartPalette): ChartOption {
    const firstFormatter = s[0]?.formatter;
    return {
      darkMode: p.darkMode,
      backgroundColor: "transparent",
      animation: false,
      grid: { top: 8, right: 8, bottom: showXAxis ? 24 : 8, left: 48 },
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
            value: number;
            color: string;
          }>;
          return items
            .map((p) => {
              const fmt = s.find((x) => x.label === p.seriesName)?.formatter;
              const val = fmt ? fmt(p.value) : p.value.toFixed(1);
              return `<span style="color:${p.color}">●</span> ${p.seriesName}: <b>${val}</b>`;
            })
            .join("<br/>");
        },
      },
      xAxis: {
        type: "category",
        data: xLabels,
        show: showXAxis,
        axisLabel: { color: p.axisLabel, fontSize: 10 },
        axisLine: { lineStyle: { color: p.border } },
        splitLine: { show: false },
      },
      yAxis: {
        type: "value",
        min: 0,
        axisLabel: {
          color: p.axisLabel,
          fontSize: 10,
          formatter: firstFormatter
            ? (v: number) => firstFormatter(v)
            : undefined,
        },
        axisLine: { show: false },
        splitLine: { lineStyle: { color: p.track } },
      },
      series: s.map((ser, idx) => ({
        name: ser.label,
        type: "line",
        data: ser.data,
        smooth,
        symbol: "none",
        lineStyle: {
          color: ser.color ?? p.series[idx % p.series.length],
          width: 2,
        },
        itemStyle: {
          color: ser.color ?? p.series[idx % p.series.length],
        },
        areaStyle: areaStyle
          ? {
              color: new graphic.LinearGradient(0, 0, 0, 1, [
                {
                  offset: 0,
                  color:
                    (ser.color ?? p.series[idx % p.series.length]) +
                    "40",
                },
                { offset: 1, color: "transparent" },
              ]),
            }
          : undefined,
      })),
    };
  }

  const palette = $derived(chartPalette(theme.resolved));

  onMount(() => {
    chart = init(container, null, { renderer: "canvas" });
    chart.setOption(buildOption(series, palette));

    const ro = new ResizeObserver(() => chart?.resize());
    ro.observe(container);
    return () => ro.disconnect();
  });

  // Reads palette as well as series, so a theme change repaints the chart. A
  // merging setOption is enough: every colour the palette supplies is stated
  // outright in the option above, so each one is overwritten by name.
  $effect(() => {
    chart?.setOption(buildOption(series, palette), { notMerge: false });
  });

  onDestroy(() => {
    chart?.dispose();
    chart = null;
  });
</script>

<div bind:this={container} style="width:100%;height:{height};"></div>
