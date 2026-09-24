<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import {
    chartPalette,
    init,
    type ChartOption,
    type ChartPalette,
    type ECharts,
  } from "$lib/echarts";
  import { usageEchartsColor } from "$lib/format";
  import { theme } from "$lib/theme.svelte";

  interface Props {
    /** 0–100, or null when the figure was not measured: the gauge then shows
     * an empty track and "—", never a reading of 0%. */
    value: number | null;
    label?: string;
    height?: string;
  }

  let { value = null, label = "", height = "180px" }: Props = $props();

  let container: HTMLDivElement;
  let chart: ECharts | null = null;

  function buildOption(value: number | null, p: ChartPalette): ChartOption {
    const pct = value ?? 0;
    const color = value === null ? p.mutedText : usageEchartsColor(pct, p.usage);
    return {
      darkMode: p.darkMode,
      backgroundColor: "transparent",
      series: [
        {
          type: "gauge",
          startAngle: 200,
          endAngle: -20,
          min: 0,
          max: 100,
          splitNumber: 4,
          radius: "88%",
          axisLine: {
            lineStyle: {
              width: 12,
              color: [
                [pct / 100, color],
                [1, p.track],
              ],
            },
          },
          pointer: { show: false },
          axisTick: { show: false },
          splitLine: { show: false },
          axisLabel: { show: false },
          detail: {
            valueAnimation: true,
            formatter: value === null ? "—" : "{value}%",
            color,
            fontSize: 14,
            fontWeight: "bold",
            offsetCenter: [0, "10%"],
          },
          title: {
            offsetCenter: [0, "40%"],
            fontSize: 11,
            color: p.mutedText,
          },
          data: [{ value: Math.round(pct), name: label }],
        },
      ],
    };
  }

  const palette = $derived(chartPalette(theme.resolved));

  onMount(() => {
    chart = init(container, null, { renderer: "canvas" });
    chart.setOption(buildOption(value, palette));

    const ro = new ResizeObserver(() => chart?.resize());
    ro.observe(container);
    return () => ro.disconnect();
  });

  // Reads palette as well as value, so a theme change repaints the gauge.
  $effect(() => {
    chart?.setOption(buildOption(value, palette), { notMerge: false });
  });

  onDestroy(() => {
    chart?.dispose();
    chart = null;
  });
</script>

<!-- The canvas carries no text a screen reader or a test can read, so the
     figure is stated here too, including when it was not measured. -->
<div
  bind:this={container}
  role="img"
  aria-label="{label}: {value === null ? 'not measured' : `${Math.round(value)}%`}"
  style="width:100%;height:{height};"
></div>
