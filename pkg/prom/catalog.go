package prom

import (
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
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
	// Aggregate wraps the selector in a PromQL aggregation, "sum" being the
	// only one used so far.
	//
	// It exists because a chart's meaning is not always one series per label
	// set. The CPU power chart is headed "Total Power Draw" and the live one
	// sums every package; without this the historical one drew a line PER
	// package, so on a two-socket host choosing a range silently replaced the
	// total with its parts, under an unchanged heading. A chart that means
	// something different in historical mode is worse than one that has no
	// history at all.
	//
	// A literal from this file, never from a request -- the same rule as
	// Matchers.
	Aggregate string
}

// catalog is ordered so the UI's metric picker reads top-down in the order a
// person looks at a machine: load first, then what it is doing to the hardware.
var catalog = []Metric{
	{Key: "cpu", Name: "sysmon_cpu_usage_percent", Label: "CPU", Unit: "percent"},
	{Key: "load1", Name: "sysmon_load1", Label: "Load (1m)", Unit: "count"},
	{Key: "memory", Name: "sysmon_memory_used_percent", Label: "Memory", Unit: "percent"},
	{Key: "swap", Name: "sysmon_swap_used_percent", Label: "Swap", Unit: "percent"},
	// Both halves of the pressure signal. "full" is the one worth alerting on,
	// and "some" is the one the live dashboard already plots -- a range picker
	// that could not draw the chart the operator was just looking at would be
	// the obvious gap.
	{
		Key: "psi_cpu_some", Name: "sysmon_psi_some_avg10", Matchers: []string{`resource="cpu"`},
		Label: "CPU pressure (some)", Unit: "percent",
	},
	{
		Key: "psi_memory_some", Name: "sysmon_psi_some_avg10", Matchers: []string{`resource="memory"`},
		Label: "Memory pressure (some)", Unit: "percent",
	},
	{
		Key: "psi_io_some", Name: "sysmon_psi_some_avg10", Matchers: []string{`resource="io"`},
		Label: "IO pressure (some)", Unit: "percent",
	},
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
	// The same series, split the two ways the panels draw them. The regular
	// expressions are literals from this file and never from a request, which
	// is what makes them safe here -- see Expr.
	{
		// Summed, and NOT split by source. The chart this feeds is headed
		// "Total Power Draw" and its live line is the sum of every package;
		// one line per package here would mean something different from the
		// same chart a moment earlier, on any host with more than one socket.
		Key: "cpu_power", Name: "sysmon_power_watts", Matchers: []string{`source=~"cpu:.*"`},
		Label: "CPU package power", Unit: "watts", Aggregate: "sum",
	},
	{
		Key: "gpu_power", Name: "sysmon_power_watts", Matchers: []string{`source=~"gpu:.*"`},
		Label: "GPU power", Unit: "watts", SplitBy: "source",
	},
	{Key: "gpu_util", Name: "sysmon_gpu_utilization_percent", Label: "GPU utilisation", Unit: "percent", SplitBy: "gpu"},
	{Key: "gpu_memory", Name: "sysmon_gpu_memory_percent", Label: "GPU memory", Unit: "percent", SplitBy: "gpu"},
	{Key: "gpu_temperature", Name: "sysmon_gpu_temperature_celsius", Label: "GPU temperature", Unit: "celsius", SplitBy: "gpu"},
	{
		Key: "npu_power", Name: "sysmon_power_watts", Matchers: []string{`source=~"npu:.*"`},
		Label: "NPU power", Unit: "watts", SplitBy: "source",
	},
	// Two charts, because an accelerator has two different numbers that look
	// like one. The label of each says which: "powered up" is runtime-PM
	// residency and "utilisation" is engine time from fdinfo. A single chart
	// titled "NPU" would leave the reader to guess, and on an idle-but-awake
	// NPU the two disagree by ~100 points.
	{
		Key: "npu_residency", Name: "sysmon_npu_residency_percent",
		Label: "NPU powered up", Unit: "percent", SplitBy: "npu",
	},
	{
		Key: "npu_util", Name: "sysmon_npu_utilization_percent",
		Label: "NPU utilisation", Unit: "percent", SplitBy: "npu",
	},
	{
		Key: "npu_temperature", Name: "sysmon_npu_temperature_celsius",
		Label: "NPU temperature", Unit: "celsius", SplitBy: "npu",
	},
	{
		Key: "npu_clients", Name: "sysmon_npu_clients",
		Label: "NPU clients", Unit: "count", SplitBy: "npu",
	},
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

// ErrInvalidInstance reports a node name that cannot appear in a selector.
// The rule it broke is pkg/nodename's, which is also what the configuration
// loader holds every fleet peer name to.
type ErrInvalidInstance struct {
	Instance string
}

func (e *ErrInvalidInstance) Error() string {
	return "prom: " + quote(e.Instance) + " is not a usable node name: " + nodename.Rule
}

// quote renders an untrusted value for an error message; see nodename.Quote.
func quote(s string) string { return nodename.Quote(s) }

// labelValueEscaper escapes a string for use inside a double-quoted PromQL
// string literal.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// Expr builds the PromQL for one metric on one node.
//
// instance is the only caller-supplied part. It is validated against
// nodename's rule AND escaped. The rule alone already leaves nothing to escape;
// the escaping is underneath it rather than instead of it, so the matcher stays
// a single literal even if the rule is ever widened (ruling on #37, 2026-09-02).
func (m Metric) Expr(instance string) (string, error) {
	if err := nodename.Validate(instance); err != nil {
		return "", &ErrInvalidInstance{Instance: instance}
	}
	instance = labelValueEscaper.Replace(instance)
	matchers := make([]string, 0, len(m.Matchers)+1)
	matchers = append(matchers, `instance="`+instance+`"`)
	matchers = append(matchers, m.Matchers...)
	selector := m.Name + "{" + strings.Join(matchers, ",") + "}"
	if m.Aggregate != "" {
		return m.Aggregate + "(" + selector + ")", nil
	}
	return selector, nil
}
