// Transport layer: adapts Wails v2 desktop events and browser WebSocket
// to a single unified callback interface.

import type { Snapshot } from "./types";

export type SnapshotCallback = (snapshot: Snapshot) => void;
export type StatusCallback = (connected: boolean) => void;

const WAILS_EVENT = "sysmon:snapshot";
const WS_RECONNECT_DELAY_MS = 3000;
const WS_MAX_RECONNECT_DELAY_MS = 30000;

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
  sendCommand: (cmd: Record<string, unknown>) => void;
}

/**
 * connect subscribes to system snapshots using whatever transport is
 * available (Wails events in desktop mode, WebSocket in browser mode).
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
  return connectWebSocket(onSnapshot, onStatus);
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
    sendCommand(cmd: Record<string, unknown>) {
      if (cmd.type === "set_interval" && typeof cmd.interval_ms === "number") {
        window.go?.main.MonitorBinding.SetInterval(cmd.interval_ms);
      }
    },
  };
}

// --- WebSocket transport ----------------------------------------------------

function connectWebSocket(
  onSnapshot: SnapshotCallback,
  onStatus: StatusCallback
): Transport {
  let ws: WebSocket | null = null;
  let stopped = false;
  let reconnectDelay = WS_RECONNECT_DELAY_MS;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  function wsUrl(): string {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    return `${proto}//${location.host}/ws`;
  }

  function connect() {
    if (stopped) return;

    ws = new WebSocket(wsUrl());

    ws.onopen = () => {
      reconnectDelay = WS_RECONNECT_DELAY_MS;
      onStatus(true);
    };

    ws.onmessage = (ev: MessageEvent<string>) => {
      try {
        const snapshot: Snapshot = JSON.parse(ev.data) as Snapshot;
        onSnapshot(snapshot);
      } catch {
        // Malformed message — ignore.
      }
    };

    ws.onclose = () => {
      ws = null;
      onStatus(false);
      if (!stopped) {
        scheduleReconnect();
      }
    };

    ws.onerror = () => {
      // onclose fires after onerror — let that handle reconnection.
      ws?.close();
    };
  }

  function scheduleReconnect() {
    reconnectTimer = setTimeout(() => {
      reconnectDelay = Math.min(reconnectDelay * 2, WS_MAX_RECONNECT_DELAY_MS);
      connect();
    }, reconnectDelay);
  }

  connect();

  return {
    disconnect() {
      stopped = true;
      if (reconnectTimer != null) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
      ws?.close();
      ws = null;
      onStatus(false);
    },
    sendCommand(cmd: Record<string, unknown>) {
      if (ws != null && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(cmd));
      }
    },
  };
}
