package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The peer transport, end to end. These exist because the interesting failures
// are all at the join between config, client and poll loop: a CA that is loaded
// but not installed, a scheme that is checked after the connection is already
// made, an enforcement flag read from the wrong generation of the config.

// selfSigned mints a throwaway CA-capable certificate, used here as both the
// client certificate and the authority that signed it. ECDSA because these are
// unit tests and a P-256 key is generated in microseconds.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aggregator"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func testPoller(t *testing.T) *Poller {
	t.Helper()
	return &Poller{
		client: outbound.NewClient(0),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// snapshotPeer serves a minimal snapshot at /api/snapshot.
func snapshotPeer(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/snapshot" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&types.Snapshot{
			Version: "test",
			Host:    types.HostInfo{Hostname: "peer"},
		}); err != nil {
			t.Errorf("encoding the peer snapshot: %v", err)
		}
	})
}

func caFileFor(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peer-ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

// plaintextAllowed is the zero-value TLS setting, spelled out. That it needs no
// field set is the compatibility guarantee this feature rests on.
func plaintextAllowed() config.Fleet {
	return config.Fleet{TimeoutMs: 5000}
}

// TestFetch_HTTPSPeerVerifiedAgainstConfiguredCA is the issue's actual fix: the
// aggregator now authenticates the node that answered.
func TestFetch_HTTPSPeerVerifiedAgainstConfiguredCA(t *testing.T) {
	srv := httptest.NewTLSServer(snapshotPeer(t))
	defer srv.Close()

	client, err := outbound.NewTLSClient(0, outbound.TLS{CAFile: caFileFor(t, srv)})
	if err != nil {
		t.Fatalf("NewTLSClient() error = %v", err)
	}
	p := testPoller(t)
	p.SetClient(client)

	snap, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed())
	if err != nil {
		t.Fatalf("fetch() error = %v, want the https poll to succeed", err)
	}
	if snap == nil || snap.Host.Hostname != "peer" {
		t.Fatalf("fetch() = %+v, want the peer's snapshot", snap)
	}
}

// TestFetch_HTTPSPeerRefusedWhenUntrusted is the other half. Without it the
// test above would pass just as well against a client that verified nothing,
// which is the state this feature exists to leave behind.
func TestFetch_HTTPSPeerRefusedWhenUntrusted(t *testing.T) {
	srv := httptest.NewTLSServer(snapshotPeer(t))
	defer srv.Close()

	// A client with no CA configured at all falls back to the system roots,
	// which do not include httptest's throwaway authority.
	p := testPoller(t)

	snap, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed())
	if err == nil {
		t.Fatalf("fetch() = %+v, want an error for an unverifiable peer", snap)
	}
	var unknown x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if !errors.As(err, &unknown) && !errors.As(err, &hostErr) {
		t.Errorf("fetch() error = %v, want a certificate verification failure", err)
	}
	if snap != nil {
		t.Errorf("fetch() returned %+v alongside the error, want nil", snap)
	}
}

// TestFetch_PlaintextPeer covers the compatibility promise in both directions:
// an http peer keeps working by default, and stops working the moment an
// operator says the fleet has finished moving to TLS.
func TestFetch_PlaintextPeer(t *testing.T) {
	srv := httptest.NewServer(snapshotPeer(t))
	defer srv.Close()

	t.Run("allowed by default", func(t *testing.T) {
		p := testPoller(t)
		// Both the shipped defaults AND a Fleet built in code must permit it;
		// the second is the case that a config.Fleet literal takes, and getting
		// it wrong denies the whole fleet rather than one peer.
		cfg := config.Fleet{TimeoutMs: 5000, TLS: config.Defaults().Fleet.TLS}
		snap, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, cfg)
		if err != nil {
			t.Fatalf("fetch() error = %v; the shipped default must keep an http fleet working", err)
		}
		if snap == nil {
			t.Fatal("fetch() returned no snapshot")
		}
	})

	t.Run("refused when plaintext is switched off", func(t *testing.T) {
		p := testPoller(t)
		cfg := config.Fleet{TimeoutMs: 5000, TLS: config.FleetTLS{RequireTLS: true}}
		snap, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, cfg)
		if err == nil {
			t.Fatalf("fetch() = %+v, want an error", snap)
		}
		var plain *ErrPlaintextPeer
		if !errors.As(err, &plain) {
			t.Fatalf("fetch() error = %T (%v), want *ErrPlaintextPeer", err, err)
		}
		if plain.Peer != "peer" {
			t.Errorf("ErrPlaintextPeer.Peer = %q, want %q", plain.Peer, "peer")
		}
		// The message has to name the peer and both ways out, because it is
		// read by someone whose fleet view just went blank.
		for _, want := range []string{"peer", "https", "require_tls"} {
			if !strings.Contains(plain.Error(), want) {
				t.Errorf("ErrPlaintextPeer message %q does not mention %q", plain.Error(), want)
			}
		}
	})
}

func TestFetch_RejectsUnusableURLs(t *testing.T) {
	p := testPoller(t)

	t.Run("scheme that is neither http nor https", func(t *testing.T) {
		_, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: "ftp://example.invalid"}, plaintextAllowed())
		var scheme *ErrPeerScheme
		if !errors.As(err, &scheme) {
			t.Fatalf("fetch() error = %T (%v), want *ErrPeerScheme", err, err)
		}
		if scheme.Peer != "peer" || scheme.Scheme != "ftp" {
			t.Errorf("ErrPeerScheme = %+v, want peer=peer scheme=ftp", scheme)
		}
	})

	t.Run("no scheme at all", func(t *testing.T) {
		_, err := fetchSnapshot(p, config.Peer{Name: "bare", URL: "192.168.1.1:8081"}, plaintextAllowed())
		if err == nil {
			t.Fatal("fetch() accepted a URL with no scheme")
		}
		// Whatever it is classified as, it must name the peer rather than
		// surfacing an opaque transport error.
		if !strings.Contains(err.Error(), "bare") {
			t.Errorf("fetch() error = %q, want it to name the peer", err)
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		_, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: "http://[::1"}, plaintextAllowed())
		var bad *ErrPeerURL
		if !errors.As(err, &bad) {
			t.Fatalf("fetch() error = %T (%v), want *ErrPeerURL", err, err)
		}
		if bad.Unwrap() == nil {
			t.Error("ErrPeerURL.Unwrap() = nil, want the parse error underneath")
		}
	})
}

// TestFetch_MutualTLS is the node-authentication half of the issue: the peer
// requires a client certificate, so an aggregator that cannot present one is
// refused by the peer rather than served.
func TestFetch_MutualTLS(t *testing.T) {
	certPEM, keyPEM := selfSigned(t)

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(certPEM) {
		t.Fatal("client CA pool did not accept the generated certificate")
	}

	srv := httptest.NewUnstartedServer(snapshotPeer(t))
	srv.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
		MinVersion: tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	caPath := caFileFor(t, srv)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client-key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write client cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	t.Run("presenting a certificate", func(t *testing.T) {
		client, err := outbound.NewTLSClient(0, outbound.TLS{
			CAFile: caPath, CertFile: certPath, KeyFile: keyPath,
		})
		if err != nil {
			t.Fatalf("NewTLSClient() error = %v", err)
		}
		p := testPoller(t)
		p.SetClient(client)

		if _, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed()); err != nil {
			t.Fatalf("fetch() error = %v, want the mTLS poll to succeed", err)
		}
	})

	t.Run("without one", func(t *testing.T) {
		client, err := outbound.NewTLSClient(0, outbound.TLS{CAFile: caPath})
		if err != nil {
			t.Fatalf("NewTLSClient() error = %v", err)
		}
		p := testPoller(t)
		p.SetClient(client)

		if snap, err := fetchSnapshot(p, config.Peer{Name: "peer", URL: srv.URL}, plaintextAllowed()); err == nil {
			t.Fatalf("fetch() = %+v, want the peer to refuse an unauthenticated aggregator", snap)
		}
	})
}

func TestSetClient(t *testing.T) {
	p := testPoller(t)
	original := p.client

	t.Run("nil is ignored", func(t *testing.T) {
		p.SetClient(nil)
		if p.client != original {
			t.Error("SetClient(nil) replaced the client; a caller with nothing to install must not disarm the poller")
		}
	})

	t.Run("replaces", func(t *testing.T) {
		// Installed as a copy with its own pooled transport (see pooled), so
		// what is checked is that the caller's settings -- its timeout and
		// its TLS policy -- are the ones the poller now uses.
		transport := &http.Transport{TLSClientConfig: &tls.Config{ServerName: "peers.example"}}
		replacement := &http.Client{Timeout: 7 * time.Second, Transport: transport}
		p.SetClient(replacement)
		if p.client == original {
			t.Fatal("SetClient() did not install the client")
		}
		installed, ok := p.client.Transport.(*http.Transport)
		if p.client.Timeout != 7*time.Second || !ok || installed.TLSClientConfig.ServerName != "peers.example" {
			t.Errorf("installed client = %+v; want the replacement's timeout and TLS policy", p.client)
		}
	})
}

// fetchSnapshot fetches a peer's whole snapshot through the poller's transport,
// which is what every test in this file exercises.
func fetchSnapshot(p *Poller, peer config.Peer, cfg config.Fleet) (*types.Snapshot, error) {
	var snap types.Snapshot
	if err := p.get(context.Background(), peer, cfg, endpointSnapshot, maxSnapshotBytes, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
