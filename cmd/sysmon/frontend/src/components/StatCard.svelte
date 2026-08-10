<script lang="ts">
  interface Props {
    label: string;
    value: string | number;
    unit?: string;
    sub?: string;
    colorClass?: string;
    trend?: "up" | "down" | "neutral";
  }

  let { label, value, unit = "", sub = "", colorClass = "", trend }: Props =
    $props();
</script>

<div class="card bg-base-200 shadow-sm">
  <div class="card-body p-4">
    <p class="text-xs text-base-content/60 uppercase tracking-wider font-medium">
      {label}
    </p>
    <div class="flex items-baseline gap-1 mt-1">
      <span class="text-2xl font-bold {colorClass}">{value}</span>
      {#if unit}
        <span class="text-sm text-base-content/60">{unit}</span>
      {/if}
      {#if trend === "up"}
        <span class="text-xs text-error ml-1">▲</span>
      {:else if trend === "down"}
        <span class="text-xs text-success ml-1">▼</span>
      {/if}
    </div>
    {#if sub}
      <p class="text-xs text-base-content/50 mt-0.5">{sub}</p>
    {/if}
  </div>
</div>
