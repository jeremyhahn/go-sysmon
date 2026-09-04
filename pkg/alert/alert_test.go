package alert_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/metrics"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// find returns the alert with the given rule, or nil.
func find(alerts []types.Alert, rule string) *types.Alert {
	for i := range alerts {
		if alerts[i].Rule == rule {
			return &alerts[i]
		}
	}
	return nil
}

func defaults() config.Alerts { return config.Defaults().Alerts }

// TestEvaluate_RAIDDegradedIsCritical covers the condition this whole feature
// was built for: fs ran a degraded root mirror for nine months unnoticed.
func TestEvaluate_RAIDDegradedIsCritical(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md2",
		RAID: &types.RAIDInfo{State: "clean", Level: "raid1", Degraded: true, ActiveDevices: 1, TotalDevices: 2},
	}}}

	got := alert.New().Evaluate(defaults(), snap, time.Now())
	a := find(got, alert.RuleRAIDDegraded)
	if a == nil {
		t.Fatalf("no raid_degraded alert in %+v", got)
	}
	if a.Severity != types.SeverityCritical {
		t.Errorf("severity = %q, want critical", a.Severity)
	}
	if a.Resource != "md2" || a.Category != "storage" {
		t.Errorf("resource/category = %q/%q, want md2/storage", a.Resource, a.Category)
	}
}

// TestEvaluate_HealthyArrayIsSilent is the other half: a healthy array must
// produce nothing, or the bell becomes noise people learn to ignore.
func TestEvaluate_HealthyArrayIsSilent(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md0",
		RAID: &types.RAIDInfo{State: "clean", Level: "raid10", ActiveDevices: 4, TotalDevices: 4, SyncAction: "idle"},
	}}}
	if got := alert.New().Evaluate(defaults(), snap, time.Now()); len(got) != 0 {
		t.Errorf("healthy array produced %+v, want no alerts", got)
	}
}

// TestEvaluate_PerResourceOverride covers the case that motivated overrides: a
// filesystem expected to run nearly full should not alert at the global level.
func TestEvaluate_PerResourceOverride(t *testing.T) {
	cfg := defaults()
	cfg.Overrides = []config.Override{{Resource: "md0", DiskFullPct: 98, DiskFullCritPct: 99}}

	snap := &types.Snapshot{Disks: []types.DiskInfo{
		{Name: "md0", TotalBytes: 100, UsedPercent: 97},
		{Name: "md2", TotalBytes: 100, UsedPercent: 97},
	}}

	got := alert.New().Evaluate(cfg, snap, time.Now())
	var forMD0, forMD2 bool
	for _, a := range got {
		if a.Rule != alert.RuleDiskFull {
			continue
		}
		switch a.Resource {
		case "md0":
			forMD0 = true
		case "md2":
			forMD2 = true
		}
	}
	if forMD0 {
		t.Error("md0 has a 98% override and should not alert at 97%")
	}
	if !forMD2 {
		t.Error("md2 uses the default 90% threshold and should alert at 97%")
	}
}

// TestEvaluate_DisabledRule verifies a zero threshold turns a rule off, which
// is how an operator silences a check they do not want.
func TestEvaluate_DisabledRule(t *testing.T) {
	cfg := defaults()
	cfg.DiskFullPct, cfg.DiskFullCritPct = 0, 0

	snap := &types.Snapshot{Disks: []types.DiskInfo{{Name: "sda", TotalBytes: 100, UsedPercent: 99}}}
	if a := find(alert.New().Evaluate(cfg, snap, time.Now()), alert.RuleDiskFull); a != nil {
		t.Errorf("disk_full fired with the rule disabled: %+v", a)
	}
}

// TestEvaluate_InterfaceDownOnlyWhenConfigured guards against the noisiest
// possible rule: hosts routinely carry unused NICs that are legitimately down.
func TestEvaluate_InterfaceDownOnlyWhenConfigured(t *testing.T) {
	snap := &types.Snapshot{Networks: []types.NetworkInfo{
		{Name: "eno1", IsUp: false, Addresses: []string{"192.168.1.5/24"}},
		{Name: "wlp2s0", IsUp: false},
		{Name: "lo", IsUp: false, IsLoopback: true, Addresses: []string{"127.0.0.1/8"}},
	}}

	got := alert.New().Evaluate(defaults(), snap, time.Now())
	var resources []string
	for _, a := range got {
		if a.Rule == alert.RuleIfaceDown {
			resources = append(resources, a.Resource)
		}
	}
	if len(resources) != 1 || resources[0] != "eno1" {
		t.Errorf("iface_down fired for %v, want only eno1", resources)
	}
}

// TestEvaluate_SinceIsStableWhileFiring checks the timestamp the UI uses to
// show how long a condition has lasted: it must not reset every snapshot.
func TestEvaluate_SinceIsStableWhileFiring(t *testing.T) {
	e := alert.New()
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md2",
		RAID: &types.RAIDInfo{State: "clean", Level: "raid1", Degraded: true, ActiveDevices: 1, TotalDevices: 2},
	}}}

	t0 := time.Now()
	first := e.Evaluate(defaults(), snap, t0)
	second := e.Evaluate(defaults(), snap, t0.Add(30*time.Second))

	if !first[0].Since.Equal(second[0].Since) {
		t.Errorf("Since changed while the alert kept firing: %v then %v", first[0].Since, second[0].Since)
	}
}

// TestEvaluate_SinceResetsAfterClearing confirms a flapping condition reports
// the age of its current episode rather than the first one ever seen.
func TestEvaluate_SinceResetsAfterClearing(t *testing.T) {
	e := alert.New()
	degraded := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md2", RAID: &types.RAIDInfo{State: "clean", Level: "raid1", Degraded: true, ActiveDevices: 1, TotalDevices: 2},
	}}}
	healthy := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md2", RAID: &types.RAIDInfo{State: "clean", Level: "raid1", ActiveDevices: 2, TotalDevices: 2, SyncAction: "idle"},
	}}}

	t0 := time.Now()
	e.Evaluate(defaults(), degraded, t0)
	e.Evaluate(defaults(), healthy, t0.Add(time.Minute))
	again := e.Evaluate(defaults(), degraded, t0.Add(2*time.Minute))

	if !again[0].Since.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("Since = %v, want the time the condition returned", again[0].Since)
	}
}

// TestEvaluate_ThrottleNeedsAnIncrease verifies throttling is reported from a
// counter rising, not from a machine that throttled at some point in its past.
func TestEvaluate_ThrottleNeedsAnIncrease(t *testing.T) {
	e := alert.New()
	snap := &types.Snapshot{Sensors: types.SensorData{
		ThermalThrottle: []types.ThrottleInfo{{CPU: 0, CoreThrottleCount: 5}},
	}}

	if a := find(e.Evaluate(defaults(), snap, time.Now()), alert.RuleCPUThrottle); a != nil {
		t.Error("a pre-existing throttle count should not alert on the first snapshot")
	}

	snap.Sensors.ThermalThrottle[0].CoreThrottleCount = 9
	if a := find(e.Evaluate(defaults(), snap, time.Now()), alert.RuleCPUThrottle); a == nil {
		t.Error("a rising throttle count should alert")
	}
}

// TestEvaluate_NilSnapshot covers the degenerate input.
func TestEvaluate_NilSnapshot(t *testing.T) {
	if got := alert.New().Evaluate(defaults(), nil, time.Now()); got != nil {
		t.Errorf("Evaluate(nil) = %+v, want nil", got)
	}
}

// TestEvaluate_SortedBySeverity checks the order the dropdown relies on.
func TestEvaluate_SortedBySeverity(t *testing.T) {
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{
			{Name: "sda", TotalBytes: 100, UsedPercent: 92},
			{Name: "md2", RAID: &types.RAIDInfo{State: "clean", Level: "raid1", Degraded: true, ActiveDevices: 1, TotalDevices: 2}},
		},
	}
	got := alert.New().Evaluate(defaults(), snap, time.Now())
	if len(got) < 2 {
		t.Fatalf("want at least two alerts, got %+v", got)
	}
	if got[0].Severity != types.SeverityCritical {
		t.Errorf("first alert severity = %q, want critical first", got[0].Severity)
	}
}

// TestEvaluate_DiskQueueDepth covers the saturation signal latency actually
// follows: a device can sit at 100% utilisation and still be fine if nothing is
// queued behind the requests.
func TestEvaluate_DiskQueueDepth(t *testing.T) {
	cfg := defaults()
	snap := &types.Snapshot{Disks: []types.DiskInfo{
		{Name: "sda", AvgQueueLength: cfg.DiskQueueLen + 2},
		{Name: "sdb", AvgQueueLength: 0.2},
	}}
	got := alert.New().Evaluate(cfg, snap, time.Now())

	var resources []string
	for _, a := range got {
		if a.Rule == alert.RuleDiskQueue {
			resources = append(resources, a.Resource)
		}
	}
	if len(resources) != 1 || resources[0] != "sda" {
		t.Errorf("disk_queue fired for %v, want only sda", resources)
	}
}

// TestEvaluate_CPUUsage checks the mean-across-threads rule, which must read
// the same on a 4-thread and a 64-thread host.
func TestEvaluate_CPUUsage(t *testing.T) {
	cfg := defaults()
	cfg.CPUPct = 90
	snap := &types.Snapshot{CPUs: []types.CPUInfo{
		{UsagePercent: 100}, {UsagePercent: 100}, {UsagePercent: 90}, {UsagePercent: 90},
	}}
	if a := find(alert.New().Evaluate(cfg, snap, time.Now()), alert.RuleCPUHigh); a == nil {
		t.Error("a host at 95% mean CPU should alert")
	}

	snap.CPUs = []types.CPUInfo{{UsagePercent: 100}, {UsagePercent: 10}, {UsagePercent: 10}, {UsagePercent: 10}}
	if a := find(alert.New().Evaluate(cfg, snap, time.Now()), alert.RuleCPUHigh); a != nil {
		t.Errorf("one busy thread out of four should not alert: %+v", a)
	}
}

// TestEvaluate_NetworkUtilisation verifies link saturation is measured only
// where the link speed is known; a percentage of an unknown capacity is
// meaningless.
func TestEvaluate_NetworkUtilisation(t *testing.T) {
	cfg := defaults()
	cfg.NetUtilPct = 80

	// 1 Gbps = 125 MB/s; 110 MB/s is 88% of it.
	snap := &types.Snapshot{Networks: []types.NetworkInfo{
		{Name: "eno1", IsUp: true, Speed: 1000, BytesRecvRate: 110_000_000},
		{Name: "veth0", IsUp: true, Speed: 0, BytesRecvRate: 900_000_000},
	}}
	got := alert.New().Evaluate(cfg, snap, time.Now())

	var resources []string
	for _, a := range got {
		if a.Rule == alert.RuleIfaceUtil {
			resources = append(resources, a.Resource)
		}
	}
	if len(resources) != 1 || resources[0] != "eno1" {
		t.Errorf("iface_util fired for %v, want only the interface with a known speed", resources)
	}
}

// TestEvaluate_GPU covers temperature, video memory and the power envelope.
func TestEvaluate_GPU(t *testing.T) {
	cfg := defaults()
	snap := &types.Snapshot{GPUs: []types.GPUInfo{{
		Index: 0, Name: "Test GPU",
		TemperatureGPU: cfg.GPUTempCritC + 1,
		MemoryTotalMiB: 8192, MemoryUsedMiB: 8000, MemoryPercent: 97,
		PowerDrawW: 99, PowerLimitW: 100,
	}}}
	got := alert.New().Evaluate(cfg, snap, time.Now())

	temp := find(got, alert.RuleGPUTemp)
	if temp == nil || temp.Severity != types.SeverityCritical {
		t.Errorf("gpu_temp = %+v, want critical above the crit threshold", temp)
	}
	if temp != nil && temp.Category != "gpu" {
		t.Errorf("gpu alert category = %q, want gpu so it points at the GPU tab", temp.Category)
	}
	if find(got, alert.RuleGPUMemory) == nil {
		t.Error("gpu memory at 97% should alert")
	}
	if find(got, alert.RuleGPUPower) == nil {
		t.Error("a GPU at 99% of its power limit should alert")
	}
}

// TestEvaluate_GPUBusyIsNotAnAlert guards a deliberate omission: a fully
// utilised GPU is the card doing its job, not a fault.
func TestEvaluate_GPUBusyIsNotAnAlert(t *testing.T) {
	snap := &types.Snapshot{GPUs: []types.GPUInfo{{
		Index: 0, GPUUtilPercent: 100, TemperatureGPU: 60,
		MemoryTotalMiB: 8192, MemoryPercent: 20,
		PowerDrawW: 50, PowerLimitW: 200,
	}}}
	if got := alert.New().Evaluate(defaults(), snap, time.Now()); len(got) != 0 {
		t.Errorf("a busy but healthy GPU produced %+v, want no alerts", got)
	}
}

// TestEvaluate_ContainerMemoryNeedsALimit checks that the percentage is only
// used where it means something: without a limit it is a share of the whole
// host and says nothing about the container hitting a wall.
func TestEvaluate_ContainerMemoryNeedsALimit(t *testing.T) {
	cfg := defaults()
	snap := &types.Snapshot{Virt: types.VirtInfo{Containers: []types.ContainerInfo{
		{Name: "limited", MemoryLimitBytes: 1 << 30, MemoryPercent: 95},
		{Name: "unlimited", MemoryLimitBytes: 0, MemoryPercent: 95},
	}}}
	got := alert.New().Evaluate(cfg, snap, time.Now())

	var resources []string
	for _, a := range got {
		if a.Rule == alert.RuleContainerMem {
			resources = append(resources, a.Resource)
		}
	}
	if len(resources) != 1 || resources[0] != "limited" {
		t.Errorf("container_memory_high fired for %v, want only the limited container", resources)
	}
}

// TestEvaluate_RuntimeUnreachable covers the case where a detected runtime
// stops answering: without it the dashboard shows an idle host rather than an
// unobservable one.
func TestEvaluate_RuntimeUnreachable(t *testing.T) {
	snap := &types.Snapshot{Virt: types.VirtInfo{
		Runtime:    types.RuntimeInfo{Engine: "docker", SocketPath: "/var/run/docker.sock"},
		Capability: types.VirtCapability{RuntimeAPIReachable: false},
	}}
	if a := find(alert.New().Evaluate(defaults(), snap, time.Now()), alert.RuleRuntimeDown); a == nil {
		t.Error("a detected but unreachable runtime should alert")
	}

	snap.Virt.Capability.RuntimeAPIReachable = true
	if a := find(alert.New().Evaluate(defaults(), snap, time.Now()), alert.RuleRuntimeDown); a != nil {
		t.Errorf("a reachable runtime should not alert: %+v", a)
	}
}

// TestEvaluate_PressureIsFiledUnderItsOwnTab checks that each pressure resource
// points at the panel that explains it.
func TestEvaluate_PressureIsFiledUnderItsOwnTab(t *testing.T) {
	cfg := defaults()
	snap := &types.Snapshot{Sensors: types.SensorData{PSI: types.PSIData{
		CPU:    types.PSIResource{FullAvg10: 50},
		Memory: types.PSIResource{FullAvg10: 50},
		IO:     types.PSIResource{FullAvg10: 50},
	}}}
	want := map[string]string{"cpu": "cpu", "memory": "memory", "io": "storage"}
	for _, a := range alert.New().Evaluate(cfg, snap, time.Now()) {
		if a.Rule != alert.RulePSIFull {
			continue
		}
		if got := want[a.Resource]; got != a.Category {
			t.Errorf("psi %s filed under %q, want %q", a.Resource, a.Category, got)
		}
	}
}

// TestEvaluate_NonOperationalArrayIsNotHealthy covers the fail-open case the
// review caught: an array that is not serving I/O has degraded == false and an
// idle sync action, so the earlier rules would have called it healthy. These
// states mean the array is unavailable, which is worse than degraded, not
// better.
func TestEvaluate_NonOperationalArrayIsNotHealthy(t *testing.T) {
	for _, state := range []string{"inactive", "suspended", "clear", "broken"} {
		snap := &types.Snapshot{Disks: []types.DiskInfo{{
			Name: "md0",
			RAID: &types.RAIDInfo{
				State: state, Level: "raid1",
				Degraded: false, SyncAction: "idle",
				ActiveDevices: 2, TotalDevices: 2,
			},
		}}}
		got := alert.New().Evaluate(defaults(), snap, time.Now())
		a := find(got, alert.RuleRAIDNotActive)
		if a == nil {
			t.Errorf("array_state %q produced no raid_not_active alert: %+v", state, got)
			continue
		}
		if a.Severity != types.SeverityCritical {
			t.Errorf("array_state %q severity = %q, want critical", state, a.Severity)
		}
	}
}

// TestEvaluate_UnreadableArrayStateIsReported checks that sysfs telling us
// nothing is surfaced rather than assumed well.
func TestEvaluate_UnreadableArrayStateIsReported(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "md0",
		RAID: &types.RAIDInfo{State: "", Level: "raid1", ActiveDevices: 2, TotalDevices: 2, SyncAction: "idle"},
	}}}
	if a := find(alert.New().Evaluate(defaults(), snap, time.Now()), alert.RuleRAIDNotActive); a == nil {
		t.Error("an array whose state could not be read should be reported, not assumed healthy")
	}
}

// TestEvaluate_OperationalArrayStatesStaySilent guards the other direction: the
// ordinary running states must not trip the new rule.
func TestEvaluate_OperationalArrayStatesStaySilent(t *testing.T) {
	for _, state := range []string{"clean", "active", "active-idle", "write-pending", "read-auto", "readonly"} {
		snap := &types.Snapshot{Disks: []types.DiskInfo{{
			Name: "md0",
			RAID: &types.RAIDInfo{
				State: state, Level: "raid10", SyncAction: "idle",
				ActiveDevices: 4, TotalDevices: 4,
			},
		}}}
		if got := alert.New().Evaluate(defaults(), snap, time.Now()); len(got) != 0 {
			t.Errorf("healthy array in state %q produced %+v, want no alerts", state, got)
		}
	}
}

// TestEvaluate_DiskQueueOverride covers the per-resource override the review
// found declared but never applied: a device given a higher queue threshold
// still fired at the global one.
func TestEvaluate_DiskQueueOverride(t *testing.T) {
	cfg := defaults()
	cfg.DiskQueueLen = 8
	cfg.Overrides = []config.Override{{Resource: "sda", DiskQueueLen: 32}}

	snap := &types.Snapshot{Disks: []types.DiskInfo{
		{Name: "sda", AvgQueueLength: 12}, // above global 8, below its own 32
		{Name: "sdb", AvgQueueLength: 12}, // above the global threshold
	}}
	got := alert.New().Evaluate(cfg, snap, time.Now())

	var resources []string
	for _, a := range got {
		if a.Rule == alert.RuleDiskQueue {
			resources = append(resources, a.Resource)
		}
	}
	if len(resources) != 1 || resources[0] != "sdb" {
		t.Errorf("disk_queue fired for %v, want only sdb; sda's override of 32 should suppress it", resources)
	}
}

// TestEvaluate_ZeroNVMeSpareAlerts is the regression this rule exists for. The
// old guard required AvailableSparePercent > 0 to treat the reading as present,
// which excluded the single most severe value: a drive with no spare blocks
// left never fired the critical rule.
func TestEvaluate_ZeroNVMeSpareAlerts(t *testing.T) {
	cfg := defaults()
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "nvme0n1", SMARTEnabled: true, SMARTHealthy: true,
		AvailableSparePercent: 0, SpareThresholdPercent: 10,
	}}}

	a := find(alert.New().Evaluate(cfg, snap, time.Now()), alert.RuleNVMeSpareLow)
	if a == nil {
		t.Fatal("a drive with 0% spare must fire nvme_spare_low; that is the worst possible reading")
	}
	if a.Severity != types.SeverityCritical {
		t.Errorf("severity = %q, want critical", a.Severity)
	}
}

// TestEvaluate_HealthyNVMeSpareIsSilent guards the other direction so the fix
// does not turn every healthy NVMe into an alert.
func TestEvaluate_HealthyNVMeSpareIsSilent(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "nvme0n1", SMARTEnabled: true, SMARTHealthy: true,
		AvailableSparePercent: 100, SpareThresholdPercent: 10, WearLevelPercent: 4,
	}}}
	if got := alert.New().Evaluate(defaults(), snap, time.Now()); len(got) != 0 {
		t.Errorf("a healthy NVMe produced %+v, want no alerts", got)
	}
}

// TestEvaluate_SATADriveHasNoEnduranceAlerts confirms the endurance rules are
// scoped to NVMe. A SATA disk reports zero in these fields because they do not
// apply, not because it has run out of spare blocks.
func TestEvaluate_SATADriveHasNoEnduranceAlerts(t *testing.T) {
	snap := &types.Snapshot{Disks: []types.DiskInfo{{
		Name: "sda", SMARTEnabled: true, SMARTHealthy: true,
		AvailableSparePercent: 0, WearLevelPercent: 0,
	}}}
	got := alert.New().Evaluate(defaults(), snap, time.Now())
	if a := find(got, alert.RuleNVMeSpareLow); a != nil {
		t.Errorf("a SATA disk must not fire an NVMe endurance rule: %+v", a)
	}
}

// TestEvaluate_ContainersWithTheSameNameStaySeparate is measured behaviour: a
// real host was running two different containers both called "sleep". Keying
// the alert on the name collided their ids, so the two conditions shared one
// first-seen timestamp and were indistinguishable in the UI.
func TestEvaluate_ContainersWithTheSameNameStaySeparate(t *testing.T) {
	cfg := defaults()
	cfg.ContainerCPUPct = 10
	snap := &types.Snapshot{
		CPUSummary: types.CPUSummary{TotalThreads: 4},
		Virt: types.VirtInfo{Containers: []types.ContainerInfo{
			{Name: "sleep", ShortID: "2e69cd5f4f72", CPUPercent: 200},
			{Name: "sleep", ShortID: "99045576c4ca", CPUPercent: 300},
		}},
	}
	got := alert.New().Evaluate(cfg, snap, time.Now())

	ids := map[string]bool{}
	for _, a := range got {
		if a.Rule == alert.RuleContainerCPU {
			ids[a.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Errorf("two distinct containers produced %d unique alert ids (%v); each needs its own identity", len(ids), ids)
	}
}

// TestEvaluate_ContainerCPUIsRelativeToTheHost guards the semantics fix.
// CPUPercent sums across cores, so a container reading 504% on a 24-thread host
// is using about a fifth of the machine, not five times it.
func TestEvaluate_ContainerCPUIsRelativeToTheHost(t *testing.T) {
	cfg := defaults()
	cfg.ContainerCPUPct = 90

	snap := &types.Snapshot{
		CPUSummary: types.CPUSummary{TotalThreads: 24},
		Virt: types.VirtInfo{Containers: []types.ContainerInfo{
			{Name: "busy", ShortID: "aaaaaaaaaaaa", CPUPercent: 504},
		}},
	}
	if a := find(alert.New().Evaluate(cfg, snap, time.Now()), alert.RuleContainerCPU); a != nil {
		t.Errorf("504%% across 24 threads is ~21%% of the host and must not alert at a 90%% threshold: %+v", a)
	}

	// Now genuinely saturating the machine.
	snap.Virt.Containers[0].CPUPercent = 2300
	a := find(alert.New().Evaluate(cfg, snap, time.Now()), alert.RuleContainerCPU)
	if a == nil {
		t.Fatal("a container using nearly the whole host should alert")
	}
	if a.Value < 90 {
		t.Errorf("reported value = %.0f, want the host share (~96)", a.Value)
	}
}

// TestEvaluate_ReusedInterfaceNameStartsAFreshBaseline covers the lifecycle of
// veth and tap devices, which a host running containers or VMs creates and
// destroys constantly and which reuse names freely.
//
// Retaining a vanished interface's counters makes its successor look like the
// same continuous series, so that interface's entire lifetime of errors gets
// divided by one snapshot interval and reported as a burst that never happened.
func TestEvaluate_ReusedInterfaceNameStartsAFreshBaseline(t *testing.T) {
	cfg := defaults()
	cfg.IfaceErrRate = 1
	e := alert.New()
	t0 := time.Now()

	withVeth := func(errs uint64) *types.Snapshot {
		return &types.Snapshot{Networks: []types.NetworkInfo{
			{Name: "eno1", IsUp: true, Addresses: []string{"10.0.0.5/24"}},
			{Name: "veth0", IsUp: true, ErrorsIn: errs},
		}}
	}
	withoutVeth := &types.Snapshot{Networks: []types.NetworkInfo{
		{Name: "eno1", IsUp: true, Addresses: []string{"10.0.0.5/24"}},
	}}

	// The interface exists and accumulates a long, quiet history.
	e.Evaluate(cfg, withVeth(0), t0)
	e.Evaluate(cfg, withVeth(5), t0.Add(10*time.Second))

	// It goes away.
	e.Evaluate(cfg, withoutVeth, t0.Add(20*time.Second))

	// A different interface reuses the name, carrying its own accumulated
	// counters. This must establish a baseline, not report the difference.
	got := e.Evaluate(cfg, withVeth(100_000), t0.Add(30*time.Second))
	if a := find(got, alert.RuleIfaceErrors); a != nil {
		t.Errorf("a reused interface name reported %+v; its first sighting must only set a baseline", a)
	}

	// And from that baseline onward the rate is measured normally.
	got = e.Evaluate(cfg, withVeth(100_100), t0.Add(40*time.Second))
	if a := find(got, alert.RuleIfaceErrors); a == nil {
		t.Error("100 errors in 10s should alert once a baseline exists")
	}
}

// TestEvaluate_VanishedInterfacesAreForgotten pins the bounded-state half: an
// interface that is gone must not be carried forward indefinitely.
func TestEvaluate_VanishedInterfacesAreForgotten(t *testing.T) {
	cfg := defaults()
	e := alert.New()
	t0 := time.Now()

	many := &types.Snapshot{Networks: make([]types.NetworkInfo, 0, 50)}
	for i := range 50 {
		many.Networks = append(many.Networks, types.NetworkInfo{
			Name: fmt.Sprintf("veth%d", i), IsUp: true,
		})
	}
	e.Evaluate(cfg, many, t0)

	// All of them disappear at once, as they do when a container host drains.
	only := &types.Snapshot{Networks: []types.NetworkInfo{{Name: "eno1", IsUp: true}}}
	e.Evaluate(cfg, only, t0.Add(time.Second))

	// Nothing observable should remain of the 50: bringing one back is a first
	// sighting again.
	back := &types.Snapshot{Networks: []types.NetworkInfo{
		{Name: "eno1", IsUp: true},
		{Name: "veth7", IsUp: true, ErrorsIn: 1_000_000},
	}}
	if a := find(e.Evaluate(cfg, back, t0.Add(2*time.Second)), alert.RuleIfaceErrors); a != nil {
		t.Errorf("a returning interface should be a fresh baseline, got %+v", a)
	}
}

// A failing drive behind a RAID controller must raise the same alert a failing
// block device raises.
//
// This is the half of #45 that makes reading those drives worth doing. Without
// it the six drives behind ci-node-1's PERC appear on the dashboard and raise
// nothing, so a failure is visible only to somebody already looking at the
// page -- which is not what the owner asked for when he asked to see them.
func TestEvaluate_FailingControllerMemberRaisesSMARTAlert(t *testing.T) {
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{
			Name: "sdb",
			// The VOLUME reports no SMART of its own, which is the whole
			// point: if the alert were driven by the disk's own fields there
			// would be nothing to fire on.
			SMARTEnabled: false,
			Members: []types.DiskMember{
				{DeviceID: 6, Serial: "L0J4VK8J", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
					Enclosure: 64, Slot: 6, Online: true, SMARTEnabled: true, SMARTHealthy: true},
				{DeviceID: 7, Serial: "L0J4Y3AJ", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
					Enclosure: 64, Slot: 7, Online: true, SMARTEnabled: true, SMARTHealthy: false},
				// A drive we could not read. Its zero value is indistinguishable
				// from "failing" if the SMARTEnabled guard is dropped, and
				// alerting critical on a drive that told us nothing is the
				// same substitution of an absence for an answer this whole
				// change exists to remove -- just pointing the other way.
				{DeviceID: 8, Serial: "L0J4HNUJ", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
					Enclosure: 64, Slot: 8, Online: true, SMARTEnabled: false, SMARTHealthy: false,
					SMARTStatus: types.SMARTUnreadable},
			},
		}},
	}

	alerts := alert.New().Evaluate(defaults(), snap, time.Now())

	var found *types.Alert
	for i := range alerts {
		if alerts[i].Rule == alert.RuleSMARTUnhealthy {
			found = &alerts[i]
		}
	}
	if found == nil {
		t.Fatalf("no smart_unhealthy alert for a failing member; got %d alerts", len(alerts))
	}
	if found.Severity != types.SeverityCritical {
		t.Errorf("severity = %q, want critical", found.Severity)
	}
	// The rule name matters as much as the alert: anyone who has configured
	// notification on smart_unhealthy must hear this one too. A separate rule
	// would be the same silence by another route.
	if found.Rule != alert.RuleSMARTUnhealthy {
		t.Errorf("rule = %q, want %q", found.Rule, alert.RuleSMARTUnhealthy)
	}
	// The message has to name the drive the way somebody at the rack needs it:
	// there is no /dev path to print.
	for _, want := range []string{"bay 64:7", "L0J4Y3AJ", "sdb", "DKR5E-J1R2SS"} {
		if !strings.Contains(found.Message, want) {
			t.Errorf("message %q does not contain %q", found.Message, want)
		}
	}
	// And the healthy member must not fire. A rule that alerted on every
	// member would pass every assertion above.
	if strings.Contains(found.Message, "L0J4VK8J") {
		t.Errorf("the healthy member was named in the alert: %q", found.Message)
	}
	// Nor the one that never answered.
	for _, a := range alerts {
		if a.Rule == alert.RuleSMARTUnhealthy && strings.Contains(a.Message, "L0J4HNUJ") {
			t.Errorf("a drive that reported nothing raised a failure alert: %q", a.Message)
		}
	}
	if n := countRule(alerts, alert.RuleSMARTUnhealthy); n != 1 {
		t.Errorf("%d smart_unhealthy alerts, want 1", n)
	}
}

// TestEvaluate_MemberAlertIDSurvivesAMissingSerial pins the de-duplication key,
// and pins it to the position rather than to the serial.
//
// The serial looks like the better identity and is the wrong one here. It is
// read from the drive with a separate command every cycle, and that command
// fails on exactly the drive this alert is about -- one on its way out. Keying
// on it would have the ID flip between the serial and the position as the
// command succeeded and failed, so an alert firing for an hour would clear and
// re-fire as a new fault on a different drive.
func TestEvaluate_MemberAlertIDSurvivesAMissingSerial(t *testing.T) {
	member := types.DiskMember{
		DeviceID: 7, Serial: "L0J4Y3AJ", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
		Enclosure: 64, Slot: 7, Online: true, SMARTEnabled: true, SMARTHealthy: false,
	}
	idFor := func(m types.DiskMember) string {
		snap := &types.Snapshot{Disks: []types.DiskInfo{{Name: "sdb", Members: []types.DiskMember{m}}}}
		for _, a := range alert.New().Evaluate(defaults(), snap, time.Now()) {
			if a.Rule == alert.RuleSMARTUnhealthy {
				return a.ID
			}
		}
		return ""
	}

	// The cycle where the identity command failed. Same drive, same bay, no
	// serial -- and it must be the same alert.
	silent := member
	silent.Serial = ""
	if a, b := idFor(member), idFor(silent); a == "" || a != b {
		t.Errorf("alert ID changed when the serial could not be read: %q -> %q", a, b)
	}

	// And two drives in different bays are two alerts, so the position is
	// carrying real identity rather than collapsing them.
	other := member
	other.Slot, other.DeviceID = 9, 9
	if a, b := idFor(member), idFor(other); a == b {
		t.Errorf("two drives in different bays shared one alert ID: %q", a)
	}
}

// TestEvaluate_OfflineMemberIsNamed covers the drive the controller has taken
// out of service.
//
// raid_degraded does not cover it. The array state and the member state are
// separate fields, so a volume reported optimal with an offline drive is a
// contradiction that would otherwise be accepted in silence -- and even when
// the array does report degraded, that alert cannot say WHICH drive, which is
// the actionable half.
func TestEvaluate_OfflineMemberIsNamed(t *testing.T) {
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{
			Name: "sdb",
			// Deliberately NOT degraded: this is the contradiction case.
			RAID: &types.RAIDInfo{Level: "raid10", State: "active", Degraded: false,
				TotalDevices: 2, ActiveDevices: 1},
			Members: []types.DiskMember{
				{DeviceID: 6, Serial: "L0J4VK8J", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
					Enclosure: 64, Slot: 6, Online: true, SMARTEnabled: true, SMARTHealthy: true},
				{DeviceID: 7, Serial: "L0J4Y3AJ", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
					Enclosure: 64, Slot: 7, Online: false, SMARTEnabled: true, SMARTHealthy: true},
			},
		}},
	}

	alerts := alert.New().Evaluate(defaults(), snap, time.Now())
	if n := countRule(alerts, alert.RuleRAIDMemberOffline); n != 1 {
		t.Fatalf("%d raid_member_offline alerts, want 1", n)
	}
	for _, a := range alerts {
		if a.Rule != alert.RuleRAIDMemberOffline {
			continue
		}
		if a.Severity != types.SeverityCritical {
			t.Errorf("severity = %q, want critical", a.Severity)
		}
		// It has to name the bay, because that is what somebody at the rack
		// needs and the array-level alert cannot say it.
		if !strings.Contains(a.Message, "bay 64:7") {
			t.Errorf("message %q does not name the bay", a.Message)
		}
		if strings.Contains(a.Message, "L0J4VK8J") {
			t.Errorf("the online drive was named: %q", a.Message)
		}
	}

	// A drive that is offline but healthy must not also raise smart_unhealthy:
	// they are different statements and only one of them is true.
	if n := countRule(alerts, alert.RuleSMARTUnhealthy); n != 0 {
		t.Errorf("%d smart_unhealthy alerts for a healthy offline drive, want 0", n)
	}
}

// TestEvaluate_HotControllerMemberRaisesTemperature covers the other reading we
// take from these drives. On a host whose only spinning disks sit behind a
// controller, these are the only drive temperatures there are.
func TestEvaluate_HotControllerMemberRaisesTemperature(t *testing.T) {
	cfg := defaults()
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{
			Name: "sdb",
			Members: []types.DiskMember{
				{DeviceID: 6, Serial: "COOL", Enclosure: 64, Slot: 6, Temperature: 33,
					SMARTEnabled: true, SMARTHealthy: true},
				{DeviceID: 7, Serial: "HOT", Enclosure: 64, Slot: 7, Temperature: 200,
					SMARTEnabled: true, SMARTHealthy: true},
			},
		}},
	}

	alerts := alert.New().Evaluate(cfg, snap, time.Now())
	if n := countRule(alerts, alert.RuleDiskTemp); n != 1 {
		t.Fatalf("%d disk_temp alerts, want 1 (only the hot drive)", n)
	}
	for _, a := range alerts {
		if a.Rule == alert.RuleDiskTemp && !strings.Contains(a.Message, "HOT") {
			t.Errorf("the temperature alert named the wrong drive: %q", a.Message)
		}
	}
}

// countRule returns how many alerts fired for one rule.
func countRule(alerts []types.Alert, rule string) int {
	n := 0
	for _, a := range alerts {
		if a.Rule == rule {
			n++
		}
	}
	return n
}

// A threshold override written for one drive behind a controller must reach
// that drive, and an override written for the volume must not silently govern
// every drive under it.
//
// The alert's resource is "sdb/64:7" -- chosen so the identity does not flap
// when a serial cannot be read -- but its thresholds were being looked up
// under the volume's name. So the resource the alert IS and the resource its
// configuration is READ UNDER diverged, which makes a stated intention
// unreachable and applies one the user did not write.
//
// It matters most for exactly this case: drives behind a controller run warmer
// than block devices in the same chassis, so a per-member override is the
// first thing somebody would reach for.
func TestEvaluate_MemberTemperatureUsesTheMembersOwnOverride(t *testing.T) {
	member := types.DiskMember{
		DeviceID: 7, Serial: "L0J4Y3AJ", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
		Enclosure: 64, Slot: 7, Online: true, SMARTEnabled: true, SMARTHealthy: true,
		Temperature: 70,
	}
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{Name: "sdb", Members: []types.DiskMember{member}}},
	}

	// Without an override the drive is over the default warning threshold.
	if n := countRule(alert.New().Evaluate(defaults(), snap, time.Now()), alert.RuleDiskTemp); n != 1 {
		t.Fatalf("%d disk_temp alerts at 70°C with default thresholds, want 1", n)
	}

	// An override written for the MEMBER must reach it.
	raised := defaults()
	raised.Overrides = []config.Override{{Resource: "sdb/64:7", TempWarnC: 100, TempCritC: 110}}
	if n := countRule(alert.New().Evaluate(raised, snap, time.Now()), alert.RuleDiskTemp); n != 0 {
		t.Errorf("%d disk_temp alerts with a member override of 100°C, want 0: the override was unreachable", n)
	}

	// An override written for the VOLUME must not govern the member. The
	// volume and the member are different resources and a user who wrote one
	// did not write the other.
	volumeOnly := defaults()
	volumeOnly.Overrides = []config.Override{{Resource: "sdb", TempWarnC: 100, TempCritC: 110}}
	if n := countRule(alert.New().Evaluate(volumeOnly, snap, time.Now()), alert.RuleDiskTemp); n != 1 {
		t.Errorf("%d disk_temp alerts with an override on the volume, want 1: it silently governed the member", n)
	}

	// The SEVERITY has to come from the member's override too, not only the
	// decision to fire. Both branches read a threshold, and a fixture where
	// the warning branch always decides cannot tell whether the critical one
	// was fixed -- which is what the first version of this test did.
	severity := func(cfg config.Alerts) string {
		for _, a := range alert.New().Evaluate(cfg, snap, time.Now()) {
			if a.Rule == alert.RuleDiskTemp {
				return a.Severity
			}
		}
		return ""
	}
	if got := severity(defaults()); got != types.SeverityWarning {
		t.Fatalf("70°C on default thresholds = %q, want warning; this fixture cannot distinguish the branches", got)
	}
	lowered := defaults()
	lowered.Overrides = []config.Override{{Resource: "sdb/64:7", TempWarnC: 40, TempCritC: 50}}
	if got := severity(lowered); got != types.SeverityCritical {
		t.Errorf("70°C with a member critical threshold of 50 = %q, want critical: the critical branch ignored the override", got)
	}
}

// The metric label and the alert key for one drive are DIFFERENT on purpose,
// and this pins it so unifying them fails rather than passing review.
//
// Two identifiers for one thing is normally the defect this repository keeps
// removing. Here it is correct, because the two answer different questions and
// fail in opposite directions: a metric is long-lived history where a re-bay
// must not continue one drive's series under another's readings, so it travels
// with the drive as the SERIAL. An alert is de-duplication within a run where
// flapping is the failure, and the serial is read from the drive every cycle
// and goes missing on exactly the drive an alert is about — so it uses the
// POSITION, which comes from the controller's configuration and cannot vanish
// while the member exists.
//
// Both sides carry a comment saying so. A comment does not fail; this does.
func TestMemberIdentity_MetricUsesSerialAndAlertUsesPosition(t *testing.T) {
	member := types.DiskMember{
		DeviceID: 7, Serial: "L0J4Y3AJ", Vendor: "HITACHI", Model: "DKR5E-J1R2SS",
		Enclosure: 64, Slot: 7, Online: true,
		Temperature: 200, SMARTEnabled: true, SMARTHealthy: false,
	}
	snap := &types.Snapshot{
		Disks: []types.DiskInfo{{Name: "sdb", Members: []types.DiskMember{member}}},
	}

	var alertID string
	for _, a := range alert.New().Evaluate(defaults(), snap, time.Now()) {
		if a.Rule == alert.RuleSMARTUnhealthy {
			alertID = a.ID
		}
	}
	if alertID == "" {
		t.Fatal("no smart_unhealthy alert for a failing member")
	}

	w := &metrics.TextWriter{}
	metrics.Write(w, snap)
	series := w.String()

	// The alert is keyed by position and must NOT contain the serial: a serial
	// in the ID is the flapping identity this choice exists to avoid.
	if !strings.Contains(alertID, "64:7") {
		t.Errorf("alert ID %q does not carry the drive's position", alertID)
	}
	if strings.Contains(alertID, member.Serial) {
		t.Errorf("alert ID %q carries the serial; it would change whenever that read fails", alertID)
	}

	// The metric is labelled by serial and must NOT be labelled by position: a
	// position in the label is a series that silently continues under a
	// different drive after a re-bay.
	if !strings.Contains(series, `serial="L0J4Y3AJ"`) {
		t.Error("the member metric is not labelled by serial")
	}
	if strings.Contains(series, `serial="64:7"`) || strings.Contains(series, `slot=`) {
		t.Error("the member metric is labelled by position; a re-bay would continue another drive's series")
	}
}

// ---- smart_unreadable (#57) -------------------------------------------------

// memberSnapshot wraps members into the shape the evaluator reads.
func memberSnapshot(members ...types.DiskMember) *types.Snapshot {
	return &types.Snapshot{Disks: []types.DiskInfo{{Name: "sdb", Members: members}}}
}

// fires reports whether rule appears in the alerts.
func fires(alerts []types.Alert, rule string) bool {
	for _, a := range alerts {
		if a.Rule == rule {
			return true
		}
	}
	return false
}

// TestEvaluate_SMARTUnreadableOnlyAfterADriveHasAnswered is the rule's entire
// design in one test: a drive that has NEVER answered is a capability fact, a
// drive that STOPS is an event, and only the second is worth telling anyone
// about.
//
// The two cases run against the same evaluator in sequence, because the
// difference between them IS the sequence. A test that checked them
// independently would pass against an implementation with no memory at all.
func TestEvaluate_SMARTUnreadableOnlyAfterADriveHasAnswered(t *testing.T) {
	silent := types.DiskMember{
		DeviceID: 7, Enclosure: 64, Slot: 7, Online: true,
		Serial: "L0J4Y3AJ", Vendor: "HITACHI",
		SMARTEnabled: false,
	}
	answering := silent
	answering.SMARTEnabled, answering.SMARTHealthy = true, true

	e := alert.New()
	now := time.Now()

	// A drive that has never answered: silence, however many cycles pass.
	// Firing here would put a permanent warning on every host whose drives do
	// not do SMART behind their controller.
	for i := range 3 {
		if fires(e.Evaluate(defaults(), memberSnapshot(silent), now.Add(time.Duration(i)*time.Second)), alert.RuleSMARTUnreadable) {
			t.Fatalf("cycle %d: warned about a drive that has never answered", i)
		}
	}

	// It answers. Still nothing to say.
	if fires(e.Evaluate(defaults(), memberSnapshot(answering), now.Add(4*time.Second)), alert.RuleSMARTUnreadable) {
		t.Fatal("warned about a drive that is answering")
	}

	// And now it stops. This is the event.
	alerts := e.Evaluate(defaults(), memberSnapshot(silent), now.Add(5*time.Second))
	if !fires(alerts, alert.RuleSMARTUnreadable) {
		t.Fatal("a drive that answered and then stopped raised nothing")
	}
	for _, a := range alerts {
		if a.Rule != alert.RuleSMARTUnreadable {
			continue
		}
		// Warning, not critical: an absence is not a failure.
		if a.Severity != types.SeverityWarning {
			t.Errorf("severity = %q, want warning: an absent reading is not a failing drive", a.Severity)
		}
		// Keyed by position, like every other member alert -- and for the
		// reason this rule exists: the serial is read with a separate command
		// that fails on exactly the drive this fires for.
		if a.Resource != "sdb/64:7" {
			t.Errorf("resource = %q, want sdb/64:7", a.Resource)
		}
	}

	// Recovery clears it, because the rule only fires while the reading is
	// absent. A stuck warning on a drive that came back would be its own
	// false signal.
	if fires(e.Evaluate(defaults(), memberSnapshot(answering), now.Add(6*time.Second)), alert.RuleSMARTUnreadable) {
		t.Error("the warning survived the drive answering again")
	}
}

// A fresh evaluator has no history, so a drive that was already silent before a
// restart is indistinguishable from one that never answered -- and is treated
// as the latter.
//
// That is the honest reading rather than a limitation to work around:
// "has answered in this run" is the only claim a process can make about what it
// observed, and persisting it would mean alerting on a fact nobody in this
// process saw.
func TestEvaluate_SMARTUnreadableStateResetsWithTheProcess(t *testing.T) {
	silent := types.DiskMember{
		DeviceID: 7, Enclosure: 64, Slot: 7, Online: true, SMARTEnabled: false,
	}
	answering := silent
	answering.SMARTEnabled, answering.SMARTHealthy = true, true

	before := alert.New()
	before.Evaluate(defaults(), memberSnapshot(answering), time.Now())
	if !fires(before.Evaluate(defaults(), memberSnapshot(silent), time.Now()), alert.RuleSMARTUnreadable) {
		t.Fatal("the pre-restart evaluator did not warn, so the comparison below means nothing")
	}

	// The same drive, the same silence, a new process.
	after := alert.New()
	if fires(after.Evaluate(defaults(), memberSnapshot(silent), time.Now()), alert.RuleSMARTUnreadable) {
		t.Error("warned after a restart about a drive this process never saw answer")
	}
}

// Two drives in the same volume are tracked separately: one going quiet must
// not implicate the other, and one that has answered must not vouch for a
// neighbour that has not.
func TestEvaluate_SMARTUnreadableIsPerDrive(t *testing.T) {
	answering := types.DiskMember{DeviceID: 7, Enclosure: 64, Slot: 7, Online: true, SMARTEnabled: true, SMARTHealthy: true}
	neverAnswered := types.DiskMember{DeviceID: 9, Enclosure: 64, Slot: 9, Online: true, SMARTEnabled: false}

	e := alert.New()
	now := time.Now()
	e.Evaluate(defaults(), memberSnapshot(answering, neverAnswered), now)

	// The first stops; the second has still never answered.
	stopped := answering
	stopped.SMARTEnabled, stopped.SMARTHealthy = false, false
	alerts := e.Evaluate(defaults(), memberSnapshot(stopped, neverAnswered), now.Add(time.Second))

	var resources []string
	for _, a := range alerts {
		if a.Rule == alert.RuleSMARTUnreadable {
			resources = append(resources, a.Resource)
		}
	}
	if len(resources) != 1 || resources[0] != "sdb/64:7" {
		t.Errorf("smart_unreadable fired for %v, want exactly [sdb/64:7]: the drive in bay 9 "+
			"has never answered and is a capability fact, not an event", resources)
	}
}
