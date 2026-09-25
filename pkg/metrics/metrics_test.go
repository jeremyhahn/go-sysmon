package metrics

import (
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// render runs the exposition writer over a snapshot.
func render(snap *types.Snapshot) string {
	m := &TextWriter{}
	Write(m, snap)
	return m.String()
}

// TestWrite_CoreSeries checks the shape Prometheus requires:
// a HELP and TYPE line before each metric, emitted once, and labelled samples.
func TestWrite_CoreSeries(t *testing.T) {
	snap := &types.Snapshot{
		Host:       types.HostInfo{Uptime: 120},
		CPUs:       []types.CPUInfo{{UsagePercent: 20, UsageAvailable: true}, {UsagePercent: 40, UsageAvailable: true}},
		CPUSummary: types.CPUSummary{TotalThreads: 2},
		Memory:     types.MemoryInfo{TotalBytes: 100, UsedBytes: 50, UsedPercent: 50},
		Disks: []types.DiskInfo{{
			Name: "sda", TotalBytes: 10, UsedPercent: 80, UtilPercent: 12.5,
			SMARTEnabled: true, SMARTHealthy: true,
		}},
	}
	out := render(snap)

	for _, want := range []string{
		"# HELP sysmon_up",
		"# TYPE sysmon_up gauge",
		"sysmon_up 1",
		"sysmon_cpu_usage_percent 30",
		"sysmon_memory_used_percent 50",
		`sysmon_disk_util_percent{device="sda"} 12.5`,
		`sysmon_disk_smart_healthy{device="sda"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}

	if got := strings.Count(out, "# TYPE sysmon_disk_util_percent gauge"); got != 1 {
		t.Errorf("TYPE for sysmon_disk_util_percent appeared %d times, want exactly 1", got)
	}
}

// TestWrite_CPUGaugeOverMeasuredCoresOnly: the CPU gauge is the mean over the
// measured cores, and is omitted -- not zero -- when none was measured, so the
// collector's first interval never charts as an idle host.
func TestWrite_CPUGaugeOverMeasuredCoresOnly(t *testing.T) {
	partial := render(&types.Snapshot{CPUs: []types.CPUInfo{{UsagePercent: 50, UsageAvailable: true}, {}}})
	if !strings.Contains(partial, "sysmon_cpu_usage_percent 50\n") {
		t.Errorf("an unknown core dragged the mean; exposition:\n%s", partial)
	}

	unknown := render(&types.Snapshot{CPUs: []types.CPUInfo{{}, {}}})
	if strings.Contains(unknown, "sysmon_cpu_usage_percent") {
		t.Errorf("no core was measured, yet the gauge was published:\n%s", unknown)
	}
}

// TestWrite_RAIDAndAlerts covers the two series that make this
// endpoint worth having: array health and the same alerts the dashboard shows.
func TestWrite_RAIDAndAlerts(t *testing.T) {
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{
			Name: "md2",
			RAID: &types.RAIDInfo{Level: "raid1", Degraded: true, ActiveDevices: 1, TotalDevices: 2},
		}},
		Alerts: []types.Alert{
			{Rule: "raid_degraded", Severity: types.SeverityCritical, Resource: "md2", Category: "storage"},
			{Rule: "disk_full", Severity: types.SeverityWarning, Resource: "sda", Category: "storage"},
		},
	}
	out := render(snap)

	for _, want := range []string{
		`sysmon_raid_degraded{array="md2",level="raid1"} 1`,
		`sysmon_raid_active_devices{array="md2"} 1`,
		`sysmon_alert{rule="raid_degraded",severity="critical",resource="md2",category="storage"} 1`,
		`sysmon_alerts_firing{severity="critical"} 1`,
		`sysmon_alerts_firing{severity="warning"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}
}

// TestWrite_HealthyArrayReportsZero verifies a healthy array is
// still exported, as 0. A missing series and a healthy one must not look the
// same to an alerting rule.
func TestWrite_HealthyArrayReportsZero(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md0",
		RAID: &types.RAIDInfo{Level: "raid10", ActiveDevices: 4, TotalDevices: 4},
	}}}
	if !strings.Contains(render(snap), `sysmon_raid_degraded{array="md0",level="raid10"} 0`) {
		t.Error("a healthy array should still export sysmon_raid_degraded as 0")
	}
}

// TestWrite_LoopbackExcluded keeps the series count down and
// avoids a meaningless interface in every dashboard query.
func TestWrite_LoopbackExcluded(t *testing.T) {
	snap := &types.Snapshot{Networks: []types.NetworkInfo{
		{Name: "lo", IsLoopback: true, IsUp: true},
		{Name: "eno1", IsUp: true},
	}}
	out := render(snap)
	if strings.Contains(out, `interface="lo"`) {
		t.Error("loopback should not be exported")
	}
	if !strings.Contains(out, `sysmon_network_up{interface="eno1"} 1`) {
		t.Error("a real interface should be exported")
	}
}

// TestEscapeLabel covers the characters the text format reserves; an
// unescaped quote in a device name would corrupt the whole scrape.
func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		`plain`:      `plain`,
		`with"quote`: `with\"quote`,
		`back\slash`: `back\\slash`,
		"new\nline":  `new\nline`,
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTextWriter_LabelValueExposition pins the exact bytes a label value
// produces. The exposition format escapes only backslash, double quote and
// line feed; every other byte, including non-ASCII UTF-8 and control
// characters, is passed through verbatim. Escaping twice, or using Go's %q,
// changes the string Prometheus stores.
func TestTextWriter_LabelValueExposition(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"plain", "sda", `m{device="sda"} 1`},
		{"empty", "", `m{device=""} 1`},
		{"quote", `q"u`, `m{device="q\"u"} 1`},
		{"backslash", `a\b`, `m{device="a\\b"} 1`},
		{"newline", "a\nb", `m{device="a\nb"} 1`},
		{"latin", "café", `m{device="café"} 1`},
		{"cjk", "温度", `m{device="温度"} 1`},
		{"control", "a\x01b", "m{device=\"a\x01b\"} 1"},
		{"tab", "a\tb", "m{device=\"a\tb\"} 1"},
		{"all reserved", "\\\"\n", `m{device="\\\"\n"} 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w TextWriter
			w.Gauge("m", "h", 1, "device", tc.value)
			want := "# HELP m h\n# TYPE m gauge\n" + tc.want + "\n"
			if got := w.String(); got != want {
				t.Errorf("exposition for %q:\n got %q\nwant %q", tc.value, got, want)
			}
			if got := parseLabelValue(t, w.String()); got != tc.value {
				t.Errorf("round trip: got %q, want %q", got, tc.value)
			}
		})
	}
}

// TestTextWriter_MultipleLabels checks the separator and that each value is
// escaped independently.
func TestTextWriter_MultipleLabels(t *testing.T) {
	var w TextWriter
	w.Gauge("m", "h", 2, "a", `x"`, "b", "ü")
	want := "# HELP m h\n# TYPE m gauge\n" + `m{a="x\"",b="ü"} 2` + "\n"
	if got := w.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestTextWriter_HelpEscaping follows the exposition spec for HELP text:
// backslash and line feed are escaped, and nothing else is -- a quote in HELP
// is literal. An unescaped newline would end the comment line early and turn
// the rest of the help text into a malformed sample line.
func TestTextWriter_HelpEscaping(t *testing.T) {
	cases := []struct {
		name string
		help string
		want string
	}{
		{"plain", "Host uptime.", `Host uptime.`},
		{"newline", "line one\nline two", `line one\nline two`},
		{"backslash", `C:\path`, `C:\\path`},
		{"quote is literal", `say "hi"`, `say "hi"`},
		{"utf8", "Température", `Température`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w TextWriter
			w.Gauge("m", tc.help, 1)
			want := "# HELP m " + tc.want + "\n# TYPE m gauge\nm 1\n"
			if got := w.String(); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// parseLabelValue reads back the single label value of the one sample in an
// exposition body, undoing the three escapes the format defines. It is an
// independent reading of the spec: no Prometheus parser is in the module
// graph, and a test that reused escapeLabel's inverse would only prove the
// code agrees with itself.
func parseLabelValue(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `="`)
	if start < 0 {
		t.Fatalf("no label in %q", body)
	}
	var out strings.Builder
	for i := start + 2; i < len(body); i++ {
		c := body[i]
		switch c {
		case '"':
			return out.String()
		case '\\':
			i++
			if i >= len(body) {
				t.Fatalf("dangling escape in %q", body)
			}
			switch body[i] {
			case '\\':
				out.WriteByte('\\')
			case '"':
				out.WriteByte('"')
			case 'n':
				out.WriteByte('\n')
			default:
				t.Fatalf("undefined escape \\%c in %q", body[i], body)
			}
		case '\n':
			t.Fatalf("raw line feed inside a label value in %q", body)
		default:
			out.WriteByte(c)
		}
	}
	t.Fatalf("unterminated label value in %q", body)
	return ""
}

// TestWrite_ZeroSpareIsExported pairs with the alerting fix: a
// suppressed zero would remove sysmon_disk_spare_percent from the scrape at the
// exact moment spare capacity is exhausted, so a Prometheus rule watching it
// would see an absent series instead of a critical value.
func TestWrite_ZeroSpareIsExported(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "nvme0n1", SMARTEnabled: true, SMARTHealthy: true,
		AvailableSparePercent: 0, WearLevelPercent: 0,
	}}}
	out := render(snap)
	for _, want := range []string{
		`sysmon_disk_spare_percent{device="nvme0n1"} 0`,
		`sysmon_disk_wear_percent{device="nvme0n1"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}
}

// TestWrite_EnduranceIsNVMeOnly keeps the endurance series off
// devices where those fields have no meaning.
func TestWrite_EnduranceIsNVMeOnly(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "sda", SMARTEnabled: true, SMARTHealthy: true,
	}}}
	if strings.Contains(render(snap), "sysmon_disk_spare_percent") {
		t.Error("a SATA disk should not export NVMe endurance series")
	}
}

// TestRender_MemberDrivesAreLabelledBySerial pins the label choice, which is
// not cosmetic. A drive behind a controller has no device name, and the names
// of the things around it are not stable: ci-node-1's rebuild renamed every
// PERC and BOSS device on the machine. A series keyed on position would have
// silently become a different series across that reboot -- worse than a
// missing one, because the history looks continuous.
func TestRender_MemberDrivesAreLabelledBySerial(t *testing.T) {
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{
			Name: "sdb",
			Members: []types.DiskMember{
				{DeviceID: 6, Serial: "L0J4VK8J", Temperature: 33, SMARTEnabled: true, SMARTHealthy: true},
				{DeviceID: 7, Serial: "L0J4Y3AJ", Temperature: 34, SMARTEnabled: true, SMARTHealthy: false},
				// No serial: no stable identity, so no series. Two unlabelled
				// drives would otherwise collide into one.
				{DeviceID: 8, Temperature: 33, SMARTEnabled: true, SMARTHealthy: true},
			},
		}},
	}

	out := render(snap)

	for _, want := range []string{
		`sysmon_disk_member_smart_healthy{serial="L0J4VK8J",volume="sdb"} 1`,
		`sysmon_disk_member_smart_healthy{serial="L0J4Y3AJ",volume="sdb"} 0`,
		`sysmon_disk_member_temperature_celsius{serial="L0J4VK8J",volume="sdb"} 33`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series:\n%s", want)
		}
	}
	// The volume label is what lets a query group an array's members.
	if strings.Contains(out, `serial="",`) {
		t.Error("a drive with no serial was exported, which collides with every other drive with no serial")
	}
	if got := strings.Count(out, "sysmon_disk_member_smart_healthy{"); got != 2 {
		t.Errorf("exported %d member health series, want 2", got)
	}
}

// TestRender_NoSyncPercentWhenTheSyncStateWasNeverRead keeps a percentage
// nobody measured off the dashboard.
//
// A hardware RAID volume's rebuild progress is not read, so there is no
// percentage to export. Exporting one would put a number on a graph that no
// rule could tell from a measured value -- 0 reading as a stalled rebuild and
// 100 as a finished one, on an array that might be neither.
func TestRender_NoSyncPercentWhenTheSyncStateWasNeverRead(t *testing.T) {
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{
			// A hardware volume: state known, sync state not.
			{Name: "sda", RAID: &types.RAIDInfo{Level: "raid1", State: "active"}},
			// An md array, which does report it.
			{Name: "md0", RAID: &types.RAIDInfo{
				Level: "raid10", State: "active", SyncAction: "idle", SyncProgressPercent: 100}},
		},
	}

	out := render(snap)

	if strings.Contains(out, `sysmon_raid_sync_percent{array="sda"}`) {
		t.Error("exported a sync percentage for an array whose sync state was never read")
	}
	// The same trap one field over, and the one the gate found after the sync
	// fix: a hardware volume's mismatch count is zero because nothing asked,
	// and "0 sectors differed" is a clean consistency check that never ran.
	if strings.Contains(out, `sysmon_raid_mismatch_count{array="sda"}`) {
		t.Error("exported a mismatch count for an array whose consistency state was never read")
	}
	// The md array must keep its series: the fix must not have bought silence
	// by dropping the metric for everyone.
	if !strings.Contains(out, `sysmon_raid_sync_percent{array="md0"} 100`) {
		t.Error("dropped the sync percentage for an md array that reports one")
	}
	if !strings.Contains(out, `sysmon_raid_mismatch_count{array="md0"}`) {
		t.Error("dropped the mismatch count for an md array that reports one")
	}
	// Everything else about the hardware volume is still exported -- the gap
	// is one field wide, not the whole array.
	if !strings.Contains(out, `sysmon_raid_total_devices{array="sda"}`) {
		t.Error("the hardware volume lost its other RAID series too")
	}
}

// TestTextWriter_GaugeAndCounterDeclareTheirTypes pins the exact exposition
// text for both kinds. The TYPE line is what tells Prometheus how rate() and
// increase() must treat a drop: over a counter it is a reset, over a gauge it
// is a decrease. Declaring the wrong one is silent and produces the wrong
// answer on exactly the host that rebooted.
func TestTextWriter_GaugeAndCounterDeclareTheirTypes(t *testing.T) {
	m := &TextWriter{}
	m.Gauge("sysmon_g", "A gauge.", 1.5, "device", "sda")
	m.Gauge("sysmon_g", "A gauge.", 2, "device", "sdb")
	m.Counter("sysmon_c_total", "A counter.", 42)
	m.Counter("sysmon_l_total", "A labelled counter.", 7, "a", "x", "b", "y")

	want := "# HELP sysmon_g A gauge.\n" +
		"# TYPE sysmon_g gauge\n" +
		"sysmon_g{device=\"sda\"} 1.5\n" +
		"sysmon_g{device=\"sdb\"} 2\n" +
		"# HELP sysmon_c_total A counter.\n" +
		"# TYPE sysmon_c_total counter\n" +
		"sysmon_c_total 42\n" +
		"# HELP sysmon_l_total A labelled counter.\n" +
		"# TYPE sysmon_l_total counter\n" +
		"sysmon_l_total{a=\"x\",b=\"y\"} 7\n"
	if got := m.String(); got != want {
		t.Errorf("exposition mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestTextWriter_CounterKeepsFullPrecision guards the value formatting for
// counters in particular: a page count since boot runs well past the range
// where a short float rendering would round it, and a rounded counter moves in
// steps that rate() reads as bursts.
func TestTextWriter_CounterKeepsFullPrecision(t *testing.T) {
	m := &TextWriter{}
	m.Counter("sysmon_c_total", "A counter.", 123456789)
	if !strings.Contains(m.String(), "sysmon_c_total 1.23456789e+08\n") {
		t.Errorf("counter value lost precision:\n%s", m.String())
	}
}

// TestWrite_SwapPagingCountersAreCounters is the point of #135: the cumulative
// pswpin/pswpout totals are exported, under _total names, declared as counters.
func TestWrite_SwapPagingCountersAreCounters(t *testing.T) {
	out := render(&types.Snapshot{Memory: types.MemoryInfo{
		SwapInPages: 1204, SwapOutPages: 5310, SwapCountersAvailable: true,
	}})

	for _, want := range []string{
		"# TYPE sysmon_swap_in_pages_total counter\n",
		"sysmon_swap_in_pages_total 1204\n",
		"# TYPE sysmon_swap_out_pages_total counter\n",
		"sysmon_swap_out_pages_total 5310\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}
	for _, bad := range []string{
		"# TYPE sysmon_swap_in_pages_total gauge",
		"# TYPE sysmon_swap_out_pages_total gauge",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("a monotonic total was declared a gauge: %q", bad)
		}
	}
}

// TestWrite_SwapCountersOmittedWhenUnread: when /proc/vmstat could not be read
// the collector reports zero, and a counter sitting at zero reads as a host
// that has never paged. An absent series is a gap somebody can see; a
// fabricated zero is not.
func TestWrite_SwapCountersOmittedWhenUnread(t *testing.T) {
	out := render(&types.Snapshot{Memory: types.MemoryInfo{SwapCountersAvailable: false}})

	if strings.Contains(out, "sysmon_swap_in_pages_total") ||
		strings.Contains(out, "sysmon_swap_out_pages_total") {
		t.Errorf("an unread paging counter was exported:\n%s", out)
	}
	// The occupancy gauge is unaffected: it comes from /proc/meminfo, and the
	// gauge rule of exporting a zero rather than dropping the series holds.
	if !strings.Contains(out, "sysmon_swap_used_percent 0\n") {
		t.Errorf("sysmon_swap_used_percent must still be exported:\n%s", out)
	}
}

// TestWrite_ZeroSwapCountersExportedWhenRead: a host that genuinely has never
// paged reads its counters successfully and reports zero. That zero is a
// measurement, and it has to be published, or the series would only appear
// the first time the host swaps.
func TestWrite_ZeroSwapCountersExportedWhenRead(t *testing.T) {
	out := render(&types.Snapshot{Memory: types.MemoryInfo{SwapCountersAvailable: true}})

	for _, want := range []string{
		"sysmon_swap_in_pages_total 0\n",
		"sysmon_swap_out_pages_total 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}
}

// TestWrite_IsDeterministicWithCounters: the same snapshot rendered twice is
// byte-identical, counters included. The scrape-versus-push comparison and any
// diff of two scrapes rely on it.
func TestWrite_IsDeterministicWithCounters(t *testing.T) {
	snap := &types.Snapshot{Memory: types.MemoryInfo{
		SwapInPages: 1, SwapOutPages: 2, SwapCountersAvailable: true,
	}}
	if first, second := render(snap), render(snap); first != second {
		t.Errorf("two renders differ\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestWrite_PageSizeIsExportedAsAGauge. The page size is what the history
// charts multiply the paging counters' rate by to draw bytes, and it differs
// between hosts -- 16 KiB here, as on some arm64 kernels -- so it has to come
// from the node rather than from a constant.
func TestWrite_PageSizeIsExportedAsAGauge(t *testing.T) {
	out := render(&types.Snapshot{Memory: types.MemoryInfo{PageSizeBytes: 16384}})
	for _, want := range []string{
		"# TYPE sysmon_memory_page_size_bytes gauge\n",
		"sysmon_memory_page_size_bytes 16384\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}
}

// TestWrite_PageSizeOmittedWhenUnknown. A reading with no page size -- a peer
// running a build from before the field -- must not publish a zero: every
// swap rate multiplied by it would draw as no paging at all, where an absent
// series leaves the chart honestly empty.
func TestWrite_PageSizeOmittedWhenUnknown(t *testing.T) {
	out := render(&types.Snapshot{Memory: types.MemoryInfo{SwapCountersAvailable: true}})
	if strings.Contains(out, "sysmon_memory_page_size_bytes") {
		t.Errorf("an unknown page size was exported:\n%s", out)
	}
}

// TestWrite_EverySeriesIsUnique writes a two-socket host with two of every
// device and checks no two samples share a metric name and label set. Two that
// did would be one series with two values at one time: a scrape keeps an
// arbitrary one, and remote write sends a duplicate the receiver refuses with
// the whole batch. The two packages' Core 0 was exactly that.
func TestWrite_EverySeriesIsUnique(t *testing.T) {
	snap := &types.Snapshot{
		CPUs: []types.CPUInfo{{UsagePercent: 20, UsageAvailable: true}},
		Sensors: types.SensorData{
			CoreTemps: []types.CoreTemp{
				{PackageID: 0, CoreID: 0, TempCelsius: 50}, {PackageID: 0, CoreID: 1, TempCelsius: 51},
				{PackageID: 1, CoreID: 0, TempCelsius: 60}, {PackageID: 1, CoreID: 1, TempCelsius: 61},
			},
			PackagePower: []types.PackagePower{{PackageName: "package-0", PowerW: 90}, {PackageName: "package-1", PowerW: 95}},
			PSI:          types.PSIData{Available: true},
		},
		GPUs: []types.GPUInfo{{Index: 0}, {Index: 1}},
		NPUs: []types.NPUInfo{{Index: 0}, {Index: 1}},
		Disks: []types.DiskInfo{
			{Name: "nvme0n1", SMARTEnabled: true, TotalBytes: 1, Temperature: 40, WearLevelPercent: 1},
			{Name: "nvme1n1", SMARTEnabled: true, TotalBytes: 1, Temperature: 41, WearLevelPercent: 2},
			{Name: "sda", Members: []types.DiskMember{{Serial: "A", Temperature: 30}, {Serial: "B", Temperature: 31}}},
			{Name: "md0", RAID: &types.RAIDInfo{Level: "raid1", SyncAction: "idle"}},
		},
		Networks: []types.NetworkInfo{{Name: "eth0", IsUp: true}, {Name: "eth1", IsUp: true}},
		Alerts: []types.Alert{
			{Rule: "cpu_temp", Severity: types.SeverityWarning, Category: "cpu"},
			{Rule: "memory_high", Severity: types.SeverityWarning, Category: "memory"},
			{Rule: "disk_full", Severity: types.SeverityCritical, Category: "storage", Resource: "nvme0n1"},
			{Rule: "disk_full", Severity: types.SeverityCritical, Category: "storage", Resource: "nvme1n1"},
		},
	}

	seen := make(map[string]bool)
	for _, line := range strings.Split(render(snap), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series := line[:strings.LastIndexByte(line, ' ')]
		if seen[series] {
			t.Errorf("series %s is written twice", series)
		}
		seen[series] = true
	}
	for _, want := range []string{
		`sysmon_cpu_temperature_celsius{package="0",core="0"}`,
		`sysmon_cpu_temperature_celsius{package="1",core="0"}`,
	} {
		if !seen[want] {
			t.Errorf("no series %s", want)
		}
	}
}
