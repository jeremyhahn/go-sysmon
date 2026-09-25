package prom

import "strconv"

// RemoteWriteError reports a remote-write endpoint that answered with a status
// outside 2xx.
//
// Status is kept apart from the body because the two mean different things to
// a caller: 4xx is a request Prometheus will never accept, however many times
// it is replayed -- a sample older than the retention window, a label set that
// does not parse -- while 5xx and 429 are transient and worth retrying. Body is
// truncated, since a misconfigured URL can point at anything and a rejected
// write must not paste a stranger's HTML into the log.
//
// BodyErr is set when the body could not be read in full, and Body then holds
// what was read before the failure. The status stands either way: it, not the
// body, is the rejection.
type RemoteWriteError struct {
	URL     string
	Status  int
	Body    string
	BodyErr error
}

func (e *RemoteWriteError) Error() string {
	msg := "prom: remote write to " + e.URL + " returned " + strconv.Itoa(e.Status) + ": " + e.Body
	if e.BodyErr != nil {
		msg += " (response body unreadable: " + e.BodyErr.Error() + ")"
	}
	return msg
}

// Retryable reports whether replaying the same request could succeed. A 429 is
// backpressure and a 5xx is the receiver's problem; every other rejection is
// about the request itself and replaying it only wastes the buffer.
//
// This set is the protocol's, not a judgement call. The Remote-Write 1.0
// specification is explicit in both directions:
//
//	Senders MUST retry write requests on HTTP 5xx responses [...]
//	They MUST NOT retry write requests on HTTP 2xx and 4xx responses other
//	than 429. They MAY retry on HTTP 429 responses.
//	[Receivers] MUST respond with HTTP status code 4xx when the request is
//	invalid, will never be able to succeed and should not be retried.
//
// -- https://prometheus.io/docs/specs/prw/remote_write_spec/, Retries & Backoff
//
// 408 is the tempting exception and must not be made one. Read as English it
// says the receiver stopped waiting, which sounds transient; read as this
// protocol it is a 4xx, which a conforming receiver sends only for a request
// that can never succeed. Retrying it means holding a batch at the head of the
// queue behind newer ones for a rejection the receiver has already declared
// permanent -- and Prometheus's own sender (storage/remote/client.go) retries
// 5xx and 429 alone. A receiver that wants a write retried has 503 to say so.
func (e *RemoteWriteError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// RequestError reports a remote-write or query request that never produced a
// response: a refused connection, a DNS failure, a timeout, a malformed URL.
type RequestError struct {
	URL string
	Err error
}

func (e *RequestError) Error() string {
	return "prom: request to " + e.URL + " failed: " + e.Err.Error()
}

func (e *RequestError) Unwrap() error { return e.Err }

// QueryError reports a Prometheus query API response that parsed but reported a
// failure, carrying the errorType and error fields Prometheus supplies.
type QueryError struct {
	Query  string
	Type   string
	Detail string
}

func (e *QueryError) Error() string {
	return "prom: query " + strconv.Quote(e.Query) + " failed (" + e.Type + "): " + e.Detail
}

// DecodeError reports a response from the query API that could not be parsed as
// the documented shape. It is distinct from QueryError: that one is Prometheus
// saying no, this one is something that is not Prometheus answering at all --
// most often a query_url pointing at a proxy or the wrong service.
type DecodeError struct {
	URL string
	Err error
}

func (e *DecodeError) Error() string {
	return "prom: cannot decode the response from " + e.URL + ": " + e.Err.Error()
}

func (e *DecodeError) Unwrap() error { return e.Err }

// UnexpectedResultTypeError reports a range query answered with something other
// than a matrix.
//
// It is wrapped in DecodeError rather than reported as a query failure, because
// it is the same class: something answered that is not Prometheus answering
// this question. A vector decodes into the range shape as an empty result and
// would otherwise draw as "no history for this window".
type UnexpectedResultTypeError struct {
	Got string
}

func (e *UnexpectedResultTypeError) Error() string {
	if e.Got == "" {
		return "the response carries no resultType, so it is not a range query answer"
	}
	return "a range query was answered with a " + quote(e.Got) + " result, not a matrix"
}

// UnknownMetricError reports a history request for a metric key that is not in
// the allowlist.
//
// The dashboard is unauthenticated, so the history endpoint maps a fixed set of
// keys to PromQL rather than forwarding an expression. Passing a caller's
// expression through would make go-sysmon an open query proxy for everything
// in the time-series database, including whatever else writes to it.
type UnknownMetricError struct {
	Key string
}

func (e *UnknownMetricError) Error() string {
	return "prom: no history is published for metric " + strconv.Quote(e.Key)
}

// InvalidRangeError reports a history request whose window cannot be served: an
// end at or before the start, or a step that would ask Prometheus for more
// points than it will return.
type InvalidRangeError struct {
	Message string
}

func (e *InvalidRangeError) Error() string {
	return "prom: invalid range: " + e.Message
}

// NoQueryEndpointError reports a range query on a node with no query_url. It is
// distinct from a query that failed: nothing was attempted, because there is
// nowhere to attempt it, and a caller that has not checked Enabled first gets
// told which of the two happened.
type NoQueryEndpointError struct{}

func (e *NoQueryEndpointError) Error() string {
	return "prom: no prometheus.query_url is configured, so no history can be read"
}

// UnknownNodeError reports a history request for a node this dashboard does not
// serve. It is deliberately distinct from InvalidInstanceError: that one is a
// name no node could have, this one is a well-formed name belonging to some
// other machine in the same time-series database.
type UnknownNodeError struct {
	Node string
}

func (e *UnknownNodeError) Error() string {
	return "prom: " + quote(e.Node) + " is not this node or one of its configured peers"
}

// LookbackUnpublishedError reports a time-series database that does not say how
// long its query lookback is. Reason says what it did answer. See lookback.go
// for what each known backend publishes.
type LookbackUnpublishedError struct {
	Reason string
}

func (e *LookbackUnpublishedError) Error() string {
	return "prom: the time-series database does not publish its query lookback: " + e.Reason
}
