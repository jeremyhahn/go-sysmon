// Node ordering, the browser half.
//
// The fleet list is sorted twice: here for the view, and in pkg/fleet for
// GET /api/fleet. They must produce the SAME order or the API and the screen
// disagree about what the fleet looks like, which is the thing this ordering
// exists to prevent.
//
// This used to be `a.name.localeCompare(b.name)`, which is locale-aware and
// therefore cannot match Go: it puts "a" before "B" where Go's bytewise
// comparison puts "B" first, and orders accented characters differently again.
// Node names are unrestricted strings -- an unnamed peer defaults to its URL --
// so that was reachable, not theoretical.
//
// The rule is described once, in pkg/fleet/order.go. In short: fold A-Z to
// lower case and nothing else, compare by code point, tie-break on the
// unfolded name. Both halves are checked against pkg/fleet/testdata/order.json,
// which the Go side generates, because nothing else connects them.

/** foldASCII lower-cases A-Z and leaves every other character alone.
 *
 * Deliberately not toLowerCase(): that applies Unicode case mapping, which Go
 * and JavaScript can disagree about, and the whole point here is a rule both
 * implement identically. */
function foldASCII(s: string): string {
  let out = "";
  for (const ch of s) {
    const c = ch.codePointAt(0)!;
    out += c >= 0x41 && c <= 0x5a ? String.fromCodePoint(c + 0x20) : ch;
  }
  return out;
}

/**
 * compareCodePoints orders two strings by code point.
 *
 * Not `<`. JavaScript compares strings by UTF-16 code unit, so a character
 * above the basic plane -- encoded as a surrogate pair starting at 0xD800 --
 * sorts below one at 0xE000-0xFFFF, while Go's bytewise UTF-8 comparison puts
 * it above. Iterating code points is the only form that matches Go exactly.
 */
function compareCodePoints(a: string, b: string): number {
  const ca = Array.from(a);
  const cb = Array.from(b);
  const n = Math.min(ca.length, cb.length);
  for (let i = 0; i < n; i++) {
    const x = ca[i].codePointAt(0)!;
    const y = cb[i].codePointAt(0)!;
    if (x !== y) return x < y ? -1 : 1;
  }
  return ca.length === cb.length ? 0 : ca.length < cb.length ? -1 : 1;
}

/** compareNodeNames orders two node names, matching pkg/fleet exactly. */
export function compareNodeNames(a: string, b: string): number {
  const folded = compareCodePoints(foldASCII(a), foldASCII(b));
  return folded !== 0 ? folded : compareCodePoints(a, b);
}
