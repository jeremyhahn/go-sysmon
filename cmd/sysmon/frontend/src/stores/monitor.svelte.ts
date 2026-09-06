// Svelte 5 runes-based reactive store for system monitor data.
// Uses a class with $state fields so that exported state is never reassigned
// at the module level (Svelte 5 forbids top-level $state reassignment in
// .svelte.ts modules that are used as shared state).

import { connect } from "$lib/transport";
import type { IntervalScope, Transport } from "$lib/transport";
import { sortBySeverity } from "$lib/alerts";
import { readStoredJSON, writeStoredJSON } from "$lib/storage";
import type {
  Alert,
  CPUInfo,
  DiskInfo,
  Fleet,
  FleetNode,
  NetworkInfo,
  PeerSnapshot,
  SensorData,
  Snapshot,
} from "$lib/types";

const HISTORY_LENGTH = 60;

const FLEET_URL = "/api/fleet";

/**
 * A peer snapshot and the fleet summary are plain HTTP round trips through the
 * local node, so they are polled no faster than once a second however tight the
 * local stream is set.
 *
 * Note what this does NOT control: the aggregator refreshes its peers on its
 * own server-side schedule (fleet.poll_interval_ms), so fetching faster than
 * that just re-reads the same cached figures. The refresh rate here decides how
 * promptly the browser picks up what the server already has; each node card
 * carries its own age so stale data is visible rather than implied.
 */
const MIN_PEER_POLL_MS = 1000;

/** localStorage key holding the alert ids the user has already been shown. */
const SEEN_ALERTS_KEY = "sysmon.alerts.seen";

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((v) => typeof v === "string");
}

/**
 * loadSeenAlertIds reads the previously acknowledged alert ids. Losing this
 * state is harmless — the badge simply restarts from empty — so any storage
 * or shape problem falls back to no ids rather than propagating.
 */
function loadSeenAlertIds(): string[] {
  return readStoredJSON(SEEN_ALERTS_KEY, isStringArray) ?? [];
}

function saveSeenAlertIds(ids: string[]): void {
  writeStoredJSON(SEEN_ALERTS_KEY, ids);
}

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

  /** Last successful /api/fleet response; null until the first poll lands. */
  fleet = $state<Fleet | null>(null);

  /**
   * Name of the peer whose data every panel is showing, or null for this host.
   * Selecting a peer swaps the source of `snapshot` and of every history
   * buffer, so the panels themselves need no notion of a fleet.
   */
  selectedNode = $state<string | null>(null);

  /**
   * peerFreshness describes the LAST poll of the selected peer. It is null when
   * this host is selected, whose data comes from the live stream and is current
   * by construction.
   */
  peerFreshness = $state<{
    reachable: boolean;
    error: string;
    ageSeconds: number;
  } | null>(null);

  /** When the last peer reading actually arrived, and the age it carried, so a
   * failed poll can report an age that keeps moving rather than one frozen at
   * the last success. Both are reset on every selection change: a new peer must
   * not inherit the previous one's clock. */
  #peerReadingAt: number | null = null;
  #peerBaseAge = 0;

  /**
   * showingLiveData answers one question for the whole dashboard: is what is on
   * screen current?
   *
   * Which fact settles it depends on WHERE the data comes from. This host's
   * panels are fed by the event stream, so `connected` is the question. A
   * selected peer's are refreshed by an independent HTTP poll, so the stream
   * says nothing about them -- and a peer that has been selected but has not
   * answered yet is not live either, because peerFreshness is cleared on
   * selection and stays null until the first poll returns.
   *
   * It lives here because two places ask it -- the Overview's Live badge and
   * the historical banner's claim about the gauges -- and they answered it
   * differently: the banner was fixed to be selection-aware while the badge
   * still read the local stream, so a reachable peer showed "Disconnected"
   * with the stream down, and an unreachable one showed "Live" with it up.
   * Two answers to one question is how a dashboard contradicts itself.
   */
  readonly showingLiveData = $derived(
    this.selectedNode == null ? this.connected : this.peerFreshness?.reachable === true
  );

  /** Alert ids already surfaced to the user, persisted across reloads. */
  seenAlertIds = $state<string[]>(loadSeenAlertIds());

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

  // -------------------------------------------------------------------------
  // Alerts
  // -------------------------------------------------------------------------

  /** Active alerts for the node being viewed, worst severity first. */
  readonly alerts = $derived(sortBySeverity(this.snapshot?.alerts ?? []));

  /**
   * Alerts bucketed by category. Category values are dashboard tab ids, which
   * is what lets a tab wear a dot and an alert row navigate to its panel.
   */
  readonly alertsByCategory = $derived.by(() => {
    const byCategory: Record<string, Alert[]> = {};
    for (const alert of this.alerts) {
      (byCategory[alert.category] ??= []).push(alert);
    }
    return byCategory;
  });

  readonly criticalCount = $derived(
    this.alerts.filter((a) => a.severity === "critical").length
  );

  readonly warningCount = $derived(
    this.alerts.filter((a) => a.severity === "warning").length
  );

  /** Alerts that have appeared since the user last opened the alert list. */
  readonly newAlerts = $derived.by(() => {
    const seen = new Set(this.seenAlertIds);
    return this.alerts.filter((a) => !seen.has(a.id));
  });

  /**
   * alertsFor returns the alerts a panel or card should react to. Omit
   * `resource` for every alert in the category; pass it to match one device.
   */
  alertsFor(category: string, resource?: string): Alert[] {
    const inCategory = this.alertsByCategory[category] ?? [];
    if (resource === undefined) return inCategory;
    return inCategory.filter((a) => a.resource === resource);
  }

  /** markAlertsSeen acknowledges everything currently firing. */
  markAlertsSeen(): void {
    // Storing only the active ids prunes itself: an alert that clears drops
    // out of the list and would count as new again if it ever came back.
    const ids = this.alerts.map((a) => a.id);
    this.seenAlertIds = ids;
    saveSeenAlertIds(ids);
  }

  // -------------------------------------------------------------------------
  // Fleet
  // -------------------------------------------------------------------------

  /** True only when the backend reports fleet mode configured and on. */
  readonly fleetEnabled = $derived(this.fleet?.enabled === true);

  /**
   * Fleet members exactly as reported, in no guaranteed order. Ordering is
   * deliberately not applied here: the Fleet view owns filter, sort and paging
   * as one pipeline, and sorting in both places would run the comparator twice
   * per poll for no benefit.
   */
  readonly fleetNodes = $derived(this.fleet?.nodes ?? []);

  /** The local node's own version, shown beside a peer's when they differ. */
  readonly localVersion = $derived(
    this.fleet?.nodes?.find((n) => n.self)?.version ?? ""
  );

  /**
   * The local node's own commit. Named in the drift tooltips so "a different
   * build than this one" says WHICH build -- the version string beside it is
   * the same word on every node and identifies nothing.
   */
  readonly localCommit = $derived(
    this.fleet?.nodes?.find((n) => n.self)?.git_commit ?? ""
  );

  /** The FleetNode currently selected, or null when viewing this host. */
  readonly selectedNodeInfo = $derived<FleetNode | null>(
    this.selectedNode == null
      ? null
      : this.fleet?.nodes?.find((n) => n.name === this.selectedNode) ?? null
  );

  // Tracking maps for manual delta calculation between snapshots.
  #prevNetworkMap = new Map<string, { sent: number; recv: number }>();
  #prevDiskMap = new Map<string, { readBytes: number; writeBytes: number }>();
  #transport: Transport | null = null;
  #fleetTimer: ReturnType<typeof globalThis.setTimeout> | null = null;
  #peerTimer: ReturnType<typeof globalThis.setTimeout> | null = null;
  #fleetPolling = false;

  /**
   * Monotonic token for the current node selection. Every peer request carries
   * the value that was current when it was issued, and anything returning
   * under a superseded token is discarded. A response that is merely late must
   * never speak for the node the user has since chosen.
   */
  #selectionGeneration = 0;

  setTransport(t: Transport): void {
    this.#transport = t;
  }

  /**
   * setInterval changes the cadence of the poller feeding the current view.
   *
   * Which poller that is depends on what is on screen (#97): this node's own
   * monitor while its own data is shown, the aggregator's peer refresh while a
   * peer or the fleet is shown. Driving the local monitor from a peer's page
   * would change a number nobody is looking at and leave the displayed data
   * arriving at its old rate.
   *
   * The server's answer is applied rather than assumed, because a fleet
   * request is floored server-side: asking for 250ms and displaying 250ms
   * while peers refresh once a second is the same class of untruth as a stale
   * reading presented as live.
   */
  setInterval(ms: number): void {
    this.intervalMs = ms;
    const scope: IntervalScope = this.selectedNode == null ? "local" : "fleet";
    void this.#transport?.setInterval(ms, scope)?.then((effective) => {
      if (effective == null) return;
      this.fleetIntervalMs = effective.fleetIntervalMs;
    });
  }

  /** The cadence peers are actually refreshed at, as the server reports it.
   * Zero when this node has no fleet. */
  fleetIntervalMs = $state(0);

  /** effectiveIntervalMs is the rate the data on screen is actually collected
   * at: the fleet poller's when a peer is selected, this node's otherwise. */
  get effectiveIntervalMs(): number {
    if (this.selectedNode != null && this.fleetIntervalMs > 0) {
      return this.fleetIntervalMs;
    }
    return this.intervalMs;
  }

  /**
   * selectNode switches every panel to a peer's data, or back to this host
   * when name is null.
   */
  selectNode(name: string | null): void {
    if (this.selectedNode === name) return;
    // Invalidate every in-flight peer request and any poll loop still running
    // for the previous selection before anything else changes.
    const generation = ++this.#selectionGeneration;
    this.selectedNode = name;
    // The previous peer's freshness must not linger over the new selection,
    // and neither must its clock: a new peer whose first poll fails would
    // otherwise inherit however long the last one had been unreachable.
    this.peerFreshness = null;
    this.#peerReadingAt = null;
    this.#peerBaseAge = 0;
    this.#stopPeerPolling();
    // Two hosts must never share a history buffer: a 60-second chart that
    // straddles a node switch would be a lie about both of them.
    this.#resetSeries();
    if (name != null) {
      void this.#pollPeer(name, generation);
    }
    // Returning to the local node needs no fetch — the live stream is still
    // running and repopulates the panels on its next tick.
  }

  /** startFleetPolling begins the /api/fleet poll loop. Idempotent. */
  startFleetPolling(): void {
    // Guarding on the timer alone was only accidentally correct: it stays null
    // from the first call until the first response lands, so a second call in
    // that window would have started a duplicate loop.
    if (this.#fleetPolling) return;
    this.#fleetPolling = true;
    void this.#pollFleet();
  }

  async #pollFleet(): Promise<void> {
    try {
      const res = await fetch(FLEET_URL);
      if (res.ok) {
        const fleet = (await res.json()) as Fleet;
        this.fleet = { enabled: fleet.enabled === true, nodes: fleet.nodes ?? [] };

        // A node that leaves the fleet must not strand the UI on a dead
        // selection with no way back.
        const selected = this.selectedNode;
        if (
          selected != null &&
          !this.fleet.nodes.some((n) => n.name === selected)
        ) {
          this.selectNode(null);
        }
      } else if (res.status === 404) {
        // An older backend has no fleet endpoint at all, which is simply the
        // standalone case.
        this.fleet = { enabled: false, nodes: [] };
      }
    } catch {
      // Keep the last known fleet rather than blanking the view on one
      // dropped request.
    }
    // Follow the refresh selector like every other stream, rather than a fixed
    // ten seconds that ignored it.
    this.#fleetTimer = globalThis.setTimeout(() => {
      void this.#pollFleet();
    }, Math.max(this.intervalMs, MIN_PEER_POLL_MS));
  }

  async #pollPeer(name: string, generation: number): Promise<void> {
    try {
      const res = await fetch(
        `/api/fleet/${encodeURIComponent(name)}/snapshot`
      );
      if (!res.ok) {
        this.#peerPollFailed(generation, `the aggregator answered ${res.status}`);
      } else {
        // Parse first and let applySnapshot arbitrate. Checking the selection
        // before awaiting the body is exactly the bug this replaces: the user
        // can switch nodes while the JSON is still being parsed, and a check
        // that has already passed cannot see it.
        const peer = (await res.json()) as PeerSnapshot;
        if (this.#selectionGeneration === generation) {
          this.peerFreshness = {
            reachable: peer.reachable,
            error: peer.error ?? "",
            ageSeconds: peer.age_seconds,
          };
          // Only a CURRENT reading is folded in. The aggregator keeps serving
          // the last good snapshot after a peer goes away, and applying it on
          // every cycle appended the same values to the history again and
          // again -- a flat line that reads as "measured and steady" when
          // nothing was measured at all. The panels keep showing the last
          // reading; the stale badge is what says it is not live.
          // The base for a later failure's age: what the aggregator said, and
          // when it said it. Recorded for any envelope that arrives, not only
          // one carrying a snapshot -- a reachable:false answer is still a
          // reading whose age this side knows.
          this.#peerReadingAt = Date.now();
          this.#peerBaseAge = peer.age_seconds;
          if (peer.reachable && peer.snapshot != null) {
            this.applySnapshot(peer.snapshot, name);
          }
        }
      }
    } catch {
      // A poll that fails outright -- refused connection, a body that is not
      // JSON, the aggregator itself down -- says the same thing as an envelope
      // carrying reachable:false, and used to say nothing at all.
      //
      // The old comment here claimed "the fleet list and the stale badge are
      // what report it unreachable", and the badge is driven by peerFreshness,
      // which this path left untouched. So the last SUCCESSFUL reading kept
      // its reachable:true and the page went on presenting a frozen snapshot
      // as live, with no badge -- the exact state the badge exists for, in the
      // one case that never reached it.
      this.#peerPollFailed(generation, "the last poll of this node failed");
    }
    // A superseded loop stops here instead of rescheduling, so switching
    // A -> B -> A cannot leave two loops polling the same node.
    if (this.#selectionGeneration !== generation) return;
    this.#peerTimer = globalThis.setTimeout(() => {
      void this.#pollPeer(name, generation);
    }, Math.max(this.intervalMs, MIN_PEER_POLL_MS));
  }

  /**
   * peerPollFailed marks the selected peer unreachable when a poll does not
   * come back at all.
   *
   * The age is carried forward from the last reading that did arrive rather
   * than frozen at its value: a badge stuck on "12m old" while an hour passes
   * is a smaller version of the same lie the badge exists to prevent.
   */
  #peerPollFailed(generation: number, error: string): void {
    if (this.#selectionGeneration !== generation) return;
    // Computed from the last envelope that arrived, never accumulated onto the
    // previous answer. Adding the elapsed time to an age that already contains
    // it double-counts: failures at +1s, +2s and +3s reported 1s, 3s and 6s.
    const since =
      this.#peerReadingAt == null
        ? 0
        : (Date.now() - this.#peerReadingAt) / 1000;
    this.peerFreshness = {
      reachable: false,
      error,
      ageSeconds: this.#peerBaseAge + since,
    };
  }

  #stopPeerPolling(): void {
    if (this.#peerTimer != null) {
      globalThis.clearTimeout(this.#peerTimer);
      this.#peerTimer = null;
    }
  }

  /** #resetSeries clears the snapshot and every history buffer. */
  #resetSeries(): void {
    this.snapshot = null;
    this.cpuHistory = [];
    this.cpuOverallHistory = makeHistory(0);
    this.memHistory = makeHistory(0);
    this.networkHistory = [];
    this.diskHistory = [];
    this.loadHistory = makeHistory(0);
    this.gpuUtilHistory = [];
    this.gpuMemHistory = [];
    this.gpuTempHistory = [];
    this.gpuPowerHistory = [];
    this.gpuPcieRxHistory = [];
    this.gpuPcieTxHistory = [];
    this.cpuTempHistory = [];
    this.packagePowerHistory = makeHistory(0);
    this.psiCpuHistory = makeHistory(0);
    this.psiMemHistory = makeHistory(0);
    this.psiIOHistory = makeHistory(0);
    this.#prevNetworkMap.clear();
    this.#prevDiskMap.clear();
  }

  /**
   * applySnapshot folds one snapshot into the store. `fromNode` names the node
   * that produced it — null for this host, a peer name otherwise — and it must
   * equal the current selection or the snapshot is dropped.
   *
   * The comparison is deliberately against *which* node rather than merely
   * whether some peer is selected. The weaker test let a response that landed
   * after the user switched peers render one host's data, and history, under
   * another host's name.
   */
  applySnapshot(snap: Snapshot, fromNode: string | null = null): void {
    if (fromNode !== this.selectedNode) return;

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

// The fleet list decides whether the Fleet tab exists at all, so it is polled
// from module initialisation rather than from a component that may never mount.
monitor.startFleetPolling();
