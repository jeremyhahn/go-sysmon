import { test, expect } from '@playwright/test';
import {
  DEFAULT_PAGE_SIZE,
  fleetPageUrl,
  lastPageIfPast,
  pageCount,
  parsePageSize,
  rangeLabel,
} from '../src/lib/fleetpage';

/**
 * The browser's half of the fleet paging contract (#15): the request it makes
 * and the arithmetic over the counts the server sends back.
 */

test.describe('fleet paging arithmetic', () => {
  test('a stored page size is honoured only if it is still offered', () => {
    expect(parsePageSize('100')).toBe(100);
    expect(parsePageSize('250')).toBe(250);
    for (const bad of [null, '', 'ten', '0', '-25', '37', '1e2']) {
      expect(parsePageSize(bad), String(bad)).toBe(DEFAULT_PAGE_SIZE);
    }
  });

  test('the request carries the bounds, and the search only when there is one', () => {
    expect(fleetPageUrl({ search: '', page: 0, pageSize: 25 })).toBe('/api/fleet?offset=0&limit=25');
    expect(fleetPageUrl({ search: '   ', page: 2, pageSize: 50 })).toBe('/api/fleet?offset=100&limit=50');
    // Trimmed as the server trims, and encoded: a search is user text.
    expect(fleetPageUrl({ search: ' a&b=c ', page: 1, pageSize: 25 })).toBe(
      '/api/fleet?q=a%26b%3Dc&offset=25&limit=25'
    );
    expect(fleetPageUrl({ search: 'db', page: 0, pageSize: 25, anchor: 'db' })).toBe(
      '/api/fleet?q=db&offset=0&limit=25&anchor=db'
    );
  });

  test('page count never drops below one', () => {
    expect(pageCount(0, 25)).toBe(1);
    expect(pageCount(25, 25)).toBe(1);
    expect(pageCount(26, 25)).toBe(2);
    expect(pageCount(10000, 25)).toBe(400);
  });

  test('a page past the end is moved to the last page, and one in range is left alone', () => {
    expect(lastPageIfPast(3, 40, 25)).toBe(1);
    expect(lastPageIfPast(5, 0, 25)).toBe(0);
    expect(lastPageIfPast(1, 40, 25)).toBeNull();
    expect(lastPageIfPast(0, 0, 25)).toBeNull();
  });

  test('the range names the nodes shown of those matched', () => {
    const page = { q: '', offset: 25, limit: 25, matched: 180, total: 10000 };
    expect(rangeLabel(page, 25)).toBe('26–50 of 180');
    expect(rangeLabel({ ...page, offset: 175 }, 5)).toBe('176–180 of 180');
    expect(rangeLabel({ ...page, matched: 0 }, 0)).toBe('0 of 0');
    // An empty page of a non-empty list -- the moment before the view moves
    // back into range -- does not claim a range it is not showing.
    expect(rangeLabel({ ...page, offset: 200 }, 0)).toBe('0 of 180');
  });
});
