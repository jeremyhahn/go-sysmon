// Wording for the fleet roll-up (#46).
//
// The server computes every figure; this module only turns them into the
// sentences the roll-up shows, so the rule each figure follows -- the worst
// value with its node named, or a sum with its denominator, never a mean --
// is visible in one place rather than spread through markup.
import { formatAge } from "./format";
import type {
  FleetBuild,
  FleetRollup,
  NodeAttention,
  TempSource,
} from "./types";

/** How each component kind is named beside a temperature. */
const TEMP_SOURCE_LABELS: Record<TempSource, string> = {
  cpu: "CPU",
  drive: "drive",
  gpu: "GPU",
  npu: "NPU",
};

/** "GPU", "drive" …, or "" when the kind is unknown. */
export function tempSourceLabel(kind: TempSource | undefined): string {
  return kind ? (TEMP_SOURCE_LABELS[kind] ?? "") : "";
}

/** How many nodes the "needs attention" list names before summarising. */
export const ATTENTION_LIMIT = 5;

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/**
 * The state count, with unreachable as its own term and how long the longest
 * absence has lasted: "11 nodes: 7 healthy, 2 warning, 1 critical,
 * 1 unreachable (no reading for 2h 14m)".
 *
 * Every term is always present, zeros included: "0 critical" is a checked
 * statement, and a term that appears only when non-zero reads as a different
 * sentence each poll.
 */
export function rollupHeadline(r: FleetRollup): string {
  let line =
    `${plural(r.nodes, "node", "nodes")}: ${r.healthy} healthy, ` +
    `${r.warning} warning, ${r.critical} critical, ${r.unreachable} unreachable`;
  const absence = absenceNote(r);
  if (absence !== "") line += ` (${absence})`;
  return line;
}

/**
 * Why the unreachable count is what it is. "1 unreachable" reads the same for
 * a node mid-restart and one dead since Tuesday; the duration is what tells
 * them apart. A node that never answered has no reading to be old, and is
 * said to have never answered rather than given a zero-second gap.
 */
export function absenceNote(r: FleetRollup): string {
  const parts: string[] = [];
  if (r.longest_absence_seconds > 0) {
    parts.push(`no reading for ${formatAge(r.longest_absence_seconds)}`);
  }
  if (r.never_answered > 0) {
    parts.push(
      r.never_answered === r.unreachable
        ? "never answered"
        : `${r.never_answered} never answered`
    );
  }
  return parts.join("; ");
}

/** "of 9 reachable nodes", or "of 10 nodes" when every node answered. */
export function coverage(of: number, r: FleetRollup): string {
  return r.unreachable > 0
    ? `of ${plural(of, "reachable node", "reachable nodes")}`
    : `of ${plural(of, "node", "nodes")}`;
}

/** The attention list as shown: the worst few, and how many were left out. */
export function visibleAttention(r: FleetRollup): {
  shown: NodeAttention[];
  more: number;
} {
  const all = r.attention ?? [];
  return {
    shown: all.slice(0, ATTENTION_LIMIT),
    more: Math.max(0, all.length - ATTENTION_LIMIT),
  };
}

/** What a node needs attention for, as one line. */
export function attentionReason(a: NodeAttention): string {
  if (a.status === "unreachable") {
    const since =
      a.age_seconds > 0
        ? `no reading for ${formatAge(a.age_seconds)}`
        : "never answered";
    return a.reasons.length > 0 ? `${since} — ${a.reasons[0]}` : since;
  }
  return a.reasons.length > 0 ? a.reasons.join("; ") : a.status;
}

/**
 * The builds line: "10 on a46f6cf, 1 on c00eb87 (gitea)". Nodes are named for
 * every group but the largest, which is the fleet's common build and would
 * otherwise list most of the fleet -- unless the largest is tied, when no build
 * is the common one and every group is named. Identity, not health -- it is
 * never coloured.
 */
export function buildsLine(builds: FleetBuild[]): string {
  const common =
    builds.length > 1 && builds[0].nodes.length > builds[1].nodes.length;
  return builds
    .map((b, i) => {
      const commit = b.commit === "" ? "an unstated build" : b.commit;
      const named =
        builds.length > 1 && (i > 0 || !common)
          ? ` (${b.nodes.join(", ")})`
          : "";
      return `${b.nodes.length} on ${commit}${named}`;
    })
    .join(", ");
}
