// Presentation helpers for backend-evaluated alerts.
//
// The backend decides *whether* something is wrong; this module only decides
// how that looks. Keeping the mapping in one place is what makes a critical
// disk, a critical interface and a critical node read identically.

import type { Alert, NodeStatus, Severity } from "./types";

/** Sort order: critical first, info last. */
const SEVERITY_RANK: Record<Severity, number> = {
  critical: 0,
  warning: 1,
  info: 2,
};

export function severityRank(severity: Severity): number {
  return SEVERITY_RANK[severity] ?? SEVERITY_RANK.info;
}

/** sortBySeverity returns a new array, critical → warning → info. */
export function sortBySeverity(alerts: Alert[]): Alert[] {
  return [...alerts].sort(
    (a, b) => severityRank(a.severity) - severityRank(b.severity)
  );
}

/** worstSeverity returns the highest severity present, or null when empty. */
export function worstSeverity(alerts: Alert[]): Severity | null {
  let worst: Severity | null = null;
  for (const a of alerts) {
    if (worst == null || severityRank(a.severity) < severityRank(worst)) {
      worst = a.severity;
    }
  }
  return worst;
}

/**
 * alertBorderClass highlights a card that owns an alerting resource. Returns
 * an empty string when nothing is firing, so it is safe to interpolate
 * unconditionally.
 */
export function alertBorderClass(alerts: Alert[]): string {
  switch (worstSeverity(alerts)) {
    case "critical":
      return "border border-error/40";
    case "warning":
      return "border border-warning/40";
    case "info":
      return "border border-info/40";
    default:
      return "";
  }
}

/**
 * severityDotClass is the class list for a severity indicator dot: its
 * background colour, plus `severity-dot`, which keeps the dot visible under
 * forced colours (app.css), where a background fill is otherwise removed.
 */
export function severityDotClass(severity: Severity | null): string {
  return `severity-dot ${severityDotFill(severity)}`;
}

function severityDotFill(severity: Severity | null): string {
  switch (severity) {
    case "critical":
      return "bg-error";
    case "warning":
      return "bg-warning";
    case "info":
      return "bg-info";
    default:
      return "bg-base-content/30";
  }
}

/** severityBadgeClass is the daisyUI badge modifier for a severity. */
export function severityBadgeClass(severity: Severity | null): string {
  switch (severity) {
    case "critical":
      return "badge-error";
    case "warning":
      return "badge-warning";
    case "info":
      return "badge-info";
    default:
      return "badge-neutral";
  }
}

/** severityTextClass is the text colour for a severity. */
export function severityTextClass(severity: Severity | null): string {
  switch (severity) {
    case "critical":
      return "text-error";
    case "warning":
      return "text-warning";
    case "info":
      return "text-info";
    default:
      return "text-base-content";
  }
}

/**
 * alertClass picks the daisyUI alert modifier for a banner summarising a group
 * of alerts — the banner takes the colour of its worst member.
 */
export function alertClass(alerts: Alert[]): string {
  switch (worstSeverity(alerts)) {
    case "critical":
      return "alert-error";
    case "warning":
      return "alert-warning";
    default:
      return "alert-info";
  }
}

// --- Fleet node status -----------------------------------------------------

export function nodeStatusBadgeClass(status: NodeStatus): string {
  switch (status) {
    case "unreachable":
      return "badge-neutral";
    case "critical":
      return "badge-error";
    case "warning":
      return "badge-warning";
    default:
      return "badge-success";
  }
}

/** nodeStatusBorderClass makes an unhealthy node card obvious at a glance. */
export function nodeStatusBorderClass(status: NodeStatus): string {
  switch (status) {
    case "unreachable":
      return "border-2 border-dashed border-base-content/40";
    case "critical":
      return "border border-error/60";
    case "warning":
      return "border border-warning/50";
    default:
      return "border border-transparent";
  }
}
