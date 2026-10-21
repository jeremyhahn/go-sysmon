package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// hstsHeader is the one security header that depends on the request: a
// browser ignores Strict-Transport-Security received over plain HTTP (RFC 6797
// section 8.1), so it is sent only on requests that arrived over TLS, here or
// at a trusted proxy (requestSecure).
const hstsHeader = "Strict-Transport-Security"

// contentSecurityPolicy is fitted to the dashboard and to nothing more.
//
// Scripts and styles load only from this origin, with no inline code of either
// kind: the pre-paint theme bootstrap is a file (theme-init.js) for exactly
// this reason, and chart tooltips are built as DOM nodes styled through the
// CSSOM, which style-src does not govern, rather than as HTML strings carrying
// style attributes, which it does.
//
// img-src also allows data: because daisyUI draws its select arrows, checkbox
// ticks and similar glyphs as data: SVG images from the stylesheet. An image
// cannot run script, so this widens nothing that the policy exists to close.
//
// connect-src 'self' covers the event stream and every API call: the
// dashboard reaches fleet peers only through this server's relay, never
// directly. frame-ancestors 'none' refuses framing outright; an embedder that
// frames the dashboard in its own UI replaces this policy through
// Config.SecurityHeaders.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// deniedFeatures are the powerful browser features the dashboard never uses.
// Each is denied to the page and to anything it could ever embed. Only
// features Chromium recognises are listed: an unrecognised name is not an
// error, but it prints a console warning on every page load, and a warning
// that is always there teaches everyone to ignore the console.
var deniedFeatures = []string{
	"accelerometer",
	"autoplay",
	"browsing-topics",
	"camera",
	"clipboard-read",
	"clipboard-write",
	"display-capture",
	"encrypted-media",
	"fullscreen",
	"gamepad",
	"geolocation",
	"gyroscope",
	"hid",
	"idle-detection",
	"local-fonts",
	"magnetometer",
	"microphone",
	"midi",
	"payment",
	"picture-in-picture",
	"publickey-credentials-create",
	"publickey-credentials-get",
	"screen-wake-lock",
	"serial",
	"storage-access",
	"usb",
	"window-management",
	"xr-spatial-tracking",
}

// permissionsPolicy renders deniedFeatures as a Permissions-Policy value, each
// feature with an empty allowlist.
func permissionsPolicy() string {
	denied := make([]string, len(deniedFeatures))
	for i, feature := range deniedFeatures {
		denied[i] = feature + "=()"
	}
	return strings.Join(denied, ", ")
}

// DefaultSecurityHeaders returns the browser security headers every response
// carries unless Config.SecurityHeaders replaces them. The result is a fresh
// copy: an embedder may modify it -- appending a source to the
// Content-Security-Policy, say -- and pass it back as Config.SecurityHeaders.
//
// Strict-Transport-Security is included, but it is sent only on requests that
// arrived over TLS, whether the server terminates TLS itself or an embedding
// application does and mounts the server as an http.Handler. Behind a reverse
// proxy that terminates TLS the request reaching this server is plain HTTP, so
// it is sent only when the proxy is in Config.TrustedProxies and reports that
// the client used HTTPS; otherwise the proxy is the one that must send it.
func DefaultSecurityHeaders() http.Header {
	return http.Header{
		"Content-Security-Policy":      {contentSecurityPolicy},
		"Cross-Origin-Opener-Policy":   {"same-origin"},
		"Cross-Origin-Resource-Policy": {"same-origin"},
		"Permissions-Policy":           {permissionsPolicy()},
		"Referrer-Policy":              {"no-referrer"},
		hstsHeader:                     {"max-age=63072000"},
		"X-Content-Type-Options":       {"nosniff"},
	}
}

// validateSecurityHeaders reports whether an embedder's overrides can be
// sent. It returns a SecurityHeaderError for a name that is not a valid field
// name, a value carrying a control character, or a header named twice under
// two spellings -- map order would decide which one won, differently on every
// run.
func validateSecurityHeaders(overrides http.Header) error {
	seen := make(map[string]string, len(overrides))
	for name, values := range overrides {
		if !httpguts.ValidHeaderFieldName(name) {
			return &types.SecurityHeaderError{Name: name, Reason: "invalid header name"}
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return &types.SecurityHeaderError{Name: name, Reason: "invalid header value"}
			}
		}
		key := http.CanonicalHeaderKey(name)
		if other, dup := seen[key]; dup {
			return &types.SecurityHeaderError{Name: name, Reason: "named twice, also as " + strconv.Quote(other)}
		}
		seen[key] = name
	}
	return nil
}

// mergeSecurityHeaders applies an embedder's overrides, already validated, to
// the defaults.
//
// Each header named in overrides replaces the default of the same name, or is
// added when there is none. A header named with no values is removed. Names
// are compared canonically, so "content-security-policy" replaces the default
// policy rather than being sent beside it.
//
// Every value slice in the result has its capacity clipped to its length. The
// slices are shared by every response, and a handler appending to its own
// response's header would otherwise write into the next response's.
func mergeSecurityHeaders(overrides http.Header) http.Header {
	headers := DefaultSecurityHeaders()
	for name, values := range overrides {
		key := http.CanonicalHeaderKey(name)
		if len(values) == 0 {
			delete(headers, key)
			continue
		}
		headers[key] = slices.Clip(slices.Clone(values))
	}
	return headers
}

// securityHeaders sets headers on every response next writes, before next
// runs, so a handler can still set anything it owns -- a Content-Type, a
// Cache-Control -- and nothing it forgets can leave a response without them.
//
// It wraps the whole mux rather than individual routes: a route added later,
// the mux's own 404 and 405 answers, the static assets and the event stream
// all pass through it, and none of them has to remember anything. It sets
// headers and nothing else, so the ResponseWriter the handler receives is the
// server's own, with its flushing and deadlines intact for the event stream.
func securityHeaders(headers http.Header, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for name, values := range headers {
			if name == hstsHeader && !requestSecure(r) {
				continue
			}
			h[name] = values
		}
		next.ServeHTTP(w, r)
	})
}
