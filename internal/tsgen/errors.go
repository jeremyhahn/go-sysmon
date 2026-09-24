package tsgen

// SourceError reports a Go source file that could not be read or parsed. It
// carries the path because the generator is pointed at a file by the caller,
// and "cannot parse" without the name is not actionable in a build log.
type SourceError struct {
	Path  string
	Cause error
}

func (e *SourceError) Error() string {
	return "tsgen: read " + e.Path + ": " + e.Cause.Error()
}

func (e *SourceError) Unwrap() error { return e.Cause }

// UnsupportedTypeError reports a Go field the generator has no TypeScript
// spelling for.
//
// It is deliberately fatal rather than skipped. A skipped field is a field
// missing from the emitted interface and missing from the schema, which is
// exactly the silent drift this generator exists to remove: the dashboard
// would go on reading a value nothing declares and nothing validates. Adding
// a new shape to pkg/types should stop the build until the generator knows how
// to spell it.
type UnsupportedTypeError struct {
	Struct string
	Field  string
	GoType string
}

func (e *UnsupportedTypeError) Error() string {
	return "tsgen: " + e.Struct + "." + e.Field + ": no TypeScript form for Go type " + e.GoType
}

// UnknownRootError reports a root type name that is not declared in the parsed
// source. It catches a typo in the generator's own root list, which would
// otherwise silently emit a smaller schema than intended.
type UnknownRootError struct {
	Name string
}

func (e *UnknownRootError) Error() string {
	return "tsgen: root type " + e.Name + " is not declared in the source"
}

// WriteError reports an output file that could not be written.
type WriteError struct {
	Path  string
	Cause error
}

func (e *WriteError) Error() string {
	return "tsgen: write " + e.Path + ": " + e.Cause.Error()
}

func (e *WriteError) Unwrap() error { return e.Cause }

// UnsupportedDeclError reports a struct field declaration the parser will not
// read, in a type that is on the wire.
//
// It exists because the alternative was skipping one silently. encoding/json
// promotes an embedded struct's fields and emits every name in a `A, B string`
// group, so a field the parser passes over is still a field the server sends --
// absent from the interfaces, absent from the schema, and therefore refused by
// the validator that consumes it. That is precisely the drift this generator
// replaced a hand-written mirror to prevent, so it stops the build instead.
type UnsupportedDeclError struct {
	Struct string
	Field  string
	Reason string
}

func (e *UnsupportedDeclError) Error() string {
	where := e.Struct
	if e.Field != "" {
		where += "." + e.Field
	}
	return "tsgen: " + where + ": " + e.Reason
}

// UnsupportedConstError reports a constant of a wire enum type whose value the
// generator cannot compute.
//
// Skipping it would emit a TypeScript union missing a value the server can
// marshal, and the validator would then refuse every frame carrying it -- the
// same silent drift UnsupportedDeclError guards against, one level down.
type UnsupportedConstError struct {
	Enum   string
	Const  string
	Reason string
}

func (e *UnsupportedConstError) Error() string {
	return "tsgen: const " + e.Const + " of type " + e.Enum + ": " + e.Reason
}
