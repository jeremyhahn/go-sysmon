//go:build integration

package server

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestHeaders_EveryResponseCarriesTheSecurityHeaders verifies the policy
// headers reach the browser on every kind of response -- the dashboard, an
// API answer, an error and a refusal -- and that HSTS is not sent over plain
// HTTP, where it would be ignored at best.
func TestHeaders_EveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	want := server.DefaultSecurityHeaders()

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/api/snapshot", ""},
		{http.MethodGet, "/no-such-page", ""},
		{http.MethodPost, "/api/interval", `{"interval_ms":7}`},
	} {
		_, header, _ := request(t, http.DefaultClient, c.method, s.URL(c.path), c.body)
		for name := range want {
			if name == "Strict-Transport-Security" {
				if got := header.Get(name); got != "" {
					t.Errorf("%s %s sent HSTS %q over plain HTTP", c.method, c.path, got)
				}
				continue
			}
			if got := header.Get(name); got != want.Get(name) {
				t.Errorf("%s %s %s = %q, want %q", c.method, c.path, name, got, want.Get(name))
			}
		}
	}
}

// TestAssets_TheDashboardIsServedWithCacheValidation verifies the embedded
// dashboard is served with an ETag a browser can revalidate against, that an
// unchanged page is answered 304, and that content-hashed bundles are cached
// for good while the page naming them is revalidated every time.
func TestAssets_TheDashboardIsServedWithCacheValidation(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})

	status, header, body := request(t, http.DefaultClient, http.MethodGet, s.URL("/"), "")
	if status != http.StatusOK || !strings.Contains(body, `<div id="app">`) {
		t.Fatalf("GET / = %d, want the dashboard page", status)
	}
	etag := header.Get("ETag")
	if etag == "" || !strings.Contains(header.Get("Cache-Control"), "no-cache") {
		t.Errorf("GET / ETag %q Cache-Control %q, want a validator and revalidation", etag, header.Get("Cache-Control"))
	}

	req, err := http.NewRequest(http.MethodGet, s.URL("/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	if status, _, _ := harness.Do(t, http.DefaultClient, req); status != http.StatusNotModified {
		t.Errorf("GET / If-None-Match its own ETag = %d, want 304", status)
	}

	hashed := hashedBundle(t, s)
	_, header, _ = request(t, http.DefaultClient, http.MethodGet, s.URL("/"+hashed), "")
	if got := header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("GET /%s Cache-Control = %q, want an immutable, long-lived bundle", hashed, got)
	}
}

// hashedBundle returns the path of a content-hashed bundle the dashboard page
// preloads. The page names it relative to itself ("./assets/..."), so the
// dashboard also loads under a reverse proxy's sub-path.
func hashedBundle(t *testing.T, s *harness.Server) string {
	t.Helper()
	_, _, page := request(t, http.DefaultClient, http.MethodGet, s.URL("/"), "")
	for _, field := range strings.Split(page, `"`) {
		if strings.HasPrefix(field, "./assets/") && strings.Count(field, "-") > 0 && strings.HasSuffix(field, ".js") {
			return strings.TrimPrefix(field, "./")
		}
	}
	t.Fatalf("the dashboard page names no content-hashed bundle:\n%s", page)
	return ""
}

// TestProxy_ServesThroughAReverseProxy runs the server behind a real reverse
// proxy, as the trusted platform will: the API and the dashboard answer
// through it, and the event stream is delivered event by event rather than
// held in the proxy's buffer.
func TestProxy_ServesThroughAReverseProxy(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	target, err := url.Parse("http://" + s.Addr)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	var version map[string]string
	harness.GetJSON(t, front.URL+"/api/version", &version)
	if version["version"] == "" {
		t.Errorf("/api/version through the proxy = %v", version)
	}

	st := openStream(t, front.URL+"/api/events", streamOptions{client: front.Client()})
	if got := st.header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no: a buffering proxy would hold the stream", got)
	}
	// Two snapshots arrive one after the other: the proxy flushed each.
	for range 2 {
		if ev := st.next(t); ev.name != "snapshot" {
			t.Fatalf("event through the proxy = %q, want snapshot", ev.name)
		}
	}
}
