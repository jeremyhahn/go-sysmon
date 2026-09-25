package nodename

import (
	"errors"
	"strings"
	"testing"
)

// TestValidate_AcceptsFleetNames covers the names a fleet actually uses:
// hostnames, dotted FQDNs, IPv4 addresses and short labels. Rejecting any of
// them would turn a working config into a startup failure.
func TestValidate_AcceptsFleetNames(t *testing.T) {
	for _, name := range []string{
		"mon0", "fs", "vm-host-1", "host.example.com", "a", "node_1",
		"10.0.0.5", "fe80", strings.Repeat("a", 253),
	} {
		if err := Validate(name); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", name, err)
		}
	}
}

// TestValidate_RejectsUnusableNames covers the shapes that cannot be an
// instance label: a URL (what an unnamed peer used to be called), anything that
// could close or extend a PromQL matcher, and the boundary cases of the
// pattern.
func TestValidate_RejectsUnusableNames(t *testing.T) {
	for _, name := range []string{
		"", " ", "http://db-01:8081", `mon0"}`, "mon0\\", "mon0\nx",
		"mon0 # c", ".lead", "-lead", "::1", strings.Repeat("a", 254),
	} {
		err := Validate(name)
		var invalid *InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("Validate(%q) = %v, want *InvalidError", name, err)
			continue
		}
		if invalid.Name != name {
			t.Errorf("InvalidError.Name = %q, want %q", invalid.Name, name)
		}
		// The message is what an operator reads at startup, so it has to say
		// what the rule is, not just that it was broken.
		if !strings.Contains(err.Error(), Rule) {
			t.Errorf("Validate(%q) error %q does not state the rule", name, err)
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("Validate(%q) error carries a raw newline", name)
		}
	}
}

// TestQuote_Neutralises checks the escaping used when an untrusted name is
// echoed: it must be bounded and unable to break its quotes or forge a line.
func TestQuote_Neutralises(t *testing.T) {
	if got, want := Quote("a\"b\\c\nd"), `"a.b.c.d"`; got != want {
		t.Errorf("Quote() = %s, want %s", got, want)
	}
	long := Quote(strings.Repeat("x", 100))
	if want := `"` + strings.Repeat("x", 64) + `..."`; long != want {
		t.Errorf("Quote(long) = %s, want %s", long, want)
	}
	if got := Quote(""); got != `""` {
		t.Errorf("Quote(\"\") = %s, want \"\"", got)
	}
}
