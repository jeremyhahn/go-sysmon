<script lang="ts">
  import TimeSeriesChart from "./TimeSeriesChart.svelte";
  import { formatBytes, formatBytesRate } from "$lib/format";
  import type { NetworkInfo } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const networkHistory = $derived(monitor.networkHistory);

  const networks = $derived(snapshot?.networks ?? []);

  function getHistory(name: string) {
    return networkHistory.find((h) => h.name === name);
  }

  function statusBadge(iface: NetworkInfo): string {
    if (!iface.is_up) return "badge-error";
    if (iface.is_loopback) return "badge-neutral";
    if (iface.is_virtual) return "badge-info";
    return "badge-success";
  }

  function statusLabel(iface: NetworkInfo): string {
    if (!iface.is_up) return "DOWN";
    if (iface.is_loopback) return "loopback";
    if (iface.is_virtual) return "virtual";
    return "UP";
  }
</script>

<div class="space-y-4">
  {#if networks.length > 0}
    {#each networks as iface (iface.name)}
      {@const nh = getHistory(iface.name)}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <!-- Header -->
          <div class="flex flex-wrap items-center justify-between gap-2 mb-3">
            <div>
              <div class="flex items-center gap-2">
                <h3 class="font-semibold">{iface.name}</h3>
                <span class="badge {statusBadge(iface)} badge-sm">
                  {statusLabel(iface)}
                </span>
              </div>
              <p class="text-xs text-base-content/60">
                {iface.hardware_addr || "no MAC"}
                {#if iface.driver}
                  &bull; {iface.driver}
                {/if}
                {#if iface.speed_mbps > 0}
                  &bull; {iface.speed_mbps} Mbps
                {/if}
                {#if iface.duplex}
                  {iface.duplex}
                {/if}
              </p>
            </div>
            <div class="text-right text-xs space-y-0.5">
              <div class="text-success font-medium">
                ↑ {formatBytesRate(iface.bytes_sent_rate)}
              </div>
              <div class="text-info font-medium">
                ↓ {formatBytesRate(iface.bytes_recv_rate)}
              </div>
            </div>
          </div>

          <!-- IP addresses -->
          {#if iface.addresses != null && iface.addresses.length > 0}
            <div class="flex flex-wrap gap-1 mb-3">
              {#each iface.addresses as addr (addr)}
                <span class="badge badge-outline badge-xs font-mono">{addr}</span>
              {/each}
            </div>
          {/if}

          <!-- Traffic chart -->
          {#if nh != null}
            <div class="mb-3">
              <p class="text-xs text-base-content/60 mb-1">Traffic Rate (60s)</p>
              <TimeSeriesChart
                series={[
                  {
                    label: "Send",
                    data: nh.sentRates,
                    color: "#36d399",
                    formatter: formatBytesRate,
                  },
                  {
                    label: "Recv",
                    data: nh.recvRates,
                    color: "#38bdf8",
                    formatter: formatBytesRate,
                  },
                ]}
                height="140px"
              />
            </div>
          {/if}

          <!-- Counters grid -->
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs text-base-content/60">
            <div>
              <span class="text-success">Sent:</span>
              {formatBytes(iface.bytes_sent)}
              ({iface.packets_sent.toLocaleString()} pkts)
            </div>
            <div>
              <span class="text-info">Recv:</span>
              {formatBytes(iface.bytes_recv)}
              ({iface.packets_recv.toLocaleString()} pkts)
            </div>
            <div class={iface.errors_in + iface.errors_out > 0 ? "text-error" : ""}>
              Errors: {iface.errors_in} in / {iface.errors_out} out
            </div>
            <div class={iface.drops_in + iface.drops_out > 0 ? "text-warning" : ""}>
              Drops: {iface.drops_in} in / {iface.drops_out} out
            </div>
          </div>

          <!-- Details row -->
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs text-base-content/50 mt-1">
            <div>MTU: {iface.mtu}</div>
            {#if iface.flags != null && iface.flags.length > 0}
              <div class="col-span-3 truncate">
                Flags: {iface.flags.join(", ")}
              </div>
            {/if}
          </div>
        </div>
      </div>
    {/each}
  {:else}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {/if}
</div>
