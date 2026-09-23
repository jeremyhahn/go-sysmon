package types_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

// wirePayloads are the roots of every JSON document the server sends the
// dashboard: the snapshot (streamed, and served whole), the fleet node
// envelope, the fleet view, the settings view and the two history answers.
func wirePayloads() []any {
	return []any{
		types.Snapshot{},
		types.PeerSnapshot{},
		types.NodeSummary{},
		types.Fleet{},
		types.ConfigView{},
		types.HistoryCatalog{},
		types.History{},
	}
}

// TestReplaceNilSlices_NoListInAnyPayloadEncodesAsNull is the regression guard
// for #151: no list field of any payload reaches the wire as null.
//
// It is driven by reflection over pkg/types, not by a list of field names, so
// a list field added later is covered without anyone remembering to add it.
// Each payload is tested three ways. As its zero value: the host with none of
// anything, where every top-level list is nil. With every pointer allocated and
// every list left nil, which reaches the lists behind a pointer -- a fleet
// node's snapshot. And as a shell in which every list holds a populated
// element followed by emptier ones: those are where every NESTED list is nil
// (an NPU with no engines, a bond with no slaves, a RAID volume with no
// members), and the populated one is what lets the walk reach the level below.
//
// And it checks that it tested something. The fixture must produce a null for
// every list field the type graph contains, before normalisation; a fixture
// that silently stopped reaching part of the tree would otherwise report a
// clean pass while checking nothing there.
func TestReplaceNilSlices_NoListInAnyPayloadEncodesAsNull(t *testing.T) {
	for _, payload := range wirePayloads() {
		typ := reflect.TypeOf(payload)
		t.Run(typ.Name(), func(t *testing.T) {
			want := sliceFieldPaths(typ)
			if len(want) == 0 {
				t.Fatalf("%s has no list fields; it does not belong in this table", typ.Name())
			}

			covered := map[string]bool{}
			for _, fixture := range []reflect.Value{reflect.New(typ), pointersOnly(typ), shell(typ)} {
				before := nullSlicePaths(t, fixture.Interface(), payload)
				for _, path := range before {
					covered[typePath(path)] = true
				}

				types.ReplaceNilSlices(fixture.Interface())

				if after := nullSlicePaths(t, fixture.Interface(), payload); len(after) != 0 {
					t.Errorf("lists still encoded as null after ReplaceNilSlices: %v", after)
				}
			}

			for _, path := range want {
				if !covered[path] {
					t.Errorf("the fixture never produced a nil %q, so the guard did not check it", path)
				}
			}
		})
	}
}

// TestWirePayloads_HoldNothingTheWalkCannotEnter keeps ReplaceNilSlices'
// guarantee complete. It treats maps and interfaces as leaves, and it cannot
// set a field promoted from an unexported embedded struct -- reflection makes
// such fields read-only, though encoding/json still encodes them. A list in
// any of those would reach the wire as null. None exists in any payload today;
// this fails when one is added, instead of letting its lists be skipped
// silently.
func TestWirePayloads_HoldNothingTheWalkCannotEnter(t *testing.T) {
	for _, payload := range wirePayloads() {
		root := reflect.TypeOf(payload)
		if found := unwalkable(root); len(found) != 0 {
			t.Errorf("%s holds fields ReplaceNilSlices cannot enter: %v\n"+
				"extend ReplaceNilSlices (and this guard) before adding one to a wire payload",
				root.Name(), found)
		}
	}
}

type hiddenEmbedding struct {
	Items []int `json:"items"`
}

func TestUnwalkable_FindsThem(t *testing.T) {
	type holder struct {
		hiddenEmbedding
		Labels  map[string][]string `json:"labels"`
		Payload any                 `json:"payload"`
		Items   []struct {
			Extra map[string]int `json:"extra"`
		} `json:"items"`
	}
	got := unwalkable(reflect.TypeOf(holder{}))
	want := []string{"(embedded hiddenEmbedding)", "items[].extra", "labels", "payload"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// unwalkable lists the paths, by type, of every map, interface and unexported
// embedded struct reachable from t.
func unwalkable(t reflect.Type) []string {
	var out []string
	var visit func(t reflect.Type, path string, depth int)
	visit = func(t reflect.Type, path string, depth int) {
		if depth > maxShellDepth {
			return
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Map, reflect.Interface:
			out = append(out, path)
		case reflect.Slice, reflect.Array:
			visit(t.Elem(), path+"[]", depth+1)
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if f.Anonymous && !f.IsExported() {
					out = append(out, joinTypePath(path, "(embedded "+f.Type.Name()+")"))
					continue
				}
				name, ok := jsonFieldName(f)
				if !ok {
					continue
				}
				visit(f.Type, joinTypePath(path, name), depth+1)
			}
		}
	}
	visit(t, "", 0)
	sort.Strings(out)
	return out
}

func joinTypePath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// TestReplaceNilSlices_KeepsPopulatedListsUntouched: normalisation must not
// replace, truncate or reallocate a list that has contents.
func TestReplaceNilSlices_KeepsPopulatedListsUntouched(t *testing.T) {
	snap := types.Snapshot{
		CPUs: []types.CPUInfo{{Index: 3, Flags: []string{"sse4_2", "avx2"}}},
	}
	cpus := snap.CPUs
	flags := snap.CPUs[0].Flags

	types.ReplaceNilSlices(&snap)

	if &snap.CPUs[0] != &cpus[0] {
		t.Error("a populated list was reallocated")
	}
	if &snap.CPUs[0].Flags[0] != &flags[0] || len(snap.CPUs[0].Flags) != 2 {
		t.Errorf("a populated nested list changed: %v", snap.CPUs[0].Flags)
	}
	if snap.CPUs[0].Index != 3 {
		t.Errorf("an element's scalar changed: index %d", snap.CPUs[0].Index)
	}
	if snap.GPUs == nil || len(snap.GPUs) != 0 {
		t.Errorf("a nil sibling list was not replaced with an empty one: %#v", snap.GPUs)
	}
}

// TestReplaceNilSlices_IgnoresWhatItCannotChange covers every input that has
// nothing addressable: none may panic, and none may be altered.
func TestReplaceNilSlices_IgnoresWhatItCannotChange(t *testing.T) {
	var nilSnapshot *types.Snapshot
	value := types.Fleet{}

	tests := []struct {
		name string
		v    any
	}{
		{"untyped nil", nil},
		{"nil pointer", nilSnapshot},
		{"struct by value", value},
		{"scalar", 42},
		{"pointer to scalar", new(int)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			types.ReplaceNilSlices(tt.v)
		})
	}
	if value.Nodes != nil {
		t.Error("a struct passed by value was modified through a copy")
	}
}

// TestReplaceNilSlices_TopLevelSlice: a pointer to a list is itself a list to
// fill, and its elements are walked.
func TestReplaceNilSlices_TopLevelSlice(t *testing.T) {
	var empty []types.HistorySeries
	types.ReplaceNilSlices(&empty)
	if empty == nil {
		t.Error("a nil top-level list was left nil")
	}

	series := []types.HistorySeries{{Name: "sda"}}
	types.ReplaceNilSlices(&series)
	if series[0].Points == nil {
		t.Error("an element of a top-level list was not walked")
	}
}

// TestReplaceNilSlices_Arrays: a fixed-size array is walked element by
// element, since an element can hold a list.
func TestReplaceNilSlices_Arrays(t *testing.T) {
	var pair [2]types.HistorySeries
	types.ReplaceNilSlices(&pair)
	for i, s := range pair {
		if s.Points == nil {
			t.Errorf("array element %d still has a nil list", i)
		}
	}

	// An array whose element cannot hold a list is not entered at all.
	var scalars [3]int
	types.ReplaceNilSlices(&scalars)
}

// TestReplaceNilSlices_SkipsUnexportedFields: an unexported field is not
// encoded and cannot be set through reflection. Touching it would panic.
func TestReplaceNilSlices_SkipsUnexportedFields(t *testing.T) {
	type withHidden struct {
		Visible []int
		hidden  []int
	}
	v := withHidden{}
	types.ReplaceNilSlices(&v)
	if v.Visible == nil {
		t.Error("the exported list was not filled")
	}
	if v.hidden != nil {
		t.Error("the unexported list was changed")
	}
}

// TestReplaceNilSlices_RecursiveTypes: a type that contains itself must not
// send planning into an infinite loop, and must still be walked at every depth.
func TestReplaceNilSlices_RecursiveTypes(t *testing.T) {
	type node struct {
		Name     string
		Next     *node
		Children []node
	}
	tree := node{Next: &node{}, Children: []node{{}}}
	types.ReplaceNilSlices(&tree)
	if tree.Next.Children == nil {
		t.Error("a list behind a pointer to the same type was not filled")
	}
	if tree.Children[0].Children == nil {
		t.Error("a list inside an element of the same type was not filled")
	}
}

// TestReplaceNilSlices_PointerCycles: a value that points back into itself is
// walked once, not until the stack is exhausted.
func TestReplaceNilSlices_PointerCycles(t *testing.T) {
	type node struct {
		Next  *node
		Items []int
	}
	a := &node{}
	b := &node{Next: a}
	a.Next = b

	types.ReplaceNilSlices(a)

	if a.Items == nil || b.Items == nil {
		t.Error("a node on the cycle was not normalised")
	}
}

// TestReplaceNilSlices_SliceCycles: a cycle can run through a slice as well as
// a pointer -- an element whose children include its parent.
func TestReplaceNilSlices_SliceCycles(t *testing.T) {
	type node struct {
		Children []node
		Items    []int
	}
	root := make([]node, 1)
	root[0].Children = root

	types.ReplaceNilSlices(&root)

	if root[0].Items == nil {
		t.Error("the node on the cycle was not normalised")
	}
}

// TestReplaceNilSlices_ReadOnlyOnceNormalised is the property the concurrent
// call sites rely on: the walk writes only to a nil list, so a value that has
// been through it can be walked again by any number of goroutines while others
// encode it. Under -race, a write here would fail the run.
func TestReplaceNilSlices_ReadOnlyOnceNormalised(t *testing.T) {
	snap := representativeSnapshot()
	types.ReplaceNilSlices(&snap)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			types.ReplaceNilSlices(&snap)
		}()
		go func() {
			defer wg.Done()
			if _, err := json.Marshal(&snap); err != nil {
				t.Errorf("marshal: %v", err)
			}
		}()
	}
	wg.Wait()
}

// BenchmarkReplaceNilSlices measures one pass over a snapshot shaped like a
// busy host's -- 24 logical CPUs, a thousand processes, a dozen disks and
// interfaces -- in the two states it meets in service. "first" is a freshly
// collected snapshot with nil lists to fill. "normalised" is the steady-state
// cost of the later passes, which only read. Encoding the same snapshot is
// measured alongside for scale.
func BenchmarkReplaceNilSlices(b *testing.B) {
	b.Run("first", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			snap := representativeSnapshot()
			b.StartTimer()
			types.ReplaceNilSlices(&snap)
		}
	})
	b.Run("normalised", func(b *testing.B) {
		snap := representativeSnapshot()
		types.ReplaceNilSlices(&snap)
		b.ReportAllocs()
		for b.Loop() {
			types.ReplaceNilSlices(&snap)
		}
	})
	b.Run("json.Marshal for scale", func(b *testing.B) {
		snap := representativeSnapshot()
		types.ReplaceNilSlices(&snap)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := json.Marshal(&snap); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// representativeSnapshot is a busy host's snapshot with the lists a real one
// leaves nil: every per-device list that the device does not populate.
func representativeSnapshot() types.Snapshot {
	snap := types.Snapshot{}
	for i := 0; i < 24; i++ {
		snap.CPUs = append(snap.CPUs, types.CPUInfo{Index: i, Flags: []string{"fpu", "vme", "de", "pse", "sse4_2", "avx2"}})
	}
	for i := 0; i < 12; i++ {
		snap.Disks = append(snap.Disks, types.DiskInfo{Index: i, Name: "sd" + strconv.Itoa(i)})
		snap.Networks = append(snap.Networks, types.NetworkInfo{Index: i, Name: "eth" + strconv.Itoa(i), Addresses: []string{"192.0.2.1/24"}})
	}
	for i := 0; i < 1000; i++ {
		snap.Processes.Processes = append(snap.Processes.Processes, types.ProcessDetail{PID: int32(i), Name: "proc"})
	}
	for i := 0; i < 12; i++ {
		snap.Sensors.CoreTemps = append(snap.Sensors.CoreTemps, types.CoreTemp{CoreID: i})
	}
	return snap
}

// nullSlicePaths encodes v and reports the list fields encoded as null.
func nullSlicePaths(t *testing.T, v, like any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	paths, err := typestest.NullSlices(raw, like)
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	return paths
}

var arrayIndex = regexp.MustCompile(`\[\d+\]`)

// typePath turns an instance path ("npus[1].engines") into the type-level path
// sliceFieldPaths reports ("npus[].engines").
func typePath(path string) string {
	return arrayIndex.ReplaceAllString(path, "[]")
}

// sliceFieldPaths lists every list field reachable from t by type, in the form
// typePath produces. It follows the same JSON naming the encoder does.
func sliceFieldPaths(t reflect.Type) []string {
	var out []string
	var visit func(t reflect.Type, path string, depth int)
	visit = func(t reflect.Type, path string, depth int) {
		if depth > maxShellDepth {
			return
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Slice, reflect.Array:
			if t.Kind() == reflect.Slice {
				out = append(out, path)
			}
			visit(t.Elem(), path+"[]", depth+1)
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if f.Anonymous && f.IsExported() && f.Tag.Get("json") == "" && f.Type.Kind() == reflect.Struct {
					// Flattened into the parent by encoding/json.
					visit(f.Type, path, depth)
					continue
				}
				name, ok := jsonFieldName(f)
				if !ok {
					continue
				}
				child := name
				if path != "" {
					child = path + "." + name
				}
				visit(f.Type, child, depth+1)
			}
		}
	}
	visit(t, "", 0)
	sort.Strings(out)
	return out
}

func jsonFieldName(f reflect.StructField) (string, bool) {
	if !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			tag = tag[:i]
			break
		}
	}
	if tag == "" {
		return f.Name, true
	}
	return tag, true
}

// maxShellDepth bounds the fixture for a type that contains itself. pkg/types
// has none today; the bound is what keeps the guard terminating if it gains
// one.
const maxShellDepth = 16

// pointersOnly builds a pointer to a value of type t in which every pointer is
// allocated and every list is left nil.
func pointersOnly(t reflect.Type) reflect.Value {
	v := reflect.New(t)
	allocate(v.Elem(), 0)
	return v
}

func allocate(v reflect.Value, depth int) {
	if depth > maxShellDepth {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		allocate(v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				allocate(v.Field(i), depth+1)
			}
		}
	}
}

// shell builds a pointer to a value of type t in which every pointer is
// allocated and every list holds three elements: a shell, a zero value, and a
// value with its pointers allocated and its lists nil. The third is what
// reaches a list behind a pointer inside a list -- a disk's RAID members.
func shell(t reflect.Type) reflect.Value {
	v := reflect.New(t)
	fill(v.Elem(), 0)
	return v
}

func fill(v reflect.Value, depth int) {
	if depth > maxShellDepth {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 3, 3)
		fill(s.Index(0), depth+1)
		allocate(s.Index(2), depth+1)
		v.Set(s)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fill(v.Index(i), depth+1)
		}
	}
}
