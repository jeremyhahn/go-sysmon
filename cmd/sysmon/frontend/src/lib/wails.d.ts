// The Wails v2 runtime, as injected into the desktop window.
//
// This is a declaration about the HOST, not a wire type, which is why it is
// here rather than in $lib/types: nothing in pkg/types describes it and the
// generator has nothing to say about it. It lived in the hand-written types.ts
// until that file became a barrel over generated output (#38).

import type { Snapshot } from "./types";

declare global {
  interface Window {
    runtime?: {
      EventsOn: (event: string, callback: (data: unknown) => void) => void;
      EventsOff: (event: string) => void;
    };
    go?: {
      main: {
        MonitorBinding: {
          GetSnapshot: () => Promise<Snapshot>;
          SetInterval: (ms: number) => Promise<void>;
          // Restarts the frame stream, so the next frame is a whole snapshot.
          // The desktop's equivalent of reopening the socket (#98).
          Resync: () => Promise<void>;
        };
      };
    };
  }
}
