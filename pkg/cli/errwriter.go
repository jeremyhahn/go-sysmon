package cli

import "io"

// errWriter wraps an io.Writer and remembers the first write error it sees.
// Once an error occurs every subsequent write becomes a no-op, so a long
// sequence of formatted writes can be issued without checking each one and
// then inspected a single time via Err.
//
// This is the error-tracking writer pattern described in Effective Go. The
// renderers emit well over a hundred writes per report; checking each call
// individually would bury the formatting logic without catching anything the
// first recorded error does not already report.
type errWriter struct {
	w   io.Writer
	err error
}

// newErrWriter returns an errWriter that forwards to w.
func newErrWriter(w io.Writer) *errWriter {
	return &errWriter{w: w}
}

// Write implements io.Writer. It forwards to the underlying writer until a
// write fails, after which it reports the recorded error without writing.
func (ew *errWriter) Write(p []byte) (int, error) {
	if ew.err != nil {
		return 0, ew.err
	}
	n, err := ew.w.Write(p)
	ew.err = err
	return n, err
}

// Err returns the first write error encountered, or nil if all writes
// succeeded.
func (ew *errWriter) Err() error {
	return ew.err
}
