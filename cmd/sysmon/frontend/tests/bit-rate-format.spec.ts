import { test, expect } from '@playwright/test';
import { formatBitRate, formatByteRateAsBits } from '../src/lib/format';

/**
 * Network rates in decimal bits per second (#257).
 *
 * Links are rated in SI bits -- a 1 Gbps port moves 10^9 bits a second -- so
 * the units step by 1000, not by the 1024 the byte formatter uses.
 */

test.describe('formatBitRate', () => {
  test('zero has a unit', () => {
    expect(formatBitRate(0)).toBe('0 bps');
  });

  test('each unit starts at its boundary', () => {
    expect(formatBitRate(1)).toBe('1.0 bps');
    expect(formatBitRate(999)).toBe('999.0 bps');
    expect(formatBitRate(1_000)).toBe('1.0 Kbps');
    expect(formatBitRate(1_000_000)).toBe('1.0 Mbps');
    expect(formatBitRate(1_000_000_000)).toBe('1.0 Gbps');
    expect(formatBitRate(1_000_000_000_000)).toBe('1.0 Tbps');
  });

  test('a value just below a boundary stays in the smaller unit', () => {
    expect(formatBitRate(999_000)).toBe('999.0 Kbps');
    expect(formatBitRate(999_000_000)).toBe('999.0 Mbps');
    expect(formatBitRate(999_000_000_000)).toBe('999.0 Gbps');
  });

  test('Tbps is the largest unit', () => {
    expect(formatBitRate(2.5e12)).toBe('2.5 Tbps');
    expect(formatBitRate(4e15)).toBe('4000.0 Tbps');
  });

  test('a fraction of a bit stays in bps', () => {
    expect(formatBitRate(0.5)).toBe('0.5 bps');
  });

  test('a negative rate keeps its sign and unit', () => {
    expect(formatBitRate(-2_000)).toBe('-2.0 Kbps');
  });

  test('a non-finite rate renders as a dash', () => {
    expect(formatBitRate(Number.NaN)).toBe('-');
    expect(formatBitRate(Number.POSITIVE_INFINITY)).toBe('-');
  });

  test('decimals are honoured', () => {
    expect(formatBitRate(1_234_567, 2)).toBe('1.23 Mbps');
  });
});

test.describe('formatByteRateAsBits', () => {
  test('a byte is eight bits', () => {
    expect(formatByteRateAsBits(125_000)).toBe('1.0 Mbps');
    expect(formatByteRateAsBits(125_000_000)).toBe('1.0 Gbps');
  });

  test('zero and invalid rates pass through', () => {
    expect(formatByteRateAsBits(0)).toBe('0 bps');
    expect(formatByteRateAsBits(Number.NaN)).toBe('-');
  });
});
