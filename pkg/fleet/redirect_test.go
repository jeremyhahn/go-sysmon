package fleet

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
)

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
		client: &http.Client{CheckRedirect: outbound.Check},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	snap, err := p.fetch(config.Peer{Name: "peer", URL: peer.URL}, 2*time.Second)
	if err == nil {
		t.Fatalf("fetch() followed the redirect and returned %+v, want it refused", snap)
	}
	var redir *outbound.ErrRedirect
	if !errors.As(err, &redir) {
		t.Fatalf("fetch() = %v, want an *outbound.ErrRedirect", err)
	}
	if snap != nil {
		t.Errorf("fetch() returned a snapshot %+v alongside the error, want nil", snap)
	}
}
