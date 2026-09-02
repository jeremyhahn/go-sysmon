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
    value: number;     // 0–100
    label?: string;
    height?: string;
  }

  let { value = 0, label = "", height = "180px" }: Props = $props();

  let container: HTMLDivElement;
  let chart: ECharts | null = null;

  function buildOption(pct: number, p: ChartPalette): ChartOption {
    const color = usageEchartsColor(pct);
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
            formatter: "{value}%",
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

<div bind:this={container} style="width:100%;height:{height};"></div>
