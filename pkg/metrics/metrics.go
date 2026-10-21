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

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Sink receives one sample. It is the seam between "what the metrics are",
// which is this file, and "where they go", which is an HTTP response body or a
// remote-write batch.
//
// Gauge and Counter are separate methods because the type is part of what a
// sample means, not a formatting detail. rate() and increase() treat a drop in
// a counter as a reset and a drop in a gauge as a decrease, so a monotonic
// total published as a gauge is read wrongly after every reboot -- silently,
// and on exactly the host an operator is asking about.
//
// Labels are alternating name/value pairs rather than a map, because the order
// of a sample's labels is part of the exposition text and a map has none.
type Sink interface {
	// Gauge records a value that can go up or down.
	Gauge(name, help string, value float64, labels ...string)
	// Counter records a cumulative total that only resets, never decreases.
	// Its name carries the _total suffix, per Prometheus convention.
	Counter(name, help string, value float64, labels ...string)
}

// Prometheus text exposition, written by hand.
//
// The client library would pull in a large dependency tree to re-express data
// this package already holds in a finished form: every value is a plain gauge
// or counter taken from the latest snapshot, with no registry, no collectors
// and no concurrency of its own to manage. Writing the four-line text format directly
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

// Metric types, as the exposition's TYPE line spells them.
const (
	typeGauge   = "gauge"
	typeCounter = "counter"
)

// Gauge writes one gauge with optional labels, preceded by HELP and TYPE the
// first time that metric name is seen.
func (m *TextWriter) Gauge(name, help string, value float64, labels ...string) {
	m.sample(typeGauge, name, help, value, labels)
}

// Counter writes one counter with optional labels, preceded by HELP and TYPE
// the first time that metric name is seen.
func (m *TextWriter) Counter(name, help string, value float64, labels ...string) {
	m.sample(typeCounter, name, help, value, labels)
}

// sample writes one line of either type.
func (m *TextWriter) sample(kind, name, help string, value float64, labels []string) {
	m.header(kind, name, help)
	m.b.WriteString(name)
	if len(labels) > 0 {
		m.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				m.b.WriteByte(',')
			}
			m.b.WriteString(labels[i])
			m.b.WriteString(`="`)
			m.b.WriteString(escapeLabel(labels[i+1]))
			m.b.WriteByte('"')
		}
		m.b.WriteByte('}')
	}
	fmt.Fprintf(&m.b, " %g\n", value)
}

// header emits the HELP/TYPE preamble the first time a metric name is used.
func (m *TextWriter) header(kind, name, help string) {
	if m.emitted == nil {
		m.emitted = make(map[string]bool)
	}
	if m.emitted[name] {
		return
	}
	m.emitted[name] = true
	fmt.Fprintf(&m.b, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, kind)
}

// The exposition format defines exactly these escapes. Label values escape
// backslash, double quote and line feed; HELP text escapes only backslash and
// line feed, because a quote carries no meaning in a comment line. Every other
// byte -- non-ASCII UTF-8, tabs, other control characters -- is written
// verbatim, since the format has no other escape sequences and a parser would
// either reject one or store it literally.
var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

// escapeLabel escapes a label value for the text exposition format. The
// result is written between double quotes as-is; it must not be quoted again.
func escapeLabel(v string) string { return labelEscaper.Replace(v) }

// escapeHelp escapes HELP text for the text exposition format.
func escapeHelp(v string) string { return helpEscaper.Replace(v) }

// Bool01 renders a boolean as the 1/0 Prometheus expects. Exported because
// every sink that describes a flag needs it and there should be one of it.
func Bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Write describes snap to sink. It is total over a non-nil snapshot: a gauge
// the collector could not fill is exported as its zero rather than omitted, so
// an alerting rule watching a series never sees it disappear at the moment it
// matters.
//
// Counters are the exception, and deliberately. A counter stuck at zero reads
// as a quiet host rather than an unmeasured one, and a zero after a real value
// reads as a reset, so an unread counter is omitted rather than published.
func Write(m Sink, snap *types.Snapshot) {
	m.Gauge("sysmon_up", "1 when the exporter produced this scrape.", 1)
	m.Gauge("sysmon_uptime_seconds", "Host uptime in seconds.", float64(snap.Host.Uptime))

	// CPU
	// Omitted, not zero, when no core's usage was measured: a gauge at zero
	// would chart the collector's first interval as an idle host.
	if mean, measured := types.MeanCPUUsage(snap.CPUs); measured > 0 {
		m.Gauge("sysmon_cpu_usage_percent", "Mean CPU usage across the logical processors whose usage was measured.", mean)
	}
	m.Gauge("sysmon_load1", "One-minute load average.", snap.LoadAvg.Load1)
	m.Gauge("sysmon_load5", "Five-minute load average.", snap.LoadAvg.Load5)
	m.Gauge("sysmon_load15", "Fifteen-minute load average.", snap.LoadAvg.Load15)

	// Memory
	m.Gauge("sysmon_memory_total_bytes", "Total physical memory.", float64(snap.Memory.TotalBytes))
	m.Gauge("sysmon_memory_used_bytes", "Used physical memory.", float64(snap.Memory.UsedBytes))
	m.Gauge("sysmon_memory_used_percent", "Used physical memory as a percentage.", snap.Memory.UsedPercent)
	m.Gauge("sysmon_swap_used_percent", "Used swap as a percentage.", snap.Memory.SwapPercent)
	// The kernel's page size, which turns the paging counters below into
	// bytes: it is 4 KiB on x86 but 16 or 64 KiB on some arm64 kernels, so no
	// constant in a query could. Omitted when unknown -- a reading from a peer
	// too old to report it -- because a zero would scale every rate to zero.
	if snap.Memory.PageSizeBytes > 0 {
		m.Gauge("sysmon_memory_page_size_bytes", "Memory page size in bytes.",
			float64(snap.Memory.PageSizeBytes))
	}
	// Paging, not occupancy: rate() over these is how much swapping is
	// happening now, which resident swap cannot say. Gated on availability
	// because an unreadable /proc/vmstat reports zero, and a zero counter is
	// indistinguishable from a host that has never paged (#135).
	if snap.Memory.SwapCountersAvailable {
		m.Counter("sysmon_swap_in_pages_total",
			"Pages swapped in since boot (/proc/vmstat pswpin). Multiply by the page size for bytes.",
			float64(snap.Memory.SwapInPages))
		m.Counter("sysmon_swap_out_pages_total",
			"Pages swapped out since boot (/proc/vmstat pswpout). Multiply by the page size for bytes.",
			float64(snap.Memory.SwapOutPages))
	}

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

	// Compute accelerators (NPUs).
	//
	// A snapshot carries two percentages for an accelerator and they do not
	// measure the same thing, so the metric names say which is which. Residency
	// is how long the device was POWERED UP; utilisation is how much work it
	// did. Exporting either under a name that could be read as the other is the
	// mistake this block is arranged to prevent: a residency figure alerted on
	// as utilisation fires on an idle-but-awake NPU, and a utilisation figure
	// read as residency reads as a powered-down device whenever the driver
	// publishes no fdinfo.
	for _, n := range snap.NPUs {
		npu := fmt.Sprintf("%d", n.Index)
		m.Gauge("sysmon_power_watts", "Instantaneous power draw.", n.PowerDrawW,
			"source", "npu:"+npu)
		m.Gauge("sysmon_npu_residency_percent",
			"Share of the sampling interval the accelerator was powered up, from runtime-PM counters. Power-on residency, not compute utilisation.",
			n.ResidencyPercent, "npu", npu)
		m.Gauge("sysmon_npu_utilization_percent",
			"Compute utilisation: the busiest engine's share of the interval, from per-client DRM usage-stats fdinfo. Legitimately 0 when the driver implements no fdinfo, or when this process may not read the owning task's fdinfo.",
			n.UtilPercent, "npu", npu)
		m.Gauge("sysmon_npu_clients", "Processes holding the accelerator open.",
			float64(n.ClientCount), "npu", npu)
		m.Gauge("sysmon_npu_temperature_celsius",
			"Accelerator temperature, when the driver registers an hwmon node.",
			n.TemperatureC, "npu", npu)
		m.Gauge("sysmon_npu_frequency_mhz", "Current accelerator clock.",
			float64(n.FrequencyMHz), "npu", npu)
		// Why staleness is a series of its own: the collector deliberately does
		// not re-read a driver's own attributes while the device is suspended,
		// because the read resumes the hardware and costs idle power. The
		// values sourced from them -- the clock above among them -- are then
		// carried forward, and a flat line with nothing saying why is the
		// reading most likely to be mistaken for a broken collector.
		m.Gauge("sysmon_npu_counters_stale",
			"1 when the driver's own counters were not re-read, because reading them would have resumed a suspended accelerator.",
			Bool01(n.CountersStale), "npu", npu)
		m.Gauge("sysmon_npu_counters_age_seconds",
			"Age of the carried-forward driver counters; 0 when they were read this cycle.",
			n.CountersAgeSec, "npu", npu)
		// NPUInfo.BusyTimeUs is deliberately NOT exported here, not even as
		// a counter. It is a raw cumulative driver counter, but it follows D0
		// residency rather than job execution: rate() over it is a utilisation
		// figure pinned near 100% whenever anything keeps the NPU awake,
		// including this monitor's own polling (measured at 98.8% of resumed
		// time with no client on the device; see types.NPUInfo). A correctly
		// typed counter would still invite that reading.
		// sysmon_npu_residency_percent already reports what it measures, and
		// says so in its name. The snapshot and the API still carry the raw
		// counter for anybody debugging a specific driver.
	}

	// Temperatures
	// A core is identified by its package and its number within it. Core
	// numbers restart on every package, so "core" alone gives both sockets'
	// Core 0 one label set on a two-socket host: two samples of one series at
	// one timestamp, which a receiver refuses along with the whole batch.
	for _, c := range snap.Sensors.CoreTemps {
		m.Gauge("sysmon_cpu_temperature_celsius", "CPU core temperature.", c.TempCelsius,
			"package", fmt.Sprintf("%d", c.PackageID), "core", fmt.Sprintf("%d", c.CoreID))
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
			// The drive's own health and endurance travel with the drive, so
			// they also carry its serial. A device name is assigned at boot and
			// NVMe names are not stable across one -- ci-node-1's 2026-08-31
			// rebuild swapped two of four -- so a query keyed on "device" alone
			// can join one drive's history to another's. "device" stays for the
			// queries already written against it. A drive that reports no
			// serial gets no serial label rather than an empty one: serial=""
			// is the same label set as no serial at all, so it says nothing.
			ident := []string{"device", dev}
			if d.Serial != "" {
				ident = append(ident, "serial", d.Serial)
			}
			m.Gauge("sysmon_disk_smart_healthy", "1 when SMART reports no failing attribute.", Bool01(d.SMARTHealthy), ident...)
			m.Gauge("sysmon_disk_power_on_hours", "Drive power-on hours.", float64(d.PowerOnHours), ident...)
			// Exported for every NVMe namespace, including zero. Suppressing a
			// zero would drop the series precisely when spare capacity is
			// exhausted, so an alerting rule watching it would see an absent
			// metric at the moment it most needs a value.
			if strings.HasPrefix(dev, "nvme") {
				m.Gauge("sysmon_disk_wear_percent", "NVMe wear level used.", float64(d.WearLevelPercent), ident...)
				m.Gauge("sysmon_disk_spare_percent", "NVMe spare blocks remaining.", float64(d.AvailableSparePercent), ident...)
				writeDataUnits(m, d, ident)
			}
		}
		// Drives behind a hardware RAID controller. They are not block devices
		// and have no device name, so they are labelled by SERIAL -- the only
		// identifier that survives a reboot. ci-node-1's 2026-08-31 rebuild
		// renamed every PERC and BOSS device and swapped two of four NVMe
		// names; a series keyed on anything else silently becomes a DIFFERENT
		// series across such a reboot, which is worse than a missing one
		// because the history looks continuous and is not.
		//
		// The volume the drives sit behind is carried as a second label, so a
		// query can group an array's members without joining on anything.
		//
		// THE ALERT PATH KEYS THE SAME DRIVE BY POSITION, NOT BY SERIAL, and
		// that is deliberate rather than an inconsistency to tidy away. Two
		// identifiers for one thing is normally the defect this repository
		// keeps removing; here it is correct, because the two answer different
		// questions and fail in opposite directions:
		//
		//   metric  long-lived history. A re-bay must not silently continue
		//           one drive's series under another drive's readings, so the
		//           identity has to travel WITH the drive. A serial that
		//           cannot be read produces a GAP, which is honest.
		//   alert   de-duplication within a run, where flapping is the
		//           failure. The serial is read from the drive every cycle and
		//           goes missing on exactly the drive an alert is about -- one
		//           on its way out -- so keying on it would clear and re-fire
		//           an hour-old alert as a new fault on a different drive. The
		//           position comes from the controller's configuration and
		//           cannot vanish while the member exists.
		//
		// See pkg/alert.memberResource, which carries the reciprocal note. If
		// you are here to unify the two, this is the reason not to.
		for j := range d.Members {
			mem := &d.Members[j]
			if mem.Serial == "" {
				// Without a stable identity there is no series worth writing:
				// two unlabelled drives would collide into one.
				continue
			}
			if mem.Temperature > 0 {
				m.Gauge("sysmon_disk_member_temperature_celsius", "Temperature of a drive behind a RAID controller.",
					mem.Temperature, "serial", mem.Serial, "volume", dev)
			}
			if mem.SMARTEnabled {
				m.Gauge("sysmon_disk_member_smart_healthy", "1 when a drive behind a RAID controller reports itself well.",
					Bool01(mem.SMARTHealthy), "serial", mem.Serial, "volume", dev)
			}
		}
		if d.RAID != nil {
			m.Gauge("sysmon_raid_degraded", "1 when the md array is degraded.", Bool01(d.RAID.Degraded), "array", dev, "level", d.RAID.Level)
			m.Gauge("sysmon_raid_active_devices", "Members currently in sync.", float64(d.RAID.ActiveDevices), "array", dev)
			m.Gauge("sysmon_raid_total_devices", "Members the array is configured for.", float64(d.RAID.TotalDevices), "array", dev)
			// Both of these are md-only readings, and both have a zero value
			// that reads as a clean result: 0 percent as a stalled rebuild,
			// and 0 mismatches as a consistency check that found nothing. A
			// hardware RAID volume reports neither -- its controller is never
			// asked -- so it exports neither. An omitted series is a gap
			// somebody can see; a fabricated one is not.
			//
			// An empty SyncAction is the flag for that, documented on
			// RAIDInfo. md's applyRAID sets it on every array it accepts, so
			// md series are unaffected.
			if d.RAID.SyncAction != "" {
				m.Gauge("sysmon_raid_sync_percent", "Resync/recovery progress.", d.RAID.SyncProgressPercent, "array", dev)
				m.Gauge("sysmon_raid_mismatch_count", "Sectors that differed on the last check.", float64(d.RAID.MismatchCount), "array", dev)
			}
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
			"rule", a.Rule, "severity", string(a.Severity), "resource", a.Resource, "category", a.Category)
	}
	m.Gauge("sysmon_alerts_firing", "Number of alerts currently firing, by severity.",
		countSeverity(snap.Alerts, types.SeverityCritical), "severity", string(types.SeverityCritical))
	m.Gauge("sysmon_alerts_firing", "Number of alerts currently firing, by severity.",
		countSeverity(snap.Alerts, types.SeverityWarning), "severity", string(types.SeverityWarning))
}

// countSeverity counts alerts at one severity.
// nvmeDataUnitBytes is the size of one NVMe SMART "data unit": the
// specification counts in thousands of 512-byte units, so one unit is
// 1000 x 512 bytes, not a power of two.
const nvmeDataUnitBytes = 1000 * 512

// writeDataUnits exports an NVMe drive's lifetime read and write volume, in
// the drive's own data units and in bytes. Both are counters: the drive's
// figures only grow, and increase() over them is the volume moved in a window
// -- the weekly write volume that, beside the wear percentage, says how fast a
// drive is being consumed. A zero is an unread log page, not an unused drive
// (every drive leaves the factory with writes on it), so it is omitted.
func writeDataUnits(m Sink, d *types.DiskInfo, ident []string) {
	if d.DataUnitsWritten > 0 {
		m.Counter("sysmon_disk_data_units_written_total",
			"NVMe data units written over the drive's life; one unit is 512000 bytes.",
			float64(d.DataUnitsWritten), ident...)
		m.Counter("sysmon_disk_written_bytes_total", "Bytes written over the drive's life, from NVMe data units.",
			float64(d.DataUnitsWritten)*nvmeDataUnitBytes, ident...)
	}
	if d.DataUnitsRead > 0 {
		m.Counter("sysmon_disk_data_units_read_total",
			"NVMe data units read over the drive's life; one unit is 512000 bytes.",
			float64(d.DataUnitsRead), ident...)
		m.Counter("sysmon_disk_read_bytes_total", "Bytes read over the drive's life, from NVMe data units.",
			float64(d.DataUnitsRead)*nvmeDataUnitBytes, ident...)
	}
}

func countSeverity(alerts []types.Alert, severity types.Severity) float64 {
	var n float64
	for _, a := range alerts {
		if a.Severity == severity {
			n++
		}
	}
	return n
}
