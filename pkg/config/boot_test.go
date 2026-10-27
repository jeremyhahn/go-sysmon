package config

import "testing"

// changedEverywhere is the defaults with every boot-only setting changed. A
// new entry in bootSettings fails TestBootSettings_EachEntryComparesAndPinsItsOwnKey
// until it is changed here too.
func changedEverywhere() Config {
	c := Defaults()
	c.Server.Addr = "127.0.0.1:19090"
	c.Server.TLSCert = "/etc/go-sysmon/next-cert.pem"
	c.Server.TLSKey = "/etc/go-sysmon/next-key.pem"
	c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	c.Server.Watch = !c.Server.Watch
	c.Fleet.TLS.CAFile = "/etc/go-sysmon/next-fleet-ca.pem"
	c.Fleet.TLS.CertFile = "/etc/go-sysmon/next-fleet-cert.pem"
	c.Fleet.TLS.KeyFile = "/etc/go-sysmon/next-fleet-key.pem"
	c.Fleet.TLS.InsecureSkipVerify = !c.Fleet.TLS.InsecureSkipVerify
	c.BMC.RedfishURL = "https://bmc.example.test"
	c.BMC.Username = "monitor"
	c.BMC.PasswordFile = "/etc/go-sysmon/bmc-password"
	c.BMC.CAFile = "/etc/go-sysmon/bmc-ca.pem"
	c.BMC.InsecureSkipVerify = !c.BMC.InsecureSkipVerify
	c.BMC.TimeoutMs += 1000
	return c
}

// TestBootSettings_EachEntryComparesAndPinsItsOwnKey holds each boot-only
// entry to the field its key names in the settings view: an entry whose
// accessor addressed another field would defer one setting and pin another,
// leaving the view to report a value that is not in force.
func TestBootSettings_EachEntryComparesAndPinsItsOwnKey(t *testing.T) {
	boot := Defaults()
	for _, s := range bootSettings {
		next := changedEverywhere()
		if next.raw(s.key) == boot.raw(s.key) {
			t.Errorf("%s: changedEverywhere does not change it", s.key)
			continue
		}
		if !s.changed(&next, &boot) {
			t.Errorf("%s: changed = false for %q against the boot %q", s.key, next.raw(s.key), boot.raw(s.key))
		}
		s.pin(&next, &boot)
		if got, want := next.raw(s.key), boot.raw(s.key); got != want {
			t.Errorf("%s = %q after pin, want the boot %q", s.key, got, want)
		}
		untouched := changedEverywhere()
		for _, other := range bootSettings {
			if other.key == s.key {
				continue
			}
			if got, want := next.raw(other.key), untouched.raw(other.key); got != want {
				t.Errorf("pinning %s changed %s to %q, want %q", s.key, other.key, got, want)
			}
		}
	}
}

// TestBootSettings_AnUnchangedConfigDefersNothing is a reload of the file the
// process booted from: nothing needs a restart.
func TestBootSettings_AnUnchangedConfigDefersNothing(t *testing.T) {
	boot := changedEverywhere()
	next := changedEverywhere()
	for _, s := range bootSettings {
		if s.changed(&next, &boot) {
			t.Errorf("%s: changed = true for equal values %q", s.key, next.raw(s.key))
		}
	}
}

// TestBootOnlyKeys_AreSettingsTheViewShows refuses a boot-only key the
// settings view does not know, which a misspelt entry would be.
func TestBootOnlyKeys_AreSettingsTheViewShows(t *testing.T) {
	known := make(map[string]bool)
	for _, key := range scalarKeys() {
		known[key] = true
	}
	seen := make(map[string]bool)
	for _, key := range bootOnlyKeys() {
		if !known[key] {
			t.Errorf("boot-only key %s is not a setting the view shows", key)
		}
		if seen[key] {
			t.Errorf("boot-only key %s is listed twice", key)
		}
		seen[key] = true
	}
}
