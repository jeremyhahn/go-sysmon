<script lang="ts">
  import Overview from "./views/Overview.svelte";
  import CpuPanel from "./components/CpuPanel.svelte";
  import GpuPanel from "./components/GpuPanel.svelte";
  import MemoryPanel from "./components/MemoryPanel.svelte";
  import DiskPanel from "./components/DiskPanel.svelte";
  import NetworkPanel from "./components/NetworkPanel.svelte";
  import ContainerPanel from "./components/ContainerPanel.svelte";
  import VirtPanel from "./components/VirtPanel.svelte";
  import HostPanel from "./components/HostPanel.svelte";
  import { monitor } from "./stores/monitor.svelte";

  const connected = $derived(monitor.connected);
  const snapshot = $derived(monitor.snapshot);
  const currentInterval = $derived(monitor.intervalMs);

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

  type Tab = "overview" | "host" | "cpu" | "gpu" | "memory" | "storage" | "network" | "virt" | "containers";

  let activeTab = $state<Tab>("overview");

  const tabs: { id: Tab; label: string }[] = [
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
    <div class="flex-1 flex items-center gap-3">
      <!-- Simple monitor icon using SVG -->
      <svg
        xmlns="http://www.w3.org/2000/svg"
        class="h-5 w-5 text-primary"
        fill="none"
        viewBox="0 0 24 24"
        stroke="currentColor"
        stroke-width="2"
      >
        <rect x="2" y="3" width="20" height="14" rx="2" />
        <path d="M8 21h8M12 17v4" />
        <path d="M6 8h.01M6 11h4M6 14h12" stroke-linecap="round" />
      </svg>
      <span class="font-bold text-base tracking-tight">System Monitor</span>
    </div>

    <div class="flex items-center gap-4 text-xs text-base-content/60 shrink-0">
      {#if snapshot != null}
        <span class="whitespace-nowrap">Updated: {lastUpdate}</span>
      {/if}
      <select
        class="select select-xs select-bordered bg-base-200 text-base-content/80 font-medium"
        value={currentInterval}
        onchange={onIntervalChange}
      >
        {#each intervalOptions as opt (opt.value)}
          <option value={opt.value}>{opt.label}</option>
        {/each}
      </select>
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
        <button
          class="tab tab-bordered whitespace-nowrap {activeTab === tab.id
            ? 'tab-active text-primary border-primary'
            : 'text-base-content/60 hover:text-base-content'} text-sm py-3 px-4 border-b-2 transition-colors"
          onclick={() => (activeTab = tab.id)}
        >
          {tab.label}
        </button>
      {/each}
    </div>
  </div>

  <!-- Main content -->
  <main class="flex-1 p-4">
    {#if activeTab === "overview"}
      <Overview />
    {:else if activeTab === "host"}
      <HostPanel />
    {:else if activeTab === "cpu"}
      <CpuPanel />
    {:else if activeTab === "gpu"}
      <GpuPanel />
    {:else if activeTab === "memory"}
      <MemoryPanel />
    {:else if activeTab === "storage"}
      <DiskPanel />
    {:else if activeTab === "network"}
      <NetworkPanel />
    {:else if activeTab === "virt"}
      <VirtPanel />
    {:else if activeTab === "containers"}
      <ContainerPanel />
    {/if}
  </main>

  <!-- Footer -->
  <footer class="footer footer-center p-2 bg-base-100 text-base-content/40 text-xs border-t border-base-300">
    <p>go-sysmon — real-time system monitor</p>
  </footer>
</div>
