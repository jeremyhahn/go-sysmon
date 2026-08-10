<script lang="ts">
  import GaugeChart from "./GaugeChart.svelte";
  import TimeSeriesChart from "./TimeSeriesChart.svelte";
  import StatCard from "./StatCard.svelte";
  import {
    formatBytes,
    formatPercent,
    usageProgressColor,
    usageColor,
  } from "$lib/format";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const memHistory = $derived(monitor.memHistory);

  const mem = $derived(snapshot?.memory ?? null);

  const totalDimmCapacity = $derived(
    mem?.dimms != null && mem.dimms.length > 0
      ? mem.dimms.reduce((sum, d) => sum + d.size_bytes, 0)
      : 0
  );
</script>

<div class="space-y-4">
  {#if mem != null}
    <!-- Summary gauges + stat cards -->
    <div class="grid grid-cols-1 lg:grid-cols-3 gap-4">
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4 items-center">
          <h3 class="card-title text-sm text-base-content/70">RAM Usage</h3>
          <GaugeChart value={mem.used_percent} label="RAM" height="180px" />
        </div>
      </div>

      <div class="card bg-base-200 shadow-sm lg:col-span-2">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-2">
            Memory Usage (60s)
          </h3>
          <TimeSeriesChart
            series={[
              {
                label: "Used %",
                data: memHistory,
                color: "#38bdf8",
                formatter: (v) => `${v.toFixed(1)}%`,
              },
            ]}
            height="160px"
            showXAxis
          />
        </div>
      </div>
    </div>

    <!-- Stat cards row -->
    <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
      <StatCard
        label="Total"
        value={formatBytes(mem.total_bytes)}
        colorClass="text-base-content"
      />
      <StatCard
        label="Used"
        value={formatBytes(mem.used_bytes)}
        colorClass={usageColor(mem.used_percent)}
        sub={formatPercent(mem.used_percent)}
      />
      <StatCard
        label="Available"
        value={formatBytes(mem.available_bytes)}
        colorClass="text-success"
      />
      <StatCard
        label="Free"
        value={formatBytes(mem.free_bytes)}
        colorClass="text-base-content/70"
      />
    </div>

    <!-- Buffers / cached / slab breakdown -->
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <h3 class="card-title text-sm text-base-content/70 mb-3">
          Buffers / Cache Breakdown
        </h3>
        <div class="space-y-3">
          {#each [
            { label: "Buffers", bytes: mem.buffers_bytes, color: "progress-info" },
            { label: "Cached", bytes: mem.cached_bytes, color: "progress-primary" },
            { label: "Shared", bytes: mem.shared_bytes, color: "progress-secondary" },
            { label: "Slab", bytes: mem.slab_bytes, color: "progress-accent" },
          ] as item (item.label)}
            {#if item.bytes > 0}
              <div class="space-y-1">
                <div class="flex justify-between text-xs text-base-content/70">
                  <span>{item.label}</span>
                  <span>
                    {formatBytes(item.bytes)}
                    ({formatPercent((item.bytes / mem.total_bytes) * 100)})
                  </span>
                </div>
                <progress
                  class="progress w-full h-2 {item.color}"
                  value={(item.bytes / mem.total_bytes) * 100}
                  max="100"
                ></progress>
              </div>
            {/if}
          {/each}
        </div>
      </div>
    </div>

    <!-- Swap -->
    {#if mem.swap_total_bytes > 0}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">Swap</h3>
          <div class="space-y-2">
            <div class="flex justify-between text-xs text-base-content/70">
              <span>Swap Used</span>
              <span class="{usageColor(mem.swap_percent)} font-medium">
                {formatBytes(mem.swap_used_bytes)} / {formatBytes(mem.swap_total_bytes)}
                ({formatPercent(mem.swap_percent)})
              </span>
            </div>
            <progress
              class="progress w-full h-3 {usageProgressColor(mem.swap_percent)}"
              value={mem.swap_percent}
              max="100"
            ></progress>
            <p class="text-xs text-base-content/50">
              Free: {formatBytes(mem.swap_free_bytes)}
            </p>
          </div>
        </div>
      </div>
    {/if}

    <!-- DIMM table -->
    {#if mem.dimms != null && mem.dimms.length > 0}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <div class="flex items-center justify-between mb-3">
            <h3 class="card-title text-sm text-base-content/70">
              Memory Modules (DIMMs)
            </h3>
            {#if totalDimmCapacity > 0}
              <span class="text-xs text-base-content/60">
                Total capacity: <span class="font-semibold text-base-content">
                  {formatBytes(totalDimmCapacity)}
                </span>
              </span>
            {/if}
          </div>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>Location</th>
                  <th>Bank</th>
                  <th>Size</th>
                  <th>Type</th>
                  <th>Speed</th>
                  <th>Conf. Speed</th>
                  <th>Data / Total Width</th>
                  <th>Rank</th>
                  <th>Voltage</th>
                  <th>Temp</th>
                  <th>Form Factor</th>
                  <th>Manufacturer</th>
                  <th>Part Number</th>
                </tr>
              </thead>
              <tbody>
                {#each mem.dimms as dimm (dimm.location)}
                  <tr>
                    <td class="font-mono text-xs">{dimm.location}</td>
                    <td class="text-xs text-base-content/70">{dimm.bank_locator || "—"}</td>
                    <td>{formatBytes(dimm.size_bytes)}</td>
                    <td>{dimm.type}</td>
                    <td>
                      {#if dimm.speed_mts > 0}
                        {dimm.speed_mts} MT/s
                      {:else}
                        —
                      {/if}
                    </td>
                    <td>
                      {#if dimm.configured_speed_mts > 0}
                        {dimm.configured_speed_mts} MT/s
                      {:else}
                        —
                      {/if}
                    </td>
                    <td class="font-mono text-xs">
                      {#if dimm.data_width_bits > 0 || dimm.total_width_bits > 0}
                        {dimm.data_width_bits} / {dimm.total_width_bits} bits
                      {:else}
                        —
                      {/if}
                    </td>
                    <td>
                      {#if dimm.rank > 0}
                        {dimm.rank}
                      {:else}
                        —
                      {/if}
                    </td>
                    <td class="font-mono text-xs">
                      {#if dimm.configured_voltage > 0}
                        {dimm.configured_voltage.toFixed(1)}V
                      {:else}
                        —
                      {/if}
                    </td>
                    <td>
                      {#if dimm.temperature > 0}
                        <span class="{dimm.temperature >= 85 ? 'text-error' : dimm.temperature >= 70 ? 'text-warning' : 'text-success'} font-semibold">
                          {dimm.temperature.toFixed(0)}°C
                        </span>
                      {:else if !mem.temp_sensor_detected}
                        <span class="tooltip tooltip-left" data-tip="No DIMM thermal sensor detected. Ensure jc42 (DDR3/DDR4) or spd5118 (DDR5) kernel modules are loaded. DDR5 may require CONFIG_SENSORS_SPD5118_DETECT=y.">
                          <span class="badge badge-ghost badge-xs gap-1 text-base-content/40">
                            No sensor
                          </span>
                        </span>
                      {:else}
                        —
                      {/if}
                    </td>
                    <td>{dimm.form_factor}</td>
                    <td>{dimm.manufacturer}</td>
                    <td class="font-mono text-xs">{dimm.part_number}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    {/if}
  {:else}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {/if}
</div>
