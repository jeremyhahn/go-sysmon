package prom

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
)

// Both outbound clients in this package are pointed at a URL an operator wrote
// in a config file, and both treat the exchange as authoritative: one hands
// this node's metrics over, the other draws a chart of the past from what comes
// back. A default http.Client follows redirects anywhere, so either could be
// walked to a different host, or from https down to plaintext, with the result
// still attributed to the configured endpoint.
//
// These are end-to-end through a real listener rather than unit tests of the
// policy -- pkg/outbound covers the policy itself. What is being checked here
// is that the clients are BUILT with it, which is exactly what was missing:
// the policy existed in this repository and these two did not use it.

// redirectingTo returns a server that answers every request with a 302 to dst.
func redirectingTo(t *testing.T, dst string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWriter_RefusesRedirectOffTheConfiguredHost(t *testing.T) {
	// Somewhere the operator did not configure, which would happily accept the
	// metrics if they arrived.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer elsewhere.Close()

	configured := redirectingTo(t, elsewhere.URL+"/api/v1/write")

	w := NewWriter(configured.URL, 5*time.Second)
	err := w.Write(context.Background(), goldenSeries())
	if err == nil {
		t.Fatal("Write() = nil; the samples were delivered to a host that was never configured")
	}

	var redirect *outbound.ErrRedirect
	if !errors.As(err, &redirect) {
		t.Fatalf("Write() = %v, want an *outbound.ErrRedirect", err)
	}
	if redirect.Reason != "redirect leaves the configured host" {
		t.Errorf("Reason = %q", redirect.Reason)
	}
}

func TestQuerier_RefusesRedirectOffTheConfiguredHost(t *testing.T) {
	// A well-formed answer from the wrong place is the dangerous case: it
	// parses, and the chart drawn from it looks exactly like this node's past.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer elsewhere.Close()

	configured := redirectingTo(t, elsewhere.URL+"/api/v1/query_range")

	q := NewQuerier(func() QueryOptions {
		return QueryOptions{BaseURL: configured.URL, Timeout: 5 * time.Second}
	})
	_, err := q.QueryRange(context.Background(), "up",
		time.UnixMilli(0), time.UnixMilli(60000), time.Second)
	if err == nil {
		t.Fatal("QueryRange() = nil; history was read from a host that was never configured")
	}

	var redirect *outbound.ErrRedirect
	if !errors.As(err, &redirect) {
		t.Fatalf("QueryRange() = %v, want an *outbound.ErrRedirect", err)
	}
}

// TestOutboundClients_RefuseAnHTTPSDowngrade covers the other half of the
// policy. A cross-host redirect is the obvious attack; quietly stepping down to
// plaintext on the same host is the quiet one, and it puts every sample this
// node writes on the wire in clear.
func TestOutboundClients_RefuseAnHTTPSDowngrade(t *testing.T) {
	err := outbound.Check(
		mustRequest(t, "http://prom.internal:9090/api/v1/write"),
		[]*http.Request{mustRequest(t, "https://prom.internal:9090/api/v1/write")},
	)
	var redirect *outbound.ErrRedirect
	if !errors.As(err, &redirect) {
		t.Fatalf("Check() = %v, want an *outbound.ErrRedirect", err)
	}

	// And an upgrade on the same host is allowed: that is an ingress enforcing
	// TLS, and refusing it would break a real deployment to prevent nothing.
	if err := outbound.Check(
		mustRequest(t, "https://prom.internal:9090/api/v1/write"),
		[]*http.Request{mustRequest(t, "http://prom.internal:9090/api/v1/write")},
	); err != nil {
		t.Fatalf("Check() refused an http->https upgrade on the same host: %v", err)
	}
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("building request for %s: %v", raw, err)
	}
	return req
}

// TestWriter_RefusesASameHostRedirectThatDropsTheBody is the case the host and
// scheme rules do not cover, and the only one of the three that loses data
// silently.
//
// Go implements RFC 9110 §15.4: a 301, 302 or 303 answering a POST is followed
// with a GET and no body. So a receiver -- or a proxy in front of one -- that
// answers /api/v1/write with "302 -> /" turns a batch of samples into a fetch
// of a landing page. The page returns 200, Write returns nil, and Exporter
// removes the batch and counts it as written. Same host, same scheme, no error
// anywhere: the samples are simply gone, and the only visible symptom is a gap
// in Grafana weeks later.
func TestWriter_RefusesASameHostRedirectThatDropsTheBody(t *testing.T) {
	var landingMethod atomic.Value
	var landingBody atomic.Int64
	landingBody.Store(-1)

	// One server, so this cannot be mistaken for the cross-host rule doing the
	// work: the redirect never leaves the configured host.
	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/write" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		landingMethod.Store(r.Method)
		landingBody.Store(int64(len(body)))
		w.WriteHeader(http.StatusOK) // "Success." As far as a caller can tell.
	}))
	defer configured.Close()

	w := NewWriter(configured.URL+"/api/v1/write", 5*time.Second)
	err := w.Write(context.Background(), goldenSeries())
	if err == nil {
		t.Fatalf("Write() = nil, but the samples were never posted anywhere: "+
			"the redirect was followed as a %v carrying %d bytes",
			landingMethod.Load(), landingBody.Load())
	}

	var redirect *outbound.ErrRedirect
	if !errors.As(err, &redirect) {
		t.Fatalf("Write() = %v, want an *outbound.ErrRedirect", err)
	}
	if got := landingMethod.Load(); got != nil {
		t.Errorf("the redirect target was requested with %v; it should not have been requested at all", got)
	}
}

// TestWriter_FollowsASameHostRedirectThatKeepsThePost is the other half, and
// the reason the rule is "the method must not change" rather than "no
// redirects". 307 and 308 preserve the method and Go replays the body, so an
// ingress moving the write path is a real deployment that must keep working --
// and refusing every redirect to fix the 302 case would break it.
func TestWriter_FollowsASameHostRedirectThatKeepsThePost(t *testing.T) {
	var gotMethod atomic.Value
	var gotBytes atomic.Int64

	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/write" {
			http.Redirect(w, r, "/api/v2/write", http.StatusTemporaryRedirect)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		gotMethod.Store(r.Method)
		gotBytes.Store(int64(len(body)))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer configured.Close()

	w := NewWriter(configured.URL+"/api/v1/write", 5*time.Second)
	if err := w.Write(context.Background(), goldenSeries()); err != nil {
		t.Fatalf("Write() = %v, want the 307 followed", err)
	}
	if got := gotMethod.Load(); got != http.MethodPost {
		t.Errorf("the redirect target saw method %v, want POST", got)
	}
	if gotBytes.Load() == 0 {
		t.Error("the redirect target received an empty body; the samples were not replayed")
	}
}
