// Package delta turns consecutive snapshots into sparse patches.
//
// The event stream used to encode a whole snapshot for every client on every
// tick. A snapshot of this host is 270 kB of JSON, of which about 74% is the
// process list and most of the rest is hardware description that never changes
// -- CPU flags, DIMM part numbers, disk models, interface MAC addresses. Sending
// it repeatedly costs O(clients x metrics) per tick in encoding alone, before
// the per-client gzip stream on top.
//
// Measured on a 24-core host with 1002 processes, one second apart:
//
//	full snapshot          270 kB raw, 24.7 kB gzipped on a warm stream
//	delta                  3.4 kB raw, 0.6 kB gzipped
//	delta, list churning   29 kB raw, 7.7 kB gzipped
//	JSON encoding          0.85 ms per full snapshot, 0.026 ms per delta
//
// The encoding saving is the structural one: a delta is computed ONCE per tick
// against the previous snapshot and the same bytes go to every client that is
// up to date, so the cost becomes O(metrics) plus O(clients) writes rather than
// O(clients x metrics).
//
// # The format
//
// A patch is a tree mirroring the snapshot's shape, holding only what changed.
// Exactly one field of Patch is ever set, and which one says how to apply it:
//
//	{"r": <value>}   replace this node outright
//	{"o": {...}}     an object: apply each named sub-patch, leave the rest
//	{"i": {...}}     an array of equal length: apply by position
//	{"a": {...}}     an array whose elements have a stable identity: apply by it
//
// Absence means "unchanged", so a field that did not move costs nothing.
//
// # Why identity, not position
//
// Diffing the process list by position collapses the moment it reorders, which
// it does constantly: every surviving process appears to have changed. On the
// measurements above that is the difference between a 3 kB delta and a 206 kB
// one. Arrays listed in arrayKeys are therefore matched by the field that
// identifies their elements -- pid, name, index -- and everything else falls
// back to position, then to wholesale replacement. The fallbacks are always
// correct; the table only decides how small the result is.
//
// The key travels in the patch rather than being looked up by both sides. A
// client with its own copy of the table would apply the wrong one the moment
// the two drifted, and would do it silently.
package delta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Patch is one node of a delta. Exactly one field is set.
type Patch struct {
	// Replace holds the node's new value.
	//
	// A raw message rather than a *json.RawMessage, and that is load-bearing:
	// replacing a value with JSON null is a real change, and encoding/json
	// decodes null into a POINTER field by setting the pointer to nil -- which
	// is also how "this patch does not replace anything" is spelled. The two
	// would be indistinguishable after a round trip through the wire, and the
	// receiver would silently keep the old value. A value RawMessage decodes
	// null as the four bytes "null", which is not empty, so omitempty still
	// omits the absent case and only the absent case.
	Replace json.RawMessage `json:"r,omitempty"`

	// Object patches named fields of an object, leaving unnamed ones alone.
	Object map[string]*Patch `json:"o,omitempty"`

	// Index patches array elements by position. Only used when both arrays
	// have the same length, so a position always refers to the same slot.
	Index map[string]*Patch `json:"i,omitempty"`

	// Keyed patches an array by element identity.
	Keyed *KeyedPatch `json:"a,omitempty"`
}

// KeyedPatch describes an array whose elements are matched by an identity
// field.
type KeyedPatch struct {
	// Key is the field that identifies an element, e.g. "pid".
	Key string `json:"k"`

	// Order is the identities of the new array, in order. It is sent only when
	// the order changed, which -- since adding or removing an element changes
	// it too -- means it is present whenever membership changed. When absent
	// the receiver keeps the array it has and patches in place.
	//
	// A POINTER, and that is load-bearing for the same reason Replace is not
	// one. An array that lost its last element has an order, and it is empty;
	// "no order" means the membership did not change. As a plain []string with
	// omitempty those two encode identically -- the field simply disappears --
	// so the last process exiting was sent as "nothing about membership
	// changed" and every receiver kept the process it already had, on a
	// contiguous frame, rendered as current. Exactly the failure this format
	// exists to prevent, and invisible to a round-trip test that never
	// encodes.
	Order *[]string `json:"ord,omitempty"`

	// Patches are the changes to elements that survived, by identity.
	Patches map[string]*Patch `json:"p,omitempty"`

	// New are elements that were not there before, whole, by identity.
	New map[string]json.RawMessage `json:"n,omitempty"`
}

// arrayKeys names the field that identifies the elements of an array, by the
// array's path through the snapshot.
//
// Paths are JSON field names joined with ".", with "[]" marking a step through
// an array -- "disks[].smart_attrs" is the SMART attributes of any disk.
//
// This table is a size optimisation and not a correctness requirement: an array
// missing from it is diffed by position, or replaced wholesale when its length
// changed. Both are correct, just larger. What matters is that the identity
// named here is genuinely unique within its array, which delta_test.go checks
// against a real snapshot -- a key that repeats would silently merge two
// elements into one.
var arrayKeys = map[string]string{
	"alerts":                        "id",
	"cpus":                          "index",
	"disks":                         "name",
	"disks[].members":               "device_id",
	"disks[].partitions":            "device",
	"disks[].raid.members":          "name",
	"disks[].smart_attrs":           "id",
	"gpus":                          "index",
	"npus":                          "index",
	"npus[].clients":                "pid",
	"npus[].engines":                "name",
	"memory.dimms":                  "index",
	"networks":                      "name",
	"processes.process_list":        "pid",
	"sensors.core_temps":            "core_id",
	"sensors.thermal_zones":         "name",
	"virtualization.containers":     "id",
	"virtualization.runtime.images": "id",
	"virtualization.vms":            "pid",
}

// Tree is a snapshot decoded into generic JSON values.
//
// Numbers are kept as json.Number rather than float64. Two reasons: a 64-bit
// counter past 2^53 would lose precision through float64 and come back changed,
// which would show up as an endless stream of deltas for a value that never
// moved; and comparing the original text is both exact and cheaper than
// comparing floats.
type Tree = any

// Decode reads JSON into a Tree.
func Decode(raw []byte) (Tree, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree Tree
	if err := dec.Decode(&tree); err != nil {
		return nil, &ErrDecode{Err: err}
	}
	return tree, nil
}

// Diff describes what changed between two trees, or nil when nothing did.
func Diff(before, after Tree) (*Patch, error) {
	return diff(before, after, "")
}

func diff(before, after Tree, path string) (*Patch, error) {
	if reflect.DeepEqual(before, after) {
		return nil, nil
	}

	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return replace(after)
		}
		return diffObject(b, a, path)

	case []any:
		a, ok := after.([]any)
		if !ok {
			return replace(after)
		}
		return diffArray(b, a, path)

	default:
		// Scalars, and a value whose type changed. Either way the whole node
		// is sent: there is nothing smaller to describe.
		return replace(after)
	}
}

// diffObject patches the fields that moved.
//
// A key set that changed is replaced wholesale rather than patched. Go structs
// marshal a fixed set of keys, so this happens only when a field is added or
// removed by a build, or when a value stopped being an object -- rare enough
// that the extra bytes cost nothing, and far cheaper to reason about than a
// deletion instruction that would exist to be exercised by nothing.
func diffObject(before, after map[string]any, path string) (*Patch, error) {
	if len(before) != len(after) {
		return replace(after)
	}
	fields := make(map[string]*Patch)
	for name, value := range after {
		previous, ok := before[name]
		if !ok {
			return replace(after)
		}
		sub, err := diff(previous, value, join(path, name))
		if err != nil {
			return nil, err
		}
		if sub != nil {
			fields[name] = sub
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &Patch{Object: fields}, nil
}

// diffArray picks the cheapest correct description of an array's change.
func diffArray(before, after []any, path string) (*Patch, error) {
	if key, ok := arrayKeys[path]; ok {
		if patch, ok, err := diffKeyed(before, after, path, key); err != nil || ok {
			return patch, err
		}
		// An element without the key, or a repeated key: fall through. The
		// identity is not usable here, and guessing would merge two elements.
	}
	if len(before) == len(after) {
		return diffIndexed(before, after, path)
	}
	return replace(after)
}

// diffKeyed matches elements by identity. The second return is false when the
// array cannot be keyed this way, leaving the caller to fall back.
func diffKeyed(before, after []any, path, key string) (*Patch, bool, error) {
	previous, ok := indexByKey(before, key)
	if !ok {
		return nil, false, nil
	}
	if _, ok := indexByKey(after, key); !ok {
		return nil, false, nil
	}

	patch := &KeyedPatch{Key: key}
	elementPath := path + "[]"
	for _, element := range after {
		id, _ := identity(element, key)
		if old, existed := previous[id]; existed {
			sub, err := diff(old, element, elementPath)
			if err != nil {
				return nil, false, err
			}
			if sub != nil {
				if patch.Patches == nil {
					patch.Patches = make(map[string]*Patch)
				}
				patch.Patches[id] = sub
			}
			continue
		}
		raw, err := json.Marshal(element)
		if err != nil {
			return nil, false, &ErrEncode{Path: elementPath, Err: err}
		}
		if patch.New == nil {
			patch.New = make(map[string]json.RawMessage)
		}
		patch.New[id] = raw
	}

	if order, changed := orderIfChanged(before, after, key); changed {
		patch.Order = &order
	}
	if patch.Order == nil && patch.Patches == nil && patch.New == nil {
		return nil, true, nil
	}
	return &Patch{Keyed: patch}, true, nil
}

// diffIndexed patches by position, for arrays of equal length that have no
// usable identity -- the fixed hardware lists, mostly.
func diffIndexed(before, after []any, path string) (*Patch, error) {
	elements := make(map[string]*Patch)
	elementPath := path + "[]"
	for i := range after {
		sub, err := diff(before[i], after[i], elementPath)
		if err != nil {
			return nil, err
		}
		if sub != nil {
			elements[itoa(i)] = sub
		}
	}
	if len(elements) == 0 {
		return nil, nil
	}
	return &Patch{Index: elements}, nil
}

// indexByKey maps each element to its identity, reporting false when any
// element lacks the key or two share one.
func indexByKey(elements []any, key string) (map[string]any, bool) {
	out := make(map[string]any, len(elements))
	for _, element := range elements {
		id, ok := identity(element, key)
		if !ok {
			return nil, false
		}
		if _, duplicate := out[id]; duplicate {
			return nil, false
		}
		out[id] = element
	}
	return out, true
}

// identity renders an element's key field as a string.
//
// Only scalars can identify an element: an object or an array as an identity
// would be a modelling mistake, not a key. Numbers are further restricted to
// plain integers a browser parses exactly, because an identity crosses a
// language boundary -- this side renders it from the JSON text, the receiver
// from whatever JSON.parse produced. Those agree for a string and for an
// integer inside 2^53, and disagree for 1e3 against 1000, or for 1.50 against
// 1.5. A key that could be spelled two ways would have the two sides matching
// different elements, and the receiver would find no patch for an element it
// holds and keep the value it already had -- a stale reading, presented as
// current, with nothing raised anywhere.
//
// Refusing here is not a loss: indexByKey reports the array as unkeyable and
// the differ falls back to position, which is correct and merely larger.
func identity(element any, key string) (string, bool) {
	object, ok := element.(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := object[key]
	if !ok {
		return "", false
	}
	switch v := value.(type) {
	case string:
		return v, true
	case json.Number:
		if !isExactInteger(v.String()) {
			return "", false
		}
		return v.String(), true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// maxExactDigits is how many decimal digits stay exact through a browser's
// number type. 2^53-1 has sixteen, so fifteen is safe for any sign.
const maxExactDigits = 15

// isExactInteger reports whether a JSON number's text is a plain integer that
// survives a round trip through a float64 unchanged AND is spelled the one way
// a browser would spell it.
//
// Both halves matter, and the second is easy to miss. This side renders an
// identity from the JSON TEXT; the receiver renders it from String(number)
// after JSON.parse. "1e3" and "1.50" fail the first test. "007" and "-0" pass
// it and still disagree: a browser renders them "7" and "0". The consequence is
// the same either way -- the two sides match different elements, the receiver
// finds no patch for an element it holds, and it keeps the value it already
// had. A stale reading, presented as current, with nothing raised anywhere.
//
// So the text must be canonical: no leading zeros, and no signed zero.
func isExactInteger(text string) bool {
	digits := strings.TrimPrefix(text, "-")
	if digits == "" || len(digits) > maxExactDigits {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	// "0" is the only spelling that may start with one, and only unsigned:
	// JSON permits -0 and a browser renders it "0".
	if digits[0] == '0' && (len(digits) > 1 || len(text) != len(digits)) {
		return false
	}
	return true
}

// orderIfChanged returns the identities of after, and whether they differ from
// before's.
func orderIfChanged(before, after []any, key string) ([]string, bool) {
	order := make([]string, 0, len(after))
	for _, element := range after {
		id, _ := identity(element, key)
		order = append(order, id)
	}
	if len(before) != len(after) {
		return order, true
	}
	for i, element := range before {
		id, _ := identity(element, key)
		if id != order[i] {
			return order, true
		}
	}
	return nil, false
}

func replace(value any) (*Patch, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, &ErrEncode{Err: err}
	}
	return &Patch{Replace: json.RawMessage(raw)}, nil
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// itoa formats small non-negative indices without pulling in strconv's general
// case; array positions are the only thing it ever sees.
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// ArrayKeys returns the identity field configured for each array path. Exported
// so tests can check the table against a real snapshot.
func ArrayKeys() map[string]string {
	out := make(map[string]string, len(arrayKeys))
	for path, key := range arrayKeys {
		out[path] = key
	}
	return out
}
