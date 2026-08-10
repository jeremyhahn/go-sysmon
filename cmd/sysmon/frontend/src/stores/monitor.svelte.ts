// Svelte 5 runes-based reactive store for system monitor data.
// Uses a class with $state fields so that exported state is never reassigned
// at the module level (Svelte 5 forbids top-level $state reassignment in
// .svelte.ts modules that are used as shared state).

import { connect } from "$lib/transport";
import type { Transport } from "$lib/transport";
import type { Snapshot, CPUInfo, NetworkInfo, DiskInfo, SensorData } from "$lib/types";

const HISTORY_LENGTH = 60;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeHistory<T>(fill: T): T[] {
  return Array<T>(HISTORY_LENGTH).fill(fill);
}

function pushHistory<T>(arr: T[], value: T): T[] {
  const next = arr.slice(1);
  next.push(value);
  return next;
}

// ---------------------------------------------------------------------------
// Sub-types
// ---------------------------------------------------------------------------

export interface NetworkHistory {
  name: string;
  sentRates: number[];
  recvRates: number[];
}

export interface DiskHistory {
  name: string;
  readRates: number[];
  writeRates: number[];
}

// ---------------------------------------------------------------------------
// Store class
// ---------------------------------------------------------------------------

class MonitorStore {
  snapshot = $state<Snapshot | null>(null);
  connected = $state(false);
  intervalMs = $state(1000);
  cpuHistory = $state<number[][]>([]);
  cpuOverallHistory = $state<number[]>(makeHistory(0));
  memHistory = $state<number[]>(makeHistory(0));
  networkHistory = $state<NetworkHistory[]>([]);
  diskHistory = $state<DiskHistory[]>([]);
  loadHistory = $state<number[]>(makeHistory(0));

  // Per-GPU histories — outer index is GPU index, inner array is 60 data points.
  gpuUtilHistory = $state<number[][]>([]);
  gpuMemHistory = $state<number[][]>([]);
  gpuTempHistory = $state<number[][]>([]);
  gpuPowerHistory = $state<number[][]>([]);
  gpuPcieRxHistory = $state<number[][]>([]);
  gpuPcieTxHistory = $state<number[][]>([]);

  // Sensor histories.
  // Per-core temp history: outer index matches core_temps array order.
  cpuTempHistory = $state<number[][]>([]);
  // Total package power summed across all packages.
  packagePowerHistory = $state<number[]>(makeHistory(0));
  // PSI some_avg10 histories for CPU, memory, and IO.
  psiCpuHistory = $state<number[]>(makeHistory(0));
  psiMemHistory = $state<number[]>(makeHistory(0));
  psiIOHistory = $state<number[]>(makeHistory(0));

  // Derived values live inside the class so they can read $state fields.
  readonly overallCpuPercent = $derived(
    this.snapshot != null
      ? this.snapshot.cpus.reduce((sum, c) => sum + c.usage_percent, 0) /
          Math.max(this.snapshot.cpus.length, 1)
      : 0
  );

  readonly topCpuCores = $derived(
    this.snapshot != null
      ? [...this.snapshot.cpus]
          .sort((a, b) => b.usage_percent - a.usage_percent)
          .slice(0, 5)
      : ([] as CPUInfo[])
  );

  readonly topNetworkByTraffic = $derived(
    this.snapshot != null
      ? [...this.snapshot.networks]
          .filter((n) => !n.is_loopback)
          .sort(
            (a, b) =>
              b.bytes_sent_rate +
              b.bytes_recv_rate -
              (a.bytes_sent_rate + a.bytes_recv_rate)
          )
          .slice(0, 5)
      : ([] as NetworkInfo[])
  );

  readonly unhealthyDisks = $derived(
    this.snapshot != null
      ? this.snapshot.disks.filter((d) => d.smart_enabled && !d.smart_healthy)
      : ([] as DiskInfo[])
  );

  // Maximum CPU core temperature across all reported core_temps entries.
  readonly maxCpuTemp = $derived(
    this.snapshot?.sensors?.core_temps != null &&
    this.snapshot.sensors.core_temps.length > 0
      ? Math.max(...this.snapshot.sensors.core_temps.map((t) => t.temp_celsius))
      : 0
  );

  // Total package power draw summed across all RAPL packages.
  readonly totalPackagePowerW = $derived(
    this.snapshot?.sensors?.package_power != null
      ? this.snapshot.sensors.package_power.reduce(
          (sum, p) => sum + p.power_w,
          0
        )
      : 0
  );

  // Tracking maps for manual delta calculation between snapshots.
  #prevNetworkMap = new Map<string, { sent: number; recv: number }>();
  #prevDiskMap = new Map<string, { readBytes: number; writeBytes: number }>();
  #transport: Transport | null = null;

  setTransport(t: Transport): void {
    this.#transport = t;
  }

  setInterval(ms: number): void {
    this.intervalMs = ms;
    this.#transport?.setInterval(ms);
  }

  applySnapshot(snap: Snapshot): void {
    this.snapshot = snap;

    // CPU history
    const cores = snap.cpus.length;
    if (this.cpuHistory.length !== cores) {
      this.cpuHistory = Array.from({ length: cores }, () => makeHistory(0));
    }
    this.cpuHistory = this.cpuHistory.map((hist, i) =>
      pushHistory(hist, snap.cpus[i]?.usage_percent ?? 0)
    );

    const overallPct =
      snap.cpus.reduce((s, c) => s + c.usage_percent, 0) /
      Math.max(cores, 1);
    this.cpuOverallHistory = pushHistory(this.cpuOverallHistory, overallPct);

    // Memory history
    this.memHistory = pushHistory(this.memHistory, snap.memory.used_percent);

    // Load history
    this.loadHistory = pushHistory(this.loadHistory, snap.load_avg.load1);

    // Network history
    this.networkHistory = snap.networks.map((iface) => {
      const existing = this.networkHistory.find((h) => h.name === iface.name);
      const sentRates = existing?.sentRates ?? makeHistory(0);
      const recvRates = existing?.recvRates ?? makeHistory(0);

      let sentRate = iface.bytes_sent_rate;
      let recvRate = iface.bytes_recv_rate;

      // Fall back to manual delta when backend sends zero rates.
      if (sentRate === 0 && recvRate === 0) {
        const prev = this.#prevNetworkMap.get(iface.name);
        if (prev != null) {
          sentRate = Math.max(0, iface.bytes_sent - prev.sent);
          recvRate = Math.max(0, iface.bytes_recv - prev.recv);
        }
      }

      this.#prevNetworkMap.set(iface.name, {
        sent: iface.bytes_sent,
        recv: iface.bytes_recv,
      });

      return {
        name: iface.name,
        sentRates: pushHistory(sentRates, sentRate),
        recvRates: pushHistory(recvRates, recvRate),
      };
    });

    // Disk IO rate history
    this.diskHistory = snap.disks.map((disk) => {
      const existing = this.diskHistory.find((h) => h.name === disk.name);
      const readRates = existing?.readRates ?? makeHistory(0);
      const writeRates = existing?.writeRates ?? makeHistory(0);

      const prev = this.#prevDiskMap.get(disk.name);
      let readRate = 0;
      let writeRate = 0;
      if (prev != null) {
        readRate = Math.max(0, disk.read_bytes - prev.readBytes);
        writeRate = Math.max(0, disk.write_bytes - prev.writeBytes);
      }
      this.#prevDiskMap.set(disk.name, {
        readBytes: disk.read_bytes,
        writeBytes: disk.write_bytes,
      });

      return {
        name: disk.name,
        readRates: pushHistory(readRates, readRate),
        writeRates: pushHistory(writeRates, writeRate),
      };
    });

    // Sensor histories
    const sensors: SensorData | undefined = snap.sensors;
    if (sensors != null) {
      // Per-core temperature history — keyed by core_temps array order.
      const coreCount = sensors.core_temps?.length ?? 0;
      if (this.cpuTempHistory.length !== coreCount) {
        this.cpuTempHistory = Array.from({ length: coreCount }, () =>
          makeHistory(0)
        );
      }
      if (coreCount > 0) {
        this.cpuTempHistory = this.cpuTempHistory.map((hist, i) =>
          pushHistory(hist, sensors.core_temps[i]?.temp_celsius ?? 0)
        );
      }

      // Package power — sum all packages.
      const totalPowerW =
        sensors.package_power?.reduce((s, p) => s + p.power_w, 0) ?? 0;
      this.packagePowerHistory = pushHistory(this.packagePowerHistory, totalPowerW);

      // PSI averages.
      this.psiCpuHistory = pushHistory(
        this.psiCpuHistory,
        sensors.psi?.cpu?.some_avg10 ?? 0
      );
      this.psiMemHistory = pushHistory(
        this.psiMemHistory,
        sensors.psi?.memory?.some_avg10 ?? 0
      );
      this.psiIOHistory = pushHistory(
        this.psiIOHistory,
        sensors.psi?.io?.some_avg10 ?? 0
      );
    }

    // GPU histories
    const gpuCount = snap.gpus?.length ?? 0;
    if (this.gpuUtilHistory.length !== gpuCount) {
      this.gpuUtilHistory = Array.from({ length: gpuCount }, () => makeHistory(0));
      this.gpuMemHistory = Array.from({ length: gpuCount }, () => makeHistory(0));
      this.gpuTempHistory = Array.from({ length: gpuCount }, () => makeHistory(0));
      this.gpuPowerHistory = Array.from({ length: gpuCount }, () => makeHistory(0));
      this.gpuPcieRxHistory = Array.from({ length: gpuCount }, () => makeHistory(0));
      this.gpuPcieTxHistory = Array.from({ length: gpuCount }, () => makeHistory(0));
    }
    if (gpuCount > 0) {
      this.gpuUtilHistory = this.gpuUtilHistory.map((hist, i) =>
        pushHistory(hist, snap.gpus[i]?.gpu_util_percent ?? 0)
      );
      this.gpuMemHistory = this.gpuMemHistory.map((hist, i) =>
        pushHistory(hist, snap.gpus[i]?.memory_percent ?? 0)
      );
      this.gpuTempHistory = this.gpuTempHistory.map((hist, i) =>
        pushHistory(hist, snap.gpus[i]?.temperature_gpu ?? 0)
      );
      this.gpuPowerHistory = this.gpuPowerHistory.map((hist, i) =>
        pushHistory(hist, snap.gpus[i]?.power_draw_w ?? 0)
      );
      this.gpuPcieRxHistory = this.gpuPcieRxHistory.map((hist, i) =>
        pushHistory(hist, snap.gpus[i]?.pcie_rx_mbps ?? 0)
      );
      this.gpuPcieTxHistory = this.gpuPcieTxHistory.map((hist, i) =>
        pushHistory(hist, snap.gpus[i]?.pcie_tx_mbps ?? 0)
      );
    }
  }
}

// Single shared instance — import this from all components.
export const monitor = new MonitorStore();

// Bootstrap transport once at module initialisation.
const transport = connect(
  (snap) => monitor.applySnapshot(snap),
  (status) => {
    monitor.connected = status;
  }
);
monitor.setTransport(transport);
