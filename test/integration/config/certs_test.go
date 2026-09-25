//go:build integration

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pki is a certificate authority and a server certificate it issued for
// 127.0.0.1, written as PEM files.
type pki struct {
	caFile   string
	certFile string
	keyFile  string
	pool     *x509.CertPool
}

// newPKI issues a CA and a loopback server certificate for this test.
func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	caKey := newKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sysmon integration CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create the CA: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey := newKey(t)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}

	p := pki{
		caFile:   writePEM(t, dir, "ca.pem", "CERTIFICATE", caDER),
		certFile: writePEM(t, dir, "server.pem", "CERTIFICATE", leafDER),
		keyFile:  writePEM(t, dir, "server-key.pem", "EC PRIVATE KEY", keyDER),
		pool:     x509.NewCertPool(),
	}
	p.pool.AddCert(ca)
	return p
}

// client trusts the test CA and nothing else.
func (p pki) client() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: p.pool, MinVersion: tls.VersionTLS12}}}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return key
}

func writePEM(t *testing.T, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
