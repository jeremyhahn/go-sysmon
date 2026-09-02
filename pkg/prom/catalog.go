package prom

import (
	"regexp"
	"strings"
)

// The history catalog.
//
// The dashboard is unauthenticated -- that is a known property of this service,
// tracked as issue #16 -- so the history endpoint maps a fixed set of keys to
// PromQL rather than forwarding an expression from the browser. Passing a
// caller's expression through would turn go-sysmon into an open query proxy for
// everything in the time-series database, including whatever else writes to it,
// on a port that anything able to route to the host can reach.
//
// Every Name below must be a metric pkg/server/metrics.go actually writes.
// A key naming a metric this node never produces would draw an empty chart and
// look like a node with no history rather than a mistake in this table, so
// pkg/server has a test that checks the two against each other.

// Metric is one series the dashboard can ask for history of.
type Metric struct {
	// Key is the stable identifier used by the API and the UI. It is not the
	// metric name: several keys share a metric and differ only by matcher.
	Key string
	// Name is the Prometheus metric name.
	Name string
	// Matchers are extra label selectors, as already-safe literal strings from
	// this file -- never from a request.
	Matchers []string
	// Label is the series title shown in the UI.
	Label string
	// Unit is how the values are formatted: "percent", "bytes_per_second",
	// "celsius", "watts", "count".
	Unit string
	// SplitBy is the label that distinguishes one returned series from another
	// -- device, interface, gpu. Empty when a node yields a single series.
	SplitBy string
}

// catalog is ordered so the UI's metric picker reads top-down in the order a
// person looks at a machine: load first, then what it is doing to the hardware.
var catalog = []Metric{
	{Key: "cpu", Name: "sysmon_cpu_usage_percent", Label: "CPU", Unit: "percent"},
	{Key: "load1", Name: "sysmon_load1", Label: "Load (1m)", Unit: "count"},
	{Key: "memory", Name: "sysmon_memory_used_percent", Label: "Memory", Unit: "percent"},
	{Key: "swap", Name: "sysmon_swap_used_percent", Label: "Swap", Unit: "percent"},
	{
		Key: "psi_cpu", Name: "sysmon_psi_full_avg10", Matchers: []string{`resource="cpu"`},
		Label: "CPU pressure (full)", Unit: "percent",
	},
	{
		Key: "psi_memory", Name: "sysmon_psi_full_avg10", Matchers: []string{`resource="memory"`},
		Label: "Memory pressure (full)", Unit: "percent",
	},
	{
		Key: "psi_io", Name: "sysmon_psi_full_avg10", Matchers: []string{`resource="io"`},
		Label: "IO pressure (full)", Unit: "percent",
	},
	{Key: "disk_used", Name: "sysmon_disk_used_percent", Label: "Filesystem used", Unit: "percent", SplitBy: "device"},
	{Key: "disk_util", Name: "sysmon_disk_util_percent", Label: "Disk busy", Unit: "percent", SplitBy: "device"},
	{Key: "disk_read", Name: "sysmon_disk_read_bytes_rate", Label: "Disk read", Unit: "bytes_per_second", SplitBy: "device"},
	{Key: "disk_write", Name: "sysmon_disk_write_bytes_rate", Label: "Disk write", Unit: "bytes_per_second", SplitBy: "device"},
	{Key: "disk_temperature", Name: "sysmon_disk_temperature_celsius", Label: "Drive temperature", Unit: "celsius", SplitBy: "device"},
	{Key: "net_rx", Name: "sysmon_network_receive_bytes_rate", Label: "Network in", Unit: "bytes_per_second", SplitBy: "interface"},
	{Key: "net_tx", Name: "sysmon_network_transmit_bytes_rate", Label: "Network out", Unit: "bytes_per_second", SplitBy: "interface"},
	{Key: "cpu_temperature", Name: "sysmon_cpu_temperature_celsius", Label: "CPU temperature", Unit: "celsius", SplitBy: "core"},
	{Key: "power", Name: "sysmon_power_watts", Label: "Power draw", Unit: "watts", SplitBy: "source"},
	{Key: "gpu_util", Name: "sysmon_gpu_utilization_percent", Label: "GPU utilisation", Unit: "percent", SplitBy: "gpu"},
	{Key: "gpu_temperature", Name: "sysmon_gpu_temperature_celsius", Label: "GPU temperature", Unit: "celsius", SplitBy: "gpu"},
	{
		Key: "alerts_critical", Name: "sysmon_alerts_firing", Matchers: []string{`severity="critical"`},
		Label: "Critical alerts", Unit: "count",
	},
	{
		Key: "alerts_warning", Name: "sysmon_alerts_firing", Matchers: []string{`severity="warning"`},
		Label: "Warning alerts", Unit: "count",
	},
}

// Catalog returns every metric history is published for, in display order.
func Catalog() []Metric {
	return append([]Metric(nil), catalog...)
}

// Lookup finds a metric by key.
func Lookup(key string) (Metric, error) {
	for _, m := range catalog {
		if m.Key == key {
			return m, nil
		}
	}
	return Metric{}, &ErrUnknownMetric{Key: key}
}

// instancePattern is what a node name may look like. Fleet names are hostnames
// or short labels, so this is deliberately narrower than PromQL would allow:
// with the value constrained to characters that carry no meaning inside a
// matcher, building the selector by concatenation cannot be turned into a
// different query. Escaping alone would also work, and would leave the safety
// of every future call site depending on someone remembering to do it.
var instancePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

// ErrInvalidInstance reports a node name that cannot appear in a selector.
type ErrInvalidInstance struct {
	Instance string
}

func (e *ErrInvalidInstance) Error() string {
	return "prom: " + quote(e.Instance) + " is not a usable node name"
}

func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return `"` + strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' {
			return '.'
		}
		return r
	}, s) + `"`
}

// Expr builds the PromQL for one metric on one node.
//
// instance is the only caller-supplied part, and it is validated rather than
// escaped; see instancePattern.
func (m Metric) Expr(instance string) (string, error) {
	if !instancePattern.MatchString(instance) {
		return "", &ErrInvalidInstance{Instance: instance}
	}
	matchers := make([]string, 0, len(m.Matchers)+1)
	matchers = append(matchers, `instance="`+instance+`"`)
	matchers = append(matchers, m.Matchers...)
	return m.Name + "{" + strings.Join(matchers, ",") + "}", nil
}
