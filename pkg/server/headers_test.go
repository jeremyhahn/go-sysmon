package server_test

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// hsts is the default Strict-Transport-Security value.
var hsts = server.DefaultSecurityHeaders().Get("Strict-Transport-Security")

// getHeaders requests url and returns the response headers, failing the test on a transport error.
func getHeaders(t *testing.T, client *http.Client, url string) http.Header {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Errorf("read body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
	return resp.Header
}

// TestSecurityHeaders_HSTSFollowsTheServersOwnTransport starts the server on
// its own listener in each transport mode it has -- plain, static certificate
// files, and the per-handshake GetConfigForClient hook -- and checks HSTS is
// sent exactly when the connection is TLS, beside the headers every mode sends.
func TestSecurityHeaders_HSTSFollowsTheServersOwnTransport(t *testing.T) {
	cert, leaf := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, cert)

	m := newMonitor(t, "headers-host")
	waitForSnapshot(t, m)

	modes := map[string]struct {
		tls  *server.TLS
		hsts string
	}{
		"plain HTTP": {tls: nil, hsts: ""},
		"static certificate files": {
			tls:  &server.TLS{CertFile: certPath, KeyFile: keyPath},
			hsts: hsts,
		},
		"GetConfigForClient": {
			tls: &server.TLS{
				GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
					return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
				},
			},
			hsts: hsts,
		},
	}

	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			srv, err := server.NewWithConfig(server.Config{Monitor: m, Addr: "127.0.0.1:0", TLS: mode.tls})
			if err != nil {
				t.Fatalf("NewWithConfig: %v", err)
			}
			addr := startTLSServer(t, srv)

			scheme, client := "http://", http.DefaultClient
			if srv.UsesTLS() {
				scheme, client = "https://", clientFor(t, leaf)
			}

			for _, path := range []string{"/api/snapshot", "/"} {
				got := getHeaders(t, client, scheme+addr+path)
				if v := got.Get("Strict-Transport-Security"); v != mode.hsts {
					t.Errorf("%s: Strict-Transport-Security = %q, want %q", path, v, mode.hsts)
				}
				if v := got.Get("X-Content-Type-Options"); v != "nosniff" {
					t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, v)
				}
			}
		})
	}
}

// TestSecurityHeaders_EmbedderOverridesReachTheWire is the documented
// extension point, end to end: a replaced policy, an added header and a removed
// one, through NewWithConfig and ServeHTTP as an embedder mounts it.
func TestSecurityHeaders_EmbedderOverridesReachTheWire(t *testing.T) {
	const framed = "default-src 'self'; frame-ancestors https://dash.example"

	m := newMonitor(t, "embedded-host")
	waitForSnapshot(t, m)

	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		SecurityHeaders: http.Header{
			"Content-Security-Policy":    {framed},
			"X-Robots-Tag":               {"noindex"},
			"Cross-Origin-Opener-Policy": nil,
		},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	got := getHeaders(t, ts.Client(), ts.URL+"/api/version")

	if v := got.Values("Content-Security-Policy"); len(v) != 1 || v[0] != framed {
		t.Errorf("Content-Security-Policy = %q, want only %q", v, framed)
	}
	if v := got.Get("X-Robots-Tag"); v != "noindex" {
		t.Errorf("X-Robots-Tag = %q, want noindex", v)
	}
	if v := got.Get("Cross-Origin-Opener-Policy"); v != "" {
		t.Errorf("Cross-Origin-Opener-Policy = %q, want it removed", v)
	}
	if v := got.Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want the untouched default", v)
	}
}

// TestSecurityHeaders_NewServesTheDefaults covers the constructor with no
// configuration at all: an embedder calling New gets the same headers.
func TestSecurityHeaders_NewServesTheDefaults(t *testing.T) {
	m := newMonitor(t, "new-host")
	waitForSnapshot(t, m)

	ts := httptest.NewServer(server.New(m, ":0", nil))
	t.Cleanup(ts.Close)
	got := getHeaders(t, ts.Client(), ts.URL+"/api/snapshot")

	for name, want := range server.DefaultSecurityHeaders() {
		if name == "Strict-Transport-Security" {
			continue
		}
		if v := got.Get(name); v != want[0] {
			t.Errorf("%s = %q, want %q", name, v, want[0])
		}
	}
}

// TestNewWithConfig_RejectsUnsendableSecurityHeader checks a header that
// cannot be sent faithfully fails construction instead of being mangled on the
// wire -- net/http would otherwise rewrite a CRLF in a value to spaces.
func TestNewWithConfig_RejectsUnsendableSecurityHeader(t *testing.T) {
	m := newMonitor(t, "bad-header-host")

	_, err := server.NewWithConfig(server.Config{
		Monitor:         m,
		SecurityHeaders: http.Header{"Content-Security-Policy": {"default-src 'self'\r\nX-Injected: 1"}},
	})
	var target *types.SecurityHeaderError
	if !errors.As(err, &target) {
		t.Fatalf("err = %v, want *types.SecurityHeaderError", err)
	}
	if target.Name != "Content-Security-Policy" {
		t.Errorf("Name = %q, want Content-Security-Policy", target.Name)
	}
}

// TestDefaultSecurityHeaders_ReturnsACopy lets an embedder edit the result
// freely: doing so must not change what any server sends.
func TestDefaultSecurityHeaders_ReturnsACopy(t *testing.T) {
	first := server.DefaultSecurityHeaders()
	first.Set("Content-Security-Policy", "edited")
	first.Del("X-Content-Type-Options")

	second := server.DefaultSecurityHeaders()
	if v := second.Get("Content-Security-Policy"); v == "edited" {
		t.Error("an edit to one result leaked into the next")
	}
	if v := second.Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", v)
	}
}
