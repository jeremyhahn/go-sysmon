// Package outbound builds HTTP clients that stay pointed where they were aimed.
//
// Every outbound request this service makes goes somewhere an operator wrote in
// a config file: a fleet peer, a remote-write receiver, a Prometheus to query.
// In all three cases the response is treated as authoritative — health data on
// a dashboard, metrics accepted into a database, a chart of the past — so
// *where it came from* is a trust decision rather than a detail.
//
// Go's default client follows redirects, anywhere, including across hosts and
// from https down to plain http. That turns "the endpoint you configured" into
// "wherever that endpoint last pointed you", silently, with the answer still
// attributed to the original. This package exists so that decision is made once
// and applied to every client rather than remembered at each call site.
package outbound

import (
	"fmt"
	"net/http"
	"time"
)

// maxRedirects caps a redirect chain. Supplying CheckRedirect replaces the
// client's own ten-hop limit, so the cap has to be reimposed here or a peer
// could loop a caller indefinitely inside its timeout.
const maxRedirects = 5

// NewClient returns a client that will not be walked off the host it was
// pointed at.
//
// A zero timeout means the caller bounds requests itself, with a context
// deadline per request — which is what every caller here does, because it lets
// the timeout be reconfigured without discarding the connection pool.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: Check}
}

// Check is the redirect policy, exported so a caller that must build its own
// client still gets the same rules.
//
// A scheme downgrade and a host change are refused outright. Neither is
// something a monitoring endpoint legitimately needs, and both are exactly what
// an on-path attacker would use. An upgrade from http to https on the same host
// is allowed: that is an ingress enforcing TLS, and refusing it would break a
// real deployment to prevent nothing.
//
// A hop that changes the request's method is refused as well, and that one is
// not about where the request goes. RFC 9110 has Go rewrite a POST to a GET on
// 301, 302 and 303, dropping the body -- so a receiver that answers a write
// with "302 -> /" turns a batch of samples into a fetch of a landing page, and
// a 200 from that page is indistinguishable, to the caller, from the samples
// having been stored. Same host, same scheme, no warning, data gone. 307 and
// 308 are the redirects that preserve the method, and they still pass.
func Check(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return &ErrRedirect{
			From:   via[0].URL.String(),
			To:     req.URL.String(),
			Reason: fmt.Sprintf("more than %d redirects", maxRedirects),
		}
	}

	orig := via[0].URL
	prev := via[len(via)-1]
	switch {
	case req.URL.Scheme != "http" && req.URL.Scheme != "https":
		return &ErrRedirect{From: orig.String(), To: req.URL.String(),
			Reason: fmt.Sprintf("scheme %q is not http or https", req.URL.Scheme)}
	case orig.Scheme == "https" && req.URL.Scheme != "https":
		return &ErrRedirect{From: orig.String(), To: req.URL.String(),
			Reason: "refuses to downgrade https to " + req.URL.Scheme}
	case req.URL.Host != orig.Host:
		return &ErrRedirect{From: orig.String(), To: req.URL.String(),
			Reason: "redirect leaves the configured host"}
	// Last, so a hop that is both off-host and method-changing is reported as
	// the off-host one. Either refusal is correct; naming the destination is
	// more use to whoever reads the log than naming the verb.
	case req.Method != prev.Method:
		return &ErrRedirect{From: orig.String(), To: req.URL.String(),
			Reason: fmt.Sprintf("redirect rewrites the %s as a %s, discarding the request body",
				prev.Method, req.Method)}
	}
	return nil
}

// ErrRedirect reports a redirect that would have moved a request off the
// configured endpoint. It names the hop, and is deliberately built only from
// URLs the operator already configured or the endpoint itself returned.
type ErrRedirect struct {
	From   string
	To     string
	Reason string
}

func (e *ErrRedirect) Error() string {
	return fmt.Sprintf("refusing redirect from %s to %s: %s", e.From, e.To, e.Reason)
}
