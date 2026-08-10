// Transport layer: adapts Wails v2 desktop events and browser server-sent
// events to a single unified callback interface.

import type { Snapshot } from "./types";

export type SnapshotCallback = (snapshot: Snapshot) => void;
export type StatusCallback = (connected: boolean) => void;

const WAILS_EVENT = "sysmon:snapshot";

const EVENTS_URL = "/api/events";
const INTERVAL_URL = "/api/interval";

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
export interface Transport {
  disconnect: () => void;
  setInterval: (ms: number) => void;
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

// --- Wails transport -------------------------------------------------------

function connectWails(
  onSnapshot: SnapshotCallback,
  onStatus: StatusCallback
): Transport {
  const rt = window.runtime!;

  rt.EventsOn(WAILS_EVENT, (data: unknown) => {
    try {
      const snapshot = data as Snapshot;
      onSnapshot(snapshot);
      onStatus(true);
    } catch {
      // Malformed event payload — ignore silently.
    }
  });

  onStatus(true);

  // Request an immediate snapshot so the UI is not blank on first load.
  window.go?.main.MonitorBinding.GetSnapshot().then((snap) => {
    onSnapshot(snap);
  }).catch(() => {
    // GetSnapshot is best-effort; streaming events will populate data shortly.
  });

  return {
    disconnect() {
      rt.EventsOff(WAILS_EVENT);
      onStatus(false);
    },
    setInterval(ms: number) {
      window.go?.main.MonitorBinding.SetInterval(ms);
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

  function open() {
    if (stopped) return;

    source = new EventSource(EVENTS_URL);

    source.onopen = () => {
      reconnectDelay = RECONNECT_DELAY_MS;
      onStatus(true);
    };

    source.addEventListener("snapshot", (ev: MessageEvent<string>) => {
      try {
        const snapshot: Snapshot = JSON.parse(ev.data) as Snapshot;
        onSnapshot(snapshot);
        onStatus(true);
      } catch {
        // Malformed payload — ignore.
      }
    });

    // The server sends "bye" when the monitor stops. There is nothing left to
    // stream, so retrying would just hammer a server that has told us it is
    // done.
    source.addEventListener("bye", () => {
      stopped = true;
      source?.close();
      source = null;
      onStatus(false);
    });

    source.onerror = () => {
      onStatus(false);

      // EventSource reconnects by itself while it is CONNECTING, using the
      // retry interval the stream advertised. It only reaches CLOSED when it
      // has given up — an HTTP error or a bad content type — and that is the
      // only case this code has to handle.
      if (source?.readyState === EventSource.CLOSED) {
        source = null;
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
      onStatus(false);
    },
    setInterval(ms: number) {
      // The poll rate belongs to the server's single monitor, so it is a plain
      // REST call rather than something carried on the stream.
      void fetch(INTERVAL_URL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ interval_ms: ms }),
      }).catch(() => {
        // Best-effort: a dropped rate change corrects itself the next time the
        // user picks an interval, and the stream is unaffected.
      });
    },
  };
}
