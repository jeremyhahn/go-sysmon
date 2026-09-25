package tsgen

import (
	"errors"
	"strings"
	"testing"
)

// The error messages are what a failed build prints, so they are worth
// asserting on: "tsgen: read x.go" with no reason, or a reason with no file,
// each cost the reader the same second look.

func TestSourceError(t *testing.T) {
	cause := errors.New("expected declaration")
	err := &SourceError{Path: "pkg/types/types.go", Cause: cause}

	if got := err.Error(); got != "tsgen: read pkg/types/types.go: expected declaration" {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Error("Unwrap does not expose the cause, so errors.Is cannot see it")
	}
}

func TestSourceError_WrappedTargetNotPresent(t *testing.T) {
	err := &SourceError{Path: "x.go", Cause: errors.New("boom")}
	if errors.Is(err, errors.New("boom")) {
		t.Error("errors.Is matched a different error with the same text")
	}
}

func TestWriteError(t *testing.T) {
	cause := errors.New("permission denied")
	err := &WriteError{Path: "out/types.generated.ts", Cause: cause}

	if got := err.Error(); got != "tsgen: write out/types.generated.ts: permission denied" {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Error("Unwrap does not expose the cause")
	}
}

func TestUnsupportedTypeError(t *testing.T) {
	err := &UnsupportedTypeError{Struct: "NetworkInfo", Field: "Labels", GoType: "map[string]string"}
	want := "tsgen: NetworkInfo.Labels: no TypeScript form for Go type map[string]string"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestUnsupportedTypeError_Empty is the degenerate case: a zero value must
// still produce a sentence rather than panicking on a nil field somewhere.
func TestUnsupportedTypeError_Empty(t *testing.T) {
	err := &UnsupportedTypeError{}
	if got := err.Error(); !strings.HasPrefix(got, "tsgen: ") {
		t.Errorf("Error() = %q, want it to keep the prefix", got)
	}
}

func TestUnknownRootError(t *testing.T) {
	err := &UnknownRootError{Name: "Snapshott"}
	want := "tsgen: root type Snapshott is not declared in the source"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestUnknownRootError_Empty(t *testing.T) {
	if got := (&UnknownRootError{}).Error(); !strings.Contains(got, "root type") {
		t.Errorf("Error() = %q, want it to still say what kind of name is missing", got)
	}
}

// TestUnsupportedDeclError covers both spellings: with a field name and, for an
// embedded field that has none, without one.
func TestUnsupportedDeclError(t *testing.T) {
	withField := (&UnsupportedDeclError{Struct: "Root", Field: "Alpha", Reason: "shared name"}).Error()
	if !strings.Contains(withField, "Root.Alpha") || !strings.Contains(withField, "shared name") {
		t.Errorf("Error() = %q, want it to name Root.Alpha and the reason", withField)
	}
	embedded := (&UnsupportedDeclError{Struct: "Root", Reason: "embedded field"}).Error()
	if !strings.Contains(embedded, "Root") || strings.Contains(embedded, "Root.") {
		t.Errorf("Error() = %q, want it to name Root with no trailing field", embedded)
	}
}

// TestUnsupportedDeclError_Empty: a zero value still reads as a tsgen error
// rather than panicking or returning a bare prefix.
func TestUnsupportedDeclError_Empty(t *testing.T) {
	if got := (&UnsupportedDeclError{}).Error(); !strings.HasPrefix(got, "tsgen:") {
		t.Errorf("Error() = %q, want a tsgen-prefixed message", got)
	}
}
