package types

import (
	"reflect"
	"sync"
)

// ReplaceNilSlices replaces every nil slice reachable from v through structs,
// pointers, slices and arrays with an empty, non-nil one, so that
// encoding/json writes [] where it would have written null. v must be a
// pointer; anything else has nothing addressable to change and is left alone.
//
// Maps and interfaces are leaves: their contents are not addressable, and
// replacing a map value in place would write to a map other goroutines may be
// reading. Fields promoted from an unexported embedded struct are skipped too,
// because reflection will not set them, though encoding/json encodes them. No
// payload type in this package has any of these, and
// TestWirePayloads_HoldNothingTheWalkCannotEnter fails the day one does, so
// that whoever adds it decides how its lists reach the wire rather than having
// them silently skipped.
//
// # Why
//
// A nil Go slice marshals to JSON null, so a host with no NPUs used to send
// "npus": null and an NPU with no engines "engines": null. The dashboard
// dereferences these lists with .map, .length and spreads, and a null in any
// of them is a thrown exception that blanks a panel. The owner's ruling on #151
// is that the wire never carries null for a list: an empty list and "none"
// are the same answer, and the distinction is dropped deliberately.
//
// Normalising here, by reflection, rather than at each place a collector
// builds a list is what makes the guarantee hold for a field that does not
// exist yet. There are dozens of list fields filled by a dozen collectors,
// most of them leave a list nil precisely on the host that has none of the
// thing -- the case nobody's development machine exercises -- and a field
// added next month would be one more site to remember. One walk over the
// finished value has nothing to remember.
//
// # Where it runs, and why that is safe
//
// It runs where a value is finished and BEFORE it is shared: at the end of a
// collection, in the monitor before a snapshot is published, on a peer's
// snapshot as it is decoded, and on each REST response as it is written. It
// writes only to a slice that is nil; a value that has already been through it
// is only read. That is what lets the later passes run over data other
// goroutines are reading: everything they reach that is shared was normalised
// before it was published, so for them the walk is read-only.
//
// # Cost
//
// The walk is planned once per type and the plan cached, and it never descends
// into a slice whose elements cannot contain a slice. The process list -- a
// thousand entries on a busy host and the bulk of every snapshot -- is
// therefore a single nil check rather than a thousand. BenchmarkReplaceNilSlices
// measures a representative snapshot.
func ReplaceNilSlices(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	var w sliceWalker
	w.walk(rv.Elem())
}

// sliceWalker carries one walk's state.
//
// A value can point back into itself, through a pointer or through a slice
// whose backing array holds an ancestor. encoding/json refuses such a value,
// so no payload holds one, but the walk must still terminate on it. It does
// what encoding/json does: count how deep it is, and only past a depth no real
// payload reaches start recording the pointers and slices it has entered. A
// snapshot is about six levels deep, so an ordinary walk records nothing and
// allocates nothing.
type sliceWalker struct {
	depth   int
	entered map[visit]bool
}

// cycleCheckDepth is how deep a walk goes before it starts recording what it
// has entered.
const cycleCheckDepth = 64

// visit identifies a pointer or slice already entered. The type is part of the
// key because a struct and its first field share an address, and the length
// because two slices of one array can start at the same element.
type visit struct {
	addr uintptr
	len  int
	typ  reflect.Type
}

// enter reports whether the walk should descend into v, a non-nil pointer or
// slice, recording it once the walk is deep enough to be in a cycle.
func (w *sliceWalker) enter(v reflect.Value) bool {
	if w.depth < cycleCheckDepth {
		return true
	}
	key := visit{addr: v.Pointer(), typ: v.Type()}
	if v.Kind() == reflect.Slice {
		key.len = v.Len()
	}
	if w.entered[key] {
		return false
	}
	if w.entered == nil {
		w.entered = make(map[visit]bool)
	}
	w.entered[key] = true
	return true
}

// walk visits one value. Only the kinds that can lead to a slice are handled;
// every other kind is a leaf.
func (w *sliceWalker) walk(v reflect.Value) {
	w.depth++
	defer func() { w.depth-- }()

	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() && w.enter(v) {
			w.walk(v.Elem())
		}
	case reflect.Struct:
		for _, i := range planFor(v.Type()).fields {
			w.walk(v.Field(i))
		}
	case reflect.Slice:
		if v.IsNil() {
			if v.CanSet() {
				v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			}
			return
		}
		if planFor(v.Type().Elem()).reachesSlice && w.enter(v) {
			for i := 0; i < v.Len(); i++ {
				w.walk(v.Index(i))
			}
		}
	case reflect.Array:
		if planFor(v.Type().Elem()).reachesSlice {
			for i := 0; i < v.Len(); i++ {
				w.walk(v.Index(i))
			}
		}
	}
}

// slicePlan is what the walk needs to know about one type.
type slicePlan struct {
	// reachesSlice is true when a value of the type is, or can contain, a
	// slice. A type for which it is false is never entered.
	reachesSlice bool

	// fields are the indices of a struct's exported fields that reach a
	// slice. Unexported fields are not encoded and cannot be set.
	fields []int
}

// slicePlans caches a *slicePlan per reflect.Type. Reads are lock-free, which
// matters because every snapshot consults it many times.
var slicePlans sync.Map

// planFor returns the cached plan for t, building it on first use.
func planFor(t reflect.Type) *slicePlan {
	if plan, ok := slicePlans.Load(t); ok {
		return plan.(*slicePlan)
	}
	plan := buildPlan(t, map[reflect.Type]bool{})
	actual, _ := slicePlans.LoadOrStore(t, plan)
	return actual.(*slicePlan)
}

// buildPlan works out whether t can lead to a slice. visiting holds the types
// on the current path, so a recursive type terminates; one is assumed to reach
// a slice, which costs a walk at worst and never misses one.
func buildPlan(t reflect.Type, visiting map[reflect.Type]bool) *slicePlan {
	if visiting[t] {
		return &slicePlan{reachesSlice: true}
	}
	visiting[t] = true
	defer delete(visiting, t)

	switch t.Kind() {
	case reflect.Slice:
		return &slicePlan{reachesSlice: true}
	case reflect.Pointer, reflect.Array:
		return &slicePlan{reachesSlice: buildPlan(t.Elem(), visiting).reachesSlice}
	case reflect.Struct:
		plan := &slicePlan{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			if buildPlan(field.Type, visiting).reachesSlice {
				plan.fields = append(plan.fields, i)
			}
		}
		plan.reachesSlice = len(plan.fields) > 0
		return plan
	default:
		return &slicePlan{}
	}
}
