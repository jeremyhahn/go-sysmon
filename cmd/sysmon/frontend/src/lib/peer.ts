// What a drill-in answer contributes to the dashboard.
import type { PeerSnapshot, Snapshot } from "./types";

/**
 * liveReading is the snapshot a GET /api/fleet/{name}/snapshot envelope adds
 * to the dashboard, or null when it carries none that is current.
 *
 * A stale envelope -- the peer did not answer, and the aggregator served its
 * last snapshot with `reachable: false` -- contributes nothing: re-applying
 * that reading every cycle would draw a flat line that reads as "measured and
 * steady" when nothing was measured. The panels keep the last reading and the
 * stale badge says it is not live.
 *
 * A current envelope can still repeat a reading: the aggregator answers a
 * drill-in from its last fetch for the length of the fleet interval, so a
 * page refreshing faster than that receives the same snapshot more than once.
 * That is the store's job to fold once, by the rule in isNewReading.
 */
export function liveReading(peer: PeerSnapshot): Snapshot | null {
  return peer.reachable && peer.snapshot != null ? peer.snapshot : null;
}
