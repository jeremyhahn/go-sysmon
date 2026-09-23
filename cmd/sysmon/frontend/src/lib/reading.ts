/** The date Go renders its zero time.Time as. */
const ZERO_TIME = "0001-01-01T00:00:00";

/**
 * isNewReading reports whether a snapshot taken at `timestamp` is a reading
 * the histories have not yet recorded, given the timestamp of the last one
 * they did (`lastAt`, null before any).
 *
 * A reading is identified by when it was taken, and the same one can arrive
 * twice: the server republishes it when only the node's identity changed (a
 * rename, #162), and a peer polled faster than it collects answers with the
 * reading it already gave. Each must add one point to a chart, or the chart
 * records a measurement nobody made -- and a rate derived from the previous
 * reading drops to zero for it.
 *
 * A snapshot with no timestamp cannot be told apart from another, so it is
 * always new. Go's zero time is the same absence spelled as a date: a server
 * dates every reading it publishes, but an older one relayed a Snapshotter's
 * undated reading as "0001-01-01T00:00:00Z", and treating that as an identity
 * would freeze every chart after the first frame.
 */
export function isNewReading(lastAt: string | null, timestamp: string): boolean {
  return !timestamp || timestamp.startsWith(ZERO_TIME) || timestamp !== lastAt;
}
