package fleet

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"sort"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/order.json")

const orderPath = "testdata/order.json"

// orderCase is a set of node names and the order this package puts them in.
type orderCase struct {
	Name   string   `json:"name"`
	Why    string   `json:"why"`
	Input  []string `json:"input"`
	Sorted []string `json:"sorted"`
}

// orderInputs are the name sets the order is pinned by.
//
// Every one of them is chosen to DISTINGUISH the rule from a plausible wrong
// one. A list of lowercase ASCII names sorts identically under bytewise
// comparison, under localeCompare and under anything else anyone might reach
// for -- which is precisely why the original tests could not see that the two
// layers disagreed.
var orderInputs = []struct {
	name  string
	why   string
	input []string
}{
	{
		name:  "mixed case",
		why:   "localeCompare puts a before B; a bytewise comparison puts B first. The fold makes both sides agree, and on the answer a person expects.",
		input: []string{"beta", "Alpha", "alpha", "BETA", "Gamma"},
	},
	{
		name:  "case-only difference",
		why:   "the fold makes these equal, so the tie-break on the unfolded name is what gives them a stable order at all",
		input: []string{"node", "NODE", "Node", "nOdE"},
	},
	{
		name:  "digits and separators against letters",
		why:   "hyphens, dots and digits sort below letters by code point; a locale collation may ignore punctuation entirely",
		input: []string{"vstation-10", "vstation-2", "vstation.1", "vstationa", "vstation"},
	},
	{
		name:  "URL-shaped punctuation",
		why:   "an unnamed peer was once named after its URL, and a node's own name is not held to the peer-name rule, so ':' '/' and '.' still have to order predictably",
		input: []string{"http://198.51.100.11:8081", "https://fs:8081", "fs", "HTTP://a"},
	},
	{
		name:  "accented characters",
		why:   "a locale collation files é next to e; comparing by code point puts every multi-byte character after all of ASCII",
		input: []string{"zurich", "évry", "Evry", "aachen"},
	},
	{
		name:  "above the basic plane",
		why:   "JavaScript's < compares UTF-16 code units, so a surrogate pair sorts below U+E000-U+FFFF while Go puts it above; comparing code points is the only form that matches",
		input: []string{"node￿", "node\U0001f600", "node", "node"},
	},
	{
		name:  "empty and prefix",
		why:   "a shorter name that is a prefix of a longer one must come first, and an empty name must not sort arbitrarily",
		input: []string{"nodeb", "node", "", "nodea"},
	},
}

// TestCompareNodeNames_MatchesTheCommittedOrder pins the node order against a
// committed file, so a change to it -- such as making the comparison
// allocation-free -- cannot move a node without the diff saying so. The order
// is what every page boundary of GET /api/fleet is cut from; a quiet change
// would move nodes between pages under a viewer paging through them.
//
// Regenerate with: go test ./pkg/fleet -run TestCompareNodeNames -update
func TestCompareNodeNames_MatchesTheCommittedOrder(t *testing.T) {
	cases := make([]orderCase, 0, len(orderInputs))
	for _, in := range orderInputs {
		sorted := append([]string(nil), in.input...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return CompareNodeNames(sorted[i], sorted[j]) < 0
		})
		cases = append(cases, orderCase{Name: in.name, Why: in.why, Input: in.input, Sorted: sorted})
	}

	want, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(orderPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", orderPath)
		return
	}

	got, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with -update)", orderPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with CompareNodeNames.\n"+
			"If the order was meant to change, regenerate with:\n"+
			"go test ./pkg/fleet -run TestCompareNodeNames -update\n"+
			"and say in the change why every consumer's node order moves.",
			orderPath)
	}
}

// TestCompareNodeNames_IsATotalOrder. The sort exists so the list cannot
// reshuffle between polls; a comparator that is not antisymmetric or not
// transitive would let it, whatever the order looks like on any one render.
func TestCompareNodeNames_IsATotalOrder(t *testing.T) {
	var names []string
	for _, in := range orderInputs {
		names = append(names, in.input...)
	}

	for _, a := range names {
		if got := CompareNodeNames(a, a); got != 0 {
			t.Errorf("CompareNodeNames(%q, %q) = %d, want 0", a, a, got)
		}
		for _, b := range names {
			ab, ba := CompareNodeNames(a, b), CompareNodeNames(b, a)
			if (ab < 0) != (ba > 0) || (ab == 0) != (ba == 0) {
				t.Errorf("not antisymmetric: (%q,%q) = %d but (%q,%q) = %d", a, b, ab, b, a, ba)
			}
			for _, c := range names {
				if CompareNodeNames(a, b) <= 0 && CompareNodeNames(b, c) <= 0 &&
					CompareNodeNames(a, c) > 0 {
					t.Errorf("not transitive: %q <= %q <= %q but %q > %q", a, b, c, a, c)
				}
			}
		}
	}
}
