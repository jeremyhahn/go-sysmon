<script lang="ts">
  // The fleet-level statement (#46): what the fleet is, rather than what each
  // node is. Every figure comes from the server's roll-up and follows one rule
  // -- the worst value with its node named, or a sum with its denominator,
  // never a mean -- and a figure no reachable node could report is left out
  // rather than shown as zero.
  import { nodeStatusBadgeClass } from "$lib/alerts";
  import {
    formatBytesRate,
    formatPercent,
    formatTemp,
    formatWatts,
    usageColor,
  } from "$lib/format";
  import {
    attentionReason,
    buildsLine,
    coverage,
    rollupHeadline,
    tempSourceLabel,
    visibleAttention,
  } from "$lib/rollup";
  import type {
    FleetExtreme,
    FleetHealth,
    FleetRollup,
    NodeAttention,
  } from "$lib/types";

  interface Props {
    rollup: FleetRollup;
    /** Called with a node's entry when it is chosen from the attention list. */
    onPick: (node: NodeAttention) => void;
  }

  let { rollup, onPick }: Props = $props();

  const headline = $derived(rollupHeadline(rollup));
  const attention = $derived(visibleAttention(rollup));

  /** "ci-node-1 (GPU)", or the bare node when the kind is unknown. */
  function hottestWhere(e: FleetExtreme): string {
    const kind = tempSourceLabel(e.kind);
    return kind === "" ? e.node : `${e.node} (${kind})`;
  }

  function deviceLine(h: FleetHealth, noun: string): string {
    return `${h.unhealthy} unhealthy of ${h.devices} ${noun}, on ${h.nodes} ${coverage(h.of, rollup)}`;
  }
</script>

<section class="space-y-3" aria-label="Fleet roll-up">
  <!-- The state count. Unreachable is its own term, never folded into the
       healthy count or a fraction: the node is unknown, not fine. -->
  <p class="text-sm font-semibold" data-testid="fleet-headline">{headline}</p>

  {#if attention.shown.length > 0}
    <ul class="text-xs space-y-1" aria-label="Nodes needing attention">
      {#each attention.shown as a (a.name)}
        <li class="flex flex-wrap items-baseline gap-2">
          <span class="badge {nodeStatusBadgeClass(a.status)} badge-xs">{a.status}</span>
          <button
            type="button"
            class="link link-hover font-semibold"
            onclick={() => onPick(a)}
          >
            {a.name}
          </button>
          <span class="text-base-content/70 break-words">{attentionReason(a)}</span>
        </li>
      {/each}
      {#if attention.more > 0}
        <li class="text-base-content/60">and {attention.more} more</li>
      {/if}
    </ul>
  {/if}

  <div
    class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-x-4 gap-y-2 text-xs"
    data-testid="fleet-figures"
  >
    <!-- Nodes whose CPU usage could not be measured are counted, so the
         busiest figure is never read as covering them. -->
    {#if rollup.cpu || rollup.cpu_unknown > 0}
      <div data-testid="fleet-cpu">
        <p class="text-base-content/60">Highest CPU</p>
        {#if rollup.cpu}
          <p class="font-mono {usageColor(rollup.cpu.value)}">
            {formatPercent(rollup.cpu.value)} <span class="text-base-content/70">{rollup.cpu.node}</span>
          </p>
        {/if}
        {#if rollup.cpu_unknown > 0}
          <p class="text-base-content/60">
            unknown on {rollup.cpu_unknown} node{rollup.cpu_unknown === 1 ? "" : "s"}
          </p>
        {/if}
      </div>
    {/if}
    {#if rollup.memory}
      <div>
        <p class="text-base-content/60">Highest memory</p>
        <p class="font-mono {usageColor(rollup.memory.value)}">
          {formatPercent(rollup.memory.value)} <span class="text-base-content/70">{rollup.memory.node}</span>
        </p>
      </div>
    {/if}
    {#if rollup.disk}
      <div>
        <p class="text-base-content/60">Fullest filesystem</p>
        <p class="font-mono {usageColor(rollup.disk.value)}">
          {formatPercent(rollup.disk.value)} <span class="text-base-content/70">{rollup.disk.node}</span>
        </p>
      </div>
    {/if}
    {#if rollup.load_per_core}
      <div>
        <p class="text-base-content/60">Highest load / core</p>
        <p class="font-mono">
          {rollup.load_per_core.value.toFixed(2)} <span class="text-base-content/70">{rollup.load_per_core.node}</span>
        </p>
      </div>
    {/if}
    {#if rollup.temperature}
      <div>
        <!-- Every component that reports a temperature: CPU cores, drives,
             GPUs and NPUs, with the kind named beside the node. -->
        <p class="text-base-content/60">Hottest component</p>
        <p class="font-mono">
          {formatTemp(rollup.temperature.value)} <span class="text-base-content/70">{hottestWhere(rollup.temperature)}</span>
        </p>
      </div>
    {/if}
    <!-- Paging, not occupancy (#134): full swap that is not being paged is not
         pressure. Nodes whose paging could not be measured are counted, so a
         quiet figure is never read as covering them. -->
    {#if rollup.swap_out || rollup.swap_unknown > 0}
      <div data-testid="fleet-swap">
        <p class="text-base-content/60">Swap-out</p>
        {#if rollup.swap_out}
          <p class="font-mono">
            {#if rollup.swap_out.value > 0}
              {formatBytesRate(rollup.swap_out.value)} <span class="text-base-content/70">{rollup.swap_out.node}</span>
            {:else}
              no paging
            {/if}
          </p>
        {/if}
        {#if rollup.swap_unknown > 0}
          <p class="text-base-content/60">
            unknown on {rollup.swap_unknown} node{rollup.swap_unknown === 1 ? "" : "s"}
          </p>
        {/if}
      </div>
    {/if}
    <!-- A sum is always shown with the nodes it was taken over. Nodes without
         a power sensor report 0 meaning "no sensor", not "no draw", so a bare
         total would be confidently wrong. -->
    {#if rollup.power}
      <div data-testid="fleet-power">
        <p class="text-base-content/60">Power</p>
        <p class="font-mono">{formatWatts(rollup.power.value)}</p>
        <p class="text-base-content/60">across {rollup.power.nodes} {coverage(rollup.power.of, rollup)}</p>
      </div>
    {/if}
    <!-- Summed over each node's physical devices, so bonded, bridged and
         guest traffic is counted once; with the nodes it covers. -->
    {#if rollup.network}
      <div data-testid="fleet-network">
        <p class="text-base-content/60">Network</p>
        <p class="font-mono">
          <span class="text-info">↓ {formatBytesRate(rollup.network.rx_rate)}</span>
          <span class="text-success ml-1">↑ {formatBytesRate(rollup.network.tx_rate)}</span>
        </p>
        <p class="text-base-content/60">across {rollup.network.nodes} {coverage(rollup.network.of, rollup)}</p>
      </div>
    {/if}
    <!-- Devices, not nodes, and including the drives behind a RAID controller.
         Omitted when no node has any: "0 of 0 unhealthy" would read as a clean
         bill of health for hardware that does not exist. -->
    {#if rollup.smart}
      <div data-testid="fleet-smart">
        <p class="text-base-content/60">SMART</p>
        <p class="font-mono {rollup.smart.unhealthy > 0 ? 'text-error' : 'text-success'}">
          {deviceLine(rollup.smart, "devices")}
        </p>
      </div>
    {/if}
    {#if rollup.raid}
      <div data-testid="fleet-raid">
        <p class="text-base-content/60">RAID</p>
        <p class="font-mono {rollup.raid.unhealthy > 0 ? 'text-error' : 'text-success'}">
          {deviceLine(rollup.raid, rollup.raid.devices === 1 ? "array" : "arrays")}
        </p>
      </div>
    {/if}
  </div>

  {#if rollup.builds.length > 0}
    <!-- Identity, not health: never coloured, never counted as a fault. -->
    <p class="text-xs text-base-content/60" data-testid="fleet-builds">
      Builds: {buildsLine(rollup.builds, rollup.build_groups)}
    </p>
  {/if}
</section>
