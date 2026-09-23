// Package typestest checks encoded go-sysmon payloads against the wire
// contract in pkg/types. It exists for tests, in the way net/http/httptest
// does: several packages emit these payloads, and each needs to prove the same
// property of what it emits.
package typestest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// DecodeError reports that a payload handed to NullSlices is not JSON.
type DecodeError struct {
	Cause error
}

func (e *DecodeError) Error() string {
	return "typestest: payload is not json: " + e.Cause.Error()
}

func (e *DecodeError) Unwrap() error {
	return e.Cause
}

// NullSlices returns the path of every field in raw that is a slice in the Go
// type of like and is encoded as JSON null. like is only consulted for its
// type; pass a zero value or a nil pointer of the payload type.
//
// The check is driven by the Go type rather than by a list of field names, so
// a list field added to pkg/types is covered the day it is added. It is also
// driven by the ENCODED bytes rather than by the Go value, because the bytes
// are the contract: they are what the dashboard receives.
//
// Paths are JSON names joined with ".", with an array position in brackets --
// "npus[0].engines". They are sorted, so a failure reads the same every run.
func NullSlices(raw []byte, like any) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, &DecodeError{Cause: err}
	}
	var found []string
	walk(tree, reflect.TypeOf(like), "", &found)
	sort.Strings(found)
	return found, nil
}

// walk visits node, which was encoded from a value of type t.
func walk(node any, t reflect.Type, path string, found *[]string) {
	if t == nil {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		if node == nil {
			if t.Kind() == reflect.Slice {
				*found = append(*found, displayPath(path))
			}
			return
		}
		elements, ok := node.([]any)
		if !ok {
			return
		}
		for i, element := range elements {
			walk(element, t.Elem(), path+"["+strconv.Itoa(i)+"]", found)
		}
	case reflect.Struct:
		object, ok := node.(map[string]any)
		if !ok {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if promoted(field) {
				// encoding/json writes an untagged embedded struct's fields
				// into the enclosing object, so they are walked there.
				walk(node, field.Type, path, found)
				continue
			}
			name, ok := jsonName(field)
			if !ok {
				continue
			}
			value, present := object[name]
			if !present {
				continue
			}
			walk(value, field.Type, joinPath(path, name), found)
		}
	}
}

// promoted reports whether encoding/json flattens field into its parent: an
// exported, untagged, embedded struct (or pointer to one).
func promoted(field reflect.StructField) bool {
	if !field.Anonymous || !field.IsExported() || field.Tag.Get("json") != "" {
		return false
	}
	t := field.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// jsonName is the key encoding/json writes for field, and false for a field it
// does not write at all.
func jsonName(field reflect.StructField) (string, bool) {
	if !field.IsExported() {
		return "", false
	}
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name, true
	}
	return field.Name, true
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func displayPath(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}
