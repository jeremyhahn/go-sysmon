package gitea

import "strconv"

// RequestError reports a request that got no usable answer: it could not be
// built or sent, or its answer could not be read or decoded.
type RequestError struct {
	Method string
	Path   string
	Cause  error
}

func (e *RequestError) Error() string {
	return "gitea: " + e.Method + " " + e.Path + ": " + e.Cause.Error()
}

func (e *RequestError) Unwrap() error { return e.Cause }

// APIError reports a request Gitea answered with a status other than 2xx.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := "gitea: " + e.Method + " " + e.Path + ": HTTP " + strconv.Itoa(e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// LabelNotFoundError reports a label name the repository does not have.
type LabelNotFoundError struct {
	Label string
}

func (e *LabelNotFoundError) Error() string {
	return "gitea: the repository has no label " + strconv.Quote(e.Label)
}

// LabelNotAppliedError reports an issue that was filed but does not carry its
// label after two attempts. The issue exists; it is invisible to anything that
// queries by that label until someone labels it.
type LabelNotAppliedError struct {
	Issue int64
	Label string
}

func (e *LabelNotAppliedError) Error() string {
	return "gitea: issue #" + strconv.FormatInt(e.Issue, 10) + " was filed but label " +
		strconv.Quote(e.Label) + " did not stick; label it by hand"
}
