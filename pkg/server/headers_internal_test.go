package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// alwaysSent is every default security header except HSTS, which depends on
// the transport and is asserted separately.
func alwaysSent() http.Header {
	h := DefaultSecurityHeaders()
	h.Del(hstsHeader)
	return h
}

// routeRequest builds a request for a ServeMux pattern: the method and path it
// names, with every wildcard filled in and a body where the route reads one.
func routeRequest(t *testing.T, ctx context.Context, baseURL, pattern string) *http.Request {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		t.Fatalf("pattern %q has no method", pattern)
	}
	path = strings.ReplaceAll(path, "{name}", "peer")

	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"interval_ms":1000}`)
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		t.Fatalf("request for %q: %v", pattern, err)
	}
	return req
}

// fetchHeaders sends req and returns the response headers exactly as they
// arrived on the wire. An ordinary body is read to the end, so the server is
// never left blocked writing to a client that stopped listening; the event
// stream's is abandoned, since it never ends.
func fetchHeaders(t *testing.T, client *http.Client, req *http.Request) http.Header {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Errorf("%s %s: read body: %v", req.Method, req.URL.Path, err)
		}
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("%s %s: close body: %v", req.Method, req.URL.Path, err)
	}
	return resp.Header
}

// assertSecurityHeaders fails for every default header got lacks or carries
// with a different value, and for HSTS present when it must not be or absent
// when it must be.
func assertSecurityHeaders(t *testing.T, label string, got http.Header, wantHSTS bool) {
	t.Helper()
	for name, want := range alwaysSent() {
		if v := got.Values(name); strings.Join(v, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %s = %q, want %q", label, name, v, want)
		}
	}
	hsts := got.Get(hstsHeader)
	switch {
	case wantHSTS && hsts != DefaultSecurityHeaders().Get(hstsHeader):
		t.Errorf("%s: %s = %q over TLS, want %q", label, hstsHeader, hsts, DefaultSecurityHeaders().Get(hstsHeader))
	case !wantHSTS && hsts != "":
		t.Errorf("%s: %s = %q over plain HTTP, want it absent", label, hstsHeader, hsts)
	}
}

// walkRoutes requests every route srv registers, plus the answers no route
// owns -- the mux's 405 and a missing file -- and asserts each carries the
// security headers.
func walkRoutes(t *testing.T, srv *Server, ts *httptest.Server, wantHSTS bool) {
	t.Helper()
	routes := srv.routes()

	// Guard the guard: a table that came back empty, or without the stream,
	// would pass every assertion below by walking nothing.
	for _, must := range []string{"GET /api/events", "GET /api/snapshot", "GET /"} {
		if _, ok := routes[must]; !ok {
			t.Fatalf("route table has no %q; the walk would prove nothing", must)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := ts.Client()
	for pattern := range routes {
		req := routeRequest(t, ctx, ts.URL, pattern)
		assertSecurityHeaders(t, pattern, fetchHeaders(t, client, req), wantHSTS)
	}

	unowned := map[string]string{
		"405 from the mux":  "DELETE /api/snapshot",
		"404 from the root": "GET /no/such/file.js",
	}
	for label, pattern := range unowned {
		req := routeRequest(t, ctx, ts.URL, pattern)
		assertSecurityHeaders(t, label, fetchHeaders(t, client, req), wantHSTS)
	}
}

// serverShapes are the two ways GET / is answered: a built dashboard, or the
// plain status page an embedder without assets gets.
func serverShapes(t *testing.T) map[string]*Server {
	t.Helper()
	mon := startedMonitor(t, idleSnapshotter{}, 20*time.Millisecond)
	assets := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>t</title>")},
		"theme-init.js":        {Data: []byte("void 0;")},
		"assets/index.js":      {Data: []byte("void 0;")},
		"assets/index.css":     {Data: []byte("body{}")},
		"assets/x-abcdefgh.js": {Data: []byte("void 0;")},
	}
	return map[string]*Server{
		"with assets":    newServer(Config{Monitor: mon, Assets: assets}, nil),
		"without assets": newServer(Config{Monitor: mon}, nil),
	}
}

// TestEveryRouteCarriesSecurityHeaders walks the whole route table over plain
// HTTP: JSON, the event stream, the scrape endpoint, static assets and the
// status page all carry the headers, and none carries HSTS.
func TestEveryRouteCarriesSecurityHeaders(t *testing.T) {
	for shape, srv := range serverShapes(t) {
		t.Run(shape, func(t *testing.T) {
			ts := httptest.NewServer(srv)
			defer ts.Close()
			walkRoutes(t, srv, ts, false)
		})
	}
}

// TestEveryRouteCarriesHSTSOverTLS walks the same table over TLS terminated in
// front of the handler, which is where an embedder mounting the server on its
// own HTTPS listener puts it. Every route then also carries HSTS.
func TestEveryRouteCarriesHSTSOverTLS(t *testing.T) {
	for shape, srv := range serverShapes(t) {
		t.Run(shape, func(t *testing.T) {
			ts := httptest.NewTLSServer(srv)
			defer ts.Close()
			walkRoutes(t, srv, ts, true)
		})
	}
}

// TestSecurityHeaders_HandlerMayStillSetItsOwn checks the middleware runs
// before the handler, not after: a route keeps the Content-Type it sets, and
// the event stream keeps its streaming headers.
func TestSecurityHeaders_HandlerMayStillSetItsOwn(t *testing.T) {
	srv := serverShapes(t)["with assets"]
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := map[string]string{
		"GET /api/version":      "application/json",
		"GET /api/events":       "text/event-stream",
		"GET /assets/index.css": "text/css; charset=utf-8",
	}
	for pattern, contentType := range want {
		got := fetchHeaders(t, ts.Client(), routeRequest(t, ctx, ts.URL, pattern))
		if ct := got.Get("Content-Type"); ct != contentType {
			t.Errorf("%s: Content-Type = %q, want %q", pattern, ct, contentType)
		}
	}
}

// TestMergeSecurityHeaders covers the embedder's extension point: replacing a
// default, adding a header, removing one, and matching names canonically.
func TestMergeSecurityHeaders(t *testing.T) {
	const framedPolicy = "default-src 'self'; frame-ancestors https://dash.example"
	tests := map[string]struct {
		overrides http.Header
		check     func(t *testing.T, got http.Header)
	}{
		"nil keeps every default": {
			overrides: nil,
			check: func(t *testing.T, got http.Header) {
				assertSecurityHeaders(t, "merged", got, true)
			},
		},
		"a named header replaces the default": {
			overrides: http.Header{"Content-Security-Policy": {framedPolicy}},
			check: func(t *testing.T, got http.Header) {
				if v := got.Values("Content-Security-Policy"); len(v) != 1 || v[0] != framedPolicy {
					t.Errorf("Content-Security-Policy = %q, want only %q", v, framedPolicy)
				}
			},
		},
		"a non-canonical name replaces rather than duplicates": {
			overrides: http.Header{"content-security-policy": {framedPolicy}},
			check: func(t *testing.T, got http.Header) {
				if v := got.Values("Content-Security-Policy"); len(v) != 1 || v[0] != framedPolicy {
					t.Errorf("Content-Security-Policy = %q, want only %q", v, framedPolicy)
				}
				// A lowercase key kept beside the canonical one would be one
				// header too many, sent twice under two spellings.
				if len(got) != len(DefaultSecurityHeaders()) {
					t.Errorf("merged has %d headers, want %d: %v", len(got), len(DefaultSecurityHeaders()), got)
				}
			},
		},
		"a new header is added beside the defaults": {
			overrides: http.Header{"X-Frame-Options": {"DENY"}},
			check: func(t *testing.T, got http.Header) {
				if v := got.Get("X-Frame-Options"); v != "DENY" {
					t.Errorf("X-Frame-Options = %q, want DENY", v)
				}
				assertSecurityHeaders(t, "merged", got, true)
			},
		},
		"a header with no values is removed": {
			overrides: http.Header{"Cross-Origin-Opener-Policy": {}},
			check: func(t *testing.T, got http.Header) {
				if _, ok := got["Cross-Origin-Opener-Policy"]; ok {
					t.Error("Cross-Origin-Opener-Policy survived an empty override")
				}
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateSecurityHeaders(tc.overrides); err != nil {
				t.Fatalf("validateSecurityHeaders: %v", err)
			}
			tc.check(t, mergeSecurityHeaders(tc.overrides))
		})
	}
}

// TestMergeSecurityHeaders_ValuesAreNotShared pins the aliasing guards. The
// merged slices are shared by every response, so each must be its own copy of
// the caller's override, and full to capacity so that a handler appending to
// its own response's header allocates rather than writing into the next one's.
func TestMergeSecurityHeaders_ValuesAreNotShared(t *testing.T) {
	overrides := http.Header{"X-Extra": {"one"}}
	merged := mergeSecurityHeaders(overrides)

	overrides["X-Extra"][0] = "changed"
	if got := merged.Get("X-Extra"); got != "one" {
		t.Errorf("X-Extra = %q after the caller edited its override, want %q", got, "one")
	}
	for name, values := range merged {
		if cap(values) != len(values) {
			t.Errorf("%s: cap = %d, len = %d; an append would write into shared memory", name, cap(values), len(values))
		}
	}
}

// TestValidateSecurityHeaders_Rejects covers every override that cannot be
// sent faithfully.
func TestValidateSecurityHeaders_Rejects(t *testing.T) {
	tests := map[string]struct {
		overrides http.Header
		reason    string
	}{
		"invalid name":         {http.Header{"Bad Name": {"x"}}, "invalid header name"},
		"empty name":           {http.Header{"": {"x"}}, "invalid header name"},
		"CRLF in a value":      {http.Header{"X-Foo": {"a\r\nSet-Cookie: s=1"}}, "invalid header value"},
		"NUL in a value":       {http.Header{"X-Foo": {"a\x00b"}}, "invalid header value"},
		"one header two cases": {http.Header{"X-Foo": {"a"}, "x-foo": {"b"}}, "named twice"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateSecurityHeaders(tc.overrides)
			var target *types.SecurityHeaderError
			if !errors.As(err, &target) {
				t.Fatalf("err = %v, want *types.SecurityHeaderError", err)
			}
			if !strings.HasPrefix(target.Reason, tc.reason) {
				t.Errorf("Reason = %q, want prefix %q", target.Reason, tc.reason)
			}
		})
	}
}

// TestPermissionsPolicyDeniesEveryListedFeature pins the rendering: every
// listed feature with an empty allowlist, nothing else.
func TestPermissionsPolicyDeniesEveryListedFeature(t *testing.T) {
	policy := permissionsPolicy()
	entries := strings.Split(policy, ", ")
	if len(entries) != len(deniedFeatures) {
		t.Fatalf("policy has %d entries, want %d: %q", len(entries), len(deniedFeatures), policy)
	}
	for i, entry := range entries {
		if want := deniedFeatures[i] + "=()"; entry != want {
			t.Errorf("entry %d = %q, want %q", i, entry, want)
		}
	}
}

// TestContentSecurityPolicyIsStrict pins the directives the dashboard's safety
// rests on, so loosening one is a failing test rather than a quiet edit.
func TestContentSecurityPolicyIsStrict(t *testing.T) {
	directives := make(map[string]string)
	for _, d := range strings.Split(contentSecurityPolicy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	want := map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"style-src":       "'self'",
		"img-src":         "'self' data:",
		"connect-src":     "'self'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"form-action":     "'self'",
		"frame-ancestors": "'none'",
	}
	for name, value := range want {
		if got, ok := directives[name]; !ok || got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for name := range directives {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected directive %q; add it to this test deliberately", name)
		}
	}
	for _, loose := range []string{"'unsafe-inline'", "'unsafe-eval'", "*"} {
		if strings.Contains(contentSecurityPolicy, loose) {
			t.Errorf("policy contains %s", loose)
		}
	}
}
