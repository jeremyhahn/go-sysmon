package fleet

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
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

// TestCheckPeerRedirect covers the policy rule by rule. The poller reports a
// decoded response as the peer's authoritative health, so a redirect that
// leaves the configured endpoint means the dashboard would attribute another
// server's answer -- or an attacker's -- to that peer.
func TestCheckPeerRedirect(t *testing.T) {
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

			err := checkPeerRedirect(req(t, tc.to), via)
			if tc.wantErr {
				var redir *ErrPeerRedirect
				if !errors.As(err, &redir) {
					t.Fatalf("checkPeerRedirect() = %v, want an *ErrPeerRedirect (%s)", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkPeerRedirect() = %v, want it allowed", err)
			}
		})
	}
}

// TestCheckPeerRedirect_CapsTheChain guards the consequence of supplying
// CheckRedirect at all: it replaces the client's own ten-hop limit, so without
// a cap here a peer answering every request with a same-host redirect would
// spin the poller until its timeout.
func TestCheckPeerRedirect_CapsTheChain(t *testing.T) {
	via := make([]*http.Request, maxPeerRedirects)
	for i := range via {
		via[i] = req(t, "http://peer:8081/api/snapshot")
	}

	var redir *ErrPeerRedirect
	if err := checkPeerRedirect(req(t, "http://peer:8081/api/snapshot"), via); !errors.As(err, &redir) {
		t.Fatalf("checkPeerRedirect() at hop %d = %v, want an *ErrPeerRedirect", maxPeerRedirects, err)
	}
}

// TestPollerFetch_RefusesRedirectOffTheConfiguredHost proves the policy is
// wired into the client the poller actually uses, not merely defined. A peer
// that redirects elsewhere must fail the poll rather than silently return the
// other server's snapshot as its own.
func TestPollerFetch_RefusesRedirectOffTheConfiguredHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"host":{"hostname":"impostor"}}`)); err != nil {
			t.Errorf("writing impostor body: %v", err)
		}
	}))
	defer elsewhere.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/snapshot", http.StatusFound)
	}))
	defer peer.Close()

	p := &Poller{
		client: &http.Client{CheckRedirect: checkPeerRedirect},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	snap, err := p.fetch(config.Peer{Name: "peer", URL: peer.URL}, 2*time.Second)
	if err == nil {
		t.Fatalf("fetch() followed the redirect and returned %+v, want it refused", snap)
	}
	var redir *ErrPeerRedirect
	if !errors.As(err, &redir) {
		t.Fatalf("fetch() = %v, want an *ErrPeerRedirect", err)
	}
	if snap != nil {
		t.Errorf("fetch() returned a snapshot %+v alongside the error, want nil", snap)
	}
}
