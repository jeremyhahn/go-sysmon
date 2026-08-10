package cli

// RenderError indicates a failure during terminal rendering.
type RenderError struct {
	Function string
	Cause    error
}

func (e *RenderError) Error() string {
	return "cli render " + e.Function + ": " + e.Cause.Error()
}

func (e *RenderError) Unwrap() error {
	return e.Cause
}

// nilSnapshotError indicates a nil snapshot was passed to a render function.
type nilSnapshotError struct{}

func (e *nilSnapshotError) Error() string {
	return "nil snapshot"
}

// errNilSnapshot is the sentinel error value for nil snapshots.
var errNilSnapshot = &nilSnapshotError{}
