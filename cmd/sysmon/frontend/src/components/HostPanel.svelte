<script lang="ts">
  import TimeSeriesChart from "./TimeSeriesChart.svelte";
  import StatCard from "./StatCard.svelte";
  import ProcessTable from "./ProcessTable.svelte";
  import { formatDuration, formatPercent } from "$lib/format";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const loadHistory = $derived(monitor.loadHistory);
  const overallCpuPercent = $derived(monitor.overallCpuPercent);

  const host = $derived(snapshot?.host ?? null);
  const load = $derived(snapshot?.load_avg ?? null);
  const procs = $derived(snapshot?.processes ?? null);
  const mem = $derived(snapshot?.memory ?? null);
</script>

<div class="space-y-4">
  {#if host != null && load != null && procs != null && mem != null}
    <!-- Host identity card -->
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h2 class="text-lg font-bold">{host.hostname}</h2>
            <p class="text-xs text-base-content/60">
              {host.os} {host.platform_version} &bull; {host.kernel_version} &bull;
              {host.kernel_arch} &bull; Uptime: {formatDuration(host.uptime)}
            </p>
            {#if host.board_name}
              <p class="text-xs text-base-content/50">
                {host.board_vendor ? host.board_vendor + " " : ""}{host.board_name}{host.board_version ? " (v" + host.board_version + ")" : ""}
              </p>
            {/if}
            {#if host.bios_version}
              <p class="text-xs text-base-content/50">
                BIOS: {host.bios_vendor ? host.bios_vendor + " " : ""}{host.bios_version}{host.bios_date ? " (" + host.bios_date + ")" : ""}
              </p>
            {/if}
          </div>
          <div class="text-right">
            <p class="text-xs text-base-content/50">Platform</p>
            <p class="font-medium">{host.platform}</p>
          </div>
        </div>
      </div>
    </div>

    <!-- Load averages -->
    <div class="grid grid-cols-1 lg:grid-cols-3 gap-4">
      <div class="card bg-base-200 shadow-sm lg:col-span-2">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70 mb-2">
            Load Average (1m) — 60s history
          </h3>
          <TimeSeriesChart
            series={[
              {
                label: "Load 1m",
                data: loadHistory,
                color: "#a78bfa",
              },
            ]}
            height="120px"
            showXAxis
          />
        </div>
      </div>

      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4 justify-center">
          <h3 class="card-title text-sm text-base-content/70 mb-3">
            Load Averages
          </h3>
          <div class="space-y-2">
            {#each [
              { label: "1 min", value: load.load1 },
              { label: "5 min", value: load.load5 },
              { label: "15 min", value: load.load15 },
            ] as item (item.label)}
              <div class="flex justify-between items-center">
                <span class="text-sm text-base-content/60">{item.label}</span>
                <span class="font-mono font-semibold">
                  {item.value.toFixed(2)}
                </span>
              </div>
            {/each}
          </div>
        </div>
      </div>
    </div>

    <!-- Process summary + quick stats -->
    <div class="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-7 gap-3">
      <StatCard
        label="CPU"
        value={formatPercent(overallCpuPercent)}
        colorClass={overallCpuPercent >= 80
          ? "text-error"
          : overallCpuPercent >= 50
            ? "text-warning"
            : "text-success"}
      />
      <StatCard
        label="Memory"
        value={formatPercent(mem.used_percent)}
        colorClass={mem.used_percent >= 80
          ? "text-error"
          : mem.used_percent >= 50
            ? "text-warning"
            : "text-success"}
      />
      <StatCard
        label="Total Processes"
        value={procs.total}
        colorClass="text-base-content"
      />
      <StatCard
        label="Running (on CPU)"
        value={procs.running}
        colorClass="text-success"
      />
      <StatCard
        label="Sleeping"
        value={procs.sleeping}
        colorClass="text-base-content/70"
      />
      <StatCard
        label="Idle"
        value={procs.idle}
        colorClass="text-base-content/70"
      />
      <StatCard
        label="Zombie"
        value={procs.zombie}
        colorClass={procs.zombie > 0 ? "text-warning" : "text-base-content/50"}
      />
    </div>

    <!-- Process table -->
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <ProcessTable processes={procs.process_list ?? []} />
      </div>
    </div>
  {:else}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {/if}
</div>
