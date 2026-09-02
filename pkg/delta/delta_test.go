package delta

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// tree parses a JSON literal the way the differ sees one.
func tree(t *testing.T, raw string) Tree {
	t.Helper()
	value, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode(%s): %v", raw, err)
	}
	return value
}

// roundTrip diffs two trees and applies the result back, returning the patch
// and what it reconstructed.
func roundTrip(t *testing.T, before, after Tree) (*Patch, Tree) {
	t.Helper()
	patch, err := Diff(before, after)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if patch == nil {
		return nil, before
	}

	// Through JSON, not the in-memory value: the wire is where a field with the
	// wrong tag, or a pointer that omitempty drops, actually goes missing.
	raw, err := Encode(patch)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var decoded Patch
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding the encoded patch: %v", err)
	}

	got, err := Apply(before, &decoded)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return &decoded, got
}

// TestDiffApply_ReconstructsExactly is the contract: whatever changed, applying
// the patch to the old tree must produce the new one, byte for byte.
func TestDiffApply_ReconstructsExactly(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
		wantUnchanged bool
	}{
		{
			name:          "nothing moved",
			before:        `{"a":1,"b":[1,2,3]}`,
			after:         `{"a":1,"b":[1,2,3]}`,
			wantUnchanged: true,
		},
		{name: "a scalar", before: `{"a":1,"b":2}`, after: `{"a":1,"b":3}`},
		{name: "a nested scalar", before: `{"a":{"b":{"c":1}}}`, after: `{"a":{"b":{"c":2}}}`},
		{name: "a string to null", before: `{"a":"x"}`, after: `{"a":null}`},
		{name: "null to a string", before: `{"a":null}`, after: `{"a":"x"}`},
		{name: "a type change", before: `{"a":1}`, after: `{"a":"1"}`},
		{name: "an object becomes an array", before: `{"a":{"b":1}}`, after: `{"a":[1]}`},
		{name: "a field appears", before: `{"a":1}`, after: `{"a":1,"b":2}`},
		{name: "a field disappears", before: `{"a":1,"b":2}`, after: `{"a":1}`},
		{name: "a scalar array element", before: `{"a":[1,2,3]}`, after: `{"a":[1,9,3]}`},
		{name: "an array grows", before: `{"a":[1,2]}`, after: `{"a":[1,2,3]}`},
		{name: "an array shrinks", before: `{"a":[1,2,3]}`, after: `{"a":[1,2]}`},
		{name: "an empty array", before: `{"a":[]}`, after: `{"a":[1]}`},
		{name: "a large integer", before: `{"a":9007199254740993}`, after: `{"a":9007199254740995}`},
		{name: "a float", before: `{"a":1.5}`, after: `{"a":1.5000001}`},
		{name: "a bool", before: `{"a":true}`, after: `{"a":false}`},
		{name: "the whole root", before: `{"a":1}`, after: `[1,2]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := tree(t, tt.before), tree(t, tt.after)
			patch, got := roundTrip(t, before, after)
			if tt.wantUnchanged && patch != nil {
				t.Fatalf("Diff() = %+v for identical trees, want nil", patch)
			}
			if !reflect.DeepEqual(got, after) {
				t.Fatalf("applying the patch gave %v, want %v", got, after)
			}
			// The base must be left alone: the caller keeps it as the base for
			// the next diff, and a mutated one would corrupt every later delta.
			if !reflect.DeepEqual(before, tree(t, tt.before)) {
				t.Fatal("Apply modified the tree it was given")
			}
		})
	}
}

// TestDiffApply_KeyedArrays covers the case the whole design exists for: a
// list whose elements have an identity and whose order moves.
func TestDiffApply_KeyedArrays(t *testing.T) {
	const path = "processes.process_list"
	if arrayKeys[path] != "pid" {
		t.Fatalf("this test assumes %s is keyed by pid", path)
	}

	proc := func(pid int, cpu float64) string {
		return fmt.Sprintf(`{"pid":%d,"name":"p%d","cpu_percent":%g}`, pid, pid, cpu)
	}
	wrap := func(elements ...string) string {
		return `{"processes":{"process_list":[` + strings.Join(elements, ",") + `]}}`
	}

	tests := []struct {
		name          string
		before, after string
		wantKeyed     bool
	}{
		{
			name:      "one element's field moved",
			before:    wrap(proc(1, 1), proc(2, 2)),
			after:     wrap(proc(1, 1), proc(2, 9)),
			wantKeyed: true,
		},
		{
			name:      "the list reordered but nothing changed",
			before:    wrap(proc(1, 1), proc(2, 2), proc(3, 3)),
			after:     wrap(proc(3, 3), proc(1, 1), proc(2, 2)),
			wantKeyed: true,
		},
		{
			name:      "a process exited",
			before:    wrap(proc(1, 1), proc(2, 2), proc(3, 3)),
			after:     wrap(proc(1, 1), proc(3, 3)),
			wantKeyed: true,
		},
		{
			name:      "a process started",
			before:    wrap(proc(1, 1)),
			after:     wrap(proc(1, 1), proc(7, 7)),
			wantKeyed: true,
		},
		{
			name:      "reordered, changed, added and removed at once",
			before:    wrap(proc(1, 1), proc(2, 2), proc(3, 3)),
			after:     wrap(proc(3, 8), proc(9, 9), proc(1, 1)),
			wantKeyed: true,
		},
		{
			name: "two elements share an identity",
			// Not a key at all, then: matching by it would merge them. The
			// differ has to notice and fall back rather than lose one.
			before:    wrap(proc(1, 1), proc(1, 2)),
			after:     wrap(proc(1, 3), proc(1, 4)),
			wantKeyed: false,
		},
		{
			name:      "an element has no identity field",
			before:    `{"processes":{"process_list":[{"name":"p"}]}}`,
			after:     `{"processes":{"process_list":[{"name":"q"}]}}`,
			wantKeyed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := tree(t, tt.before), tree(t, tt.after)
			patch, got := roundTrip(t, before, after)
			if !reflect.DeepEqual(got, after) {
				t.Fatalf("applying the patch gave\n %v\nwant\n %v", got, after)
			}
			if patch == nil {
				t.Fatal("Diff() = nil for trees that differ")
			}

			keyed := patch.Object["processes"].Object["process_list"].Keyed != nil
			if keyed != tt.wantKeyed {
				t.Fatalf("keyed = %v, want %v", keyed, tt.wantKeyed)
			}
			if keyed && patch.Object["processes"].Object["process_list"].Keyed.Key != "pid" {
				t.Error("the patch does not carry the key it was built with; a " +
					"receiver has no other way to know which field identifies an element")
			}
		})
	}
}

// TestDiffApply_ReorderIsNotAFullRewrite. Diffing by position collapses the
// moment a list reorders -- every surviving element looks changed -- and the
// process list reorders constantly. Without this, the whole feature buys
// nothing on the array that is 74% of a snapshot.
func TestDiffApply_ReorderIsNotAFullRewrite(t *testing.T) {
	build := func(pids []int, busy int) Tree {
		elements := make([]string, 0, len(pids))
		for _, pid := range pids {
			cpu := 0.5
			if pid == busy {
				cpu = 99
			}
			elements = append(elements,
				fmt.Sprintf(`{"pid":%d,"name":"process-%d","cmdline":"/usr/bin/process-%d --flag","cpu_percent":%g}`,
					pid, pid, pid, cpu))
		}
		return tree(t, `{"processes":{"process_list":[`+strings.Join(elements, ",")+`]}}`)
	}

	pids := make([]int, 200)
	for i := range pids {
		pids[i] = i + 1
	}
	before := build(pids, 1)

	// The same 200 processes, listed in a different order, one of them busy.
	rotated := append(append([]int{}, pids[100:]...), pids[:100]...)
	after := build(rotated, 42)

	patch, got := roundTrip(t, before, after)
	if !reflect.DeepEqual(got, after) {
		t.Fatal("the patch did not reconstruct the reordered list")
	}

	raw, err := Encode(patch)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}

	// The order alone is 200 identities; the rest is two changed values.
	// Anything approaching the size of the list itself means the identities
	// were not used.
	if len(raw)*4 > len(whole) {
		t.Fatalf("the patch is %d bytes against a %d byte snapshot; a reorder is "+
			"being sent as a rewrite", len(raw), len(whole))
	}
}

// TestApply_RejectsAPatchThatDoesNotFit.
//
// A receiver that has diverged from the sender cannot be repaired by any later
// patch, and a partly applied one leaves numbers that look measured and are
// not. Every mismatch has to be an error, so the caller starts again from a
// whole snapshot.
func TestApply_RejectsAPatchThatDoesNotFit(t *testing.T) {
	for _, tt := range refusalInputs {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Apply(tree(t, tt.tree), tt.patch); err == nil {
				t.Fatal("Apply() = nil error for a patch that does not fit")
			}
		})
	}
}

// refusalInputs are the patches that must be refused, shared with the fixture
// the browser implementation is held to (testdata/refusals.json).
//
// It is a package-level table rather than a local one for the reason this list
// grew: the two implementations disagreed about several of these, in the
// direction where Go accepted a patch the browser refused. Positive cases were
// already generated from one source and checked on both sides; the refusals
// were written twice, by hand, and drifted.
var refusalInputs = []struct {
	name  string
	tree  string
	patch *Patch
}{
	{
		name:  "an object patch on an array",
		tree:  `[1,2]`,
		patch: &Patch{Object: map[string]*Patch{"a": {Replace: json.RawMessage(`1`)}}},
	},
	{
		name:  "a field that is not there",
		tree:  `{"a":1}`,
		patch: &Patch{Object: map[string]*Patch{"b": {Replace: json.RawMessage(`1`)}}},
	},
	{
		name:  "a positional patch on an object",
		tree:  `{"a":1}`,
		patch: &Patch{Index: map[string]*Patch{"0": {Replace: json.RawMessage(`1`)}}},
	},
	{
		name:  "a position past the end",
		tree:  `[1,2]`,
		patch: &Patch{Index: map[string]*Patch{"5": {Replace: json.RawMessage(`1`)}}},
	},
	// Positions Go's strconv.Atoi refuses and JavaScript's Number() would not.
	// The differ never emits these; a sender that is not the differ can.
	{
		name:  "a position spelled in exponent notation",
		tree:  `[0,1]`,
		patch: &Patch{Index: map[string]*Patch{"1e0": {Replace: json.RawMessage(`9`)}}},
	},
	{
		name:  "a position spelled in hexadecimal",
		tree:  `[0,1]`,
		patch: &Patch{Index: map[string]*Patch{"0x1": {Replace: json.RawMessage(`9`)}}},
	},
	{
		// Number("") is 0, so this used to patch the first element.
		name:  "a position that is the empty string",
		tree:  `[0,1]`,
		patch: &Patch{Index: map[string]*Patch{"": {Replace: json.RawMessage(`9`)}}},
	},
	{
		name:  "a position padded with a space",
		tree:  `[0,1]`,
		patch: &Patch{Index: map[string]*Patch{" 1": {Replace: json.RawMessage(`9`)}}},
	},
	// Spellings that parse to a position already named. Whichever of the two
	// replacements won would depend on map iteration order in Go and property
	// order in the browser.
	{
		name: "two positions that are the same index",
		tree: `[0,1]`,
		patch: &Patch{Index: map[string]*Patch{
			"0":  {Replace: json.RawMessage(`8`)},
			"+0": {Replace: json.RawMessage(`9`)},
		}},
	},
	{
		name:  "a position with a leading zero",
		tree:  `[0,1]`,
		patch: &Patch{Index: map[string]*Patch{"01": {Replace: json.RawMessage(`9`)}}},
	},
	{
		name:  "a keyed patch on an object",
		tree:  `{"a":1}`,
		patch: &Patch{Keyed: &KeyedPatch{Key: "pid"}},
	},
	{
		name:  "a keyed patch with no key",
		tree:  `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{}},
	},
	{
		name:  "elements that do not carry the key",
		tree:  `[{"name":"a"}]`,
		patch: &Patch{Keyed: &KeyedPatch{Key: "pid"}},
	},
	{
		name:  "elements sharing an identity",
		tree:  `[{"pid":1},{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{Key: "pid"}},
	},
	{
		name: "an order naming an element that was never sent",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key: "pid", Order: order([]string{"1", "2"}),
		}},
	},
	{
		// Go refuses this because "toString" is not a key of the map. The
		// browser used to accept it, because `"toString" in value` is true for
		// every object: the patch would have created a field the snapshot does
		// not have and rendered it.
		name:  "a field that only the prototype has",
		tree:  `{"a":1}`,
		patch: &Patch{Object: map[string]*Patch{"toString": {Replace: json.RawMessage(`1`)}}},
	},
	{
		name:  "a patch naming no operation",
		tree:  `{"a":1}`,
		patch: &Patch{},
	},
	// Every pair of operations. The type says exactly one is set; a chain that
	// dispatches on the first it recognises applies one and discards the other,
	// so the receiver renders a tree the sender never described and reports it
	// current. Which one wins depends on the order of the cases, so the two
	// implementations could disagree while both reporting success -- which is
	// why this is enumerated rather than sampled.
	{
		name: "a replacement and an object patch",
		tree: `{"a":1}`,
		patch: &Patch{
			Replace: json.RawMessage(`{"a":2}`),
			Object:  map[string]*Patch{"a": {Replace: json.RawMessage(`3`)}},
		},
	},
	{
		name: "a replacement and a positional patch",
		tree: `[1,2]`,
		patch: &Patch{
			Replace: json.RawMessage(`[3,4]`),
			Index:   map[string]*Patch{"0": {Replace: json.RawMessage(`5`)}},
		},
	},
	{
		name: "a replacement and a keyed patch",
		tree: `[{"pid":1}]`,
		patch: &Patch{
			Replace: json.RawMessage(`[]`),
			Keyed:   &KeyedPatch{Key: "pid", Order: order([]string{"1"})},
		},
	},
	{
		name: "an object patch and a positional patch",
		tree: `{"a":1}`,
		patch: &Patch{
			Object: map[string]*Patch{"a": {Replace: json.RawMessage(`2`)}},
			Index:  map[string]*Patch{"0": {Replace: json.RawMessage(`3`)}},
		},
	},
	{
		name: "an object patch and a keyed patch",
		tree: `{"a":1}`,
		patch: &Patch{
			Object: map[string]*Patch{"a": {Replace: json.RawMessage(`2`)}},
			Keyed:  &KeyedPatch{Key: "pid"},
		},
	},
	{
		name: "a positional patch and a keyed patch",
		tree: `[{"pid":1}]`,
		patch: &Patch{
			Index: map[string]*Patch{"0": {Replace: json.RawMessage(`2`)}},
			Keyed: &KeyedPatch{Key: "pid", Order: order([]string{"1"})},
		},
	},
	{
		// A replacement with null is a real operation, so this is two, not one.
		name: "a null replacement and an object patch",
		tree: `{"a":1}`,
		patch: &Patch{
			Replace: json.RawMessage(`null`),
			Object:  map[string]*Patch{"a": {Replace: json.RawMessage(`2`)}},
		},
	},
	{
		name: "a nested node naming two operations",
		tree: `{"a":{"b":1}}`,
		patch: &Patch{Object: map[string]*Patch{"a": {
			Replace: json.RawMessage(`{"b":2}`),
			Object:  map[string]*Patch{"b": {Replace: json.RawMessage(`3`)}},
		}}},
	},
	// The identities below are the silent half, and the reason the check
	// exists at all. Each one names an element the order never places, so
	// the operation is simply never looked up: the element keeps the value
	// it already had and the frame reports success. Nothing fails, nothing
	// is logged, and the dashboard shows a measurement that is no longer
	// true. The browser implementation already refused these, so Go
	// accepting them was also a divergence between the two halves of one
	// format.
	{
		name: "an element changed that this array does not contain",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:     "pid",
			Patches: map[string]*Patch{"999": {Object: map[string]*Patch{"pid": {Replace: json.RawMessage(`2`)}}}},
		}},
	},
	{
		name: "an element changed that an explicit order drops",
		tree: `[{"pid":1},{"pid":2}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:     "pid",
			Order:   order([]string{"1"}),
			Patches: map[string]*Patch{"2": {Object: map[string]*Patch{"pid": {Replace: json.RawMessage(`3`)}}}},
		}},
	},
	{
		name: "an element supplied that the order never places",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:   "pid",
			Order: order([]string{"1"}),
			New:   map[string]json.RawMessage{"2": json.RawMessage(`{"pid":2}`)},
		}},
	},
	{
		name: "an element supplied with no order at all",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key: "pid",
			New: map[string]json.RawMessage{"2": json.RawMessage(`{"pid":2}`)},
		}},
	},
	// A supplied element's position in New is a CLAIM about its identity, and
	// nothing checked the claim against the element itself.
	{
		name: "an element supplied under an identity that is not its own",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:   "pid",
			Order: order([]string{"1", "2"}),
			New:   map[string]json.RawMessage{"2": json.RawMessage(`{"pid":999,"cpu":1}`)},
		}},
	},
	{
		name: "an element supplied with no key field at all",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:   "pid",
			Order: order([]string{"1", "2"}),
			New:   map[string]json.RawMessage{"2": json.RawMessage(`{"cpu":1}`)},
		}},
	},
	{
		// "New" means new. The differ never supplies an identity the array
		// already has, and taking it would replace an element the same patch
		// describes as surviving.
		name: "an element supplied as new that the array already has",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:   "pid",
			Order: order([]string{"1"}),
			New:   map[string]json.RawMessage{"1": json.RawMessage(`{"pid":1,"cpu":9}`)},
		}},
	},
	{
		// A sub-patch can rewrite the key field itself. The differ represents an
		// identity change as a removal plus a New element, never as a survivor
		// changing its key -- so this is a frame no differ produced, and taking
		// it leaves the receiver with an array the sender does not have.
		name: "a survivor patched into a different identity",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:     "pid",
			Patches: map[string]*Patch{"1": {Object: map[string]*Patch{"pid": {Replace: json.RawMessage(`2`)}}}},
		}},
	},
	{
		name: "a survivor patched into one with no key field",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:     "pid",
			Patches: map[string]*Patch{"1": {Replace: json.RawMessage(`{"cpu":1}`)}},
		}},
	},
	{
		name: "an element both supplied and changed",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key:     "pid",
			Order:   order([]string{"2"}),
			New:     map[string]json.RawMessage{"2": json.RawMessage(`{"pid":2}`)},
			Patches: map[string]*Patch{"2": {Object: map[string]*Patch{"pid": {Replace: json.RawMessage(`3`)}}}},
		}},
	},
	{
		name: "an order naming the same element twice",
		tree: `[{"pid":1}]`,
		patch: &Patch{Keyed: &KeyedPatch{
			Key: "pid", Order: order([]string{"1", "1"}),
		}},
	},
}

// TestDiffApply_RandomTrees is the property test.
//
// The hand-written cases above cover the shapes anyone thought of. This covers
// the ones nobody did: random trees, randomly mutated, every one of which must
// survive the round trip. The seed is fixed so a failure is reproducible.
func TestDiffApply_RandomTrees(t *testing.T) {
	rng := rand.New(rand.NewSource(20260901))

	for i := 0; i < 3000; i++ {
		before := randomTree(rng, 0)
		after := mutate(rng, before, 0)

		patch, err := Diff(before, after)
		if err != nil {
			t.Fatalf("case %d: Diff: %v", i, err)
		}

		got := before
		if patch != nil {
			raw, err := Encode(patch)
			if err != nil {
				t.Fatalf("case %d: Encode: %v", i, err)
			}
			var decoded Patch
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("case %d: decoding the patch: %v", i, err)
			}
			if got, err = Apply(before, &decoded); err != nil {
				t.Fatalf("case %d: Apply: %v\nbefore %v\nafter %v\npatch %s", i, err, before, after, raw)
			}
		}
		if !reflect.DeepEqual(got, after) {
			t.Fatalf("case %d: round trip lost data\nbefore %v\nafter  %v\ngot    %v",
				i, before, after, got)
		}
	}
}

// TestDiffApply_RandomKeyedArrays is the property test for the keyed path,
// which the one above does not reach.
//
// randomTree builds trees whose field names are f0, f1, f2, so no array in it
// ever sits at a path in arrayKeys -- every array is diffed by position or
// replaced wholesale, and the keyed branch, the largest and most delicate part
// of the format, was covered only by hand-written cases. The empty-membership
// bug lived there: an array that lost its last element encoded as "membership
// unchanged", every receiver kept the element it had, and three thousand
// random cases said nothing because none of them was keyed.
//
// So this generates at a path that IS keyed, and mutates membership as well as
// values -- including down to empty, which is the case that was wrong.
func TestDiffApply_RandomKeyedArrays(t *testing.T) {
	rng := rand.New(rand.NewSource(20260902))

	// The elements a case draws from. Fixed identities so before and after can
	// share them, with the values varying independently.
	element := func(pid int, value any) map[string]any {
		return map[string]any{"pid": json.Number(strconv.Itoa(pid)), "value": value}
	}
	list := func(pids []int, rng *rand.Rand) Tree {
		out := make([]any, 0, len(pids))
		for _, pid := range pids {
			out = append(out, element(pid, randomScalar(rng)))
		}
		return map[string]any{"processes": map[string]any{"process_list": out}}
	}
	subset := func(rng *rand.Rand) []int {
		// Sometimes empty, which is the whole point.
		n := rng.Intn(5)
		pids := rng.Perm(6)[:n]
		return pids
	}

	for i := 0; i < 3000; i++ {
		before := list(subset(rng), rng)
		after := list(subset(rng), rng)

		patch, err := Diff(before, after)
		if err != nil {
			t.Fatalf("case %d: Diff: %v", i, err)
		}

		got := before
		if patch != nil {
			// Through the wire. The bug this test exists for was invisible in
			// memory: Diff produced the right empty order and the encoding
			// dropped it.
			raw, err := Encode(patch)
			if err != nil {
				t.Fatalf("case %d: Encode: %v", i, err)
			}
			var decoded Patch
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("case %d: decoding the patch: %v", i, err)
			}
			if got, err = Apply(before, &decoded); err != nil {
				t.Fatalf("case %d: Apply: %v\nbefore %v\nafter %v\npatch %s", i, err, before, after, raw)
			}
		}
		if !reflect.DeepEqual(got, after) {
			raw, _ := Encode(patch)
			t.Fatalf("case %d: round trip lost data\nbefore %v\nafter  %v\ngot    %v\npatch  %s",
				i, before, after, got, raw)
		}
	}
}

// randomTree builds a small JSON value, biased towards the shapes a snapshot
// actually has: objects of scalars, and arrays of objects carrying a pid.
func randomTree(rng *rand.Rand, depth int) Tree {
	if depth > 3 {
		return randomScalar(rng)
	}
	switch rng.Intn(6) {
	case 0, 1:
		n := rng.Intn(4)
		object := make(map[string]any, n)
		for i := 0; i < n; i++ {
			object[fmt.Sprintf("f%d", i)] = randomTree(rng, depth+1)
		}
		return object
	case 2:
		n := rng.Intn(4)
		array := make([]any, 0, n)
		for i := 0; i < n; i++ {
			array = append(array, randomTree(rng, depth+1))
		}
		return array
	case 3:
		// An array whose elements have an identity, so the keyed path is
		// exercised even though these paths are not in the table -- the
		// positional fallback has to reconstruct them too.
		n := rng.Intn(4)
		array := make([]any, 0, n)
		for i := 0; i < n; i++ {
			array = append(array, map[string]any{
				"pid": json.Number(fmt.Sprintf("%d", rng.Intn(6))),
				"v":   randomScalar(rng),
			})
		}
		return array
	default:
		return randomScalar(rng)
	}
}

func randomScalar(rng *rand.Rand) Tree {
	switch rng.Intn(6) {
	case 0:
		return nil
	case 1:
		return rng.Intn(2) == 0
	case 2:
		return json.Number(fmt.Sprintf("%d", rng.Intn(1000)))
	case 3:
		return json.Number(fmt.Sprintf("%d.5", rng.Intn(1000)))
	case 4:
		return json.Number("9007199254740993")
	default:
		return fmt.Sprintf("s%d", rng.Intn(10))
	}
}

// mutate returns a changed copy of a tree: a different value somewhere, a
// different shape, or nothing at all.
func mutate(rng *rand.Rand, value Tree, depth int) Tree {
	if depth > 4 || rng.Intn(4) == 0 {
		if rng.Intn(3) == 0 {
			return value
		}
		return randomTree(rng, depth)
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for name, sub := range v {
			out[name] = mutate(rng, sub, depth+1)
		}
		if rng.Intn(6) == 0 {
			out[fmt.Sprintf("added%d", rng.Intn(3))] = randomScalar(rng)
		}
		if rng.Intn(6) == 0 {
			for name := range out {
				delete(out, name)
				break
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(v)+1)
		for _, element := range v {
			if rng.Intn(8) == 0 {
				continue // removed
			}
			out = append(out, mutate(rng, element, depth+1))
		}
		if rng.Intn(5) == 0 {
			out = append(out, randomTree(rng, depth+1))
		}
		if len(out) > 1 && rng.Intn(4) == 0 {
			i, j := rng.Intn(len(out)), rng.Intn(len(out))
			out[i], out[j] = out[j], out[i]
		}
		return out
	default:
		return randomScalar(rng)
	}
}

// TestIdentity_RefusesNumbersThatCouldBeSpelledTwoWays.
//
// An identity crosses a language boundary: this side renders it from the JSON
// text, the browser from whatever JSON.parse produced. Those agree for a string
// and for an integer inside 2^53, and disagree for 1e3 against 1000, or 1.50
// against 1.5, or an integer past the exact range. A key that could be spelled
// two ways would leave the two sides matching different elements -- and the
// receiver, finding no patch for an element it holds, would keep the value it
// already had and count the frame as applied. A stale reading, presented as
// current, with nothing raised anywhere.
//
// Refusing costs nothing: the array falls back to positional diffing, which is
// correct and merely larger.
func TestIdentity_RefusesNumbersThatCouldBeSpelledTwoWays(t *testing.T) {
	usable := []string{"0", "1", "-1", "1002", "999999999999999"}
	for _, text := range usable {
		element := map[string]any{"pid": json.Number(text)}
		got, ok := identity(element, "pid")
		if !ok || got != text {
			t.Errorf("identity(%s) = %q, %v; want it usable", text, got, ok)
		}
	}

	refused := []string{
		"1e3",                  // the same value as 1000, spelled differently
		"1E3",                  //
		"1.0",                  // an integer that is not written as one
		"1.50",                 // trailing zero a browser drops
		"9007199254740993",     // past 2^53, and rounded by a browser
		"-9007199254740993",    //
		"12345678901234567890", // past uint64 as well
		// The two that pass every digit test and still disagree. A browser
		// parses these to numbers and renders them "7" and "0", so the sides
		// would match different elements while both believing they had matched.
		"007",
		"-0",
		"-00",
	}
	for _, text := range refused {
		element := map[string]any{"pid": json.Number(text)}
		if got, ok := identity(element, "pid"); ok {
			t.Errorf("identity(%s) = %q, usable; want it refused", text, got)
		}
	}

	// A string identity is taken as written, whatever it looks like.
	if got, ok := identity(map[string]any{"name": "1e3"}, "name"); !ok || got != "1e3" {
		t.Errorf("a string identity was refused: %q, %v", got, ok)
	}
}

// TestDiff_FallsBackWhenIdentitiesAreUnusable: refusing an identity must leave
// a correct patch behind, not no patch.
func TestDiff_FallsBackWhenIdentitiesAreUnusable(t *testing.T) {
	before := tree(t, `{"processes":{"process_list":[{"pid":1e3,"cpu":1},{"pid":2e3,"cpu":2}]}}`)
	after := tree(t, `{"processes":{"process_list":[{"pid":1e3,"cpu":9},{"pid":2e3,"cpu":2}]}}`)

	patch, got := roundTrip(t, before, after)
	if !reflect.DeepEqual(got, after) {
		t.Fatal("the fallback patch did not reconstruct the change")
	}
	list := patch.Object["processes"].Object["process_list"]
	if list.Keyed != nil {
		t.Fatal("an array whose identities are ambiguous was still keyed")
	}
	if list.Index == nil {
		t.Fatalf("expected a positional patch, got %+v", list)
	}
}

// order wraps a literal for the pointer field. Order is a pointer because an
// empty order is a real membership -- the array is now empty -- and a plain
// slice with omitempty encodes that identically to "membership unchanged".
func order(ids []string) *[]string { return &ids }
