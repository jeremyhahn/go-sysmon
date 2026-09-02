package prom

import (
	"errors"
	"strings"
	"testing"
)

func TestCatalog_KeysAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, m := range Catalog() {
		if seen[m.Key] {
			t.Errorf("duplicate catalog key %q: Lookup would return whichever came first", m.Key)
		}
		seen[m.Key] = true
		if m.Name == "" || m.Label == "" || m.Unit == "" {
			t.Errorf("catalog entry %q is incomplete: %+v", m.Key, m)
		}
	}
}

func TestCatalog_IsACopy(t *testing.T) {
	first := Catalog()
	first[0].Key = "mutated"
	if Catalog()[0].Key == "mutated" {
		t.Fatal("Catalog() hands out the package's own slice; a caller can rewrite the allowlist")
	}
}

func TestLookup(t *testing.T) {
	m, err := Lookup("cpu")
	if err != nil {
		t.Fatalf("Lookup(cpu) = %v", err)
	}
	if m.Name != "sysmon_cpu_usage_percent" {
		t.Errorf("Lookup(cpu).Name = %q", m.Name)
	}

	var unknown *ErrUnknownMetric
	if _, err := Lookup("sysmon_cpu_usage_percent"); !errors.As(err, &unknown) {
		// The key is not the metric name, deliberately: several keys share a
		// metric and differ only by matcher.
		t.Fatalf("Lookup by metric name = %v, want *ErrUnknownMetric", err)
	}
	if _, err := Lookup(""); !errors.As(err, &unknown) {
		t.Fatalf("Lookup(\"\") = %v, want *ErrUnknownMetric", err)
	}
}

func TestMetric_Expr(t *testing.T) {
	m, err := Lookup("psi_memory")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Expr("mon0")
	if err != nil {
		t.Fatalf("Expr() = %v", err)
	}
	want := `sysmon_psi_full_avg10{instance="mon0",resource="memory"}`
	if got != want {
		t.Fatalf("Expr() = %q, want %q", got, want)
	}
}

// TestMetric_Expr_RejectsInjection is the security property of this package.
//
// The dashboard is unauthenticated, and the node name is the one part of the
// expression a request supplies. Each input below either closes the matcher and
// appends a different query, or smuggles one through a comment or a newline. If
// any is accepted, GET /api/history becomes an open PromQL proxy for everything
// in the time-series database.
func TestMetric_Expr_RejectsInjection(t *testing.T) {
	m, err := Lookup("cpu")
	if err != nil {
		t.Fatal(err)
	}

	bad := []string{
		`mon0"} or on() group_left() node_exporter_build_info{`,
		`mon0"}`,
		`mon0" or foo="`,
		"mon0\\",
		"mon0\nsysmon_up",
		"mon0 # comment",
		`mon0{`,
		`.*`,
		`mon0"} or vector(1) #`,
		"",
		" ",
		".leadingdot",
		"-leadingdash",
		strings.Repeat("a", 254),
	}
	for _, instance := range bad {
		if got, err := m.Expr(instance); err == nil {
			t.Errorf("Expr(%q) = %q, want a rejection", instance, got)
		} else {
			var invalid *ErrInvalidInstance
			if !errors.As(err, &invalid) {
				t.Errorf("Expr(%q) = %v, want *ErrInvalidInstance", instance, err)
			}
			// The rejection is logged and returned to a caller, so it must not
			// echo the payload unescaped.
			if strings.Contains(invalid.Error(), "\n") {
				t.Errorf("Expr(%q) error message carries a raw newline", instance)
			}
		}
	}

	good := []string{"mon0", "fs", "vm-host-1", "host.example.com", "a", "node_1", "10.0.0.5", "fe80::1"[:4]}
	for _, instance := range good {
		if _, err := m.Expr(instance); err != nil {
			t.Errorf("Expr(%q) = %v, want a usable name", instance, err)
		}
	}
}
