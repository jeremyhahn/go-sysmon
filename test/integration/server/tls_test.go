//go:build integration

package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestTLS_ServesHTTPSWithTheOperatorsCertificate serves with --tls-cert and
// --tls-key: a client trusting the operator's CA gets the API, with HSTS; a
// client that does not trust it is refused at the handshake; and plain HTTP
// to the TLS port gets no API answer.
func TestTLS_ServesHTTPSWithTheOperatorsCertificate(t *testing.T) {
	pki := newPKI(t)
	client := pki.client()
	s := harness.Serve(t, harness.ServeOptions{
		Args:   []string{"--tls-cert", pki.certFile, "--tls-key", pki.keyFile},
		Client: client,
	})
	base := "https://" + s.Addr

	status, header, body := request(t, client, http.MethodGet, base+"/api/version", "")
	if status != http.StatusOK || !strings.Contains(body, `"version"`) {
		t.Fatalf("GET /api/version over TLS = %d %s", status, body)
	}
	if got := header.Get("Strict-Transport-Security"); got == "" {
		t.Error("no HSTS over TLS")
	}

	st := openStream(t, base+"/api/events", streamOptions{client: client})
	if ev := st.next(t); ev.name != "snapshot" {
		t.Errorf("first event over TLS = %q, want snapshot", ev.name)
	}

	untrusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	req, err := http.NewRequest(http.MethodGet, base+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := untrusting.Do(req)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Error(cerr)
		}
		t.Fatalf("a client not trusting the CA got %d, want the handshake refused", resp.StatusCode)
	}
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) {
		t.Errorf("a client not trusting the CA got %v, want an unknown-authority failure", err)
	}

	status, _ = harness.Get(t, "http://"+s.Addr+"/api/version")
	if status == http.StatusOK {
		t.Error("plain HTTP to the TLS port was answered")
	}
}

// TestTLS_UnusableFilesStopTheServiceAtStartup verifies a certificate the
// service cannot use is a startup error naming the problem, not a server
// that starts and fails every handshake.
func TestTLS_UnusableFilesStopTheServiceAtStartup(t *testing.T) {
	pki := newPKI(t)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := newPKI(t)

	cases := map[string]struct {
		args []string
		want string
	}{
		"a certificate with no key":           {[]string{"--tls-cert", pki.certFile}, "tls"},
		"a certificate file that is absent":   {[]string{"--tls-cert", filepath.Join(t.TempDir(), "absent.pem"), "--tls-key", pki.keyFile}, "absent.pem"},
		"a certificate that is not PEM":       {[]string{"--tls-cert", garbage, "--tls-key", pki.keyFile}, "tls"},
		"a key that is not the certificate's": {[]string{"--tls-cert", pki.certFile, "--tls-key", other.keyFile}, "tls"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"serve", "--addr", "127.0.0.1:0", "--config", filepath.Join(t.TempDir(), "absent.yaml")}, c.args...)
			r := harness.Run(t, harness.Bins.Sysmon, nil, args...)
			if r.Code == 0 {
				t.Fatalf("serve with %s exited 0, want a startup failure", name)
			}
			if !strings.Contains(strings.ToLower(r.Output()), c.want) {
				t.Errorf("serve with %s said:\n%s\nwant it to mention %q", name, r.Output(), c.want)
			}
		})
	}
}
