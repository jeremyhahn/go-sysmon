//go:build integration

package server

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

// testPKI is a certificate authority the suite makes for itself, and a
// server certificate it issued for the loopback address.
type testPKI struct {
	// caFile, certFile and keyFile are PEM files on disk.
	caFile   string
	certFile string
	keyFile  string
	// pool trusts the CA.
	pool *x509.CertPool
	// leaf is the server certificate and key, for an embedded server.
	leaf tls.Certificate
}

// newPKI makes a CA and a server certificate for 127.0.0.1 and localhost,
// valid for an hour either side of now.
func newPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
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
		t.Fatalf("CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	p := &testPKI{
		caFile:   writePEM(t, filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER),
		certFile: writePEM(t, filepath.Join(dir, "cert.pem"), "CERTIFICATE", der),
		keyFile:  writePEM(t, filepath.Join(dir, "key.pem"), "EC PRIVATE KEY", keyDER),
		pool:     x509.NewCertPool(),
	}
	p.pool.AddCert(caCert)
	p.leaf, err = tls.LoadX509KeyPair(p.certFile, p.keyFile)
	if err != nil {
		t.Fatalf("load the server key pair: %v", err)
	}
	return p
}

// writePEM writes one PEM block to path and returns path.
func writePEM(t *testing.T, path, kind string, der []byte) string {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// client returns an HTTPS client that trusts only the test CA.
func (p *testPKI) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: p.pool, MinVersion: tls.VersionTLS12},
	}}
}
