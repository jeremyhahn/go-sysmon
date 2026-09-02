package delta

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// update rewrites the shared fixture instead of checking it.
var update = flag.Bool("update", false, "rewrite testdata/cases.json")

// casesPath is the fixture both implementations are held to.
const casesPath = "testdata/cases.json"

// deltaCase is one before/patch/after triple.
type deltaCase struct {
	Name   string          `json:"name"`
	Why    string          `json:"why"`
	Before json.RawMessage `json:"before"`
	Patch  json.RawMessage `json:"patch"`
	After  json.RawMessage `json:"after"`
}

// fixtureInputs are the pairs the fixture is generated from.
//
// One per shape the format can take, chosen so that an implementation which
// mishandles any single one fails on exactly that case rather than on
// something incidental.
var fixtureInputs = []struct {
	name          string
	why           string
	before, after string
}{
	{
		name:   "a scalar moved",
		why:    "the ordinary case: one number changed and nothing else may be touched",
		before: `{"host":{"hostname":"mon0","uptime":100},"load_avg":{"load1":0.5}}`,
		after:  `{"host":{"hostname":"mon0","uptime":101},"load_avg":{"load1":0.5}}`,
	},
	{
		name:   "a value became null",
		why:    "null is a value, not an absence; a receiver that reads the patch as 'no replacement' keeps the old one",
		before: `{"disk":{"raid":{"level":"raid1"}}}`,
		after:  `{"disk":{"raid":null}}`,
	},
	{
		name:   "a value stopped being null",
		why:    "the other direction, which a receiver special-casing null can get wrong on its own",
		before: `{"disk":{"raid":null}}`,
		after:  `{"disk":{"raid":{"level":"raid1"}}}`,
	},
	{
		name:   "a value changed type",
		why:    "there is nothing smaller to send than the whole node",
		before: `{"value":42}`,
		after:  `{"value":"42"}`,
	},
	{
		name:   "an object gained a field",
		why:    "the key set changed, so the object is replaced rather than patched",
		before: `{"a":{"x":1}}`,
		after:  `{"a":{"x":1,"y":2}}`,
	},
	{
		name:   "an array changed in place",
		why:    "equal lengths with no configured identity: patched by position",
		before: `{"sensors":{"thermal_throttle":[{"count":1},{"count":2}]}}`,
		after:  `{"sensors":{"thermal_throttle":[{"count":1},{"count":9}]}}`,
	},
	{
		name:   "an array changed length",
		why:    "positions no longer refer to the same elements, so the array is replaced",
		before: `{"cpus":[{"index":0}]}`,
		after:  `{"cpus":[{"index":0},{"index":1}]}`,
	},
	{
		name: "identities that are JavaScript prototype members",
		why: "the browser looks these up by property access, where \"constructor\" " +
			"and \"toString\" resolve to inherited functions rather than to nothing; " +
			"Go looks them up in a map, where they are ordinary strings. A network " +
			"interface or thermal zone is named by the host, so this is data, not a bug",
		before: `{"networks":[{"name":"constructor","rx":1},{"name":"toString","rx":2},{"name":"eth0","rx":3}]}`,
		after:  `{"networks":[{"name":"constructor","rx":9},{"name":"toString","rx":2},{"name":"eth0","rx":3}]}`,
	},
	{
		name: "a keyed array lost its last element",
		why: "an empty order is a real membership -- the array is now empty -- and a " +
			"receiver reading the absent field as \"membership unchanged\" keeps the " +
			"element it has and renders it as current",
		before: `{"processes":{"process_list":[{"pid":1,"name":"a"}]}}`,
		after:  `{"processes":{"process_list":[]}}`,
	},
	{
		name:   "a keyed element changed",
		why:    "the common case for the process list: one field of one element",
		before: `{"processes":{"process_list":[{"pid":1,"cpu_percent":0},{"pid":2,"cpu_percent":0}]}}`,
		after:  `{"processes":{"process_list":[{"pid":1,"cpu_percent":0},{"pid":2,"cpu_percent":50}]}}`,
	},
	{
		name:   "a keyed array reordered",
		why:    "nothing changed but the order; a receiver applying patches by position would corrupt every element",
		before: `{"processes":{"process_list":[{"pid":1,"cpu_percent":1},{"pid":2,"cpu_percent":2},{"pid":3,"cpu_percent":3}]}}`,
		after:  `{"processes":{"process_list":[{"pid":3,"cpu_percent":3},{"pid":1,"cpu_percent":1},{"pid":2,"cpu_percent":2}]}}`,
	},
	{
		name:   "a keyed element disappeared",
		why:    "the order is the authoritative membership; anything not named in it is gone",
		before: `{"processes":{"process_list":[{"pid":1},{"pid":2},{"pid":3}]}}`,
		after:  `{"processes":{"process_list":[{"pid":1},{"pid":3}]}}`,
	},
	{
		name:   "a keyed element appeared",
		why:    "a new element travels whole, and has to land in the right position",
		before: `{"processes":{"process_list":[{"pid":1,"name":"a"}]}}`,
		after:  `{"processes":{"process_list":[{"pid":7,"name":"g"},{"pid":1,"name":"a"}]}}`,
	},
	{
		name:   "a keyed array reordered, changed, gained and lost at once",
		why:    "the four operations interacting, which is what a real tick looks like",
		before: `{"processes":{"process_list":[{"pid":1,"cpu_percent":1},{"pid":2,"cpu_percent":2},{"pid":3,"cpu_percent":3}]}}`,
		after:  `{"processes":{"process_list":[{"pid":3,"cpu_percent":30},{"pid":9,"cpu_percent":9},{"pid":1,"cpu_percent":1}]}}`,
	},
	{
		name:   "an array keyed by a string",
		why:    "identities are rendered as text, so a numeric key and a string key must behave the same",
		before: `{"disks":[{"name":"sda","used_percent":10},{"name":"sdb","used_percent":20}]}`,
		after:  `{"disks":[{"name":"sdb","used_percent":21},{"name":"sda","used_percent":10}]}`,
	},
	{
		name:   "nothing moved",
		why:    "there is no patch to send, and a receiver must treat that as 'keep what you have'",
		before: `{"host":{"uptime":1}}`,
		after:  `{"host":{"uptime":1}}`,
	},
}

// TestFixture_MatchesTheCommittedCases keeps the two implementations honest.
//
// pkg/delta and cmd/sysmon/frontend/src/lib/delta.ts apply the same format and
// nothing in either language connects them. A disagreement would not fail
// anything: the server would go on sending correct patches and the dashboard
// would go on rendering something slightly wrong, indefinitely. Both are run
// against this one file -- generated here, read by tests/delta.spec.ts -- so a
// change to the format has to be made in both places or one of them fails.
//
// Regenerate with: go test ./pkg/delta -run TestFixture -update
func TestFixture_MatchesTheCommittedCases(t *testing.T) {
	cases := make([]deltaCase, 0, len(fixtureInputs))
	for _, in := range fixtureInputs {
		before, after := mustDecode(t, in.before), mustDecode(t, in.after)

		patch, err := Diff(before, after)
		if err != nil {
			t.Fatalf("%s: Diff: %v", in.name, err)
		}

		// Every case is proved here before it is written out, so the fixture
		// can never ask the other implementation for something wrong.
		got, err := Apply(before, patch)
		if err != nil {
			t.Fatalf("%s: Apply: %v", in.name, err)
		}
		if !reflect.DeepEqual(got, after) {
			t.Fatalf("%s: the patch does not reconstruct the case", in.name)
		}

		raw := json.RawMessage("null")
		if patch != nil {
			encoded, err := Encode(patch)
			if err != nil {
				t.Fatalf("%s: Encode: %v", in.name, err)
			}
			raw = encoded
		}

		cases = append(cases, deltaCase{
			Name:   in.name,
			Why:    in.why,
			Before: json.RawMessage(compact(t, in.before)),
			Patch:  raw,
			After:  json.RawMessage(compact(t, in.after)),
		})
	}

	want, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(casesPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(casesPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", casesPath)
		return
	}

	got, err := os.ReadFile(casesPath)
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with -update)", casesPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with the differ.\n"+
			"Regenerate it with: go test ./pkg/delta -run TestFixture -update\n"+
			"and check that cmd/sysmon/frontend/src/lib/delta.ts still applies every case.",
			casesPath)
	}
}

func mustDecode(t *testing.T, raw string) Tree {
	t.Helper()
	value, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode(%s): %v", raw, err)
	}
	return value
}

func compact(t *testing.T, raw string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(raw)); err != nil {
		t.Fatalf("compacting %s: %v", raw, err)
	}
	return out.Bytes()
}

// refusalsPath is the second fixture both implementations are held to: the
// patches that must be REFUSED.
//
// The positive cases were generated from one source and checked on both sides
// from the day the format existed. The refusals were written twice, by hand,
// and drifted -- the browser rejected a patch naming an element the array does
// not contain and Go applied it, returning the unchanged tree and reporting
// success. Exactly the divergence the shared fixture exists to prevent, in the
// half that did not have one.
const refusalsPath = "testdata/refusals.json"

// refusalCase is one tree and the patch that must not be applied to it.
type refusalCase struct {
	Name  string          `json:"name"`
	Tree  json.RawMessage `json:"tree"`
	Patch json.RawMessage `json:"patch"`
}

// TestRefusalFixture_MatchesTheCommittedCases regenerates and checks the
// refusal fixture.
//
// Regenerate with: go test ./pkg/delta -run TestRefusalFixture -update
//
// Every case is proved here before it is written out -- Apply must actually
// refuse it -- so the fixture can never ask the browser to reject something
// this implementation accepts.
func TestRefusalFixture_MatchesTheCommittedCases(t *testing.T) {
	cases := make([]refusalCase, 0, len(refusalInputs))
	for _, in := range refusalInputs {
		if _, err := Apply(tree(t, in.tree), in.patch); err == nil {
			t.Fatalf("%s: Apply() = nil error; it does not belong in a refusal fixture", in.name)
		}
		raw, err := Encode(in.patch)
		if err != nil {
			t.Fatalf("%s: Encode: %v", in.name, err)
		}
		cases = append(cases, refusalCase{
			Name:  in.name,
			Tree:  json.RawMessage(compact(t, in.tree)),
			Patch: raw,
		})
	}

	want, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	if *update {
		if err := os.WriteFile(refusalsPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", refusalsPath)
		return
	}

	got, err := os.ReadFile(refusalsPath)
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with -update)", refusalsPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with refusalInputs.\n"+
			"Regenerate with: go test ./pkg/delta -run TestRefusalFixture -update", refusalsPath)
	}
}

// acceptedPath is the third fixture: patches that must be ACCEPTED, with the
// exact tree they produce.
//
// cases.json is generated from before/after pairs by the differ, so it can only
// hold patches the differ emits. This one holds patches a receiver must handle
// that the differ never produces -- the wire is not the differ, and both
// implementations have to agree about a frame whoever sent it.
const acceptedPath = "testdata/accepted.json"

// acceptedCase is a tree, a patch, and what applying one to the other gives.
type acceptedCase struct {
	Name  string          `json:"name"`
	Why   string          `json:"why"`
	Tree  json.RawMessage `json:"tree"`
	Patch json.RawMessage `json:"patch"`
	Want  json.RawMessage `json:"want"`
}

// acceptedInputs are written as wire JSON rather than as Go structs, because
// what they are about is the wire: the difference between a field being absent
// and a field being present and null, which the Go type cannot express.
var acceptedInputs = []struct {
	name, why, tree, patch, want string
}{
	{
		name: "a replacement beside a null object patch",
		why: "null is not an operation. The exactly-one rule refuses a node that " +
			"names two THINGS TO DO; \"o\": null says there is nothing to do, so " +
			"nothing is discarded by applying the replacement",
		tree:  `{"a":1}`,
		patch: `{"r":1,"o":null}`,
		want:  `1`,
	},
	{
		name:  "a positional patch beside a null object patch",
		why:   "the same, with the surviving operation second in the dispatch order",
		tree:  `[1,2]`,
		patch: `{"o":null,"i":{"0":{"r":9}}}`,
		want:  `[9,2]`,
	},
	{
		name:  "an object patch beside a null keyed patch",
		why:   "and again for the keyed field, which is a pointer in Go and an object here",
		tree:  `{"a":1}`,
		patch: `{"o":{"a":{"r":2}},"a":null}`,
		want:  `{"a":2}`,
	},
	{
		name: "a null replacement beside every other field null",
		why: "\"r\" present and holding null IS an operation -- that distinction is " +
			"the reason the Go type carries a value rather than a pointer -- while " +
			"the other three being null are not",
		tree:  `{"a":1}`,
		patch: `{"r":null,"o":null,"i":null,"a":null}`,
		want:  `null`,
	},
}

// TestAcceptedFixture_MatchesTheCommittedCases regenerates and checks it.
//
// Regenerate with: go test ./pkg/delta -run TestAcceptedFixture -update
//
// Every case is applied here before it is written out, and the recorded result
// is what this implementation produced -- so the fixture cannot ask the browser
// for an answer the server does not give.
func TestAcceptedFixture_MatchesTheCommittedCases(t *testing.T) {
	cases := make([]acceptedCase, 0, len(acceptedInputs))
	for _, in := range acceptedInputs {
		var patch Patch
		if err := json.Unmarshal([]byte(in.patch), &patch); err != nil {
			t.Fatalf("%s: decoding the patch: %v", in.name, err)
		}
		got, err := Apply(tree(t, in.tree), &patch)
		if err != nil {
			t.Fatalf("%s: Apply: %v; it does not belong in an accepted fixture", in.name, err)
		}
		if !reflect.DeepEqual(got, mustDecode(t, in.want)) {
			t.Fatalf("%s: Apply gave %v, want %s", in.name, got, in.want)
		}
		cases = append(cases, acceptedCase{
			Name:  in.name,
			Why:   in.why,
			Tree:  json.RawMessage(compact(t, in.tree)),
			Patch: json.RawMessage(compact(t, in.patch)),
			Want:  json.RawMessage(compact(t, in.want)),
		})
	}

	want, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	if *update {
		if err := os.WriteFile(acceptedPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", acceptedPath)
		return
	}

	got, err := os.ReadFile(acceptedPath)
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with -update)", acceptedPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with acceptedInputs.\n"+
			"Regenerate with: go test ./pkg/delta -run TestAcceptedFixture -update", acceptedPath)
	}
}
