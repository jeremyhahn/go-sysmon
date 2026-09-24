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

/** How many nodes the attention list names: the server's bound,
 * types.RollupNames. */
export const ATTENTION_SHOWN = 5;

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

/**
 * What the fleet tree could not compose (#190): subtrees left out entirely,
 * and nodes sharing a name. "" when there is nothing to say.
 */
export function treeNotes(r: FleetRollup): string[] {
  const notes: string[] = [];
  const refused = r.subtree_errors ?? 0;
  if (refused > 0) {
    notes.push(
      `${plural(refused, "aggregator's fleet is", "aggregators' fleets are")} not shown: see ${refused === 1 ? "its card" : "their cards"}`
    );
  }
  const shared = r.name_conflicts ?? 0;
  if (shared > 0) {
    notes.push(`${plural(shared, "node shares", "nodes share")} a name with another node and cannot be opened`);
  }
  return notes;
}

/** "of 9 reachable nodes", or "of 10 nodes" when every node answered. */
export function coverage(of: number, r: FleetRollup): string {
  return r.unreachable > 0
    ? `of ${plural(of, "reachable node", "reachable nodes")}`
    : `of ${plural(of, "node", "nodes")}`;
}

/**
 * The attention list as shown: the worst few, as the server sends them, and
 * how many it did not send. The server bounds the list; the state counts are
 * whole-fleet, so every node needing attention is in exactly one of them.
 */
export function visibleAttention(r: FleetRollup): {
  shown: NodeAttention[];
  more: number;
} {
  // The server sends at most ATTENTION_SHOWN. The slice is for a server from
  // before #15, which sent the whole list -- a tab left open across a
  // rollback must not list a thousand nodes.
  const shown = (r.attention ?? []).slice(0, ATTENTION_SHOWN);
  const needing = r.critical + r.unreachable + r.warning;
  return { shown, more: Math.max(0, needing - shown.length) };
}

/** What a node needs attention for, as one line. */
export function attentionReason(a: NodeAttention): string {
  if (a.status === "unreachable") {
    const since =
      a.age_seconds > 0
        ? `no reading for ${formatAge(a.age_seconds)}`
        : "never answered";
    // A child aggregator takes its whole subtree with it (#190): those nodes
    // are unknown too, and saying how many is what makes one line honest.
    const below =
      (a.subtree ?? 0) > 0
        ? `; ${plural(a.subtree ?? 0, "node", "nodes")} below it unknown`
        : "";
    return a.reasons.length > 0
      ? `${since}${below} — ${a.reasons[0]}`
      : `${since}${below}`;
  }
  return a.reasons.length > 0 ? a.reasons.join("; ") : a.status;
}

/**
 * The builds line: "10 on a46f6cf, 1 on c00eb87 (gitea)". Nodes are named for
 * every group but the largest, which is the fleet's common build and would
 * otherwise list most of the fleet -- unless the largest is tied, when no build
 * is the common one and every group is named. Identity, not health -- it is
 * never coloured.
 *
 * The server sends at most a few groups and names at most a few nodes in each,
 * with the true sizes beside them, so a large fleet mid-rollout reads
 * "6000 on a46f6cf, 4000 on c00eb87 (n1, n2, n3, n4, n5 and 3995 more)"
 * rather than listing four thousand names.
 */
export function buildsLine(
  builds: FleetBuild[],
  groups: number = builds.length
): string {
  const common =
    builds.length > 1 && sizeOf(builds[0]) > sizeOf(builds[1]);
  const line = builds
    .map((b, i) => {
      const commit = b.commit === "" ? "an unstated build" : b.commit;
      const named =
        builds.length > 1 && (i > 0 || !common) ? ` (${namesOf(b)})` : "";
      return `${sizeOf(b)} on ${commit}${named}`;
    })
    .join(", ");
  const unsent = groups - builds.length;
  return unsent > 0
    ? `${line}, and ${plural(unsent, "other build", "other builds")}`
    : line;
}

/** A group's size. A server from before #15 sent no count, and named every
 * node in the group instead, so the names are the count. */
function sizeOf(b: FleetBuild): number {
  return b.count ?? b.nodes.length;
}

/** "a, b, c", or "a, b, c and 7 more" when the group has more than it named. */
function namesOf(b: FleetBuild): string {
  const unnamed = sizeOf(b) - b.nodes.length;
  const names = b.nodes.join(", ");
  return unnamed > 0 ? `${names} and ${unnamed} more` : names;
}
