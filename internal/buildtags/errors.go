package buildtags

import (
	"strconv"
	"strings"
)

// SourceError reports a file or directory that could not be read, or a build
// constraint that does not parse.
type SourceError struct {
	Path  string
	Cause error
}

func (e *SourceError) Error() string {
	return "buildtags: " + e.Path + ": " + e.Cause.Error()
}

func (e *SourceError) Unwrap() error { return e.Cause }

// UnknownTagError reports build constraints naming a tag that no shipped build
// shape sets. Every file behind such a tag is compiled by no gate.
type UnknownTagError struct {
	Uses []Use
}

func (e *UnknownTagError) Error() string {
	var b strings.Builder
	b.WriteString("buildtags: ")
	b.WriteString(strconv.Itoa(len(e.Uses)))
	b.WriteString(" use(s) of a build tag that no shipped build shape sets, so no gate compiles the files behind them.\n")
	b.WriteString("Add the tag set to BUILD_SHAPES in the Makefile, or remove the constraint:")
	for _, u := range e.Uses {
		b.WriteString("\n  ")
		b.WriteString(u.File)
		b.WriteString(":")
		b.WriteString(strconv.Itoa(u.Line))
		b.WriteString(": ")
		b.WriteString(u.Tag)
	}
	return b.String()
}

// PlatformListError reports that the toolchain's platform list could not be
// read: `go tool dist list` failed (Cause), printed a line that is not a
// GOOS/GOARCH pair (Line), or printed no pairs at all (neither).
type PlatformListError struct {
	Line  string
	Cause error
}

func (e *PlatformListError) Error() string {
	switch {
	case e.Cause != nil:
		return "buildtags: go tool dist list: " + e.Cause.Error()
	case e.Line != "":
		return "buildtags: go tool dist list: not a GOOS/GOARCH pair: " + strconv.Quote(e.Line)
	default:
		return "buildtags: go tool dist list printed no platforms"
	}
}

func (e *PlatformListError) Unwrap() error { return e.Cause }
