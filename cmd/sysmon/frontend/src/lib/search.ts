// Searching the recorded history.
//
// The owner asked, 2026-09-02, for "the ability to search across historical
// data points". The date range (#31) answered half of it -- a window over the
// charts that already exist -- and this is the other half: naming what you want
// to see rather than finding the panel it lives on.
//
// What is searched is the catalog crossed with the resources this node actually
// has. `disk_util` is one catalogued metric, but on a machine with six drives it
// is six answerable questions, and "sda" is what an operator types. The
// resources come from the live snapshot rather than from a new endpoint: the
// dashboard already knows every disk, interface, GPU and core, and asking
// Prometheus what labels exist would be a second source for the same list --
// one that can disagree with the machine in front of you.

import type { HistoryMetric, HistoryUnit } from "$lib/types";

/** A resource dimension and the values this node has for it. */
export type Resources = Record<string, string[]>;

/**
 * ResourceLabels names resources whose value is not what a person calls them,
 * by split label and value: a core's value is its history key, "1:0", and its
 * name "P1 · Core 0". A resource with no entry is called by its value.
 */
export type ResourceLabels = Record<string, Record<string, string>>;

/** One thing that can be charted: a metric, and the resource it is about. */
export interface SearchHit {
  /** The catalog key to request. */
  metric: string;
  /** The metric's own name, as the catalog gives it. */
  label: string;
  unit: HistoryUnit;
  /** The label a split metric is divided by, or "" when it is not split. */
  splitBy: string;
  /** The value of that label, or null for an unsplit metric. */
  resource: string | null;
  /** What the resource is called on screen: its value, unless the search was
   * given a name for it ("P1 · Core 0" for the core "1:0"). */
  resourceName: string | null;
  /** What the result is called on screen, and what makes it unique. */
  title: string;
}

/** The most hits a search will return, so a broad query cannot ask the server
 * for two hundred ranges at once. */
export const MAX_HITS = 24;

/**
 * searchHistory returns the series matching a query, best first.
 *
 * Ranking is by how specific the match is, because the two ends of the query
 * space want opposite things: "sda" is a resource and should bring back that
 * drive's metrics, while "cpu" is a metric and should bring back the metric.
 * Within a rank the order is the catalog's, which is stable -- a list that
 * reorders itself as you type is unusable.
 */
export function searchHistory(
  query: string,
  metrics: HistoryMetric[],
  resources: Resources,
  labels: ResourceLabels = {}
): SearchHit[] {
  const needle = query.trim().toLowerCase();
  if (needle === "") return [];

  const exactResource: SearchHit[] = [];
  const partialResource: SearchHit[] = [];
  const metricMatch: SearchHit[] = [];

  for (const metric of metrics) {
    const label = metric.label.toLowerCase();
    const key = metric.key.toLowerCase();
    const named = label.includes(needle) || key.includes(needle);

    if (metric.split_by === "") {
      if (named) metricMatch.push(hit(metric, null));
      continue;
    }

    const values = resources[metric.split_by] ?? [];
    const names = labels[metric.split_by] ?? {};
    for (const value of values) {
      const name = names[value] ?? value;
      const loweredValue = value.toLowerCase();
      const loweredName = name.toLowerCase();
      if (loweredValue === needle || loweredName === needle) {
        exactResource.push(hit(metric, value, name));
      } else if (loweredValue.includes(needle) || loweredName.includes(needle)) {
        partialResource.push(hit(metric, value, name));
      } else if (named) {
        metricMatch.push(hit(metric, value, name));
      }
    }

    // A split metric on a node with no such resources is still a real answer
    // when the metric itself was named -- "this node records disk_util and has
    // no disks" is worth showing as an empty chart rather than as no result,
    // which reads as "no history for that" and is a different claim.
    if (named && values.length === 0) {
      metricMatch.push(hit(metric, null));
    }
  }

  return [...exactResource, ...partialResource, ...metricMatch].slice(0, MAX_HITS);
}

function hit(metric: HistoryMetric, resource: string | null, name: string | null = resource): SearchHit {
  return {
    metric: metric.key,
    label: metric.label,
    unit: metric.unit,
    splitBy: metric.split_by,
    resource,
    resourceName: name,
    title: resource == null ? metric.label : metric.label + " \u2014 " + name,
  };
}

/** hitKey identifies a hit, so the same series cannot be pinned twice. */
export function hitKey(hit: SearchHit): string {
  return hit.metric + " " + (hit.resource ?? "");
}
