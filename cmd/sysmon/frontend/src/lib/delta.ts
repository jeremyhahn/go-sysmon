// Applying the deltas the event stream sends.
//
// This is the browser half of pkg/delta. A snapshot of a busy host is 270 kB of
// JSON, of which about three quarters is the process list and most of the rest
// is hardware description that never changes, so the server sends what moved
// instead of sending all of it to every client on every tick.
//
// The format is described in pkg/delta/delta.go. A patch is a tree mirroring
// the snapshot's shape, holding only what changed, and exactly one field of a
// node says how to apply it:
//
//   {"r": value}   replace this node
//   {"o": {...}}   an object: apply each named sub-patch, leave the rest alone
//   {"i": {...}}   an array of equal length: apply by position
//   {"a": {...}}   an array whose elements have an identity: apply by it
//
// The two implementations are held to one committed set of cases --
// tests/fixtures/delta-cases.json, generated from the Go differ -- because
// nothing else connects them, and a disagreement would show up as a dashboard
// quietly rendering the wrong numbers rather than as anything failing.

/** Patch is one node of a delta. Exactly one field is set. */
export interface Patch {
  /** Replace this node outright. Present-and-null is a real change: the field
   * is `undefined` when there is no replacement, `null` when the new value is
   * JSON null. */
  r?: unknown;
  /** Patch these fields of an object. */
  o?: Record<string, Patch>;
  /** Patch these positions of an array. */
  i?: Record<string, Patch>;
  /** Patch this array by element identity. */
  a?: KeyedPatch;
}

/** KeyedPatch describes an array whose elements are matched by an identity
 * field rather than by position. */
export interface KeyedPatch {
  /** The field that identifies an element, e.g. "pid". It travels in the patch
   * rather than being looked up here: a table kept on both sides would apply
   * the wrong key the moment the two drifted, and would do it silently. */
  k: string;
  /** The identities of the new array, in order. Sent only when the order
   * changed -- which, since adding or removing an element changes it too, means
   * it is present whenever membership did. */
  ord?: string[];
  /** Changes to elements that survived, by identity. */
  p?: Record<string, Patch>;
  /** Elements that were not there before, whole, by identity. */
  n?: Record<string, unknown>;
}

/**
 * DeltaError reports a patch that does not fit the value it was applied to.
 *
 * It means this client's copy and the server's have diverged, and no later
 * patch can repair that. The only correct response is to start again from a
 * whole snapshot -- which is why this throws rather than merging what it can. A
 * partly applied patch leaves numbers on screen that look measured and are not,
 * and that is the one failure this stream must never produce.
 */
export class DeltaError extends Error {
  readonly path: string;
  constructor(path: string, reason: string) {
    super(`delta: cannot apply a patch at ${path || "the snapshot root"}: ${reason}`);
    this.name = "DeltaError";
    this.path = path;
  }
}

/**
 * applyPatch returns the value a patch describes.
 *
 * The input is never modified: a new value is built, sharing the parts that did
 * not change. The caller keeps its previous snapshot, which matters because
 * Svelte compares object identity to decide what to re-render -- an in-place
 * mutation would update nothing.
 */
export function applyPatch(value: unknown, patch: Patch | null | undefined): unknown {
  return apply(value, patch, "");
}

function apply(value: unknown, patch: Patch | null | undefined, path: string): unknown {
  if (patch == null) return value;

  // Exactly one operation, counted rather than dispatched on. The chain below
  // took the first it recognised, so a frame carrying two -- a root replacement
  // and an object patch, say -- applied one and silently discarded the other,
  // rendering a tree the sender never described and reporting it current.
  // Which one won also depended on the order of these lines, so the two
  // implementations could disagree while both "succeeded".
  const replaces = "r" in patch; // present at all, including when null
  const operations =
    Number(replaces) + Number(patch.o != null) + Number(patch.i != null) + Number(patch.a != null);
  if (operations > 1) {
    throw new DeltaError(path, "the patch names more than one operation");
  }

  if (replaces) return patch.r;
  if (patch.o != null) return applyObject(value, patch.o, path);
  if (patch.i != null) return applyIndex(value, patch.i, path);
  if (patch.a != null) return applyKeyed(value, patch.a, path);

  throw new DeltaError(path, "the patch names no operation");
}

function applyObject(
  value: unknown,
  fields: Record<string, Patch>,
  path: string
): unknown {
  if (!isObject(value)) {
    throw new DeltaError(path, "an object patch met something that is not an object");
  }
  const out: Record<string, unknown> = { ...value };
  for (const [name, sub] of Object.entries(fields)) {
    const at = path === "" ? name : `${path}.${name}`;
    // own(), not `name in out`. `in` walks the prototype chain, so a patch
    // naming "toString" or "constructor" would be accepted against an object
    // that has no such field -- while pkg/delta, which looks the name up in a
    // map, refuses it. Every membership test on this side has to be the map
    // lookup the other side makes, or the two formats are not the same format.
    if (!own(out, name)) {
      throw new DeltaError(at, "the patch names a field that is not there");
    }
    out[name] = apply(out[name], sub, at);
  }
  return out;
}

/**
 * own reports whether an object carries a key itself.
 *
 * JSON.parse produces objects with the ordinary prototype, so `patch.p["constructor"]`
 * is a function rather than undefined and `"toString" in value` is true for
 * every object. Go's maps have no prototype and neither mistake exists there.
 * A network interface or thermal zone named by the host supplies these
 * identities as data, so this is reachable without anything being wrong.
 */
function own(value: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key);
}

/**
 * POSITION is a canonical unsigned decimal index, which is what pkg/delta
 * requires and what strconv.Itoa writes.
 *
 * Not merely "what Atoi accepts": Atoi reads "0", "+0", "-0" and "00" as the
 * same index, so a patch could carry two replacements for one element and which
 * one won would depend on map iteration order in Go and property order here.
 * Two receivers of one frame, disagreeing, with neither able to notice.
 */
const POSITION = /^(0|[1-9][0-9]*)$/;

function applyIndex(
  value: unknown,
  elements: Record<string, Patch>,
  path: string
): unknown {
  if (!Array.isArray(value)) {
    throw new DeltaError(path, "a positional patch met something that is not an array");
  }
  const out = value.slice();
  for (const [position, sub] of Object.entries(elements)) {
    // Parsed the way Go parses it, not the way JavaScript would. Number()
    // accepts "1e0", "0x1", " 1" and "" -- which is 0 -- while pkg/delta calls
    // every one of them malformed. A frame this side applies and the server's
    // own decoder rejects is not the same format.
    const i = POSITION.test(position) ? Number(position) : NaN;
    if (!Number.isInteger(i) || i < 0 || i >= out.length) {
      throw new DeltaError(
        path,
        `the patch names position ${position}, which this array does not have`
      );
    }
    out[i] = apply(out[i], sub, `${path}[]`);
  }
  return out;
}

function applyKeyed(value: unknown, patch: KeyedPatch, path: string): unknown {
  if (!Array.isArray(value)) {
    throw new DeltaError(path, "a keyed patch met something that is not an array");
  }
  if (!patch.k) {
    throw new DeltaError(path, "the keyed patch names no key");
  }

  const existing = new Map<string, unknown>();
  for (const element of value) {
    const id = identity(element, patch.k);
    if (id == null || existing.has(id)) {
      throw new DeltaError(
        path,
        `the array does not identify its elements by "${patch.k}"`
      );
    }
    existing.set(id, element);
  }

  // The order, when present, is the authoritative membership: anything not
  // named in it is gone. When absent the array kept its shape and only its
  // elements moved.
  const order =
    patch.ord ??
    value.map((element) => identity(element, patch.k) as string);

  // Every identity the sender named has to be consumed by the order, and each
  // one exactly once.
  //
  // Without this a patch can be half-interpreted and still report success: an
  // entry in "p" or "n" whose identity the order never mentions is simply never
  // looked up, the element keeps the value it already had, and the frame counts
  // as applied. The dashboard then shows a reading that looks measured and is
  // stale, which is the one outcome this stream must never produce. An identity
  // can only fail to match if the two sides spell it differently -- the sender
  // refuses any that could -- so reaching this is a bug somewhere, and a resync
  // is the right answer to it.
  const used = new Set<string>();
  const out: unknown[] = [];
  for (const id of order) {
    if (used.has(id)) {
      throw new DeltaError(path, `the patch names element "${id}" twice`);
    }
    used.add(id);

    if (patch.n != null && own(patch.n, id)) {
      const supplied = patch.n[id];
      // The element has to BE the one the patch says it is. Its position in
      // "n" is a claim about its identity and nothing checked the claim
      // against the element, so {"ord":["2"],"n":{"2":{"pid":999}}} placed an
      // element whose identity is 999 under the key 2 and counted the frame as
      // applied.
      const actual = identity(supplied, patch.k);
      if (actual !== id) {
        throw new DeltaError(
          path,
          `the patch supplies element "${id}" whose own "${patch.k}" says it is "${actual}"`
        );
      }
      // "n" means new. An identity the array already has is a claim the
      // differ never makes.
      if (existing.has(id)) {
        throw new DeltaError(
          path,
          `the patch supplies element "${id}" as new, and the array already has it`
        );
      }
      out.push(supplied);
      continue;
    }
    if (!existing.has(id)) {
      throw new DeltaError(
        path,
        `the patch keeps element "${id}", which is neither present nor supplied`
      );
    }
    const sub = patch.p != null && own(patch.p, id) ? patch.p[id] : undefined;
    const patched = apply(existing.get(id), sub, `${path}[]`);
    // A survivor must still be the element it was addressed as: a sub-patch
    // can rewrite the key field itself, and the result is an array the sender
    // does not have, reported as applied.
    const stillIs = identity(patched, patch.k);
    if (stillIs !== id) {
      throw new DeltaError(
        path,
        `the patch changes element "${id}" into one whose "${patch.k}" says it is "${stillIs}"`
      );
    }
    out.push(patched);
  }

  for (const id of Object.keys(patch.n ?? {})) {
    if (!used.has(id)) {
      throw new DeltaError(
        path,
        `the patch supplies element "${id}", which its order never places`
      );
    }
  }
  for (const id of Object.keys(patch.p ?? {})) {
    if (!used.has(id)) {
      throw new DeltaError(
        path,
        `the patch changes element "${id}", which this array does not contain`
      );
    }
    // An identity both supplied whole and patched names one element twice, and
    // only the whole one is used: the change would be dropped.
    if (patch.n != null && own(patch.n, id)) {
      throw new DeltaError(path, `the patch both supplies and changes element "${id}"`);
    }
  }
  return out;
}

/**
 * How many decimal digits stay exact through a browser's number type; the same
 * constant as pkg/delta's maxExactDigits, and it has to be.
 */
const MAX_EXACT_DIGITS = 15;

/** identity renders an element's key field, or null when it cannot be one. */
function identity(element: unknown, key: string): string | null {
  if (!isObject(element)) return null;
  const value = element[key];
  switch (typeof value) {
    case "string":
      return value;
    case "number":
      // The same numbers pkg/delta accepts, and no others: a plain integer
      // small enough to survive a float64 exactly, spelled the one way both
      // sides spell it. Without this the browser accepted identities Go
      // refuses -- 1e16, or an unsafe integer silently rounded and then
      // matched under the rounded value.
      //
      // One asymmetry is not closable here and is deliberate: Go reads the
      // JSON TEXT, so it refuses a valid but non-canonical spelling such as
      // "1e3" or "1.0" outright, while JSON.parse has already normalised those
      // to 1000 and 1 before this sees them. It costs nothing in practice --
      // the differ only emits a keyed patch when the Go side accepted every
      // identity, so a frame carrying such a spelling did not come from one.
      //
      // ("007" is NOT an example of this: JSON forbids a leading zero, so both
      //  decoders reject it before any of this runs. Go's own leading-zero
      //  check guards input that did not come through a JSON decoder at all.)
      if (!Number.isSafeInteger(value)) return null;
      if (Object.is(value, -0)) return null;
      if (Math.abs(value).toString().length > MAX_EXACT_DIGITS) return null;
      return String(value);
    case "boolean":
      return value ? "true" : "false";
    default:
      return null;
  }
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
