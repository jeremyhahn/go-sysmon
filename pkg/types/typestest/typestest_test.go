package typestest_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

type inner struct {
	Tags []string `json:"tags"`
}

type payload struct {
	List      []int `json:"list"`
	Optional  []int `json:"optional,omitempty"`
	Untagged  []int
	Skipped   []int      `json:"-"`
	Nested    inner      `json:"nested"`
	Pointer   *inner     `json:"pointer"`
	Items     []inner    `json:"items"`
	Fixed     [2]inner   `json:"fixed"`
	Scalar    string     `json:"scalar"`
	hidden    []int      //nolint:unused // proves unexported fields are ignored
	Mismatch  []inner    `json:"mismatch"`
	Structure inner      `json:"structure"`
	Matrix    [][]string `json:"matrix"`
}

func TestNullSlices_ReportsEveryNullListByPath(t *testing.T) {
	raw := []byte(`{
		"list": null,
		"Untagged": null,
		"-": null,
		"nested": {"tags": null},
		"pointer": {"tags": null},
		"items": [{"tags": ["a"]}, {"tags": null}],
		"fixed": [{"tags": null}, {"tags": []}],
		"scalar": null,
		"mismatch": "not a list",
		"structure": "not an object",
		"matrix": [null, ["x"]]
	}`)

	got, err := typestest.NullSlices(raw, payload{})
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	want := []string{
		"Untagged",
		"fixed[0].tags",
		"items[1].tags",
		"list",
		"matrix[0]",
		"nested.tags",
		"pointer.tags",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestNullSlices_CleanPayloadReportsNothing(t *testing.T) {
	raw := []byte(`{"list": [], "nested": {"tags": []}, "pointer": null, "items": []}`)
	got, err := typestest.NullSlices(raw, &payload{})
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a payload with no null lists reported %v", got)
	}
}

func TestNullSlices_RootList(t *testing.T) {
	got, err := typestest.NullSlices([]byte(`null`), []types.FleetNode(nil))
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"(root)"}) {
		t.Errorf("got %v, want the root reported", got)
	}
}

func TestNullSlices_UntypedLikeChecksNothing(t *testing.T) {
	got, err := typestest.NullSlices([]byte(`{"list": null}`), nil)
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("with no type to follow nothing can be a list, got %v", got)
	}
}

func TestNullSlices_RejectsInvalidJSON(t *testing.T) {
	_, err := typestest.NullSlices([]byte(`{"list": [`), payload{})
	var decodeErr *typestest.DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("got %T (%v), want *typestest.DecodeError", err, err)
	}
	if decodeErr.Unwrap() == nil || decodeErr.Error() == "" {
		t.Error("the decode error lost its cause")
	}
}

// Inner is exported so it can be embedded the way types.FleetNode embeds
// types.NodeSummary.
type Inner struct {
	Tags []string `json:"tags"`
}

type flattened struct {
	Inner
	Named Inner `json:"named"`
}

// TestNullSlices_FollowsEmbeddedStructsIntoTheParent: encoding/json writes an
// untagged embedded struct's fields into the enclosing object, so its lists
// are found there -- at the parent's path, not under the type's name.
func TestNullSlices_FollowsEmbeddedStructsIntoTheParent(t *testing.T) {
	got, err := typestest.NullSlices([]byte(`{"tags": null, "named": {"tags": null}}`), flattened{})
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	want := []string{"named.tags", "tags"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NullSlices = %v, want %v", got, want)
	}
	if got, err := typestest.NullSlices([]byte(`{"tags": [], "named": {"tags": []}}`), flattened{}); err != nil || len(got) != 0 {
		t.Errorf("NullSlices on a payload with no nulls = %v, %v; want none", got, err)
	}
}
