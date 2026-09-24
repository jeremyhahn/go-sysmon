package outbound

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// selfSigned mints a throwaway CA-capable certificate for one host.
//
// ECDSA rather than RSA because these run in unit tests: a P-256 key is
// generated in microseconds where a 2048-bit RSA key takes tens of
// milliseconds, and nothing here is testing the key type.
func selfSigned(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
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

// writeTemp puts b in a file under the test's own directory and returns the path.
func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestTLSConfigured(t *testing.T) {
	tests := []struct {
		name string
		tls  TLS
		want bool
	}{
		{"zero value", TLS{}, false},
		{"ca only", TLS{CAFile: "/ca.pem"}, true},
		{"client key pair", TLS{CertFile: "/c.pem", KeyFile: "/k.pem"}, true},
		{"insecure alone", TLS{InsecureSkipVerify: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tls.Configured(); got != tc.want {
				t.Errorf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTLSConfig_LoadsCAAndKeyPair(t *testing.T) {
	certPEM, keyPEM := selfSigned(t, "peer.example")
	caPath := writeTemp(t, "ca.pem", certPEM)
	certPath := writeTemp(t, "client.pem", certPEM)
	keyPath := writeTemp(t, "client-key.pem", keyPEM)

	cfg, err := TLS{CAFile: caPath, CertFile: certPath, KeyFile: keyPath}.Config()
	if err != nil {
		t.Fatalf("Config() error = %v, want nil", err)
	}
	if cfg.RootCAs == nil {
		t.Error("Config() left RootCAs nil; the ca_file was not loaded")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Config() loaded %d client certificates, want 1", len(cfg.Certificates))
	}
	if cfg.InsecureSkipVerify {
		t.Error("Config() set InsecureSkipVerify without being asked to")
	}
	if cfg.MinVersion == 0 {
		t.Error("Config() left MinVersion unset; it must floor the negotiation")
	}
}

// TestTLSConfig_Rejects covers every way the settings can be wrong. Each must
// fail loudly: falling back to system roots would verify peers against
// authorities the operator did not choose, and the only symptom would be a peer
// that should have been refused not being.
func TestTLSConfig_Rejects(t *testing.T) {
	certPEM, keyPEM := selfSigned(t, "peer.example")
	dir := t.TempDir()

	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client-key.pem")
	junkPath := filepath.Join(dir, "junk.pem")
	for path, b := range map[string][]byte{
		certPath: certPEM,
		keyPath:  keyPEM,
		junkPath: []byte("this is not a certificate\n"),
	} {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	tests := []struct {
		name string
		tls  TLS
	}{
		{"cert without key", TLS{CertFile: certPath}},
		{"key without cert", TLS{KeyFile: keyPath}},
		{"ca file missing", TLS{CAFile: filepath.Join(dir, "absent.pem")}},
		{"ca file has no PEM", TLS{CAFile: junkPath}},
		{"key pair does not load", TLS{CertFile: certPath, KeyFile: junkPath}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := tc.tls.Config()
			if err == nil {
				t.Fatalf("Config() = %+v, want an error", cfg)
			}
			var want *types.TLSConfigError
			if !errors.As(err, &want) {
				t.Errorf("Config() error = %T (%v), want *types.TLSConfigError", err, err)
			}
			if cfg != nil {
				t.Errorf("Config() returned %+v alongside the error, want nil", cfg)
			}
		})
	}
}

// TestNewTLSClient_UnconfiguredMatchesNewClient is the guarantee that a
// plaintext fleet is untouched: the caller need not branch, and what it gets is
// the client it would have got before this feature existed.
func TestNewTLSClient_Unconfigured(t *testing.T) {
	c, err := NewTLSClient(3*time.Second, TLS{})
	if err != nil {
		t.Fatalf("NewTLSClient() error = %v, want nil", err)
	}
	if c.Transport != nil {
		t.Errorf("NewTLSClient() installed a transport %T for an unconfigured TLS; want the default", c.Transport)
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("NewTLSClient() timeout = %v, want 3s", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Error("NewTLSClient() dropped the redirect policy")
	}
}

func TestNewTLSClient_RejectsBadSettings(t *testing.T) {
	c, err := NewTLSClient(0, TLS{CAFile: filepath.Join(t.TempDir(), "absent.pem")})
	if err == nil {
		t.Fatalf("NewTLSClient() = %+v, want an error for an unreadable ca_file", c)
	}
	if c != nil {
		t.Errorf("NewTLSClient() returned a client alongside the error: %+v", c)
	}
}

// TestNewTLSClient_VerifiesAgainstTheConfiguredCA is the actual security
// property: the same server is reached when its CA is trusted and refused when
// it is not. A client that verified nothing would pass the first half only.
func TestNewTLSClient_VerifiesAgainstTheConfiguredCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caPath := writeTemp(t, "peer-ca.pem", pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: srv.Certificate().Raw,
	}))

	t.Run("trusted", func(t *testing.T) {
		c, err := NewTLSClient(5*time.Second, TLS{CAFile: caPath})
		if err != nil {
			t.Fatalf("NewTLSClient() error = %v", err)
		}
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("Get() error = %v, want the handshake to succeed", err)
		}
		t.Cleanup(func() {
			if err := resp.Body.Close(); err != nil {
				t.Errorf("close response body: %v", err)
			}
		})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Get() status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("untrusted", func(t *testing.T) {
		other, _ := selfSigned(t, "somebody.else")
		c, err := NewTLSClient(5*time.Second, TLS{CAFile: writeTemp(t, "other-ca.pem", other)})
		if err != nil {
			t.Fatalf("NewTLSClient() error = %v", err)
		}
		resp, err := c.Get(srv.URL)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Errorf("close response body: %v", closeErr)
			}
			t.Fatal("Get() succeeded against a server signed by an authority the client does not trust")
		}
	})
}
