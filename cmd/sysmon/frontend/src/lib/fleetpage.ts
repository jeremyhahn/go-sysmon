// Paging the fleet view (#15).
//
// The server searches, orders and pages the fleet; GET /api/fleet returns one
// page and the counts to draw the controls from. This module is the browser's
// half of that contract -- the request it makes and the arithmetic over the
// counts it gets back -- kept apart from the store and the view so it can be
// tested without either.

import type { FleetPage } from "./types";

// Relative to the page, like every API URL: see EVENTS_URL in transport.ts.
export const FLEET_URL = "api/fleet";

/** The page sizes the view offers. */
export const PAGE_SIZE_OPTIONS = [25, 50, 100, 250];
export const DEFAULT_PAGE_SIZE = 25;

/** The page the viewer has asked for: a search, a zero-based page index and a
 * page size. */
export interface FleetPageRequest {
  search: string;
  page: number;
  pageSize: number;
  /** A node whose page to open instead of `page`, once: the server knows
   * where it falls among the matches, which a search for its name alone
   * does not say. Cleared when the answer arrives. */
  anchor?: string;
}

/** A stored page size is honoured only if it is still one of the options. */
export function parsePageSize(raw: string | null): number {
  const parsed = raw == null ? Number.NaN : Number.parseInt(raw, 10);
  return PAGE_SIZE_OPTIONS.includes(parsed) ? parsed : DEFAULT_PAGE_SIZE;
}

/** The request URL for one page. The search is trimmed here as the server
 * would trim it, and left out when empty. */
export function fleetPageUrl(req: FleetPageRequest): string {
  const params = new URLSearchParams();
  const search = req.search.trim();
  if (search !== "") params.set("q", search);
  params.set("offset", String(req.page * req.pageSize));
  params.set("limit", String(req.pageSize));
  if (req.anchor) params.set("anchor", req.anchor);
  return `${FLEET_URL}?${params.toString()}`;
}

/** How many pages the matching nodes fill; never fewer than one, so an empty
 * result still has a page to be on. */
export function pageCount(matched: number, pageSize: number): number {
  return Math.max(1, Math.ceil(matched / pageSize));
}

/**
 * lastPageIfPast returns the last page's index when page lies past the end of
 * the matches -- a node left the fleet, or a search narrowed, under a page the
 * viewer was already on -- and null when page is still in range.
 */
export function lastPageIfPast(
  page: number,
  matched: number,
  pageSize: number
): number | null {
  const last = pageCount(matched, pageSize) - 1;
  return page > last ? last : null;
}

/** "26–50 of 180": the nodes this page shows, of those that matched. */
export function rangeLabel(p: FleetPage, shown: number): string {
  if (p.matched === 0 || shown === 0) return `0 of ${p.matched}`;
  return `${p.offset + 1}–${p.offset + shown} of ${p.matched}`;
}
