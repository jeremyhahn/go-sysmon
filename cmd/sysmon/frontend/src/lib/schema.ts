/**
 * Validating a frame against the shape the server actually declares.
 *
 * The schema itself is generated from pkg/types/types.go (schema.generated.ts).
 * This module holds the two things a generator cannot know: what the walk
 * means, and how much of it the dashboard insists on.
 *
 * ## Why this exists
 *
 * `JSON.parse(...) as Snapshot` is a promise to the compiler that cannot fail
 * at runtime. Everything the page reads afterwards — `.map()`, `.join()`,
 * arithmetic — assumes that promise was kept. The old check enumerated the
 * snapshot's sections and lists by dotted path and stopped at the container
 * level, so `networks: [{ flags: "x" }]` was accepted and the page threw on
 * `.join()`; `{ bytes_sent_rate: "x" }` was accepted, coerced through
 * arithmetic without throwing, and rendered as a number while the page reported
 * itself connected and current. Descending into elements by hand meant
 * restating the whole recursive type in a second dialect, which is the copy
 * this change removes rather than doubles (#38).
 *
 * ## How strict, and why not stricter
 *
 * Two rules, and the difference between them is the whole design:
 *
 * 1. **Presence is required only where the old check required it** — the
 *    snapshot's own sections and lists, named in REQUIRED_SECTIONS and
 *    REQUIRED_ARRAYS below. Those are what
 *    the store walks with no guard at all.
 * 2. **Everywhere else, a value is checked only if it is there.** A key the
 *    frame does not carry is not an error.
 *
 * Rule 2 is what keeps this safe to make strict. A field added on the Go side
 * is an unknown key, and unknown keys are ignored. A field renamed on the Go
 * side arrives under its new name and the old one is simply absent — one panel
 * renders undefined, visibly, in one cell. Neither refuses the frame.
 *
 * That matters because the failure mode of an over-strict validator is worse
 * than the one it prevents: refusing every frame blanks the whole dashboard and
 * looks, from the outside, exactly like a connection that will not stay up.
 * What IS refused is a value of the wrong kind — a string where a list belongs,
 * a list where an object belongs — because that is the shape that renders as a
 * plausible number and is never noticed.
 *
 * ## Saying so when it happens
 *
 * Every refusal names the path and what was expected there
 * ("networks[3].flags: expected array, got string"). A refused frame costs a
 * resync, and a systematically refused shape would otherwise present as a
 * reconnect loop with nothing in the log to distinguish it from a flapping
 * server. $lib/transport carries the reason out to reportGap, which says which
 * of the two it is.
 */

import { SCHEMA, type FieldSchema } from "./schema.generated";
import type { Snapshot } from "./types";

// The vocabulary of shapes belongs to the generator, so it is declared where
// the data is. Re-exported here so a caller has one import for the walk and for
// the types it walks over.
export type { FieldSchema, FieldEntry, TypeSchema } from "./schema.generated";

/**
 * REQUIRED_ARRAYS are the snapshot's own lists the store spreads and reduces
 * with no guard at all — `snapshot.cpus.reduce(...)`, `[...snapshot.networks]`,
 * `snapshot.disks.filter(...)`. For those, null is as unusable as a string.
 *
 * The page assuming that is older than any validator here and is not changed by
 * this; what the check does is refuse the frame rather than let it throw
 * mid-render.
 */
export const REQUIRED_ARRAYS: readonly string[] = ["cpus", "disks", "networks"];

/**
 * REQUIRED_SECTIONS are the object-valued sections the page reads through
 * without a guard, as dotted paths. Go declares them as structs rather than
 * pointers, so a server that is running sends every one of them.
 *
 * This is the old validator's list unchanged. Presence is the one thing the
 * generated schema cannot decide: it knows a struct is always marshalled, but
 * not whether THIS build's page would survive an older server that predates the
 * field. Keeping the list is what makes this change strictly-more-checking
 * rather than differently-checking.
 */
export const REQUIRED_SECTIONS: readonly string[] = [
  "host",
  "cpu_summary",
  "memory",
  "load_avg",
  "processes",
  "sensors",
  "sensors.psi",
  "sensors.psi.cpu",
  "sensors.psi.memory",
  "sensors.psi.io",
  "virtualization",
  "virtualization.runtime",
  "virtualization.capability",
];

/**
 * OPTIONAL_SECTIONS are the sections allowed to be missing entirely, and their
 * members are then not required either.
 *
 * A kernel without pressure-stall information does not report it and the page
 * guards for that — but not in halves: psiRows reads `some_avg10` off whatever
 * it is handed. So `sensors.psi` absent is fine, and `sensors.psi` present
 * without `cpu` is not, which is why it is named here rather than simply
 * dropped from the list above.
 */
export const OPTIONAL_SECTIONS: readonly string[] = [
  "sensors.psi",
  // #187: an older build does not report runtime or hypervisor detection at
  // all, which the tab rule reads as "unknown" and shows the tab.
  "virtualization.container_runtime",
  "virtualization.hypervisor",
];

/** isObject is a plain JSON object: not null, not an array. */
export function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** describe names what a value actually is, for a refusal message. */
function describe(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value;
}

/**
 * checkValue validates one value against its declared shape, returning null
 * when it holds and a reason naming `path` when it does not.
 *
 * Returning the first failure rather than collecting them all is deliberate: a
 * frame is refused whole, so the second reason changes nothing and the first
 * one is the one worth reading.
 */
export function checkValue(value: unknown, shape: FieldSchema, path: string): string | null {
  if (value === null) {
    // A nil Go slice or pointer. Only the schema can say whether that is a
    // machine reporting it has none of something or a malformed frame.
    if (shape.nullable) return null;
    // ...and an optional section is allowed to arrive as an explicit null as
    // well as be missing. The old check read both through one `== null`, so
    // refusing the null half here would be a regression dressed as strictness.
    if (OPTIONAL_SECTIONS.includes(path)) return null;
    return `${path}: expected ${shape.kind}, got null`;
  }

  switch (shape.kind) {
    case "string":
      return typeof value === "string" ? null : `${path}: expected string, got ${describe(value)}`;
    case "number":
      // NaN and Infinity do not survive JSON, so any number here is finite.
      return typeof value === "number" ? null : `${path}: expected number, got ${describe(value)}`;
    case "boolean":
      return typeof value === "boolean"
        ? null
        : `${path}: expected boolean, got ${describe(value)}`;
    case "object":
      if (!isObject(value)) return `${path}: expected object, got ${describe(value)}`;
      return checkType(value, shape.ref, path);
    case "array": {
      if (!Array.isArray(value)) return `${path}: expected array, got ${describe(value)}`;
      for (let i = 0; i < value.length; i++) {
        // A null inner list is handed on as [], for the same reason checkType
        // does it for a field: the generated types spell it T[][].
        if (value[i] === null && shape.elem.kind === "array") {
          value[i] = [];
          continue;
        }
        const reason = checkValue(value[i], shape.elem, `${path}[${i}]`);
        if (reason !== null) return reason;
      }
      return null;
    }
  }
}

/**
 * checkType validates an object against a named wire type.
 *
 * A key the object does not carry is skipped rather than refused — see rule 2
 * above. A key the type does not declare is skipped too: the server is allowed
 * to grow, and refusing a frame for carrying more than this build knows about
 * would make every deploy an outage on the older half of the fleet.
 */
export function checkType(value: Record<string, unknown>, type: string, path: string): string | null {
  const fields = SCHEMA[type];
  if (fields === undefined) {
    // Only reachable if the generated schema and the code that names a type
    // disagree, which means the two were built from different trees.
    return `${path}: no schema for type ${type}`;
  }
  for (const field of fields) {
    const held = value[field.key];
    if (held === undefined) continue;
    // A list that arrives as null becomes an empty one. This build never
    // sends a null list (#151), but a peer on an older build can, and the
    // interfaces spell every list `T[]`: an accepted null left in place would
    // be a `.map` on null behind a type that says it cannot happen.
    if (held === null && field.type.kind === "array") {
      value[field.key] = [];
      continue;
    }
    const reason = checkValue(held, field.type, path === "" ? field.key : `${path}.${field.key}`);
    if (reason !== null) return reason;
  }
  return null;
}

/**
 * checkSnapshot validates a whole frame, returning null when it is a snapshot
 * and a reason when it is not.
 *
 * Presence is enforced here and only here, for the containers listed above.
 * Everything below them is checked by shape alone.
 */
export function checkSnapshot(value: unknown): string | null {
  if (!isObject(value)) return `snapshot: expected object, got ${describe(value)}`;
  if (typeof value.timestamp !== "string") {
    return `timestamp: expected string, got ${describe(value.timestamp)}`;
  }

  for (const path of REQUIRED_SECTIONS) {
    if (optionalAndAbsent(value, path)) continue;
    const held = at(value, path);
    if (!isObject(held)) return `${path}: expected object, got ${describe(held)}`;
  }
  for (const key of REQUIRED_ARRAYS) {
    if (!Array.isArray(value[key])) {
      return `${key}: expected array, got ${describe(value[key])}`;
    }
  }

  return checkType(value, "Snapshot", "");
}

/**
 * at walks a dotted path, returning undefined at the first step that is not an
 * object rather than throwing.
 */
function at(value: Record<string, unknown>, path: string): unknown {
  let held: unknown = value;
  for (const step of path.split(".")) {
    if (!isObject(held)) return undefined;
    held = held[step];
  }
  return held;
}

/**
 * optionalAndAbsent reports a section this machine does not have, whose members
 * are therefore not required either.
 */
function optionalAndAbsent(value: Record<string, unknown>, path: string): boolean {
  for (const section of OPTIONAL_SECTIONS) {
    if (path !== section && !path.startsWith(section + ".")) continue;
    if (at(value, section) == null) return true;
  }
  return false;
}

/** A validated frame, or the reason it was refused. */
export type SnapshotCheck =
  | { ok: true; snapshot: Snapshot }
  | { ok: false; reason: string };

/**
 * validateSnapshot is checkSnapshot in the form a caller can use without a
 * cast.
 *
 * `value as Snapshot` after a successful check is a second promise to the
 * compiler beside the one the check just made good, and the two can drift: the
 * cast keeps compiling after the check stops covering the type. Returning the
 * narrowed value from the function that did the checking cannot.
 */
export function validateSnapshot(value: unknown): SnapshotCheck {
  const reason = checkSnapshot(value);
  if (reason !== null) return { ok: false, reason };
  return { ok: true, snapshot: value as Snapshot };
}
