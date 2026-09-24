package pins

import (
	"errors"
	"strconv"
	"strings"
)

// PinFileError reports a file a pin is read from -- go.mod, .node-version --
// that could not be read or parsed, or that holds no usable version.
type PinFileError struct {
	Path   string
	Reason string
	Cause  error
}

func (e *PinFileError) Error() string {
	msg := "pins: " + e.Path + ": " + e.Reason
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *PinFileError) Unwrap() error { return e.Cause }

// SourceError reports a file or directory the offline check could not read or
// parse.
type SourceError struct {
	Path  string
	Cause error
}

func (e *SourceError) Error() string {
	return "pins: " + e.Path + ": " + e.Cause.Error()
}

func (e *SourceError) Unwrap() error { return e.Cause }

// Mismatch is one version reference that disagrees with its pin.
type Mismatch struct {
	File   string
	Line   int
	Found  string
	Reason string
}

// MismatchError reports every version reference that disagrees with the pin
// it repeats: go.mod's go directive, the Node.js version file, or the Debian
// and Ubuntu releases the Makefile pins.
type MismatchError struct {
	Mismatches []Mismatch
}

func (e *MismatchError) Error() string {
	var b strings.Builder
	b.WriteString("pins: ")
	b.WriteString(strconv.Itoa(len(e.Mismatches)))
	b.WriteString(" version reference(s) disagree with their pin. Make each one name exactly the pinned release:")
	for _, m := range e.Mismatches {
		b.WriteString("\n  ")
		b.WriteString(m.File)
		if m.Line > 0 {
			b.WriteString(":")
			b.WriteString(strconv.Itoa(m.Line))
		}
		b.WriteString(": ")
		b.WriteString(strconv.Quote(m.Found))
		b.WriteString(": ")
		b.WriteString(m.Reason)
	}
	return b.String()
}

// VersionError reports a string that is not a version of the kind expected,
// such as a Go release ("go1.27.1"), a module version ("v1.2.3") or a Node.js
// release ("24.21.0").
type VersionError struct {
	Kind  string
	Value string
}

func (e *VersionError) Error() string {
	return "pins: " + strconv.Quote(e.Value) + " is not a valid " + e.Kind + " version"
}

// ItemError reports that one item reached no verdict: its latest release could
// not be fetched, or the pin or the reported release is not a valid version.
// It is never a verdict about the pin -- an item with no verdict has not been
// checked, and is not current.
type ItemError struct {
	Item  string
	Cause error
}

func (e *ItemError) Error() string {
	return "pins: " + e.Item + ": no verdict: " + e.Cause.Error()
}

func (e *ItemError) Unwrap() error { return e.Cause }

// UncheckedError reports the items the check reached no verdict on.
type UncheckedError struct {
	Failures []*ItemError
}

func (e *UncheckedError) Error() string {
	var b strings.Builder
	b.WriteString("pins: ")
	b.WriteString(strconv.Itoa(len(e.Failures)))
	b.WriteString(" item(s) could not be checked against their latest stable release:")
	for _, f := range e.Failures {
		b.WriteString("\n  ")
		b.WriteString(f.Error())
	}
	return b.String()
}

// Unwrap exposes every failure to errors.Is and errors.As.
func (e *UncheckedError) Unwrap() []error {
	errs := make([]error, len(e.Failures))
	for i, f := range e.Failures {
		errs[i] = f
	}
	return errs
}

// LagError reports the items that are not pinned at their latest stable
// release.
type LagError struct {
	Results []Result
}

func (e *LagError) Error() string {
	var b strings.Builder
	b.WriteString("pins: ")
	b.WriteString(strconv.Itoa(len(e.Results)))
	b.WriteString(" item(s) not at their latest stable release:")
	for _, r := range e.Results {
		b.WriteString("\n  ")
		b.WriteString(r.String())
	}
	return b.String()
}

// FetchResponseError reports a latest-release source that answered, but not
// with a usable answer: an HTTP status other than 200, or a body that is not
// what the source documents.
type FetchResponseError struct {
	Source string
	Status int
	Detail string
}

func (e *FetchResponseError) Error() string {
	msg := "pins: " + e.Source
	if e.Status != 0 {
		msg += ": HTTP " + strconv.Itoa(e.Status)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// ToolSpecError reports a tool argument that is not "name=module@version".
type ToolSpecError struct {
	Spec string
}

func (e *ToolSpecError) Error() string {
	return "pins: tool " + strconv.Quote(e.Spec) + " is not name=module@version"
}

// CommandError reports a command that failed, with what it wrote to standard
// error.
type CommandError struct {
	Command string
	Stderr  string
	Cause   error
}

func (e *CommandError) Error() string {
	msg := "pins: " + e.Command + ": " + e.Cause.Error()
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *CommandError) Unwrap() error { return e.Cause }

// ErrUnknownCommand reports a request to run a program Run does not know.
var ErrUnknownCommand = errors.New("pins: not a command this package runs")

// RunnerLabelError reports a hosted runner label the runner-images table does
// not list. GitHub offers no such runner, so there is no verdict to give, and
// a job that asks for it waits forever.
type RunnerLabelError struct {
	Label string
}

func (e *RunnerLabelError) Error() string {
	return "pins: runner label " + strconv.Quote(e.Label) + " is not a GitHub-hosted runner listed by actions/runner-images"
}
