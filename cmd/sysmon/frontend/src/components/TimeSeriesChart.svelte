<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import {
    init,
    graphic,
    AXIS_POINTER_COLOR,
    DARK_MODE,
    type ChartOption,
    type ECharts,
  } from "$lib/echarts";

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

  const DEFAULT_COLORS = [
    "#36d399", // success green
    "#38bdf8", // sky blue
    "#fb923c", // orange
    "#a78bfa", // purple
    "#f472b6", // pink
  ];

  // Build 60-point X-axis labels: "-59s" … "0s"
  const xLabels = Array.from({ length: 60 }, (_, i) => `${i - 59}s`);

  function buildOption(s: SeriesConfig[]): ChartOption {
    const firstFormatter = s[0]?.formatter;
    return {
      darkMode: DARK_MODE,
      backgroundColor: "transparent",
      animation: false,
      grid: { top: 8, right: 8, bottom: showXAxis ? 24 : 8, left: 48 },
      tooltip: {
        trigger: "axis",
        backgroundColor: "#1d232a",
        borderColor: "#3d4451",
        textStyle: { color: "#a6adbb", fontSize: 11 },
        axisPointer: {
          lineStyle: { color: AXIS_POINTER_COLOR },
          crossStyle: { color: AXIS_POINTER_COLOR },
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
        axisLabel: { color: "#6b7280", fontSize: 10 },
        axisLine: { lineStyle: { color: "#3d4451" } },
        splitLine: { show: false },
      },
      yAxis: {
        type: "value",
        min: 0,
        axisLabel: {
          color: "#6b7280",
          fontSize: 10,
          formatter: firstFormatter
            ? (v: number) => firstFormatter(v)
            : undefined,
        },
        axisLine: { show: false },
        splitLine: { lineStyle: { color: "#2a323c" } },
      },
      series: s.map((ser, idx) => ({
        name: ser.label,
        type: "line",
        data: ser.data,
        smooth,
        symbol: "none",
        lineStyle: {
          color: ser.color ?? DEFAULT_COLORS[idx % DEFAULT_COLORS.length],
          width: 2,
        },
        itemStyle: {
          color: ser.color ?? DEFAULT_COLORS[idx % DEFAULT_COLORS.length],
        },
        areaStyle: areaStyle
          ? {
              color: new graphic.LinearGradient(0, 0, 0, 1, [
                {
                  offset: 0,
                  color:
                    (ser.color ?? DEFAULT_COLORS[idx % DEFAULT_COLORS.length]) +
                    "40",
                },
                { offset: 1, color: "transparent" },
              ]),
            }
          : undefined,
      })),
    };
  }

  onMount(() => {
    chart = init(container, null, { renderer: "canvas" });
    chart.setOption(buildOption(series));

    const ro = new ResizeObserver(() => chart?.resize());
    ro.observe(container);
    return () => ro.disconnect();
  });

  $effect(() => {
    chart?.setOption(buildOption(series), { notMerge: false });
  });

  onDestroy(() => {
    chart?.dispose();
    chart = null;
  });
</script>

<div bind:this={container} style="width:100%;height:{height};"></div>
