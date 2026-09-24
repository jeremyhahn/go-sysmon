package fleet

import "strings"

// Node ordering.
//
// The fleet list is ordered here, and only here: GET /api/fleet searches and
// pages the fleet server-side (#15), so the dashboard shows the order it is
// sent. Until then the browser sorted the list as well, and the two had to
// agree -- which is where this rule comes from, and why it is the rule it is.
//
// Go compares strings bytewise; JavaScript's localeCompare is locale-aware and
// collates "a" before "B" where Go puts "B" first, and orders accented
// characters differently again. Node names mix case and punctuation -- a peer
// name may carry '.', '_', ':' and '-', and this node's own name is whatever
// fleet.local_name or the hostname says -- so that was reachable rather than
// theoretical. The rule was chosen to be implementable identically in any
// language, with no locale data and no library, and it stays so: a consumer of
// the API that sorts nodes itself can match it exactly.
//
//  1. Fold ASCII letters to lower case. Only A-Z, nothing else: it is the
//     entire difference between "B" and "b" for realistic host names, and it is
//     the one case mapping every language agrees on.
//  2. Compare by code point. Go's bytewise comparison of valid UTF-8 IS
//     code-point order; JavaScript's "<" is UTF-16 code-unit order, which
//     differs above the basic plane.
//  3. Break ties on the unfolded name, so "NODE" and "node" have a stable
//     order rather than an arbitrary one.
//
// testdata/order.json pins the rule with cases chosen to distinguish it from
// every plausible wrong one; the search in query.go folds by the same rule.

// foldASCII lower-cases A-Z and leaves every other byte alone.
func foldASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		b.WriteByte(foldByte(s[i]))
	}
	return b.String()
}

// foldByte lower-cases one byte if it is A-Z. Every byte of a multi-byte UTF-8
// sequence is 0x80 or above, so folding byte by byte never touches one.
func foldByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// CompareNodeNames orders two node names. It returns a negative number, zero or
// a positive number, matching the shape of a JavaScript comparator so the two
// implementations read the same.
//
// The folded comparison is made byte by byte rather than on folded copies: it
// runs O(n log n) times for every fleet request, and a copy of both names per
// comparison was a fifth of what serving a ten-thousand-node page cost. Folding
// maps each byte on its own, so comparing folded bytes is comparing the folded
// strings.
func CompareNodeNames(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if fa, fb := foldByte(a[i]), foldByte(b[i]); fa != fb {
			if fa < fb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
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
