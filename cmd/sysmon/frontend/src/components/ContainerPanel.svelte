<script lang="ts">
  import StatCard from "./StatCard.svelte";
  import DataTable from "./DataTable.svelte";
  import type { Column } from "$lib/table";
  import type { ContainerInfo, ImageInfo } from "$lib/types";
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
  const containers = $derived(virt?.containers ?? []);
  const runtime = $derived(virt?.runtime ?? null);
  const capability = $derived(virt?.capability ?? null);
  // An empty list means different things depending on whether we could look.
  const canObserve = $derived(capability?.containers_observable ?? false);
  // Containers Docker knows about that are not running, and therefore have no
  // cgroup for us to measure.
  const notRunning = $derived(
    runtime?.available
      ? Math.max(0, runtime.containers_total - runtime.containers_running)
      : 0
  );

  // Total footprint and what a prune would return.
  const footprint = $derived(
    runtime != null
      ? runtime.layers_bytes + runtime.volumes_bytes + runtime.build_cache_bytes
      : 0
  );
  const reclaimable = $derived(
    runtime != null
      ? runtime.reclaimable_bytes +
        runtime.volumes_reclaimable_bytes +
        runtime.build_cache_reclaimable_bytes
      : 0
  );

  // Rollups.
  const totalCpu = $derived(containers.reduce((s, c) => s + c.cpu_percent, 0));
  const hostThreads = $derived(snapshot?.cpu_summary?.total_threads ?? 0);
  const totalMemory = $derived(containers.reduce((s, c) => s + c.memory_bytes, 0));
  const totalPids = $derived(containers.reduce((s, c) => s + c.pids, 0));
  const totalOomKills = $derived(containers.reduce((s, c) => s + c.oom_kills, 0));
  const throttledCount = $derived(
    containers.filter((c) => c.throttled_percent > 0).length
  );

  // A container selected for the detail drawer.
  let selectedId = $state<string | null>(null);
  const selected = $derived(
    containers.find((c) => c.id === selectedId) ?? null
  );

  function toggle(id: string) {
    selectedId = selectedId === id ? null : id;
  }

  // Throttling is the signal that a container is CPU-capped, so surface it
  // even when average CPU looks low.
  function throttleColor(pct: number): string {
    if (pct >= 25) return "text-error";
    if (pct > 0) return "text-warning";
    return "text-base-content/40";
  }

  const containerColumns: Column<ContainerInfo>[] = [
    { key: "name", label: "Name", sortValue: (c) => c.name },
    { key: "id", label: "ID", sortValue: (c) => c.short_id },
    { key: "runtime", label: "Runtime", sortValue: (c) => c.runtime },
    { key: "cpu", label: "CPU", align: "right", sortValue: (c) => c.cpu_percent,
      title: "Percent of one core, as docker stats reports it" },
    { key: "limit", label: "Limit", align: "right", sortValue: (c) => c.cpu_limit_cores },
    { key: "throttled", label: "Throttled", align: "right", sortValue: (c) => c.throttled_percent,
      title: "Share of scheduling periods in which the kernel stopped this container" },
    { key: "memory", label: "Memory", align: "right", sortValue: (c) => c.memory_bytes },
    { key: "peak", label: "Peak", align: "right", sortValue: (c) => c.memory_peak_bytes },
    { key: "pids", label: "PIDs", align: "right", sortValue: (c) => c.pids },
    { key: "read", label: "Read", align: "right", sortValue: (c) => c.read_bytes_rate },
    { key: "write", label: "Write", align: "right", sortValue: (c) => c.write_bytes_rate },
    { key: "uptime", label: "Uptime", align: "right", sortValue: (c) => c.uptime_seconds },
  ];

  const imageColumns: Column<ImageInfo>[] = [
    { key: "id", label: "ID", sortValue: (i) => i.short_id },
    { key: "tag", label: "Tag", sortValue: (i) => (i.tags ?? ["\uffff"])[0] },
    { key: "size", label: "Size", align: "right", sortValue: (i) => i.size_bytes },
    {
      key: "shared",
      label: "Shared",
      align: "right",
      sortValue: (i) => i.shared_size_bytes,
      title: "Portion of this image in layers shared with other images",
    },
    {
      key: "used",
      label: "Used by",
      align: "right",
      sortValue: (i) => i.containers,
      title: "Containers referencing this image; 0 means it can be pruned",
    },
    { key: "created", label: "Created", align: "right", sortValue: (i) => i.created_unix },
  ];

  function imageAge(unix: number): string {
    if (!unix) return "–";
    const days = Math.floor((Date.now() / 1000 - unix) / 86400);
    if (days < 1) return "today";
    if (days < 30) return `${days}d`;
    if (days < 365) return `${Math.floor(days / 30)}mo`;
    return `${Math.floor(days / 365)}y`;
  }

  function pressureColor(pct: number): string {
    if (pct >= 20) return "text-error";
    if (pct >= 5) return "text-warning";
    return "text-base-content/40";
  }
</script>

<div class="space-y-4">
  {#if virt != null}
    <div class="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-6 gap-3">
      <StatCard label="Containers" value={containers.length} colorClass="text-base-content" />
      <StatCard
        label="Total CPU"
        value={formatPercent(percentOfCapacity(totalCpu, hostThreads))}
        sub={`${formatCores(totalCpu)} of ${hostThreads} host threads`}
        colorClass={usageColor(percentOfCapacity(totalCpu, hostThreads))}
      />
      <StatCard label="Total Memory" value={formatBytes(totalMemory)} colorClass="text-base-content" />
      <StatCard label="Total PIDs" value={totalPids} colorClass="text-base-content" />
      <StatCard
        label="Throttled"
        value={throttledCount}
        sub={throttledCount > 0 ? "CPU-capped" : "none"}
        colorClass={throttledCount > 0 ? "text-warning" : "text-base-content/50"}
      />
      <StatCard
        label="OOM Kills"
        value={totalOomKills}
        colorClass={totalOomKills > 0 ? "text-error" : "text-base-content/50"}
      />
      {#if runtime?.available}
        <StatCard
          label="Not Running"
          value={notRunning}
          sub="exist but stopped"
          colorClass={notRunning > 0 ? "text-base-content/70" : "text-base-content/50"}
        />
      {/if}
    </div>

    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <div class="flex items-baseline justify-between">
          <h3 class="card-title text-sm text-base-content/70">Containers</h3>
          {#if virt.cgroup_version}
            <span class="text-xs text-base-content/50">
              cgroup {virt.cgroup_version}
              {#if virt.runtimes && virt.runtimes.length > 0}
                &bull; {virt.runtimes.join(", ")}
              {/if}
            </span>
          {/if}
        </div>

        {#if containers.length === 0}
          <div class="py-4 space-y-1">
            <p class="text-sm text-base-content/70">
              {canObserve
                ? "No containers are running on this host."
                : "Container metrics are unavailable on this host."}
            </p>
            {#if canObserve && notRunning > 0}
              <!-- The common confusion: containers exist but are stopped, so
                   they have no processes, no cgroup and no metrics. Say so
                   rather than leaving an empty table to imply a fault. -->
              <p class="text-xs text-base-content/70">
                Docker knows about <span class="font-semibold">{runtime?.containers_total}</span>
                container{runtime?.containers_total === 1 ? "" : "s"} on this host,
                {notRunning} of which {notRunning === 1 ? "is" : "are"} not running.
              </p>
              <p class="text-xs text-base-content/50">
                A stopped container has no processes and no cgroup, so it reports
                no CPU, memory or I/O. Start one to see it here.
              </p>
            {:else if canObserve && !runtime?.available}
              <p class="text-xs text-base-content/50">
                Detection is working (cgroup {virt.cgroup_version}); nothing is running.
                Start the server with <span class="font-mono">--docker</span> to also see
                containers that exist but are stopped.
              </p>
            {:else if canObserve}
              <p class="text-xs text-base-content/50">
                Detection is working (cgroup {virt.cgroup_version}); there is
                simply nothing running.
              </p>
            {/if}
            {#if capability?.notes && capability.notes.length > 0}
              <ul class="text-xs text-base-content/50 list-disc list-inside">
                {#each capability.notes as note (note)}
                  <li>{note}</li>
                {/each}
              </ul>
            {/if}
          </div>
        {:else}
          <p class="text-xs text-base-content/40">Select a row for detail.</p>
          <DataTable
            rows={containers}
            columns={containerColumns}
            cells={containerCells}
            searchText={(c: ContainerInfo) => `${c.name} ${c.short_id} ${c.runtime}`}
            rowKey={(c: ContainerInfo) => c.id}
            initialSort="cpu"
            initialDirection="desc"
            searchPlaceholder="Search name, ID or runtime…"
            emptyMessage="No containers to show."
            onRowClick={(c: ContainerInfo) => toggle(c.id)}
            isSelected={(c: ContainerInfo) => c.id === selectedId}
          />
        {/if}
      </div>
    </div>

    <!-- Runtime storage -->
    {#if runtime != null && runtime.available}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <div class="flex flex-wrap items-baseline justify-between gap-2">
            <h3 class="card-title text-sm text-base-content/70">Images &amp; Storage</h3>
            <span class="text-xs text-base-content/50 font-mono">
              {runtime.engine} {runtime.version} &bull; {runtime.storage_driver} on
              {runtime.backing_filesystem} &bull; {runtime.root_dir}
            </span>
          </div>

          <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mt-2">
            <StatCard label="Images" value={runtime.images_total} colorClass="text-base-content" />
            <StatCard
              label="Image Layers"
              value={formatBytes(runtime.layers_bytes)}
              sub={`${runtime.unused_images} unused, ${runtime.dangling_images} dangling`}
              colorClass="text-base-content"
            />
            <StatCard
              label="Total Footprint"
              value={formatBytes(footprint)}
              sub="images + volumes + cache"
              colorClass="text-base-content"
            />
            <StatCard
              label="Reclaimable"
              value={formatBytes(reclaimable)}
              sub="prune would free"
              colorClass={reclaimable > 0 ? "text-warning" : "text-base-content/50"}
            />
          </div>

          <div class="overflow-x-auto mt-3">
            <table class="table table-sm">
              <thead>
                <tr>
                  <th>Type</th>
                  <th class="text-right">Count</th>
                  <th class="text-right">Size</th>
                  <th class="text-right">Reclaimable</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td>Images</td>
                  <td class="text-right">{runtime.images_total}</td>
                  <td class="text-right">{formatBytes(runtime.layers_bytes)}</td>
                  <td class="text-right">{formatBytes(runtime.reclaimable_bytes)}</td>
                </tr>
                <tr>
                  <td>Volumes</td>
                  <td class="text-right">{runtime.volumes_count}</td>
                  <td class="text-right">{formatBytes(runtime.volumes_bytes)}</td>
                  <td class="text-right">{formatBytes(runtime.volumes_reclaimable_bytes)}</td>
                </tr>
                <tr>
                  <td>Build cache</td>
                  <td class="text-right">{runtime.build_cache_entries}</td>
                  <td class="text-right">{formatBytes(runtime.build_cache_bytes)}</td>
                  <td class="text-right">{formatBytes(runtime.build_cache_reclaimable_bytes)}</td>
                </tr>
              </tbody>
            </table>
          </div>

          {#if runtime.images && runtime.images.length > 0}
            <div class="mt-3">
              <DataTable
                rows={runtime.images}
                columns={imageColumns}
                cells={imageCells}
                searchText={(i: ImageInfo) => `${i.short_id} ${(i.tags ?? []).join(" ")}`}
                rowKey={(i: ImageInfo) => i.id}
                title="Images"
                initialSort="size"
                initialDirection="desc"
                searchPlaceholder="Search tag or ID…"
                emptyMessage="No images on this host."
                truncated={runtime.images_truncated}
              />
              <p class="text-xs text-base-content/40 mt-1">
                Sizes include shared layers, so they do not sum to the total.
              </p>
            </div>
          {/if}
        </div>
      </div>
    {/if}

    <!-- Detail drawer -->
    {#if selected != null}
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4">
          <h3 class="card-title text-sm text-base-content/70">
            {selected.name}
            <span class="font-mono text-xs text-base-content/50">{selected.short_id}</span>
          </h3>

          <div class="grid grid-cols-2 sm:grid-cols-4 gap-4 mt-2 text-sm">
            <div>
              <p class="text-xs text-base-content/50">CPU throttling</p>
              <p class="font-mono {throttleColor(selected.throttled_percent)}">
                {selected.nr_throttled} / {selected.nr_periods} periods
              </p>
              <p class="text-xs text-base-content/40">
                {(selected.throttled_usec / 1e6).toFixed(1)}s stopped
              </p>
            </div>
            <div>
              <p class="text-xs text-base-content/50">CPU affinity</p>
              <p class="font-mono">{selected.cpu_set || "–"}</p>
            </div>
            <div>
              <p class="text-xs text-base-content/50">Anonymous / cache</p>
              <p class="font-mono">
                {formatBytes(selected.anon_bytes)} / {formatBytes(selected.file_bytes)}
              </p>
            </div>
            <div>
              <p class="text-xs text-base-content/50">Swap</p>
              <p class="font-mono">{formatBytes(selected.swap_bytes)}</p>
            </div>

            <div>
              <p class="text-xs text-base-content/50">OOM kills / events</p>
              <p class="font-mono {selected.oom_kills > 0 ? 'text-error' : ''}">
                {selected.oom_kills} / {selected.oom_events}
              </p>
            </div>
            <div>
              <p class="text-xs text-base-content/50">Major faults</p>
              <p class="font-mono">{selected.major_faults.toLocaleString()}</p>
            </div>
            <div>
              <p class="text-xs text-base-content/50">PIDs peak / limit</p>
              <p class="font-mono">
                {selected.pids_peak} / {selected.pids_max > 0 ? selected.pids_max : "∞"}
              </p>
            </div>
            <div>
              <p class="text-xs text-base-content/50">IOPS read / write</p>
              <p class="font-mono">
                {selected.read_iops.toLocaleString()} / {selected.write_iops.toLocaleString()}
              </p>
            </div>
          </div>

          <div class="mt-3">
            <p class="text-xs text-base-content/50 mb-1">
              Pressure stall (share of the last 10s spent waiting)
            </p>
            <div class="grid grid-cols-3 gap-4 text-sm">
              <div>
                <span class="text-xs text-base-content/50">CPU</span>
                <p class="font-mono {pressureColor(selected.cpu_pressure)}">
                  {formatPercent(selected.cpu_pressure)}
                </p>
              </div>
              <div>
                <span class="text-xs text-base-content/50">Memory</span>
                <p class="font-mono {pressureColor(selected.memory_pressure)}">
                  {formatPercent(selected.memory_pressure)}
                </p>
              </div>
              <div>
                <span class="text-xs text-base-content/50">I/O</span>
                <p class="font-mono {pressureColor(selected.io_pressure)}">
                  {formatPercent(selected.io_pressure)}
                </p>
              </div>
            </div>
          </div>

          {#if selected.command}
            <p class="text-xs text-base-content/40 mt-3 break-all font-mono">
              {selected.command}
            </p>
          {/if}
        </div>
      </div>
    {/if}
  {:else}
    <div class="flex justify-center items-center h-32">
      <span class="loading loading-spinner loading-md text-primary"></span>
    </div>
  {/if}
</div>

{#snippet imageCells(img: ImageInfo)}
  <td class="font-mono text-xs">{img.short_id}</td>
  <td class={img.dangling ? "text-base-content/50 italic" : ""}>
    {img.tags && img.tags.length > 0 ? img.tags[0] : "<none>"}
  </td>
  <td class="text-right">{formatBytes(img.size_bytes)}</td>
  <td class="text-right text-base-content/60">{formatBytes(img.shared_size_bytes)}</td>
  <td class="text-right {img.in_use ? '' : 'text-warning'}">
    {img.in_use ? img.containers : "–"}
  </td>
  <td class="text-right text-base-content/60">{imageAge(img.created_unix)}</td>
{/snippet}

{#snippet containerCells(c: ContainerInfo)}
  <td class="font-medium {selectedId === c.id ? 'text-primary' : ''}">{c.name}</td>
  <td class="font-mono text-xs text-base-content/60">{c.short_id}</td>
  <td class="text-base-content/70">{c.runtime}</td>
  <td class="text-right {usageColor(c.cpu_percent)}">{formatPercent(c.cpu_percent)}</td>
  <td class="text-right text-base-content/60">
    {c.cpu_limit_cores > 0 ? `${c.cpu_limit_cores} core` : "–"}
  </td>
  <td class="text-right {throttleColor(c.throttled_percent)}">
    {c.throttled_percent > 0 ? formatPercent(c.throttled_percent) : "–"}
  </td>
  <td class="text-right">
    {formatBytes(c.memory_bytes)}
    {#if c.memory_limit_bytes > 0}
      <span class="text-base-content/50">/ {formatBytes(c.memory_limit_bytes)}</span>
    {/if}
  </td>
  <td class="text-right text-base-content/60">{formatBytes(c.memory_peak_bytes)}</td>
  <td class="text-right">{c.pids}</td>
  <td class="text-right">{formatBytesRate(c.read_bytes_rate)}</td>
  <td class="text-right">{formatBytesRate(c.write_bytes_rate)}</td>
  <td class="text-right text-base-content/60">{formatDuration(c.uptime_seconds)}</td>
{/snippet}
