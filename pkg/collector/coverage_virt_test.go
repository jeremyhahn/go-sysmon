package collector

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func withProcRoot(t *testing.T, root string) {
	t.Helper()
	orig := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = orig })
}

// fakeVMProcess writes the /proc entries a libvirt hypervisor process would
// have, and the matching machine scope under the cgroup root.
func fakeVMProcess(t *testing.T, procDir, cgDir string, pid int32, scope string) string {
	t.Helper()

	pidDir := filepath.Join(procDir, strconv.Itoa(int(pid)))
	writeSysfs(t, filepath.Join(pidDir, "comm"), "qemu-system-x86")
	writeSysfs(t, filepath.Join(pidDir, "cgroup"), "0::"+scope+"/libvirt/emulator")

	full := filepath.Join(cgDir, scope)
	writeSysfs(t, filepath.Join(full, "cgroup.procs"), strconv.Itoa(int(pid)))
	return full
}

const testVMScope = `/machine.slice/machine-qemu\x2d3\x2dwin11.scope`

// TestVMCgroupPath_FindsMachineScope verifies the libvirt machine scope is
// resolved from the hypervisor's cgroup line. This is the unprivileged route
// to a guest's I/O counters, so losing it silently zeroes VM disk metrics.
func TestVMCgroupPath_FindsMachineScope(t *testing.T) {
	procDir, cgDir := t.TempDir(), t.TempDir()
	withProcRoot(t, procDir)
	withCgroupRoot(t, cgDir)

	want := fakeVMProcess(t, procDir, cgDir, 4242, testVMScope)

	if got := vmCgroupPath(4242); got != want {
		t.Errorf("vmCgroupPath() = %q, want %q", got, want)
	}
}

// TestVMCgroupPath_UnknownProcess verifies a PID with no /proc entry and a
// scope with no cgroup directory both yield an empty path rather than one that
// does not exist.
func TestVMCgroupPath_UnknownProcess(t *testing.T) {
	procDir, cgDir := t.TempDir(), t.TempDir()
	withProcRoot(t, procDir)
	withCgroupRoot(t, cgDir)

	if got := vmCgroupPath(9999); got != "" {
		t.Errorf("vmCgroupPath() for a missing process = %q, want empty", got)
	}

	// A process whose cgroup line names a scope that is not mounted.
	pidDir := filepath.Join(procDir, "77")
	writeSysfs(t, filepath.Join(pidDir, "cgroup"), `0::/machine.slice/machine-qemu\x2d9\x2dghost.scope/libvirt/emulator`)
	if got := vmCgroupPath(77); got != "" {
		t.Errorf("vmCgroupPath() for an unmounted scope = %q, want empty", got)
	}

	// A cgroup v1 style line, which has no scope at all.
	otherDir := filepath.Join(procDir, "78")
	writeSysfs(t, filepath.Join(otherDir, "cgroup"), "9:devices:/user.slice")
	if got := vmCgroupPath(78); got != "" {
		t.Errorf("vmCgroupPath() for a v1 cgroup line = %q, want empty", got)
	}
}

// TestApplyVMCgroupMetrics_ComputesDiskRates verifies guest I/O is reported as
// a per-second rate against the previous sample, not as a cumulative total.
func TestApplyVMCgroupMetrics_ComputesDiskRates(t *testing.T) {
	procDir, cgDir := t.TempDir(), t.TempDir()
	withProcRoot(t, procDir)
	withCgroupRoot(t, cgDir)

	const pid int32 = 4242
	scopeDir := fakeVMProcess(t, procDir, cgDir, pid, testVMScope)
	writeSysfs(t, filepath.Join(scopeDir, "io.stat"),
		"8:0 rbytes=3000000 wbytes=1000000 rios=300 wios=100")
	writeSysfs(t, filepath.Join(scopeDir, "memory.current"), "8589934592")
	writeSysfs(t, filepath.Join(scopeDir, "memory.peak"), "9000000000")
	writeSysfs(t, filepath.Join(scopeDir, "cpu.pressure"),
		"some avg10=1.50 avg60=0.80 avg300=0.20 total=123456")
	writeSysfs(t, filepath.Join(scopeDir, "cpu.stat"), "usage_usec 5000000")

	c := NewVirtCollector(quietLogger())
	now := time.Now()
	key := "vm:" + strconv.Itoa(int(pid))
	// A sample from two seconds ago, one megabyte behind on each counter.
	c.prevVM[key] = virtIOSnapshot{
		ReadBytes:  1_000_000,
		WriteBytes: 500_000,
		At:         now.Add(-2 * time.Second),
	}

	vm := types.VMInfo{PID: pid}
	next := make(map[string]virtIOSnapshot)
	c.applyVMCgroupMetrics(&vm, now, next)

	if vm.CgroupPath != scopeDir {
		t.Errorf("CgroupPath = %q, want %q", vm.CgroupPath, scopeDir)
	}
	if vm.MemoryCurrentBytes != 8589934592 || vm.MemoryPeakBytes != 9000000000 {
		t.Errorf("memory = %d current / %d peak, want 8589934592 / 9000000000",
			vm.MemoryCurrentBytes, vm.MemoryPeakBytes)
	}
	if vm.DiskReadBytes != 3_000_000 || vm.DiskWriteBytes != 1_000_000 {
		t.Errorf("totals = %d read / %d written, want 3000000 / 1000000",
			vm.DiskReadBytes, vm.DiskWriteBytes)
	}
	// (3000000 - 1000000) / 2s = 1 MB/s; (1000000 - 500000) / 2s = 250 kB/s.
	if vm.DiskReadRate != 1_000_000 {
		t.Errorf("DiskReadRate = %d, want 1000000", vm.DiskReadRate)
	}
	if vm.DiskWriteRate != 250_000 {
		t.Errorf("DiskWriteRate = %d, want 250000", vm.DiskWriteRate)
	}
	if vm.CPUPressure != 1.50 {
		t.Errorf("CPUPressure = %v, want 1.50", vm.CPUPressure)
	}
	if _, ok := next[key]; !ok {
		t.Error("no snapshot was recorded for the next cycle")
	}
}

// TestApplyVMCgroupMetrics_FirstSampleHasNoRate verifies a guest seen for the
// first time reports no rate. Reporting the cumulative total as a rate would
// show a fresh VM doing gigabytes per second.
func TestApplyVMCgroupMetrics_FirstSampleHasNoRate(t *testing.T) {
	procDir, cgDir := t.TempDir(), t.TempDir()
	withProcRoot(t, procDir)
	withCgroupRoot(t, cgDir)

	const pid int32 = 4243
	scopeDir := fakeVMProcess(t, procDir, cgDir, pid, testVMScope)
	writeSysfs(t, filepath.Join(scopeDir, "io.stat"), "8:0 rbytes=9000000000 wbytes=800000000")

	c := NewVirtCollector(quietLogger())
	vm := types.VMInfo{PID: pid}
	c.applyVMCgroupMetrics(&vm, time.Now(), make(map[string]virtIOSnapshot))

	if vm.DiskReadRate != 0 || vm.DiskWriteRate != 0 {
		t.Errorf("rates = %d / %d on the first sample, want 0 / 0",
			vm.DiskReadRate, vm.DiskWriteRate)
	}
}

// TestApplyVMCgroupMetrics_NoCgroup verifies a guest whose cgroup cannot be
// found is left untouched rather than partially filled.
func TestApplyVMCgroupMetrics_NoCgroup(t *testing.T) {
	withProcRoot(t, t.TempDir())
	withCgroupRoot(t, t.TempDir())

	c := NewVirtCollector(quietLogger())
	vm := types.VMInfo{PID: 1234}
	next := make(map[string]virtIOSnapshot)
	c.applyVMCgroupMetrics(&vm, time.Now(), next)

	if vm.CgroupPath != "" || len(next) != 0 {
		t.Errorf("CgroupPath = %q with %d snapshots, want empty and 0", vm.CgroupPath, len(next))
	}
}

// TestVMCPUPercent_DeltaOverWindow verifies guest CPU is a rate over the
// sampling window. gopsutil's lifetime average understates a busy guest by an
// order of magnitude, which is why this is computed from deltas.
func TestVMCPUPercent_DeltaOverWindow(t *testing.T) {
	c := NewVirtCollector(quietLogger())
	now := time.Now()
	const pid int32 = 7000
	key := "vmcpu:" + strconv.Itoa(int(pid))

	// 100 CPU-seconds consumed, one second ago.
	c.prevVM[key] = virtIOSnapshot{CPUUsageUsec: 100 * 1e6, At: now.Add(-time.Second)}

	next := make(map[string]virtIOSnapshot)
	// 4 more CPU-seconds in one wall-clock second: four cores busy.
	if got := c.vmCPUPercent(pid, 104, now, next); got != 400 {
		t.Errorf("vmCPUPercent() = %v, want 400 (four cores busy)", got)
	}
	if _, ok := next[key]; !ok {
		t.Error("no CPU snapshot was recorded for the next cycle")
	}
}

// TestVMCPUPercent_UnusableBaselines verifies the cases that would otherwise
// produce a negative or infinite percentage.
func TestVMCPUPercent_UnusableBaselines(t *testing.T) {
	now := time.Now()
	const pid int32 = 7001
	key := "vmcpu:" + strconv.Itoa(int(pid))

	tests := []struct {
		name  string
		prev  *virtIOSnapshot
		total float64
	}{
		{"no previous sample", nil, 50},
		{
			"counter went backwards after a restart",
			&virtIOSnapshot{CPUUsageUsec: 500 * 1e6, At: now.Add(-time.Second)},
			10,
		},
		{
			"no time elapsed",
			&virtIOSnapshot{CPUUsageUsec: 1e6, At: now},
			50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewVirtCollector(quietLogger())
			if tt.prev != nil {
				c.prevVM[key] = *tt.prev
			}
			got := c.vmCPUPercent(pid, tt.total, now, make(map[string]virtIOSnapshot))
			if got != 0 {
				t.Errorf("vmCPUPercent() = %v, want 0", got)
			}
		})
	}
}

// TestAppendUnique_SkipsDuplicates verifies a value already present is not
// added again, which keeps a guest's NIC list free of repeats when several
// MACs resolve to the same tap device.
func TestAppendUnique_SkipsDuplicates(t *testing.T) {
	list := appendUnique(nil, "vnet0")
	list = appendUnique(list, "vnet1")
	list = appendUnique(list, "vnet0")

	if len(list) != 2 || list[0] != "vnet0" || list[1] != "vnet1" {
		t.Errorf("got %v, want [vnet0 vnet1]", list)
	}
}

// TestProcessIdentity_MissingProcess verifies a process that has exited yields
// empty identity fields rather than a panic.
func TestProcessIdentity_MissingProcess(t *testing.T) {
	withProcRoot(t, t.TempDir())

	name, cmdline := processIdentity(4242)
	if name != "" || cmdline != "" {
		t.Errorf("got name=%q cmdline=%q, want both empty", name, cmdline)
	}
}
