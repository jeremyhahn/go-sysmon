// Package metrics describes a snapshot as Prometheus metrics.
//
// It is the single description of what go-sysmon exports, consumed two ways:
// pkg/server renders it as the text exposition format for GET /metrics, and
// pkg/prom's exporter turns it into samples pushed over remote write. Both go
// through Write, so a scraped node and a pushed node cannot report different
// numbers -- which is the same argument the alert metrics below already make
// about the dashboard and Alertmanager.
package metrics

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Sink receives one gauge. It is the seam between "what the metrics are", which
// is this file, and "where they go", which is an HTTP response body or a
// remote-write batch.
//
// Labels are alternating name/value pairs rather than a map, because the order
// of a gauge's labels is part of the exposition text and a map has none.
type Sink interface {
	Gauge(name, help string, value float64, labels ...string)
}

// SampleSink adapts a prom.Builder, stamping every gauge with one instant so a
// whole snapshot arrives in the time series as a single moment rather than
// smeared across however long the collection took.
type SampleSink struct {
	Builder *prom.Builder
	TimeMs  int64
}

// Gauge implements Sink. The help text is dropped: remote write carries samples
// and labels, and Prometheus takes its HELP from the scrape endpoint.
func (s SampleSink) Gauge(name, _ string, value float64, labels ...string) {
	s.Builder.Add(name, value, s.TimeMs, labels...)
}

// Prometheus text exposition, written by hand.
//
// The client library would pull in a large dependency tree to re-express data
// this package already holds in a finished form: every value is a plain gauge
// taken from the latest snapshot, with no registry, no collectors and no
// concurrency of its own to manage. Writing the four-line text format directly
// keeps the dependency surface small, which matters for something deployed onto
// small hosts.
//
// The host is deliberately not a label. Prometheus attaches `instance` at scrape
// time, and duplicating it here would produce two competing identities for the
// same machine.

// TextWriter accumulates the exposition body. Its zero value is ready to use.
type TextWriter struct {
	b       strings.Builder
	emitted map[string]bool
}

// String returns the accumulated exposition body.
func (m *TextWriter) String() string { return m.b.String() }

// Gauge writes one gauge with optional labels, preceded by HELP and TYPE the
// first time that metric name is seen.
func (m *TextWriter) Gauge(name, help string, value float64, labels ...string) {
	m.header(name, help)
	m.b.WriteString(name)
	if len(labels) > 0 {
		m.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				m.b.WriteByte(',')
			}
			fmt.Fprintf(&m.b, "%s=%q", labels[i], escapeLabel(labels[i+1]))
		}
		m.b.WriteByte('}')
	}
	fmt.Fprintf(&m.b, " %g\n", value)
}

// header emits the HELP/TYPE preamble the first time a metric name is used.
func (m *TextWriter) header(name, help string) {
	if m.emitted == nil {
		m.emitted = make(map[string]bool)
	}
	if m.emitted[name] {
		return
	}
	m.emitted[name] = true
	fmt.Fprintf(&m.b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
}

// escapeLabel escapes the characters the exposition format reserves in label
// values.
func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

// Bool01 renders a boolean as the 1/0 Prometheus expects. Exported because
// every sink that describes a flag needs it and there should be one of it.
func Bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Write describes snap to sink. It is total over a non-nil snapshot: a field
// the collector could not fill is exported as its zero rather than omitted, so
// an alerting rule watching a series never sees it disappear at the moment it
// matters.
func Write(m Sink, snap *types.Snapshot) {
	m.Gauge("sysmon_up", "1 when the exporter produced this scrape.", 1)
	m.Gauge("sysmon_uptime_seconds", "Host uptime in seconds.", float64(snap.Host.Uptime))

	// CPU
	var cpuSum float64
	for _, c := range snap.CPUs {
		cpuSum += c.UsagePercent
	}
	if len(snap.CPUs) > 0 {
		m.Gauge("sysmon_cpu_usage_percent", "Mean CPU usage across all logical processors.",
			cpuSum/float64(len(snap.CPUs)))
	}
	m.Gauge("sysmon_load1", "One-minute load average.", snap.LoadAvg.Load1)
	m.Gauge("sysmon_load5", "Five-minute load average.", snap.LoadAvg.Load5)
	m.Gauge("sysmon_load15", "Fifteen-minute load average.", snap.LoadAvg.Load15)

	// Memory
	m.Gauge("sysmon_memory_total_bytes", "Total physical memory.", float64(snap.Memory.TotalBytes))
	m.Gauge("sysmon_memory_used_bytes", "Used physical memory.", float64(snap.Memory.UsedBytes))
	m.Gauge("sysmon_memory_used_percent", "Used physical memory as a percentage.", snap.Memory.UsedPercent)
	m.Gauge("sysmon_swap_used_percent", "Used swap as a percentage.", snap.Memory.SwapPercent)

	// Pressure stall: "full" is the strong signal and is the one worth alerting
	// on, so both are exported and labelled.
	//
	// The three resources are a fixed slice rather than a map. Ranging a map
	// gave the exposition a different line order on every scrape, and with it a
	// different position for the HELP and TYPE preamble, which is emitted with
	// the first sample of a metric. The output stayed valid and stopped being
	// diffable, and two renders of one snapshot could not be compared -- which
	// is exactly what the scrape-versus-push test does.
	for _, p := range []struct {
		name string
		res  types.PSIResource
	}{
		{"cpu", snap.Sensors.PSI.CPU},
		{"io", snap.Sensors.PSI.IO},
		{"memory", snap.Sensors.PSI.Memory},
	} {
		m.Gauge("sysmon_psi_some_avg10", "Share of the last 10s some task was stalled.", p.res.SomeAvg10, "resource", p.name)
		m.Gauge("sysmon_psi_full_avg10", "Share of the last 10s every task was stalled.", p.res.FullAvg10, "resource", p.name)
	}

	// Power
	for _, p := range snap.Sensors.PackagePower {
		m.Gauge("sysmon_power_watts", "Instantaneous power draw.", p.PowerW, "source", "cpu:"+p.PackageName)
	}
	for _, g := range snap.GPUs {
		m.Gauge("sysmon_power_watts", "Instantaneous power draw.", g.PowerDrawW,
			"source", fmt.Sprintf("gpu:%d", g.Index))
		m.Gauge("sysmon_gpu_utilization_percent", "GPU utilisation.", g.GPUUtilPercent,
			"gpu", fmt.Sprintf("%d", g.Index))
		// Exported because there is an alert threshold on it -- exhausted video
		// memory fails work outright -- and a rule that fires in the dashboard
		// with no series behind it cannot be corroborated or graphed.
		m.Gauge("sysmon_gpu_memory_percent", "Video memory in use.", g.MemoryPercent,
			"gpu", fmt.Sprintf("%d", g.Index))
		m.Gauge("sysmon_gpu_temperature_celsius", "GPU temperature.", g.TemperatureGPU,
			"gpu", fmt.Sprintf("%d", g.Index))
	}

	// Temperatures
	for _, c := range snap.Sensors.CoreTemps {
		m.Gauge("sysmon_cpu_temperature_celsius", "CPU core temperature.", c.TempCelsius,
			"core", fmt.Sprintf("%d", c.CoreID))
	}

	// Disks
	for i := range snap.Disks {
		d := &snap.Disks[i]
		dev := d.Name
		m.Gauge("sysmon_disk_util_percent", "Share of wall-clock time the device was busy.", d.UtilPercent, "device", dev)
		m.Gauge("sysmon_disk_queue_length", "Average queue depth (iostat avgqu-sz).", d.AvgQueueLength, "device", dev)
		m.Gauge("sysmon_disk_read_bytes_rate", "Read throughput in bytes per second.", float64(d.ReadBytesRate), "device", dev)
		m.Gauge("sysmon_disk_write_bytes_rate", "Write throughput in bytes per second.", float64(d.WriteBytesRate), "device", dev)
		if d.TotalBytes > 0 {
			m.Gauge("sysmon_disk_used_percent", "Filesystem usage across the device's mounts.", d.UsedPercent, "device", dev)
			m.Gauge("sysmon_disk_total_bytes", "Total filesystem capacity on the device.", float64(d.TotalBytes), "device", dev)
		}
		if d.Temperature > 0 {
			m.Gauge("sysmon_disk_temperature_celsius", "Drive temperature.", d.Temperature, "device", dev)
		}
		if d.SMARTEnabled {
			m.Gauge("sysmon_disk_smart_healthy", "1 when SMART reports no failing attribute.", Bool01(d.SMARTHealthy), "device", dev)
			m.Gauge("sysmon_disk_power_on_hours", "Drive power-on hours.", float64(d.PowerOnHours), "device", dev)
			// Exported for every NVMe namespace, including zero. Suppressing a
			// zero would drop the series precisely when spare capacity is
			// exhausted, so an alerting rule watching it would see an absent
			// metric at the moment it most needs a value.
			if strings.HasPrefix(dev, "nvme") {
				m.Gauge("sysmon_disk_wear_percent", "NVMe wear level used.", float64(d.WearLevelPercent), "device", dev)
				m.Gauge("sysmon_disk_spare_percent", "NVMe spare blocks remaining.", float64(d.AvailableSparePercent), "device", dev)
			}
		}
		if d.RAID != nil {
			m.Gauge("sysmon_raid_degraded", "1 when the md array is degraded.", Bool01(d.RAID.Degraded), "array", dev, "level", d.RAID.Level)
			m.Gauge("sysmon_raid_active_devices", "Members currently in sync.", float64(d.RAID.ActiveDevices), "array", dev)
			m.Gauge("sysmon_raid_total_devices", "Members the array is configured for.", float64(d.RAID.TotalDevices), "array", dev)
			m.Gauge("sysmon_raid_sync_percent", "Resync/recovery progress.", d.RAID.SyncProgressPercent, "array", dev)
			m.Gauge("sysmon_raid_mismatch_count", "Sectors that differed on the last check.", float64(d.RAID.MismatchCount), "array", dev)
		}
	}

	// Network
	for i := range snap.Networks {
		n := &snap.Networks[i]
		if n.IsLoopback {
			continue
		}
		m.Gauge("sysmon_network_up", "1 when the interface is up.", Bool01(n.IsUp), "interface", n.Name)
		m.Gauge("sysmon_network_receive_bytes_rate", "Receive throughput in bytes per second.", n.BytesRecvRate, "interface", n.Name)
		m.Gauge("sysmon_network_transmit_bytes_rate", "Transmit throughput in bytes per second.", n.BytesSentRate, "interface", n.Name)
		m.Gauge("sysmon_network_errors_total_current", "Cumulative interface errors (in+out).",
			float64(n.ErrorsIn+n.ErrorsOut), "interface", n.Name)
		m.Gauge("sysmon_network_drops_total_current", "Cumulative interface drops (in+out).",
			float64(n.DropsIn+n.DropsOut), "interface", n.Name)
	}

	// Alerts: the same evaluation the dashboard shows.
	alerts := make([]types.Alert, len(snap.Alerts))
	copy(alerts, snap.Alerts)
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].ID < alerts[j].ID })
	for _, a := range alerts {
		m.Gauge("sysmon_alert", "1 for each firing alert rule.", 1,
			"rule", a.Rule, "severity", a.Severity, "resource", a.Resource, "category", a.Category)
	}
	m.Gauge("sysmon_alerts_firing", "Number of alerts currently firing, by severity.",
		countSeverity(snap.Alerts, types.SeverityCritical), "severity", types.SeverityCritical)
	m.Gauge("sysmon_alerts_firing", "Number of alerts currently firing, by severity.",
		countSeverity(snap.Alerts, types.SeverityWarning), "severity", types.SeverityWarning)
}

// countSeverity counts alerts at one severity.
func countSeverity(alerts []types.Alert, severity string) float64 {
	var n float64
	for _, a := range alerts {
		if a.Severity == severity {
			n++
		}
	}
	return n
}
