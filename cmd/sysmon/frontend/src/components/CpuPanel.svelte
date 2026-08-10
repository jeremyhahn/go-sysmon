<script lang="ts">
  import TimeSeriesChart from "./TimeSeriesChart.svelte";
  import GaugeChart from "./GaugeChart.svelte";
  import {
    formatPercent,
    formatMHz,
    formatBytes,
    formatTemp,
    formatWatts,
    tempColor,
    tempProgressColor,
    usageColor,
    usageProgressColor,
  } from "$lib/format";
  import type { CPUInfo } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const cpuHistory = $derived(monitor.cpuHistory);
  const cpuOverallHistory = $derived(monitor.cpuOverallHistory);
  const overallCpuPercent = $derived(monitor.overallCpuPercent);

  // First CPU provides model info (all logical cores share the same model).
  const firstCpu = $derived(snapshot?.cpus[0] ?? null);

  // Unique physical CPUs by physical_id for the details table.
  const physicalCpus = $derived<CPUInfo[]>(
    snapshot != null
      ? [
          ...new Map(
            snapshot.cpus.map((c) => [c.physical_id || String(c.index), c])
          ).values(),
        ]
      : []
  );

  const cpuSummary = $derived(snapshot?.cpu_summary ?? null);

  // Whether any logical CPU reports a non-zero temperature.
  const hasCpuTemps = $derived(
    snapshot?.cpus.some((c) => (c.temperature_celsius ?? 0) > 0) ?? false
  );

  // Flag search state.
  let flagSearch = $state("");

  const allFlags = $derived(firstCpu?.flags ?? []);

  const filteredFlags = $derived(
    flagSearch.trim() === ""
      ? allFlags
      : allFlags.filter((f) =>
          f.toLowerCase().includes(flagSearch.trim().toLowerCase())
        )
  );

  // Sensor data derived from the monitor store.
  const cpuTempHistory = $derived(monitor.cpuTempHistory);
  const packagePowerHistory = $derived(monitor.packagePowerHistory);

  const sensors = $derived(snapshot?.sensors ?? null);

  const hasCoreTempData = $derived((sensors?.core_temps?.length ?? 0) > 0);
  const hasCoreVoltageData = $derived((sensors?.core_voltages?.length ?? 0) > 0);
  const hasPackagePowerData = $derived((sensors?.package_power?.length ?? 0) > 0);
  const hasThrottleData = $derived((sensors?.thermal_throttle?.length ?? 0) > 0);

  const totalPowerW = $derived(
    sensors?.package_power?.reduce((s, p) => s + p.power_w, 0) ?? 0
  );
  const totalMaxPowerW = $derived(
    sensors?.package_power?.reduce((s, p) => s + p.max_power_w, 0) ?? 0
  );
  const totalPowerPercent = $derived(
    totalMaxPowerW > 0 ? (totalPowerW / totalMaxPowerW) * 100 : 0
  );

  const hasThrottling = $derived(
    sensors?.thermal_throttle?.some(
      (t) => t.core_throttle_count > 0 || t.package_throttle_count > 0
    ) ?? false
  );

  function powerBarColor(pct: number): string {
    if (pct >= 85) return "progress-error";
    if (pct >= 70) return "progress-warning";
    return "progress-success";
  }
</script>

<div class="space-y-4">
  <!-- Overall usage + chart -->
  <div class="grid grid-cols-1 lg:grid-cols-3 gap-4">
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4 items-center">
        <h3 class="card-title text-sm text-base-content/70">Overall CPU</h3>
        <GaugeChart value={overallCpuPercent} label="CPU" height="180px" />
      </div>
    </div>

    <div class="card bg-base-200 shadow-sm lg:col-span-2">
      <div class="card-body p-4">
        <h3 class="card-title text-sm text-base-content/70 mb-2">
          CPU Usage (60s)
        </h3>
        <TimeSeriesChart
          series={[
            {
              label: "Overall %",
              data: cpuOverallHistory,
              color: "#36d399",
              formatter: (v) => `${v.toFixed(1)}%`,
            },
          ]}
          height="160px"
          showXAxis
        />
      </div>
    </div>
  </div>

  <!-- Per-core bars -->
  {#if snapshot != null}
    <!-- CPU Summary banner -->
    {#if cpuSummary != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-3">
          <div class="flex flex-wrap gap-4 text-sm text-base-content/80">
            <span>
              <span class="font-semibold text-base-content">
                {cpuSummary.total_cores}
              </span>
              Cores
            </span>
            <span class="text-base-content/30">/</span>
            <span>
              <span class="font-semibold text-base-content">
                {cpuSummary.total_threads}
              </span>
              Threads
            </span>
            <span class="text-base-content/30">/</span>
            <span>
              <span class="font-semibold text-base-content">
                {cpuSummary.sockets}
              </span>
              Socket{cpuSummary.sockets !== 1 ? "s" : ""}
            </span>
            <span class="text-base-content/30">/</span>
            <span>
              <span class="font-semibold text-base-content">
                {cpuSummary.cores_per_socket}
              </span>
              Cores/Socket
            </span>
            <span class="text-base-content/30">/</span>
            <span>
              <span class="font-semibold text-base-content">
                {cpuSummary.threads_per_core}
              </span>
              Threads/Core
            </span>
            {#if cpuSummary.max_mhz > 0}
              <span class="text-base-content/30">/</span>
              <span>
                Max
                <span class="font-semibold text-base-content">
                  {formatMHz(cpuSummary.max_mhz)}
                </span>
              </span>
            {/if}
          </div>
        </div>
      </div>
    {/if}

    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <h3 class="card-title text-sm text-base-content/70 mb-3">
          Per-Core Usage
        </h3>
        <div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-3">
          {#each snapshot.cpus as cpu (cpu.index)}
            <div class="space-y-1">
              <div class="flex justify-between items-center text-xs">
                <span class="text-base-content/70">Core {cpu.index}</span>
                <span class="flex items-center gap-2">
                  {#if hasCpuTemps && (cpu.temperature_celsius ?? 0) > 0}
                    <span
                      class="{tempColor(
                        cpu.temperature_celsius,
                        0
                      )} text-xs"
                    >
                      {formatTemp(cpu.temperature_celsius)}
                    </span>
                  {/if}
                  <span class="{usageColor(cpu.usage_percent)} font-semibold">
                    {formatPercent(cpu.usage_percent)}
                  </span>
                </span>
              </div>
              <progress
                class="progress w-full h-2 {usageProgressColor(cpu.usage_percent)}"
                value={cpu.usage_percent}
                max="100"
              ></progress>
            </div>
          {/each}
        </div>
      </div>
    </div>

    <!-- Per-core sparklines (only shown when > 1 CPU) -->
    {#if snapshot.cpus.length > 1 && cpuHistory.length === snapshot.cpus.length}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            Per-Core History (60s)
          </h3>
          <TimeSeriesChart
            series={snapshot.cpus.map((cpu, i) => ({
              label: `Core ${cpu.index}`,
              data: cpuHistory[i] ?? [],
              formatter: (v) => `${v.toFixed(1)}%`,
            }))}
            height="200px"
            showXAxis
          />
        </div>
      </div>
    {/if}

    <!-- CPU info table -->
    {#if physicalCpus.length > 0}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            CPU Details
          </h3>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Vendor</th>
                  <th>Cores</th>
                  <th>Threads</th>
                  <th>Frequency</th>
                  <th>Cache</th>
                  <th>Microcode</th>
                  {#if hasCpuTemps}
                    <th>Temp</th>
                  {/if}
                </tr>
              </thead>
              <tbody>
                {#each physicalCpus as cpu (cpu.physical_id || cpu.index)}
                  <tr>
                    <td class="max-w-xs truncate">{cpu.model_name}</td>
                    <td>{cpu.vendor_id}</td>
                    <td>{cpu.cores}</td>
                    <td>{cpu.threads}</td>
                    <td>{formatMHz(cpu.mhz)}</td>
                    <td>{formatBytes(cpu.cache_size)}</td>
                    <td class="font-mono text-xs">{cpu.microcode}</td>
                    {#if hasCpuTemps}
                      <td
                        class="font-semibold {tempColor(
                          cpu.temperature_celsius ?? 0,
                          0
                        )}"
                      >
                        {formatTemp(cpu.temperature_celsius ?? 0)}
                      </td>
                    {/if}
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    {/if}

    <!-- CPU Flags -->
    {#if allFlags.length > 0}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <div class="flex items-center justify-between mb-3">
            <h3 class="card-title text-sm text-base-content/70">CPU Flags</h3>
            <span class="text-xs text-base-content/50">
              Showing {filteredFlags.length} of {allFlags.length} flags
            </span>
          </div>
          <input
            type="text"
            class="input input-bordered input-xs w-full max-w-xs mb-3"
            placeholder="Filter flags..."
            bind:value={flagSearch}
          />
          <div class="flex flex-wrap gap-1">
            {#each filteredFlags as flag (flag)}
              <span class="badge badge-outline badge-xs font-mono">{flag}</span>
            {/each}
            {#if filteredFlags.length === 0 && flagSearch.trim() !== ""}
              <span class="text-xs text-base-content/50">
                No flags match "{flagSearch}"
              </span>
            {/if}
          </div>
        </div>
      </div>
    {/if}

    <!-- ------------------------------------------------------------------ -->
    <!-- CPU Temperatures                                                     -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasCoreTempData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            CPU Temperatures
          </h3>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>Package</th>
                  <th>Core</th>
                  <th>Label</th>
                  <th>Temp</th>
                  <th>High</th>
                  <th>Crit</th>
                  <th class="w-32">Level</th>
                </tr>
              </thead>
              <tbody>
                {#each sensors.core_temps as ct, i (i)}
                  {@const pct =
                    ct.crit_celsius > 0
                      ? Math.min(100, (ct.temp_celsius / ct.crit_celsius) * 100)
                      : 0}
                  <tr>
                    <td>{ct.package_id}</td>
                    <td>{ct.core_id}</td>
                    <td class="text-base-content/70">{ct.label}</td>
                    <td
                      class="font-semibold {tempColor(
                        ct.temp_celsius,
                        ct.crit_celsius
                      )}"
                    >
                      {formatTemp(ct.temp_celsius)}
                    </td>
                    <td class="text-base-content/60">
                      {formatTemp(ct.high_celsius)}
                    </td>
                    <td class="text-base-content/60">
                      {formatTemp(ct.crit_celsius)}
                    </td>
                    <td>
                      {#if ct.crit_celsius > 0}
                        <progress
                          class="progress w-full h-1.5 {tempProgressColor(
                            ct.temp_celsius,
                            ct.crit_celsius
                          )}"
                          value={pct}
                          max="100"
                        ></progress>
                      {/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>

          {#if cpuTempHistory.length > 0}
            <div class="mt-4">
              <h4
                class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2"
              >
                Temperature History (60s)
              </h4>
              <TimeSeriesChart
                series={sensors.core_temps.map((ct, i) => ({
                  label: ct.label || `Core ${ct.core_id}`,
                  data: cpuTempHistory[i] ?? [],
                  formatter: (v) => `${v.toFixed(0)}°C`,
                }))}
                height="160px"
                showXAxis
              />
            </div>
          {/if}
        </div>
      </div>
    {/if}

    <!-- ------------------------------------------------------------------ -->
    <!-- CPU Voltages                                                         -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasCoreVoltageData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            CPU Voltages
          </h3>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>Channel</th>
                  <th>Label</th>
                  <th>Voltage</th>
                  <th>Source</th>
                </tr>
              </thead>
              <tbody>
                {#each sensors.core_voltages as cv, i (i)}
                  <tr>
                    <td>{cv.channel}</td>
                    <td>{cv.label}</td>
                    <td class="font-semibold font-mono">
                      {cv.voltage_v.toFixed(3)} V
                    </td>
                    <td class="text-base-content/60 font-mono text-xs">
                      {cv.hwmon_name}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    {/if}

    <!-- ------------------------------------------------------------------ -->
    <!-- Package Power                                                        -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasPackagePowerData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            Package Power (RAPL)
          </h3>

          <div class="space-y-3">
            {#each sensors.package_power as pkg (pkg.package_name)}
              {@const pct =
                pkg.max_power_w > 0
                  ? Math.min(100, (pkg.power_w / pkg.max_power_w) * 100)
                  : 0}
              <div class="space-y-1">
                <div class="flex justify-between items-center text-xs">
                  <span class="text-base-content/70 font-medium">
                    {pkg.package_name}
                  </span>
                  <span class="font-semibold">
                    {formatWatts(pkg.power_w)}
                    {#if pkg.max_power_w > 0}
                      <span class="text-base-content/50 font-normal">
                        / {formatWatts(pkg.max_power_w)}
                      </span>
                    {/if}
                  </span>
                </div>
                {#if pkg.max_power_w > 0}
                  <progress
                    class="progress w-full h-2 {powerBarColor(pct)}"
                    value={pct}
                    max="100"
                  ></progress>
                {/if}
                {#if pkg.energy_joules > 0}
                  <p class="text-xs text-base-content/40">
                    Energy: {pkg.energy_joules.toFixed(1)} J
                  </p>
                {/if}
              </div>
            {/each}
          </div>

          <div class="mt-4">
            <h4
              class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2"
            >
              Total Power Draw (60s)
            </h4>
            <TimeSeriesChart
              series={[
                {
                  label: "Power W",
                  data: packagePowerHistory,
                  color:
                    totalPowerPercent >= 85
                      ? "#f87272"
                      : totalPowerPercent >= 70
                        ? "#fbbd23"
                        : "#36d399",
                  formatter: formatWatts,
                },
              ]}
              height="120px"
              showXAxis
            />
          </div>
        </div>
      </div>
    {/if}

    <!-- ------------------------------------------------------------------ -->
    <!-- Thermal Throttle                                                     -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasThrottleData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <div class="flex items-center gap-2 mb-3">
            <h3 class="card-title text-sm text-base-content/70">
              Thermal Throttle
            </h3>
            {#if hasThrottling}
              <span class="badge badge-warning badge-xs">Active</span>
            {:else}
              <span class="badge badge-success badge-xs">None</span>
            {/if}
          </div>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>CPU</th>
                  <th>Core Throttle Events</th>
                  <th>Package Throttle Events</th>
                </tr>
              </thead>
              <tbody>
                {#each sensors.thermal_throttle as tt (tt.cpu)}
                  <tr>
                    <td>{tt.cpu}</td>
                    <td
                      class={tt.core_throttle_count > 0
                        ? "text-warning font-semibold"
                        : "text-base-content/50"}
                    >
                      {tt.core_throttle_count.toLocaleString()}
                    </td>
                    <td
                      class={tt.package_throttle_count > 0
                        ? "text-error font-semibold"
                        : "text-base-content/50"}
                    >
                      {tt.package_throttle_count.toLocaleString()}
                    </td>
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
