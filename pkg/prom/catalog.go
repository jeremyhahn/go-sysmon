package prom

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
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
	// Unit is how the values are formatted, which also picks the chart's axis.
	// A Counter entry's unit is always a per-second one, because what it draws
	// is a rate.
	Unit types.HistoryUnit
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
	// Counter marks Name as a cumulative counter rather than a gauge. Its
	// history is drawn as rate(selector[window]), with the window derived
	// from the query step and the series' measured sample gap -- see
	// RateWindow and Querier.SampleGap.
	//
	// A raw counter charted over time is a line that only ever rises, and a
	// reboot's reset draws as a cliff. rate() is what turns it into the
	// question a chart answers -- how much is happening now -- and it is also
	// what treats the reset as a reset, which is why the metric is exported as
	// a counter in the first place (#135).
	Counter bool
	// ScaleBy names a per-node gauge the value is multiplied by, matched on
	// the full label set: the swap counters count pages, and are drawn in
	// bytes by multiplying their rate by the node's page size.
	//
	// A node that does not export the gauge -- a peer running a build from
	// before it existed -- matches nothing, so its chart is empty rather than
	// scaled by a guess. A literal from this file, never from a request.
	ScaleBy string
}

// catalog is ordered so the UI's metric picker reads top-down in the order a
// person looks at a machine: load first, then what it is doing to the hardware.
var catalog = []Metric{
	{Key: "cpu", Name: "sysmon_cpu_usage_percent", Label: "CPU", Unit: types.HistoryUnitPercent},
	{Key: "load1", Name: "sysmon_load1", Label: "Load (1m)", Unit: types.HistoryUnitCount},
	{Key: "memory", Name: "sysmon_memory_used_percent", Label: "Memory", Unit: types.HistoryUnitPercent},
	{Key: "swap", Name: "sysmon_swap_used_percent", Label: "Swap", Unit: types.HistoryUnitPercent},
	// Paging, next to occupancy: resident swap says how much is parked there,
	// and these say how hard the machine is moving it. The kernel counts
	// pages, and the page size is not the same everywhere -- 4 KiB on x86, 16
	// or 64 KiB on some arm64 kernels -- so each node's own page size, which
	// it exports, is what turns its rate into bytes.
	{
		Key: "swap_in", Name: "sysmon_swap_in_pages_total", Counter: true,
		ScaleBy: "sysmon_memory_page_size_bytes",
		Label:   "Swap in", Unit: types.HistoryUnitBytesPerSecond,
	},
	{
		Key: "swap_out", Name: "sysmon_swap_out_pages_total", Counter: true,
		ScaleBy: "sysmon_memory_page_size_bytes",
		Label:   "Swap out", Unit: types.HistoryUnitBytesPerSecond,
	},
	// Both halves of the pressure signal. "full" is the one worth alerting on,
	// and "some" is the one the live dashboard already plots -- a range picker
	// that could not draw the chart the operator was just looking at would be
	// the obvious gap.
	{
		Key: "psi_cpu_some", Name: "sysmon_psi_some_avg10", Matchers: []string{`resource="cpu"`},
		Label: "CPU pressure (some)", Unit: types.HistoryUnitPercent,
	},
	{
		Key: "psi_memory_some", Name: "sysmon_psi_some_avg10", Matchers: []string{`resource="memory"`},
		Label: "Memory pressure (some)", Unit: types.HistoryUnitPercent,
	},
	{
		Key: "psi_io_some", Name: "sysmon_psi_some_avg10", Matchers: []string{`resource="io"`},
		Label: "IO pressure (some)", Unit: types.HistoryUnitPercent,
	},
	{
		Key: "psi_cpu", Name: "sysmon_psi_full_avg10", Matchers: []string{`resource="cpu"`},
		Label: "CPU pressure (full)", Unit: types.HistoryUnitPercent,
	},
	{
		Key: "psi_memory", Name: "sysmon_psi_full_avg10", Matchers: []string{`resource="memory"`},
		Label: "Memory pressure (full)", Unit: types.HistoryUnitPercent,
	},
	{
		Key: "psi_io", Name: "sysmon_psi_full_avg10", Matchers: []string{`resource="io"`},
		Label: "IO pressure (full)", Unit: types.HistoryUnitPercent,
	},
	{Key: "disk_used", Name: "sysmon_disk_used_percent", Label: "Filesystem used", Unit: types.HistoryUnitPercent, SplitBy: "device"},
	{Key: "disk_util", Name: "sysmon_disk_util_percent", Label: "Disk busy", Unit: types.HistoryUnitPercent, SplitBy: "device"},
	{Key: "disk_read", Name: "sysmon_disk_read_bytes_rate", Label: "Disk read", Unit: types.HistoryUnitBytesPerSecond, SplitBy: "device"},
	{Key: "disk_write", Name: "sysmon_disk_write_bytes_rate", Label: "Disk write", Unit: types.HistoryUnitBytesPerSecond, SplitBy: "device"},
	{Key: "disk_temperature", Name: "sysmon_disk_temperature_celsius", Label: "Drive temperature", Unit: types.HistoryUnitCelsius, SplitBy: "device"},
	{Key: "net_rx", Name: "sysmon_network_receive_bytes_rate", Label: "Network in", Unit: types.HistoryUnitBytesPerSecond, SplitBy: "interface"},
	{Key: "net_tx", Name: "sysmon_network_transmit_bytes_rate", Label: "Network out", Unit: types.HistoryUnitBytesPerSecond, SplitBy: "interface"},
	{Key: "cpu_temperature", Name: "sysmon_cpu_temperature_celsius", Label: "CPU temperature", Unit: types.HistoryUnitCelsius, SplitBy: "core"},
	{Key: "power", Name: "sysmon_power_watts", Label: "Power draw", Unit: types.HistoryUnitWatts, SplitBy: "source"},
	// The same series, split the two ways the panels draw them. The regular
	// expressions are literals from this file and never from a request, which
	// is what makes them safe here -- see Expr.
	{
		// Summed, and NOT split by source. The chart this feeds is headed
		// "Total Power Draw" and its live line is the sum of every package;
		// one line per package here would mean something different from the
		// same chart a moment earlier, on any host with more than one socket.
		Key: "cpu_power", Name: "sysmon_power_watts", Matchers: []string{`source=~"cpu:.*"`},
		Label: "CPU package power", Unit: types.HistoryUnitWatts, Aggregate: "sum",
	},
	{
		Key: "gpu_power", Name: "sysmon_power_watts", Matchers: []string{`source=~"gpu:.*"`},
		Label: "GPU power", Unit: types.HistoryUnitWatts, SplitBy: "source",
	},
	{Key: "gpu_util", Name: "sysmon_gpu_utilization_percent", Label: "GPU utilisation", Unit: types.HistoryUnitPercent, SplitBy: "gpu"},
	{Key: "gpu_memory", Name: "sysmon_gpu_memory_percent", Label: "GPU memory", Unit: types.HistoryUnitPercent, SplitBy: "gpu"},
	{Key: "gpu_temperature", Name: "sysmon_gpu_temperature_celsius", Label: "GPU temperature", Unit: types.HistoryUnitCelsius, SplitBy: "gpu"},
	{
		Key: "npu_power", Name: "sysmon_power_watts", Matchers: []string{`source=~"npu:.*"`},
		Label: "NPU power", Unit: types.HistoryUnitWatts, SplitBy: "source",
	},
	// Two charts, because an accelerator has two different numbers that look
	// like one. The label of each says which: "powered up" is runtime-PM
	// residency and "utilisation" is engine time from fdinfo. A single chart
	// titled "NPU" would leave the reader to guess, and on an idle-but-awake
	// NPU the two disagree by ~100 points.
	{
		Key: "npu_residency", Name: "sysmon_npu_residency_percent",
		Label: "NPU powered up", Unit: types.HistoryUnitPercent, SplitBy: "npu",
	},
	{
		Key: "npu_util", Name: "sysmon_npu_utilization_percent",
		Label: "NPU utilisation", Unit: types.HistoryUnitPercent, SplitBy: "npu",
	},
	{
		Key: "npu_temperature", Name: "sysmon_npu_temperature_celsius",
		Label: "NPU temperature", Unit: types.HistoryUnitCelsius, SplitBy: "npu",
	},
	{
		Key: "npu_clients", Name: "sysmon_npu_clients",
		Label: "NPU clients", Unit: types.HistoryUnitCount, SplitBy: "npu",
	},
	{
		Key: "alerts_critical", Name: "sysmon_alerts_firing", Matchers: []string{`severity="critical"`},
		Label: "Critical alerts", Unit: types.HistoryUnitCount,
	},
	{
		Key: "alerts_warning", Name: "sysmon_alerts_firing", Matchers: []string{`severity="warning"`},
		Label: "Warning alerts", Unit: types.HistoryUnitCount,
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

// Selector is the metric's plain series selector on one node, before any
// rate, scaling or aggregation: what a caller measures the sample cadence of.
//
// instance is the only caller-supplied part. It is validated against
// nodename's rule AND escaped. The rule alone already leaves nothing to escape;
// the escaping is underneath it rather than instead of it, so the matcher stays
// a single literal even if the rule is ever widened (ruling on #37, 2026-09-02).
func (m Metric) Selector(instance string) (string, error) {
	matcher, err := instanceMatcher(instance)
	if err != nil {
		return "", err
	}
	matchers := make([]string, 0, len(m.Matchers)+1)
	matchers = append(matchers, matcher)
	matchers = append(matchers, m.Matchers...)
	return m.Name + "{" + strings.Join(matchers, ",") + "}", nil
}

// instanceMatcher is the validated, escaped instance="..." matcher.
func instanceMatcher(instance string) (string, error) {
	if err := nodename.Validate(instance); err != nil {
		return "", &ErrInvalidInstance{Instance: instance}
	}
	return `instance="` + labelValueEscaper.Replace(instance) + `"`, nil
}

// Expr builds the PromQL for one metric on one node, for a range query at the
// given step. gap is the longest interval between two samples of the series
// over the range -- Querier.SampleGap measures it -- and with the step it
// sizes a counter's rate window; see RateWindow.
//
// A gauge ignores step and gap: its query is the selector, exactly as it was
// before counters existed.
func (m Metric) Expr(instance string, step, gap time.Duration) (string, error) {
	expr, err := m.Selector(instance)
	if err != nil {
		return "", err
	}
	if m.Counter {
		window, err := RateWindow(step, gap)
		if err != nil {
			return "", err
		}
		expr = "rate(" + expr + "[" + promDuration(window) + "])"
	}
	if m.ScaleBy != "" {
		// Matched on every label, not on instance alone. The gauge is written
		// by the same writer, in the same batch, with exactly the counter's
		// labels, so the default one-to-one match pairs each counter series
		// with its own node's page size. on(instance) would pair it with
		// every page-size series of the node: when external_labels are
		// reloaded the old and new series coexist for a lookback, the match
		// turns many-to-many, and Prometheus refuses the whole query.
		matcher, err := instanceMatcher(instance)
		if err != nil {
			return "", err
		}
		expr += " * " + m.ScaleBy + "{" + matcher + "}"
	}
	if m.Counter {
		// Only while the counter itself is there. A range selector does not
		// apply Prometheus' lookback, so once a window is longer than it --
		// a week's range already is -- rate() keeps answering from old
		// samples after the counter stops being written, and the chart draws
		// the last rate as though it were still happening. An unread
		// counter is omitted on purpose (#135): an absent series must stay a
		// gap. The instant selector here does apply the lookback.
		selector, err := m.Selector(instance)
		if err != nil {
			return "", err
		}
		expr += " and " + selector
	}
	if m.Aggregate != "" {
		return m.Aggregate + "(" + expr + ")", nil
	}
	return expr, nil
}

// Lookback is how far before a range's start the query reads: the rate
// window for a counter, nothing for a gauge. A caller bounding how much of the
// database one request may scan has to count it, because a counter's window
// grows with the step and the step is the caller's choice.
func (m Metric) Lookback(step, gap time.Duration) (time.Duration, error) {
	if !m.Counter {
		return 0, nil
	}
	return RateWindow(step, gap)
}

// rateFloorGaps is how many sample gaps a rate window spans at the least.
// See RateWindow.
const rateFloorGaps = 2

// RateWindow is the range a counter's rate() is taken over, for a query at
// step against a series whose samples are never more than gap apart:
//
//	window = max(step + gap, 2 * gap)
//
// step + gap, because rate() at each point of a range query only sees the
// samples inside its own window. A window exactly one step wide ends where the
// next begins, and the increase between the last sample of one window and the
// first of the next falls in neither -- a burst of paging between two chart
// points would simply not be drawn. One extra gap makes consecutive windows
// overlap by a sample, so every increase is counted somewhere.
//
// 2 * gap as the floor, because rate() needs two samples in a window to say
// anything, and two gaps is exactly what guarantees them. Prometheus windows
// are left-open, (t-w, t]. With no two samples more than gap apart, the last
// sample at or before t is later than t-gap, and the one before it is later
// than t-2*gap: both are inside a window of 2*gap. A lost write or a slow
// collection is not a reason to widen this, because gap is MEASURED from the
// samples actually stored (Querier.SampleGap), so it already includes them.
func RateWindow(step, gap time.Duration) (time.Duration, error) {
	if step <= 0 {
		return 0, &ErrInvalidRange{Message: "step must be greater than zero"}
	}
	if gap <= 0 {
		return 0, &ErrInvalidRange{Message: "the sample gap must be greater than zero"}
	}
	if gap > math.MaxInt64/rateFloorGaps || step > math.MaxInt64-gap {
		return 0, &ErrInvalidRange{Message: "the step is too large to take a rate over"}
	}
	return max(step+gap, rateFloorGaps*gap), nil
}

// promDuration renders a duration as a PromQL range: whole seconds when it is
// one, milliseconds otherwise, rounded up so a window is never shorter than
// the one computed.
func promDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}
	return strconv.FormatInt(int64(ms), 10) + "ms"
}
