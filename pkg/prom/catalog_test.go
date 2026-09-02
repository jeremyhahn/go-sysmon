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

// TestMetric_Expr_Aggregates. A chart whose live line is a sum must keep
// meaning the same thing when it is drawn from history.
func TestMetric_Expr_Aggregates(t *testing.T) {
	m := Metric{
		Name:      "sysmon_power_watts",
		Matchers:  []string{`source=~"cpu:.*"`},
		Aggregate: "sum",
	}
	got, err := m.Expr("mon0")
	if err != nil {
		t.Fatalf("Expr() = %v", err)
	}
	if want := `sum(sysmon_power_watts{instance="mon0",source=~"cpu:.*"})`; got != want {
		t.Fatalf("Expr() = %q, want %q", got, want)
	}
}

// TestCatalog_CPUPowerIsATotal pins the entry rather than the mechanism,
// because the mechanism is not what was wrong.
//
// The chart is headed "Total Power Draw" and its live line is the sum of every
// CPU package. The historical entry returned one series per package, so on a
// two-socket host choosing a date range silently replaced the total with its
// parts under an unchanged heading -- a chart that means something different
// in historical mode, which is worse than a chart with no history.
func TestCatalog_CPUPowerIsATotal(t *testing.T) {
	m, err := Lookup("cpu_power")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Aggregate != "sum" {
		t.Errorf("cpu_power aggregate = %q, want sum: the chart it feeds is a total", m.Aggregate)
	}
	if m.SplitBy != "" {
		t.Errorf("cpu_power splits by %q; a total has no parts to split into", m.SplitBy)
	}

	expr, err := m.Expr("mon0")
	if err != nil {
		t.Fatalf("Expr: %v", err)
	}
	if !strings.HasPrefix(expr, "sum(") {
		t.Errorf("cpu_power query = %q, want it summed", expr)
	}

	// The per-source chart is a different entry and must stay split, so this
	// is not a blanket change to every power series.
	whole, err := Lookup("power")
	if err != nil {
		t.Fatalf("Lookup power: %v", err)
	}
	if whole.Aggregate != "" || whole.SplitBy != "source" {
		t.Errorf("the per-source power entry = aggregate %q split %q, want it split by source",
			whole.Aggregate, whole.SplitBy)
	}
}
