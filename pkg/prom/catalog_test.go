package prom

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
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

	var unknown *UnknownMetricError
	if _, err := Lookup("sysmon_cpu_usage_percent"); !errors.As(err, &unknown) {
		// The key is not the metric name, deliberately: several keys share a
		// metric and differ only by matcher.
		t.Fatalf("Lookup by metric name = %v, want *UnknownMetricError", err)
	}
	if _, err := Lookup(""); !errors.As(err, &unknown) {
		t.Fatalf("Lookup(\"\") = %v, want *UnknownMetricError", err)
	}
}

func TestMetric_Expr(t *testing.T) {
	m, err := Lookup("psi_memory")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Expr("mon0", testStep, testSampleInterval)
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
		if got, err := m.Expr(instance, testStep, testSampleInterval); err == nil {
			t.Errorf("Expr(%q) = %q, want a rejection", instance, got)
		} else {
			var invalid *InvalidInstanceError
			if !errors.As(err, &invalid) {
				t.Errorf("Expr(%q) = %v, want *InvalidInstanceError", instance, err)
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
		if _, err := m.Expr(instance, testStep, testSampleInterval); err != nil {
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
	got, err := m.Expr("mon0", testStep, testSampleInterval)
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

	expr, err := m.Expr("mon0", testStep, testSampleInterval)
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

// TestLabelValueEscaper covers the escaping Expr applies beneath validation.
// Validation leaves nothing for it to do today, which is exactly why it is
// tested on its own: were the name rule ever widened, this is what keeps the
// instance matcher a single string literal.
func TestLabelValueEscaper(t *testing.T) {
	cases := map[string]string{
		"mon0":          "mon0",
		`a"b`:           `a\"b`,
		`a\b`:           `a\\b`,
		"a\nb":          `a\nb`,
		`x"} or up{y="`: `x\"} or up{y=\"`,
	}
	for in, want := range cases {
		if got := labelValueEscaper.Replace(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestInvalidInstanceError_StatesTheRule. The history endpoint returns this text
// as its 400, so it has to say what a usable name looks like.
func TestInvalidInstanceError_StatesTheRule(t *testing.T) {
	m, err := Lookup("cpu")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Expr("http://db-01:8081", testStep, testSampleInterval)
	if err == nil || !strings.Contains(err.Error(), nodename.Rule) {
		t.Errorf("Expr(url) error = %v, want it to state %q", err, nodename.Rule)
	}
}

// The step and sample interval the gauge tests query at. A gauge ignores both,
// which TestCatalog_GaugeQueriesAreUnchanged holds it to.
const (
	testStep           = time.Minute
	testSampleInterval = 15 * time.Second
)

// TestCatalog_GaugeQueriesAreUnchanged pins every gauge entry's query to the
// exact string it produced before counters existed, at several steps.
//
// Counters changed Expr's signature, and a gauge that picked up a rate() or a
// range by accident would still return a well-formed query -- Prometheus would
// answer a rate of a percentage without complaint, and the chart would draw a
// derivative under the metric's own name. This table is the literal output of
// the catalog before the change.
func TestCatalog_GaugeQueriesAreUnchanged(t *testing.T) {
	want := map[string]string{
		"cpu":              `sysmon_cpu_usage_percent{instance="mon0"}`,
		"load1":            `sysmon_load1{instance="mon0"}`,
		"memory":           `sysmon_memory_used_percent{instance="mon0"}`,
		"swap":             `sysmon_swap_used_percent{instance="mon0"}`,
		"psi_cpu_some":     `sysmon_psi_some_avg10{instance="mon0",resource="cpu"}`,
		"psi_memory_some":  `sysmon_psi_some_avg10{instance="mon0",resource="memory"}`,
		"psi_io_some":      `sysmon_psi_some_avg10{instance="mon0",resource="io"}`,
		"psi_cpu":          `sysmon_psi_full_avg10{instance="mon0",resource="cpu"}`,
		"psi_memory":       `sysmon_psi_full_avg10{instance="mon0",resource="memory"}`,
		"psi_io":           `sysmon_psi_full_avg10{instance="mon0",resource="io"}`,
		"disk_used":        `sysmon_disk_used_percent{instance="mon0"}`,
		"disk_util":        `sysmon_disk_util_percent{instance="mon0"}`,
		"disk_read":        `sysmon_disk_read_bytes_rate{instance="mon0"}`,
		"disk_write":       `sysmon_disk_write_bytes_rate{instance="mon0"}`,
		"disk_temperature": `sysmon_disk_temperature_celsius{instance="mon0"}`,
		"net_rx":           `sysmon_network_receive_bytes_rate{instance="mon0"}`,
		"net_tx":           `sysmon_network_transmit_bytes_rate{instance="mon0"}`,
		// Changed deliberately with the package label: a series without one
		// cannot say which package it was, and is not charted.
		"cpu_temperature": `sysmon_cpu_temperature_celsius{instance="mon0",package!=""}`,
		"power":           `sysmon_power_watts{instance="mon0"}`,
		"cpu_power":       `sum(sysmon_power_watts{instance="mon0",source=~"cpu:.*"})`,
		"gpu_power":       `sysmon_power_watts{instance="mon0",source=~"gpu:.*"}`,
		"gpu_util":        `sysmon_gpu_utilization_percent{instance="mon0"}`,
		"gpu_memory":      `sysmon_gpu_memory_percent{instance="mon0"}`,
		"gpu_temperature": `sysmon_gpu_temperature_celsius{instance="mon0"}`,
		"npu_power":       `sysmon_power_watts{instance="mon0",source=~"npu:.*"}`,
		"npu_residency":   `sysmon_npu_residency_percent{instance="mon0"}`,
		"npu_util":        `sysmon_npu_utilization_percent{instance="mon0"}`,
		"npu_temperature": `sysmon_npu_temperature_celsius{instance="mon0"}`,
		"npu_clients":     `sysmon_npu_clients{instance="mon0"}`,
		"alerts_critical": `sysmon_alerts_firing{instance="mon0",severity="critical"}`,
		"alerts_warning":  `sysmon_alerts_firing{instance="mon0",severity="warning"}`,
	}

	gauges := 0
	for _, m := range Catalog() {
		if m.Counter {
			continue
		}
		gauges++
		expected, ok := want[m.Key]
		if !ok {
			t.Errorf("gauge %q has no pinned query; add it to this table", m.Key)
			continue
		}
		for _, step := range []time.Duration{time.Second, testStep, 24 * time.Hour} {
			for _, sample := range []time.Duration{0, time.Second, testSampleInterval} {
				got, err := m.Expr("mon0", step, sample)
				if err != nil {
					t.Errorf("Expr(%q) at step %v, sample %v = %v", m.Key, step, sample, err)
					continue
				}
				if got != expected {
					t.Errorf("Expr(%q) at step %v, sample %v = %q, want %q",
						m.Key, step, sample, got, expected)
				}
			}
		}
	}
	if gauges != len(want) {
		t.Errorf("%d gauge entries, %d pinned: a pinned gauge was removed or became a counter",
			gauges, len(want))
	}
}

// TestMetric_Expr_CounterIsARate. A counter is drawn as its per-second rate
// over a window sized from the step and the measured sample gap, never as the
// cumulative total -- and the swap counters, which count pages, are scaled to
// bytes by the node's own page size.
func TestMetric_Expr_CounterIsARate(t *testing.T) {
	const scale = ` * sysmon_memory_page_size_bytes{instance="mon0"}`
	cases := []struct {
		key       string
		step, gap time.Duration
		want      string
	}{
		// At a one-minute step the window is a step plus a gap.
		{"swap_out", time.Minute, 15 * time.Second,
			`rate(sysmon_swap_out_pages_total{instance="mon0"}[75s])` + scale +
				` and sysmon_swap_out_pages_total{instance="mon0"}`},
		// At a one-second step the floor holds it at two gaps.
		{"swap_in", time.Second, time.Minute,
			`rate(sysmon_swap_in_pages_total{instance="mon0"}[120s])` + scale +
				` and sysmon_swap_in_pages_total{instance="mon0"}`},
		// A sub-second remainder is written in milliseconds, not truncated.
		{"swap_in", 1500 * time.Millisecond, 250 * time.Millisecond,
			`rate(sysmon_swap_in_pages_total{instance="mon0"}[1750ms])` + scale +
				` and sysmon_swap_in_pages_total{instance="mon0"}`},
	}
	for _, c := range cases {
		m, err := Lookup(c.key)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", c.key, err)
		}
		got, err := m.Expr("mon0", c.step, c.gap)
		if err != nil {
			t.Fatalf("Expr(%q) = %v", c.key, err)
		}
		if got != c.want {
			t.Errorf("Expr(%q, step %v, gap %v) = %q, want %q", c.key, c.step, c.gap, got, c.want)
		}
	}
}

// TestMetric_Expr_CounterAggregatesTheRate. Summing counters and then taking
// the rate would turn one series' reset into a drop in the sum that rate()
// cannot recognise; the aggregation has to go outside, and outside the
// scaling too.
func TestMetric_Expr_CounterAggregatesTheRate(t *testing.T) {
	m := Metric{Name: "x_total", Counter: true, Aggregate: "sum"}
	got, err := m.Expr("mon0", time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Expr() = %v", err)
	}
	if want := `sum(rate(x_total{instance="mon0"}[75s]) and x_total{instance="mon0"})`; got != want {
		t.Fatalf("Expr() = %q, want %q", got, want)
	}

	m.ScaleBy = "x_scale"
	got, err = m.Expr("mon0", time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("Expr() = %v", err)
	}
	if want := `sum(rate(x_total{instance="mon0"}[75s]) * x_scale{instance="mon0"} and x_total{instance="mon0"})`; got != want {
		t.Fatalf("Expr() = %q, want %q", got, want)
	}
}

// TestMetric_Selector. The measured selector is the plain series, with none
// of the rate or scaling around it: the gap is a property of the counter's
// own samples.
func TestMetric_Selector(t *testing.T) {
	m, err := Lookup("swap_out")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Selector("mon0")
	if err != nil {
		t.Fatalf("Selector() = %v", err)
	}
	if want := `sysmon_swap_out_pages_total{instance="mon0"}`; got != want {
		t.Errorf("Selector() = %q, want %q", got, want)
	}
	var invalid *InvalidInstanceError
	if _, err := m.Selector(`mon0"}`); !errors.As(err, &invalid) {
		t.Errorf("Selector(injection) = %v, want *InvalidInstanceError", err)
	}
}

// TestMetric_Expr_CounterRejectsUnusableWindows. A counter's query depends on
// the step, so a step it cannot size a window from is refused as a bad range
// rather than sent to Prometheus as a malformed query.
func TestMetric_Expr_CounterRejectsUnusableWindows(t *testing.T) {
	m, err := Lookup("swap_out")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		step, gap time.Duration
	}{
		{"zero step", 0, 15 * time.Second},
		{"negative step", -time.Second, 15 * time.Second},
		{"zero gap: nothing measured", time.Minute, 0},
		{"negative gap", time.Minute, -time.Second},
		{"step overflows the window", math.MaxInt64, 15 * time.Second},
		{"gap overflows the floor", time.Minute, math.MaxInt64/2 + 1},
	}
	for _, c := range cases {
		got, err := m.Expr("mon0", c.step, c.gap)
		var bad *InvalidRangeError
		if !errors.As(err, &bad) {
			t.Errorf("%s: Expr() = %q, %v; want *InvalidRangeError", c.name, got, err)
		}
	}

	// The node name is still checked first, and still with its own error.
	var invalid *InvalidInstanceError
	if _, err := m.Expr(`mon0"}`, time.Minute, 15*time.Second); !errors.As(err, &invalid) {
		t.Errorf("Expr(injection) on a counter = %v, want *InvalidInstanceError", err)
	}
}

func TestRateWindow(t *testing.T) {
	cases := []struct {
		name      string
		step, gap time.Duration
		want      time.Duration
	}{
		{"step far below the gap is held at two gaps", time.Second, time.Minute, 2 * time.Minute},
		{"step equal to the gap", time.Minute, time.Minute, 2 * time.Minute},
		{"step past the gap overlaps by one gap", 61 * time.Second, time.Minute, 121 * time.Second},
		{"a 30-day range at the default resolution", 72 * time.Minute, 15 * time.Second, 72*time.Minute + 15*time.Second},
	}
	for _, c := range cases {
		got, err := RateWindow(c.step, c.gap)
		if err != nil {
			t.Errorf("%s: RateWindow = %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: RateWindow(%v, %v) = %v, want %v", c.name, c.step, c.gap, got, c.want)
		}
	}
}

func TestRateWindow_RejectsUnusableInputs(t *testing.T) {
	for _, c := range []struct{ step, gap time.Duration }{
		{0, time.Second},
		{time.Second, 0},
		{-1, -1},
		{math.MaxInt64 - time.Second + 1, time.Second},
	} {
		if got, err := RateWindow(c.step, c.gap); err == nil {
			t.Errorf("RateWindow(%v, %v) = %v, want an error", c.step, c.gap, got)
		}
	}
}

func TestPromDuration(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute:                        "60s",
		75 * time.Second:                   "75s",
		1750 * time.Millisecond:            "1750ms",
		time.Millisecond + time.Nanosecond: "2ms",
		72*time.Minute + 15*time.Second:    "4335s",
		time.Second + 500*time.Microsecond: "1001ms",
	}
	for in, want := range cases {
		if got := promDuration(in); got != want {
			t.Errorf("promDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestCatalog_CountersAreRates. A counter entry draws a rate, so its unit
// must be a per-second one; the swap counters are drawn in bytes, scaled by
// the page size.
func TestCatalog_CountersAreRates(t *testing.T) {
	counters := 0
	for _, m := range Catalog() {
		if !m.Counter {
			if m.ScaleBy != "" {
				t.Errorf("gauge %q is scaled; only a counter's rate is", m.Key)
			}
			continue
		}
		counters++
		if !strings.HasSuffix(string(m.Unit), "_per_second") {
			t.Errorf("counter %q has unit %q, want a per-second unit", m.Key, m.Unit)
		}
		if !strings.HasSuffix(m.Name, "_total") {
			t.Errorf("counter %q reads %q, which is not named as a counter", m.Key, m.Name)
		}
	}
	for _, key := range []string{"swap_in", "swap_out"} {
		m, err := Lookup(key)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", key, err)
		}
		if !m.Counter || m.Unit != types.HistoryUnitBytesPerSecond || m.ScaleBy != "sysmon_memory_page_size_bytes" {
			t.Errorf("%q = counter %v, unit %q, scaled by %q; want a bytes/s rate scaled by the page size",
				key, m.Counter, m.Unit, m.ScaleBy)
		}
	}
	if counters == 0 {
		t.Fatal("the catalog has no counters")
	}
}

// TestMetric_Lookback. A gauge reads nothing before a range's start; a
// counter reads its rate window, which a caller bounding the scan must count.
func TestMetric_Lookback(t *testing.T) {
	gauge, err := Lookup("swap")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := gauge.Lookback(24*time.Hour, 0); err != nil || got != 0 {
		t.Errorf("gauge Lookback = %v, %v; want 0, nil", got, err)
	}

	counter, err := Lookup("swap_out")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := counter.Lookback(time.Minute, 15*time.Second); err != nil || got != 75*time.Second {
		t.Errorf("counter Lookback = %v, %v; want 75s", got, err)
	}
	var bad *InvalidRangeError
	if _, err := counter.Lookback(0, 15*time.Second); !errors.As(err, &bad) {
		t.Errorf("counter Lookback at step 0 = %v, want *InvalidRangeError", err)
	}
}

// TestMetric_Expr_ScaleMatchesTheWholeLabelSet. When external_labels are
// reloaded, a node's old and new series coexist for a lookback. Joined on
// instance alone, each counter series would match both page-size series and
// Prometheus would refuse the query as many-to-many; the default match on
// every label pairs each series with its own.
func TestMetric_Expr_ScaleMatchesTheWholeLabelSet(t *testing.T) {
	m, err := Lookup("swap_in")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Expr("mon0", time.Minute, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, modifier := range []string{"on(", "ignoring(", "group_left", "group_right"} {
		if strings.Contains(got, modifier) {
			t.Errorf("Expr = %q narrows the match with %s", got, modifier)
		}
	}
}

// TestMetric_Expr_CounterIsGatedOnItsPresence. A range selector ignores
// Prometheus' lookback, so a week-long window keeps producing the last rate
// long after a counter stopped being written -- and an unreadable counter is
// omitted on purpose, to leave a gap. The instant selector the rate is
// and-ed with does apply the lookback, so the gap survives.
func TestMetric_Expr_CounterIsGatedOnItsPresence(t *testing.T) {
	m, err := Lookup("swap_out")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Expr("mon0", 72*time.Minute, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := ` and sysmon_swap_out_pages_total{instance="mon0"}`; !strings.HasSuffix(got, want) {
		t.Errorf("Expr = %q, want it gated with %q", got, want)
	}
}
