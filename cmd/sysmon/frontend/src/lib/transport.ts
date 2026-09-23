// Transport layer: adapts Wails v2 desktop events and browser server-sent
// events to a single unified callback interface.

import { applyPatch, type Patch } from "./delta";
import type { Snapshot } from "./types";

export type SnapshotCallback = (snapshot: Snapshot) => void;
export type StatusCallback = (connected: boolean) => void;

// The desktop stream carries the same two frame kinds as the socket: one whole
// snapshot, then patches against it. The names are distinct from the old
// "sysmon:snapshot" so a frontend and a binding that disagree about the
// protocol fail immediately rather than parsing an envelope as a snapshot and
// rendering a page of undefined values.
const WAILS_FULL_EVENT = "sysmon:full";
const WAILS_DELTA_EVENT = "sysmon:delta";

// The stream is opened in delta mode. The server keeps the original protocol
// on the same endpoint for any other client, but this dashboard ships with the
// server that serves it, so there is no version to be skewed against -- and no
// listener here for the old event either. A negotiation that somehow failed
// should leave the page visibly empty rather than quietly falling back, because
// falling back is exactly the bug that would never be noticed.
const EVENTS_URL = "/api/events?deltas=1";
const INTERVAL_URL = "/api/interval";

/** A whole snapshot, with the sequence number it belongs to. */
interface FullFrame {
  seq: number;
  snapshot: Snapshot;
}

/** A change from the frame before it. patch is absent when nothing moved. */
interface DeltaFrame {
  seq: number;
  patch?: Patch;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** A sequence number has to be a real, whole, exact number to be compared
 * against the one before it. NaN and a float both make `seq + 1` meaningless. */
function isSequence(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

/**
 * parseFull reads a full frame, returning null when it is not one.
 *
 * `JSON.parse(...) as FullFrame` is a promise to the compiler, not a check. It
 * cannot fail at runtime, so anything that is syntactically JSON — `null`, a
 * number, `{}`, an envelope with no snapshot — used to be accepted, assigned to
 * the rendered state, and reported as connected and current. **That is the
 * failure this whole protocol exists to prevent**, arriving through the one
 * door that was not looking: not a stale reading, but no reading at all,
 * presented as live truth.
 *
 * What is validated here is the ENVELOPE -- a sequence number that is a usable
 * one, and a payload present at all. What makes the payload a snapshot is
 * isSnapshot's, and it is the same question a delta's result has to answer, so
 * it lives there rather than being asked once on this path.
 */
function parseFull(raw: string): FullFrame | null {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isObject(value) || !isSequence(value.seq)) return null;
  if (!isSnapshot(value.snapshot)) return null;
  return { seq: value.seq, snapshot: value.snapshot };
}

/**
 * The shape of a snapshot, as dotted paths, in the three kinds that matter.
 *
 * Derived from the `Snapshot` type in $lib/types rather than from the fields
 * one happens to think of. Three rounds of review found this by enumeration --
 * top level, then `sensors`, then `memory.dimms` and the virtualization lists
 * -- each round correct and each one short, because it listed what was in
 * front of it. Reading the type gives the whole set at once:
 *
 *   cd cmd/sysmon/frontend && node --input-type=module -e '...'  -- or simply
 *   every `X[]` and every nested interface in the Snapshot tree.
 *
 * OBJECTS are required: the Go types are structs, never pointers, so a real
 * server always sends them, and the page reads through them without a guard.
 *
 * WHAT THIS DOES NOT ESTABLISH. It checks the snapshot's SHAPE -- that every
 * section and list the page walks is the kind of thing it expects. It does not
 * descend INTO array elements, so it says nothing about the values inside one.
 *
 * That gap is not benign and must not be described as if it were. An earlier
 * version of this comment claimed a malformed element "throws in one panel
 * where somebody can see it", and that is only true of some of them:
 * `{"bytes_sent_rate": "x"}` passes, coerces through arithmetic rather than
 * throwing, and renders as a number or a NaN while the page reports itself
 * connected and current. Even the cases that do throw only throw once the panel
 * holding them is rendered, so a frame can be presented as current from another
 * tab. A delta can rewrite a single element, so this is reachable frame by
 * frame and not only on connection.
 *
 * So the guarantee is narrower than "renderable", which is what an earlier
 * version of this said and it contradicted the paragraph above it: an accepted
 * frame has the container shape the page walks -- the sections are objects and
 * the lists are lists. Whether it renders, and whether the readings inside those
 * lists are the right type, are both unchecked. Closing that means element-level
 * validation, and doing it by hand means restating the whole recursive
 * Snapshot type here in a second dialect -- a copy whose own drift refuses
 * EVERY frame and blanks the dashboard the moment a field is added on the Go
 * side and missed here. Generating the schema from the Go types has neither
 * problem and is filed as #38.
 *
 * ARRAYS are required to BE arrays, and null or absent is allowed: a nil Go
 * slice marshals to null, so a machine with no fans, no GPU and no alerts
 * sends null for all three. Presence is what every call site already checks
 * and it is not the same thing -- `"x" != null` is true, `"x".length > 0` is
 * true, and then `.map()` throws.
 */
const SNAPSHOT_OBJECTS = [
  "host",
  "cpu_summary",
  "memory",
  "load_avg",
  "processes",
  "sensors",
  "sensors.psi",
  "sensors.psi.cpu",
  "sensors.psi.memory",
  "sensors.psi.io",
  "virtualization",
  "virtualization.runtime",
  "virtualization.capability",
  "virtualization.container_runtime",
  "virtualization.hypervisor",
] as const;

/**
 * REQUIRED_ARRAYS are the three the store spreads and reduces with no guard at
 * all -- `snapshot.cpus.reduce(...)`, `[...snapshot.networks]`,
 * `snapshot.disks.filter(...)`. For those, null is as unusable as a string.
 *
 * (The page assuming that is older than this validator and is not changed
 * here; what this does is refuse the frame rather than let it throw mid-render.)
 */
const REQUIRED_ARRAYS = ["cpus", "disks", "networks"] as const;

const SNAPSHOT_ARRAYS = [
  "gpus",
  "alerts",
  "memory.dimms",
  "processes.process_list",
  "sensors.core_temps",
  "sensors.core_voltages",
  "sensors.package_power",
  "sensors.thermal_throttle",
  "sensors.thermal_zones",
  "sensors.fans",
  "virtualization.containers",
  "virtualization.vms",
  "virtualization.runtimes",
  "virtualization.runtime.images",
  "virtualization.capability.notes",
  "virtualization.container_runtime.evidence",
  "virtualization.hypervisor.evidence",
] as const;

/**
 * PSI is the one section allowed to be missing entirely.
 *
 * A kernel without pressure-stall information does not report it and the page
 * guards for that -- but not in halves: psiRows reads `some_avg10` off
 * whatever it is handed. So `sensors.psi` absent is fine, and `sensors.psi`
 * present without `cpu` is not, which is why it is named here rather than
 * simply dropped from the required list.
 */
const OPTIONAL_SECTIONS = [
  "sensors.psi",
  // Tab detection (#187). A server predating it does not send these, and the
  // page reads a missing one as "unknown", which shows the tab.
  "virtualization.container_runtime",
  "virtualization.hypervisor",
] as const;

/** at walks a dotted path, returning undefined at the first step that is not
 * an object rather than throwing. */
function at(value: Record<string, unknown>, path: string): unknown {
  let held: unknown = value;
  for (const step of path.split(".")) {
    if (!isObject(held)) return undefined;
    held = held[step];
  }
  return held;
}

/** optionalAndAbsent reports a section the machine does not have, whose
 * members are therefore not required either. */
function optionalAndAbsent(value: Record<string, unknown>, path: string): boolean {
  for (const section of OPTIONAL_SECTIONS) {
    if (path !== section && !path.startsWith(section + ".")) continue;
    if (at(value, section) == null) return true;
  }
  return false;
}

function isSnapshot(value: unknown): value is Snapshot {
  if (!isObject(value)) return false;
  if (typeof value.timestamp !== "string") return false;

  for (const path of SNAPSHOT_OBJECTS) {
    if (optionalAndAbsent(value, path)) continue;
    if (!isObject(at(value, path))) return false;
  }
  for (const path of REQUIRED_ARRAYS) {
    if (!Array.isArray(at(value, path))) return false;
  }
  for (const path of SNAPSHOT_ARRAYS) {
    if (optionalAndAbsent(value, path)) continue;
    const held = at(value, path);
    // Null or absent is a thing this machine does not have.
    if (held == null) continue;
    if (!Array.isArray(held)) return false;
  }
  return true;
}


/**
 * parseDelta reads a delta frame, returning null when it is not one.
 *
 * Only the envelope is checked. The patch's own shape is applyPatch's contract
 * — it refuses anything that is not a patch and throws, which the caller below
 * already turns into a resync — and checking it twice would put "what is a
 * patch" in two places that can disagree. A missing patch is legitimate: it
 * means nothing moved.
 */
function parseDelta(raw: string): DeltaFrame | null {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isObject(value) || !isSequence(value.seq)) return null;
  return { seq: value.seq, patch: value.patch as Patch | undefined };
}

/**
 * How many resyncs in a row are treated as a stream that is simply broken.
 *
 * A gap means the server's frames did not arrive in order, which cannot
 * ordinarily happen over one connection, so reopening once is the right answer
 * and it is immediate. Repeating means something is actually wrong, and
 * hammering the server is not the way to find out.
 *
 * Exported so the browser tests count rounds against this number rather than
 * their own copy of it: a test that hardcodes 3 keeps passing while claiming
 * something else if the limit is ever changed.
 */
export const MAX_IMMEDIATE_RESYNCS = 3;

// Backoff bounds for the manual reconnect path. EventSource retries on its own
// using the interval the server advertises, so these only apply when it gives
// up entirely — a non-2xx response or the wrong content type.
const RECONNECT_DELAY_MS = 3000;
const MAX_RECONNECT_DELAY_MS = 30000;

/**
 * isWailsRuntime returns true when the page is loaded inside a Wails v2
 * desktop window (the runtime object is injected by the Go host).
 */
function isWailsRuntime(): boolean {
  return typeof window !== "undefined" && window.runtime != null;
}

// Transport is the opaque handle returned by connect().
/** IntervalScope names which poller a rate change is aimed at.
 *
 * "local" is this node's own monitor. "fleet" is the aggregator's peer refresh,
 * which is the poller that actually feeds the fleet view and a selected peer --
 * changing the local monitor while looking at a peer would report a cadence
 * nothing was collecting at (#97). */
export type IntervalScope = "local" | "fleet";

/** Effective is what the server is actually running at, after its own floor.
 * A request for 250ms on the fleet poller comes back as 1000. */
export interface Effective {
  intervalMs: number;
  fleetIntervalMs: number;
}

// Transport is the opaque handle returned by connect().
export interface Transport {
  disconnect: () => void;
  setInterval: (ms: number, scope: IntervalScope) => Promise<Effective | null>;
}

/**
 * connect subscribes to system snapshots using whatever transport is
 * available (Wails events in desktop mode, server-sent events in the browser).
 *
 * Returns a Transport handle whose disconnect() method tears down the
 * subscription cleanly.
 */
export function connect(
  onSnapshot: SnapshotCallback,
  onStatus: StatusCallback
): Transport {
  if (isWailsRuntime()) {
    return connectWails(onSnapshot, onStatus);
  }
  return connectEventSource(onSnapshot, onStatus);
}

// --- The frame protocol, once ----------------------------------------------

/**
 * createFrameApplier holds one client's place in the delta stream: the snapshot
 * every patch is applied to, and the sequence number it carries.
 *
 * Both transports use it. The rules are identical whether the frames arrive
 * over a socket or over the Wails event bridge -- a patch applies to exactly
 * one frame, a gap means this client cannot be trusted to be current, and a
 * result that is not a snapshot is not rendered -- and only the recovery
 * differs: the browser reopens the socket, the desktop asks the binding to
 * restart its stream. A second copy of these rules would be a second place for
 * them to be wrong, and the failure they prevent is readings that look measured
 * and are not.
 */
function createFrameApplier(
  onSnapshot: SnapshotCallback,
  onStatus: StatusCallback,
  onGap: (reason: string) => void
) {
  let state: Snapshot | null = null;
  let seq = 0;

  return {
    /** forget drops this client's place, so the next frame must be a full. */
    forget() {
      state = null;
      seq = 0;
    },

    /** seen reports whether a snapshot has been applied. */
    get current(): Snapshot | null {
      return state;
    },

    full(raw: string): boolean {
      const frame = parseFull(raw);
      if (frame == null) {
        // Not a gap, but treated as one: a frame that is not a snapshot leaves
        // this client with nothing it can trust.
        onGap("a full frame that is not a snapshot");
        return false;
      }
      state = frame.snapshot;
      seq = frame.seq;
      onSnapshot(state);
      onStatus(true);
      return true;
    },

    delta(raw: string): boolean {
      const frame = parseDelta(raw);
      if (frame == null) {
        onGap("a delta frame that is not a delta");
        return false;
      }
      // A patch describes the change from exactly one frame. Applying it to
      // anything else produces a snapshot that never existed.
      if (state == null) {
        onGap("a delta with no snapshot to apply it to");
        return false;
      }
      if (frame.seq !== seq + 1) {
        onGap(`a gap in the stream: expected ${seq + 1}, got ${frame.seq}`);
        return false;
      }

      let patched: unknown;
      try {
        patched = applyPatch(state, frame.patch);
      } catch (e) {
        onGap(e instanceof Error ? e.message : "a patch that did not apply");
        return false;
      }

      // The patch applied; that does not make the result a snapshot. A patch is
      // a tree of operations, and one of them replaces the node it is applied
      // to -- at the root, that replaces the whole measurement with whatever
      // the frame carried. `as Snapshot` would accept null, a number or an
      // arbitrary object and render it as current.
      if (!isSnapshot(patched)) {
        onGap("a delta whose result is not a snapshot");
        return false;
      }

      state = patched;
      seq = frame.seq;
      onSnapshot(state);
      onStatus(true);
      return true;
    },
  };
}

// --- Wails transport -------------------------------------------------------

function connectWails(
  onSnapshot: SnapshotCallback,
  onStatus: StatusCallback
): Transport {
  const rt = window.runtime!;

  // The desktop has no socket to reopen, so recovery is a binding call: the
  // stream restarts and begins with a whole snapshot. Without it a window that
  // lost its place would go on applying patches to a snapshot the server never
  // sent it, drifting silently instead of failing (#98).
  const applier = createFrameApplier(onSnapshot, onStatus, (reason) => {
    console.warn(`sysmon: restarting the desktop stream (${reason})`);
    applier.forget();
    onStatus(false);
    void window.go?.main.MonitorBinding.Resync();
  });

  rt.EventsOn(WAILS_FULL_EVENT, (data: unknown) => {
    if (typeof data === "string") applier.full(data);
  });
  rt.EventsOn(WAILS_DELTA_EVENT, (data: unknown) => {
    if (typeof data === "string") applier.delta(data);
  });

  // Request an immediate snapshot so the UI is not blank on first load. It is
  // rendered but NOT adopted as the base for patches: it carries no sequence
  // number, so a patch computed against the server's frame would not be
  // describing this object.
  window.go?.main.MonitorBinding.GetSnapshot().then((snap) => {
    if (applier.current == null) {
      onSnapshot(snap);
      onStatus(true);
    }
  }).catch(() => {
    // GetSnapshot is best-effort; streaming frames will populate data shortly.
  });

  return {
    disconnect() {
      rt.EventsOff(WAILS_FULL_EVENT);
      rt.EventsOff(WAILS_DELTA_EVENT);
      applier.forget();
      onStatus(false);
    },
    async setInterval(ms: number, _scope: IntervalScope) {
      // The desktop build has no fleet, so both scopes mean the local monitor.
      await window.go?.main.MonitorBinding.SetInterval(ms);
      return { intervalMs: ms, fleetIntervalMs: 0 };
    },
  };
}

// --- Server-sent events transport ------------------------------------------

function connectEventSource(
  onSnapshot: SnapshotCallback,
  onStatus: StatusCallback
): Transport {
  let source: EventSource | null = null;
  let stopped = false;
  let reconnectDelay = RECONNECT_DELAY_MS;
  let reconnectTimer: ReturnType<typeof globalThis.setTimeout> | null = null;

  // The snapshot every delta is applied to, and the sequence number it carries,
  // live in the shared applier -- the same one the desktop transport uses.
  // Both are cleared whenever the stream is reopened: a patch is only ever
  // applied to the frame the server computed it against, and after a reconnect
  // this client has no idea which frame that was.
  let consecutiveResyncs = 0;
  const applier = createFrameApplier(onSnapshot, onStatus, (reason) => resync(reason));

  /**
   * resync abandons the stream and opens a new one, which begins with a whole
   * snapshot.
   *
   * It is what happens when a delta cannot be trusted: a gap in the sequence, a
   * patch that does not fit, or a patch arriving with no snapshot to apply it
   * to. The alternative -- keeping what is on screen and waiting for the next
   * delta -- would leave readings that look measured and are not, which is the
   * one thing this stream must never do.
   */
  function resync(reason: string) {
    if (stopped) return;
    applier.forget();
    source?.close();
    source = null;
    onStatus(false);

    consecutiveResyncs++;
    if (consecutiveResyncs > MAX_IMMEDIATE_RESYNCS) {
      console.warn(`sysmon: repeated stream resync (${reason}); backing off`);
      scheduleReconnect();
      return;
    }
    open();
  }

  function open() {
    if (stopped) return;

    applier.forget();
    source = new EventSource(EVENTS_URL);

    // A socket that is open is not a dashboard that is current. The reconnect
    // delay is reset here, because the connection did succeed -- but the status
    // is NOT, because resync() cleared this transport's snapshot and nothing
    // cleared the one already rendered. Reporting "connected" now would relabel
    // those numbers as live the instant the socket opened, and leave them that
    // way for as long as the reopened stream sent nothing: the exact failure
    // this stream exists to prevent, arrived at through the recovery path
    // rather than through a dropped frame. onStatus(true) is raised where the
    // evidence is -- an accepted full or delta, below.
    source.onopen = () => {
      reconnectDelay = RECONNECT_DELAY_MS;
    };

    source.addEventListener("full", (ev: MessageEvent<string>) => {
      applier.full(ev.data);
      // consecutiveResyncs is deliberately NOT cleared here. Every reopened
      // stream begins with a full frame, so treating one as proof of recovery
      // clears the counter a moment after resync() raised it and the limit is
      // never reached. A server that answers each new connection with a good
      // full and then the same broken delta would be reconnected to forever, as
      // fast as the frames arrive.
    });

    source.addEventListener("delta", (ev: MessageEvent<string>) => {
      if (!applier.delta(ev.data)) return;
      // An applied delta is the evidence a full frame is not: this stream
      // delivered a contiguous frame that fitted the snapshot it was computed
      // against, which is the thing that was failing. Only that clears the
      // limiter.
      consecutiveResyncs = 0;
    });

    // The server sends "bye" when its monitor stops — most often because the
    // service is being restarted, not shut down for good. Close this stream and
    // reconnect on the normal backoff so the console re-attaches on its own when
    // the backend comes back. (disconnect() still stops retries for a real
    // teardown, since it sets `stopped`.)
    source.addEventListener("bye", () => {
      source?.close();
      source = null;
      applier.forget();
      onStatus(false);
      scheduleReconnect();
    });

    source.onerror = () => {
      onStatus(false);

      // EventSource reconnects by itself while it is CONNECTING, using the
      // retry interval the stream advertised. It only reaches CLOSED when it
      // has given up — an HTTP error or a bad content type — and that is the
      // only case this code has to handle.
      if (source?.readyState === EventSource.CLOSED) {
        source = null;
        applier.forget();
        scheduleReconnect();
      }
    };
  }

  function scheduleReconnect() {
    if (stopped || reconnectTimer != null) return;

    reconnectTimer = globalThis.setTimeout(() => {
      reconnectTimer = null;
      reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY_MS);
      open();
    }, reconnectDelay);
  }

  open();

  return {
    disconnect() {
      stopped = true;
      if (reconnectTimer != null) {
        globalThis.clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
      source?.close();
      source = null;
      applier.forget();
      onStatus(false);
    },
    async setInterval(ms: number, scope: IntervalScope) {
      // The poll rate belongs to a server-side poller, so it is a plain REST
      // call rather than something carried on the stream.
      //
      // The response is read rather than discarded: the server answers with
      // what is in force after its own floor, and the header states that
      // instead of what was asked for.
      try {
        const res = await fetch(INTERVAL_URL, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ interval_ms: ms, scope }),
        });
        if (!res.ok) return null;
        const body = await res.json();
        return {
          intervalMs: Number(body.interval_ms) || ms,
          fleetIntervalMs: Number(body.fleet_interval_ms) || 0,
        };
      } catch {
        // Best-effort: a dropped rate change corrects itself the next time the
        // user picks an interval, and the stream is unaffected.
        return null;
      }
    },
  };
}
