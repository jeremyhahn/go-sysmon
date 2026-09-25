//go:build integration

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
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// ── Stand-in peers ───────────────────────────────────────────────────────────
//
// A real `sysmon serve` cannot misbehave on demand, so a peer that has to --
// answer with the wrong status, a body that is not JSON, a redirect, a
// certificate nobody trusts -- is an in-process HTTP server the test controls.
// Everything a real node does is tested against real nodes.

// standIn is an HTTP server whose answer to each path the test sets, and can
// change while an aggregator polls it.
type standIn struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	// hits counts the requests for each path.
	hits map[string]int
	// headers and queries are the last request's headers and raw query for
	// each path.
	headers map[string]http.Header
	queries map[string]string
}

// newStandIn starts a plain-HTTP stand-in on loopback.
func newStandIn(t *testing.T) *standIn {
	t.Helper()
	s := &standIn{routes: map[string]http.HandlerFunc{}, hits: map[string]int{}, headers: map[string]http.Header{}, queries: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// newTLSStandIn starts an HTTPS stand-in presenting cert. With clientCAs set it
// requires a client certificate signed by them.
func newTLSStandIn(t *testing.T, cert tls.Certificate, clientCAs *x509.CertPool) *standIn {
	t.Helper()
	s := &standIn{routes: map[string]http.HandlerFunc{}, hits: map[string]int{}, headers: map[string]http.Header{}, queries: map[string]string{}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if clientCAs != nil {
		s.TLS.ClientCAs = clientCAs
		s.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	// Handshake failures are the point of several tests; the server's own
	// complaint about each is noise.
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h, ok := s.routes[r.URL.Path]
	s.hits[r.URL.Path]++
	s.headers[r.URL.Path] = r.Header.Clone()
	s.queries[r.URL.Path] = r.URL.RawQuery
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

// handle sets the answer to path.
func (s *standIn) handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = h
}

// hitsOf is how many requests path has had.
func (s *standIn) hitsOf(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// headerOf is the last request for path's header key.
func (s *standIn) headerOf(path, key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[path].Get(key)
}

// queryOf is the last request for path's raw query.
func (s *standIn) queryOf(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries[path]
}

// jsonAnswer answers with v as JSON.
func jsonAnswer(v any) http.HandlerFunc {
	body, err := json.Marshal(v)
	return func(w http.ResponseWriter, _ *http.Request) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, werr := w.Write(body); werr != nil {
			return
		}
	}
}

// rawAnswer answers with body, status and content type as given.
func rawAnswer(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			return
		}
	}
}

// ── Nodes as a peer describes itself ─────────────────────────────────────────

// leafSummary is the summary a healthy leaf on this build serves.
func leafSummary(name string) types.NodeSummary {
	return types.NodeSummary{
		Hostname:      name + ".example",
		NodeName:      name,
		Version:       "test",
		UptimeSeconds: 3600,
		CPUPercent:    12.5,
		CPUAvailable:  true,
		MemoryPercent: 40,
		LoadPerCore:   0.2,
		Alerts:        []types.Alert{},
	}
}

// alertOf is an alert of severity on resource.
func alertOf(severity types.Severity, rule, resource string) types.Alert {
	return types.Alert{
		ID: rule + ":" + resource, Rule: rule, Severity: severity, Category: "storage",
		Resource: resource, Message: rule + " on " + resource, Since: time.Unix(1_790_000_000, 0).UTC(),
	}
}

// ── Aggregators ──────────────────────────────────────────────────────────────

// peer is one fleet.peers entry.
type peer struct{ name, url string }

// fleetYAML is a configuration naming this node local and polling peers.
// extra is appended under fleet: (indented two spaces) and may be empty.
func fleetYAML(local string, pollMs, timeoutMs int, peers []peer, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet:\n  local_name: %s\n  poll_interval_ms: %d\n  timeout_ms: %d\n", local, pollMs, timeoutMs)
	if extra != "" {
		b.WriteString(extra)
	}
	if len(peers) > 0 {
		b.WriteString("  peers:\n")
		for _, p := range peers {
			fmt.Fprintf(&b, "    - name: %s\n      url: %s\n", p.name, p.url)
		}
	}
	return b.String()
}

// onePoll is a poll interval long enough that an aggregator polls exactly once
// -- when it starts -- for the life of a test, so what it shows is what that
// one poll found, and a request made later cannot race a second.
const onePoll = 600_000

// fleetOf decodes GET /api/fleet with query from the aggregator at addr.
func fleetOf(t *testing.T, addr, query string) types.Fleet {
	t.Helper()
	var v types.Fleet
	harness.GetJSON(t, "http://"+addr+"/api/fleet"+query, &v)
	return v
}

// awaitFleet waits until the aggregator's fleet view satisfies ok, and
// returns it.
func awaitFleet(t *testing.T, addr, what string, ok func(types.Fleet) bool) types.Fleet {
	t.Helper()
	var v types.Fleet
	await.Until(t, what, func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/fleet", nil)
		if err != nil {
			t.Fatalf("build GET /api/fleet: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		var got types.Fleet
		err = json.NewDecoder(resp.Body).Decode(&got)
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close GET /api/fleet: %v", cerr)
		}
		if err != nil {
			return false
		}
		v = got
		return ok(got)
	})
	return v
}

// polled reports whether every peer in v has been polled at least once.
func polled(v types.Fleet) bool {
	if !v.Enabled {
		return false
	}
	for _, n := range v.Nodes {
		if n.Error == "not polled yet" {
			return false
		}
	}
	return true
}

// node returns the node named name from v.
func node(t *testing.T, v types.Fleet, name string) types.FleetNode {
	t.Helper()
	for _, n := range v.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node %q in the fleet view %+v", name, names(v))
	return types.FleetNode{}
}

// names lists v's node names in order.
func names(v types.Fleet) []string {
	out := make([]string, len(v.Nodes))
	for i, n := range v.Nodes {
		out[i] = n.Name
	}
	return out
}

// request sends GET path to addr with header set, and returns the status, the
// response headers and the body.
func request(t *testing.T, addr, path string, header map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return harness.Do(t, http.DefaultClient, req)
}

// ── PKI ──────────────────────────────────────────────────────────────────────

// authority is a certificate authority the test made.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// pemPath is the CA certificate written as PEM, for fleet.tls.ca_file.
	pemPath string
	pool    *x509.CertPool
}

// newAuthority creates a CA and writes its certificate to a file.
func newAuthority(t *testing.T, name string) *authority {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA %s: %v", name, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA %s: %v", name, err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	path := filepath.Join(t.TempDir(), name+"-ca.pem")
	writePEM(t, path, "CERTIFICATE", der)
	return &authority{cert: cert, key: key, pemPath: path, pool: pool}
}

// leafOptions shape a certificate the authority issues.
type leafOptions struct {
	// ips and dnsNames are the names it is valid for; 127.0.0.1 when both are
	// empty.
	ips      []net.IP
	dnsNames []string
	// expired issues a certificate that ran out an hour ago.
	expired bool
	// client issues a client certificate rather than a server one.
	client bool
}

// issued is a certificate and its key, in memory and on disk.
type issued struct {
	pair     tls.Certificate
	certPath string
	keyPath  string
}

// issue signs a leaf certificate.
func (a *authority) issue(t *testing.T, cn string, o leafOptions) issued {
	t.Helper()
	key := newKey(t)
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	if o.expired {
		notBefore, notAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
	}
	ips := o.ips
	if len(ips) == 0 && len(o.dnsNames) == 0 {
		ips = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	usage := x509.ExtKeyUsageServerAuth
	if o.client {
		usage = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
		DNSNames:     o.dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatalf("issue %s: %v", cn, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key of %s: %v", cn, err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, cn+".pem"), filepath.Join(dir, cn+"-key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load %s: %v", cn, err)
	}
	return issued{pair: pair, certPath: certPath, keyPath: keyPath}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return key
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("draw a serial number: %v", err)
	}
	return n
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// hostPort is the host:port of a stand-in's URL.
func hostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u.Host
}
