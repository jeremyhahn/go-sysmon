package outbound

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

// req builds the minimal *http.Request the redirect policy inspects.
func req(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return &http.Request{URL: u}
}

// TestCheck covers the policy rule by rule. The poller reports a
// decoded response as the peer's authoritative health, so a redirect that
// leaves the configured endpoint means the dashboard would attribute another
// server's answer -- or an attacker's -- to that peer.
func TestCheck(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		to      string
		wantErr bool
		reason  string
	}{
		{
			name: "first request is not a redirect",
			to:   "http://peer:8081/api/snapshot",
		},
		{
			name: "same host and scheme is allowed",
			from: "https://peer:8081/api/snapshot",
			to:   "https://peer:8081/api/snapshot/",
		},
		{
			name: "http to https on the same host is an upgrade, not a downgrade",
			from: "http://peer:8081/api/snapshot",
			to:   "https://peer:8081/api/snapshot",
		},
		{
			name: "https downgraded to http is refused", from: "https://peer:8081/api/snapshot",
			to: "http://peer:8081/api/snapshot", wantErr: true, reason: "downgrade",
		},
		{
			name: "a different host is refused even over https", from: "https://peer:8081/api/snapshot",
			to: "https://attacker:8081/api/snapshot", wantErr: true, reason: "host",
		},
		{
			name: "a different port is a different host", from: "http://peer:8081/api/snapshot",
			to: "http://peer:9999/api/snapshot", wantErr: true, reason: "host",
		},
		{
			name: "a non-http scheme is refused", from: "http://peer:8081/api/snapshot",
			to: "file:///etc/passwd", wantErr: true, reason: "scheme",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var via []*http.Request
			if tc.from != "" {
				via = []*http.Request{req(t, tc.from)}
			}

			err := Check(req(t, tc.to), via)
			if tc.wantErr {
				var redir *RedirectError
				if !errors.As(err, &redir) {
					t.Fatalf("Check() = %v, want a *RedirectError (%s)", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check() = %v, want it allowed", err)
			}
		})
	}
}

// TestCheck_CapsTheChain guards the consequence of supplying
// CheckRedirect at all: it replaces the client's own ten-hop limit, so without
// a cap here a peer answering every request with a same-host redirect would
// spin the poller until its timeout.
func TestCheck_CapsTheChain(t *testing.T) {
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = req(t, "http://peer:8081/api/snapshot")
	}

	var redir *RedirectError
	if err := Check(req(t, "http://peer:8081/api/snapshot"), via); !errors.As(err, &redir) {
		t.Fatalf("Check() at hop %d = %v, want a *RedirectError", maxRedirects, err)
	}
}

// methodReq builds a request the policy can compare methods on. The table above
// leaves Method empty on both sides, which is equal and therefore says nothing
// about this rule.
func methodReq(t *testing.T, method, raw string) *http.Request {
	t.Helper()
	r := req(t, raw)
	r.Method = method
	return r
}

// TestCheck_RefusesAHopThatChangesTheMethod. The other rules are about where a
// request ends up; this one is about whether it is still the same request when
// it gets there. Go follows a 301, 302 or 303 answering a POST by issuing a GET
// with no body, so a same-host redirect can quietly convert a write into a
// read -- and a 2xx from the destination is reported to the caller as the write
// having succeeded.
func TestCheck_RefusesAHopThatChangesTheMethod(t *testing.T) {
	cases := []struct {
		name    string
		from    string // the method of the request already made
		to      string // the method Go proposes for the next hop
		wantErr bool
	}{
		{name: "302 rewrites a POST as a GET", from: http.MethodPost, to: http.MethodGet, wantErr: true},
		{name: "307 keeps the POST", from: http.MethodPost, to: http.MethodPost},
		{name: "a GET redirected to a GET is unaffected", from: http.MethodGet, to: http.MethodGet},
	}

	const url = "http://receiver:9090/api/v1/write"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via := []*http.Request{methodReq(t, tc.from, url)}
			err := Check(methodReq(t, tc.to, url), via)

			var redir *RedirectError
			if tc.wantErr {
				if !errors.As(err, &redir) {
					t.Fatalf("Check() = %v, want a *RedirectError: the body is dropped on this hop", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check() = %v, want it allowed", err)
			}
		})
	}
}
