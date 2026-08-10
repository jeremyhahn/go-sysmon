<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import * as echarts from "echarts";
  import { usageEchartsColor } from "$lib/format";

  interface Props {
    value: number;     // 0–100
    label?: string;
    height?: string;
  }

  let { value = 0, label = "", height = "180px" }: Props = $props();

  let container: HTMLDivElement;
  let chart: echarts.ECharts | null = null;

  function buildOption(pct: number): echarts.EChartsOption {
    const color = usageEchartsColor(pct);
    return {
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
                [1, "#2a323c"],
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
            color: "#a6adbb",
          },
          data: [{ value: Math.round(pct), name: label }],
        },
      ],
    };
  }

  onMount(() => {
    chart = echarts.init(container, "dark", { renderer: "canvas" });
    chart.setOption(buildOption(value));

    const ro = new ResizeObserver(() => chart?.resize());
    ro.observe(container);
    return () => ro.disconnect();
  });

  $effect(() => {
    chart?.setOption(buildOption(value), { notMerge: false });
  });

  onDestroy(() => {
    chart?.dispose();
    chart = null;
  });
</script>

<div bind:this={container} style="width:100%;height:{height};"></div>
