package fleet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
)

// PeerRequestError reports a request to a peer that failed before the peer
// gave an HTTP answer, or whose answer could not be read.
//
// Its message is served to anyone who can reach the dashboard -- as a fleet
// card's error, a roll-up's reason for naming a node, and a drill-in's 502 --
// and none of those carries authentication of its own. The error net/http
// returns cannot be passed through: it spells out the request URL and the
// address dialled, "Get \"http://198.51.100.10:8081/api/summary\": dial tcp
// 198.51.100.10:8081: connect: connection refused", which hands out the
// peer's scheme, address and port that GET /api/fleet deliberately withholds
// (#120). A DNS failure names the resolver, a certificate mismatch names the
// host, and a reset mid-body names both ends of the connection.
//
// So the message is built from an allowlist rather than scrubbed: it states
// what happened in words this package chose, and anything it does not
// recognise is reported without detail. Scrubbing the address out of the
// original text cannot be made reliable -- the address dialled is the resolved
// one, not the configured host, so there is no string to look for. The whole
// error stays reachable through Unwrap, and the poller logs it on this host,
// where the operator can read it and an unauthenticated caller cannot.
type PeerRequestError struct {
	Peer  string
	Cause error
}

func (e *PeerRequestError) Error() string {
	return "fleet: request to peer " + e.Peer + " failed: " + peerFailure(e.Cause)
}

func (e *PeerRequestError) Unwrap() error { return e.Cause }

// failureUnknown is the reason given for an error peerFailure does not
// recognise. It says where the detail is, rather than guessing at it.
const failureUnknown = "request failed; see this node's log"

// failureRule recognises one kind of transport failure and describes it
// without an address.
type failureRule func(err error) (reason string, ok bool)

// failureRules are tried in order and the first that recognises the error
// describes it. The order matters: a timeout is reported as one whatever layer
// it surfaced from, and a TLS alert arrives inside the same *net.OpError that
// would otherwise be read as a socket error.
var failureRules = []failureRule{
	timedOut,
	canceled,
	lookupFailed,
	certificateRejected,
	tlsRefused,
	notTLS,
	socketError,
	closedEarly,
	malformedBody,
}

// peerFailure describes err in words that carry no address.
func peerFailure(err error) string {
	for _, rule := range failureRules {
		if reason, ok := rule(err); ok {
			return reason
		}
	}
	return failureUnknown
}

func timedOut(err error) (string, bool) {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return "timed out", true
	}
	return "", false
}

func canceled(err error) (string, bool) {
	if errors.Is(err, context.Canceled) {
		return "request canceled", true
	}
	return "", false
}

// lookupFailed covers name resolution. *net.DNSError's own text names both the
// host looked up and the resolver asked, so only its outcome is kept.
func lookupFailed(err error) (string, bool) {
	var dns *net.DNSError
	if !errors.As(err, &dns) {
		return "", false
	}
	if dns.IsNotFound {
		return "host name not found", true
	}
	return "host name lookup failed", true
}

// certificateRejected covers a peer certificate this node would not accept.
// x509's own messages name the host and the names the certificate is valid
// for, which is exactly the topology being withheld.
func certificateRejected(err error) (string, bool) {
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &unknownCA):
		return "certificate signed by an unknown authority", true
	case errors.As(err, &hostname):
		return "certificate is not valid for the peer's host name", true
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return "certificate has expired or is not yet valid", true
	case errors.As(err, &invalid), errors.As(err, &verify):
		return "certificate verification failed", true
	}
	return "", false
}

// tlsRefused covers the peer ending the handshake with an alert -- most often
// because it requires a client certificate this node did not present. The
// alert arrives as a *net.OpError whose Op is "remote error"; its own text is
// a fixed TLS alert name.
func tlsRefused(err error) (string, bool) {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" && op.Err != nil {
		return "peer refused the TLS handshake: " + op.Err.Error(), true
	}
	return "", false
}

// notTLS covers an https URL pointed at a listener that does not speak TLS.
func notTLS(err error) (string, bool) {
	var record tls.RecordHeaderError
	if errors.Is(err, http.ErrSchemeMismatch) || errors.As(err, &record) {
		return "peer did not answer with TLS", true
	}
	return "", false
}

// socketError covers the connection itself: refused, reset, no route. The
// errno's text is the kernel's fixed description and names nothing.
func socketError(err error) (string, bool) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error(), true
	}
	return "", false
}

func closedEarly(err error) (string, bool) {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection closed before a complete response", true
	}
	return "", false
}

// malformedBody covers an answer that arrived but is not what this node asked
// for. The decoder's messages describe the JSON, never the connection.
func malformedBody(err error) (string, bool) {
	var syntax *json.SyntaxError
	var mistyped *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return "malformed response: " + syntax.Error(), true
	case errors.As(err, &mistyped):
		return "malformed response: " + mistyped.Error(), true
	}
	return "", false
}
