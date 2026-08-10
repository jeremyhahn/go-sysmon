<script lang="ts">
  import GaugeChart from "./GaugeChart.svelte";
  import DataTable from "./DataTable.svelte";
  import type { Column } from "$lib/table";
  import type { SMARTAttr } from "$lib/types";
  import TimeSeriesChart from "./TimeSeriesChart.svelte";
  import {
    formatBytes,
    formatBytesRate,
    formatHoursHuman,
    formatDataUnits,
    usageColor,
  } from "$lib/format";
  import type { DiskInfo } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const diskHistory = $derived(monitor.diskHistory);

  const disks = $derived(snapshot?.disks ?? []);

  // Sorted worst-first by default: the lowest "worst" value is the attribute
  // that has come closest to its failure threshold over the drive's life,
  // which is what an operator is looking for.
  const smartColumns: Column<SMARTAttr>[] = [
    { key: "id", label: "ID", align: "right", sortValue: (a) => a.id },
    { key: "name", label: "Name", sortValue: (a) => a.name },
    { key: "value", label: "Value", align: "right", sortValue: (a) => a.value,
      title: "Current normalised value; higher is healthier" },
    { key: "worst", label: "Worst", align: "right", sortValue: (a) => a.worst,
      title: "Lowest normalised value ever recorded for this attribute" },
    { key: "threshold", label: "Threshold", align: "right", sortValue: (a) => a.threshold,
      title: "Value at which the drive considers this attribute failed" },
    { key: "raw", label: "Raw", align: "right", sortValue: (a) => a.raw_value },
    { key: "type", label: "Type", sortValue: (a) => a.type },
    { key: "status", label: "Status", sortValue: (a) => (a.failing ? 0 : 1) },
  ];

  // Track which disks have SMART attrs table expanded.
  let expanded = $state<Set<string>>(new Set());

  function toggleSmart(name: string) {
    const next = new Set(expanded);
    if (next.has(name)) {
      next.delete(name);
    } else {
      next.add(name);
    }
    expanded = next;
  }

  function smartBadge(disk: DiskInfo): string {
    if (!disk.smart_enabled) return "badge-neutral";
    return disk.smart_healthy ? "badge-success" : "badge-error";
  }

  function smartLabel(disk: DiskInfo): string {
    if (!disk.smart_enabled) return "SMART N/A";
    return disk.smart_healthy ? "Healthy" : "FAILING";
  }

  function getDiskHistory(name: string) {
    return diskHistory.find((h) => h.name === name);
  }

  // Life remaining color: green > 50%, yellow 20–50%, red < 20%.
  function lifeColor(pct: number): string {
    if (pct <= 20) return "text-error";
    if (pct <= 50) return "text-warning";
    return "text-success";
  }

  function lifeProgressColor(pct: number): string {
    if (pct <= 20) return "progress-error";
    if (pct <= 50) return "progress-warning";
    return "progress-success";
  }

  // Determine if a disk has any NVMe health data worth displaying.
  function hasNvmeHealth(disk: DiskInfo): boolean {
    return (
      disk.life_remaining_percent > 0 ||
      disk.wear_level_percent > 0 ||
      disk.available_spare_percent > 0 ||
      disk.media_errors > 0 ||
      disk.error_log_entries > 0 ||
      disk.unsafe_shutdowns > 0 ||
      disk.data_units_read > 0 ||
      disk.data_units_written > 0 ||
      disk.estimated_hours_left > 0 ||
      disk.critical_warning > 0 ||
      !!disk.firmware_version
    );
  }

  // Returns true if a disk has any alert condition.
  function diskHasAlert(disk: DiskInfo): boolean {
    return (
      disk.critical_warning > 0 ||
      disk.media_errors > 0 ||
      disk.life_remaining_percent > 0 && disk.life_remaining_percent < 20 ||
      (disk.available_spare_percent > 0 &&
        disk.spare_threshold_percent > 0 &&
        disk.available_spare_percent < disk.spare_threshold_percent)
    );
  }

  const alertDisks = $derived(disks.filter(diskHasAlert));
</script>

<div class="space-y-4">
  {#if disks.length > 0}
    <!-- Global alert banner -->
    {#if alertDisks.length > 0}
      <div role="alert" class="alert alert-error shadow-sm">
        <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5 shrink-0 stroke-current" fill="none" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.538-1.333-3.308 0L3.732 16c-.77 1.333.192 3 1.732 3z"/>
        </svg>
        <div>
          <p class="font-semibold">Disk Health Alert</p>
          <p class="text-sm">
            {alertDisks.map((d) => `/dev/${d.name}`).join(", ")} —
            one or more disks require attention.
          </p>
        </div>
      </div>
    {/if}

    {#each disks as disk (disk.name)}
      {@const dh = getDiskHistory(disk.name)}
      {@const hasAlert = diskHasAlert(disk)}
      <div class="card bg-base-200 shadow-sm {hasAlert ? 'border border-error/40' : ''}">
        <div class="card-body p-4">
          <!-- Header -->
          <div class="flex flex-wrap items-center justify-between gap-2 mb-3">
            <div class="flex items-center gap-3">
              <div>
                <h3 class="font-semibold">
                  /dev/{disk.name}
                </h3>
                <p class="text-xs text-base-content/60">
                  {disk.model || disk.vendor || "Unknown"} &bull;
                  {disk.drive_type}
                  {disk.rotational ? "(HDD)" : "(SSD/NVMe)"}
                  {#if disk.firmware_version}
                    &bull; FW: {disk.firmware_version}
                  {/if}
                  {#if disk.nvme_version}
                    &bull; NVMe {disk.nvme_version}
                  {/if}
                </p>
              </div>
            </div>
            <div class="flex items-center gap-2">
              {#if disk.critical_warning > 0}
                <span class="badge badge-error badge-sm animate-pulse">
                  Critical Warning
                </span>
              {/if}
              {#if disk.temperature_celsius > 0}
                <span
                  class="badge badge-outline text-xs {disk.temperature_celsius > 60
                    ? 'badge-error'
                    : disk.temperature_celsius > 45
                      ? 'badge-warning'
                      : 'badge-success'}"
                >
                  {disk.temperature_celsius.toFixed(0)}°C
                </span>
              {/if}
              <span class="badge {smartBadge(disk)} badge-sm">
                {smartLabel(disk)}
              </span>
            </div>
          </div>

          <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
            <!-- Gauge -->
            {#if disk.total_bytes > 0}
              <div class="flex flex-col items-center">
                <GaugeChart
                  value={disk.used_percent}
                  label={disk.name}
                  height="160px"
                />
                <div class="text-xs text-base-content/60 text-center -mt-2">
                  <span class="{usageColor(disk.used_percent)} font-medium">
                    {formatBytes(disk.used_bytes)}
                  </span>
                  / {formatBytes(disk.total_bytes)}
                </div>
              </div>
            {:else}
              <div class="flex items-center justify-center text-xs text-base-content/50">
                No filesystem usage data
              </div>
            {/if}

            <!-- IO chart -->
            <div class="md:col-span-2">
              <p class="text-xs text-base-content/60 mb-1">I/O Rate (60s)</p>
              {#if dh != null}
                <TimeSeriesChart
                  series={[
                    {
                      label: "Read",
                      data: dh.readRates,
                      color: "#36d399",
                      formatter: formatBytesRate,
                    },
                    {
                      label: "Write",
                      data: dh.writeRates,
                      color: "#fb923c",
                      formatter: formatBytesRate,
                    },
                  ]}
                  height="150px"
                />
              {:else}
                <div class="h-32 flex items-center justify-center text-xs text-base-content/50">
                  Waiting for data...
                </div>
              {/if}

              <!-- IO counters -->
              <div class="grid grid-cols-2 gap-2 mt-2 text-xs text-base-content/60">
                <div>
                  Read: {formatBytes(disk.read_bytes)} total /
                  {disk.read_count} ops
                </div>
                <div>
                  Write: {formatBytes(disk.write_bytes)} total /
                  {disk.write_count} ops
                </div>
                {#if disk.power_on_hours > 0}
                  <div>Power on: {disk.power_on_hours}h</div>
                {/if}
                {#if disk.power_cycles > 0}
                  <div>Power cycles: {disk.power_cycles}</div>
                {/if}
              </div>
            </div>
          </div>

          <!-- NVMe / SMART Health Section -->
          {#if hasNvmeHealth(disk)}
            <div class="mt-4 border-t border-base-300 pt-4">
              <h4 class="text-xs font-semibold text-base-content/70 mb-3 uppercase tracking-wider">
                Health &amp; Endurance
              </h4>
              <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-3">
                <!-- Life Remaining -->
                {#if disk.life_remaining_percent > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1">
                    <p class="text-xs text-base-content/60">Life Remaining</p>
                    <p class="text-lg font-bold {lifeColor(disk.life_remaining_percent)}">
                      {disk.life_remaining_percent}%
                    </p>
                    <progress
                      class="progress w-full h-1.5 {lifeProgressColor(disk.life_remaining_percent)}"
                      value={disk.life_remaining_percent}
                      max="100"
                    ></progress>
                  </div>
                {/if}

                <!-- Wear Level -->
                {#if disk.wear_level_percent > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1">
                    <p class="text-xs text-base-content/60">Wear Level</p>
                    <p class="text-lg font-bold {lifeColor(100 - disk.wear_level_percent)}">
                      {disk.wear_level_percent}% used
                    </p>
                    <progress
                      class="progress w-full h-1.5 {lifeProgressColor(100 - disk.wear_level_percent)}"
                      value={100 - disk.wear_level_percent}
                      max="100"
                    ></progress>
                  </div>
                {/if}

                <!-- Available Spare -->
                {#if disk.available_spare_percent > 0}
                  {@const spareAlert = disk.spare_threshold_percent > 0 && disk.available_spare_percent < disk.spare_threshold_percent}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1 {spareAlert ? 'border border-error/50' : ''}">
                    <p class="text-xs text-base-content/60">Available Spare</p>
                    <p class="text-lg font-bold {spareAlert ? 'text-error' : 'text-success'}">
                      {disk.available_spare_percent}%
                    </p>
                    {#if disk.spare_threshold_percent > 0}
                      <p class="text-xs text-base-content/50">
                        Threshold: {disk.spare_threshold_percent}%
                      </p>
                    {/if}
                  </div>
                {/if}

                <!-- Estimated Time Remaining -->
                {#if disk.estimated_hours_left > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1">
                    <p class="text-xs text-base-content/60">Est. Life Left</p>
                    <p class="text-sm font-semibold text-base-content">
                      {formatHoursHuman(disk.estimated_hours_left)}
                    </p>
                  </div>
                {/if}

                <!-- Data Written -->
                {#if disk.data_units_written > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1">
                    <p class="text-xs text-base-content/60">Data Written</p>
                    <p class="text-sm font-semibold text-base-content">
                      {formatDataUnits(disk.data_units_written)}
                    </p>
                  </div>
                {/if}

                <!-- Data Read -->
                {#if disk.data_units_read > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1">
                    <p class="text-xs text-base-content/60">Data Read</p>
                    <p class="text-sm font-semibold text-base-content">
                      {formatDataUnits(disk.data_units_read)}
                    </p>
                  </div>
                {/if}

                <!-- Media Errors -->
                <div class="bg-base-300 rounded-lg p-3 space-y-1 {disk.media_errors > 0 ? 'border border-error/50' : ''}">
                  <p class="text-xs text-base-content/60">Media Errors</p>
                  <p class="text-lg font-bold {disk.media_errors > 0 ? 'text-error' : 'text-success'}">
                    {disk.media_errors}
                  </p>
                </div>

                <!-- Error Log Entries -->
                <div class="bg-base-300 rounded-lg p-3 space-y-1 {disk.error_log_entries > 0 ? 'border border-error/50' : ''}">
                  <p class="text-xs text-base-content/60">Error Log Entries</p>
                  <p class="text-lg font-bold {disk.error_log_entries > 0 ? 'text-error' : 'text-success'}">
                    {disk.error_log_entries}
                  </p>
                </div>

                <!-- Unsafe Shutdowns -->
                <div class="bg-base-300 rounded-lg p-3 space-y-1">
                  <p class="text-xs text-base-content/60">Unsafe Shutdowns</p>
                  <p class="text-lg font-bold text-base-content">
                    {disk.unsafe_shutdowns}
                  </p>
                </div>

                <!-- Warning Temp Time -->
                {#if disk.warning_temp_time_minutes > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1 border border-warning/50">
                    <p class="text-xs text-base-content/60">Warn Temp Time</p>
                    <p class="text-sm font-semibold text-warning">
                      {disk.warning_temp_time_minutes} min
                    </p>
                  </div>
                {/if}

                <!-- Critical Temp Time -->
                {#if disk.critical_temp_time_minutes > 0}
                  <div class="bg-base-300 rounded-lg p-3 space-y-1 border border-error/50">
                    <p class="text-xs text-base-content/60">Crit Temp Time</p>
                    <p class="text-sm font-semibold text-error">
                      {disk.critical_temp_time_minutes} min
                    </p>
                  </div>
                {/if}
              </div>
            </div>
          {/if}

          <!-- Partitions -->
          {#if disk.partitions != null && disk.partitions.length > 0}
            <div class="mt-3">
              <p class="text-xs text-base-content/60 mb-1 font-medium">
                Partitions
              </p>
              <div class="overflow-x-auto">
                <table class="table table-xs">
                  <thead>
                    <tr>
                      <th>Device</th>
                      <th>Mount</th>
                      <th>FS</th>
                      <th>Options</th>
                    </tr>
                  </thead>
                  <tbody>
                    {#each disk.partitions as part (part.device)}
                      <tr>
                        <td class="font-mono text-xs">{part.device}</td>
                        <td class="font-mono text-xs">{part.mountpoint}</td>
                        <td>{part.fstype}</td>
                        <td class="text-xs text-base-content/50 max-w-xs truncate">
                          {part.opts}
                        </td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            </div>
          {/if}

          <!-- SMART attributes (collapsible) -->
          {#if disk.smart_attrs != null && disk.smart_attrs.length > 0}
            <div class="mt-3">
              <button
                class="btn btn-xs btn-ghost flex items-center gap-1"
                onclick={() => toggleSmart(disk.name)}
              >
                <span>{expanded.has(disk.name) ? "▼" : "▶"}</span>
                SMART Attributes ({disk.smart_attrs.length})
              </button>

              {#if expanded.has(disk.name)}
                <div class="mt-2">
                  <DataTable
                    rows={disk.smart_attrs}
                    columns={smartColumns}
                    cells={smartCells}
                    searchText={(a: SMARTAttr) => `${a.id} ${a.name} ${a.type}`}
                    rowKey={(a: SMARTAttr) => a.id}
                    initialSort="worst"
                    initialDirection="asc"
                    pageSize={10}
                    searchPlaceholder="Search attribute or ID…"
                    emptyMessage="No SMART attributes reported."
                  />
                </div>
              {/if}
            </div>
          {/if}
        </div>
      </div>
    {/each}
  {:else}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {/if}
</div>

{#snippet smartCells(attr: SMARTAttr)}
  <td class="text-right font-mono text-xs">{attr.id}</td>
  <td class={attr.failing ? "text-error font-medium" : ""}>{attr.name}</td>
  <td class="text-right">{attr.value}</td>
  <td class="text-right {attr.worst <= attr.threshold ? 'text-error' : ''}">{attr.worst}</td>
  <td class="text-right text-base-content/60">{attr.threshold}</td>
  <td class="text-right font-mono text-xs">{attr.raw_value}</td>
  <td class="text-xs text-base-content/70">{attr.type}</td>
  <td>
    {#if attr.failing}
      <span class="badge badge-error badge-xs">FAIL</span>
    {:else}
      <span class="badge badge-success badge-xs">OK</span>
    {/if}
  </td>
{/snippet}
