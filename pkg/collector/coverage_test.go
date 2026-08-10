package collector

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- CPU topology from sysfs ----------------------------------------------

// buildCPUTree writes a synthetic /sys/devices/system/cpu tree.
func buildCPUTree(t *testing.T, root string, layout map[string][2]int) {
	t.Helper()
	for cpu, ids := range layout {
		topo := filepath.Join(root, cpu, "topology")
		writeSysfsFile(t, filepath.Join(topo, "physical_package_id"), itoaTest(ids[0])+"\n")
		writeSysfsFile(t, filepath.Join(topo, "core_id"), itoaTest(ids[1])+"\n")
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func withCPUBase(t *testing.T, root string) {
	t.Helper()
	orig := cpuSysfsBase
	cpuSysfsBase = root
	t.Cleanup(func() { cpuSysfsBase = orig })
}

// TestSummaryFromSysfs_DerivesTopology covers the normal path: two sockets,
// two cores each, two threads per core.
func TestSummaryFromSysfs_DerivesTopology(t *testing.T) {
	root := t.TempDir()
	withCPUBase(t, root)

	// 8 logical CPUs: 2 sockets x 2 cores x 2 threads.
	buildCPUTree(t, root, map[string][2]int{
		"cpu0": {0, 0}, "cpu1": {0, 0},
		"cpu2": {0, 1}, "cpu3": {0, 1},
		"cpu4": {1, 0}, "cpu5": {1, 0},
		"cpu6": {1, 1}, "cpu7": {1, 1},
	})

	got, err := summaryFromSysfs()
	if err != nil {
		t.Fatalf("summaryFromSysfs() error = %v", err)
	}
	if got.Sockets != 2 {
		t.Errorf("Sockets = %d, want 2", got.Sockets)
	}
	if got.TotalCores != 4 {
		t.Errorf("TotalCores = %d, want 4", got.TotalCores)
	}
	if got.TotalThreads != 8 {
		t.Errorf("TotalThreads = %d, want 8", got.TotalThreads)
	}
	if got.CoresPerSocket != 2 {
		t.Errorf("CoresPerSocket = %d, want 2", got.CoresPerSocket)
	}
	if got.ThreadsPerCore != 2 {
		t.Errorf("ThreadsPerCore = %d, want 2", got.ThreadsPerCore)
	}
}

// TestSummaryFromSysfs_SingleCore covers a machine with no SMT, where the
// divisions must not produce a zero.
func TestSummaryFromSysfs_SingleCore(t *testing.T) {
	root := t.TempDir()
	withCPUBase(t, root)
	buildCPUTree(t, root, map[string][2]int{"cpu0": {0, 0}})

	got, err := summaryFromSysfs()
	if err != nil {
		t.Fatalf("summaryFromSysfs() error = %v", err)
	}
	if got.CoresPerSocket != 1 || got.ThreadsPerCore != 1 {
		t.Errorf("degenerate topology produced %+v", got)
	}
}

// TestSummaryFromSysfs_IgnoresNonCPUEntries verifies directories such as
// cpufreq and cpuidle are not counted as logical CPUs.
func TestSummaryFromSysfs_IgnoresNonCPUEntries(t *testing.T) {
	root := t.TempDir()
	withCPUBase(t, root)
	buildCPUTree(t, root, map[string][2]int{"cpu0": {0, 0}, "cpu1": {0, 1}})
	for _, noise := range []string{"cpufreq", "cpuidle", "power", "hotplug"} {
		if err := os.MkdirAll(filepath.Join(root, noise), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	got, err := summaryFromSysfs()
	if err != nil {
		t.Fatalf("summaryFromSysfs() error = %v", err)
	}
	if got.TotalThreads != 2 {
		t.Errorf("TotalThreads = %d, want 2; non-CPU entries were counted", got.TotalThreads)
	}
}

func TestSummaryFromSysfs_MissingTreeIsAnError(t *testing.T) {
	withCPUBase(t, filepath.Join(t.TempDir(), "absent"))

	_, err := summaryFromSysfs()
	if err == nil {
		t.Fatal("summaryFromSysfs() = nil error for a missing sysfs tree")
	}
}

// TestSummaryFromSysfs_EmptyTreeIsAnError covers a readable directory with no
// CPUs in it, which must not yield a zero-core summary.
func TestSummaryFromSysfs_EmptyTreeIsAnError(t *testing.T) {
	withCPUBase(t, t.TempDir())

	_, err := summaryFromSysfs()
	if err == nil {
		t.Fatal("summaryFromSysfs() = nil error for a tree with no CPUs")
	}
	var topoErr *types.SysfsTopologyError
	if !asError(err, &topoErr) {
		t.Errorf("error = %T, want *types.SysfsTopologyError", err)
	}
}

// TestBuildSummary_FallsBackWhenSysfsUnavailable verifies the collector still
// reports a topology when sysfs cannot be read.
func TestBuildSummary_FallsBackWhenSysfsUnavailable(t *testing.T) {
	withCPUBase(t, filepath.Join(t.TempDir(), "absent"))
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewCPUCollector(quietLogger())
	got := c.buildSummary()

	if got.TotalThreads <= 0 {
		t.Errorf("fallback produced no threads: %+v", got)
	}
}

// TestReadProcCPUInfoMHz_ParsesFallback covers the frequency fallback used
// when cpufreq is absent.
func TestReadProcCPUInfoMHz_ParsesFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	writeSysfsFile(t, path, "processor\t: 0\ncpu MHz\t\t: 3200.125\nflags\t\t: fpu\n")

	orig := procCPUInfoPath
	procCPUInfoPath = path
	t.Cleanup(func() { procCPUInfoPath = orig })

	if got := readProcCPUInfoMHz(); got != 3200.125 {
		t.Errorf("readProcCPUInfoMHz() = %v, want 3200.125", got)
	}
}

func TestReadProcCPUInfoMHz_MissingOrMalformed(t *testing.T) {
	dir := t.TempDir()
	orig := procCPUInfoPath
	t.Cleanup(func() { procCPUInfoPath = orig })

	procCPUInfoPath = filepath.Join(dir, "absent")
	if got := readProcCPUInfoMHz(); got != 0 {
		t.Errorf("missing file = %v, want 0", got)
	}

	bad := filepath.Join(dir, "bad")
	writeSysfsFile(t, bad, "cpu MHz\t\t: not-a-number\n")
	procCPUInfoPath = bad
	if got := readProcCPUInfoMHz(); got != 0 {
		t.Errorf("malformed value = %v, want 0", got)
	}
}

// ---- distribution detection -----------------------------------------------

func withOSRelease(t *testing.T, content string) {
	t.Helper()
	orig := osReleasePath
	if content == "" {
		osReleasePath = filepath.Join(t.TempDir(), "absent")
	} else {
		path := filepath.Join(t.TempDir(), "os-release")
		writeSysfsFile(t, path, content)
		osReleasePath = path
	}
	t.Cleanup(func() { osReleasePath = orig })
}

// TestDistroFamily_EveryFamily covers each packaging family, which decides
// which install command a user is told to run.
func TestDistroFamily_EveryFamily(t *testing.T) {
	tests := map[string]string{
		`ID=fedora`:             "fedora",
		`ID="rhel"`:             "fedora",
		`ID=centos`:             "fedora",
		`ID=rocky`:              "fedora",
		`ID=arch`:               "arch",
		`ID=manjaro`:            "arch",
		`ID=ubuntu`:             "debian",
		`ID=linuxmint`:          "debian",
		`NAME="Something Else"`: "debian",
	}
	for content, want := range tests {
		withOSRelease(t, content)
		if got := distroFamily(); got != want {
			t.Errorf("distroFamily(%q) = %q, want %q", content, got, want)
		}
	}
}

func TestDistroFamily_MissingFileDefaultsToDebian(t *testing.T) {
	withOSRelease(t, "")
	if got := distroFamily(); got != "debian" {
		t.Errorf("distroFamily() = %q, want debian when os-release is absent", got)
	}
}

// TestInstallHint_PerFamilyCommands verifies each family gets the right
// package manager and the right package names.
func TestInstallHint_PerFamilyCommands(t *testing.T) {
	libs := []DesktopLib{{SOName: "libgtk-3.so.0", Debian: "libgtk-3-0", Fedora: "gtk3", Arch: "gtk3"}}

	tests := []struct{ release, wantCmd, wantPkg string }{
		{"ID=fedora", "sudo dnf install", "gtk3"},
		{"ID=arch", "sudo pacman -S", "gtk3"},
		{"ID=ubuntu", "sudo apt install", "libgtk-3-0"},
	}
	for _, tt := range tests {
		withOSRelease(t, tt.release)
		got := InstallHint(libs)
		if !strings.HasPrefix(got, tt.wantCmd) {
			t.Errorf("%s: hint = %q, want it to start with %q", tt.release, got, tt.wantCmd)
		}
		if !strings.Contains(got, tt.wantPkg) {
			t.Errorf("%s: hint = %q, want package %q", tt.release, got, tt.wantPkg)
		}
	}
}

// ---- SystemCollector wiring -----------------------------------------------

// TestEnableRuntimeAPI_TogglesTheCollector verifies the opt-in reaches the
// runtime collector, which is off by default because its socket is
// root-equivalent access.
func TestEnableRuntimeAPI_TogglesTheCollector(t *testing.T) {
	s := NewSystemCollector(quietLogger())

	if s.runtime.Enabled() {
		t.Error("the runtime API must start disabled")
	}
	s.EnableRuntimeAPI(true)
	if !s.runtime.Enabled() {
		t.Error("EnableRuntimeAPI(true) did not take effect")
	}
	s.EnableRuntimeAPI(false)
	if s.runtime.Enabled() {
		t.Error("EnableRuntimeAPI(false) did not take effect")
	}
}

// TestWaitForRuntimeDiskUsage_ReturnsWhenDisabled verifies the wait cannot
// hang a one-shot command when the runtime API was never enabled.
func TestWaitForRuntimeDiskUsage_ReturnsWhenDisabled(t *testing.T) {
	s := NewSystemCollector(quietLogger())

	done := make(chan struct{})
	go func() {
		s.WaitForRuntimeDiskUsage(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("WaitForRuntimeDiskUsage hung with the collector disabled")
	}
}

// TestWaitForRuntimeDiskUsage_HonoursContext verifies a cancelled context ends
// the wait even when the collector is enabled and nothing ever answers.
func TestWaitForRuntimeDiskUsage_HonoursContext(t *testing.T) {
	s := NewSystemCollector(quietLogger())
	s.EnableRuntimeAPI(true)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.WaitForRuntimeDiskUsage(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("WaitForRuntimeDiskUsage ignored its context deadline")
	}
}

// ---- sensor to CPU merge --------------------------------------------------

// TestMergeSensorIntoCPUs_MatchesByPhysicalAndCoreID is the regression test
// for per-core temperatures landing on the wrong core: coretemp numbers its
// sensors by physical core ID, which is sparse and not the CPU index.
func TestMergeSensorIntoCPUs_MatchesByPhysicalAndCoreID(t *testing.T) {
	cpus := []types.CPUInfo{
		{Index: 0, PhysicalID: "0", CoreID: "0"},
		{Index: 1, PhysicalID: "0", CoreID: "4"},
		{Index: 2, PhysicalID: "0", CoreID: "44"},
	}
	sensors := types.SensorData{
		CoreTemps: []types.CoreTemp{
			{PackageID: 0, CoreID: 0, TempCelsius: 36},
			{PackageID: 0, CoreID: 4, TempCelsius: 32},
			{PackageID: 0, CoreID: 44, TempCelsius: 38},
		},
		CoreVoltages: []types.CoreVoltage{{Label: "Vcore", VoltageV: 1.05}},
	}

	mergeSensorIntoCPUs(cpus, sensors)

	want := map[int]float64{0: 36, 1: 32, 2: 38}
	for _, cpu := range cpus {
		if cpu.TemperatureCelsius != want[cpu.Index] {
			t.Errorf("cpu %d temperature = %v, want %v",
				cpu.Index, cpu.TemperatureCelsius, want[cpu.Index])
		}
		if cpu.VoltageV != 1.05 {
			t.Errorf("cpu %d voltage = %v, want 1.05", cpu.Index, cpu.VoltageV)
		}
	}
}

// TestMergeSensorIntoCPUs_NoSensorsLeavesCPUsAlone verifies a machine with no
// temperature sensors is left with zeroes rather than wrong values.
func TestMergeSensorIntoCPUs_NoSensorsLeavesCPUsAlone(t *testing.T) {
	cpus := []types.CPUInfo{{Index: 0, PhysicalID: "0", CoreID: "0"}}
	mergeSensorIntoCPUs(cpus, types.SensorData{})

	if cpus[0].TemperatureCelsius != 0 || cpus[0].VoltageV != 0 {
		t.Errorf("cpu was modified with no sensor data: %+v", cpus[0])
	}
}

// TestMergeSensorIntoCPUs_UnmatchedSensorIsIgnored covers a sensor whose core
// has no matching CPU, which must not panic or mis-assign.
func TestMergeSensorIntoCPUs_UnmatchedSensorIsIgnored(t *testing.T) {
	cpus := []types.CPUInfo{{Index: 0, PhysicalID: "0", CoreID: "0"}}
	sensors := types.SensorData{
		CoreTemps: []types.CoreTemp{{PackageID: 9, CoreID: 99, TempCelsius: 70}},
	}

	mergeSensorIntoCPUs(cpus, sensors)

	if cpus[0].TemperatureCelsius != 0 {
		t.Errorf("an unmatched sensor was applied: %v", cpus[0].TemperatureCelsius)
	}
}

// asError is a local errors.As wrapper keeping the imports small.
func asError(err error, target any) bool {
	switch tgt := target.(type) {
	case **types.SysfsTopologyError:
		e, ok := err.(*types.SysfsTopologyError)
		if ok {
			*tgt = e
		}
		return ok
	}
	return false
}

// ---- PCIe and DPM parsing (pure) ------------------------------------------

// TestPcieGenFromSpeed_EveryGeneration covers the link-speed table used to
// report which PCIe generation a GPU negotiated.
func TestPcieGenFromSpeed_EveryGeneration(t *testing.T) {
	tests := map[string]int{
		"2.5 GT/s PCIe":  1,
		"5.0 GT/s PCIe":  2,
		"8.0 GT/s PCIe":  3,
		"16.0 GT/s PCIe": 4,
		"32.0 GT/s PCIe": 5,
		"64.0 GT/s PCIe": 6,
		"128.0 GT/s":     0, // beyond the table
		"":               0,
		"unknown":        0,
		"2.5":            0, // missing the unit
		"abc GT/s":       0,
	}
	for in, want := range tests {
		if got := pcieGenFromSpeed(in); got != want {
			t.Errorf("pcieGenFromSpeed(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---- disk sysfs fallback ---------------------------------------------------

// TestCollectFromSysfs_BuildsDisksWithoutGHW covers the path taken when the
// ghw library cannot enumerate block devices, which is the unprivileged case.
func TestCollectFromSysfs_BuildsDisksWithoutGHW(t *testing.T) {
	root := t.TempDir()
	withSysBlockRoot(t, root)

	// Two devices, one with identity attributes and one bare.
	writeSysfsFile(t, filepath.Join(root, "sda", "size"), "1953525168\n")
	writeSysfsFile(t, filepath.Join(root, "sda", "device", "model"), "Test Disk\n")
	writeSysfsFile(t, filepath.Join(root, "sda", "device", "serial"), "SER123\n")
	writeSysfsFile(t, filepath.Join(root, "sda", "device", "vendor"), "ACME\n")
	writeSysfsFile(t, filepath.Join(root, "nvme0n1", "size"), "1000215216\n")

	c := NewDiskCollector(quietLogger())
	disks := c.collectFromSysfs(nil, nil)

	if len(disks) != 2 {
		t.Fatalf("got %d disks, want 2: %+v", len(disks), disks)
	}

	byName := map[string]types.DiskInfo{}
	for _, d := range disks {
		byName[d.Name] = d
	}

	sda, ok := byName["sda"]
	if !ok {
		t.Fatal("sda missing from the result")
	}
	// size is in 512-byte sectors.
	if sda.SizeBytes != 1953525168*512 {
		t.Errorf("SizeBytes = %d, want %d", sda.SizeBytes, uint64(1953525168)*512)
	}
	if sda.Model != "Test Disk" || sda.Serial != "SER123" || sda.Vendor != "ACME" {
		t.Errorf("identity not read: %+v", sda)
	}
	if _, ok := byName["nvme0n1"]; !ok {
		t.Error("a device without identity attributes was dropped")
	}
}

// TestCollectFromSysfs_SkipsNonDisks verifies loop devices and partitions are
// not reported as physical disks.
func TestCollectFromSysfs_SkipsNonDisks(t *testing.T) {
	root := t.TempDir()
	withSysBlockRoot(t, root)

	writeSysfsFile(t, filepath.Join(root, "sda", "size"), "100\n")
	for _, skip := range []string{"loop0", "ram0", "dm-0"} {
		writeSysfsFile(t, filepath.Join(root, skip, "size"), "100\n")
	}

	c := NewDiskCollector(quietLogger())
	disks := c.collectFromSysfs(nil, nil)

	for _, d := range disks {
		if strings.HasPrefix(d.Name, "loop") || strings.HasPrefix(d.Name, "ram") {
			t.Errorf("pseudo device %q was reported as a disk", d.Name)
		}
	}
}

func TestCollectFromSysfs_MissingRoot(t *testing.T) {
	withSysBlockRoot(t, filepath.Join(t.TempDir(), "absent"))
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewDiskCollector(quietLogger())
	if disks := c.collectFromSysfs(nil, nil); len(disks) != 0 {
		t.Errorf("got %d disks from a missing sysfs root", len(disks))
	}
}

// TestApplySMART_DispatchesByDeviceName verifies NVMe and ATA devices take
// their respective code paths and that a permission denial is not fatal.
func TestApplySMART_DispatchesByDeviceName(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewDiskCollector(quietLogger())

	// Neither device exists, so both paths take their error branch. The point
	// is that dispatch happens and neither panics nor aborts collection.
	for _, name := range []string{"nvme9n9", "sdzz"} {
		info := types.DiskInfo{Name: name}
		c.applySMART(name, &info)
		if info.SMARTEnabled {
			t.Errorf("%s reported SMART enabled for a nonexistent device", name)
		}
	}
}

// ---- SMBIOS DIMM parsing --------------------------------------------------

// dimmBlob builds a populated Type 17 structure using the shared helper in
// internal_test.go, so this file exercises the collectDIMMs read path rather
// than re-testing the parser.
func dimmBlob(locator string, sizeMB uint16, speed uint16) []byte {
	return buildType17(64, 64, sizeMB, 0, 0x09, 0x22, speed, 0x01, speed,
		1100, 1100, 1100, locator, "BANK 0", "ACME", "SN123", "PN456")
}

func withSMBIOSTable(t *testing.T, data []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "DMI")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture table: %v", err)
	}
	orig := smbiosTablePath
	smbiosTablePath = path
	t.Cleanup(func() { smbiosTablePath = orig })
}

// TestCollectDIMMs_ReadsSMBIOSTable covers the privileged read path using a
// fixture table, so it runs without root.
func TestCollectDIMMs_ReadsSMBIOSTable(t *testing.T) {
	withSMBIOSTable(t, dimmBlob("DIMM_A1", 16384, 3200))
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	dimms := c.collectDIMMs()

	if len(dimms) != 1 {
		t.Fatalf("got %d DIMMs, want 1", len(dimms))
	}
	d := dimms[0]
	if d.SizeBytes != 16384*1024*1024 {
		t.Errorf("SizeBytes = %d, want %d", d.SizeBytes, 16384*1024*1024)
	}
	if d.SpeedMTs != 3200 {
		t.Errorf("SpeedMTs = %d, want 3200", d.SpeedMTs)
	}
	if d.Location != "DIMM_A1" {
		t.Errorf("Locator = %q, want DIMM_A1", d.Location)
	}
	if d.Manufacturer != "ACME" {
		t.Errorf("Manufacturer = %q, want ACME", d.Manufacturer)
	}
}

// TestCollectDIMMs_MultipleModules verifies every populated slot is returned
// and indexed in order.
func TestCollectDIMMs_MultipleModules(t *testing.T) {
	table := append(dimmBlob("DIMM_A1", 8192, 2400), dimmBlob("DIMM_B1", 8192, 2400)...)
	withSMBIOSTable(t, table)
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	dimms := c.collectDIMMs()

	if len(dimms) != 2 {
		t.Fatalf("got %d DIMMs, want 2", len(dimms))
	}
	for i, d := range dimms {
		if d.Index != i {
			t.Errorf("dimms[%d].Index = %d, want %d", i, d.Index, i)
		}
	}
}

// TestCollectDIMMs_UnreadableTableFallsBack verifies an unreadable SMBIOS
// table does not abort memory collection; the ghw path is used instead.
func TestCollectDIMMs_UnreadableTableFallsBack(t *testing.T) {
	orig := smbiosTablePath
	smbiosTablePath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { smbiosTablePath = orig })
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("collectDIMMs panicked with no SMBIOS table: %v", r)
		}
	}()
	_ = c.collectDIMMs()
}

// ---- runtime API error paths ----------------------------------------------

func TestRuntimeStatusError_Message(t *testing.T) {
	e := &runtimeStatusError{Path: "/system/df", Status: 500}
	got := e.Error()
	if !strings.Contains(got, "/system/df") || !strings.Contains(got, "500") {
		t.Errorf("Error() = %q, want it to name the path and status", got)
	}
}

// TestFindRuntimeSocket_NoneReachable verifies an absent runtime is reported
// as such rather than producing a bogus socket path.
func TestFindRuntimeSocket_NoneReachable(t *testing.T) {
	orig := runtimeSocketPaths
	dir := t.TempDir()
	runtimeSocketPaths = []struct {
		path   string
		engine string
	}{
		{filepath.Join(dir, "a.sock"), "docker"},
		{filepath.Join(dir, "b.sock"), "podman"},
	}
	t.Cleanup(func() { runtimeSocketPaths = orig })

	sock, engine := findRuntimeSocket()
	if sock != "" || engine != "" {
		t.Errorf("findRuntimeSocket() = (%q, %q), want empty", sock, engine)
	}
}

// TestRuntimeCollect_DisabledClearsPreviousData verifies turning the runtime
// API off drops what it had gathered, rather than serving stale inventory.
func TestRuntimeCollect_DisabledClearsPreviousData(t *testing.T) {
	c := NewRuntimeCollector(quietLogger())

	// Seed a previous result.
	seeded := types.RuntimeInfo{Available: true, Engine: "docker", ImagesTotal: 5}
	c.info.Store(&seeded)

	c.Enable(false)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if got := c.Info(); got.Available || got.ImagesTotal != 0 {
		t.Errorf("disabling left stale data behind: %+v", got)
	}
}
