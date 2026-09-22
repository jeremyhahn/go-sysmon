<script lang="ts">
  /**
   * NPU panel: one card per compute accelerator from /sys/class/accel.
   *
   * Three of these numbers are routinely misread, so the panel is written to
   * keep them apart:
   *
   * - Residency is the share of the interval the device was POWERED UP. It is
   *   never called usage, utilisation, or load, because it says nothing about
   *   work done. With no runtime PM (`state` empty) it is meaningless and is
   *   left out rather than drawn as zero.
   * - Utilisation is real compute time from DRM fdinfo, per engine and per
   *   client. Zero is a legitimate reading -- an idle device, a driver without
   *   fdinfo, or a server that may not read another user's processes -- so it
   *   is rendered plainly and never as an error.
   * - Busy time is the driver's raw cumulative counter, shown as a counter
   *   with no bar, gauge, or derived percentage. On intel_vpu it advances with
   *   power-on time rather than job execution, so a percentage made from it
   *   would sit near 100% forever.
   *
   * While a device sleeps its driver counters are not re-read, because reading
   * them wakes the hardware. Those figures are then marked cached with their
   * age -- normal idle behaviour, styled as information, not as a fault.
   */

  import { history } from "$stores/history.svelte";
  import MetricChart from "./MetricChart.svelte";
  import {
    formatAge,
    formatBytes,
    formatDuration,
    formatMHz,
    formatPercent,
    formatTemp,
    formatWatts,
    tempColor,
    usageColor,
    usageEchartsColor,
    usageProgressColor,
  } from "$lib/format";
  import type { NPUInfo } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";
  import { chartPalette } from "$lib/echarts";
  import { theme } from "$lib/theme.svelte";

  const RESIDENCY_HELP =
    "Share of the interval the accelerator was powered up — how long it was " +
    "awake, not how much work it did. Taken from the driver's runtime-PM counters.";
  const UTILIZATION_HELP =
    "Real compute utilisation: per-client engine time from the DRM usage-stats " +
    "fdinfo interface. 0% is a valid reading when the device is idle, when the " +
    "driver does not implement fdinfo, or when this process may not read the " +
    "owning task's fdinfo.";
  const BUSY_TIME_HELP =
    "The driver's raw cumulative busy counter, shown as a counter only. On " +
    "intel_vpu it tracks power-on time rather than job execution, so a " +
    "percentage derived from it would read near 100% whenever anything keeps " +
    "the device awake.";
  const MEMORY_COUNTER_HELP =
    "The driver's own counter named memory_utilization. It is not a measure of " +
    "memory in use and has no total to be a percentage of.";
  const STALE_HELP =
    "Reading the driver counters resumes a suspended device, so they are left " +
    "alone while it sleeps and the last known values are carried forward. This " +
    "is ordinary idle behaviour.";
  const UNREAD_HELP =
    "This device has been suspended for every sample so far, so its driver " +
    "counters have never been read -- reading them would wake the hardware. " +
    "The figures are unknown rather than zero. Ordinary idle behaviour.";

  const windowLabel = $derived(history.windowLabel);
  const palette = $derived(chartPalette(theme.resolved));

  const npuResidencyHistory = $derived(monitor.npuResidencyHistory);
  const npuUtilHistory = $derived(monitor.npuUtilHistory);

  const npus = $derived<NPUInfo[]>(monitor.snapshot?.npus ?? []);

  /** stateBadge styles runtime-PM state; suspended is idle, not a problem. */
  function stateBadge(state: string): string {
    if (state === "active") return "badge-success";
    if (state === "suspended") return "badge-ghost";
    return "badge-outline";
  }

  /**
   * formatCounterUs renders a raw cumulative driver counter in microseconds as
   * elapsed time. Deliberately never a percentage.
   */
  function formatCounterUs(microseconds: number): string {
    const seconds = microseconds / 1_000_000;
    if (seconds < 1) return `${(microseconds / 1000).toFixed(1)} ms`;
    if (seconds < 600) return `${seconds.toFixed(1)} s`;
    return formatDuration(Math.round(seconds));
  }

  /** formatCounterNs is formatCounterUs for the fdinfo nanosecond counters. */
  function formatCounterNs(nanoseconds: number): string {
    return formatCounterUs(nanoseconds / 1000);
  }

  /** formatMs renders a runtime-PM millisecond total as elapsed time. */
  function formatMs(milliseconds: number): string {
    return formatCounterUs(milliseconds * 1000);
  }
</script>

<div class="space-y-4">
  {#each npus as npu (npu.index)}
    {@const residencyHistData = npuResidencyHistory[npu.index] ?? []}
    {@const utilHistData = npuUtilHistory[npu.index] ?? []}
    <!-- An empty state means the device reports no runtime PM at all, and
         residency is then meaningless rather than zero. -->
    {@const hasResidency = npu.state !== ""}
    <!-- Stale with a zero age means the counters were never read at all:
         the device has been asleep for every sample, so the figures are
         unknown rather than zero. -->
    {@const countersUnread = npu.counters_stale && npu.counters_age_sec <= 0}

    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4 space-y-4">

        <!-- NPU header -->
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div>
            <h3 class="font-bold text-base">
              NPU {npu.index}: {npu.name}
            </h3>
            <p class="text-xs text-base-content/60 mt-0.5">
              Driver: {npu.driver || "unknown"}
              {#if npu.vendor}
                &bull; {npu.vendor}
              {/if}
              {#if npu.device}
                &bull; {npu.device}
              {/if}
            </p>
          </div>
          <div class="flex flex-wrap gap-2 text-xs">
            {#if npu.pci_bus_id}
              <span class="badge badge-outline badge-xs font-mono">
                PCI {npu.pci_bus_id}
              </span>
            {/if}
            {#if npu.pcie_gen_current > 0}
              <span class="badge badge-outline badge-xs">
                PCIe Gen{npu.pcie_gen_current}
                {#if npu.pcie_width_current > 0}
                  x{npu.pcie_width_current}
                {/if}
                {#if npu.pcie_gen_max > 0 && npu.pcie_width_max > 0}
                  / Gen{npu.pcie_gen_max} x{npu.pcie_width_max}
                {/if}
              </span>
            {/if}
            {#if npu.state}
              <span class="badge badge-xs {stateBadge(npu.state)}">{npu.state}</span>
            {:else}
              <span
                class="tooltip tooltip-left"
                data-tip="This device does not report runtime power management, so no residency figure is available."
              >
                <span class="badge badge-outline badge-xs">no runtime PM</span>
              </span>
            {/if}
            {#if npu.power_state}
              <span class="badge badge-outline badge-xs font-mono">{npu.power_state}</span>
            {/if}
            {#if npu.sched_mode}
              <span class="badge badge-outline badge-xs">sched: {npu.sched_mode}</span>
            {/if}
            {#if npu.counters_stale}
              <!-- Informational, not a warning: see STALE_HELP. -->
              <span
                class="tooltip tooltip-left"
                data-tip={countersUnread ? UNREAD_HELP : STALE_HELP}
              >
                <span class="badge badge-info badge-outline badge-xs gap-1">
                  {#if countersUnread}
                    counters not read while suspended
                  {:else}
                    counters cached &bull; {formatAge(npu.counters_age_sec)} old
                  {/if}
                </span>
              </span>
            {/if}
          </div>
        </div>

        <!-- Residency and utilisation, kept visibly separate -->
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div class="card bg-base-100 border border-base-300 shadow-none">
            <div class="card-body p-3 space-y-2">
              <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium flex items-center gap-1">
                Residency
                <span class="tooltip tooltip-right" data-tip={RESIDENCY_HELP}>
                  <span class="badge badge-ghost badge-xs cursor-help">?</span>
                </span>
              </h4>
              {#if hasResidency}
                <p class="text-sm font-semibold {usageColor(npu.residency_percent)}">
                  {formatPercent(npu.residency_percent)}
                </p>
                <progress
                  class="progress w-full h-2 {usageProgressColor(npu.residency_percent)}"
                  value={npu.residency_percent}
                  max="100"
                ></progress>
                <p class="text-xs text-base-content/60">
                  Time powered up, not work done &bull; awake {formatMs(npu.active_time_ms)},
                  suspended {formatMs(npu.suspended_time_ms)}
                </p>
              {:else}
                <p class="text-sm font-semibold text-base-content/60">Not reported</p>
                <p class="text-xs text-base-content/60">
                  This device exposes no runtime power management, so there is no
                  residency to report.
                </p>
              {/if}
            </div>
          </div>

          <div class="card bg-base-100 border border-base-300 shadow-none">
            <div class="card-body p-3 space-y-2">
              <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium flex items-center gap-1">
                Utilization
                <span class="tooltip tooltip-left" data-tip={UTILIZATION_HELP}>
                  <span class="badge badge-ghost badge-xs cursor-help">?</span>
                </span>
              </h4>
              <p class="text-sm font-semibold {usageColor(npu.util_percent)}">
                {formatPercent(npu.util_percent)}
              </p>
              <progress
                class="progress w-full h-2 {usageProgressColor(npu.util_percent)}"
                value={npu.util_percent}
                max="100"
              ></progress>
              <p class="text-xs text-base-content/60">
                Compute time from DRM fdinfo &bull;
                {npu.client_count}
                {npu.client_count === 1 ? "client" : "clients"}
              </p>
            </div>
          </div>
        </div>

        <!-- Raw driver counters: counters only, never percentages -->
        <div class="card bg-base-100 border border-base-300 shadow-none">
          <div class="card-body p-3 space-y-2">
            <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium">
              Driver counters
              {#if npu.counters_stale}
                <span
                  class="tooltip tooltip-right"
                  data-tip={countersUnread ? UNREAD_HELP : STALE_HELP}
                >
                  <span class="badge badge-info badge-outline badge-xs ml-1">
                    {#if countersUnread}
                      not read while suspended
                    {:else}
                      cached {formatAge(npu.counters_age_sec)} ago
                    {/if}
                  </span>
                </span>
              {/if}
            </h4>
            <div class="grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-2 text-xs text-base-content/80">
              <div class="flex justify-between gap-2">
                <span class="text-base-content/60 flex items-center gap-1">
                  Busy time (cumulative)
                  <span class="tooltip tooltip-right" data-tip={BUSY_TIME_HELP}>
                    <span class="badge badge-ghost badge-xs cursor-help">?</span>
                  </span>
                </span>
                <span class="font-semibold {npu.counters_stale ? 'text-base-content/60' : ''}">
                  {#if countersUnread && npu.busy_time_us === 0}
                    not read
                  {:else}
                    {formatCounterUs(npu.busy_time_us)}
                    {#if npu.counters_stale}
                      <span class="text-info">(cached)</span>
                    {/if}
                  {/if}
                </span>
              </div>
              <div class="flex justify-between gap-2">
                <span class="text-base-content/60 flex items-center gap-1">
                  memory_utilization
                  <span class="tooltip tooltip-left" data-tip={MEMORY_COUNTER_HELP}>
                    <span class="badge badge-ghost badge-xs cursor-help">?</span>
                  </span>
                </span>
                <span class="font-semibold {npu.counters_stale ? 'text-base-content/60' : ''}">
                  {#if countersUnread && npu.memory_utilization_bytes === 0}
                    not read
                  {:else}
                    {formatBytes(npu.memory_utilization_bytes)}
                    {#if npu.counters_stale}
                      <span class="text-info">(cached)</span>
                    {/if}
                  {/if}
                </span>
              </div>
            </div>
          </div>
        </div>

        <!-- Charts: one series each, never mixed -->
        <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
          <div class="card bg-base-100 border border-base-300 shadow-none">
            <div class="card-body p-3">
              <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                Residency ({windowLabel})
              </h4>
              {#if hasResidency}
                <!-- No catalogued history metric for NPUs, so this is the
                     live series only. -->
                <MetricChart
                  series={[
                    {
                      label: "Residency %",
                      data: residencyHistData,
                      color: usageEchartsColor(npu.residency_percent, palette.usage),
                      formatter: (v) => `${v.toFixed(1)}%`,
                    },
                  ]}
                  height="120px"
                />
                <p class="text-xs text-base-content/60 mt-1">
                  Share of each interval the device was powered up.
                </p>
              {:else}
                <p class="text-xs text-base-content/60">
                  No runtime power management on this device.
                </p>
              {/if}
            </div>
          </div>

          <div class="card bg-base-100 border border-base-300 shadow-none">
            <div class="card-body p-3">
              <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                Utilization ({windowLabel})
              </h4>
              <MetricChart
                series={[
                  {
                    label: "Utilization %",
                    data: utilHistData,
                    color: "#38bdf8",
                    formatter: (v) => `${v.toFixed(1)}%`,
                  },
                ]}
                height="120px"
              />
              <p class="text-xs text-base-content/60 mt-1">
                Compute time from DRM fdinfo. A flat 0% means no work, or a
                driver that does not report it.
              </p>
            </div>
          </div>
        </div>

        <!-- Engines -->
        {#if npu.engines.length > 0}
          <div class="card bg-base-100 border border-base-300 shadow-none">
            <div class="card-body p-3">
              <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                Engines
              </h4>
              <div class="overflow-x-auto">
                <table class="table table-xs table-zebra">
                  <thead>
                    <tr>
                      <th>Engine</th>
                      <th>Utilization</th>
                      <th>Engine time (cumulative)</th>
                    </tr>
                  </thead>
                  <tbody>
                    {#each npu.engines as engine (engine.name)}
                      <tr>
                        <td class="font-mono">{engine.name}</td>
                        <td class="{usageColor(engine.busy_percent)} font-semibold">
                          {formatPercent(engine.busy_percent)}
                        </td>
                        <td>{formatCounterNs(engine.busy_ns)}</td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        {/if}

        <!-- Clients -->
        {#if npu.clients.length > 0}
          <div class="card bg-base-100 border border-base-300 shadow-none">
            <div class="card-body p-3">
              <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                Clients
              </h4>
              <div class="overflow-x-auto">
                <table class="table table-xs table-zebra">
                  <thead>
                    <tr>
                      <th>PID</th>
                      <th>Process</th>
                      <th>Utilization</th>
                      <th>Memory</th>
                    </tr>
                  </thead>
                  <tbody>
                    {#each npu.clients as client (client.client_id || client.pid)}
                      <tr>
                        <td class="font-mono">{client.pid}</td>
                        <td>{client.name}</td>
                        <td class="{usageColor(client.busy_percent)} font-semibold">
                          {formatPercent(client.busy_percent)}
                        </td>
                        <td>{formatBytes(client.memory_bytes)}</td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        {/if}

        <!-- Device detail footer -->
        <div class="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-4 gap-x-6 gap-y-2 text-xs text-base-content/80">
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">Device</span>
            <span class="font-semibold font-mono">{npu.device || "—"}</span>
          </div>
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">Driver</span>
            <span class="font-semibold">{npu.driver || "—"}</span>
          </div>
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">Vendor</span>
            <span class="font-semibold">
              {npu.vendor || "—"}
              {#if npu.vendor_id}
                <span class="text-base-content/60 font-mono">
                  [{npu.vendor_id}{npu.device_id ? `:${npu.device_id}` : ""}]
                </span>
              {/if}
            </span>
          </div>
          {#if npu.device_class}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Class</span>
              <span class="font-semibold font-mono">{npu.device_class}</span>
            </div>
          {/if}
          {#if npu.label}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Label</span>
              <span class="font-semibold">{npu.label}</span>
            </div>
          {/if}
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">Power state</span>
            <span class="font-semibold font-mono">{npu.power_state || "—"}</span>
          </div>
          {#if npu.frequency_mhz > 0}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Frequency</span>
              <span class="font-semibold">
                {formatMHz(npu.frequency_mhz)}
                {#if npu.frequency_max_mhz > 0}
                  / {formatMHz(npu.frequency_max_mhz)}
                {/if}
              </span>
            </div>
          {:else if npu.frequency_max_mhz > 0}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Max frequency</span>
              <span class="font-semibold">{formatMHz(npu.frequency_max_mhz)}</span>
            </div>
          {/if}
          {#if npu.governor}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Governor</span>
              <span class="font-semibold">{npu.governor}</span>
            </div>
          {/if}
          {#if npu.sched_mode}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Sched mode</span>
              <span class="font-semibold">{npu.sched_mode}</span>
            </div>
          {/if}
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">NUMA node</span>
            <span class="font-semibold">{npu.numa_node >= 0 ? npu.numa_node : "—"}</span>
          </div>
          {#if npu.local_cpu_list}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Local CPUs</span>
              <span class="font-semibold font-mono">{npu.local_cpu_list}</span>
            </div>
          {/if}
          {#if npu.irq > 0}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">IRQ</span>
              <span class="font-semibold">{npu.irq}</span>
            </div>
          {/if}
          {#if npu.temperature_c > 0}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Temperature</span>
              <span class="font-semibold {tempColor(npu.temperature_c, 0)}">
                {formatTemp(npu.temperature_c)}
              </span>
            </div>
          {/if}
          {#if npu.power_draw_w > 0}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Power draw</span>
              <span class="font-semibold">{formatWatts(npu.power_draw_w)}</span>
            </div>
          {/if}
          {#if npu.autosuspend_delay_ms >= 0}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/60">Autosuspend delay</span>
              <span class="font-semibold">{npu.autosuspend_delay_ms} ms</span>
            </div>
          {/if}
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">Runtime usage</span>
            <span class="font-semibold">{npu.runtime_usage}</span>
          </div>
          <div class="flex justify-between gap-2">
            <span class="text-base-content/60">Clients</span>
            <span class="font-semibold">{npu.client_count}</span>
          </div>
        </div>

      </div>
    </div>
  {/each}
</div>
