<script lang="ts">

  import { history } from "$stores/history.svelte";  import MetricChart from "./MetricChart.svelte";
  import AlertBanner from "./AlertBanner.svelte";
  import { alertBorderClass } from "$lib/alerts";
  import { formatBytes, formatByteRateAsBits, formatInterfaceRate } from "$lib/format";
  import type { NetworkInfo } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";


  // Every heading below says which window it is drawing. With a range
  // applied that is no longer 60 seconds, and a specific wrong answer reads
  // worse than none.
  const windowLabel = $derived(history.windowLabel);
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

  const networkAlerts = $derived(monitor.alertsFor("network"));

  function statusLabel(iface: NetworkInfo): string {
    if (!iface.is_up) return "DOWN";
    if (iface.is_loopback) return "loopback";
    if (iface.is_virtual) return "virtual";
    return "UP";
  }
</script>

<div class="space-y-4">
  <AlertBanner alerts={networkAlerts} title="Network Alert" />

  {#if networks.length > 0}
    {#each networks as iface (iface.name)}
      {@const nh = getHistory(iface.name)}
      {@const ifaceAlerts = monitor.alertsFor("network", iface.name)}
      <div class="card bg-base-200 shadow-sm {alertBorderClass(ifaceAlerts)}">
        <div class="card-body p-4">
          <!-- Header -->
          <div class="flex flex-wrap items-center justify-between gap-2 mb-3">
            <div>
              <div class="flex items-center gap-2">
                <h3 class="font-semibold">{iface.name}</h3>
                <span class="badge {statusBadge(iface)} badge-sm">
                  {statusLabel(iface)}
                </span>
                {#if iface.kind && iface.kind !== "ethernet" && iface.kind !== "loopback"}
                  <span class="badge badge-outline badge-sm">{iface.kind}</span>
                {/if}
                {#if iface.oper_state && iface.oper_state !== "up" && iface.oper_state !== "unknown" && iface.is_up}
                  <span class="badge badge-warning badge-sm">{iface.oper_state}</span>
                {/if}
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
              {#if iface.bus}
                <p class="text-xs text-base-content/60">
                  {[iface.vendor, iface.model].filter(Boolean).join(" ") || "Unknown NIC"}
                  &bull; {iface.bus.toUpperCase()} {iface.vendor_id}:{iface.device_id}
                  {#if iface.subsystem_vendor && iface.subsystem_vendor !== iface.vendor}
                    &bull; {iface.subsystem_vendor}
                  {/if}
                </p>
              {/if}
            </div>
            <div class="text-right text-xs space-y-0.5">
              <div class="text-success font-medium">
                ↑ {formatInterfaceRate(iface, iface.bytes_sent_rate)}
              </div>
              <div class="text-info font-medium">
                ↓ {formatInterfaceRate(iface, iface.bytes_recv_rate)}
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

          <!-- This interface's own resolvers -->
          {#if iface.dns_servers != null && iface.dns_servers.length > 0}
            <div class="text-xs text-base-content/70 mb-3" data-testid="iface-dns">
              <span>DNS:</span>
              {#each iface.dns_servers as ns (ns)}
                <span class="badge badge-ghost badge-xs font-mono ml-1">{ns}</span>
              {/each}
              {#if iface.dns_search != null && iface.dns_search.length > 0}
                <span class="ml-2">search:</span> {iface.dns_search.join(" ")}
              {/if}
            </div>
          {/if}

          <!-- Bonding / bridge topology -->
          {#if iface.bond_mode || iface.bond_master || (iface.bridge_ports != null && iface.bridge_ports.length > 0)}
            <div class="flex flex-wrap items-center gap-1 mb-3 text-xs">
              {#if iface.bond_mode}
                <span class="text-base-content/60">bond</span>
                <span class="badge badge-outline badge-xs">{iface.bond_mode}</span>
                {#if iface.bond_slaves != null}
                  {#each iface.bond_slaves as sl (sl)}
                    <span class="badge badge-ghost badge-xs font-mono">{sl}</span>
                  {/each}
                {/if}
              {/if}
              {#if iface.bond_master}
                <span class="text-base-content/60">slave of</span>
                <span class="badge badge-ghost badge-xs font-mono">{iface.bond_master}</span>
              {/if}
              {#if iface.bridge_ports != null && iface.bridge_ports.length > 0}
                <span class="text-base-content/60">bridge</span>
                {#each iface.bridge_ports as bp (bp)}
                  <span class="badge badge-ghost badge-xs font-mono">{bp}</span>
                {/each}
              {/if}
            </div>
          {/if}

          <!-- Wireless link -->
          {#if iface.wireless != null}
            <div class="flex flex-wrap items-center gap-3 mb-3 text-xs" data-testid="wireless">
              <span class="text-base-content/60">wifi</span>
              {#if iface.wireless.ssid}
                <span>SSID: {iface.wireless.ssid}</span>
              {/if}
              {#if iface.wireless.bssid}
                <span>BSSID: <span class="font-mono">{iface.wireless.bssid}</span></span>
              {/if}
              <span>Quality: {iface.wireless.link_quality}</span>
              <span>Signal: {iface.wireless.signal_level_dbm} dBm</span>
              {#if iface.wireless.noise_level_dbm !== 0}
                <span>Noise: {iface.wireless.noise_level_dbm} dBm</span>
              {/if}
            </div>
          {/if}

          <!-- Traffic chart -->
          {#if nh != null}
            <div class="mb-3">
              <p class="text-xs text-base-content/60 mb-1">Traffic Rate ({windowLabel})</p>
              <MetricChart
                metrics={["net_tx", "net_rx"]}
                splitValue={iface.name}
                series={[
                  {
                    label: "Send",
                    data: nh.sentRates,
                    color: "#36d399",
                    formatter: formatByteRateAsBits,
                  },
                  {
                    label: "Recv",
                    data: nh.recvRates,
                    color: "#38bdf8",
                    formatter: formatByteRateAsBits,
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
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs text-base-content/60 mt-1">
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
