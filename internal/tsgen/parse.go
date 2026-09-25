// Package tsgen turns the Go wire types into the TypeScript the dashboard
// reads them with: the interfaces, and a runtime schema those interfaces are
// checked against as frames arrive.
//
// Both were hand-mirrored from pkg/types/types.go, and a hand mirror drifts
// silently. It already had: NetworkInfo carried `index` and `wireless` on the
// Go side and neither appeared in the TypeScript, so the dashboard could not
// see fields the server had been sending it (#38). Generating both from the
// one source makes that class of divergence impossible rather than unlikely.
//
// The parse is syntactic, over a single file, with no type checker and no
// module dependencies: every wire type lives in pkg/types/types.go, so the
// declarations needed to resolve a field are all in front of the parser. That
// keeps the generator something `go run` can do from a bare checkout, which
// matters because it runs in the same builds that must work without npm.
package tsgen

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Kind is the TypeScript shape of a field, as far as validating it goes.
type Kind string

const (
	// KindString covers Go strings, named string types, and time.Time, which
	// marshals to an RFC3339 string.
	KindString Kind = "string"
	// KindNumber covers every Go integer and float. JSON has one number type.
	KindNumber Kind = "number"
	// KindBoolean covers bool.
	KindBoolean Kind = "boolean"
	// KindObject is a nested struct, named by Ref.
	KindObject Kind = "object"
	// KindArray is a slice, whose element shape is Elem.
	KindArray Kind = "array"
)

// Type is the shape of one field.
//
// Nullable is a property of the Go declaration, not a policy: a nil slice and
// a nil pointer both marshal to JSON null, so a machine with no GPUs sends
// `"gpus": null` and a disk that is not an array sends `"raid": null`. The
// validator has to know that to tell "this machine has none" from "this frame
// is malformed".
type Type struct {
	Kind     Kind
	Ref      string // set when Kind is KindObject
	Elem     *Type  // set when Kind is KindArray
	Nullable bool
}

// Field is one member of a struct, as it appears on the wire.
type Field struct {
	// JSONName is the key the server actually sends, from the json tag.
	JSONName string
	// GoName is the Go field name, used for error messages that have to be
	// traced back to the source.
	GoName string
	Doc    string
	Type   Type
	// Optional marks a field the server may omit entirely: json:",omitempty"
	// on a value that can be empty.
	Optional bool
}

// Struct is one Go struct, in declaration order.
type Struct struct {
	Name   string
	Doc    string
	Fields []Field
}

// Enum is a named Go string type together with the values declared for it, so
// the TypeScript can be a union rather than a bare string.
type Enum struct {
	Name   string
	Doc    string
	Values []string
}

// Model is everything the renderers need, reachable from the roots and nothing
// else.
type Model struct {
	Structs []Struct
	Enums   []Enum
}

// Parse reads path and returns the types reachable from roots.
//
// Reachability rather than "everything in the file" is deliberate: the emitted
// schema is what the dashboard validates against, and a type nothing on the
// wire refers to has no business being in it.
func Parse(path string, roots []string) (*Model, error) {
	return ParseSource(path, nil, roots)
}

// ParseSource is Parse with the source supplied directly, in any form
// go/parser accepts. A nil src reads path from disk.
//
// It exists so the generator can be tested against a handful of declarations
// rather than against the 1100-line file it actually runs on: a test that has
// to read pkg/types to know what to expect proves the parser agrees with
// itself and nothing else.
func ParseSource(path string, src any, roots []string) (*Model, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, &SourceError{Path: path, Cause: err}
	}

	structs, enums, docs, err := declarations(file)
	if err != nil {
		return nil, err
	}

	for _, root := range roots {
		if _, ok := structs[root]; !ok {
			return nil, &UnknownRootError{Name: root}
		}
	}

	model := &Model{}
	seen := map[string]bool{}
	usedEnums := map[string]bool{}

	// Breadth-first from the roots, so a type is emitted once however many
	// fields reach it.
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true

		decl, ok := structs[name]
		if !ok {
			continue
		}
		converted, refs, err := convertStruct(name, decl, structs, enums)
		if err != nil {
			return nil, err
		}
		converted.Doc = docs[name]
		model.Structs = append(model.Structs, converted)
		for _, ref := range refs {
			if enums[ref] != nil {
				usedEnums[ref] = true
				continue
			}
			queue = append(queue, ref)
		}
	}

	sort.Slice(model.Structs, func(i, j int) bool {
		return model.Structs[i].Name < model.Structs[j].Name
	})

	for name := range usedEnums {
		model.Enums = append(model.Enums, *enums[name])
	}
	sort.Slice(model.Enums, func(i, j int) bool {
		return model.Enums[i].Name < model.Enums[j].Name
	})

	return model, nil
}

// declarations indexes the struct and named-string-type declarations in a
// file, together with the doc comment on each.
func declarations(file *ast.File) (map[string]*ast.StructType, map[string]*Enum, map[string]string, error) {
	structs := map[string]*ast.StructType{}
	enums := map[string]*Enum{}
	docs := map[string]string{}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			// A one-spec declaration carries its doc on the GenDecl; a grouped
			// one carries it on the spec.
			doc := docText(ts.Doc)
			if doc == "" && len(gen.Specs) == 1 {
				doc = docText(gen.Doc)
			}
			docs[ts.Name.Name] = doc

			switch t := ts.Type.(type) {
			case *ast.StructType:
				structs[ts.Name.Name] = t
			case *ast.Ident:
				if t.Name == "string" {
					enums[ts.Name.Name] = &Enum{Name: ts.Name.Name, Doc: doc}
				}
			}
		}
	}

	if err := collectEnumValues(file, enums); err != nil {
		return nil, nil, nil, err
	}
	return structs, enums, docs, nil
}

// collectEnumValues fills in the declared values of every named string type,
// in declaration order, from the constants of that type.
//
// Values are computed with go/constant rather than read off the source text,
// so every form Go accepts spells the value encoding/json will send: raw and
// escaped literals, concatenation, parentheses, references to earlier
// constants, conversions, and a const group's implicit repetition. A
// constant's type follows Go's rules too: an explicit type wins, and an
// untyped declaration takes the type of its typed operand, so
// `StateAB = StateA + "b"` is a State. A constant of an enum type that cannot
// be computed stops the build; a constant of any other type is not on the
// wire and is ignored when it cannot be.
func collectEnumValues(file *ast.File, enums map[string]*Enum) error {
	known := map[string]typedConst{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Go repeats the previous spec's type and expressions for a spec that
		// omits them, within one const group.
		var prevType ast.Expr
		var prevValues []ast.Expr
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typ, values := vs.Type, vs.Values
			if len(values) == 0 {
				typ, values = prevType, prevValues
			}
			prevType, prevValues = typ, values

			for i, name := range vs.Names {
				if i >= len(values) {
					break
				}
				c, reason := evalString(values[i], known, enums)
				enum := constEnum(typ, values[i], c, reason == "", known, enums)
				if reason != "" {
					if enum != nil {
						return &UnsupportedConstError{Enum: enum.Name, Const: name.Name, Reason: reason}
					}
					continue
				}
				c.enum = ""
				if enum != nil {
					c.enum = enum.Name
					enum.add(constant.StringVal(c.value))
				}
				known[name.Name] = c
			}
		}
	}
	return nil
}

// typedConst is a computed string constant and the enum type it carries, if
// any.
type typedConst struct {
	value constant.Value
	enum  string
}

// constEnum returns the enum a constant belongs to. An explicit type decides
// it; otherwise the type inferred while evaluating the value does. When the
// value could not be evaluated, an untyped declaration is still treated as
// the enum's if its expression names that enum or one of its constants, so
// the failure stops the build rather than dropping a value.
func constEnum(typ, value ast.Expr, c typedConst, evaluated bool, known map[string]typedConst, enums map[string]*Enum) *Enum {
	if typ != nil {
		if ident, ok := typ.(*ast.Ident); ok {
			return enums[ident.Name]
		}
		return nil
	}
	if evaluated {
		return enums[c.enum]
	}
	var found *Enum
	ast.Inspect(value, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || found != nil {
			return found == nil
		}
		if e := enums[ident.Name]; e != nil {
			found = e
		} else if k, ok := known[ident.Name]; ok && k.enum != "" {
			found = enums[k.enum]
		}
		return found == nil
	})
	return found
}

// evalString computes a constant string expression and the enum type Go
// gives it, returning a reason in place of a value when the expression is not
// one it can compute.
func evalString(expr ast.Expr, known map[string]typedConst, enums map[string]*Enum) (typedConst, string) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return typedConst{}, "value is not a string"
		}
		v := constant.MakeFromLiteral(e.Value, e.Kind, 0)
		if v.Kind() != constant.String {
			return typedConst{}, "malformed string literal " + e.Value
		}
		return typedConst{value: v}, ""
	case *ast.ParenExpr:
		return evalString(e.X, known, enums)
	case *ast.Ident:
		c, ok := known[e.Name]
		if !ok {
			return typedConst{}, e.Name + " is not a string constant declared earlier in the file"
		}
		return c, ""
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return typedConst{}, "operator " + e.Op.String() + " is not string concatenation"
		}
		x, reason := evalString(e.X, known, enums)
		if reason != "" {
			return typedConst{}, reason
		}
		y, reason := evalString(e.Y, known, enums)
		if reason != "" {
			return typedConst{}, reason
		}
		// A typed operand types the result; Go rejects two different ones.
		enum := x.enum
		if enum == "" {
			enum = y.enum
		} else if y.enum != "" && y.enum != enum {
			return typedConst{}, "concatenates " + x.enum + " and " + y.enum
		}
		return typedConst{value: constant.BinaryOp(x.value, token.ADD, y.value), enum: enum}, ""
	case *ast.CallExpr:
		ident, ok := e.Fun.(*ast.Ident)
		if !ok || len(e.Args) != 1 || (ident.Name != "string" && enums[ident.Name] == nil) {
			return typedConst{}, "call " + types.ExprString(e.Fun) + " is not a conversion to a string type"
		}
		c, reason := evalString(e.Args[0], known, enums)
		if reason != "" {
			return typedConst{}, reason
		}
		c.enum = ident.Name
		if ident.Name == "string" {
			c.enum = ""
		}
		return c, ""
	}
	return typedConst{}, "expression " + types.ExprString(expr) + " is not a constant the generator can compute"
}

// add appends a value unless the enum already has it: implicit repetition in
// a const group declares the same value again, and a union carries it once.
func (e *Enum) add(value string) {
	for _, v := range e.Values {
		if v == value {
			return
		}
	}
	e.Values = append(e.Values, value)
}

// convertStruct turns one parsed struct into the model's form, returning the
// named types its fields refer to.
//
// Embedded structs are flattened the way encoding/json flattens them, so the
// emitted interface lists exactly the keys the server sends; see
// collectFields and resolveFields.
func convertStruct(
	name string,
	decl *ast.StructType,
	structs map[string]*ast.StructType,
	enums map[string]*Enum,
) (Struct, []string, error) {
	candidates, err := collectFields(name, decl, 0, false, map[string]bool{name: true}, structs, enums)
	if err != nil {
		return Struct{}, nil, err
	}
	fields, err := resolveFields(name, candidates)
	if err != nil {
		return Struct{}, nil, err
	}

	out := Struct{Name: name, Fields: fields}
	var refs []string
	for _, f := range fields {
		if ref := namedRef(f.Type); ref != "" {
			refs = append(refs, ref)
		}
	}
	return out, refs, nil
}

// candidate is a field that may reach the wire, with what encoding/json needs
// to decide between two fields of the same key: how deeply it is embedded and
// whether its key came from a tag.
type candidate struct {
	field  Field
	depth  int
	tagged bool
}

// collectFields gathers every field decl contributes to the wire, in
// declaration order, descending into untagged embedded structs.
//
// root names the struct being converted, for errors. optional marks fields
// reached through an embedded pointer, which encoding/json omits entirely when
// the pointer is nil.
func collectFields(
	root string,
	decl *ast.StructType,
	depth int,
	optional bool,
	visiting map[string]bool,
	structs map[string]*ast.StructType,
	enums map[string]*Enum,
) ([]candidate, error) {
	var out []candidate
	for _, field := range decl.Fields.List {
		if len(field.Names) == 0 {
			embedded, err := collectEmbedded(root, field, depth, optional, visiting, structs, enums)
			if err != nil {
				return nil, err
			}
			out = append(out, embedded...)
			continue
		}

		// `A, B string` declares two wire fields, and both are emitted. A json
		// NAME tag cannot be shared between them -- encoding/json would send
		// the same key twice -- so that combination is refused rather than
		// guessed at. Without one, each name carries its own.
		if len(field.Names) > 1 && tagNamesField(field.Tag) {
			return nil, &UnsupportedDeclError{
				Struct: root,
				Field:  field.Names[0].Name,
				Reason: "several names share one json name tag, which would put the same key on the wire twice",
			}
		}

		for _, ident := range field.Names {
			if !ast.IsExported(ident.Name) {
				continue
			}
			c, keep, err := namedCandidate(root, field, ident.Name, depth, optional, structs, enums)
			if err != nil {
				return nil, err
			}
			if keep {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// namedCandidate converts one named field (or a tagged embedding, which
// encoding/json treats as a named field) into a candidate. keep is false for a
// field tagged "-".
func namedCandidate(
	root string,
	field *ast.Field,
	goName string,
	depth int,
	viaPointer bool,
	structs map[string]*ast.StructType,
	enums map[string]*Enum,
) (candidate, bool, error) {
	jsonName, optional, skip, quoted := jsonTag(field.Tag, goName)
	if skip {
		return candidate{}, false, nil
	}
	if quoted {
		return candidate{}, false, &UnsupportedDeclError{
			Struct: root,
			Field:  goName,
			Reason: `json ",string" sends the value as a quoted string, which the generator does not model`,
		}
	}

	converted, err := convertType(field.Type, root, goName, structs, enums)
	if err != nil {
		return candidate{}, false, err
	}

	return candidate{
		field: Field{
			JSONName: jsonName,
			GoName:   goName,
			Doc:      docText(field.Doc),
			Type:     converted,
			Optional: optional || viaPointer,
		},
		depth:  depth,
		tagged: tagNamesField(field.Tag),
	}, true, nil
}

// collectEmbedded handles an embedded field the way encoding/json does:
//
//   - tagged with a name, it is an ordinary field of that name;
//   - tagged "-", it is not on the wire;
//   - untagged, the fields of the embedded struct are promoted one level down,
//     and through a pointer every promoted field may be absent.
//
// Anything whose wire form the generator does not model stops the build: a
// non-struct or qualified type (time.Time would promote its MarshalJSON and
// replace the whole object), an unexported type, and a cycle.
func collectEmbedded(
	root string,
	field *ast.Field,
	depth int,
	optional bool,
	visiting map[string]bool,
	structs map[string]*ast.StructType,
	enums map[string]*Enum,
) ([]candidate, error) {
	typ := field.Type
	viaPointer := false
	if star, ok := typ.(*ast.StarExpr); ok {
		typ, viaPointer = star.X, true
	}
	ident, ok := typ.(*ast.Ident)
	if !ok {
		return nil, &UnsupportedDeclError{
			Struct: root,
			Field:  types.ExprString(field.Type),
			Reason: "embedded type " + types.ExprString(field.Type) + " is not a struct declared on the wire, so its JSON form is not modelled",
		}
	}

	if tagNamesField(field.Tag) {
		c, keep, err := namedCandidate(root, field, ident.Name, depth, optional, structs, enums)
		if err != nil || !keep {
			return nil, err
		}
		return []candidate{c}, nil
	}
	if _, _, skip, _ := jsonTag(field.Tag, ident.Name); skip {
		return nil, nil
	}

	inner, ok := structs[ident.Name]
	if !ok || !ast.IsExported(ident.Name) {
		return nil, &UnsupportedDeclError{
			Struct: root,
			Field:  ident.Name,
			Reason: "embedded type " + ident.Name + " is not an exported struct, so encoding/json would not promote its fields the way the generator models",
		}
	}
	if visiting[ident.Name] {
		return nil, &UnsupportedDeclError{
			Struct: root,
			Field:  ident.Name,
			Reason: "embedding cycle through " + ident.Name,
		}
	}

	visiting[ident.Name] = true
	defer delete(visiting, ident.Name)
	return collectFields(root, inner, depth+1, optional || viaPointer, visiting, structs, enums)
}

// resolveFields keeps one field per JSON key by encoding/json's dominance
// rules: the shallowest wins, and among equally shallow ones a single tagged
// field wins. Where encoding/json would drop the key silently -- two equally
// dominant candidates -- the build stops instead, so the author resolves it
// rather than a field quietly leaving the wire.
func resolveFields(root string, candidates []candidate) ([]Field, error) {
	byKey := map[string][]candidate{}
	var order []string
	for _, c := range candidates {
		if _, seen := byKey[c.field.JSONName]; !seen {
			order = append(order, c.field.JSONName)
		}
		byKey[c.field.JSONName] = append(byKey[c.field.JSONName], c)
	}

	fields := make([]Field, 0, len(order))
	for _, key := range order {
		winner, ok := dominant(byKey[key])
		if !ok {
			return nil, &UnsupportedDeclError{
				Struct: root,
				Field:  key,
				Reason: "two embedded fields share the json key " + key + " at the same depth; encoding/json would drop it from the wire",
			}
		}
		fields = append(fields, winner.field)
	}
	return fields, nil
}

// dominant picks the field encoding/json would marshal among candidates for
// one key, reporting false when none dominates.
func dominant(cs []candidate) (candidate, bool) {
	minDepth := cs[0].depth
	for _, c := range cs[1:] {
		if c.depth < minDepth {
			minDepth = c.depth
		}
	}
	var shallow []candidate
	for _, c := range cs {
		if c.depth == minDepth {
			shallow = append(shallow, c)
		}
	}
	if len(shallow) == 1 {
		return shallow[0], true
	}
	var tagged []candidate
	for _, c := range shallow {
		if c.tagged {
			tagged = append(tagged, c)
		}
	}
	if len(tagged) == 1 {
		return tagged[0], true
	}
	return candidate{}, false
}

// namedRef returns the named type a field's shape refers to, through any
// number of array levels, or "" for an unnamed scalar.
//
// A named string type reaches here too: SMARTStatus validates as a string but
// still has to be emitted as a union, so its name has to travel out of the
// conversion or the generated interface refers to a type nothing declares.
func namedRef(t Type) string {
	for {
		switch t.Kind {
		case KindObject:
			return t.Ref
		case KindArray:
			if t.Elem == nil {
				return ""
			}
			t = *t.Elem
		default:
			return t.Ref
		}
	}
}

// convertType maps one Go type expression onto its wire shape.
func convertType(
	expr ast.Expr,
	structName, fieldName string,
	structs map[string]*ast.StructType,
	enums map[string]*Enum,
) (Type, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string":
			return Type{Kind: KindString}, nil
		case "bool":
			return Type{Kind: KindBoolean}, nil
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
			"float32", "float64":
			return Type{Kind: KindNumber}, nil
		}
		if _, ok := enums[t.Name]; ok {
			// Validated as a string: the schema's job is to stop a number or an
			// object reaching a field the page treats as text. Which strings are
			// legal is the union's job in the emitted TypeScript, and refusing a
			// whole frame over an unrecognised value would blank the dashboard
			// on any server that learned a new one.
			return Type{Kind: KindString, Ref: t.Name}, nil
		}
		if _, ok := structs[t.Name]; ok {
			return Type{Kind: KindObject, Ref: t.Name}, nil
		}

	case *ast.SelectorExpr:
		// time.Time is the only qualified type on the wire, and it marshals to
		// an RFC3339 string.
		if pkg, ok := t.X.(*ast.Ident); ok && pkg.Name == "time" && t.Sel.Name == "Time" {
			return Type{Kind: KindString}, nil
		}

	case *ast.ArrayType:
		// A fixed-size array would marshal to a JSON array too, but none exist
		// here and treating one as a slice would claim nullability it does not
		// have, so only slices are accepted.
		if t.Len == nil {
			// encoding/json sends a byte slice as one base64 string, not as an
			// array of numbers, so modelling it as an array would refuse every
			// frame carrying it. Nothing on the wire uses one; if something
			// ever does, the build stops here.
			if ident, ok := t.Elt.(*ast.Ident); ok && (ident.Name == "byte" || ident.Name == "uint8") {
				return Type{}, &UnsupportedTypeError{Struct: structName, Field: fieldName, GoType: "[]" + ident.Name}
			}
			elem, err := convertType(t.Elt, structName, fieldName, structs, enums)
			if err != nil {
				return Type{}, err
			}
			// A nil slice marshals to null, which is how a machine says it has
			// none of something.
			return Type{Kind: KindArray, Elem: &elem, Nullable: true}, nil
		}

	case *ast.StarExpr:
		inner, err := convertType(t.X, structName, fieldName, structs, enums)
		if err != nil {
			return Type{}, err
		}
		inner.Nullable = true
		return inner, nil
	}

	return Type{}, &UnsupportedTypeError{
		Struct: structName,
		Field:  fieldName,
		GoType: types.ExprString(expr),
	}
}

// jsonTag reads the encoding/json tag, returning the wire key, whether the
// server may omit the field, and whether it is off the wire entirely.
// tagNamesField reports whether tag gives the field an explicit json name, as
// opposed to no tag, no json key, or options alone such as `json:",omitempty"`.
// jsonTagValue returns the json key of a struct tag literal. The literal is
// unquoted first because Go accepts a tag in either quoting, and encoding/json
// reads the unquoted form: trimming backquotes alone misreads an interpreted
// "json:\"x\"" tag. go/parser has already validated the literal, so an
// unquoting failure cannot occur on parsed source and reads as no tag.
func jsonTagValue(tag *ast.BasicLit) (string, bool) {
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return "", false
	}
	return reflect.StructTag(raw).Lookup("json")
}

func tagNamesField(tag *ast.BasicLit) bool {
	if tag == nil {
		return false
	}
	value, ok := jsonTagValue(tag)
	if !ok {
		return false
	}
	return strings.Split(value, ",")[0] != ""
}

// jsonTag reads a field's json tag the way encoding/json does: its wire name,
// whether the key may be omitted (omitempty or omitzero), whether the field is
// skipped ("-"), and whether the ",string" option re-encodes the value as a
// quoted string. Options encoding/json does not know are ignored, as it
// ignores them.
func jsonTag(tag *ast.BasicLit, goName string) (name string, optional, skip, quoted bool) {
	if tag == nil {
		// No tag means encoding/json uses the Go name verbatim.
		return goName, false, false, false
	}
	value, ok := jsonTagValue(tag)
	if !ok {
		return goName, false, false, false
	}
	parts := strings.Split(value, ",")
	if parts[0] == "-" && len(parts) == 1 {
		return "", false, true, false
	}
	name = parts[0]
	if name == "" {
		name = goName
	}
	for _, opt := range parts[1:] {
		switch opt {
		case "omitempty", "omitzero":
			optional = true
		case "string":
			quoted = true
		}
	}
	return name, optional, false, quoted
}

// docText flattens a comment group into a single string, dropping the markers.
func docText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return strings.TrimRight(group.Text(), "\n")
}
