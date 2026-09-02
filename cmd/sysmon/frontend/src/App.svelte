<script lang="ts">
  import Overview from "./views/Overview.svelte";
  import Fleet from "./views/Fleet.svelte";
  import History from "./views/History.svelte";
  import AlertBell from "./components/AlertBell.svelte";
  import SettingsDialog from "./components/SettingsDialog.svelte";
  import ThemeToggle from "./components/ThemeToggle.svelte";
  import CpuPanel from "./components/CpuPanel.svelte";
  import GpuPanel from "./components/GpuPanel.svelte";
  import MemoryPanel from "./components/MemoryPanel.svelte";
  import DiskPanel from "./components/DiskPanel.svelte";
  import NetworkPanel from "./components/NetworkPanel.svelte";
  import ContainerPanel from "./components/ContainerPanel.svelte";
  import VirtPanel from "./components/VirtPanel.svelte";
  import HostPanel from "./components/HostPanel.svelte";
  import { severityDotClass, worstSeverity } from "$lib/alerts";
  import { monitor } from "./stores/monitor.svelte";
  import { fetchCatalog } from "$lib/history";
  import type { HistoryMetric } from "$lib/types";
  import { onMount } from "svelte";

  const connected = $derived(monitor.connected);
  const snapshot = $derived(monitor.snapshot);
  const currentInterval = $derived(monitor.intervalMs);
  const fleetEnabled = $derived(monitor.fleetEnabled);
  const selectedNode = $derived(monitor.selectedNode);
  const peerFreshness = $derived(monitor.peerFreshness);

  // How long ago the selected peer last answered. Rounded to whole units: the
  // point is "minutes, not seconds", and false precision on a stale reading
  // invites more trust than it deserves.
  const staleAge = $derived.by(() => {
    const secs = peerFreshness?.ageSeconds ?? 0;
    if (secs < 60) return `${Math.round(secs)}s old`;
    if (secs < 3600) return `${Math.round(secs / 60)}m old`;
    return `${Math.round(secs / 3600)}h old`;
  });
  const alertsByCategory = $derived(monitor.alertsByCategory);

  let buildVersion = $state("");

  // The history catalog decides whether the History tab exists at all. A node
  // with no prometheus.query_url answers enabled:false, and the tab is left out
  // rather than offering a picker with nothing behind it. Asked once, on load:
  // it changes only when the config file does.
  let historyMetrics = $state<HistoryMetric[]>([]);
  let historyEnabled = $state(false);

  onMount(async () => {
    try {
      const r = await fetch("/api/version");
      if (r.ok) {
        const v = await r.json();
        buildVersion = v.version ?? "";
      }
    } catch {
      /* footer version is best-effort */
    }

    try {
      const catalog = await fetchCatalog();
      historyEnabled = catalog.enabled && catalog.metrics.length > 0;
      historyMetrics = catalog.metrics ?? [];
    } catch {
      // An older node, or one that cannot answer: no history, no tab. The live
      // dashboard is unaffected, which is the point of it being a separate
      // request from the event stream.
    }
  });

  const intervalOptions = [
    { value: 250, label: "250ms" },
    { value: 500, label: "500ms" },
    { value: 1000, label: "1s" },
    { value: 5000, label: "5s" },
    { value: 10000, label: "10s" },
    { value: 15000, label: "15s" },
    { value: 30000, label: "30s" },
    { value: 60000, label: "60s" },
  ];

  function onIntervalChange(e: Event) {
    const target = e.target as HTMLSelectElement;
    monitor.setInterval(Number(target.value));
  }

  type Tab =
    | "fleet"
    | "overview"
    | "history"
    | "host"
    | "cpu"
    | "gpu"
    | "memory"
    | "storage"
    | "network"
    | "virt"
    | "containers";

  let activeTab = $state<Tab>("overview");

  // Plain locals, not $state: they gate one-time behaviour and must not
  // re-trigger the effect that reads them.
  let userPickedTab = false;
  let appliedFleetDefault = false;

  // Tab ids double as alert categories, which is what lets an alert row
  // navigate and a tab wear a severity dot without a lookup table.
  const baseTabs: { id: Tab; label: string }[] = [
    { id: "overview", label: "Overview" },
    { id: "host", label: "Host" },
    { id: "cpu", label: "CPU" },
    { id: "gpu", label: "GPU" },
    { id: "memory", label: "Memory" },
    { id: "storage", label: "Storage" },
    { id: "network", label: "Network" },
    { id: "virt", label: "Virtualization" },
    { id: "containers", label: "Containers" },
  ];

  const tabs = $derived<{ id: Tab; label: string }[]>([
    ...(fleetEnabled ? [{ id: "fleet" as Tab, label: "Fleet" }] : []),
    ...baseTabs,
    // Last, because it is the only tab that is not about right now.
    ...(historyEnabled ? [{ id: "history" as Tab, label: "History" }] : []),
  ]);

  // Settings live in a dialog, not a tab: they are a reference you consult
  // while looking at something else.
  let settingsOpen = $state(false);

  /**
   * Leaving a peer returns to the fleet dashboard rather than this node's
   * Overview: the fleet list is where the user came from, and dropping them on
   * a different node's Overview loses that context.
   */
  function returnToFleet() {
    selectFleetNode(null);
    userPickedTab = true;
    activeTab = "fleet";
  }

  // A standalone node has no Fleet tab, so never leave the user stranded on
  // one if fleet mode is turned off while the page is open.
  // Never leave the user stranded on a tab the node does not have -- fleet mode
  // turned off while the page is open, or a history catalog that never arrived.
  const effectiveTab = $derived<Tab>(
    (activeTab === "fleet" && !fleetEnabled) ||
      (activeTab === "history" && !historyEnabled)
      ? "overview"
      : activeTab
  );

  // Fleet is the landing tab when the node has peers — but only until the user
  // expresses a preference, since the fleet list arrives after first paint.
  $effect(() => {
    if (fleetEnabled && !appliedFleetDefault && !userPickedTab) {
      appliedFleetDefault = true;
      activeTab = "fleet";
    }
  });

  function selectTab(id: Tab) {
    userPickedTab = true;
    // Opening the fleet dashboard drops any selected peer: the fleet list is a
    // view of every node, so carrying one node's context into it would leave
    // the other tabs quietly showing that peer instead of this host.
    if (id === "fleet") {
      selectFleetNode(null);
    }
    activeTab = id;
  }

  /** Jumps to the panel an alert belongs to, from the navbar alert list. */
  function navigateToCategory(category: string) {
    if (tabs.some((t) => t.id === category)) {
      selectTab(category as Tab);
    }
  }

  function selectFleetNode(name: string | null) {
    monitor.selectNode(name);
    // Drilling into a peer should land on its dashboard, not leave the user
    // looking at the list they just clicked.
    if (name != null) selectTab("overview");
  }

  /** Severity dot for a tab, or null when nothing is firing in its category. */
  function tabSeverity(id: Tab) {
    return worstSeverity(alertsByCategory[id] ?? []);
  }

  // Formatted timestamp for the navbar.
  const lastUpdate = $derived(
    snapshot != null
      ? new Date(snapshot.timestamp).toLocaleTimeString()
      : "–"
  );
</script>

<div class="min-h-screen bg-base-300 flex flex-col">
  <!-- Navbar -->
  <nav class="navbar bg-base-100 shadow-md px-6 gap-4 min-h-12">
    <div class="flex-1 flex items-center gap-3 min-w-0">
      <!-- Simple monitor icon using SVG -->
      <svg
        xmlns="http://www.w3.org/2000/svg"
        class="h-5 w-5 text-primary shrink-0"
        fill="none"
        viewBox="0 0 24 24"
        stroke="currentColor"
        stroke-width="2"
      >
        <rect x="2" y="3" width="20" height="14" rx="2" />
        <path d="M8 21h8M12 17v4" />
        <path d="M6 8h.01M6 11h4M6 14h12" stroke-linecap="round" />
      </svg>
      <span class="font-bold text-base tracking-tight shrink-0">System Monitor</span>

      {#if selectedNode != null}
        <!-- Viewing a peer is a mode, so it gets a persistent breadcrumb. The
             way back is omitted on the fleet dashboard itself, where it would
             point at the page already being shown. -->
        <div class="flex items-center gap-1.5 text-xs min-w-0">
          <span class="text-base-content/30">/</span>
          <span class="badge badge-primary badge-sm font-medium truncate">
            {selectedNode}
          </span>
          <button type="button" class="btn btn-ghost btn-xs" onclick={returnToFleet}>
            Return to fleet dashboard
          </button>
          {#if peerFreshness != null && !peerFreshness.reachable}
            <!-- The aggregator keeps serving this peer's last good reading, so
                 every panel below still shows numbers. Without this badge they
                 are indistinguishable from live ones, which is the worst way
                 for a monitoring tool to be wrong: the operator reads a
                 healthy, unchanging machine that may in fact be gone. -->
            <span
              class="badge badge-warning badge-sm font-medium whitespace-nowrap"
              title={peerFreshness.error || "the last poll of this node failed"}
            >
              stale &middot; {staleAge}
            </span>
          {/if}
        </div>
      {/if}
    </div>

    <div class="flex items-center gap-4 text-xs text-base-content/60 shrink-0">
      {#if snapshot != null}
        <span class="whitespace-nowrap">Updated: {lastUpdate}</span>
      {/if}
      <select
        class="select select-xs select-bordered bg-base-200 text-base-content/80 font-medium"
        aria-label="Polling interval"
        title="Polling interval"
        value={currentInterval}
        onchange={onIntervalChange}
      >
        {#each intervalOptions as opt (opt.value)}
          <option value={opt.value}>{opt.label}</option>
        {/each}
      </select>
      <ThemeToggle />
      <AlertBell onNavigate={navigateToCategory} />
      <button
        type="button"
        class="btn btn-ghost btn-xs btn-circle"
        aria-label="Configuration"
        title="Configuration"
        onclick={() => (settingsOpen = true)}
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-4 w-4"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          stroke-width="2"
        >
          <circle cx="12" cy="12" r="3" />
          <path
            d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>
      <div class="flex items-center gap-1.5">
        <span
          class="inline-block w-2 h-2 rounded-full {connected
            ? 'bg-success'
            : 'bg-error'} animate-pulse"
        ></span>
        <span class="{connected ? 'text-success' : 'text-error'} font-medium">
          {connected ? "Connected" : "Disconnected"}
        </span>
      </div>
    </div>
  </nav>

  <!-- Tab bar -->
  <div class="bg-base-100 border-b border-base-300 px-4">
    <div class="flex overflow-x-auto">
      {#each tabs as tab (tab.id)}
        {@const severity = tabSeverity(tab.id)}
        <button
          class="tab tab-bordered whitespace-nowrap {effectiveTab === tab.id
            ? 'tab-active text-primary border-primary'
            : 'text-base-content/60 hover:text-base-content'} text-sm py-3 px-4 border-b-2 transition-colors flex items-center gap-1.5"
          onclick={() => selectTab(tab.id)}
        >
          {tab.label}
          {#if severity != null}
            <span
              class="inline-block w-1.5 h-1.5 rounded-full {severityDotClass(severity)}"
              title="{tab.label} has active alerts"
              aria-hidden="true"
            ></span>
          {/if}
        </button>
      {/each}
    </div>
  </div>

  <!-- Main content -->
  <main class="flex-1 p-4">
    {#if effectiveTab === "fleet"}
      <Fleet onSelect={selectFleetNode} />
    {:else if effectiveTab === "overview"}
      <Overview />
    {:else if effectiveTab === "host"}
      <HostPanel />
    {:else if effectiveTab === "cpu"}
      <CpuPanel />
    {:else if effectiveTab === "gpu"}
      <GpuPanel />
    {:else if effectiveTab === "memory"}
      <MemoryPanel />
    {:else if effectiveTab === "storage"}
      <DiskPanel />
    {:else if effectiveTab === "network"}
      <NetworkPanel />
    {:else if effectiveTab === "virt"}
      <VirtPanel />
    {:else if effectiveTab === "containers"}
      <ContainerPanel />
    {:else if effectiveTab === "history"}
      <History metrics={historyMetrics} />
    {/if}
  </main>

  <!-- Footer -->
  <footer class="footer footer-center p-2 bg-base-100 text-base-content/40 text-xs border-t border-base-300">
    <p>go-sysmon{buildVersion ? ` v${buildVersion}` : ""} — real-time system monitor</p>
  </footer>

  <SettingsDialog open={settingsOpen} onClose={() => (settingsOpen = false)} />
</div>
