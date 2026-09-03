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
