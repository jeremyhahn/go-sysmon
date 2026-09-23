import { test, expect } from '@playwright/test';
import { liveReading } from '../src/lib/peer';
import { isNewReading } from '../src/lib/reading';
import type { PeerSnapshot, Snapshot } from '../src/lib/types';

/**
 * The drill-in path, end to end in node: the envelopes a selected peer's poll
 * receives, through the two rules the store applies to them -- liveReading
 * (only a current envelope counts) and isNewReading (one chart point per
 * reading) -- to the readings that reach the histories.
 *
 * The aggregator answers a drill-in from its last fetch for the whole fleet
 * interval (#181), so a page refreshing every second against a 10 s fleet
 * receives each reading about ten times. Folding every copy would record
 * measurements nobody made, and the disk rates -- derived from the previous
 * reading -- would read zero for each copy and then spike.
 */

function snapshot(timestamp: string): Snapshot {
  return { timestamp } as Snapshot;
}

function envelope(timestamp: string, reachable = true): PeerSnapshot {
  return { snapshot: snapshot(timestamp), reachable, age_seconds: reachable ? 0 : 30 };
}

/** The timestamps the store would fold into its histories, in order. */
function folded(stream: PeerSnapshot[]): string[] {
  let lastAt: string | null = null;
  const out: string[] = [];
  for (const peer of stream) {
    const reading = liveReading(peer);
    if (reading == null || !isNewReading(lastAt, reading.timestamp)) continue;
    lastAt = reading.timestamp || null;
    out.push(reading.timestamp);
  }
  return out;
}

const T1 = '2026-09-22T10:00:00Z';
const T2 = '2026-09-22T10:00:10Z';
const T3 = '2026-09-22T10:00:20Z';

test('a reading served repeatedly within the fleet interval is folded once', () => {
  const stream = [
    ...Array.from({ length: 10 }, () => envelope(T1)),
    ...Array.from({ length: 10 }, () => envelope(T2)),
  ];
  expect(folded(stream)).toEqual([T1, T2]);
});

test('a stale envelope folds nothing, and the peer coming back folds its new reading', () => {
  expect(folded([envelope(T1), envelope(T1, false), envelope(T1, false), envelope(T3)])).toEqual([T1, T3]);
});

test('a stale envelope is not a live reading even though it carries a snapshot', () => {
  expect(liveReading(envelope(T1, false))).toBeNull();
  expect(liveReading({ snapshot: null, reachable: true, age_seconds: 0 })).toBeNull();
  expect(liveReading(envelope(T1))?.timestamp).toBe(T1);
});

test('the peer answering again with the reading it last gave, after going stale, is not a new point', () => {
  // The peer was down, then answers with the same snapshot it had before --
  // it has not collected since. That is the same measurement.
  expect(folded([envelope(T1), envelope(T1, false), envelope(T1)])).toEqual([T1]);
});
