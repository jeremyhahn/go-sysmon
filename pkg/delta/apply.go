package delta

import (
	"encoding/json"
	"strconv"
)

// Apply produces the tree a patch describes.
//
// It is part of the format rather than a test helper: a codec that cannot be
// read back cannot be verified, and this is what lets a round trip over real
// snapshots prove that every patch this package emits reconstructs its input
// exactly. The browser has its own implementation in src/lib/delta.ts, and both
// are held to one committed set of cases so they cannot drift.
//
// The tree is not modified: Apply builds a new one, sharing the unchanged
// parts. The previous tree therefore stays valid, which matters to a caller
// keeping it as the base for the next diff.
//
// A patch that does not fit returns ApplyError rather than a partly applied
// tree. The receiver has diverged from the sender, and no later patch can
// repair that -- the only correct answer is a whole snapshot.
func Apply(tree Tree, patch *Patch) (Tree, error) {
	return apply(tree, patch, "")
}

func apply(tree Tree, patch *Patch, path string) (Tree, error) {
	if patch == nil {
		return tree, nil
	}

	// Exactly one operation, counted rather than dispatched on.
	//
	// The type says exactly one field is set and the switch below took the
	// first one it found, so a frame carrying two -- a root replacement and an
	// object patch, say -- applied one and silently discarded the other. The
	// receiver then rendered a tree the sender never described and reported it
	// current, which is the same fail-open as applying a patch that does not
	// fit: part of the frame understood, all of it presented as understood.
	// Which of the two won also depended on the order of the cases below,
	// so two implementations could disagree while both "succeeded".
	operations := 0
	for _, present := range [...]bool{
		len(patch.Replace) > 0,
		patch.Object != nil,
		patch.Index != nil,
		patch.Keyed != nil,
	} {
		if present {
			operations++
		}
	}
	if operations > 1 {
		return nil, &ApplyError{Path: path, Reason: "the patch names more than one operation"}
	}

	switch {
	case len(patch.Replace) > 0:
		return Decode(patch.Replace)

	case patch.Object != nil:
		return applyObject(tree, patch.Object, path)

	case patch.Index != nil:
		return applyIndex(tree, patch.Index, path)

	case patch.Keyed != nil:
		return applyKeyed(tree, patch.Keyed, path)
	}
	return nil, &ApplyError{Path: path, Reason: "the patch names no operation"}
}

func applyObject(tree Tree, fields map[string]*Patch, path string) (Tree, error) {
	object, ok := tree.(map[string]any)
	if !ok {
		return nil, &ApplyError{Path: path, Reason: "an object patch met something that is not an object"}
	}
	out := make(map[string]any, len(object))
	for name, value := range object {
		out[name] = value
	}
	for name, sub := range fields {
		previous, ok := out[name]
		if !ok {
			return nil, &ApplyError{Path: join(path, name), Reason: "the patch names a field that is not there"}
		}
		patched, err := apply(previous, sub, join(path, name))
		if err != nil {
			return nil, err
		}
		out[name] = patched
	}
	return out, nil
}

func applyIndex(tree Tree, elements map[string]*Patch, path string) (Tree, error) {
	array, ok := tree.([]any)
	if !ok {
		return nil, &ApplyError{Path: path, Reason: "a positional patch met something that is not an array"}
	}
	out := make([]any, len(array))
	copy(out, array)
	for position, sub := range elements {
		// Canonical, not merely parseable. Atoi reads "0", "+0", "-0" and "00"
		// as the same index, so a patch could carry two replacements for one
		// element -- and which one won would depend on Go's map iteration
		// order here and on property order in the browser. Two receivers of
		// one frame, disagreeing, with neither able to notice. The differ
		// writes positions with strconv.Itoa, so requiring its spelling costs
		// nothing and removes the ambiguity rather than picking a winner.
		i, err := strconv.Atoi(position)
		if err != nil || strconv.Itoa(i) != position || i < 0 || i >= len(out) {
			return nil, &ApplyError{Path: path, Reason: "the patch names position " + position + ", which this array does not have"}
		}
		patched, err := apply(out[i], sub, path+"[]")
		if err != nil {
			return nil, err
		}
		out[i] = patched
	}
	return out, nil
}

func applyKeyed(tree Tree, patch *KeyedPatch, path string) (Tree, error) {
	array, ok := tree.([]any)
	if !ok {
		return nil, &ApplyError{Path: path, Reason: "a keyed patch met something that is not an array"}
	}
	if patch.Key == "" {
		return nil, &ApplyError{Path: path, Reason: "the keyed patch names no key"}
	}

	existing, ok := indexByKey(array, patch.Key)
	if !ok {
		return nil, &ApplyError{Path: path,
			Reason: "the array does not identify its elements by " + strconv.Quote(patch.Key)}
	}

	// Order, when present, is the authoritative membership: anything not named
	// in it is gone. When absent the array kept its shape and only its
	// elements moved.
	var order []string
	if patch.Order != nil {
		order = *patch.Order
	} else {
		order = make([]string, 0, len(array))
		for _, element := range array {
			id, _ := identity(element, patch.Key)
			order = append(order, id)
		}
	}

	// Every identity the patch names has to be consumed by the order, and each
	// one exactly once.
	//
	// Without this a patch can be half-interpreted and still report success:
	// Patches or New entries whose identity the order never mentions are simply
	// never looked up, the elements keep the values they already had, and the
	// frame counts as applied. That is a stale reading rendered as current --
	// the one outcome this format exists to prevent -- and it is also a
	// divergence from the browser implementation, which already refuses it. An
	// identity can only fail to match if the two sides spell it differently,
	// which the differ refuses to let happen, so reaching this means something
	// is wrong and the caller's resync is the right answer.
	used := make(map[string]bool, len(patch.New)+len(patch.Patches))
	out := make([]any, 0, len(order))
	for _, id := range order {
		if used[id] {
			return nil, &ApplyError{Path: path,
				Reason: "the patch names element " + strconv.Quote(id) + " twice"}
		}
		used[id] = true

		if raw, added := patch.New[id]; added {
			element, err := Decode(raw)
			if err != nil {
				return nil, err
			}
			// The element has to BE the one the patch says it is.
			//
			// Its position in New is a claim about its identity, and nothing
			// checked the claim against the element: {"ord":["2"],
			// "n":{"2":{"pid":999}}} placed an element whose identity is 999
			// under the key 2 and reported the frame applied. The receiver then
			// holds an array the sender does not, and every later patch names
			// elements it cannot find -- self-healing by resync, but only after
			// rendering a reading that never existed as current.
			actual, ok := identity(element, patch.Key)
			if !ok || actual != id {
				return nil, &ApplyError{Path: path,
					Reason: "the patch supplies element " + strconv.Quote(id) +
						" whose own " + strconv.Quote(patch.Key) + " says it is " +
						strconv.Quote(actual)}
			}
			// New means new. An identity already in the array is a claim the
			// differ never makes, and taking it would silently replace an
			// element that the patch also says survived.
			if _, present := existing[id]; present {
				return nil, &ApplyError{Path: path,
					Reason: "the patch supplies element " + strconv.Quote(id) +
						" as new, and the array already has it"}
			}
			out = append(out, element)
			continue
		}
		element, ok := existing[id]
		if !ok {
			return nil, &ApplyError{Path: path,
				Reason: "the patch keeps element " + strconv.Quote(id) + ", which is neither present nor supplied"}
		}
		patched, err := apply(element, patch.Patches[id], path+"[]")
		if err != nil {
			return nil, err
		}
		// A survivor must still be the element it was addressed as. A sub-patch
		// can rewrite the key field itself -- {"p":{"1":{"o":{"pid":{"r":2}}}}}
		// -- and the result is an array the sender does not have, reported as
		// applied. The differ represents an identity change as a removal plus a
		// New element, never as a survivor changing its key.
		if actual, ok := identity(patched, patch.Key); !ok || actual != id {
			return nil, &ApplyError{Path: path,
				Reason: "the patch changes element " + strconv.Quote(id) +
					" into one whose " + strconv.Quote(patch.Key) + " says it is " +
					strconv.Quote(actual)}
		}
		out = append(out, patched)
	}

	for id := range patch.New {
		if !used[id] {
			return nil, &ApplyError{Path: path,
				Reason: "the patch supplies element " + strconv.Quote(id) + ", which its order never places"}
		}
	}
	for id := range patch.Patches {
		if !used[id] {
			return nil, &ApplyError{Path: path,
				Reason: "the patch changes element " + strconv.Quote(id) + ", which this array does not contain"}
		}
		// An identity both supplied whole and patched names one element twice,
		// and only the whole one is used: the change would be dropped.
		if _, alsoNew := patch.New[id]; alsoNew {
			return nil, &ApplyError{Path: path,
				Reason: "the patch both supplies and changes element " + strconv.Quote(id)}
		}
	}
	return out, nil
}

// Encode renders a patch as the JSON that travels on the wire.
func Encode(patch *Patch) ([]byte, error) {
	raw, err := json.Marshal(patch)
	if err != nil {
		return nil, &EncodeError{Err: err}
	}
	return raw, nil
}
