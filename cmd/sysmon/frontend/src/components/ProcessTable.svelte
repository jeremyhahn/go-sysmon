<script lang="ts">
  import DataTable from "./DataTable.svelte";
  import { formatBytes, formatBytesRate } from "$lib/format";
  import type { Column } from "$lib/table";
  import type { ProcessDetail } from "$lib/types";

  let { processes }: { processes: ProcessDetail[] } = $props();

  const columns: Column<ProcessDetail>[] = [
    { key: "name", label: "Name", sortValue: (p) => p.name },
    { key: "pid", label: "PID", align: "right", sortValue: (p) => p.pid },
    { key: "username", label: "User", sortValue: (p) => p.username },
    { key: "status", label: "State", sortValue: (p) => p.status },
    { key: "cpu_percent", label: "CPU", align: "right", sortValue: (p) => p.cpu_percent,
      title: "Percent of one core, as top reports it" },
    { key: "memory_bytes", label: "Memory", align: "right", sortValue: (p) => p.memory_bytes },
    { key: "read_bytes_rate", label: "Read", align: "right", sortValue: (p) => p.read_bytes_rate },
    { key: "write_bytes_rate", label: "Write", align: "right", sortValue: (p) => p.write_bytes_rate },
    { key: "priority", label: "Nice", align: "right", sortValue: (p) => p.priority },
  ];

  function cpuClass(pct: number): string {
    if (pct >= 50) return "text-error";
    if (pct >= 10) return "text-warning";
    return "";
  }
</script>

<DataTable
  rows={processes}
  {columns}
  {cells}
  searchText={(p: ProcessDetail) => `${p.name} ${p.username} ${p.pid}`}
  rowKey={(p: ProcessDetail) => p.pid}
  title="Processes"
  initialSort="cpu_percent"
  initialDirection="desc"
  searchPlaceholder="Search name, user or PID…"
  emptyMessage="No processes to show."
/>

{#snippet cells(p: ProcessDetail)}
  <td class="font-medium">{p.name}</td>
  <td class="text-right font-mono text-xs">{p.pid}</td>
  <td class="text-base-content/70">{p.username}</td>
  <td class="text-base-content/70">{p.status}</td>
  <td class="text-right {cpuClass(p.cpu_percent)}">{p.cpu_percent.toFixed(1)}%</td>
  <td class="text-right">{formatBytes(p.memory_bytes)}</td>
  <td class="text-right">{formatBytesRate(p.read_bytes_rate)}</td>
  <td class="text-right">{formatBytesRate(p.write_bytes_rate)}</td>
  <td class="text-right text-base-content/60">{p.priority}</td>
{/snippet}
