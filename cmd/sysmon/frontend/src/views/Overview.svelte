<script lang="ts">

  import { history } from "$stores/history.svelte";  import StatCard from "$components/StatCard.svelte";
  import GaugeChart from "$components/GaugeChart.svelte";
  import MetricChart from "$components/MetricChart.svelte";
  import {
    formatBytes,
    formatBytesRate,
    formatDuration,
    formatPercent,
    formatTemp,
    formatWatts,
    usageColor,
    usageProgressColor,
    tempColor,
  } from "$lib/format";
  import { monitor } from "$stores/monitor.svelte";
  import type { PSIResource } from "$lib/types";


  // Every heading below says which window it is drawing. With a range
  // applied that is no longer 60 seconds, and a specific wrong answer reads
  // worse than none.
  const windowLabel = $derived(history.windowLabel);
  const snapshot = $derived(monitor.snapshot);
  // Not monitor.connected: that is the local stream, and this badge labels
  // whatever node is SELECTED. A reachable peer read "Disconnected" whenever
  // the local stream was down, and an unreachable one read "Live" whenever it
  // was up. See showingLiveData -- one question, asked in one place, so this
  // badge and the historical banner cannot disagree about the same data.
  const connected = $derived(monitor.showingLiveData);
  const overallCpuPercent = $derived(monitor.overallCpuPercent);
  const cpuOverallHistory = $derived(monitor.cpuOverallHistory);
  const memHistory = $derived(monitor.memHistory);
  const topNetworkByTraffic = $derived(monitor.topNetworkByTraffic);
  const unhealthyDisks = $derived(monitor.unhealthyDisks);
  const networkHistory = $derived(monitor.networkHistory);

  const host = $derived(snapshot?.host ?? null);
  const mem = $derived(snapshot?.memory ?? null);
  const load = $derived(snapshot?.load_avg ?? null);
  const procs = $derived(snapshot?.processes ?? null);
  const disks = $derived(snapshot?.disks ?? []);
  const gpus = $derived(snapshot?.gpus ?? []);

  // Sensor summary values from the monitor store derived properties.
  const maxCpuTemp = $derived(monitor.maxCpuTemp);
  const totalPackagePowerW = $derived(monitor.totalPackagePowerW);
  // Whole-box power: CPU package (RAPL) + every GPU's draw. Either source may be
  // absent (old CPUs report no RAPL energy; headless hosts have no GPU), so the
  // card shows whatever is measurable and breaks it down in the subtitle.
  const totalGpuPowerW = $derived(
    gpus.reduce((s, g) => s + (g.power_draw_w ?? 0), 0)
  );
  const totalSystemPowerW = $derived(totalPackagePowerW + totalGpuPowerW);
  const hasSystemPower = $derived(totalSystemPowerW > 0);
  const powerBreakdown = $derived(
    [
      totalPackagePowerW > 0 ? `CPU ${totalPackagePowerW.toFixed(0)} W` : null,
      totalGpuPowerW > 0 ? `GPU ${totalGpuPowerW.toFixed(0)} W` : null,
    ]
      .filter(Boolean)
      .join(" + ")
  );

  /**
   * Name the card after exactly what is being summed. This is CPU package
   * (RAPL) plus reported GPU draw — it excludes RAM outside the package domain,
   * disks, board, fans and PSU losses, so calling it "system power" would state
   * a whole-box figure the host never measured.
   */
  const powerLabel = $derived(
    totalPackagePowerW > 0 && totalGpuPowerW > 0
      ? "CPU + GPU Power"
      : totalGpuPowerW > 0
        ? "GPU Power"
        : "CPU Package Power"
  );

  const sensors = $derived(snapshot?.sensors ?? null);
  const hasSensorTemp = $derived(maxCpuTemp > 0);
  const hasSensorPower = $derived(totalPackagePowerW > 0);
  const activeFans = $derived(
    sensors?.fans?.filter((f) => f.rpm > 0) ?? []
  );

  const hasThermalZoneData = $derived((sensors?.thermal_zones?.length ?? 0) > 0);
  const hasFanData = $derived((sensors?.fans?.length ?? 0) > 0);
  const hasPsiData = $derived(sensors?.psi != null);

  /**
   * psiRows lays out one PSI resource as "some" then "full".
   *
   * "some" is the share of time at least one task was stalled on the resource;
   * "full" is the share of time *every* runnable task was stalled, which is the
   * number that corresponds to real lost throughput.
   */
  function psiRows(resource: PSIResource) {
    return [
      { label: "avg10", value: resource.some_avg10, full: false },
      { label: "avg60", value: resource.some_avg60, full: false },
      { label: "avg300", value: resource.some_avg300, full: false },
      { label: "avg10", value: resource.full_avg10, full: true },
      { label: "avg60", value: resource.full_avg60, full: true },
      { label: "avg300", value: resource.full_avg300, full: true },
    ];
  }

  const psiCpuHistory = $derived(monitor.psiCpuHistory);
  const psiMemHistory = $derived(monitor.psiMemHistory);
  const psiIOHistory = $derived(monitor.psiIOHistory);

  // Aggregate disk totals
  const totalDiskBytes = $derived(
    disks.reduce((s, d) => s + d.total_bytes, 0)
  );
  const usedDiskBytes = $derived(disks.reduce((s, d) => s + d.used_bytes, 0));
  const diskPercent = $derived(
    totalDiskBytes > 0 ? (usedDiskBytes / totalDiskBytes) * 100 : 0
  );

  // Top network interface by traffic
  const topIface = $derived(topNetworkByTraffic[0] ?? null);
  const topIfaceHistory = $derived(
    topIface != null
      ? networkHistory.find((h) => h.name === topIface.name)
      : null
  );
</script>

<div class="space-y-4">
  {#if snapshot != null && host != null && mem != null && load != null && procs != null}
    <!-- Host info banner -->
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 class="text-lg font-bold">{host.hostname}</h2>
            <p class="text-xs text-base-content/60">
              {host.os} {host.platform_version} &bull; {host.kernel_version} &bull;
              {host.kernel_arch} &bull; Uptime: {formatDuration(host.uptime)}
            </p>
            {#if host.board_name}
              <p class="text-xs text-base-content/60">
                {host.board_vendor ? host.board_vendor + " " : ""}{host.board_name}{host.board_version ? " (v" + host.board_version + ")" : ""}
              </p>
            {/if}
            {#if host.bios_version}
              <p class="text-xs text-base-content/60">
                BIOS: {host.bios_vendor ? host.bios_vendor + " " : ""}{host.bios_version}{host.bios_date ? " (" + host.bios_date + ")" : ""}
              </p>
            {/if}
          </div>
          <div class="flex items-center gap-2">
            <span
              class="badge {connected ? 'badge-success' : 'badge-error'} badge-sm gap-1"
            >
              <span
                class="w-1.5 h-1.5 rounded-full {connected
                  ? 'bg-success-content'
                  : 'bg-error-content'} animate-pulse"
              ></span>
              {connected ? "Live" : "Disconnected"}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- Gauges row -->
    <div class="grid grid-cols-2 sm:grid-cols-4 gap-4">
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-2 items-center">
          <p class="text-xs text-base-content/60 uppercase tracking-wider">CPU</p>
          <GaugeChart value={overallCpuPercent} label="CPU" height="140px" />
        </div>
      </div>
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-2 items-center">
          <p class="text-xs text-base-content/60 uppercase tracking-wider">
            Memory
          </p>
          <GaugeChart value={mem.used_percent} label="RAM" height="140px" />
        </div>
      </div>
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-2 items-center">
          <p class="text-xs text-base-content/60 uppercase tracking-wider">
            Disk
          </p>
          <GaugeChart value={diskPercent} label="Disk" height="140px" />
        </div>
      </div>
      {#each gpus as gpu (gpu.index)}
        <div class="card bg-base-200 shadow-sm">
          <div class="card-body p-2 items-center">
            <p class="text-xs text-base-content/60 uppercase tracking-wider">
              GPU {gpus.length > 1 ? gpu.index : ""}
            </p>
            <GaugeChart value={gpu.gpu_util_percent} label="GPU" height="140px" />
            <p class="text-xs text-base-content/60 -mt-1">
              VRAM {formatPercent(gpu.memory_percent)}
              {#if gpu.temperature_gpu > 0}
                &bull; {gpu.temperature_gpu}°C
              {/if}
            </p>
          </div>
        </div>
      {/each}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4 justify-center gap-2">
          <p class="text-xs text-base-content/60 uppercase tracking-wider mb-1">
            Load Avg
          </p>
          {#each [
            { label: "1m", value: load.load1 },
            { label: "5m", value: load.load5 },
            { label: "15m", value: load.load15 },
          ] as item (item.label)}
            <div class="flex justify-between text-sm">
              <span class="text-base-content/60">{item.label}</span>
              <span class="font-mono font-semibold">{item.value.toFixed(2)}</span>
            </div>
          {/each}
        </div>
      </div>
    </div>

    <!-- History charts row -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-2">
            CPU Usage ({windowLabel})
          </h3>
          <MetricChart
            metrics={["cpu"]}
            series={[
              {
                label: "CPU %",
                data: cpuOverallHistory,
                color: "#36d399",
                formatter: (v) => `${v.toFixed(1)}%`,
              },
            ]}
            height="120px"
          />
        </div>
      </div>

      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-2">
            Memory Usage ({windowLabel})
          </h3>
          <MetricChart
            metrics={["memory"]}
            series={[
              {
                label: "Mem %",
                data: memHistory,
                color: "#38bdf8",
                formatter: (v) => `${v.toFixed(1)}%`,
              },
            ]}
            height="120px"
          />
        </div>
      </div>
    </div>

    <!-- Stats grid -->
    <div class="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-6 gap-3">
      <StatCard
        label="RAM Used"
        value={formatBytes(mem.used_bytes)}
        sub="{formatPercent(mem.used_percent)} of {formatBytes(mem.total_bytes)}"
        colorClass={usageColor(mem.used_percent)}
      />
      <StatCard
        label="Swap"
        value={mem.swap_total_bytes > 0
          ? formatPercent(mem.swap_percent)
          : "N/A"}
        colorClass={mem.swap_percent >= 50 ? "text-warning" : "text-base-content/70"}
      />
      <StatCard
        label="Disk Used"
        value={formatBytes(usedDiskBytes)}
        sub="of {formatBytes(totalDiskBytes)}"
        colorClass={usageColor(diskPercent)}
      />
      <StatCard
        label="Processes"
        value={procs.total}
        sub="{procs.running} running"
        colorClass="text-base-content"
      />
      {#if topIface != null}
        <StatCard
          label="Net ↑ ({topIface.name})"
          value={formatBytesRate(topIface.bytes_sent_rate)}
          colorClass="text-success"
        />
        <StatCard
          label="Net ↓ ({topIface.name})"
          value={formatBytesRate(topIface.bytes_recv_rate)}
          colorClass="text-info"
        />
      {/if}
      {#if hasSensorTemp}
        <StatCard
          label="CPU Max Temp"
          value={formatTemp(maxCpuTemp)}
          colorClass={tempColor(maxCpuTemp, 0)}
        />
      {/if}
      {#if hasSystemPower}
        <StatCard
          label={powerLabel}
          value={formatWatts(totalSystemPowerW)}
          sub={powerBreakdown}
          colorClass="text-base-content"
        />
      {/if}
      {#if activeFans.length > 0}
        <StatCard
          label="Fans"
          value={activeFans.length}
          sub={activeFans.map((f) => f.rpm.toLocaleString() + " RPM").join(", ")}
          colorClass="text-base-content"
        />
      {/if}
    </div>

    <!-- Top network chart -->
    {#if topIface != null && topIfaceHistory != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-2">
            Network — {topIface.name} Traffic ({windowLabel})
          </h3>
          <MetricChart
            metrics={["net_tx", "net_rx"]}
            splitValue={topIface.name}
            series={[
              {
                label: "Send",
                data: topIfaceHistory.sentRates,
                color: "#36d399",
                formatter: formatBytesRate,
              },
              {
                label: "Recv",
                data: topIfaceHistory.recvRates,
                color: "#38bdf8",
                formatter: formatBytesRate,
              },
            ]}
            height="120px"
          />
        </div>
      </div>
    {/if}

    <!-- Unhealthy disks alert -->
    {#if unhealthyDisks.length > 0}
      <div class="alert alert-error shadow-sm">
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-5 w-5 shrink-0"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M12 9v2m0 4h.01M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"
          />
        </svg>
        <div>
          <p class="font-semibold">SMART failure detected</p>
          <p class="text-sm">
            Failing disks: {unhealthyDisks.map((d) => d.name).join(", ")}
          </p>
        </div>
      </div>
    {/if}

    <!-- ------------------------------------------------------------------ -->
    <!-- Thermal Zones                                                        -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasThermalZoneData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            Thermal Zones
          </h3>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>Zone</th>
                  <th>Type</th>
                  <th>Temp</th>
                  <th>Policy</th>
                </tr>
              </thead>
              <tbody>
                {#each sensors.thermal_zones as tz, i (i)}
                  <tr>
                    <td class="font-mono text-xs">{tz.name}</td>
                    <td class="text-base-content/70">{tz.type}</td>
                    <td class="font-semibold {tempColor(tz.temp_celsius, 0)}">
                      {formatTemp(tz.temp_celsius)}
                    </td>
                    <td class="text-base-content/60 text-xs">{tz.policy}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    {/if}

    <!-- ------------------------------------------------------------------ -->
    <!-- Fan Speeds                                                           -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasFanData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            Fan Speeds
          </h3>
          <div class="overflow-x-auto">
            <table class="table table-xs table-zebra">
              <thead>
                <tr>
                  <th>Label</th>
                  <th>RPM</th>
                  <th>Min RPM</th>
                  <th>Max RPM</th>
                  <th class="w-28">Speed</th>
                  <th>Source</th>
                </tr>
              </thead>
              <tbody>
                {#each sensors.fans as fan, i (i)}
                  {@const pct =
                    fan.max_rpm > 0
                      ? Math.min(100, (fan.rpm / fan.max_rpm) * 100)
                      : 0}
                  <tr>
                    <td class="font-medium">{fan.label}</td>
                    <td class="font-semibold font-mono">
                      {fan.rpm.toLocaleString()}
                    </td>
                    <td class="text-base-content/60">
                      {fan.min_rpm > 0 ? fan.min_rpm.toLocaleString() : "-"}
                    </td>
                    <td class="text-base-content/60">
                      {fan.max_rpm > 0 ? fan.max_rpm.toLocaleString() : "-"}
                    </td>
                    <td>
                      {#if fan.max_rpm > 0}
                        <progress
                          class="progress w-full h-1.5 {usageProgressColor(pct)}"
                          value={pct}
                          max="100"
                        ></progress>
                      {:else}
                        <span class="text-base-content/60 text-xs">N/A</span>
                      {/if}
                    </td>
                    <td class="text-base-content/60 font-mono text-xs">
                      {fan.hwmon_name}
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
    <!-- PSI — Pressure Stall Information                                     -->
    <!-- ------------------------------------------------------------------ -->
    {#if hasPsiData && sensors != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-1">
            Pressure Stall Information (PSI)
          </h3>
          <p class="text-xs text-base-content/60 mb-3">
            <span class="font-medium">some</span> = at least one task was stalled
            waiting for the resource &bull;
            <span class="font-medium">full</span> = every task was stalled, so
            nothing progressed at all.
          </p>

          <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
            <!-- CPU PSI -->
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3 space-y-2">
                <h4
                  class="text-xs text-base-content/60 uppercase tracking-wider font-medium"
                >
                  CPU
                </h4>
                {#each psiRows(sensors.psi.cpu) as row, i (row.full + row.label)}
                  {#if i === 0 || i === 3}
                    <p class="text-[0.65rem] uppercase tracking-wider text-base-content/60 pt-1">
                      {row.full ? "full" : "some"}
                    </p>
                  {/if}
                  <div class="space-y-0.5">
                    <div class="flex justify-between items-center text-xs">
                      <span class="text-base-content/60">{row.label}</span>
                      <span class="font-semibold">
                        {formatPercent(row.value)}
                      </span>
                    </div>
                    <progress
                      class="progress w-full h-1 {usageProgressColor(row.value)}"
                      value={row.value}
                      max="100"
                    ></progress>
                  </div>
                {/each}
              </div>
            </div>

            <!-- Memory PSI -->
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3 space-y-2">
                <h4
                  class="text-xs text-base-content/60 uppercase tracking-wider font-medium"
                >
                  Memory
                </h4>
                {#each psiRows(sensors.psi.memory) as row, i (row.full + row.label)}
                  {#if i === 0 || i === 3}
                    <p class="text-[0.65rem] uppercase tracking-wider text-base-content/60 pt-1">
                      {row.full ? "full" : "some"}
                    </p>
                  {/if}
                  <div class="space-y-0.5">
                    <div class="flex justify-between items-center text-xs">
                      <span class="text-base-content/60">{row.label}</span>
                      <span class="font-semibold">
                        {formatPercent(row.value)}
                      </span>
                    </div>
                    <progress
                      class="progress w-full h-1 {usageProgressColor(row.value)}"
                      value={row.value}
                      max="100"
                    ></progress>
                  </div>
                {/each}
              </div>
            </div>

            <!-- IO PSI -->
            <div class="card bg-base-300 shadow-none">
              <div class="card-body p-3 space-y-2">
                <h4
                  class="text-xs text-base-content/60 uppercase tracking-wider font-medium"
                >
                  I/O
                </h4>
                {#each psiRows(sensors.psi.io) as row, i (row.full + row.label)}
                  {#if i === 0 || i === 3}
                    <p class="text-[0.65rem] uppercase tracking-wider text-base-content/60 pt-1">
                      {row.full ? "full" : "some"}
                    </p>
                  {/if}
                  <div class="space-y-0.5">
                    <div class="flex justify-between items-center text-xs">
                      <span class="text-base-content/60">{row.label}</span>
                      <span class="font-semibold">
                        {formatPercent(row.value)}
                      </span>
                    </div>
                    <progress
                      class="progress w-full h-1 {usageProgressColor(row.value)}"
                      value={row.value}
                      max="100"
                    ></progress>
                  </div>
                {/each}
              </div>
            </div>
          </div>

          <!-- PSI history sparklines -->
          <div class="mt-4">
            <h4
              class="text-xs text-base-content/60 uppercase tracking-wider font-medium mb-2"
            >
              PSI Stall History — CPU / Memory / I/O ({windowLabel})
            </h4>
            <MetricChart
              metrics={["psi_cpu_some", "psi_memory_some", "psi_io_some"]}
              series={[
                {
                  label: "CPU some%",
                  data: psiCpuHistory,
                  color: "#36d399",
                  formatter: (v) => `${v.toFixed(2)}%`,
                },
                {
                  label: "Mem some%",
                  data: psiMemHistory,
                  color: "#38bdf8",
                  formatter: (v) => `${v.toFixed(2)}%`,
                },
                {
                  label: "I/O some%",
                  data: psiIOHistory,
                  color: "#fbbd23",
                  formatter: (v) => `${v.toFixed(2)}%`,
                },
              ]}
              height="120px"
              showXAxis
            />
          </div>
        </div>
      </div>
    {/if}

  {:else}
    <div class="flex flex-col items-center justify-center h-64 gap-4">
      <span class="loading loading-spinner loading-lg text-primary"></span>
      <p class="text-base-content/60">Waiting for system data…</p>
    </div>
  {/if}
</div>
