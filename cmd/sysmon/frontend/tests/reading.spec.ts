import { test, expect } from '@playwright/test';
import { isNewReading } from '../src/lib/reading';

/**
 * One chart point per reading (#162).
 *
 * The server republishes a reading unchanged but for the node's identity when
 * the node is renamed, and a peer polled faster than it collects answers with
 * the same reading twice. Folding either into the histories a second time
 * records a measurement nobody made, and the disk rates -- derived from the
 * previous reading -- drop to zero for it.
 */

test('a reading already recorded is not recorded again', () => {
  expect(isNewReading('2026-09-22T10:00:00Z', '2026-09-22T10:00:00Z')).toBe(false);
});

test('a later reading is new', () => {
  expect(isNewReading('2026-09-22T10:00:00Z', '2026-09-22T10:00:01Z')).toBe(true);
});

test('the first reading is new', () => {
  expect(isNewReading(null, '2026-09-22T10:00:00Z')).toBe(true);
});

test('a snapshot with no timestamp always counts, since it cannot be told apart', () => {
  expect(isNewReading('', '')).toBe(true);
  expect(isNewReading(null, '')).toBe(true);
});

test("Go's zero time is no timestamp at all, so it never freezes the charts", () => {
  expect(isNewReading('0001-01-01T00:00:00Z', '0001-01-01T00:00:00Z')).toBe(true);
});
