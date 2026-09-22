package fleet

import "strings"

// Node ordering.
//
// The fleet list is sorted in two places -- here, for GET /api/fleet, and in
// the browser, for the view. They have to produce the SAME order or the API and
// the screen disagree about what the fleet looks like, which is the thing this
// ordering exists to prevent.
//
// That is harder than it sounds across two languages. Go compares strings
// bytewise; JavaScript's localeCompare is locale-aware and collates "a" before
// "B" where Go puts "B" first, and orders accented characters differently
// again. Node names mix case and punctuation -- a peer name may carry '.', '_',
// ':' and '-', and this node's own name is whatever fleet.local_name or the
// hostname says -- so this is reachable rather than theoretical.
//
// The rule below is therefore chosen to be implementable identically in both,
// with no locale data and no library:
//
//  1. Fold ASCII letters to lower case. Only A-Z, nothing else: it is the
//     entire difference between "B" and "b" for realistic host names, and it is
//     the one case mapping both languages are guaranteed to agree on. Unicode
//     case folding is not used precisely because Go and JavaScript can disagree
//     about it.
//  2. Compare by code point. Not by byte, not by UTF-16 code unit -- Go's
//     bytewise comparison of valid UTF-8 IS code-point order, and JavaScript's
//     "<" is code-unit order, which differs above the basic plane. Comparing
//     code points explicitly is the only form both can implement exactly.
//  3. Break ties on the unfolded name, so "NODE" and "node" have a stable
//     order rather than an arbitrary one.
//
// cmd/sysmon/frontend/src/lib/nodeorder.ts is the other half. Both are checked
// against testdata/order.json, which is generated here, because nothing else
// connects them: they would drift silently, and the symptom would be a list
// that looks different depending on whether you read it through the API.

// foldASCII lower-cases A-Z and leaves every other byte alone.
func foldASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// CompareNodeNames orders two node names. It returns a negative number, zero or
// a positive number, matching the shape of a JavaScript comparator so the two
// implementations read the same.
func CompareNodeNames(a, b string) int {
	fa, fb := foldASCII(a), foldASCII(b)
	if fa != fb {
		if fa < fb {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
