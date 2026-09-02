<script lang="ts">
  // Fleet overview: one card per peer, worst status first, with search and
  // paging so the view stays usable as the fleet grows.
  //
  // The pipeline is filter -> sort -> page, and it lives here rather than in
  // the store: the view must not depend on the order the server happened to
  // send, and keeping the three steps together makes the ordering rule visible
  // to whoever next edits the list.
  import {
    nodeStatusBadgeClass,
    nodeStatusBorderClass,
  } from "$lib/alerts";
  import { compareNodeNames } from "$lib/nodeorder";
  import {
    formatAge,
    formatBytesRate,
    formatDuration,
    formatPercent,
    formatTemp,
    formatWatts,
    usageColor,
  } from "$lib/format";
  import { readStored, writeStored } from "$lib/storage";
  import type { FleetNode } from "$lib/types";
  import { monitor } from "$stores/monitor.svelte";

  interface Props {
    /** Called with a peer name, or null to return to the local node. */
    onSelect: (name: string | null) => void;
  }

  let { onSelect }: Props = $props();

  const PAGE_SIZE_OPTIONS = [25, 50, 100, 250];
  const DEFAULT_PAGE_SIZE = 25;
  const PAGE_SIZE_KEY = "sysmon.fleet.pageSize";
  const SEARCH_KEY = "sysmon.fleet.search";

  /** A stored page size is only honoured if it is still one of the options. */
  function loadPageSize(): number {
    const raw = readStored(PAGE_SIZE_KEY);
    const parsed = raw == null ? Number.NaN : Number.parseInt(raw, 10);
    return PAGE_SIZE_OPTIONS.includes(parsed) ? parsed : DEFAULT_PAGE_SIZE;
  }

  const allNodes = $derived(monitor.fleetNodes);
  const localVersion = $derived(monitor.localVersion);
  const selectedNode = $derived(monitor.selectedNode);

  let query = $state(readStored(SEARCH_KEY) ?? "");
  let perPage = $state(loadPageSize());
  let page = $state(0);

  // Persist the viewer's preferences. Both writes are guarded inside
  // writeStored, so a private window costs the preference and nothing else.
  $effect(() => {
    writeStored(SEARCH_KEY, query);
  });
  $effect(() => {
    writeStored(PAGE_SIZE_KEY, String(perPage));
  });

  // --- filter -> sort -> page ----------------------------------------------

  const filtered = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (q === "") return allNodes;
    return allNodes.filter(
      (n) =>
        n.name.toLowerCase().includes(q) ||
        n.hostname.toLowerCase().includes(q)
    );
  });

  // Alphabetical by name (owner ruling 2026-09-01). Names are unique within a
  // fleet, so this is a total order: nodes cannot reshuffle between polls.
  //
  // This used to sort worst-status-first so problems surfaced on page one. That
  // reads as "not sorted" to anyone looking for a host, because a warning node
  // sorts ahead of a healthy one whose name comes first alphabetically -- with
  // fs, gpu-host and omega listed above ci-node-1, the list looks shuffled. A
  // fleet view is used far more often to FIND a named host than to discover an
  // unknown fault, and the faults are not hidden by this: the summary line
  // above the table counts unreachable and degraded nodes whatever the order,
  // and the search box narrows to a host directly.
  // compareNodeNames rather than localeCompare: the backend sorts this list
  // too, and localeCompare cannot agree with it. See $lib/nodeorder.
  const sorted = $derived.by(() =>
    [...filtered].sort((a, b) => compareNodeNames(a.name, b.name))
  );

  const pageCount = $derived(Math.max(1, Math.ceil(sorted.length / perPage)));
  // Clamping on read as well as resetting on change: a node leaving the fleet
  // can shrink the list under a page the user is already sitting on.
  const safePage = $derived(Math.min(page, pageCount - 1));
  const start = $derived(safePage * perPage);
  const visible = $derived(sorted.slice(start, start + perPage));

  const rangeLabel = $derived(
    sorted.length === 0
      ? "0 of 0"
      : `${start + 1}–${Math.min(start + perPage, sorted.length)} of ${sorted.length}`
  );

  function goTo(target: number) {
    page = Math.min(Math.max(target, 0), pageCount - 1);
  }

  function clearSearch() {
    query = "";
    page = 0;
  }

  // --- whole-fleet rollups --------------------------------------------------

  /**
   * Totals are computed over every node, never over the visible page: a
   * summary that silently described one page would be worse than no summary.
   */
  const totals = $derived.by(() => {
    let unreachable = 0;
    let critical = 0;
    let warning = 0;
    let healthy = 0;
    let criticalAlerts = 0;
    let warningAlerts = 0;
    let drifted = 0;

    for (const node of allNodes) {
      switch (node.status) {
        case "unreachable":
          unreachable++;
          break;
        case "critical":
          critical++;
          break;
        case "warning":
          warning++;
          break;
        default:
          healthy++;
      }
      criticalAlerts += node.critical_count;
      warningAlerts += node.warning_count;
      if (hasVersionDrift(node)) drifted++;
    }

    return {
      nodes: allNodes.length,
      unreachable,
      critical,
      warning,
      healthy,
      criticalAlerts,
      warningAlerts,
      drifted,
    };
  });

  // --- per-node helpers -----------------------------------------------------

  // Load average per core is the comparable figure across nodes of different
  // sizes: 1.0 means "exactly saturated" on any of them.
  function loadColor(loadPerCore: number): string {
    if (loadPerCore >= 1) return "text-error";
    if (loadPerCore >= 0.7) return "text-warning";
    return "text-success";
  }

  /** A peer running a different build than this node is worth flagging. */
  function hasVersionDrift(node: FleetNode): boolean {
    return (
      localVersion !== "" && node.version !== "" && node.version !== localVersion
    );
  }

  /**
   * The count badges already carry the status word, so rendering the plain
   * status badge alongside them produced a visible duplicate — "warning" next
   * to "3 warning". Show the bare status only when no count badge says it.
   */
  function showsStatusBadge(node: FleetNode): boolean {
    if (node.status === "critical") return node.critical_count === 0;
    if (node.status === "warning") return node.warning_count === 0;
    return true;
  }

  // Selection is reported upward rather than applied here: the parent both
  // switches the store and moves the user off this tab, and one owner for
  // that pair keeps the two from drifting apart.
  function select(node: FleetNode): void {
    onSelect(node.self ? null : node.name);
  }
</script>

<div class="space-y-4">
  {#if totals.nodes === 0}
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <p class="text-sm text-base-content/70">No fleet members reported.</p>
        <p class="text-xs text-base-content/50">
          Fleet mode is enabled but the peer list is empty — check the
          <span class="font-mono">fleet.nodes</span> section of the configuration.
        </p>
      </div>
    </div>
  {:else}
    <!-- Rollup + controls -->
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-3 gap-3">
        <!-- Whole-fleet rollup, explicitly labelled so it is never mistaken
             for a description of the current page. -->
        <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
          <span class="font-semibold text-base-content/70">
            Across all {totals.nodes} node{totals.nodes === 1 ? "" : "s"}:
          </span>
          {#if totals.unreachable > 0}
            <span class="badge badge-neutral badge-sm">
              {totals.unreachable} unreachable
            </span>
          {/if}
          {#if totals.critical > 0}
            <span class="badge badge-error badge-sm">
              {totals.critical} critical
            </span>
          {/if}
          {#if totals.warning > 0}
            <span class="badge badge-warning badge-sm">
              {totals.warning} warning
            </span>
          {/if}
          <span class="badge badge-success badge-sm">
            {totals.healthy} healthy
          </span>
          <span class="text-base-content/50">
            {totals.criticalAlerts} critical / {totals.warningAlerts} warning alerts
          </span>
          {#if totals.drifted > 0}
            <span
              class="badge badge-warning badge-sm"
              title="Nodes running a different go-sysmon build than {localVersion}"
            >
              {totals.drifted} version drift
            </span>
          {/if}
        </div>

        <!-- Search + page size -->
        <div class="flex flex-wrap items-center gap-2">
          <input
            type="search"
            class="input input-bordered input-sm w-64 max-w-full"
            placeholder="Search name or hostname…"
            aria-label="Search fleet nodes"
            bind:value={query}
            oninput={() => (page = 0)}
          />
          <select
            class="select select-bordered select-sm"
            aria-label="Nodes per page"
            bind:value={perPage}
            onchange={() => (page = 0)}
          >
            {#each PAGE_SIZE_OPTIONS as size (size)}
              <option value={size}>{size} / page</option>
            {/each}
          </select>
          {#if query.trim() !== ""}
            <button type="button" class="btn btn-ghost btn-sm" onclick={clearSearch}>
              Clear search
            </button>
          {/if}
          <span class="text-xs text-base-content/60 ml-auto">
            Showing {rangeLabel}
            {#if sorted.length !== totals.nodes}
              <span class="text-base-content/40">
                (filtered from {totals.nodes})
              </span>
            {/if}
          </span>
        </div>
      </div>
    </div>

    {#if sorted.length === 0}
      <!-- Distinct from an empty fleet: there are nodes, the filter hid them. -->
      <div class="card bg-base-200 shadow-sm">
        <div class="card-body p-4 items-start gap-2">
          <p class="text-sm text-base-content/70">
            No node matches “{query.trim()}”.
          </p>
          <button type="button" class="btn btn-sm" onclick={clearSearch}>
            Clear search
          </button>
        </div>
      </div>
    {:else}
      <div class="grid grid-cols-1 xl:grid-cols-2 gap-4">
        {#each visible as node (node.name)}
          {@const isSelected = selectedNode === node.name}
          {@const drift = hasVersionDrift(node)}
          <button
            type="button"
            class="card bg-base-200 shadow-sm text-left w-full transition-colors
              {nodeStatusBorderClass(node.status)}
              {isSelected ? 'ring-2 ring-primary' : ''}
              {node.reachable ? 'hover:bg-base-100 cursor-pointer' : 'opacity-70 cursor-not-allowed'}"
            disabled={!node.reachable}
            onclick={() => select(node)}
          >
            <div class="card-body p-4 gap-2">
              <!-- Identity -->
              <div class="flex flex-wrap items-center justify-between gap-2">
                <div class="min-w-0">
                  <div class="flex items-center gap-2 flex-wrap">
                    <h3 class="font-semibold truncate">{node.name}</h3>
                    {#if node.self}
                      <span class="badge badge-outline badge-xs">this node</span>
                    {/if}
                    {#if showsStatusBadge(node)}
                      <span class="badge {nodeStatusBadgeClass(node.status)} badge-sm">
                        {node.status}
                      </span>
                    {/if}
                    {#if node.critical_count > 0}
                      <span class="badge badge-error badge-sm">
                        {node.critical_count} critical
                      </span>
                    {/if}
                    {#if node.warning_count > 0}
                      <span class="badge badge-warning badge-sm">
                        {node.warning_count} warning
                      </span>
                    {/if}
                  </div>
                  <p class="text-xs text-base-content/60 truncate">
                    {node.hostname || node.url}
                    {#if node.uptime_seconds > 0}
                      &bull; up {formatDuration(node.uptime_seconds)}
                    {/if}
                  </p>
                </div>
                <div class="text-right text-xs shrink-0">
                  {#if node.version}
                    <div class={drift ? "text-warning font-medium" : "text-base-content/50"}>
                      {node.version}
                      {#if drift}
                        <span
                          class="badge badge-warning badge-xs ml-1"
                          title="This node runs a different go-sysmon build than {localVersion}"
                        >
                          drift
                        </span>
                      {/if}
                    </div>
                  {/if}
                  <div class="text-base-content/40">
                    updated {formatAge(node.age_seconds)} ago
                  </div>
                </div>
              </div>

              {#if !node.reachable}
                <!-- An unreachable node has no metrics worth showing; say why
                     instead of drawing a row of zeroes. -->
                <div class="alert alert-error alert-soft py-2 text-sm">
                  <span class="font-semibold">Unreachable</span>
                  <span class="break-all opacity-80">
                    {node.error || "no response from this node"}
                  </span>
                </div>
              {:else}
                <!-- Core utilisation -->
                <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-sm">
                  <div>
                    <p class="text-xs text-base-content/50">CPU</p>
                    <p class="font-mono {usageColor(node.cpu_percent)}">
                      {formatPercent(node.cpu_percent)}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Memory</p>
                    <p class="font-mono {usageColor(node.memory_percent)}">
                      {formatPercent(node.memory_percent)}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Load / core</p>
                    <p class="font-mono {loadColor(node.load_per_core)}">
                      {node.load_per_core.toFixed(2)}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">Worst filesystem</p>
                    <p class="font-mono {usageColor(node.worst_disk_percent)}">
                      {formatPercent(node.worst_disk_percent)}
                    </p>
                  </div>
                </div>

                <!-- Storage health, thermals and traffic -->
                <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-sm">
                  <div>
                    <p class="text-xs text-base-content/50">RAID</p>
                    <p class="font-mono {node.raid_degraded ? 'text-error' : 'text-success'}">
                      {node.raid_degraded ? "degraded" : "ok"}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/50">SMART unhealthy</p>
                    <p class="font-mono {node.smart_unhealthy > 0 ? 'text-error' : 'text-success'}">
                      {node.smart_unhealthy}
                    </p>
                  </div>
                  {#if node.swap_percent > 0}
                    <div>
                      <p class="text-xs text-base-content/50">Swap</p>
                      <p class="font-mono {usageColor(node.swap_percent)}">
                        {formatPercent(node.swap_percent)}
                      </p>
                    </div>
                  {/if}
                  {#if node.max_temp_c > 0}
                    <div>
                      <p class="text-xs text-base-content/50">Max temp</p>
                      <p class="font-mono">{formatTemp(node.max_temp_c)}</p>
                    </div>
                  {/if}
                  {#if node.power_w > 0}
                    <div>
                      <p class="text-xs text-base-content/50">Power</p>
                      <p class="font-mono">{formatWatts(node.power_w)}</p>
                    </div>
                  {/if}
                  <div>
                    <p class="text-xs text-base-content/50">Network</p>
                    <p class="font-mono text-xs">
                      <span class="text-info">↓ {formatBytesRate(node.net_rx_rate)}</span>
                      <span class="text-success ml-1">↑ {formatBytesRate(node.net_tx_rate)}</span>
                    </p>
                  </div>
                </div>
              {/if}
            </div>
          </button>
        {/each}
      </div>

      <!-- Pagination -->
      <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-base-content/60">
        <span>Showing {rangeLabel}</span>

        {#if pageCount > 1}
          <div class="join">
            <button
              class="join-item btn btn-xs"
              onclick={() => goTo(0)}
              disabled={safePage === 0}
              aria-label="First page"
            >
              «
            </button>
            <button
              class="join-item btn btn-xs"
              onclick={() => goTo(safePage - 1)}
              disabled={safePage === 0}
              aria-label="Previous page"
            >
              ‹
            </button>
            <span class="join-item btn btn-xs btn-disabled">
              {safePage + 1} / {pageCount}
            </span>
            <button
              class="join-item btn btn-xs"
              onclick={() => goTo(safePage + 1)}
              disabled={safePage >= pageCount - 1}
              aria-label="Next page"
            >
              ›
            </button>
            <button
              class="join-item btn btn-xs"
              onclick={() => goTo(pageCount - 1)}
              disabled={safePage >= pageCount - 1}
              aria-label="Last page"
            >
              »
            </button>
          </div>
        {/if}
      </div>

      <p class="text-xs text-base-content/40">
        Select a node to view its full dashboard in the other tabs.
      </p>
    {/if}
  {/if}
</div>
