package delta

// ErrDecode reports a snapshot that could not be read as JSON. It should not
// happen -- the bytes came from encoding/json a moment earlier -- so it is
// reported rather than absorbed: a stream that silently stopped diffing would
// go on working while quietly sending nothing.
type ErrDecode struct {
	Err error
}

func (e *ErrDecode) Error() string {
	return "delta: cannot decode a snapshot: " + e.Err.Error()
}

func (e *ErrDecode) Unwrap() error { return e.Err }

// ErrEncode reports a value that could not be re-encoded while building a
// patch. Path names where in the snapshot it was, since the value itself may
// be large.
type ErrEncode struct {
	Path string
	Err  error
}

func (e *ErrEncode) Error() string {
	if e.Path == "" {
		return "delta: cannot encode a patch value: " + e.Err.Error()
	}
	return "delta: cannot encode the patch value at " + e.Path + ": " + e.Err.Error()
}

func (e *ErrEncode) Unwrap() error { return e.Err }

// ErrApply reports a patch that does not fit the value it was applied to.
//
// It means the receiver's copy and the sender's have diverged, which no further
// patch can repair: the only correct response is to start again from a whole
// snapshot. It is an error rather than a best-effort merge for exactly that
// reason -- a partly applied patch leaves numbers that look measured and are
// not, which is the failure this stream must never produce.
type ErrApply struct {
	Path   string
	Reason string
}

func (e *ErrApply) Error() string {
	where := e.Path
	if where == "" {
		where = "the snapshot root"
	}
	return "delta: cannot apply a patch at " + where + ": " + e.Reason
}
