package collector

import (
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
)

// ---- hwmonSensorIndices ---------------------------------------------------

// TestHwmonSensorIndices_SparseNumbering is the regression test for per-core
// temperatures going missing. The coretemp driver numbers sensors from the
// physical core ID, so a machine whose cores are 0, 4, 8, 9 ... produces
// temp2, temp6, temp10, temp11 ... with gaps. Scanning n = 1, 2, 3 ... and
// stopping at the first gap found only the first core.
func TestHwmonSensorIndices_SparseNumbering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Real numbering observed on an Intel Core Ultra 9 285K.
	present := []int{1, 2, 6, 10, 11, 12, 16, 22, 26, 46}
	for _, n := range present {
		writeSysfsFile(t, filepath.Join(dir, "temp"+strconv.Itoa(n)+"_input"), "40000\n")
	}

	got := hwmonSensorIndices(dir, "temp")
	if len(got) != len(present) {
		t.Fatalf("hwmonSensorIndices() returned %d indices (%v), want %d (%v)",
			len(got), got, len(present), present)
	}
	for i, want := range present {
		if got[i] != want {
			t.Errorf("index[%d] = %d, want %d (full result %v)", i, got[i], want, got)
		}
	}
}

// TestHwmonSensorIndices_EmptyAndMissing covers the degenerate inputs: a
// directory with no matching files and a path that does not exist.
func TestHwmonSensorIndices_EmptyAndMissing(t *testing.T) {
	t.Parallel()

	empty := t.TempDir()
	if got := hwmonSensorIndices(empty, "temp"); len(got) != 0 {
		t.Errorf("hwmonSensorIndices(empty) = %v, want no indices", got)
	}

	if got := hwmonSensorIndices(filepath.Join(empty, "does-not-exist"), "temp"); len(got) != 0 {
		t.Errorf("hwmonSensorIndices(missing) = %v, want no indices", got)
	}
}

// TestHwmonSensorIndices_IgnoresMalformedNames verifies that files which match
// the glob but carry no parseable index are skipped rather than counted.
func TestHwmonSensorIndices_IgnoresMalformedNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeSysfsFile(t, filepath.Join(dir, "temp1_input"), "40000\n")
	writeSysfsFile(t, filepath.Join(dir, "tempX_input"), "garbage\n")
	writeSysfsFile(t, filepath.Join(dir, "temp_input"), "garbage\n")
	// A different prefix must not be picked up.
	writeSysfsFile(t, filepath.Join(dir, "fan1_input"), "1200\n")

	got := hwmonSensorIndices(dir, "temp")
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("hwmonSensorIndices() = %v, want [1]", got)
	}
}

// TestHwmonSensorIndices_PrefixIsolation verifies each sensor family is
// enumerated independently.
func TestHwmonSensorIndices_PrefixIsolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeSysfsFile(t, filepath.Join(dir, "temp1_input"), "40000\n")
	writeSysfsFile(t, filepath.Join(dir, "temp5_input"), "41000\n")
	writeSysfsFile(t, filepath.Join(dir, "fan2_input"), "1200\n")
	writeSysfsFile(t, filepath.Join(dir, "in3_input"), "1100\n")

	if got := hwmonSensorIndices(dir, "temp"); len(got) != 2 {
		t.Errorf("temp indices = %v, want 2 entries", got)
	}
	if got := hwmonSensorIndices(dir, "fan"); len(got) != 1 || got[0] != 2 {
		t.Errorf("fan indices = %v, want [2]", got)
	}
	if got := hwmonSensorIndices(dir, "in"); len(got) != 1 || got[0] != 3 {
		t.Errorf("in indices = %v, want [3]", got)
	}
}

// ---- per-core temps over a sparse tree ------------------------------------

// TestCollectIntelCoreTemps_SparseIndices is the end-to-end version of the
// regression: every core must get a reading even though the sensor files are
// not numbered contiguously.
func TestCollectIntelCoreTemps_SparseIndices(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	sensors := map[string]string{
		"temp1_input": "55000\n",
		"temp1_label": "Package id 0\n",
		// Core 0 at temp2, then a gap before Core 4 at temp6.
		"temp2_input":  "36000\n",
		"temp2_label":  "Core 0\n",
		"temp6_input":  "32000\n",
		"temp6_label":  "Core 4\n",
		"temp10_input": "34000\n",
		"temp10_label": "Core 8\n",
		"temp46_input": "38000\n",
		"temp46_label": "Core 44\n",
	}
	buildIntelHwmon(t, base, sensors)

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()

	if len(temps) != 4 {
		t.Fatalf("got %d core temps, want 4 (one per core across the gaps): %+v", len(temps), temps)
	}

	wantByCore := map[int]float64{0: 36, 4: 32, 8: 34, 44: 38}
	for _, ct := range temps {
		want, ok := wantByCore[ct.CoreID]
		if !ok {
			t.Errorf("unexpected core id %d", ct.CoreID)
			continue
		}
		if ct.TempCelsius != want {
			t.Errorf("core %d temp = %.1f, want %.1f", ct.CoreID, ct.TempCelsius, want)
		}
		delete(wantByCore, ct.CoreID)
	}
	for missing := range wantByCore {
		t.Errorf("core %d produced no temperature reading", missing)
	}
}

// ---- fans ------------------------------------------------------------------

// TestCollectFans_SkipsUnreadableInput verifies that a fanN_input which exists
// but cannot be read (acpi_fan reports ENODEV) is skipped instead of being
// reported as a 0 RPM fan that does not exist.
func TestCollectFans_SkipsUnreadableInput(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	// A healthy fan.
	good := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(good, "name"), "amdgpu\n")
	writeSysfsFile(t, filepath.Join(good, "fan1_input"), "1545\n")
	writeSysfsFile(t, filepath.Join(good, "fan1_max"), "3200\n")

	// A fan whose input file is present but empty, standing in for a read that
	// fails at the driver level.
	bad := filepath.Join(base, "hwmon1")
	writeSysfsFile(t, filepath.Join(bad, "name"), "acpi_fan\n")
	writeSysfsFile(t, filepath.Join(bad, "fan1_input"), "")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	fans := c.collectFans()

	if len(fans) != 1 {
		t.Fatalf("got %d fans, want 1 (the unreadable one skipped): %+v", len(fans), fans)
	}
	if fans[0].RPM != 1545 {
		t.Errorf("fan RPM = %d, want 1545", fans[0].RPM)
	}
	if fans[0].HwmonName != "amdgpu" {
		t.Errorf("fan hwmon = %q, want %q", fans[0].HwmonName, "amdgpu")
	}
}

// TestCollectFans_SparseIndices verifies fan enumeration also survives gaps.
func TestCollectFans_SparseIndices(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "nct6798\n")
	for _, n := range []int{1, 4, 7} {
		writeSysfsFile(t, filepath.Join(hwmon, "fan"+strconv.Itoa(n)+"_input"), "1000\n")
	}

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	fans := c.collectFans()

	if len(fans) != 3 {
		t.Fatalf("got %d fans, want 3 across sparse indices: %+v", len(fans), fans)
	}
}

// ---- Collect wiring --------------------------------------------------------

// TestCollect_PopulatesEverySection is the regression test for four collectors
// that were implemented but never called by Collect, leaving the fans, thermal
// zones, throttle counters and PSI sections permanently empty.
func TestCollect_PopulatesEverySection(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	hwmonBase := filepath.Join(base, "hwmon")
	core := filepath.Join(hwmonBase, "hwmon0")
	writeSysfsFile(t, filepath.Join(core, "name"), "coretemp\n")
	writeSysfsFile(t, filepath.Join(core, "temp1_input"), "55000\n")
	writeSysfsFile(t, filepath.Join(core, "temp1_label"), "Package id 0\n")
	writeSysfsFile(t, filepath.Join(core, "temp2_input"), "52000\n")
	writeSysfsFile(t, filepath.Join(core, "temp2_label"), "Core 0\n")
	writeSysfsFile(t, filepath.Join(core, "fan1_input"), "1200\n")

	thermalBase := filepath.Join(base, "thermal")
	zone := filepath.Join(thermalBase, "thermal_zone0")
	writeSysfsFile(t, filepath.Join(zone, "type"), "x86_pkg_temp\n")
	writeSysfsFile(t, filepath.Join(zone, "temp"), "48000\n")

	cpuBase := filepath.Join(base, "cpu")
	writeSysfsFile(t, filepath.Join(cpuBase, "cpu0", "thermal_throttle", "core_throttle_count"), "3\n")

	pressureBase := filepath.Join(base, "pressure")
	writeSysfsFile(t, filepath.Join(pressureBase, "cpu"),
		"some avg10=1.00 avg60=2.00 avg300=3.00 total=123456\n")

	c := newSensorCollectorWithPaths(slog.Default(), hwmonBase, thermalBase,
		filepath.Join(base, "powercap"), cpuBase, pressureBase)

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	data := c.Info()

	if len(data.CoreTemps) == 0 {
		t.Error("Collect() left CoreTemps empty")
	}
	if len(data.Fans) == 0 {
		t.Error("Collect() left Fans empty; collectFans is not wired in")
	}
	if len(data.ThermalZones) == 0 {
		t.Error("Collect() left ThermalZones empty; collectThermalZones is not wired in")
	}
	if len(data.ThermalThrottle) == 0 {
		t.Error("Collect() left ThermalThrottle empty; collectThrottleInfo is not wired in")
	}
	if data.PSI.CPU.SomeAvg10 == 0 {
		t.Error("Collect() left PSI empty; collectPSI is not wired in")
	}
}

// TestCollect_MissingSysfsTreesIsNotAnError verifies Collect degrades to empty
// sections on a machine where none of the sysfs trees exist.
func TestCollect_MissingSysfsTreesIsNotAnError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope")

	c := newSensorCollectorWithPaths(slog.Default(), missing, missing, missing, missing, missing)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil when sysfs is absent", err)
	}

	data := c.Info()
	if len(data.CoreTemps) != 0 || len(data.Fans) != 0 || len(data.ThermalZones) != 0 {
		t.Errorf("Collect() invented data from a missing sysfs tree: %+v", data)
	}
}
