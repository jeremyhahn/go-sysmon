package collector

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withHwmonRoot points the DIMM sensor scan at a synthetic tree, and clears
// the once-per-run warnings for the case and after it.
func withHwmonRoot(t *testing.T, root string) {
	t.Helper()

	originalRoot := hwmonClassRoot
	hwmonClassRoot = root
	resetWarnOnce()

	t.Cleanup(func() {
		hwmonClassRoot = originalRoot
		resetWarnOnce()
	})
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
// to DIMMs in order.
func TestApplyDIMMTemperatures_MatchesPositionally(t *testing.T) {
	root := t.TempDir()
	withHwmonRoot(t, root)

	mkHwmon(t, root, "hwmon0", "spd5118", "27000")
	mkHwmon(t, root, "hwmon1", "spd5118", "28000")

	dimms := []types.DIMMInfo{{Index: 0}, {Index: 1}}
	if !applyDIMMTemperatures(dimms, slog.Default()) {
		t.Fatal("applyDIMMTemperatures() = false, want true when sensors exist")
	}
	if dimms[0].Temperature == 0 || dimms[1].Temperature == 0 {
		t.Errorf("temperatures not applied: %+v", dimms)
	}
}

// TestApplyDIMMTemperatures_NoSensorsLeavesNoReading verifies a host whose
// kernel exposes no DIMM sensor reports none, and that nothing is done to make
// one appear: the scan root is the only thing read, and it is left as it was.
func TestApplyDIMMTemperatures_NoSensorsLeavesNoReading(t *testing.T) {
	root := t.TempDir()
	withHwmonRoot(t, root)

	dimms := []types.DIMMInfo{{Index: 0}}
	if applyDIMMTemperatures(dimms, slog.Default()) {
		t.Error("applyDIMMTemperatures() = true, want false when no sensors exist")
	}
	if dimms[0].Temperature != 0 {
		t.Errorf("Temperature = %f, want 0 when no sensor was found", dimms[0].Temperature)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("the hwmon root holds %v (%v) after the scan, want it untouched", entries, err)
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

	got := strings.Count(buf.String(), "no DIMM temperature sensors are exposed")
	if got != 1 {
		t.Errorf("warned %d times across 50 cycles, want exactly 1", got)
	}
}

// ---- the cached inventory -------------------------------------------------

// TestMemoryCollector_InventoryIsStableAndTemperaturesAreNot is the pair of
// properties the cache has to hold at once: the module list is read once and
// carried forward, and the readings applied onto it are not.
//
// The second half is why Collect works on a COPY. Handing applyDIMMTemperatures
// the cached slice would leave the previous tick's value on a module whose
// sensor has stopped answering -- a temperature that is no longer being
// measured, still on the dashboard, indistinguishable from one that is.
func TestMemoryCollector_InventoryIsStableAndTemperaturesAreNot(t *testing.T) {
	root := t.TempDir()
	withHwmonRoot(t, root)
	mkHwmon(t, root, "hwmon0", "spd5118", "31000")

	c := NewMemoryCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The inventory comes from this host's SMBIOS, which a unit test cannot
	// stage. Give the collector a known one so the assertions are about the
	// caching and not about the machine the suite runs on.
	c.dimms = []types.DIMMInfo{{Location: "DIMM_A1", SizeBytes: 1 << 34}}

	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	first := c.Info()
	if len(first.DIMMs) != 1 {
		t.Fatalf("got %d DIMMs, want the one in the cached inventory", len(first.DIMMs))
	}
	if first.DIMMs[0].Temperature != 31 {
		t.Fatalf("temperature = %v, want 31 from the fixture", first.DIMMs[0].Temperature)
	}

	// The sensor goes away: the module is still installed, its temperature is
	// no longer readable.
	if err := os.RemoveAll(filepath.Join(root, "hwmon0")); err != nil {
		t.Fatalf("remove hwmon device: %v", err)
	}

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	second := c.Info()

	if len(second.DIMMs) != 1 || second.DIMMs[0].Location != "DIMM_A1" {
		t.Fatalf("inventory = %+v, want the cached module carried forward", second.DIMMs)
	}
	if got := second.DIMMs[0].Temperature; got != 0 {
		t.Errorf("temperature = %v after the sensor disappeared, want 0: a reading "+
			"that is no longer measured is being displayed as though it were", got)
	}
	if second.TempSensorDetected {
		t.Error("TempSensorDetected is true with no sensor present")
	}
}

// The constructor has to do the read, or the inventory is empty for the life of
// the process and every snapshot reports a machine with no memory modules.
//
// It compares against a second independent read rather than asserting
// non-empty, because a virtual machine legitimately reports no modules at all
// -- on that host a non-empty check passes or fails for reasons that have
// nothing to do with this wiring. Two reads of the same source agree either
// way, and disagree only if one of them did not happen.
func TestNewMemoryCollector_ReadsTheInventoryOnce(t *testing.T) {
	c := NewMemoryCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))

	direct := c.collectDIMMs()
	if len(c.dimms) != len(direct) {
		t.Fatalf("constructor read %d modules, a direct read finds %d: the inventory is not being read at construction",
			len(c.dimms), len(direct))
	}
}
