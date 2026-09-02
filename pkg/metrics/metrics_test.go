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
		CPUs:       []types.CPUInfo{{UsagePercent: 20}, {UsagePercent: 40}},
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
