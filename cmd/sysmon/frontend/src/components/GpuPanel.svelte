<script lang="ts">
  import TimeSeriesChart from "./TimeSeriesChart.svelte";
  import GaugeChart from "./GaugeChart.svelte";
  import {
    formatPercent,
    formatMHz,
    formatWatts,
    usageColor,
    usageProgressColor,
    usageEchartsColor,
  } from "$lib/format";
  import type { GPUInfo } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const gpuUtilHistory = $derived(monitor.gpuUtilHistory);
  const gpuMemHistory = $derived(monitor.gpuMemHistory);
  const gpuTempHistory = $derived(monitor.gpuTempHistory);
  const gpuPowerHistory = $derived(monitor.gpuPowerHistory);
  const gpuPcieRxHistory = $derived(monitor.gpuPcieRxHistory);
  const gpuPcieTxHistory = $derived(monitor.gpuPcieTxHistory);

  const gpus = $derived<GPUInfo[]>(snapshot?.gpus ?? []);

  // Color helpers specific to GPU temperature thresholds.
  function tempColor(celsius: number): string {
    if (celsius >= 85) return "text-error";
    if (celsius >= 70) return "text-warning";
    return "text-success";
  }

  function tempEchartsColor(celsius: number): string {
    if (celsius >= 85) return "#f87272";
    if (celsius >= 70) return "#fbbd23";
    return "#36d399";
  }

  function powerColor(drawW: number, limitW: number): string {
    if (limitW <= 0) return "text-base-content";
    const pct = (drawW / limitW) * 100;
    return usageColor(pct);
  }

  function powerEchartsColor(drawW: number, limitW: number): string {
    if (limitW <= 0) return "#36d399";
    const pct = (drawW / limitW) * 100;
    return usageEchartsColor(pct);
  }

  function pcieLinkLabel(current: number, max: number): string {
    if (max <= 0) return current > 0 ? `Gen${current}` : "N/A";
    return `Gen${current} x${current}/${max}`;
  }
</script>

<div class="space-y-4">
  {#if snapshot == null}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {:else if gpus.length === 0}
    <div class="flex flex-col items-center justify-center h-64 gap-3">
      <svg
        xmlns="http://www.w3.org/2000/svg"
        class="h-12 w-12 text-base-content/20"
        fill="none"
        viewBox="0 0 24 24"
        stroke="currentColor"
        stroke-width="1.5"
      >
        <rect x="2" y="6" width="20" height="12" rx="2" />
        <path d="M6 10h.01M10 10h4M10 14h4M18 10h.01" stroke-linecap="round" />
      </svg>
      <p class="text-base-content/50 text-sm">No GPUs detected</p>
      <p class="text-base-content/30 text-xs">
        NVIDIA Management Library (NVML) may not be available on this system.
      </p>
    </div>
  {:else}
    {#each gpus as gpu (gpu.index)}
      {@const utilHistData = gpuUtilHistory[gpu.index] ?? []}
      {@const memHistData = gpuMemHistory[gpu.index] ?? []}
      {@const tempHistData = gpuTempHistory[gpu.index] ?? []}
      {@const powerHistData = gpuPowerHistory[gpu.index] ?? []}
      {@const rxHistData = gpuPcieRxHistory[gpu.index] ?? []}
      {@const txHistData = gpuPcieTxHistory[gpu.index] ?? []}
      {@const powerLimitW = gpu.power_limit_w > 0 ? gpu.power_limit_w : gpu.power_max_w}

      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4 space-y-4">

          <!-- GPU header -->
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div>
              <h3 class="font-bold text-base">
                GPU {gpu.index}: {gpu.name}
              </h3>
              <p class="text-xs text-base-content/60 mt-0.5">
                Driver: {gpu.driver_version}
                {#if gpu.vbios_version}
                  &bull; VBIOS: {gpu.vbios_version}
                {/if}
              </p>
            </div>
            <div class="flex flex-wrap gap-2 text-xs">
              {#if gpu.pci_bus_id}
                <span class="badge badge-outline badge-xs font-mono">
                  PCI {gpu.pci_bus_id}
                </span>
              {/if}
              {#if gpu.pcie_gen_current > 0}
                <span class="badge badge-outline badge-xs">
                  PCIe Gen{gpu.pcie_gen_current}
                  {#if gpu.pcie_width_current > 0}
                    x{gpu.pcie_width_current}
                  {/if}
                  {#if gpu.pcie_gen_max > 0 && gpu.pcie_width_max > 0}
                    / Gen{gpu.pcie_gen_max} x{gpu.pcie_width_max}
                  {/if}
                </span>
              {/if}
              {#if gpu.perf_state}
                <span class="badge badge-primary badge-xs">{gpu.perf_state}</span>
              {/if}
              {#if gpu.compute_mode}
                <span class="badge badge-outline badge-xs">{gpu.compute_mode}</span>
              {/if}
            </div>
          </div>

          <!-- Utilization bars + memory summary -->
          <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
            <!-- Utilization bars -->
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3 space-y-2">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium">
                  Utilization
                </h4>
                {#each [
                  { label: "GPU", value: gpu.gpu_util_percent },
                  { label: "Mem", value: gpu.memory_util_percent },
                  { label: "Enc", value: gpu.encoder_percent },
                  { label: "Dec", value: gpu.decoder_percent },
                ] as row (row.label)}
                  <div class="space-y-0.5">
                    <div class="flex justify-between items-center text-xs">
                      <span class="text-base-content/70 w-8">{row.label}</span>
                      <span class="{usageColor(row.value)} font-semibold">
                        {formatPercent(row.value)}
                      </span>
                    </div>
                    <progress
                      class="progress w-full h-1.5 {usageProgressColor(row.value)}"
                      value={row.value}
                      max="100"
                    ></progress>
                  </div>
                {/each}
              </div>
            </div>

            <!-- Memory summary -->
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3 space-y-2">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium">
                  Memory
                </h4>
                <p class="text-sm font-semibold {usageColor(gpu.memory_percent)}">
                  {gpu.memory_used_mib.toLocaleString()} / {gpu.memory_total_mib.toLocaleString()} MiB
                </p>
                <progress
                  class="progress w-full h-2 {usageProgressColor(gpu.memory_percent)}"
                  value={gpu.memory_percent}
                  max="100"
                ></progress>
                <p class="text-xs text-base-content/60">
                  {formatPercent(gpu.memory_percent)} used &bull;
                  {gpu.memory_free_mib.toLocaleString()} MiB free
                </p>
              </div>
            </div>
          </div>

          <!-- Gauges row -->
          <div class="grid grid-cols-2 gap-4">
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-2 items-center">
                <p class="text-xs text-base-content/60 uppercase tracking-wider">
                  GPU Util
                </p>
                <GaugeChart value={gpu.gpu_util_percent} label="GPU" height="140px" />
              </div>
            </div>
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-2 items-center">
                <p class="text-xs text-base-content/60 uppercase tracking-wider">
                  VRAM
                </p>
                <GaugeChart value={gpu.memory_percent} label="VRAM" height="140px" />
              </div>
            </div>
          </div>

          <!-- Charts grid -->
          <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                  GPU Usage (60s)
                </h4>
                <TimeSeriesChart
                  series={[
                    {
                      label: "GPU %",
                      data: utilHistData,
                      color: usageEchartsColor(gpu.gpu_util_percent),
                      formatter: (v) => `${v.toFixed(1)}%`,
                    },
                  ]}
                  height="120px"
                />
              </div>
            </div>

            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                  Memory Usage (60s)
                </h4>
                <TimeSeriesChart
                  series={[
                    {
                      label: "VRAM %",
                      data: memHistData,
                      color: "#38bdf8",
                      formatter: (v) => `${v.toFixed(1)}%`,
                    },
                  ]}
                  height="120px"
                />
              </div>
            </div>

            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                  Temperature (60s)
                </h4>
                <TimeSeriesChart
                  series={[
                    {
                      label: "Temp °C",
                      data: tempHistData,
                      color: tempEchartsColor(gpu.temperature_gpu),
                      formatter: (v) => `${v.toFixed(0)}°C`,
                    },
                  ]}
                  height="120px"
                />
              </div>
            </div>

            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                  Power Draw (60s)
                </h4>
                <TimeSeriesChart
                  series={[
                    {
                      label: "Power W",
                      data: powerHistData,
                      color: powerEchartsColor(gpu.power_draw_w, powerLimitW),
                      formatter: formatWatts,
                    },
                  ]}
                  height="120px"
                />
              </div>
            </div>

            <div class="card bg-base-300 shadow-none lg:col-span-2">
              <div class="card-body p-3">
                <h4 class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2">
                  PCIe Throughput (60s)
                </h4>
                <TimeSeriesChart
                  series={[
                    {
                      label: "TX MB/s",
                      data: txHistData,
                      color: "#36d399",
                      formatter: (v) => `${v.toFixed(1)} MB/s`,
                    },
                    {
                      label: "RX MB/s",
                      data: rxHistData,
                      color: "#38bdf8",
                      formatter: (v) => `${v.toFixed(1)} MB/s`,
                    },
                  ]}
                  height="120px"
                />
              </div>
            </div>
          </div>

          <!-- Metrics footer row -->
          <div class="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-4 gap-x-6 gap-y-2 text-xs text-base-content/80">
            <div class="flex justify-between gap-2">
              <span class="text-base-content/50">Temp (GPU)</span>
              <span class="font-semibold {tempColor(gpu.temperature_gpu)}">
                {gpu.temperature_gpu}°C
              </span>
            </div>
            {#if gpu.temperature_memory > 0}
              <div class="flex justify-between gap-2">
                <span class="text-base-content/50">Temp (Mem)</span>
                <span class="font-semibold {tempColor(gpu.temperature_memory)}">
                  {gpu.temperature_memory}°C
                </span>
              </div>
            {/if}
            {#if gpu.fan_speed_percent >= 0}
              <div class="flex justify-between gap-2">
                <span class="text-base-content/50">Fan</span>
                <span class="font-semibold">{formatPercent(gpu.fan_speed_percent)}</span>
              </div>
            {/if}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/50">Power</span>
              <span class="font-semibold {powerColor(gpu.power_draw_w, powerLimitW)}">
                {formatWatts(gpu.power_draw_w)}
                {#if powerLimitW > 0}
                  / {formatWatts(powerLimitW)}
                {/if}
              </span>
            </div>
            {#if gpu.clock_graphics_mhz > 0}
              <div class="flex justify-between gap-2">
                <span class="text-base-content/50">GFX Clock</span>
                <span class="font-semibold">
                  {formatMHz(gpu.clock_graphics_mhz)}
                  {#if gpu.clock_max_gfx_mhz > 0}
                    / {formatMHz(gpu.clock_max_gfx_mhz)}
                  {/if}
                </span>
              </div>
            {/if}
            {#if gpu.clock_memory_mhz > 0}
              <div class="flex justify-between gap-2">
                <span class="text-base-content/50">Mem Clock</span>
                <span class="font-semibold">{formatMHz(gpu.clock_memory_mhz)}</span>
              </div>
            {/if}
            {#if gpu.clock_video_mhz > 0}
              <div class="flex justify-between gap-2">
                <span class="text-base-content/50">Video Clock</span>
                <span class="font-semibold">{formatMHz(gpu.clock_video_mhz)}</span>
              </div>
            {/if}
            <div class="flex justify-between gap-2">
              <span class="text-base-content/50">PCIe TX</span>
              <span class="font-semibold">{gpu.pcie_tx_mbps.toFixed(1)} MB/s</span>
            </div>
            <div class="flex justify-between gap-2">
              <span class="text-base-content/50">PCIe RX</span>
              <span class="font-semibold">{gpu.pcie_rx_mbps.toFixed(1)} MB/s</span>
            </div>
            <div class="flex justify-between gap-2">
              <span class="text-base-content/50">Processes</span>
              <span class="font-semibold">{gpu.process_count}</span>
            </div>
            <div class="flex justify-between gap-2">
              <span class="text-base-content/50">ECC</span>
              <span class="font-semibold {gpu.ecc_enabled ? (gpu.ecc_double_bit > 0 ? 'text-error' : gpu.ecc_single_bit > 0 ? 'text-warning' : 'text-success') : 'text-base-content/50'}">
                {#if !gpu.ecc_enabled}
                  Disabled
                {:else if gpu.ecc_double_bit > 0}
                  {gpu.ecc_double_bit} DBE
                {:else if gpu.ecc_single_bit > 0}
                  {gpu.ecc_single_bit} SBE
                {:else}
                  Clean
                {/if}
              </span>
            </div>
          </div>

        </div>
      </div>
    {/each}
  {/if}
</div>
