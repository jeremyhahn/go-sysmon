package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// forwarding is one request through the forwarding and security header
// middleware: who sent it, what it carried, and what came out.
type forwarding struct {
	peer    string
	headers http.Header
}

// seen is what the handler behind the middleware observed.
type seen struct {
	client string
	hsts   bool
}

// serveForwarding runs req through forwardedHeaders and securityHeaders, as
// newServer chains them, and reports the logged client and whether HSTS was
// sent.
func serveForwarding(t *testing.T, trusted []string, req forwarding, onTLS bool) seen {
	t.Helper()
	var prefixes trustedProxies
	for _, cidr := range trusted {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	var got seen
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.client = clientAddr(r)
		w.WriteHeader(http.StatusOK)
	})
	handler := forwardedHeaders(prefixes, securityHeaders(DefaultSecurityHeaders(), inner))

	r := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	r.RemoteAddr = req.peer
	for name, values := range req.headers {
		r.Header[name] = values
	}
	if onTLS {
		r.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: forwarding headers must never fail a request", w.Code)
	}
	got.hsts = w.Header().Get(hstsHeader) != ""
	return got
}

// spoofed is every forwarding header claiming an HTTPS client at 203.0.113.66.
var spoofed = http.Header{
	"Forwarded":         {`for=203.0.113.66;proto=https`},
	"X-Forwarded-For":   {"203.0.113.66"},
	"X-Forwarded-Proto": {"https"},
}

func TestForwardedHeaders_UntrustedPeerChangesNothing(t *testing.T) {
	cases := map[string][]string{
		"no proxies trusted":           nil,
		"the peer is outside the list": {"10.0.0.0/8"},
	}
	for name, trusted := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveForwarding(t, trusted, forwarding{peer: "198.51.100.7:5555", headers: spoofed}, false)
			if got.client != "198.51.100.7:5555" || got.hsts {
				t.Errorf("got client %q, hsts %v; want the peer's address and no HSTS", got.client, got.hsts)
			}
		})
	}
}

func TestForwardedHeaders_TrustedPeerSetsSchemeAndClient(t *testing.T) {
	cases := map[string]http.Header{
		"Forwarded": {"Forwarded": {`for=203.0.113.66;proto=https`}},
		"X-Forwarded-*": {
			"X-Forwarded-For":   {"203.0.113.66"},
			"X-Forwarded-Proto": {"https"},
		},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, false)
			if got.client != "203.0.113.66" || !got.hsts {
				t.Errorf("got client %q, hsts %v; want 203.0.113.66 and HSTS", got.client, got.hsts)
			}
		})
	}
}

// TestForwardedHeaders_PlainHTTPClientGetsNoHSTS: a trusted proxy reporting
// http, or naming the client without a scheme, leaves HSTS off.
func TestForwardedHeaders_PlainHTTPClientGetsNoHSTS(t *testing.T) {
	for _, headers := range []http.Header{
		{"Forwarded": {`for=203.0.113.66;proto=http`}},
		{"Forwarded": {`for=203.0.113.66`}},
		{"X-Forwarded-For": {"203.0.113.66"}},
		{"X-Forwarded-Proto": {"http"}},
	} {
		got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, false)
		if got.hsts {
			t.Errorf("%v: HSTS sent for a client that did not use HTTPS", headers)
		}
	}
}

// TestForwardedHeaders_TLSHereAlwaysSendsHSTS: a request that reached this
// server over TLS is secure whatever a proxy says about the hop before.
func TestForwardedHeaders_TLSHereAlwaysSendsHSTS(t *testing.T) {
	headers := http.Header{"X-Forwarded-Proto": {"http"}}
	got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, true)
	if !got.hsts {
		t.Error("HSTS not sent on a request that arrived over TLS")
	}
}

func TestForwardedHeaders_MultiHopTakesTheRightMostUntrusted(t *testing.T) {
	trusted := []string{"10.0.0.0/8", "fd00::/8"}
	cases := map[string]struct {
		headers http.Header
		want    string
	}{
		// The client wrote 192.0.2.1 itself; the first proxy appended the
		// address it really came from.
		"X-Forwarded-For": {
			http.Header{"X-Forwarded-For": {"192.0.2.1, 203.0.113.66, 10.0.0.9, fd00::2"}},
			"203.0.113.66",
		},
		"X-Forwarded-For over several lines": {
			http.Header{"X-Forwarded-For": {"192.0.2.1, 203.0.113.66", "10.0.0.9"}},
			"203.0.113.66",
		},
		"X-Forwarded-For with ports": {
			http.Header{"X-Forwarded-For": {"203.0.113.66:5123, [fd00::2]:443"}},
			"203.0.113.66",
		},
		"every hop trusted": {
			http.Header{"X-Forwarded-For": {"10.0.0.3, 10.0.0.9"}},
			"10.0.0.3",
		},
		"Forwarded": {
			http.Header{"Forwarded": {`for=192.0.2.1;proto=http, for=203.0.113.66;proto=https, for=10.0.0.9;proto=http`}},
			"203.0.113.66",
		},
		"Forwarded stops at a hop with no address": {
			http.Header{"Forwarded": {`for=192.0.2.1, for=unknown, for=10.0.0.9`}},
			"10.0.0.5:4000",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveForwarding(t, trusted, forwarding{peer: "10.0.0.5:4000", headers: tc.headers}, false)
			if got.client != tc.want {
				t.Errorf("client = %q, want %q", got.client, tc.want)
			}
		})
	}
}

// TestForwardedHeaders_ForwardedSchemeIsTheChosenHops: proto is read from the
// element that names the client, which says how the client connected; a
// later hop between proxies being plain HTTP does not matter.
func TestForwardedHeaders_ForwardedSchemeIsTheChosenHops(t *testing.T) {
	headers := http.Header{"Forwarded": {`for=203.0.113.66;proto=https, for=10.0.0.9;proto=http`}}
	got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, false)
	if !got.hsts {
		t.Error("HSTS not sent although the client's own hop was HTTPS")
	}
}

func TestForwardedHeaders_ForwardedBeatsXForwarded(t *testing.T) {
	headers := http.Header{
		"Forwarded":         {`for=203.0.113.66;proto=https`},
		"X-Forwarded-For":   {"192.0.2.99"},
		"X-Forwarded-Proto": {"http"},
	}
	got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, false)
	if got.client != "203.0.113.66" || !got.hsts {
		t.Errorf("got client %q, hsts %v; want Forwarded's 203.0.113.66 and HSTS", got.client, got.hsts)
	}
}

// TestForwardedHeaders_MalformedForwardedFallsBack: an unusable Forwarded is
// as good as absent, so the X-Forwarded-* headers are read instead.
func TestForwardedHeaders_MalformedForwardedFallsBack(t *testing.T) {
	headers := http.Header{
		"Forwarded":         {`for="203.0.113.66`},
		"X-Forwarded-For":   {"192.0.2.99"},
		"X-Forwarded-Proto": {"https"},
	}
	got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, false)
	if got.client != "192.0.2.99" || !got.hsts {
		t.Errorf("got client %q, hsts %v; want X-Forwarded-For's 192.0.2.99 and HSTS", got.client, got.hsts)
	}
}

func TestForwardedHeaders_MalformedAndOversizedAreIgnored(t *testing.T) {
	long := strings.Repeat("203.0.113.66, ", maxForwardingHeaderLen/len("203.0.113.66, ")+1)
	cases := map[string]http.Header{
		"Forwarded quote left open":        {"Forwarded": {`for="[2001:db8::1];proto=https`}},
		"Forwarded pair without a value":   {"Forwarded": {`for;proto=https`}},
		"Forwarded space around equals":    {"Forwarded": {`for = 203.0.113.66;proto=https`}},
		"Forwarded unquoted address:port":  {"Forwarded": {`for=203.0.113.66:80;proto=https`}},
		"Forwarded parameter named twice":  {"Forwarded": {`for=203.0.113.66;proto=https;Proto=https`}},
		"Forwarded stray quote in a value": {"Forwarded": {`for="203.0.113"66";proto=https`}},
		"Forwarded escape at the end":      {"Forwarded": {`for="203.0.113.66\";proto=https`}},
		"X-Forwarded-For a host name":      {"X-Forwarded-For": {"203.0.113.66, proxy.internal"}},
		"X-Forwarded-For only commas":      {"X-Forwarded-For": {" , ,"}},
		"X-Forwarded-Proto not a token":    {"X-Forwarded-Proto": {"https://"}},
		"Forwarded oversized":              {"Forwarded": {"for=203.0.113.66;proto=https, " + long}},
		"X-Forwarded-For oversized":        {"X-Forwarded-For": {long}, "X-Forwarded-Proto": {"https, " + long}},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveForwarding(t, []string{"10.0.0.0/8"}, forwarding{peer: "10.0.0.5:4000", headers: headers}, false)
			if got.client != "10.0.0.5:4000" || got.hsts {
				t.Errorf("got client %q, hsts %v; want the header ignored", got.client, got.hsts)
			}
		})
	}
}

func TestForwardedHeaders_IPv6(t *testing.T) {
	cases := map[string]struct {
		peer    string
		headers http.Header
		want    string
	}{
		"IPv6 peer, bracketed node with a port": {
			"[2001:db8::10]:443",
			http.Header{"Forwarded": {`For="[2001:db8:cafe::17]:4711";PROTO=https`}},
			"2001:db8:cafe::17",
		},
		"IPv6 peer, X-Forwarded-For": {
			"[2001:db8::10]:443",
			http.Header{"X-Forwarded-For": {"2001:db8:cafe::17"}, "X-Forwarded-Proto": {"https"}},
			"2001:db8:cafe::17",
		},
		"IPv4-mapped peer on a dual-stack listener": {
			"[::ffff:10.0.0.5]:4000",
			http.Header{"Forwarded": {`for="[2001:db8:cafe::17]";proto=https`}},
			"2001:db8:cafe::17",
		},
		"zoned link-local peer": {
			"[fe80::1%eth0]:4000",
			http.Header{"Forwarded": {`for=203.0.113.66;proto=https`}},
			"203.0.113.66",
		},
		"escaped quoted node": {
			"10.0.0.5:4000",
			http.Header{"Forwarded": {`for="\[2001:db8:cafe::17\]";proto=https`}},
			"2001:db8:cafe::17",
		},
	}
	trusted := []string{"10.0.0.0/8", "2001:db8::/112", "fe80::/10"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveForwarding(t, trusted, forwarding{peer: tc.peer, headers: tc.headers}, false)
			if got.client != tc.want || !got.hsts {
				t.Errorf("got client %q, hsts %v; want %q and HSTS", got.client, got.hsts, tc.want)
			}
		})
	}
}

// TestForwardedHeaders_UnparsablePeerIsUntrusted covers a RemoteAddr that is
// not an address and port -- an embedder's synthetic request, say -- which
// must not be mistaken for a trusted proxy.
func TestForwardedHeaders_UnparsablePeerIsUntrusted(t *testing.T) {
	for _, peer := range []string{"", "pipe", "10.0.0.5"} {
		got := serveForwarding(t, []string{"0.0.0.0/0"}, forwarding{peer: peer, headers: spoofed}, false)
		if got.client != peer || got.hsts {
			t.Errorf("peer %q: got client %q, hsts %v; want the headers ignored", peer, got.client, got.hsts)
		}
	}
}

func TestParseForwarded(t *testing.T) {
	addr := netip.MustParseAddr
	cases := map[string][]forwardedElement{
		`for=192.0.2.60;proto=http;by=203.0.113.43`: {{node: addr("192.0.2.60"), proto: "http"}},
		`for="192.0.2.60:443", , for=_hidden`:       {{node: addr("192.0.2.60")}, {}},
		`for="a,b;c=d", proto=https`:                {{}, {proto: "https"}},
		`;for=192.0.2.60;`:                          {{node: addr("192.0.2.60")}},
		`for="[2001:db8::1]:x"`:                     {{node: addr("2001:db8::1")}},
		`for="[2001:db8::1]junk"`:                   {{}},
		`for="[192.0.2.60]"`:                        {{}},
		`for="2001:db8::1"`:                         {{}},
	}
	for header, want := range cases {
		got, ok := parseForwarded(header)
		if !ok || len(got) != len(want) {
			t.Errorf("parseForwarded(%q) = %v, %v; want %v", header, got, ok, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseForwarded(%q)[%d] = %+v, want %+v", header, i, got[i], want[i])
			}
		}
	}
}
