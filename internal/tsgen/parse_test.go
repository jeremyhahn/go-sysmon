package tsgen

import (
	"errors"
	"strings"
	"testing"
)

// sample is a miniature of pkg/types/types.go: one root, a nested struct, a
// slice of structs, a slice of strings, a pointer, a named string type with
// constants, an omitempty field, and a time.Time. Between them they cover every
// form the real file uses.
const sample = `package types

import "time"

// Root is the top of the tree.
type Root struct {
	Timestamp time.Time  ` + "`json:\"timestamp\"`" + `
	Host      Host       ` + "`json:\"host\"`" + `
	Items     []Item     ` + "`json:\"items\"`" + `
	Tags      []string   ` + "`json:\"tags\"`" + `
	Detail    *Detail    ` + "`json:\"detail,omitempty\"`" + `
	State     State      ` + "`json:\"state\"`" + `
	Count     int        ` + "`json:\"count\"`" + `
	Enabled   bool       ` + "`json:\"enabled\"`" + `
	Ratio     float64    ` + "`json:\"ratio\"`" + `
	hidden    string
	Skipped   string     ` + "`json:\"-\"`" + `
}

// Host is a nested section.
type Host struct {
	// Name is the hostname.
	Name string ` + "`json:\"name\"`" + `
}

type Item struct {
	Label string ` + "`json:\"label\"`" + `
}

type Detail struct {
	Note string ` + "`json:\"note\"`" + `
}

// Orphan is reachable from nothing.
type Orphan struct {
	Nothing string ` + "`json:\"nothing\"`" + `
}

// State is a closed set of values.
type State string

const (
	StateUp   State = "up"
	StateDown State = "down"
)
`

func parseSample(t *testing.T) *Model {
	t.Helper()
	model, err := ParseSource("sample.go", sample, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	return model
}

func structNamed(t *testing.T, model *Model, name string) Struct {
	t.Helper()
	for _, s := range model.Structs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no struct %q in the model", name)
	return Struct{}
}

func fieldNamed(t *testing.T, s Struct, key string) Field {
	t.Helper()
	for _, f := range s.Fields {
		if f.JSONName == key {
			return f
		}
	}
	t.Fatalf("no field %q on %s", key, s.Name)
	return Field{}
}

func TestParseSource_ReachableTypesOnly(t *testing.T) {
	model := parseSample(t)

	var names []string
	for _, s := range model.Structs {
		names = append(names, s.Name)
	}
	want := "Detail Host Item Root"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("structs = %q, want %q (Orphan is reachable from nothing)", got, want)
	}

	if len(model.Enums) != 1 || model.Enums[0].Name != "State" {
		t.Fatalf("enums = %+v, want just State", model.Enums)
	}
	if got := strings.Join(model.Enums[0].Values, ","); got != "up,down" {
		t.Errorf("State values = %q, want %q in declaration order", got, "up,down")
	}
}

func TestParseSource_FieldShapes(t *testing.T) {
	root := structNamed(t, parseSample(t), "Root")

	cases := []struct {
		key      string
		kind     Kind
		ref      string
		nullable bool
		optional bool
		elem     Kind
	}{
		{key: "timestamp", kind: KindString},
		{key: "host", kind: KindObject, ref: "Host"},
		// A nil Go slice marshals to null, so every slice is nullable.
		{key: "items", kind: KindArray, nullable: true, elem: KindObject},
		{key: "tags", kind: KindArray, nullable: true, elem: KindString},
		{key: "detail", kind: KindObject, ref: "Detail", nullable: true, optional: true},
		{key: "state", kind: KindString, ref: "State"},
		{key: "count", kind: KindNumber},
		{key: "enabled", kind: KindBoolean},
		{key: "ratio", kind: KindNumber},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			f := fieldNamed(t, root, c.key)
			if f.Type.Kind != c.kind {
				t.Errorf("kind = %q, want %q", f.Type.Kind, c.kind)
			}
			if f.Type.Ref != c.ref {
				t.Errorf("ref = %q, want %q", f.Type.Ref, c.ref)
			}
			if f.Type.Nullable != c.nullable {
				t.Errorf("nullable = %v, want %v", f.Type.Nullable, c.nullable)
			}
			if f.Optional != c.optional {
				t.Errorf("optional = %v, want %v", f.Optional, c.optional)
			}
			if c.elem != "" && (f.Type.Elem == nil || f.Type.Elem.Kind != c.elem) {
				t.Errorf("elem = %+v, want kind %q", f.Type.Elem, c.elem)
			}
		})
	}
}

// TestParseSource_FieldsOffTheWireAreDropped covers the two ways a Go field is
// not part of the payload. Emitting either would put a key in the schema that
// no frame can ever carry, and the validator would then be checking a field the
// server does not have.
func TestParseSource_FieldsOffTheWireAreDropped(t *testing.T) {
	root := structNamed(t, parseSample(t), "Root")
	for _, f := range root.Fields {
		if f.GoName == "hidden" {
			t.Error("unexported field reached the model")
		}
		if f.GoName == "Skipped" {
			t.Error(`json:"-" field reached the model`)
		}
	}
}

func TestParseSource_DocComments(t *testing.T) {
	model := parseSample(t)

	root := structNamed(t, model, "Root")
	if root.Doc != "Root is the top of the tree." {
		t.Errorf("Root doc = %q, want the Go doc comment", root.Doc)
	}
	host := structNamed(t, model, "Host")
	if got := fieldNamed(t, host, "name").Doc; got != "Name is the hostname." {
		t.Errorf("Host.name doc = %q, want the Go field comment", got)
	}
}

func TestParseSource_UnparseableSource(t *testing.T) {
	_, err := ParseSource("broken.go", "package types\ntype Root struct {", []string{"Root"})
	if err == nil {
		t.Fatal("ParseSource on a truncated file returned no error")
	}
	var srcErr *SourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("error = %T (%v), want *SourceError", err, err)
	}
	if srcErr.Path != "broken.go" {
		t.Errorf("Path = %q, want the file that failed", srcErr.Path)
	}
}

func TestParseSource_UnknownRoot(t *testing.T) {
	_, err := ParseSource("sample.go", sample, []string{"Nope"})
	if err == nil {
		t.Fatal("ParseSource with a root that is not declared returned no error")
	}
	var rootErr *UnknownRootError
	if !errors.As(err, &rootErr) {
		t.Fatalf("error = %T (%v), want *UnknownRootError", err, err)
	}
	if rootErr.Name != "Nope" {
		t.Errorf("Name = %q, want the root that was missing", rootErr.Name)
	}
}

// TestParseSource_UnsupportedType is the guard that keeps a new Go shape from
// being silently dropped. A skipped field is a field missing from both the
// interface and the schema, which is exactly the drift this generator removes.
func TestParseSource_UnsupportedType(t *testing.T) {
	const src = `package types

type Root struct {
	Labels map[string]string ` + "`json:\"labels\"`" + `
}
`
	_, err := ParseSource("map.go", src, []string{"Root"})
	if err == nil {
		t.Fatal("a map field was accepted; it should stop the build")
	}
	var unsupported *UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T (%v), want *UnsupportedTypeError", err, err)
	}
	if unsupported.Field != "Labels" || unsupported.GoType != "map[string]string" {
		t.Errorf("error = %+v, want it to name the field and its Go type", unsupported)
	}
}

// TestParseSource_UntaggedFieldKeepsItsGoName covers encoding/json's own
// default. Emitting nothing for it would leave a key the server really does
// send out of the schema.
func TestParseSource_UntaggedFieldKeepsItsGoName(t *testing.T) {
	const src = `package types

type Root struct {
	Plain string
}
`
	model, err := ParseSource("plain.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if got := fieldNamed(t, structNamed(t, model, "Root"), "Plain"); got.GoName != "Plain" {
		t.Errorf("field = %+v, want the Go name used as the wire key", got)
	}
}

// TestParse_MultiNameFieldEmitsEvery covers `A, B string`, which encoding/json
// sends as two keys. The parser used to skip the whole declaration, so both
// were absent from the interfaces and the schema while the server sent them.
func TestParse_MultiNameFieldEmitsEvery(t *testing.T) {
	const src = `package types

type Root struct {
	Alpha, Beta string
	Gamma       int ` + "`json:\"gamma\"`" + `
}
`
	model, err := ParseSource("multi.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource() = %v, want nil", err)
	}
	var got []string
	for _, f := range model.Structs[0].Fields {
		got = append(got, f.JSONName)
	}
	want := []string{"Alpha", "Beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParse_MultiNameSharingAJSONNameIsRefused: one name tag cannot serve two
// fields without putting the same key on the wire twice, so it is refused
// rather than guessed at.
func TestParse_MultiNameSharingAJSONNameIsRefused(t *testing.T) {
	const src = `package types

type Root struct {
	Alpha, Beta string ` + "`json:\"shared\"`" + `
}
`
	_, err := ParseSource("shared.go", src, []string{"Root"})
	var decl *UnsupportedDeclError
	if !errors.As(err, &decl) {
		t.Fatalf("ParseSource() = %v, want *UnsupportedDeclError", err)
	}
	if !strings.Contains(decl.Error(), "twice") {
		t.Errorf("Error() = %q, want it to say the key would be sent twice", decl.Error())
	}
}

// embedSrc wraps struct declarations in a package with Root as the root.
func embedSrc(decls string) string {
	return "package types\n\n" + decls
}

func fieldKeys(s Struct) []string {
	keys := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		keys = append(keys, f.JSONName)
	}
	return keys
}

// TestParse_EmbeddedStructIsPromoted covers encoding/json's promotion: an
// untagged embedded struct's fields appear on the outer object, in the place
// the embedding is declared.
func TestParse_EmbeddedStructIsPromoted(t *testing.T) {
	src := embedSrc(`type Inner struct {
	Kept string ` + "`json:\"kept\"`" + `
	Deeper
}

type Deeper struct {
	Deep int ` + "`json:\"deep\"`" + `
}

type Root struct {
	First string ` + "`json:\"first\"`" + `
	Inner
	Last string ` + "`json:\"last\"`" + `
}
`)
	model, err := ParseSource("embed.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	root := structNamed(t, model, "Root")
	if got := strings.Join(fieldKeys(root), ","); got != "first,kept,deep,last" {
		t.Errorf("Root keys = %q, want first,kept,deep,last", got)
	}
	if f := fieldNamed(t, root, "kept"); f.Optional {
		t.Error("a field promoted through a value embedding is not optional")
	}
}

// TestParse_PointerEmbeddingMakesPromotedFieldsOptional covers a nil embedded
// pointer, which encoding/json skips entirely: every field it would promote
// may be absent.
func TestParse_PointerEmbeddingMakesPromotedFieldsOptional(t *testing.T) {
	src := embedSrc(`type Inner struct {
	Kept string ` + "`json:\"kept\"`" + `
}

type Root struct {
	*Inner
}
`)
	model, err := ParseSource("embed.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if f := fieldNamed(t, structNamed(t, model, "Root"), "kept"); !f.Optional {
		t.Error("a field promoted through a pointer embedding must be optional")
	}
}

// TestParse_TaggedEmbeddingIsANamedField covers an embedded struct with a json
// name, which encoding/json treats as an ordinary field of that name.
func TestParse_TaggedEmbeddingIsANamedField(t *testing.T) {
	src := embedSrc(`type Inner struct {
	Kept string ` + "`json:\"kept\"`" + `
}

type Root struct {
	Inner ` + "`json:\"inner\"`" + `
}
`)
	model, err := ParseSource("embed.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	f := fieldNamed(t, structNamed(t, model, "Root"), "inner")
	if f.Type.Kind != KindObject || f.Type.Ref != "Inner" {
		t.Errorf("inner = %+v, want an object of type Inner", f.Type)
	}
}

// TestParse_EmbeddingDominanceFollowsEncodingJSON covers name clashes: the
// shallower field wins, and at equal depth a tagged field beats an untagged
// one.
func TestParse_EmbeddingDominanceFollowsEncodingJSON(t *testing.T) {
	src := embedSrc(`type Inner struct {
	Name  string ` + "`json:\"name\"`" + `
	Label string
	Other string ` + "`json:\"other\"`" + `
}

type Side struct {
	Label string ` + "`json:\"Label\"`" + `
}

type Root struct {
	Name string ` + "`json:\"name\"`" + `
	Inner
	Side
}
`)
	model, err := ParseSource("embed.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	root := structNamed(t, model, "Root")
	if got := strings.Join(fieldKeys(root), ","); got != "name,Label,other" {
		t.Errorf("Root keys = %q, want name,Label,other", got)
	}
	if f := fieldNamed(t, root, "Label"); f.GoName != "Label" || fieldNamed(t, root, "name").GoName != "Name" {
		t.Errorf("dominance picked the wrong declarations: %+v", root.Fields)
	}
}

// TestParse_AmbiguousEmbeddedFieldIsRefused covers the case encoding/json
// resolves by silently dropping the key: two fields of one name at the same
// depth, neither (or both) tagged. The generator refuses it so the author
// resolves it, instead of a field vanishing from the wire.
func TestParse_AmbiguousEmbeddedFieldIsRefused(t *testing.T) {
	src := embedSrc(`type A struct {
	Dup string ` + "`json:\"dup\"`" + `
}

type B struct {
	Dup string ` + "`json:\"dup\"`" + `
}

type Root struct {
	A
	B
}
`)
	_, err := ParseSource("embed.go", src, []string{"Root"})
	var decl *UnsupportedDeclError
	if !errors.As(err, &decl) {
		t.Fatalf("error = %T (%v), want *UnsupportedDeclError", err, err)
	}
	if decl.Struct != "Root" || decl.Field != "dup" {
		t.Errorf("error = %+v, want it to name Root and the key dup", decl)
	}
}

// TestParse_UnsupportedEmbeddingsAreRefused covers embeddings whose wire
// form the generator does not model: a non-struct named type, a qualified
// type (time.Time would promote its MarshalJSON and replace the whole
// object), and a cycle.
func TestParse_UnsupportedEmbeddingsAreRefused(t *testing.T) {
	cases := map[string]string{
		"non-struct": "type Name string\n\ntype Root struct {\n\tName\n}\n",
		"qualified":  "type Root struct {\n\ttime.Time\n}\n",
		"cycle":      "type Root struct {\n\t*Loop\n}\n\ntype Loop struct {\n\t*Loop\n}\n",
	}
	for name, decls := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSource("embed.go", embedSrc(decls), []string{"Root"})
			var decl *UnsupportedDeclError
			if !errors.As(err, &decl) {
				t.Fatalf("error = %T (%v), want *UnsupportedDeclError", err, err)
			}
		})
	}
}

// enumValues parses src with Root as the only root and returns the values of
// the enum State, failing the test if parsing fails.
func enumValues(t *testing.T, src string) []string {
	t.Helper()
	model, err := ParseSource("enum.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	for _, e := range model.Enums {
		if e.Name == "State" {
			return e.Values
		}
	}
	t.Fatalf("no State enum in %+v", model.Enums)
	return nil
}

const enumRoot = `package types

type State string

type Root struct {
	S State ` + "`json:\"s\"`" + `
}
`

// TestParse_EnumConstantsAreEvaluated covers every constant form Go accepts
// for a named string type. A value read by pattern-matching the source text
// would be omitted or misspelled here, and the generated union would then
// reject a value the server can legally send.
func TestParse_EnumConstantsAreEvaluated(t *testing.T) {
	src := enumRoot + `
const prefix = "up"

const (
	StateUp      State = "up"
	StateSoon    State = prefix + "_soon"
	StateRaw     State = ` + "`raw\\value`" + `
	StateEscaped State = "tab\there"
	StateParen   State = ("pa" + "ren")
	StateAlias   State = StateUp + "!"
)

const StateConv = State("converted")

const StateViaString State = State(string("via"))
`
	want := []string{"up", "up_soon", `raw\value`, "tab\there", "paren", "up!", "converted", "via"}
	if got := enumValues(t, src); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("State values = %q, want %q", got, want)
	}
}

// TestParse_EnumImplicitRepetitionIsOneValue covers a const group that omits
// the expression, which Go reads as repeating the previous one: the same
// value, which the union must carry once.
func TestParse_EnumImplicitRepetitionIsOneValue(t *testing.T) {
	src := enumRoot + `
const (
	StateA State = "a"
	StateB
)
`
	if got := enumValues(t, src); strings.Join(got, "|") != "a" {
		t.Errorf("State values = %q, want just %q", got, "a")
	}
}

// TestParse_UnresolvableEnumConstantIsRefused keeps the generator failing
// closed: a value it cannot compute is a value it would leave out of the
// union, so it stops the build instead.
func TestParse_UnresolvableEnumConstantIsRefused(t *testing.T) {
	cases := map[string]string{
		"function call":     `const StateX State = State(strings.ToLower("X"))`,
		"undeclared ident":  `const StateX State = missing + "x"`,
		"forward reference": "const StateX State = later\nconst later = \"l\"",
		"non-string":        `const StateX State = 1 + 2`,
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSource("enum.go", enumRoot+"\n"+decl+"\n", []string{"Root"})
			if err == nil {
				t.Fatal("an unresolvable enum constant was accepted; it should stop the build")
			}
			var constErr *UnsupportedConstError
			if !errors.As(err, &constErr) {
				t.Fatalf("error = %T (%v), want *UnsupportedConstError", err, err)
			}
			if constErr.Enum != "State" || constErr.Const != "StateX" {
				t.Errorf("error = %+v, want it to name State and StateX", constErr)
			}
		})
	}
}

// TestParse_UnrelatedConstantsAreIgnored checks that the evaluator only fails
// closed for constants on the wire: a constant of another type that it cannot
// compute is none of the generator's business.
func TestParse_UnrelatedConstantsAreIgnored(t *testing.T) {
	src := enumRoot + `
const bufSize = 1 << 10
const timeout = time.Second
const StateOK State = "ok"
`
	if got := enumValues(t, src); strings.Join(got, "|") != "ok" {
		t.Errorf("State values = %q, want %q", got, "ok")
	}
}

// TestParse_EnumTypeIsInferredThroughExpressions covers constants that omit
// their type but inherit it from a typed operand, as Go's constant rules
// give them. They are enum values on the wire all the same.
func TestParse_EnumTypeIsInferredThroughExpressions(t *testing.T) {
	src := enumRoot + `
const StateA State = "a"

const (
	StateAB    = StateA + "b"
	StateBA    = "b" + StateA
	StateParen = (StateA)
	StateChain = StateAB + "c"
	untyped    = "u"
)
`
	want := []string{"a", "ab", "ba", "abc"}
	if got := enumValues(t, src); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("State values = %q, want %q", got, want)
	}
}

// TestParse_UnionOverrideMustTargetAPlainString keeps a hand-written override
// from silently outliving the Go field it narrows: if the field stops being a
// plain string, the override would re-type it in the interface while the
// schema followed the new Go type.
func TestParse_UnionOverrideMustTargetAPlainString(t *testing.T) {
	cases := map[string]string{
		"boolean":           "bool",
		"named string type": "State",
		"array of strings":  "[]string",
	}
	for name, goType := range cases {
		t.Run(name, func(t *testing.T) {
			src := `package types

type State string

type Alert struct {
	Severity ` + goType + " `json:\"severity\"`" + `
}
`
			_, err := ParseSource("override.go", src, []string{"Alert"})
			if err == nil {
				t.Fatalf("an override on a %s field was accepted; it should stop the build", goType)
			}
			var decl *UnsupportedDeclError
			if !errors.As(err, &decl) {
				t.Fatalf("error = %T (%v), want *UnsupportedDeclError", err, err)
			}
			if decl.Struct != "Alert" || decl.Field != "Severity" {
				t.Errorf("error = %+v, want it to name Alert.Severity", decl)
			}
		})
	}
}

func TestParse_UnionOverrideOnAPlainStringApplies(t *testing.T) {
	const src = `package types

type Alert struct {
	Severity string ` + "`json:\"severity\"`" + `
}
`
	model, err := ParseSource("override.go", src, []string{"Alert"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if got := fieldNamed(t, structNamed(t, model, "Alert"), "severity").TSType; got != "Severity" {
		t.Errorf("TSType = %q, want the Severity union", got)
	}
}

// TestParse_InterpretedStructTagIsRead covers a tag written as an ordinary
// double-quoted string, which Go accepts and encoding/json reads exactly like
// a backquoted one. Reading it as raw text would emit the Go name while the
// server sends the tagged one.
func TestParse_InterpretedStructTagIsRead(t *testing.T) {
	const src = `package types

type Root struct {
	Field   string "json:\"wire\""
	Maybe   string "json:\"maybe,omitempty\""
	Skipped string "json:\"-\""
}
`
	model, err := ParseSource("tags.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	root := structNamed(t, model, "Root")
	if len(root.Fields) != 2 {
		t.Fatalf("fields = %+v, want wire and maybe only", root.Fields)
	}
	if f := fieldNamed(t, root, "wire"); f.GoName != "Field" {
		t.Errorf("wire maps to %q, want Field", f.GoName)
	}
	if f := fieldNamed(t, root, "maybe"); !f.Optional {
		t.Error("maybe lost its omitempty")
	}
}

// TestParse_StringOptionIsRefused covers json:",string", which makes
// encoding/json send a number or boolean as a quoted string. The generator
// would otherwise emit a number the schema then refuses on every frame.
func TestParse_StringOptionIsRefused(t *testing.T) {
	const src = `package types

type Root struct {
	Count int ` + "`json:\"count,string\"`" + `
}
`
	_, err := ParseSource("opt.go", src, []string{"Root"})
	var decl *UnsupportedDeclError
	if !errors.As(err, &decl) {
		t.Fatalf("error = %T (%v), want *UnsupportedDeclError", err, err)
	}
	if decl.Struct != "Root" || decl.Field != "Count" {
		t.Errorf("error = %+v, want it to name Root.Count", decl)
	}
}

// TestParse_OmitzeroIsOptional covers the omitzero option, which omits a
// field exactly as omitempty can.
func TestParse_OmitzeroIsOptional(t *testing.T) {
	const src = `package types

type Root struct {
	When string ` + "`json:\"when,omitzero\"`" + `
}
`
	model, err := ParseSource("opt.go", src, []string{"Root"})
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if f := fieldNamed(t, structNamed(t, model, "Root"), "when"); !f.Optional {
		t.Error("an omitzero field was not marked optional")
	}
}

// TestParse_ByteSliceIsRefused covers []byte, which encoding/json sends as a
// base64 string rather than an array of numbers. Modelling it as an array
// would make the schema refuse every frame carrying the field.
func TestParse_ByteSliceIsRefused(t *testing.T) {
	for _, goType := range []string{"[]byte", "[]uint8"} {
		t.Run(goType, func(t *testing.T) {
			src := `package types

type Root struct {
	Blob ` + goType + " `json:\"blob\"`" + `
}
`
			_, err := ParseSource("bytes.go", src, []string{"Root"})
			var unsupported *UnsupportedTypeError
			if !errors.As(err, &unsupported) {
				t.Fatalf("error = %T (%v), want *UnsupportedTypeError", err, err)
			}
			if unsupported.Field != "Blob" {
				t.Errorf("error = %+v, want it to name Blob", unsupported)
			}
		})
	}
}

// TestParse_UnionOverrideRefusesANullableString covers an overridden field
// that becomes *string: the schema then accepts null, which the union alias
// alone would hide.
func TestParse_UnionOverrideRefusesANullableString(t *testing.T) {
	const src = `package types

type Alert struct {
	Severity *string ` + "`json:\"severity\"`" + `
}
`
	_, err := ParseSource("override.go", src, []string{"Alert"})
	var decl *UnsupportedDeclError
	if !errors.As(err, &decl) {
		t.Fatalf("error = %T (%v), want *UnsupportedDeclError", err, err)
	}
}
