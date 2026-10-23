import { test, expect } from '@playwright/test';
import { formatKilobytes } from '../src/lib/format';

/**
 * The API's cache_size is in kilobytes, so a 512 KB cache must not render as
 * "512.0 B".
 */
test.describe('formatKilobytes', () => {
  test('a size in KB gets a kilobyte-based unit', () => {
    expect(formatKilobytes(512)).toBe('512.0 KB');
    expect(formatKilobytes(36864)).toBe('36.0 MB');
    expect(formatKilobytes(1024 * 1024)).toBe('1.0 GB');
  });

  test('zero and invalid sizes pass through', () => {
    expect(formatKilobytes(0)).toBe('0 B');
    expect(formatKilobytes(Number.NaN)).toBe('-');
  });
});
