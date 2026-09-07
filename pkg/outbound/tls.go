package outbound

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TLS describes how an outbound connection authenticates the far end, and how
// it identifies itself to it.
//
// The zero value means "use the platform's own trust decisions": system root
// CAs, no client certificate, verification on. That is deliberate -- a caller
// that supplies nothing must not end up with verification off.
type TLS struct {
	// CAFile is a PEM bundle of the authorities allowed to have signed the
	// peer's certificate. Empty means the system roots, which is right for a
	// public endpoint and wrong for an internal CA.
	CAFile string

	// CertFile and KeyFile are this node's own PEM certificate and private
	// key, presented to a peer that asks for one. Both must be set together.
	// This is the client half of mTLS; the peer's server decides whether it
	// requires the certificate and what identity it grants.
	CertFile string
	KeyFile  string

	// InsecureSkipVerify disables verification of the peer's certificate
	// entirely, which reduces https to obfuscated http: it still encrypts, and
	// it authenticates nobody, so an on-path attacker can terminate the
	// connection and answer in the peer's name.
	//
	// It is named rather than inferred so that turning it on is a decision
	// someone wrote down. Nothing in this codebase sets it.
	InsecureSkipVerify bool
}

// Configured reports whether any TLS material has been supplied. A client for
// plain http peers needs none, so this is what distinguishes "no TLS settings"
// from "TLS settings that happen to be defaults".
func (t TLS) Configured() bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.InsecureSkipVerify
}

// Config resolves t into a *tls.Config.
//
// It fails rather than degrades. A CA file that cannot be read, a key pair that
// does not load, or a bundle with no certificate in it are all configuration
// mistakes, and continuing with system roots would mean verifying peers against
// authorities the operator did not choose -- silently, and only detectably by
// noticing that a peer which should have been refused was not.
func (t TLS) Config() (*tls.Config, error) {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return nil, &types.TLSConfigError{
			Message: "cert_file and key_file must be set together",
		}
	}

	// InsecureSkipVerify is carried through rather than decided here: this
	// function's job is to render the settings faithfully, and the judgement
	// about whether skipping verification is acceptable belongs to the operator
	// who wrote it in the config file. It defaults false, nothing in this
	// codebase sets it true, and startup logs a warning when it is on.
	//
	// #nosec G402 -- not a hardcoded true; this is the operator's own
	// fleet.tls.insecure_skip_verify, which has to take effect or the setting
	// would silently do nothing.
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: t.InsecureSkipVerify,
	}

	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, &types.TLSConfigError{
				Message: "read ca_file " + t.CAFile,
				Cause:   err,
			}
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, &types.TLSConfigError{
				Message: "ca_file " + t.CAFile + " contains no PEM certificate",
			}
		}
		cfg.RootCAs = pool
	}

	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, &types.TLSConfigError{
				Message: "load key pair " + t.CertFile,
				Cause:   err,
			}
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// NewTLSClient returns a client that applies t to every connection it makes and
// still refuses to be walked off the host it was pointed at.
//
// The redirect policy is the same one NewClient installs, because the two
// protections answer different halves of the same question: TLS decides whether
// the far end is who the config named, and Check decides whether the far end is
// still the host the config named.
//
// An unconfigured t yields exactly what NewClient yields, so a caller can hand
// this the zero value rather than branching.
func NewTLSClient(timeout time.Duration, t TLS) (*http.Client, error) {
	if !t.Configured() {
		return NewClient(timeout), nil
	}

	cfg, err := t.Config()
	if err != nil {
		return nil, err
	}

	// Clone the default transport rather than building one, so the connection
	// pooling, proxy handling and HTTP/2 the standard library sets up are kept.
	// A bare &http.Transport{} silently loses all of it.
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, &types.TLSConfigError{
			Message: "http.DefaultTransport is not *http.Transport",
		}
	}
	tr := transport.Clone()
	tr.TLSClientConfig = cfg

	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: Check,
		Transport:     tr,
	}, nil
}
