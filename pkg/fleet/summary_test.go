package fleet_test

import (
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// pagingSnapshot is a snapshot at t with the given cumulative swap counters.
func pagingSnapshot(t time.Time, in, out uint64) *types.Snapshot {
	s := healthySnapshot("pager")
	s.Timestamp = t
	s.Memory.SwapPercent = 98.6
	s.Memory.SwapInPages = in
	s.Memory.SwapOutPages = out
	s.Memory.PageSizeBytes = 4096
	s.Memory.SwapCountersAvailable = true
	return s
}

// TestSummarize_SwapRateOverOneInterval is #134's signal: bytes per second
// paged out across the two readings, not the occupancy that could not tell a
// node paging now from one that paged last week.
func TestSummarize_SwapRateOverOneInterval(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	prev := pagingSnapshot(t0, 1000, 5000)
	cur := pagingSnapshot(t0.Add(2*time.Second), 1100, 5500)

	s := fleet.Summarize(prev, cur)
	if !s.SwapRateAvailable || !s.SwapCountersAvailable {
		t.Fatalf("rate available = %v, counters available = %v; both were measurable",
			s.SwapRateAvailable, s.SwapCountersAvailable)
	}
	if want := 100.0 * 4096 / 2; s.SwapInRate != want {
		t.Errorf("SwapInRate = %v, want %v (100 pages x 4 KiB over 2 s)", s.SwapInRate, want)
	}
	if want := 500.0 * 4096 / 2; s.SwapOutRate != want {
		t.Errorf("SwapOutRate = %v, want %v (500 pages x 4 KiB over 2 s)", s.SwapOutRate, want)
	}
	if s.SwapPercent != 98.6 {
		t.Errorf("SwapPercent = %v; occupancy still travels beside the rate", s.SwapPercent)
	}

	// Full swap and no paging: the case #134 was filed for. Occupancy alone
	// read this as trouble.
	idle := fleet.Summarize(prev, pagingSnapshot(t0.Add(2*time.Second), 1000, 5000))
	if !idle.SwapRateAvailable || idle.SwapOutRate != 0 {
		t.Errorf("a node with full swap and no paging = available %v, out %v; want a measured zero",
			idle.SwapRateAvailable, idle.SwapOutRate)
	}
}

// TestSummarize_SwapRateUnknownRatherThanZero: every way the rate cannot be
// measured must say so. A zero here would render as "not paging", which is the
// absence-reads-as-zero defect #10 closed one instance of.
func TestSummarize_SwapRateUnknownRatherThanZero(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	later := t0.Add(2 * time.Second)

	noCounters := pagingSnapshot(later, 1100, 5500)
	noCounters.Memory.SwapCountersAvailable = false
	noPageSize := pagingSnapshot(later, 1100, 5500)
	noPageSize.Memory.PageSizeBytes = 0

	cases := map[string]struct {
		prev, cur *types.Snapshot
	}{
		"no previous reading":     {nil, pagingSnapshot(later, 1100, 5500)},
		"counters unreadable now": {pagingSnapshot(t0, 1000, 5000), noCounters},
		"counters unreadable before": {func() *types.Snapshot {
			s := pagingSnapshot(t0, 1000, 5000)
			s.Memory.SwapCountersAvailable = false
			return s
		}(), pagingSnapshot(later, 1100, 5500)},
		"no page size":           {pagingSnapshot(t0, 1000, 5000), noPageSize},
		"swap-out counter reset": {pagingSnapshot(t0, 1000, 5000), pagingSnapshot(later, 1100, 20)},
		"swap-in counter reset":  {pagingSnapshot(t0, 1000, 5000), pagingSnapshot(later, 3, 5500)},
		"rebooted, counters already past the old values": {func() *types.Snapshot {
			s := pagingSnapshot(t0, 1000, 5000)
			s.Host.BootTime = 1_790_000_000
			return s
		}(), func() *types.Snapshot {
			s := pagingSnapshot(later, 1100, 5500)
			s.Host.BootTime = 1_790_086_400
			return s
		}()},
		"same instant":          {pagingSnapshot(t0, 1000, 5000), pagingSnapshot(t0, 1100, 5500)},
		"readings out of order": {pagingSnapshot(later, 1000, 5000), pagingSnapshot(t0, 1100, 5500)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := fleet.Summarize(tc.prev, tc.cur)
			if s.SwapRateAvailable {
				t.Errorf("SwapRateAvailable = true with in=%v out=%v; the rate was not measurable",
					s.SwapInRate, s.SwapOutRate)
			}
			if s.SwapInRate != 0 || s.SwapOutRate != 0 {
				t.Errorf("rates = %v, %v; an unmeasured rate carries no number", s.SwapInRate, s.SwapOutRate)
			}
		})
	}

	if s := fleet.Summarize(nil, noCounters); s.SwapCountersAvailable {
		t.Error("SwapCountersAvailable = true for a node that could not read its counters")
	}
}

// TestSummarize_PowerIsReportedOnlyWithASensor: a node with no power sensor
// sends zero watts, and a fleet total that counted it would read a machine
// with no sensor as a machine drawing nothing.
func TestSummarize_PowerIsReportedOnlyWithASensor(t *testing.T) {
	none := fleet.Summarize(nil, healthySnapshot("vm"))
	if none.PowerReported || none.PowerW != 0 {
		t.Errorf("a node with no sensor = reported %v, %v W; want not reported", none.PowerReported, none.PowerW)
	}

	rapl := healthySnapshot("metal")
	rapl.Sensors.PackagePower = []types.PackagePower{{PowerW: 120}, {PowerW: 95}}
	rapl.GPUs = []types.GPUInfo{{PowerDrawW: 100}, {PowerDrawW: 0}}
	s := fleet.Summarize(nil, rapl)
	if !s.PowerReported || s.PowerW != 315 {
		t.Errorf("power = reported %v, %v W; want reported, 315 W (two packages and one GPU)", s.PowerReported, s.PowerW)
	}

	// A GPU that reports no draw is not a power sensor for this purpose.
	gpuOnly := healthySnapshot("gpu")
	gpuOnly.GPUs = []types.GPUInfo{{PowerDrawW: 0}}
	if s := fleet.Summarize(nil, gpuOnly); s.PowerReported {
		t.Error("PowerReported = true for a node whose only GPU reports no draw")
	}
}

// TestSummarize_ControllerMembersCountTowardDenominators is #45/#51 in the
// summary: the drives behind a RAID controller are not in the disk list, and a
// denominator that skipped them would not know what it left out.
func TestSummarize_ControllerMembersCountTowardDenominators(t *testing.T) {
	snap := healthySnapshot("ci-node-1")
	snap.Disks = []types.DiskInfo{
		{Name: "nvme0n1", TotalBytes: 100, UsedPercent: 88.2, SMARTEnabled: true, SMARTHealthy: true, Temperature: 41},
		{Name: "sda", TotalBytes: 100, UsedPercent: 12, RAID: &types.RAIDInfo{State: "clean"},
			Members: []types.DiskMember{
				{Serial: "A", SMARTEnabled: true, SMARTHealthy: true, Temperature: 38},
				{Serial: "B", SMARTEnabled: true, SMARTHealthy: false, Temperature: 52},
				{Serial: "C", SMARTEnabled: false},
			}},
		{Name: "md0", TotalBytes: 100, UsedPercent: 5, RAID: &types.RAIDInfo{State: "inactive"}},
	}

	s := fleet.Summarize(nil, snap)
	if s.SMARTCapable != 3 || s.SMARTUnhealthy != 1 {
		t.Errorf("SMART = %d unhealthy of %d; want 1 of 3 -- the NVMe and the two members that report",
			s.SMARTUnhealthy, s.SMARTCapable)
	}
	if s.RAIDArrays != 2 || s.RAIDUnhealthy != 1 || s.RAIDDegraded {
		t.Errorf("RAID = %d unhealthy of %d (degraded %v); want the inactive array unhealthy and none degraded",
			s.RAIDUnhealthy, s.RAIDArrays, s.RAIDDegraded)
	}
	if s.WorstDiskPercent != 88.2 {
		t.Errorf("WorstDiskPercent = %v, want 88.2", s.WorstDiskPercent)
	}
	if s.MaxTempC != 52 {
		t.Errorf("MaxTempC = %v, want the hottest member's 52", s.MaxTempC)
	}
}

// TestSummarize_CarriesTheAlertsThatSetItsState: the roll-up names why a node
// is not healthy, which only the alert can say. Informational alerts set no
// state and are left behind.
func TestSummarize_CarriesTheAlertsThatSetItsState(t *testing.T) {
	snap := healthySnapshot("fs")
	snap.Alerts = []types.Alert{
		{ID: "a", Severity: types.SeverityWarning, Message: "disk /srv 92.1% used"},
		{ID: "b", Severity: types.SeverityInfo, Message: "kernel updated"},
		{ID: "c", Severity: types.SeverityCritical, Message: "array md0 is inactive"},
	}
	s := fleet.Summarize(nil, snap)
	if s.WarningCount != 1 || s.CriticalCount != 1 {
		t.Errorf("counts = %d warning, %d critical; want 1 and 1", s.WarningCount, s.CriticalCount)
	}
	if len(s.Alerts) != 2 {
		t.Fatalf("Alerts = %+v; want the warning and the critical, not the info", s.Alerts)
	}
	for _, a := range s.Alerts {
		if a.Severity == types.SeverityInfo {
			t.Errorf("an informational alert travelled: %+v", a)
		}
	}

	if quiet := fleet.Summarize(nil, healthySnapshot("ok")); quiet.Alerts == nil {
		t.Error("Alerts is nil for a node with none; it must encode as [], not null")
	}
}

// TestSummarize_CarriesIdentityAndLoad covers the plain fields end to end.
func TestSummarize_CarriesIdentityAndLoad(t *testing.T) {
	snap := healthySnapshot("omega")
	snap.GitCommit, snap.BuildDate = "a46f6cf", "2026-09-22"
	snap.Networks = []types.NetworkInfo{
		{Name: "lo", IsLoopback: true, BytesRecvRate: 1e9, BytesSentRate: 1e9},
		{Name: "eth0", BytesRecvRate: 1000, BytesSentRate: 500, RatesAvailable: true},
	}
	s := fleet.Summarize(nil, snap)
	if s.Hostname != "omega" || s.Version != "0.1.2" || s.GitCommit != "a46f6cf" || s.BuildDate != "2026-09-22" || s.UptimeSeconds != 3600 {
		t.Errorf("identity = %+v", s)
	}
	if s.CPUPercent != 20 || s.LoadPerCore != 0.5 || s.MemoryPercent != 40 {
		t.Errorf("cpu %v load/core %v mem %v; want 20, 0.5, 40", s.CPUPercent, s.LoadPerCore, s.MemoryPercent)
	}
	if !s.NetReported || s.NetRxRate != 1000 || s.NetTxRate != 500 {
		t.Errorf("network = reported %v, %v rx, %v tx; loopback must not count", s.NetReported, s.NetRxRate, s.NetTxRate)
	}
}

// TestSummarize_HottestComponentIncludesAccelerators: the node's hottest
// reading is taken over every component that reports one, and names the kind.
// A sensor that reports nothing reads zero and never wins.
func TestSummarize_HottestComponentIncludesAccelerators(t *testing.T) {
	base := func() *types.Snapshot {
		s := healthySnapshot("gpu-host")
		s.Sensors.CoreTemps = []types.CoreTemp{{TempCelsius: 62}}
		s.Disks[0].Temperature = 41
		return s
	}
	cases := map[string]struct {
		edit func(*types.Snapshot)
		temp float64
		kind types.TempSource
	}{
		"cpu hottest":        {func(*types.Snapshot) {}, 62, types.TempCPU},
		"gpu core hottest":   {func(s *types.Snapshot) { s.GPUs = []types.GPUInfo{{TemperatureGPU: 88}} }, 88, types.TempGPU},
		"gpu memory hottest": {func(s *types.Snapshot) { s.GPUs = []types.GPUInfo{{TemperatureGPU: 70, TemperatureMemory: 94}} }, 94, types.TempGPU},
		"npu hottest":        {func(s *types.Snapshot) { s.NPUs = []types.NPUInfo{{TemperatureC: 75}} }, 75, types.TempNPU},
		"drive hottest":      {func(s *types.Snapshot) { s.Disks[0].Temperature = 66 }, 66, types.TempDrive},
		"accelerators report nothing": {func(s *types.Snapshot) {
			s.GPUs = []types.GPUInfo{{}}
			s.NPUs = []types.NPUInfo{{}}
		}, 62, types.TempCPU},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := base()
			tc.edit(s)
			got := fleet.Summarize(nil, s)
			if got.MaxTempC != tc.temp || got.MaxTempSource != tc.kind {
				t.Errorf("hottest = %v from %q, want %v from %q", got.MaxTempC, got.MaxTempSource, tc.temp, tc.kind)
			}
		})
	}

	if none := fleet.Summarize(nil, healthySnapshot("vm")); none.MaxTempC != 0 || none.MaxTempSource != "" {
		t.Errorf("a node with no thermometer = %v from %q; want nothing", none.MaxTempC, none.MaxTempSource)
	}
}

// TestSummarize_NPUPowerCounts: an accelerator's draw is part of the node's,
// as the Prometheus exposition already treats it.
func TestSummarize_NPUPowerCounts(t *testing.T) {
	s := healthySnapshot("npu-host")
	s.NPUs = []types.NPUInfo{{PowerDrawW: 12}, {PowerDrawW: 0}}
	if got := fleet.Summarize(nil, s); !got.PowerReported || got.PowerW != 12 {
		t.Errorf("power = reported %v, %v W; want 12 W from the NPU that reports", got.PowerReported, got.PowerW)
	}
}

// TestSummarize_AnUnknownBootTimeIsNotAReboot: a boot time of zero is
// unknown, and the rate is measured as usual.
func TestSummarize_AnUnknownBootTimeIsNotAReboot(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	prev := pagingSnapshot(t0, 0, 0)
	cur := pagingSnapshot(t0.Add(time.Second), 0, 10)
	cur.Host.BootTime = 1_790_000_000
	if s := fleet.Summarize(prev, cur); !s.SwapRateAvailable {
		t.Error("a missing boot time on one side was taken as a reboot")
	}
}

// TestSummarize_ThroughputCountsEachPacketOnce: a node's throughput is summed
// over its physical devices, so a bond, a bridge, a VLAN and a guest's vnet
// port -- all carrying traffic the NICs also carry -- add nothing twice. A node
// with no physical device (its own network namespace) reports no throughput
// rather than an overlapping sum.
func TestSummarize_ThroughputCountsEachPacketOnce(t *testing.T) {
	host := healthySnapshot("hypervisor")
	host.Networks = []types.NetworkInfo{
		{Name: "lo", IsLoopback: true, IsVirtual: true, BytesRecvRate: 1e9, BytesSentRate: 1e9},
		{Name: "eno1", BondMaster: "bond0", BytesRecvRate: 600, BytesSentRate: 300, RatesAvailable: true},
		{Name: "eno2", BondMaster: "bond0", BytesRecvRate: 400, BytesSentRate: 200, RatesAvailable: true},
		{Name: "bond0", IsVirtual: true, BytesRecvRate: 1000, BytesSentRate: 500},
		{Name: "bond0.20", IsVirtual: true, BytesRecvRate: 200, BytesSentRate: 100},
		{Name: "br0", IsVirtual: true, BytesRecvRate: 700, BytesSentRate: 350},
		{Name: "vnet0", IsVirtual: true, BytesRecvRate: 300, BytesSentRate: 150},
	}
	s := fleet.Summarize(nil, host)
	if !s.NetReported || s.NetRxRate != 1000 || s.NetTxRate != 500 {
		t.Errorf("hypervisor throughput = reported %v, %v rx, %v tx; want the NICs' 1000 and 500",
			s.NetReported, s.NetRxRate, s.NetTxRate)
	}

	container := healthySnapshot("ctr")
	container.Networks = []types.NetworkInfo{
		{Name: "lo", IsLoopback: true, IsVirtual: true, BytesRecvRate: 1e9},
		{Name: "eth0", IsVirtual: true, BytesRecvRate: 800, BytesSentRate: 80},
		{Name: "wg0", IsVirtual: true, BytesRecvRate: 700, BytesSentRate: 70},
	}
	if s := fleet.Summarize(nil, container); s.NetReported || s.NetRxRate != 0 || s.NetTxRate != 0 {
		t.Errorf("namespaced node = reported %v, %v rx, %v tx; want no throughput reported", s.NetReported, s.NetRxRate, s.NetTxRate)
	}
}

// TestSummarize_AnUnmeasuredRAPLPackageIsNotPower: a package's first reading
// is zero because there is no earlier energy sample to difference; that is
// "not yet measured", and the node is not counted as reporting power.
func TestSummarize_AnUnmeasuredRAPLPackageIsNotPower(t *testing.T) {
	s := healthySnapshot("metal")
	s.Sensors.PackagePower = []types.PackagePower{{PowerW: 0}, {PowerW: 0}}
	if got := fleet.Summarize(nil, s); got.PowerReported {
		t.Errorf("power reported at %v W from packages with no reading yet", got.PowerW)
	}
}

// TestSummarize_ADriveSharedByTwoVirtualDisksIsCountedOnce: a controller's
// virtual disks on one disk group list the same drives; each is one device,
// and one failing drive is one fault.
func TestSummarize_ADriveSharedByTwoVirtualDisksIsCountedOnce(t *testing.T) {
	members := []types.DiskMember{
		{Serial: "A", SMARTEnabled: true, SMARTHealthy: true},
		{Serial: "B", SMARTEnabled: true, SMARTHealthy: false},
		{SMARTEnabled: true, SMARTHealthy: true}, // no serial: counted where it appears
	}
	snap := healthySnapshot("perc")
	snap.Disks = []types.DiskInfo{
		{Name: "sda", TotalBytes: 100, RAID: &types.RAIDInfo{State: "clean"}, Members: members},
		{Name: "sdb", TotalBytes: 100, RAID: &types.RAIDInfo{State: "clean"}, Members: members},
	}
	s := fleet.Summarize(nil, snap)
	if s.SMARTCapable != 4 || s.SMARTUnhealthy != 1 {
		t.Errorf("SMART = %d unhealthy of %d; want 1 of 4 -- A and B once each, the unidentified member twice",
			s.SMARTUnhealthy, s.SMARTCapable)
	}
}

// TestSummarize_UnmeasuredRatesAreNotThroughput: a physical device whose
// counters have no earlier reading reads zero; the node then reports no
// throughput rather than a sum that presents the unknown as idle.
func TestSummarize_UnmeasuredRatesAreNotThroughput(t *testing.T) {
	snap := healthySnapshot("fresh")
	snap.Networks = []types.NetworkInfo{
		{Name: "eno1", BytesRecvRate: 900, RatesAvailable: true},
		{Name: "eno2"}, // just appeared: no rate yet
	}
	if s := fleet.Summarize(nil, snap); s.NetReported || s.NetRxRate != 0 {
		t.Errorf("with an unmeasured device: reported %v, rx %v; want not reported", s.NetReported, s.NetRxRate)
	}
}
