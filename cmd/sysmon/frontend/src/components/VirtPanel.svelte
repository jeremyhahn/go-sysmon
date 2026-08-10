<script lang="ts">
  import StatCard from "./StatCard.svelte";
  import {
    formatBytes,
    formatBytesRate,
    formatCores,
    formatDuration,
    formatPercent,
    percentOfCapacity,
    usageColor,
  } from "$lib/format";
  import { monitor } from "$stores/monitor.svelte";

  const snapshot = $derived(monitor.snapshot);
  const virt = $derived(snapshot?.virtualization ?? null);
  const vms = $derived(virt?.vms ?? []);
  const capability = $derived(virt?.capability ?? null);
  const canObserve = $derived(capability?.vms_observable ?? false);

  // VMs report configured guest RAM, which is what the host has committed.
  const totalMemory = $derived(vms.reduce((s, v) => s + v.memory_bytes, 0));
  const totalResident = $derived(vms.reduce((s, v) => s + v.rss_bytes, 0));
  const totalVcpus = $derived(vms.reduce((s, v) => s + v.vcpus, 0));
  const totalCpu = $derived(vms.reduce((s, v) => s + v.cpu_percent, 0));

  // Host cores, for judging whether the guests are oversubscribed.
  const hostThreads = $derived(snapshot?.cpu_summary?.total_threads ?? 0);
  const oversubscribed = $derived(hostThreads > 0 && totalVcpus > hostThreads);
</script>

<div class="space-y-4">
  {#if virt != null}
    <div class="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-6 gap-3">
      <StatCard label="Virtual Machines" value={vms.length} colorClass="text-base-content" />
      <StatCard
        label="Total CPU"
        value={formatPercent(percentOfCapacity(totalCpu, hostThreads))}
        sub={`${formatCores(totalCpu)} of ${hostThreads} host threads`}
        colorClass={usageColor(percentOfCapacity(totalCpu, hostThreads))}
      />
      <StatCard
        label="vCPUs"
        value={totalVcpus}
        sub={hostThreads > 0 ? `of ${hostThreads} host threads` : ""}
        colorClass={oversubscribed ? "text-warning" : "text-base-content"}
      />
      <StatCard
        label="Guest RAM"
        value={formatBytes(totalMemory)}
        sub="configured"
        colorClass="text-base-content"
      />
      <StatCard
        label="Resident"
        value={formatBytes(totalResident)}
        sub="on host"
        colorClass="text-base-content"
      />
      <StatCard
        label="Hypervisor"
        value={vms.length > 0 ? vms[0].hypervisor : "–"}
        colorClass="text-base-content/70"
      />
    </div>

    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <h3 class="card-title text-sm text-base-content/70">Virtual Machines</h3>

        {#if vms.length === 0}
          <div class="py-4 space-y-1">
            <p class="text-sm text-base-content/70">
              {canObserve
                ? "No virtual machines are running on this host."
                : "Virtual machines cannot be detected on this host."}
            </p>
            {#if canObserve}
              <p class="text-xs text-base-content/50">
                Detection is working; no qemu, cloud-hypervisor, VirtualBox or
                VMware process is running.
              </p>
            {:else}
              <p class="text-xs text-base-content/50">
                The process list could not be read, so their absence is not confirmed.
              </p>
            {/if}
          </div>
        {:else}
          <div class="space-y-3">
            {#each vms as vm (vm.pid)}
              <div class="rounded-lg bg-base-100 p-3">
                <div class="flex flex-wrap items-baseline justify-between gap-2">
                  <div>
                    <span class="font-semibold">{vm.name}</span>
                    <span class="text-xs text-base-content/50 ml-2">
                      {vm.hypervisor}{vm.accelerator ? ` (${vm.accelerator})` : ""}
                      &bull; pid {vm.pid}
                      {#if vm.uptime_seconds > 0}
                        &bull; up {formatDuration(vm.uptime_seconds)}
                      {/if}
                    </span>
                  </div>
                  <span class="font-mono text-sm text-right {usageColor(percentOfCapacity(vm.cpu_percent, vm.vcpus))}">
                    {formatPercent(percentOfCapacity(vm.cpu_percent, vm.vcpus))} of {vm.vcpus} vCPUs
                    <span class="block text-xs text-base-content/50">
                      {formatCores(vm.cpu_percent)} busy
                    </span>
                  </span>
                </div>

                <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mt-3 text-sm">
                  <div>
                    <p class="text-xs text-base-content/50">vCPUs</p>
                    <p class="font-mono">
                      {vm.vcpus}
                      {#if vm.vcpu_threads > 0 && vm.vcpu_threads !== vm.vcpus}
                        <span class="text-warning text-xs">
                          ({vm.vcpu_threads} threads)
                        </span>
                      {/if}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Guest RAM</p>
                    <p class="font-mono">{formatBytes(vm.memory_bytes)}</p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Resident on host</p>
                    <p class="font-mono">{formatBytes(vm.rss_bytes)}</p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Threads</p>
                    <p class="font-mono">{vm.thread_count}</p>
                  </div>

                  <div>
                    <p class="text-xs text-base-content/50">Network in</p>
                    <p class="font-mono">{formatBytesRate(vm.net_rx_rate)}</p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Network out</p>
                    <p class="font-mono">{formatBytesRate(vm.net_tx_rate)}</p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Host NICs</p>
                    <p class="font-mono">
                      {vm.tap_interfaces && vm.tap_interfaces.length > 0
                        ? vm.tap_interfaces.join(", ")
                        : "–"}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Disk image</p>
                    <p class="font-mono">
                      {vm.disk_image_bytes > 0 ? formatBytes(vm.disk_image_bytes) : "–"}
                    </p>
                  </div>
                </div>

                {#if vm.cgroup_path}
                  <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mt-3 text-sm border-t border-base-300 pt-3">
                    <div>
                      <p class="text-xs text-base-content/50">Disk read</p>
                      <p class="font-mono">{formatBytesRate(vm.disk_read_rate)}</p>
                    </div>
                    <div>
                      <p class="text-xs text-base-content/50">Disk write</p>
                      <p class="font-mono">{formatBytesRate(vm.disk_write_rate)}</p>
                    </div>
                    <div>
                      <p class="text-xs text-base-content/50">Guest memory (cgroup)</p>
                      <p class="font-mono">{formatBytes(vm.memory_current_bytes)}</p>
                    </div>
                    <div>
                      <p class="text-xs text-base-content/50">Stalled CPU / I/O</p>
                      <p class="font-mono">
                        {formatPercent(vm.cpu_pressure)} / {formatPercent(vm.io_pressure)}
                      </p>
                    </div>
                  </div>
                {/if}

                {#if vm.uuid}
                  <p class="text-xs text-base-content/40 mt-2 font-mono">{vm.uuid}</p>
                {/if}
                {#if vm.mac_addresses && vm.mac_addresses.length > 0}
                  <p class="text-xs text-base-content/40 mt-1 font-mono">
                    {vm.mac_addresses.join(", ")}
                  </p>
                {/if}
                {#if vm.disk_images && vm.disk_images.length > 0}
                  <p class="text-xs text-base-content/50 mt-1 break-all">
                    {vm.disk_images.join(", ")}
                  </p>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
      </div>
    </div>
  {:else}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {/if}
</div>
