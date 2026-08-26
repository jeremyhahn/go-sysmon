package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

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

// metricsContentType is the Prometheus text exposition format version.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// metricWriter accumulates the exposition body.
type metricWriter struct {
	b       strings.Builder
	emitted map[string]bool
}

// gauge writes one gauge with optional labels, preceded by HELP and TYPE the
// first time that metric name is seen.
func (m *metricWriter) gauge(name, help string, value float64, labels ...string) {
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
func (m *metricWriter) header(name, help string) {
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

// bool01 renders a boolean as the 1/0 Prometheus expects.
func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// handleMetrics serves the latest snapshot in Prometheus text format. The same
// alert evaluation that drives the dashboard is exported as sysmon_alert, so a
// rule firing in the UI and a rule firing in Alertmanager cannot diverge.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snap, err := s.mon.Snapshot()
	if err != nil || snap == nil {
		http.Error(w, "no snapshot available", http.StatusServiceUnavailable)
		return
	}

	m := &metricWriter{}
	writeSnapshotMetrics(m, snap)

	w.Header().Set("Content-Type", metricsContentType)
	if _, err := w.Write([]byte(m.b.String())); err != nil {
		slog.Debug("server: writing metrics", "error", err)
	}
}

// writeSnapshotMetrics renders a snapshot. Split out so it can be tested
// without an HTTP server.
func writeSnapshotMetrics(m *metricWriter, snap *types.Snapshot) {
	m.gauge("sysmon_up", "1 when the exporter produced this scrape.", 1)
	m.gauge("sysmon_uptime_seconds", "Host uptime in seconds.", float64(snap.Host.Uptime))

	// CPU
	var cpuSum float64
	for _, c := range snap.CPUs {
		cpuSum += c.UsagePercent
	}
	if len(snap.CPUs) > 0 {
		m.gauge("sysmon_cpu_usage_percent", "Mean CPU usage across all logical processors.",
			cpuSum/float64(len(snap.CPUs)))
	}
	m.gauge("sysmon_load1", "One-minute load average.", snap.LoadAvg.Load1)
	m.gauge("sysmon_load5", "Five-minute load average.", snap.LoadAvg.Load5)
	m.gauge("sysmon_load15", "Fifteen-minute load average.", snap.LoadAvg.Load15)

	// Memory
	m.gauge("sysmon_memory_total_bytes", "Total physical memory.", float64(snap.Memory.TotalBytes))
	m.gauge("sysmon_memory_used_bytes", "Used physical memory.", float64(snap.Memory.UsedBytes))
	m.gauge("sysmon_memory_used_percent", "Used physical memory as a percentage.", snap.Memory.UsedPercent)
	m.gauge("sysmon_swap_used_percent", "Used swap as a percentage.", snap.Memory.SwapPercent)

	// Pressure stall: "full" is the strong signal and is the one worth alerting
	// on, so both are exported and labelled.
	for res, p := range map[string]types.PSIResource{
		"cpu": snap.Sensors.PSI.CPU, "memory": snap.Sensors.PSI.Memory, "io": snap.Sensors.PSI.IO,
	} {
		m.gauge("sysmon_psi_some_avg10", "Share of the last 10s some task was stalled.", p.SomeAvg10, "resource", res)
		m.gauge("sysmon_psi_full_avg10", "Share of the last 10s every task was stalled.", p.FullAvg10, "resource", res)
	}

	// Power
	for _, p := range snap.Sensors.PackagePower {
		m.gauge("sysmon_power_watts", "Instantaneous power draw.", p.PowerW, "source", "cpu:"+p.PackageName)
	}
	for _, g := range snap.GPUs {
		m.gauge("sysmon_power_watts", "Instantaneous power draw.", g.PowerDrawW,
			"source", fmt.Sprintf("gpu:%d", g.Index))
		m.gauge("sysmon_gpu_utilization_percent", "GPU utilisation.", g.GPUUtilPercent,
			"gpu", fmt.Sprintf("%d", g.Index))
		m.gauge("sysmon_gpu_temperature_celsius", "GPU temperature.", g.TemperatureGPU,
			"gpu", fmt.Sprintf("%d", g.Index))
	}

	// Temperatures
	for _, c := range snap.Sensors.CoreTemps {
		m.gauge("sysmon_cpu_temperature_celsius", "CPU core temperature.", c.TempCelsius,
			"core", fmt.Sprintf("%d", c.CoreID))
	}

	// Disks
	for i := range snap.Disks {
		d := &snap.Disks[i]
		dev := d.Name
		m.gauge("sysmon_disk_util_percent", "Share of wall-clock time the device was busy.", d.UtilPercent, "device", dev)
		m.gauge("sysmon_disk_queue_length", "Average queue depth (iostat avgqu-sz).", d.AvgQueueLength, "device", dev)
		m.gauge("sysmon_disk_read_bytes_rate", "Read throughput in bytes per second.", float64(d.ReadBytesRate), "device", dev)
		m.gauge("sysmon_disk_write_bytes_rate", "Write throughput in bytes per second.", float64(d.WriteBytesRate), "device", dev)
		if d.TotalBytes > 0 {
			m.gauge("sysmon_disk_used_percent", "Filesystem usage across the device's mounts.", d.UsedPercent, "device", dev)
			m.gauge("sysmon_disk_total_bytes", "Total filesystem capacity on the device.", float64(d.TotalBytes), "device", dev)
		}
		if d.Temperature > 0 {
			m.gauge("sysmon_disk_temperature_celsius", "Drive temperature.", d.Temperature, "device", dev)
		}
		if d.SMARTEnabled {
			m.gauge("sysmon_disk_smart_healthy", "1 when SMART reports no failing attribute.", bool01(d.SMARTHealthy), "device", dev)
			m.gauge("sysmon_disk_power_on_hours", "Drive power-on hours.", float64(d.PowerOnHours), "device", dev)
			// Exported for every NVMe namespace, including zero. Suppressing a
			// zero would drop the series precisely when spare capacity is
			// exhausted, so an alerting rule watching it would see an absent
			// metric at the moment it most needs a value.
			if strings.HasPrefix(dev, "nvme") {
				m.gauge("sysmon_disk_wear_percent", "NVMe wear level used.", float64(d.WearLevelPercent), "device", dev)
				m.gauge("sysmon_disk_spare_percent", "NVMe spare blocks remaining.", float64(d.AvailableSparePercent), "device", dev)
			}
		}
		if d.RAID != nil {
			m.gauge("sysmon_raid_degraded", "1 when the md array is degraded.", bool01(d.RAID.Degraded), "array", dev, "level", d.RAID.Level)
			m.gauge("sysmon_raid_active_devices", "Members currently in sync.", float64(d.RAID.ActiveDevices), "array", dev)
			m.gauge("sysmon_raid_total_devices", "Members the array is configured for.", float64(d.RAID.TotalDevices), "array", dev)
			m.gauge("sysmon_raid_sync_percent", "Resync/recovery progress.", d.RAID.SyncProgressPercent, "array", dev)
			m.gauge("sysmon_raid_mismatch_count", "Sectors that differed on the last check.", float64(d.RAID.MismatchCount), "array", dev)
		}
	}

	// Network
	for i := range snap.Networks {
		n := &snap.Networks[i]
		if n.IsLoopback {
			continue
		}
		m.gauge("sysmon_network_up", "1 when the interface is up.", bool01(n.IsUp), "interface", n.Name)
		m.gauge("sysmon_network_receive_bytes_rate", "Receive throughput in bytes per second.", n.BytesRecvRate, "interface", n.Name)
		m.gauge("sysmon_network_transmit_bytes_rate", "Transmit throughput in bytes per second.", n.BytesSentRate, "interface", n.Name)
		m.gauge("sysmon_network_errors_total_current", "Cumulative interface errors (in+out).",
			float64(n.ErrorsIn+n.ErrorsOut), "interface", n.Name)
		m.gauge("sysmon_network_drops_total_current", "Cumulative interface drops (in+out).",
			float64(n.DropsIn+n.DropsOut), "interface", n.Name)
	}

	// Alerts: the same evaluation the dashboard shows.
	alerts := make([]types.Alert, len(snap.Alerts))
	copy(alerts, snap.Alerts)
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].ID < alerts[j].ID })
	for _, a := range alerts {
		m.gauge("sysmon_alert", "1 for each firing alert rule.", 1,
			"rule", a.Rule, "severity", a.Severity, "resource", a.Resource, "category", a.Category)
	}
	m.gauge("sysmon_alerts_firing", "Number of alerts currently firing, by severity.",
		countSeverity(snap.Alerts, types.SeverityCritical), "severity", types.SeverityCritical)
	m.gauge("sysmon_alerts_firing", "Number of alerts currently firing, by severity.",
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
