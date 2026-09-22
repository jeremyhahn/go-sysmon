package delta

import (
	"encoding/json"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

// TestDiffApply_AListThatEmptiesIsSentAsAnEmptyList covers #151 on the delta
// stream. A snapshot never carries null for a list, and a patch must not
// reintroduce one: when the last GPU goes, the last process exits or a CPU
// loses its flags, the list is described as [] -- by a keyed order that is
// empty, or a replacement with [] -- and the tree the client rebuilds holds
// an empty list where it held a full one.
//
// Every route the differ has for an array is exercised: a keyed list (gpus,
// processes), a positional list that changes length (a CPU's flags), and a
// list inside an element that survives (an NPU's engines).
func TestDiffApply_AListThatEmptiesIsSentAsAnEmptyList(t *testing.T) {
	before := types.Snapshot{
		CPUs: []types.CPUInfo{{Index: 0, Flags: []string{"fpu", "sse2"}}},
		GPUs: []types.GPUInfo{{Index: 0, Name: "gpu0"}},
		NPUs: []types.NPUInfo{{Index: 0, Name: "accel0", Engines: []types.NPUEngine{{Name: "compute"}}}},
		Processes: types.ProcessSummary{
			Processes: []types.ProcessDetail{{PID: 1, Name: "init"}, {PID: 2, Name: "kthreadd"}},
		},
	}
	after := types.Snapshot{
		CPUs: []types.CPUInfo{{Index: 0}},
		NPUs: []types.NPUInfo{{Index: 0, Name: "accel0"}},
	}
	types.ReplaceNilSlices(&before)
	types.ReplaceNilSlices(&after)

	beforeTree, afterTree := encodeTree(t, before), encodeTree(t, after)
	patch, got := roundTrip(t, beforeTree, afterTree)
	if patch == nil {
		t.Fatal("no patch for a snapshot whose lists emptied")
	}
	if paths := nullReplacements(patch, ""); len(paths) != 0 {
		t.Errorf("the patch replaces these with null: %v", paths)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal the rebuilt tree: %v", err)
	}
	nulls, err := typestest.NullSlices(raw, types.Snapshot{})
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if len(nulls) != 0 {
		t.Errorf("the rebuilt snapshot has null lists: %v", nulls)
	}
}

func encodeTree(t *testing.T, snap types.Snapshot) Tree {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return tree(t, string(raw))
}

// nullReplacements lists the paths at which patch replaces a value with null.
func nullReplacements(patch *Patch, path string) []string {
	if patch == nil {
		return nil
	}
	var found []string
	if string(patch.Replace) == "null" {
		found = append(found, path)
	}
	for name, sub := range patch.Object {
		found = append(found, nullReplacements(sub, join(path, name))...)
	}
	for index, sub := range patch.Index {
		found = append(found, nullReplacements(sub, path+"["+index+"]")...)
	}
	if patch.Keyed != nil {
		for id, sub := range patch.Keyed.Patches {
			found = append(found, nullReplacements(sub, path+"["+id+"]")...)
		}
		for id, raw := range patch.Keyed.New {
			if string(raw) == "null" {
				found = append(found, path+"["+id+"]")
			}
		}
	}
	return found
}
