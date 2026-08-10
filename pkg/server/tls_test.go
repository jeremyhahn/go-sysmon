package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- certificate helpers ------------------------------------------------

// selfSigned builds an in-memory certificate valid for localhost and the given
// SNI names. Nothing touches the host: the key pair lives only in this test.
func selfSigned(t *testing.T, names ...string) (tls.Certificate, *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "go-sysmon test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              append([]string{"localhost"}, names...),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, leaf
}

// writeKeyPair writes cert as PEM files in a temp dir and returns their paths.
func writeKeyPair(t *testing.T, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Certificate[0],
	})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	der, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certPath, keyPath
}

// clientFor returns an HTTPS client that trusts only leaf.
func clientFor(t *testing.T, leaf *x509.Certificate) *http.Client {
	t.Helper()

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// startTLSServer starts srv on addr and stops it when the test ends.
func startTLSServer(t *testing.T, srv *server.Server, addr string) {
	t.Helper()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Start() = %v, want nil after a clean Stop", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Start did not return after Stop")
		}
	})

	if !waitForServer(addr, 3*time.Second) {
		t.Fatal("TLS server did not start listening")
	}
}

// ---- NewWithConfig ------------------------------------------------------

// TestNewWithConfig_NoTLSServesPlainHTTP verifies the config constructor is a
// drop-in for New when no transport security is requested.
func TestNewWithConfig_NoTLSServesPlainHTTP(t *testing.T) {
	m := newMonitor(t, "plain-host")
	waitForSnapshot(t, m)

	srv, err := server.NewWithConfig(server.Config{Monitor: m, Addr: ":0"})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
}

// ---- static certificate files -------------------------------------------

func TestTLS_StaticCertFilesServeHTTPS(t *testing.T) {
	cert, leaf := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, cert)

	m := newMonitor(t, "tls-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS:     &server.TLS{CertFile: certPath, KeyFile: keyPath},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}

	startTLSServer(t, srv, addr)

	resp, err := clientFor(t, leaf).Get("https://" + addr + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("response carried no TLS state; the connection was not encrypted")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version = %x, want at least TLS 1.2", resp.TLS.Version)
	}
}

// TestTLS_PlainHTTPRejectedOnTLSListener confirms the listener really is
// speaking TLS rather than silently accepting cleartext.
func TestTLS_PlainHTTPRejectedOnTLSListener(t *testing.T) {
	cert, _ := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, cert)

	m := newMonitor(t, "cleartext-host")
	addr := freeAddr(t)

	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS:     &server.TLS{CertFile: certPath, KeyFile: keyPath},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	// Go answers a cleartext request on a TLS listener with a plain 400 rather
	// than dropping it, so the meaningful assertion is that the request is
	// refused, not that the transport errors.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/snapshot")
	if err != nil {
		return // refused at the transport layer, which is also acceptable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("plain HTTP request was served by a TLS listener")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a cleartext request to an HTTPS server", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "HTTPS") {
		t.Errorf("body = %q, want it to name the protocol mismatch", body)
	}
}

// ---- configuration errors -----------------------------------------------

func TestTLS_CertWithoutKeyIsRejected(t *testing.T) {
	m := newMonitor(t, "half-config-host")

	for name, cfg := range map[string]*server.TLS{
		"cert without key": {CertFile: "/nonexistent/cert.pem"},
		"key without cert": {KeyFile: "/nonexistent/key.pem"},
	} {
		_, err := server.NewWithConfig(server.Config{Monitor: m, Addr: ":0", TLS: cfg})
		if err == nil {
			t.Errorf("%s: NewWithConfig() = nil error, want a rejection", name)
			continue
		}
		var tlsErr *types.TLSConfigError
		if !errors.As(err, &tlsErr) {
			t.Errorf("%s: error = %T (%v), want *types.TLSConfigError", name, err, err)
		}
	}
}

func TestTLS_MissingCertFileIsRejected(t *testing.T) {
	m := newMonitor(t, "missing-cert-host")

	_, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    ":0",
		TLS: &server.TLS{
			CertFile: filepath.Join(t.TempDir(), "absent.pem"),
			KeyFile:  filepath.Join(t.TempDir(), "absent.key"),
		},
	})
	if err == nil {
		t.Fatal("NewWithConfig() = nil error, want a failure for an unreadable key pair")
	}

	var tlsErr *types.TLSConfigError
	if !errors.As(err, &tlsErr) {
		t.Fatalf("error = %T (%v), want *types.TLSConfigError", err, err)
	}
	if tlsErr.Unwrap() == nil {
		t.Error("TLSConfigError did not wrap the underlying load failure")
	}
}

// TestTLS_NoCertificateSourceIsRejected covers the misconfiguration that would
// otherwise surface only as an opaque handshake failure at request time.
func TestTLS_NoCertificateSourceIsRejected(t *testing.T) {
	m := newMonitor(t, "empty-tls-host")

	for name, cfg := range map[string]*server.TLS{
		"entirely empty": {},
		"empty base config": {
			Config: &tls.Config{MinVersion: tls.VersionTLS13},
		},
	} {
		_, err := server.NewWithConfig(server.Config{Monitor: m, Addr: ":0", TLS: cfg})
		if err == nil {
			t.Errorf("%s: NewWithConfig() = nil error, want a rejection", name)
			continue
		}
		var tlsErr *types.TLSConfigError
		if !errors.As(err, &tlsErr) {
			t.Errorf("%s: error = %T (%v), want *types.TLSConfigError", name, err, err)
		}
	}
}

// ---- caller-supplied base config ----------------------------------------

// TestTLS_CallerConfigIsHonouredAndNotMutated is the library-embedding case: a
// host application hands over its own settings and must get them back intact.
func TestTLS_CallerConfigIsHonouredAndNotMutated(t *testing.T) {
	cert, leaf := selfSigned(t)

	caller := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	m := newMonitor(t, "caller-config-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS:     &server.TLS{Config: caller},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	resp, err := clientFor(t, leaf).Get("https://" + addr + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("negotiated version = %x, want TLS 1.3 as the caller required", resp.TLS.Version)
	}

	// The caller's value must not have been written through.
	if len(caller.Certificates) != 1 {
		t.Errorf("caller Certificates length = %d, want it left at 1", len(caller.Certificates))
	}
	if caller.GetConfigForClient != nil {
		t.Error("caller config had GetConfigForClient installed on it")
	}
}

// TestTLS_MinVersionDefaultsWhenUnset verifies the server does not inherit
// Go's willingness to negotiate older protocol versions by default.
func TestTLS_MinVersionDefaultsWhenUnset(t *testing.T) {
	cert, leaf := selfSigned(t)

	m := newMonitor(t, "minversion-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS:     &server.TLS{Config: &tls.Config{Certificates: []tls.Certificate{cert}}},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	// A client capped at TLS 1.1 must be turned away.
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	old := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS10,
				MaxVersion: tls.VersionTLS11,
			},
		},
	}

	resp, err := old.Get("https://" + addr + "/api/snapshot")
	if err == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatal("a TLS 1.1 client completed a handshake; the floor should be TLS 1.2")
	}
}

// ---- dynamic per-handshake resolution -----------------------------------

// TestTLS_GetConfigForClientResolvesPerHandshake is the dynamic hook an
// embedding application uses to pick a certificate by SNI. It must run for
// each connection and receive the requested server name.
func TestTLS_GetConfigForClientResolvesPerHandshake(t *testing.T) {
	cert, leaf := selfSigned(t, "sysmon.example")

	var calls atomic.Int64
	seen := make(chan string, 4)

	m := newMonitor(t, "sni-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS: &server.TLS{
			GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
				calls.Add(1)
				select {
				case seen <- hi.ServerName:
				default:
				}
				return &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   tls.VersionTLS12,
					// Keep ALPN available so HTTP/2 can still be negotiated.
					NextProtos: []string{"h2", "http/1.1"},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "sysmon.example",
				MinVersion: tls.VersionTLS12,
			},
			// Force a fresh connection per request so the hook runs twice.
			DisableKeepAlives: true,
		},
	}

	for i := range 2 {
		resp, err := client.Get("https://" + addr + "/api/snapshot")
		if err != nil {
			t.Fatalf("request %d over TLS: %v", i, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, resp.StatusCode)
		}
	}

	if got := calls.Load(); got < 2 {
		t.Errorf("GetConfigForClient called %d times, want once per handshake (>=2)", got)
	}

	select {
	case name := <-seen:
		if name != "sysmon.example" {
			t.Errorf("ClientHelloInfo.ServerName = %q, want %q", name, "sysmon.example")
		}
	default:
		t.Error("GetConfigForClient never observed a ClientHelloInfo")
	}
}

// TestTLS_GetConfigForClientErrorFailsHandshake verifies a resolver that
// refuses a connection actually refuses it, rather than falling back to some
// default certificate.
func TestTLS_GetConfigForClientErrorFailsHandshake(t *testing.T) {
	m := newMonitor(t, "reject-sni-host")

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS: &server.TLS{
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				return nil, errors.New("unknown tenant")
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			//nolint:gosec // The handshake must fail before verification matters.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	resp, err := client.Get("https://" + addr + "/api/snapshot")
	if err == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatal("handshake succeeded although the resolver returned an error")
	}
}

// TestTLS_StaticCertsCombineWithDynamicResolver covers supplying both: the
// static pair is the base, and the resolver may still override per connection.
func TestTLS_StaticCertsCombineWithDynamicResolver(t *testing.T) {
	staticCert, staticLeaf := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, staticCert)

	var resolved atomic.Bool

	m := newMonitor(t, "combined-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS: &server.TLS{
			CertFile: certPath,
			KeyFile:  keyPath,
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				resolved.Store(true)
				// Returning nil defers to the base configuration.
				return nil, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	resp, err := clientFor(t, staticLeaf).Get("https://" + addr + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !resolved.Load() {
		t.Error("GetConfigForClient was not consulted when static certs were also present")
	}
}

// ---- streaming over TLS -------------------------------------------------

// TestTLS_EventStreamOverHTTPS is the end-to-end check that the SSE stream
// works over an encrypted connection, which is the deployment the TLS hook
// exists for.
func TestTLS_EventStreamOverHTTPS(t *testing.T) {
	cert, leaf := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, cert)

	m := newMonitor(t, "tls-stream-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS:     &server.TLS{CertFile: certPath, KeyFile: keyPath},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}

	c := dialEventsVia(t, transport, "https://"+addr+"/api/events", false)

	if ct := c.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if c.resp.TLS == nil {
		t.Fatal("stream response carried no TLS state; the connection was not encrypted")
	}

	ev := c.nextNamed(t, "snapshot", 5*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "tls-stream-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "tls-stream-host")
	}
}

// TestTLS_GzipEventStreamOverHTTPS confirms compression and TLS compose, which
// is the combination a real deployment runs.
func TestTLS_GzipEventStreamOverHTTPS(t *testing.T) {
	cert, leaf := selfSigned(t)
	certPath, keyPath := writeKeyPair(t, cert)

	m := newMonitor(t, "tls-gzip-host")
	waitForSnapshot(t, m)

	addr := freeAddr(t)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m,
		Addr:    addr,
		TLS:     &server.TLS{CertFile: certPath, KeyFile: keyPath},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	startTLSServer(t, srv, addr)

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}

	c := dialEventsVia(t, transport, "https://"+addr+"/api/events", true)

	if got := c.resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}

	ev := c.nextNamed(t, "snapshot", 5*time.Second)
	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "tls-gzip-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "tls-gzip-host")
	}
}
