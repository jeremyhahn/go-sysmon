package collector

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withHwmonRoot points the DIMM sensor scan at a synthetic tree and replaces
// the sensor probe with a no-op. The real probe runs modprobe and writes to
// i2c new_device, which a unit test must never do to the host.
func withHwmonRoot(t *testing.T, root string) *int {
	t.Helper()

	originalRoot := hwmonClassRoot
	originalProbe := probeDIMMTempSensors
	hwmonClassRoot = root

	probeCalls := 0
	probeDIMMTempSensors = func(*slog.Logger) { probeCalls++ }

	// The probe guard is process-wide, so re-arm it for this case and again
	// afterwards; otherwise test order decides who sees the single call.
	resetDIMMProbeOnce()
	resetWarnOnce()

	t.Cleanup(func() {
		hwmonClassRoot = originalRoot
		probeDIMMTempSensors = originalProbe
		resetDIMMProbeOnce()
		resetWarnOnce()
	})
	return &probeCalls
}

// mkHwmon writes a synthetic hwmon device with a driver name and temperature.
func mkHwmon(t *testing.T, root, dir, name, tempMilliC string) {
	t.Helper()
	writeSysfsFile(t, filepath.Join(root, dir, "name"), name+"\n")
	if tempMilliC != "" {
		writeSysfsFile(t, filepath.Join(root, dir, "temp1_input"), tempMilliC+"\n")
	}
}

// ---- scanDIMMHwmonTemps ---------------------------------------------------

func TestScanDIMMHwmonTemps_ReadsKnownDrivers(t *testing.T) {
	root := t.TempDir()
	withHwmonRoot(t, root)

	mkHwmon(t, root, "hwmon0", "spd5118", "27250")
	mkHwmon(t, root, "hwmon1", "spd5118", "28500")
	// An unrelated driver must be ignored.
	mkHwmon(t, root, "hwmon2", "coretemp", "55000")

	temps := scanDIMMHwmonTemps()
	if len(temps) != 2 {
		t.Fatalf("got %d temps, want 2 (coretemp excluded): %v", len(temps), temps)
	}
	for _, want := range []float64{27.25, 28.5} {
		found := false
		for _, got := range temps {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("temperature %.2f missing from %v", want, temps)
		}
	}
}

// TestScanDIMMHwmonTemps_SkipsUnreadable verifies a sensor reporting zero is
// not counted as a real reading.
func TestScanDIMMHwmonTemps_SkipsUnreadable(t *testing.T) {
	root := t.TempDir()
	withHwmonRoot(t, root)

	mkHwmon(t, root, "hwmon0", "spd5118", "0")
	mkHwmon(t, root, "hwmon1", "jc42", "")

	if temps := scanDIMMHwmonTemps(); len(temps) != 0 {
		t.Errorf("scanDIMMHwmonTemps() = %v, want none for unreadable sensors", temps)
	}
}

func TestScanDIMMHwmonTemps_MissingRoot(t *testing.T) {
	withHwmonRoot(t, filepath.Join(t.TempDir(), "absent"))
	if temps := scanDIMMHwmonTemps(); temps != nil {
		t.Errorf("scanDIMMHwmonTemps() = %v, want nil when hwmon is absent", temps)
	}
}

// ---- applyDIMMTemperatures ------------------------------------------------

// TestApplyDIMMTemperatures_MatchesPositionally verifies sensors are assigned
// to DIMMs in order and that the host-modifying probe is not triggered when
// sensors already exist.
func TestApplyDIMMTemperatures_MatchesPositionally(t *testing.T) {
	root := t.TempDir()
	probeCalls := withHwmonRoot(t, root)

	mkHwmon(t, root, "hwmon0", "spd5118", "27000")
	mkHwmon(t, root, "hwmon1", "spd5118", "28000")

	dimms := []types.DIMMInfo{{Index: 0}, {Index: 1}}
	if !applyDIMMTemperatures(dimms, slog.Default()) {
		t.Fatal("applyDIMMTemperatures() = false, want true when sensors exist")
	}
	if dimms[0].Temperature == 0 || dimms[1].Temperature == 0 {
		t.Errorf("temperatures not applied: %+v", dimms)
	}
	if *probeCalls != 0 {
		t.Errorf("probe ran %d times; it must not run when sensors are already present", *probeCalls)
	}
}

// TestApplyDIMMTemperatures_ProbesWhenNoSensors verifies the fallback path
// triggers exactly once, and reports failure when nothing turns up.
func TestApplyDIMMTemperatures_ProbesWhenNoSensors(t *testing.T) {
	probeCalls := withHwmonRoot(t, t.TempDir())

	dimms := []types.DIMMInfo{{Index: 0}}
	if applyDIMMTemperatures(dimms, slog.Default()) {
		t.Error("applyDIMMTemperatures() = true, want false when no sensors exist")
	}
	if *probeCalls != 1 {
		t.Errorf("probe ran %d times, want exactly 1", *probeCalls)
	}
	if dimms[0].Temperature != 0 {
		t.Errorf("Temperature = %f, want 0 when no sensor was found", dimms[0].Temperature)
	}
}

// TestApplyDIMMTemperatures_FewerSensorsThanDIMMs verifies the loop stops at
// the sensor count instead of indexing past the end.
func TestApplyDIMMTemperatures_FewerSensorsThanDIMMs(t *testing.T) {
	root := t.TempDir()
	withHwmonRoot(t, root)
	mkHwmon(t, root, "hwmon0", "spd5118", "30000")

	dimms := []types.DIMMInfo{{Index: 0}, {Index: 1}, {Index: 2}}
	if !applyDIMMTemperatures(dimms, slog.Default()) {
		t.Fatal("applyDIMMTemperatures() = false, want true")
	}
	if dimms[0].Temperature != 30 {
		t.Errorf("dimms[0].Temperature = %f, want 30", dimms[0].Temperature)
	}
	if dimms[1].Temperature != 0 || dimms[2].Temperature != 0 {
		t.Errorf("unmatched DIMMs must keep a zero temperature: %+v", dimms[1:])
	}
}

// TestApplyDIMMTemperatures_ProbesAtMostOncePerRun is the regression test for
// a host-mutating probe on the fast collection tier. Memory is collected every
// tick, so retrying meant forking modprobe and writing to every SMBus adapter
// once a second, forever, on a machine whose modules are unavailable.
func TestApplyDIMMTemperatures_ProbesAtMostOncePerRun(t *testing.T) {
	probeCalls := withHwmonRoot(t, t.TempDir())

	// Simulate a hundred collection cycles on a machine with no sensors.
	for i := 0; i < 100; i++ {
		applyDIMMTemperatures([]types.DIMMInfo{{Index: 0}}, slog.Default())
	}

	if *probeCalls != 1 {
		t.Errorf("probe ran %d times across 100 cycles, want exactly 1", *probeCalls)
	}
}

// TestApplyDIMMTemperatures_WarnsOncePerRun verifies the accompanying warning
// is not repeated on every cycle either.
func TestApplyDIMMTemperatures_WarnsOncePerRun(t *testing.T) {
	withHwmonRoot(t, t.TempDir())

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil))) })

	for i := 0; i < 50; i++ {
		applyDIMMTemperatures([]types.DIMMInfo{{Index: 0}}, logger)
	}

	got := strings.Count(buf.String(), "no DIMM temperature sensors detected")
	if got != 1 {
		t.Errorf("warned %d times across 50 cycles, want exactly 1", got)
	}
}
