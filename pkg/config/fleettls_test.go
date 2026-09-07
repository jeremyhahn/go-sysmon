package config

import "testing"

// TestFleetTLS_ZeroValueIsCompatible pins the one property the peer-transport
// feature rests on.
//
// Every deployed fleet speaks plain http. A config.Fleet is not always built by
// Defaults(): a test, an embedder, or any literal in code gets the ZERO value,
// and if the zero value meant "refuse plaintext" then all of those would refuse
// every peer at once -- silently, and only at the next deploy.
//
// This was not hypothetical. The field was first written as AllowPlaintext,
// whose zero value is false, and seven fleet tests plus one server test went
// red because a Fleet literal now denied its own peers. Phrasing it as opting
// IN to the requirement is what makes the zero value the safe one.
func TestFleetTLS_ZeroValueIsCompatible(t *testing.T) {
	var zero FleetTLS

	if zero.RequireTLS {
		t.Error("zero FleetTLS requires TLS; a Fleet built in code would refuse every http peer")
	}
	if zero.InsecureSkipVerify {
		t.Error("zero FleetTLS skips verification; the unconfigured default must verify")
	}
	if zero.CAFile != "" || zero.CertFile != "" || zero.KeyFile != "" {
		t.Errorf("zero FleetTLS names a file: %+v", zero)
	}
}

// TestDefaults_FleetTLSMatchesTheZeroValue keeps the shipped defaults and the
// zero value from drifting apart. If they ever disagree, one of the two paths
// into a Fleet is doing something the other is not, which is exactly the class
// of difference nobody notices until a deploy.
func TestDefaults_FleetTLSMatchesTheZeroValue(t *testing.T) {
	var zero FleetTLS
	if got := Defaults().Fleet.TLS; got != zero {
		t.Errorf("Defaults().Fleet.TLS = %+v, want the zero value %+v", got, zero)
	}
}

// TestBootOnly_CoversTheTLSMaterialButNotTheEnforcement is the boundary the
// docs promise: the certificates are baked into the poller's HTTP client at
// startup and cannot change without a restart, while require_tls is read per
// peer per cycle and therefore CAN be turned on by a reload.
//
// Listing require_tls as boot-only would tell an operator a reload had not
// taken effect when it had.
func TestBootOnly_CoversTheTLSMaterialButNotTheEnforcement(t *testing.T) {
	boot := make(map[string]bool)
	for _, k := range bootOnlyKeys() {
		boot[k] = true
	}

	for _, k := range []string{
		"fleet.tls.ca_file",
		"fleet.tls.cert_file",
		"fleet.tls.key_file",
		"fleet.tls.insecure_skip_verify",
	} {
		if !boot[k] {
			t.Errorf("%s is not boot-only, but it is resolved into the client at startup", k)
		}
	}

	if boot["fleet.tls.require_tls"] {
		t.Error("fleet.tls.require_tls is listed boot-only, but it is read every cycle and a reload does apply it")
	}
}

// TestScalarKeys_RenderEveryFleetTLSSetting guards the settings view. A key
// listed but not rendered shows as an empty string, which for a security
// setting reads as "not configured" -- the most misleading thing it could say.
func TestScalarKeys_RenderEveryFleetTLSSetting(t *testing.T) {
	c := Defaults()
	c.Fleet.TLS = FleetTLS{
		CAFile:             "/etc/go-sysmon/fleet-ca.pem",
		CertFile:           "/etc/go-sysmon/node.pem",
		KeyFile:            "/etc/go-sysmon/node-key.pem",
		InsecureSkipVerify: true,
		RequireTLS:         true,
	}

	listed := make(map[string]bool)
	for _, k := range scalarKeys() {
		listed[k] = true
	}

	for _, k := range []string{
		"fleet.tls.ca_file",
		"fleet.tls.cert_file",
		"fleet.tls.key_file",
		"fleet.tls.insecure_skip_verify",
		"fleet.tls.require_tls",
	} {
		if !listed[k] {
			t.Errorf("%s is missing from scalarKeys(); the settings view will not show it", k)
			continue
		}
		if c.value(k) == "" {
			t.Errorf("value(%q) is empty for a set value; a security setting must not read as unconfigured", k)
		}
	}

	// The two booleans are shown verbatim so a fleet running unverified says so.
	if got := c.value("fleet.tls.insecure_skip_verify"); got != "true" {
		t.Errorf("value(insecure_skip_verify) = %q, want %q", got, "true")
	}
	if got := c.value("fleet.tls.require_tls"); got != "true" {
		t.Errorf("value(require_tls) = %q, want %q", got, "true")
	}

	// The paths are redacted, like the server's own TLS paths: where a node
	// keeps its private key is not for an unauthenticated dashboard to hand out.
	if got := c.value("fleet.tls.key_file"); got == "/etc/go-sysmon/node-key.pem" {
		t.Errorf("value(key_file) = %q; the path must be redacted, not echoed", got)
	}
}
