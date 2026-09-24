import type { Page } from "@playwright/test";
import snapshot from "./snapshot.json" with { type: "json" };
/**
 * Zero-valued entries with EVERY field the Go types emit, generated from them
 * rather than written by hand.
 *
 * Hand-written fixtures here failed exactly once and instructively: a container
 * built from the fields a test happened to care about was missing `read_iops`,
 * so the detail drawer threw on `selected.read_iops.toLocaleString()` and never
 * rendered -- and the test reported "CPU throttling not found", which reads as
 * a missing label rather than a fixture with a hole in it.
 *
 * pkg/types has a test asserting these key sets still match the Go structs, so
 * a field added there fails loudly here instead of silently producing another
 * undefined.
 */
import templates from "./entry-templates.json" with { type: "json" };
import type { Alert, StackDetection } from "../../src/lib/types";
import { TAB_IDS, type TabLabel } from "./tabs";

/**
 * Serves a deterministic snapshot over a mocked /api/events stream.
 *
 * The DataTable tests used to run against whatever the host happened to be
 * doing, on the assumption that "a host always has more than 25 processes".
 * That is false inside a container without --pid=host: the process list is one
 * or two entries, the pager never renders because it is behind `pageCount > 1`,
 * and the tests hang waiting for a Next page button that does not exist. They
 * passed on GitHub and failed on Gitea for that reason alone.
 *
 * Pagination, search and sort are properties of the component, not of the
 * machine, so the data they run against should be fixed.
 */

type Snapshot = typeof snapshot;

/** Builds `count` processes with predictable, searchable names. */
export function makeProcesses(count: number) {
  const template = snapshot.processes.process_list[0];
  return Array.from({ length: count }, (_, i) => ({
    ...template,
    pid: 1000 + i,
    // A third are named systemd-* so a search has a known, non-empty result
    // that is still a strict subset.
    name: i % 3 === 0 ? `systemd-unit-${i}` : `worker-${i}`,
    username: i % 2 === 0 ? "root" : "jhahn",
    cpu_percent: (count - i) / 10,
    memory_bytes: (count - i) * 1024 * 1024,
  }));
}

export interface MockOptions {
  /** How many processes to serve. Default 60: enough for 3 pages of 25. */
  processCount?: number;
  /**
   * Replaces the fixture's disk list. Given as partials merged over the first
   * fixture disk, so a spec states only the fields it is about and still gets
   * a renderable card.
   */
  disks?: Partial<Snapshot["disks"][number]>[];
  /**
   * Replaces the container list. Default: the fixture's, which is empty.
   *
   * This exists so a table's columns, sorting and detail drawer can be tested
   * on a host that is running no containers. They used to be skipped there --
   * a test that reports success for not running, which is the thing this suite
   * is meant to catch rather than perform (#35).
   */
  containers?: number;
  /** Replaces the VM list. Same reasoning as containers. */
  vms?: number;
  /** Replaces the container image list. Same reasoning. */
  images?: number;
  /** Attaches SMART attributes to the first disk. Same reasoning. */
  smartAttrs?: boolean;
  /**
   * Replaces the GPU list. Default: the fixture's two, which are integrated
   * adapters reporting zeroes. Pass a count for NVIDIA-shaped entries with
   * memory, utilisation and ECC, or 0 for a host with no GPU at all -- both
   * states used to decide whether six tests ran (#35).
   */
  gpus?: number;
  /**
   * Replaces the NPU list. Default: the fixture's one Intel NPU. Pass 0 for a
   * host without one, which is what decides whether the NPU tab exists.
   */
  npus?: number;
  /**
   * The node's container runtime detection, which decides whether the
   * Containers tab exists (#187). Default: not sent at all, the shape of a
   * peer on a build that predates detection.
   */
  containerRuntime?: StackDetection;
  /** The node's hypervisor detection, for the Virtualization tab. Same default. */
  hypervisor?: StackDetection;
  /**
   * Overrides fields of the virtualization capability block, for the panels'
   * "could not look" states. Default: the fixture's, which can see everything.
   */
  capability?: Partial<Snapshot["virtualization"]["capability"]>;
  /**
   * Alerts the snapshot reports as firing. Default: none, so no spec depends
   * on the alert state of the host it runs on. `alertEveryTab()` builds one
   * per tab, the noisiest state the tab bar can be in (#163).
   */
  alerts?: Alert[];
}

/**
 * Builds one firing alert per tab, each in that tab's category.
 *
 * Each message names its tab's label, because that is the collision a loose
 * locator falls into: `{ name: 'Storage' }` matches an alert row that says
 * "Storage" as readily as it matches the tab.
 */
export function alertEveryTab(): Alert[] {
  return (Object.entries(TAB_IDS) as [TabLabel, string][]).map(([label, category], i) => ({
    id: `fixture-${category}`,
    rule: `fixture_${category}`,
    severity: i % 2 === 0 ? "critical" : "warning",
    category,
    resource: `${category}0`,
    message: `${label} fixture alert`,
    value: 95,
    threshold: 90,
    since: "2026-01-01T00:00:00Z",
  }));
}

/** Builds `count` discrete GPUs with memory, utilisation and ECC populated. */
export function makeGPUs(count: number) {
  const template = snapshot.gpus[0];
  return Array.from({ length: count }, (_, i) => ({
    ...template,
    index: i,
    name: `NVIDIA RTX PRO 6000 (${i})`,
    uuid: `GPU-0000000${i}`,
    driver_version: "580.178.04",
    compute_mode: "Default",
    memory_total_mib: 98304,
    memory_used_mib: 12288,
    memory_free_mib: 86016,
    memory_percent: 12.5,
    gpu_util_percent: 37 + i,
    memory_util_percent: 12,
    temperature_gpu: 54,
    power_draw_w: 180,
    power_limit_w: 600,
    ecc_enabled: true,
  }));
}

/** Builds `count` NPUs from the fixture's, each with a distinct utilisation. */
export function makeNPUs(count: number) {
  const template = snapshot.npus[0];
  return Array.from({ length: count }, (_, i) => ({
    ...template,
    index: i,
    device: `accel${i}`,
    util_percent: 20 + i,
  }));
}

/** Builds `count` containers with predictable, sortable values. */
export function makeContainers(count: number) {
  return Array.from({ length: count }, (_, i) => ({
    ...templates.container,
    index: i,
    id: `c0ffee${String(i).padStart(10, "0")}`,
    short_id: `c0ffee${String(i).padStart(6, "0")}`,
    name: `container-${i}`,
    runtime: "docker",
    command: `/usr/bin/app --id ${i}`,
    cgroup_path: `/sys/fs/cgroup/system.slice/docker-${i}.scope`,
    cpu_percent: (count - i) * 1.5,
    cpu_usage_usec: (count - i) * 1_000_000,
    memory_bytes: (count - i) * 64 * 1024 * 1024,
    memory_limit_bytes: 2 * 1024 * 1024 * 1024,
    memory_percent: (count - i) * 2.5,
    memory_peak_bytes: (count - i) * 80 * 1024 * 1024,
    pids: 10 + i,
    process_count: 10 + i,
    read_bytes: 0,
    write_bytes: 0,
    read_bytes_rate: 0,
    write_bytes_rate: 0,
    throttled_percent: i === 0 ? 12.5 : 0,
    nr_throttled: i === 0 ? 42 : 0,
    nr_periods: 1000,
    oom_kills: 0,
    cpu_pressure: 0,
    memory_pressure: 0,
    io_pressure: 0,
    uptime_seconds: 3600 + i,
  }));
}

/** Builds `count` virtual machines. */
export function makeVMs(count: number) {
  return Array.from({ length: count }, (_, i) => ({
    ...templates.vm,
    index: i,
    pid: 2000 + i,
    name: `guest-${i}`,
    hypervisor: "qemu",
    vcpus: 4,
    vcpu_threads: 4,
    thread_count: 8,
    memory_bytes: 4 * 1024 * 1024 * 1024,
    rss_bytes: 3 * 1024 * 1024 * 1024,
    cpu_percent: (count - i) * 10,
    uptime_seconds: 7200 + i,
    net_rx_rate: 1024,
    net_tx_rate: 2048,
    disks: [],
    networks: [],
  }));
}

/** Builds `count` container images. */
export function makeImages(count: number) {
  return Array.from({ length: count }, (_, i) => ({
    ...templates.image,
    id: `sha256:image${String(i).padStart(8, "0")}`,
    tags: [`example/app:${i}`],
    size_bytes: (count - i) * 10 * 1024 * 1024,
    created_unix: 1_700_000_000 + i,
    dangling: false,
    in_use: i % 2 === 0,
  }));
}

/** Builds a SMART attribute table with a known worst entry. */
export function makeSMARTAttrs() {
  const names = [
    "Raw_Read_Error_Rate", "Reallocated_Sector_Ct", "Power_On_Hours",
    "Temperature_Celsius", "Wear_Leveling_Count", "Total_LBAs_Written",
  ];
  return names.map((name, i) => ({
    id: i + 1,
    name,
    value: 100 - i,
    worst: 100 - i,
    threshold: 10,
    raw: String(i * 100),
    failing: false,
  }));
}

/**
 * Intercepts the dashboard's event stream and serves one snapshot.
 *
 * The fulfilled body is a complete response rather than a live stream, so the
 * browser sees the event, then EOF, and reconnects — at which point the route
 * serves the same fixture again. The UI only needs one snapshot to render, and
 * a fixed body keeps the test independent of any real server.
 *
 * Must be called before page.goto, since the app connects on load.
 */
export async function mockSnapshotFeed(page: Page, opts: MockOptions = {}) {
  const count = opts.processCount ?? 60;

  const processes = makeProcesses(count);
  const listed =
    opts.disks == null
      ? snapshot.disks
      : opts.disks.map((d, i) => ({ ...snapshot.disks[0], index: i, ...d }));
  // A separate binding rather than reassigning `listed`: the fixture file's
  // disks carry "smart_attrs": null, so a disk given a populated list is a
  // different shape and the element type has to say so.
  const disks = opts.smartAttrs
    ? listed.map((d, i) => (i === 0 ? { ...d, smart_attrs: makeSMARTAttrs() } : d))
    : listed;

  const gpus = opts.gpus == null ? snapshot.gpus : makeGPUs(opts.gpus);
  const npus = opts.npus == null ? snapshot.npus : makeNPUs(opts.npus);

  const virtualization = {
    ...snapshot.virtualization,
    containers: opts.containers == null ? snapshot.virtualization.containers : makeContainers(opts.containers),
    vms: opts.vms == null ? snapshot.virtualization.vms : makeVMs(opts.vms),
    ...(opts.containerRuntime == null ? {} : { container_runtime: opts.containerRuntime }),
    ...(opts.hypervisor == null ? {} : { hypervisor: opts.hypervisor }),
    capability: { ...snapshot.virtualization.capability, ...opts.capability },
    runtime: {
      ...snapshot.virtualization.runtime,
      // The image table renders only when a runtime socket answered, so
      // supplying images without this produces a fixture whose table is not on
      // the page at all -- and the test then fails looking for a search box,
      // which reads as a missing control rather than a missing precondition.
      // Found by running the suite rather than by reading it.
      available: opts.images == null ? snapshot.virtualization.runtime.available : true,
      images: opts.images == null ? snapshot.virtualization.runtime.images : makeImages(opts.images),
      images_total: opts.images == null ? snapshot.virtualization.runtime.images_total : opts.images,
    },
  };

  const payload: Snapshot = {
    ...snapshot,
    disks,
    gpus,
    npus,
    virtualization,
    processes: {
      ...snapshot.processes,
      total: count,
      running: count,
      sleeping: 0,
      idle: 0,
      stopped: 0,
      zombie: 0,
      process_list: processes,
    },
  } as Snapshot;

  // The fixture file has no alerts key, so it is not part of Snapshot, which is
  // the fixture's own shape; the frame gets one here. An empty list, not the
  // host's, unless a spec asks for alerts.
  const frame = { ...payload, alerts: opts.alerts ?? [] };

  // A "full" frame of the delta protocol, which is what the dashboard opens
  // the stream in. One frame is all the UI needs to render; the sequence number
  // matters only to a test that goes on to send deltas, which delta.spec.ts
  // does with its own feed.
  const body =
    `retry: 1000\n\n` +
    `event: full\ndata: ${JSON.stringify({ seq: 1, snapshot: frame })}\n\n`;

  // The path may carry a query string -- the dashboard opens the stream with
  // ?deltas=1 -- so the pattern must not be anchored at the end of the path. It
  // was, and the mock silently stopped matching the moment the parameter was
  // added: every test went on running against whatever the real host was doing,
  // which is precisely the fragility this fixture exists to remove.
  await page.route(/\/api\/events(\?|$)/, async (route) => {
    await route.fulfill({
      status: 200,
      headers: {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache",
        "X-Accel-Buffering": "no",
      },
      body,
    });
  });

  // The interval dropdown posts here; accept and ignore so the mocked page
  // never reaches a real server.
  await page.route(/\/api\/interval$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ interval_ms: 1000 }),
    });
  });
}

/** The number of processes whose name matches a "systemd" search. */
export function systemdMatchCount(total: number): number {
  return makeProcesses(total).filter((p) => p.name.includes("systemd")).length;
}
