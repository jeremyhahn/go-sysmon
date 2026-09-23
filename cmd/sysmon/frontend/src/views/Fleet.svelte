<script lang="ts">
  // Fleet overview: the whole-fleet roll-up (#46), then one card per node in
  // name order, with search and paging so the view stays usable as the fleet
  // grows.
  //
  // The pipeline is filter -> sort -> page, and it lives here rather than in
  // the store: the view must not depend on the order the server happened to
  // send, and keeping the three steps together makes the ordering rule visible
  // to whoever next edits the list.
  import {
    nodeStatusBadgeClass,
    nodeStatusBorderClass,
  } from "$lib/alerts";
  import FleetRollup from "$components/FleetRollup.svelte";
  import { compareNodeNames } from "$lib/nodeorder";
  import { tempSourceLabel } from "$lib/rollup";
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
  const rollup = $derived(monitor.fleetRollup);
  const localVersion = $derived(monitor.localVersion);
  const localCommit = $derived(monitor.localCommit);
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

  // --- per-node helpers -----------------------------------------------------

  // Load average per core is the comparable figure across nodes of different
  // sizes: 1.0 means "exactly saturated" on any of them.
  function loadColor(loadPerCore: number): string {
    if (loadPerCore >= 1) return "text-error";
    if (loadPerCore >= 0.7) return "text-warning";
    return "text-success";
  }

  /**
   * A peer running a different build than this node is worth showing.
   *
   * The server decides this and sends `build_match`; it is the only party that
   * knows its own commit, and comparing `version` here answered "no drift"
   * always -- the release string is the same word on every build this project
   * has ever produced (#68). An older server sends no field, which reads as
   * unknown and shows nothing rather than guessing.
   *
   * Divergence is reported, never counted as a fault: mid-rollout every node
   * differs until it restarts, and colouring the fleet amber for the length of
   * a deploy teaches operators to ignore the colour.
   */
  function hasBuildDrift(node: FleetNode): boolean {
    return node.build_match === "different";
  }

  /** What to show for a node's build: the commit if it stated one. */
  function buildLabel(node: FleetNode): string {
    return node.git_commit && node.git_commit !== "unknown"
      ? node.git_commit
      : "";
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

  /**
   * Swap paging for a node's card (#134). Occupancy alone cannot say whether a
   * node is paging -- full swap that nothing touches is not pressure -- so the
   * rate goes beside it, and a rate that could not be measured says so rather
   * than reading as "no paging".
   */
  function pagingLabel(node: FleetNode): string {
    if (node.swap_rate_available !== true) return "paging unknown";
    const out = node.swap_out_rate ?? 0;
    const into = node.swap_in_rate ?? 0;
    if (out === 0 && into === 0) return "no paging";
    return `out ${formatBytesRate(out)}, in ${formatBytesRate(into)}`;
  }

  /** A node picked from the roll-up's attention list: open it if it can be
   * opened, otherwise narrow the list to its card, which says why not. */
  function pick(name: string): void {
    const node = allNodes.find((n) => n.name === name);
    if (node?.reachable) {
      select(node);
      return;
    }
    query = name;
    page = 0;
  }

  // Selection is reported upward rather than applied here: the parent both
  // switches the store and moves the user off this tab, and one owner for
  // that pair keeps the two from drifting apart.
  function select(node: FleetNode): void {
    onSelect(node.self ? null : node.name);
  }
</script>

<div class="space-y-4">
  {#if allNodes.length === 0}
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-4">
        <p class="text-sm text-base-content/70">No fleet members reported.</p>
        <p class="text-xs text-base-content/60">
          Fleet mode is enabled but the peer list is empty — check the
          <span class="font-mono">fleet.nodes</span> section of the configuration.
        </p>
      </div>
    </div>
  {:else}
    <!-- Rollup + controls -->
    <div class="card bg-base-200 shadow-sm">
      <div class="card-body p-3 gap-3">
        <!-- The whole fleet, computed by the server over every node --
             never over the visible page, which a summary silently describing
             one page would be worse than no summary at all. -->
        {#if rollup}
          <FleetRollup {rollup} onPick={pick} />
        {:else}
          <p class="text-xs text-base-content/60">
            This server did not send a fleet roll-up; it predates the fleet
            summary.
          </p>
        {/if}

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
            {#if sorted.length !== allNodes.length}
              <span class="text-base-content/60">
                (filtered from {allNodes.length})
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
          {@const drift = hasBuildDrift(node)}
          {@const build = buildLabel(node)}
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
                  {#if node.version || build}
                    <div class="text-base-content/60">
                      {node.version}
                      {#if build}
                        <!-- The commit is the only part that varies between
                             builds; the version beside it has never moved. -->
                        <span class="font-mono" title={node.build_date ?? ""}>
                          {build}
                        </span>
                      {/if}
                      {#if drift}
                        <span
                          class="badge badge-neutral badge-xs ml-1"
                          title="This node runs a go-sysmon build other than {localCommit ||
                            localVersion}"
                        >
                          other build
                        </span>
                      {/if}
                    </div>
                  {/if}
                  <div class="text-base-content/60">
                    updated {formatAge(node.age_seconds)} ago
                  </div>
                  {#if node.source === "snapshot"}
                    <!-- Marked, not hidden: a peer too old to serve a summary
                         is polled for its whole snapshot, which costs the
                         aggregator what the summary exists to save. -->
                    <span
                      class="badge badge-neutral badge-xs"
                      title="This node's build predates /api/summary, so the aggregator polls its whole snapshot. Upgrade it to reduce the aggregator's load."
                      data-testid="snapshot-source"
                    >
                      polled by full snapshot
                    </span>
                  {/if}
                </div>
              </div>

              {#if !node.reachable}
                <!-- An unreachable node has no metrics worth showing; say why
                     instead of drawing a row of zeroes. -->
                <div class="alert alert-error alert-soft py-2 text-sm">
                  <!-- How long, not just that it is. A node restarting for five
                       seconds and one gone for two days produced the same word
                       and the same red panel, so every deploy repainted a
                       healthy node as a dead one (#69). -->
                  <span class="font-semibold">
                    {#if node.age_seconds > 0}
                      Unreachable for {formatAge(node.age_seconds)}
                    {:else}
                      Unreachable, never reported
                    {/if}
                  </span>
                  <span class="break-all opacity-80">
                    {node.error || "no response from this node"}
                  </span>
                </div>
              {:else}
                <!-- Core utilisation -->
                <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-sm">
                  <div>
                    <p class="text-xs text-base-content/60">CPU</p>
                    <p class="font-mono {usageColor(node.cpu_percent)}">
                      {formatPercent(node.cpu_percent)}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/60">Memory</p>
                    <p class="font-mono {usageColor(node.memory_percent)}">
                      {formatPercent(node.memory_percent)}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/60">Load / core</p>
                    <p class="font-mono {loadColor(node.load_per_core)}">
                      {node.load_per_core.toFixed(2)}
                    </p>
                  </div>
                  <div>
                    <p class="text-xs text-base-content/60">Worst filesystem</p>
                    <p class="font-mono {usageColor(node.worst_disk_percent)}">
                      {formatPercent(node.worst_disk_percent)}
                    </p>
                  </div>
                </div>

                <!-- Storage health, thermals and traffic -->
                <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-sm">
                  <!-- Omitted when there is nothing to report on, the same
                       treatment swap, temperature and power already use below.
                       A green "ok" for a host with no arrays is a health
                       statement about hardware it does not have, and a green
                       "0" for a host whose disks report no SMART says every
                       disk is well when none of them answered.

                       A FAULT IS ALWAYS SHOWN, even with no denominator. The
                       assets are embedded so this page and its /api/fleet
                       normally come from one binary -- but a tab left open
                       across a rollback would hold this bundle against an
                       older server that sends no counts, and gating on the
                       count alone would then hide a real degraded array. A
                       missing denominator is a reason to say less about a
                       healthy host, never a reason to stay quiet about a
                       broken one. -->
                  {#if node.raid_arrays > 0 || node.raid_unhealthy > 0 || node.raid_degraded}
                    <div>
                      <p class="text-xs text-base-content/60">RAID</p>
                      <!-- "unhealthy" rather than "degraded": an inactive,
                           suspended, broken or unreadable array is not
                           degraded -- it is not serving I/O at all -- and it
                           raises a CRITICAL alert while this cell said ok. -->
                      <p
                        class="font-mono {node.raid_unhealthy > 0 || node.raid_degraded
                          ? 'text-error'
                          : 'text-success'}"
                      >
                        {#if node.raid_unhealthy > 0}
                          {node.raid_arrays > 0
                            ? `${node.raid_unhealthy} of ${node.raid_arrays} unhealthy`
                            : `${node.raid_unhealthy} unhealthy`}
                        {:else if node.raid_degraded}
                          degraded
                        {:else}
                          ok
                        {/if}
                      </p>
                      <!-- The denominator, on the paths where the state alone
                           does not carry one. "ok" says the arrays are well
                           and cannot say whether that is one array or five,
                           which is #44's argument applied to its own sibling
                           cell: a health figure with no denominator cannot be
                           read. It sits beside the state as evidence rather
                           than above it as a claim, the same treatment the
                           SMART cell below uses. Omitted next to "N of M
                           unhealthy", which already carries it. -->
                      <!-- `!(x > 0)` rather than `x === 0`, so the count also
                           shows for a payload that omits raid_unhealthy
                           entirely: `undefined === 0` is false, and the cell
                           would silently lose its denominator on exactly the
                           older-server path the fault branch above exists to
                           handle. It mirrors the value's own else-branch. -->
                      {#if !(node.raid_unhealthy > 0) && node.raid_arrays > 0}
                        <p class="text-xs text-base-content/60">
                          {node.raid_arrays === 1 ? "1 array" : `${node.raid_arrays} arrays`}
                        </p>
                      {/if}
                    </div>
                  {/if}
                  {#if node.smart_capable > 0 || node.smart_unhealthy > 0}
                    <div>
                      <!-- The label is neutral and the STATE is the value, the
                           same shape as the RAID cell above. It read "SMART
                           unhealthy" over "0 of 6", which asserts a fault in
                           the heading about a host whose six devices all
                           passed -- and a roll-up is read by its headings. The
                           number was right and green the whole time; nobody
                           looked at what it sat under. -->
                      <p class="text-xs text-base-content/60">SMART</p>
                      <p class="font-mono {node.smart_unhealthy > 0 ? 'text-error' : 'text-success'}">
                        <!-- "N of M" needs an M. Without one, report the fault
                             plainly rather than inventing "1 of 0". -->
                        {#if node.smart_unhealthy > 0}
                          {node.smart_capable > 0
                            ? `${node.smart_unhealthy} of ${node.smart_capable} unhealthy`
                            : `${node.smart_unhealthy} unhealthy`}
                        {:else}
                          ok
                        {/if}
                      </p>
                      <!-- The denominator survives on the healthy path, because
                           dropping it would undo #44: "ok" alone cannot say
                           whether six devices were checked or none. It sits
                           beside the state as evidence rather than above it as
                           a claim. -->
                      {#if node.smart_unhealthy === 0 && node.smart_capable > 0}
                        <p class="text-xs text-base-content/60">
                          {node.smart_capable} checked
                        </p>
                      {/if}
                    </div>
                  {/if}
                  <!-- Shown whenever there is swap to speak of, and whenever
                       paging could not be measured: an unknown is a thing to
                       say, not a reason to leave the cell out. -->
                  {#if node.swap_percent > 0 || (node.swap_out_rate ?? 0) > 0 || (node.swap_in_rate ?? 0) > 0 || node.swap_rate_available !== true}
                    <div data-testid="swap-cell">
                      <p class="text-xs text-base-content/60">Swap</p>
                      <!-- Occupancy is not coloured as usage: full swap that
                           nothing touches is not pressure (#134). The paging
                           line beneath is what says whether it is. -->
                      <p class="font-mono">{formatPercent(node.swap_percent)}</p>
                      <p
                        class="text-xs {(node.swap_out_rate ?? 0) > 0
                          ? 'text-warning'
                          : 'text-base-content/60'}"
                      >
                        {pagingLabel(node)}
                      </p>
                    </div>
                  {/if}
                  {#if node.max_temp_c > 0}
                    <div>
                      <p class="text-xs text-base-content/60">Max temp</p>
                      <p class="font-mono" data-testid="max-temp">
                        {formatTemp(node.max_temp_c)}
                        {#if tempSourceLabel(node.max_temp_source)}
                          <span class="text-xs text-base-content/60">{tempSourceLabel(node.max_temp_source)}</span>
                        {/if}
                      </p>
                    </div>
                  {/if}
                  {#if node.power_reported || node.power_w > 0}
                    <div>
                      <p class="text-xs text-base-content/60">Power</p>
                      <p class="font-mono">{formatWatts(node.power_w)}</p>
                    </div>
                  {/if}
                  <!-- Measured at the node's physical devices; a node with none
                       (a container in its own namespace) has no figure, not a
                       zero. `!== false` so an older server, which sends no
                       net_reported, still shows its figure. -->
                  {#if node.net_reported !== false}
                    <div data-testid="net-cell">
                      <p class="text-xs text-base-content/60">Network</p>
                      <p class="font-mono text-xs">
                        <span class="text-info">↓ {formatBytesRate(node.net_rx_rate)}</span>
                        <span class="text-success ml-1">↑ {formatBytesRate(node.net_tx_rate)}</span>
                      </p>
                    </div>
                  {/if}
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

      <p class="text-xs text-base-content/60">
        Select a node to view its full dashboard in the other tabs.
      </p>
    {/if}
  {/if}
</div>
