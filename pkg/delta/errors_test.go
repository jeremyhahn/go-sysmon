package delta

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// badNumber is a value encoding/json refuses to encode. A tree from Decode
// never holds one, but Diff takes any Tree, and a value it cannot encode must
// be reported with where it was -- not dropped from the patch.
var badNumber = json.Number("not-a-number")

func TestDiff_ReportsAValueItCannotEncode(t *testing.T) {
	tests := []struct {
		name     string
		before   string
		after    func(Tree) Tree
		wantPath string
	}{
		{
			name:   "a replaced scalar",
			before: `{"a":1}`,
			after: func(tr Tree) Tree {
				tr.(map[string]any)["a"] = badNumber
				return tr
			},
		},
		{
			name:   "a field of a nested object",
			before: `{"host":{"load":1}}`,
			after: func(tr Tree) Tree {
				tr.(map[string]any)["host"].(map[string]any)["load"] = badNumber
				return tr
			},
		},
		{
			name:   "an element of a positional array",
			before: `{"xs":[1,2]}`,
			after: func(tr Tree) Tree {
				tr.(map[string]any)["xs"].([]any)[1] = badNumber
				return tr
			},
		},
		{
			name:   "a field of a keyed element that survived",
			before: `{"networks":[{"name":"eth0","rx":1}]}`,
			after: func(tr Tree) Tree {
				tr.(map[string]any)["networks"].([]any)[0].(map[string]any)["rx"] = badNumber
				return tr
			},
		},
		{
			name:   "a keyed element that is new",
			before: `{"networks":[{"name":"eth0"}]}`,
			after: func(tr Tree) Tree {
				list := tr.(map[string]any)["networks"].([]any)
				tr.(map[string]any)["networks"] = append(list, map[string]any{"name": "eth1", "rx": badNumber})
				return tr
			},
			wantPath: "networks[]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tree(t, tt.before)
			after := tt.after(tree(t, tt.before))

			patch, err := Diff(before, after)
			var encodeErr *EncodeError
			if !errors.As(err, &encodeErr) {
				t.Fatalf("Diff() = %+v, %v; want an *EncodeError", patch, err)
			}
			if encodeErr.Path != tt.wantPath {
				t.Errorf("EncodeError.Path = %q, want %q", encodeErr.Path, tt.wantPath)
			}
		})
	}
}

func TestEncode_ReportsAPatchItCannotEncode(t *testing.T) {
	raw, err := Encode(&Patch{Replace: json.RawMessage(`{not json`)})
	var encodeErr *EncodeError
	if !errors.As(err, &encodeErr) {
		t.Fatalf("Encode() = %q, %v; want an *EncodeError", raw, err)
	}

	raw, err = Encode(&Patch{Replace: json.RawMessage(`1`)})
	if err != nil || string(raw) != `{"r":1}` {
		t.Errorf("Encode() = %q, %v; want {\"r\":1}", raw, err)
	}
}

func TestDecode_RejectsWhatIsNotJSON(t *testing.T) {
	got, err := Decode([]byte(`{"a":`))
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("Decode() = %v, %v; want a *DecodeError", got, err)
	}
}

// TestApply_RefusesAnElementThatIsNotJSON: an element a keyed patch supplies
// whole is decoded, and one that does not decode is a refusal, not a partly
// applied array.
func TestApply_RefusesAnElementThatIsNotJSON(t *testing.T) {
	patch := &Patch{Keyed: &KeyedPatch{
		Key: "pid", Order: order([]string{"1", "2"}),
		New: map[string]json.RawMessage{"2": json.RawMessage(`{"pid":`)},
	}}

	got, err := Apply(tree(t, `[{"pid":1}]`), patch)
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("Apply() = %v, %v; want a *DecodeError", got, err)
	}
}

// TestApply_ANestedRefusalNamesWhereItWas verifies a patch that fails deep
// inside an array is refused as a whole, and the error says where.
func TestApply_ANestedRefusalNamesWhereItWas(t *testing.T) {
	wrongKind := &Patch{Index: map[string]*Patch{"0": {Replace: json.RawMessage(`1`)}}}
	tests := []struct {
		name  string
		tree  string
		patch *Patch
		want  string
	}{
		{
			name:  "inside a positional element",
			tree:  `{"xs":[{"a":1}]}`,
			patch: &Patch{Object: map[string]*Patch{"xs": {Index: map[string]*Patch{"0": wrongKind}}}},
			want:  "xs[]",
		},
		{
			name: "inside a keyed element that survived",
			tree: `{"ps":[{"pid":1,"a":{"b":1}}]}`,
			patch: &Patch{Object: map[string]*Patch{"ps": {Keyed: &KeyedPatch{
				Key: "pid", Patches: map[string]*Patch{"1": {Object: map[string]*Patch{"a": wrongKind}}},
			}}}},
			want: "ps[].a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply(tree(t, tt.tree), tt.patch)
			var applyErr *ApplyError
			if !errors.As(err, &applyErr) {
				t.Fatalf("Apply() = %v, %v; want an *ApplyError", got, err)
			}
			if applyErr.Path != tt.want {
				t.Errorf("ApplyError.Path = %q, want %q", applyErr.Path, tt.want)
			}
		})
	}
}

func TestErrors_SayWhatFailedAndWhere(t *testing.T) {
	cause := errors.New("unexpected end of JSON input")
	tests := []struct {
		err   error
		want  string
		cause error
	}{
		{&DecodeError{Err: cause}, "delta: cannot decode a snapshot: unexpected end of JSON input", cause},
		{&EncodeError{Err: cause}, "delta: cannot encode a patch value: unexpected end of JSON input", cause},
		{&EncodeError{Path: "networks[]", Err: cause}, "delta: cannot encode the patch value at networks[]: unexpected end of JSON input", cause},
		{&ApplyError{Reason: "no such field"}, "delta: cannot apply a patch at the snapshot root: no such field", nil},
		{&ApplyError{Path: "cpus[]", Reason: "no such field"}, "delta: cannot apply a patch at cpus[]: no such field", nil},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
		if got := errors.Unwrap(tt.err); got != tt.cause {
			t.Errorf("Unwrap(%q) = %v, want %v", tt.want, got, tt.cause)
		}
	}
}

// TestIdentity_TakesBooleansAndRefusesNonObjects covers the rest of what can
// and cannot identify an element.
func TestIdentity_TakesBooleansAndRefusesNonObjects(t *testing.T) {
	for _, v := range []bool{true, false} {
		want := "false"
		if v {
			want = "true"
		}
		if got, ok := identity(map[string]any{"up": v}, "up"); !ok || got != want {
			t.Errorf("identity(%v) = %q, %v; want %q", v, got, ok, want)
		}
	}
	for name, element := range map[string]any{
		"a scalar element": json.Number("1"),
		"an object key":    map[string]any{"pid": map[string]any{}},
		"a missing key":    map[string]any{"name": "a"},
		"a null key":       map[string]any{"pid": nil},
	} {
		if got, ok := identity(element, "pid"); ok {
			t.Errorf("%s: identity = %q, usable; want it refused", name, got)
		}
	}
}

// TestDiff_FallsBackWhenOnlyTheNewArrayIsUnkeyable: the old array keyed
// cleanly, the new one has an element without the key. The patch must still
// reconstruct the new array.
func TestDiff_FallsBackWhenOnlyTheNewArrayIsUnkeyable(t *testing.T) {
	before := tree(t, `{"networks":[{"name":"eth0","rx":1}]}`)
	after := tree(t, `{"networks":[{"rx":2}]}`)

	patch, got := roundTrip(t, before, after)
	if !reflect.DeepEqual(got, after) {
		t.Fatalf("the fallback patch reconstructed %v, want %v", got, after)
	}
	if list := patch.Object["networks"]; list.Keyed != nil {
		t.Fatalf("an array with an element lacking its key was keyed: %+v", list)
	}
}
