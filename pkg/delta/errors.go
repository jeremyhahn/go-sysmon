package delta

// DecodeError reports a snapshot that could not be read as JSON. It should not
// happen -- the bytes came from encoding/json a moment earlier -- so it is
// reported rather than absorbed: a stream that silently stopped diffing would
// go on working while quietly sending nothing.
type DecodeError struct {
	Err error
}

func (e *DecodeError) Error() string {
	return "delta: cannot decode a snapshot: " + e.Err.Error()
}

func (e *DecodeError) Unwrap() error { return e.Err }

// EncodeError reports a value that could not be re-encoded while building a
// patch. Path names where in the snapshot it was, since the value itself may
// be large.
type EncodeError struct {
	Path string
	Err  error
}

func (e *EncodeError) Error() string {
	if e.Path == "" {
		return "delta: cannot encode a patch value: " + e.Err.Error()
	}
	return "delta: cannot encode the patch value at " + e.Path + ": " + e.Err.Error()
}

func (e *EncodeError) Unwrap() error { return e.Err }

// ApplyError reports a patch that does not fit the value it was applied to.
//
// It means the receiver's copy and the sender's have diverged, which no further
// patch can repair: the only correct response is to start again from a whole
// snapshot. It is an error rather than a best-effort merge for exactly that
// reason -- a partly applied patch leaves numbers that look measured and are
// not, which is the failure this stream must never produce.
type ApplyError struct {
	Path   string
	Reason string
}

func (e *ApplyError) Error() string {
	where := e.Path
	if where == "" {
		where = "the snapshot root"
	}
	return "delta: cannot apply a patch at " + where + ": " + e.Reason
}
