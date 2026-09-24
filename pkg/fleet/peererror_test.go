package fleet

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
)

// peerAddr is the address every constructed error below is about. It is what
// must never reach a message.
const peerAddr = "198.51.100.10:8081"

// assertNamesNoAddress fails when msg carries any part of a peer's address:
// the URL, host:port, the host, or the port.
func assertNamesNoAddress(t *testing.T, msg, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	for _, leak := range []string{rawURL, u.Host, u.Hostname(), ":" + u.Port()} {
		if leak != "" && leak != ":" && strings.Contains(msg, leak) {
			t.Errorf("message %q carries %q from the peer's address %s", msg, leak, rawURL)
		}
	}
}

// TestPeerFailure_DescribesWithoutAnAddress covers each failure net/http can
// hand back, built the way the transport builds it -- the URL and the address
// dialled wrapped around the cause. Each must still say what happened.
func TestPeerFailure_DescribesWithoutAnAddress(t *testing.T) {
	target := "http://" + peerAddr + "/api/summary"
	tcpAddr, err := net.ResolveTCPAddr("tcp", peerAddr)
	if err != nil {
		t.Fatalf("resolving %s: %v", peerAddr, err)
	}
	dial := func(cause error) error {
		return &url.Error{Op: "Get", URL: target, Err: &net.OpError{
			Op: "dial", Net: "tcp", Addr: tcpAddr, Err: cause,
		}}
	}
	cert := &x509.Certificate{DNSNames: []string{"fs.internal.example"}}

	// The fixture is only a test if the raw text leaks: this is the message
	// the fleet view served before #120.
	if raw := dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)).Error(); !strings.Contains(raw, peerAddr) {
		t.Fatalf("constructed transport error %q does not carry the address; the table below proves nothing", raw)
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"refused", dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), "connection refused"},
		{"no route", dial(os.NewSyscallError("connect", syscall.EHOSTUNREACH)), "no route to host"},
		{"reset mid-body", &net.OpError{Op: "read", Net: "tcp", Addr: tcpAddr,
			Err: os.NewSyscallError("read", syscall.ECONNRESET)}, "connection reset by peer"},
		{"dial timeout", dial(os.ErrDeadlineExceeded), "timed out"},
		{"client deadline", &url.Error{Op: "Get", URL: target, Err: context.DeadlineExceeded}, "timed out"},
		{"canceled", &url.Error{Op: "Get", URL: target, Err: context.Canceled}, "request canceled"},
		{"no such host", dial(&net.DNSError{Err: "no such host", Name: "fs.internal.example",
			Server: "10.0.0.53:53", IsNotFound: true}), "host name not found"},
		{"resolver down", dial(&net.DNSError{Err: "server misbehaving", Name: "fs.internal.example",
			Server: "10.0.0.53:53"}), "host name lookup failed"},
		{"unknown authority", &url.Error{Op: "Get", URL: target,
			Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{Cert: cert}}},
			"certificate signed by an unknown authority"},
		{"wrong host", &url.Error{Op: "Get", URL: target,
			Err: &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: cert, Host: "198.51.100.10"}}},
			"certificate is not valid for the peer's host name"},
		{"expired", &url.Error{Op: "Get", URL: target,
			Err: x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired, Detail: "at " + peerAddr}},
			"certificate has expired or is not yet valid"},
		{"other invalid", &url.Error{Op: "Get", URL: target,
			Err: x509.CertificateInvalidError{Cert: cert, Reason: x509.NameMismatch}},
			"certificate verification failed"},
		{"scheme mismatch", &url.Error{Op: "Get", URL: target, Err: http.ErrSchemeMismatch}, "peer did not answer with TLS"},
		{"record header", &url.Error{Op: "Get", URL: target, Err: tls.RecordHeaderError{Msg: "tls: first record does not look like a TLS handshake"}},
			"peer did not answer with TLS"},
		{"closed", &url.Error{Op: "Get", URL: target, Err: io.EOF}, "connection closed before a complete response"},
		{"truncated body", io.ErrUnexpectedEOF, "connection closed before a complete response"},
		{"not json", &json.SyntaxError{Offset: 1}, "malformed response"},
		{"wrong shape", &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[float64](), Field: "uptime"}, "malformed response"},
		{"unrecognised", errors.New("proxyconnect tcp " + peerAddr + ": something new"), failureUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&PeerRequestError{Peer: "fs", Cause: tt.err}).Error()
			if !strings.Contains(got, tt.want) {
				t.Errorf("Error() = %q, want it to say %q", got, tt.want)
			}
			if !strings.Contains(got, "fs") {
				t.Errorf("Error() = %q, want it to name the peer", got)
			}
			assertNamesNoAddress(t, got, "http://"+peerAddr)
			for _, leak := range []string{"fs.internal.example", "10.0.0.53"} {
				if strings.Contains(got, leak) {
					t.Errorf("Error() = %q carries %q", got, leak)
				}
			}
		})
	}
}

// TestPeerRequestError_KeepsTheChain covers what the redaction must not cost:
// the poller classifies a peer by the error underneath -- a body that is not
// JSON is how an older build without /api/summary shows itself -- and the log
// keeps the whole chain.
func TestPeerRequestError_KeepsTheChain(t *testing.T) {
	cause := &json.SyntaxError{Offset: 3}
	err := error(&PeerRequestError{Peer: "fs", Cause: cause})

	if !endpointNotServed(err) {
		t.Error("endpointNotServed() = false for a wrapped syntax error; the snapshot fallback would never run")
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is() cannot reach the cause through PeerRequestError")
	}
	if isNotFound(err) {
		t.Error("isNotFound() = true for an error that is not a 404")
	}
}

// TestFetch_FailuresNameNoAddress drives real failures through the poller's
// transport -- the path GET /api/fleet, the roll-up and the drill-in all read
// from -- and checks each still says what went wrong without the address.
func TestFetch_FailuresNameNoAddress(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		target := deadpeer.URL(t)
		_, err := fetchSnapshot(testPoller(t), config.Peer{Name: "gone", URL: target}, plaintextAllowed())
		assertPeerFailure(t, err, target, "connection refused")
	})

	t.Run("untrusted certificate", func(t *testing.T) {
		srv := httptest.NewTLSServer(snapshotPeer(t))
		defer srv.Close()
		_, err := fetchSnapshot(testPoller(t), config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed())
		assertPeerFailure(t, err, srv.URL, "certificate")
	})

	t.Run("plain http behind an https url", func(t *testing.T) {
		srv := httptest.NewServer(snapshotPeer(t))
		defer srv.Close()
		target := strings.Replace(srv.URL, "http://", "https://", 1)
		_, err := fetchSnapshot(testPoller(t), config.Peer{Name: "peer", URL: target}, plaintextAllowed())
		assertPeerFailure(t, err, target, "peer did not answer with TLS")
	})

	t.Run("closed without answering", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			if err := conn.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		}))
		defer srv.Close()
		_, err := fetchSnapshot(testPoller(t), config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed())
		assertPeerFailure(t, err, srv.URL, "connection closed before a complete response")
	})

	t.Run("body cut short", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}()
			writeTruncated(t, buf)
		}))
		defer srv.Close()
		_, err := fetchSnapshot(testPoller(t), config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed())
		assertPeerFailure(t, err, srv.URL, "connection closed before a complete response")
	})
}

// TestFetch_MutualTLSRefusalNamesNoAddress is the refusal half of mTLS: the
// peer ends the handshake with an alert, and the reason is the alert's name.
func TestFetch_MutualTLSRefusalNamesNoAddress(t *testing.T) {
	certPEM, _ := selfSigned(t)
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(certPEM) {
		t.Fatal("client CA pool did not accept the generated certificate")
	}
	srv := httptest.NewUnstartedServer(snapshotPeer(t))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	client, err := outbound.NewTLSClient(0, outbound.TLS{CAFile: caFileFor(t, srv)})
	if err != nil {
		t.Fatalf("NewTLSClient() error = %v", err)
	}
	p := testPoller(t)
	p.SetClient(client)

	_, err = fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed())
	assertPeerFailure(t, err, srv.URL, "peer refused the TLS handshake: tls: certificate required")
}

// TestFetch_BadURLNamesNoAddress covers the two configuration errors that
// used to quote the URL: one that will not parse, and one whose "scheme" is
// really the peer's host because the operator wrote host:port.
func TestFetch_BadURLNamesNoAddress(t *testing.T) {
	for _, rawURL := range []string{"http://[fd00::10", "fs.internal.example:8081"} {
		_, err := fetchSnapshot(testPoller(t), config.Peer{Name: "peer", URL: rawURL}, plaintextAllowed())
		if err == nil {
			t.Fatalf("fetch(%q) = nil error, want it refused", rawURL)
		}
		for _, leak := range []string{"fd00::10", "fs.internal.example", "8081"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("fetch(%q) error = %q carries %q", rawURL, err, leak)
			}
		}
		if !strings.Contains(err.Error(), "peer") {
			t.Errorf("fetch(%q) error = %q, want it to name the peer", rawURL, err)
		}
	}
}

// assertPeerFailure checks err is a PeerRequestError that says want and
// carries no part of rawURL.
func assertPeerFailure(t *testing.T, err error, rawURL, want string) {
	t.Helper()
	var reqErr *PeerRequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %T (%v), want *PeerRequestError", err, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to say %q (cause: %v)", err, want, reqErr.Cause)
	}
	assertNamesNoAddress(t, err.Error(), rawURL)
}

// writeTruncated answers 200 promising more body than it sends.
func writeTruncated(t *testing.T, buf *bufio.ReadWriter) {
	t.Helper()
	if _, err := buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"version\":"); err != nil {
		t.Errorf("write: %v", err)
	}
	if err := buf.Flush(); err != nil {
		t.Errorf("flush: %v", err)
	}
}

// TestPeerConfig_FailuresNameNoAddress covers the settings relay's own
// request, which is made outside the poll: a peer that refuses, one that
// answers with something other than a settings view, and a URL that will not
// make a request -- each reported by name alone.
func TestPeerConfig_FailuresNameNoAddress(t *testing.T) {
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "go-sysmon is running"); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer garbage.Close()

	refused := deadpeer.URL(t)

	cfg := config.Fleet{TimeoutMs: 2000, Peers: []config.Peer{
		{Name: "garbage", URL: garbage.URL},
		{Name: "refused", URL: refused},
		{Name: "broken", URL: "http://[fd00::10"},
	}}
	p := New(func() config.Fleet { return cfg }, nil, func() string { return "local" }, testPoller(t).logger)

	_, err := p.PeerConfig(context.Background(), "garbage")
	assertPeerFailure(t, err, garbage.URL, "malformed response")

	_, err = p.PeerConfig(context.Background(), "refused")
	assertPeerFailure(t, err, refused, "connection refused")

	_, err = p.PeerConfig(context.Background(), "broken")
	var bad *ErrPeerURL
	if !errors.As(err, &bad) {
		t.Fatalf("PeerConfig(broken) error = %T (%v), want *ErrPeerURL", err, err)
	}
	if strings.Contains(err.Error(), "fd00::10") {
		t.Errorf("PeerConfig(broken) error = %q carries the address", err)
	}
}
