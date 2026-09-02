package delta_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// populatedSnapshot fills every array the key table names, with more than one
// element each, so a path that no longer exists or a key that is not unique
// shows up here rather than as an oversized delta in production.
func populatedSnapshot() *types.Snapshot {
	return &types.Snapshot{
		Timestamp: time.Unix(1788300000, 0).UTC(),
		Alerts: []types.Alert{
			{ID: "a1", Rule: "cpu_high"},
			{ID: "a2", Rule: "disk_full"},
		},
		CPUs: []types.CPUInfo{{Index: 0}, {Index: 1}},
		GPUs: []types.GPUInfo{{Index: 0}, {Index: 1}},
		Memory: types.MemoryInfo{
			DIMMs: []types.DIMMInfo{{Index: 0}, {Index: 1}},
		},
		Disks: []types.DiskInfo{
			{
				Name:       "sda",
				Partitions: []types.PartitionInfo{{Device: "sda1"}, {Device: "sda2"}},
				SMARTAttrs: []types.SMARTAttr{{ID: 5, Name: "Reallocated"}, {ID: 9, Name: "Power_On_Hours"}},
				RAID:       &types.RAIDInfo{Members: []types.RAIDMember{{Name: "sda2"}, {Name: "sdb2"}}},
			},
			{Name: "sdb"},
		},
		Networks: []types.NetworkInfo{{Name: "eno1"}, {Name: "lo"}},
		Processes: types.ProcessSummary{
			Processes: []types.ProcessDetail{{PID: 1}, {PID: 2}},
		},
		Sensors: types.SensorData{
			CoreTemps:    []types.CoreTemp{{CoreID: 0}, {CoreID: 1}},
			ThermalZones: []types.ThermalZone{{Name: "acpitz"}, {Name: "x86_pkg_temp"}},
		},
		Virt: types.VirtInfo{
			Containers: []types.ContainerInfo{{ID: "c1"}, {ID: "c2"}},
			VMs:        []types.VMInfo{{PID: 100}, {PID: 200}},
			Runtime: types.RuntimeInfo{
				Images: []types.ImageInfo{{ID: "i1"}, {ID: "i2"}},
			},
		},
	}
}

// TestArrayKeys_MatchTheSnapshot checks the table against the real types.
//
// Nothing in the compiler connects a path string to a struct field. A path that
// a rename has moved, or a key that is not actually unique, both fail the same
// way: the array quietly falls back to positional diffing, deltas grow by two
// orders of magnitude on the list that reorders most, and every test still
// passes. This is the only thing that catches it.
func TestArrayKeys_MatchTheSnapshot(t *testing.T) {
	raw, err := json.Marshal(populatedSnapshot())
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}
	tree, err := delta.Decode(raw)
	if err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}

	for path, key := range delta.ArrayKeys() {
		t.Run(path, func(t *testing.T) {
			arrays := resolve(tree, strings.Split(path, "."))
			if len(arrays) == 0 {
				t.Fatalf("no array at %q; the path no longer exists in a snapshot", path)
			}
			for _, array := range arrays {
				if len(array) < 2 {
					t.Fatalf("the fixture puts %d elements at %q; it needs at least two "+
						"for uniqueness to mean anything", len(array), path)
				}
				seen := make(map[string]bool, len(array))
				for _, element := range array {
					object, ok := element.(map[string]any)
					if !ok {
						t.Fatalf("%q holds something that is not an object", path)
					}
					value, ok := object[key]
					if !ok {
						t.Fatalf("elements of %q have no field %q; the fields are %v",
							path, key, fieldNames(object))
					}
					id := renderKey(value)
					if id == "" {
						t.Fatalf("%q at %q is not a scalar and cannot identify an element", key, path)
					}
					// Identities cross a language boundary: Go renders them
					// from the JSON text, the browser from the number
					// JSON.parse produced. Those agree for a string and for an
					// integer inside 2^53, and can disagree for anything else
					// -- 1.50 against 1.5, 1e3 against 1000. A key that could
					// stringify two ways would leave the two implementations
					// matching different elements, silently.
					if _, isNumber := value.(json.Number); isNumber {
						if strings.ContainsAny(id, ".eE") {
							t.Fatalf("%q at %q renders as %q; a key must be a string or "+
								"a plain integer, or Go and the browser will not agree on it",
								key, path, id)
						}
						if len(id) > 15 {
							t.Fatalf("%q at %q renders as %q, which is past what a browser "+
								"parses exactly", key, path, id)
						}
					}
					if seen[id] {
						t.Fatalf("%q is not unique within %q (%q appears twice); matching by it "+
							"would merge two elements into one", key, path, id)
					}
					seen[id] = true
				}
			}
		})
	}
}

// resolve walks a dotted path, descending into arrays at "[]" steps and
// returning every array the path names.
func resolve(node any, parts []string) [][]any {
	if len(parts) == 0 {
		if array, ok := node.([]any); ok {
			return [][]any{array}
		}
		return nil
	}

	name, rest := parts[0], parts[1:]
	step, descend := strings.CutSuffix(name, "[]")

	object, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	child, ok := object[step]
	if !ok {
		return nil
	}
	if !descend {
		return resolve(child, rest)
	}

	array, ok := child.([]any)
	if !ok {
		return nil
	}
	var out [][]any
	for _, element := range array {
		out = append(out, resolve(element, rest)...)
	}
	return out
}

func renderKey(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	return ""
}

func fieldNames(object map[string]any) []string {
	out := make([]string, 0, len(object))
	for name := range object {
		out = append(out, name)
	}
	return out
}
