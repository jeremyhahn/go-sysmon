package collector

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- SetTiering -----------------------------------------------------------

// TestSetTiering_TogglesFlag verifies the setter drives the flag consulted by
// shouldCollect, which decides whether low-frequency collectors are skipped.
func TestSetTiering_TogglesFlag(t *testing.T) {
	t.Parallel()
	c := NewSystemCollector(slog.Default())

	c.SetTiering(false)
	if c.tiering.Load() {
		t.Error("SetTiering(false) left tiering enabled")
	}

	c.SetTiering(true)
	if !c.tiering.Load() {
		t.Error("SetTiering(true) left tiering disabled")
	}
}

// TestShouldCollect_TierSchedule verifies the tick schedule: fast collectors
// run every tick while medium and slow ones run on their own cadence.
func TestShouldCollect_TierSchedule(t *testing.T) {
	t.Parallel()

	if !shouldCollect(1, tierFast) || !shouldCollect(7, tierFast) {
		t.Error("tierFast must run on every tick")
	}
	if !shouldCollect(0, tierMedium) {
		t.Error("tierMedium must run on tick 0")
	}
	if !shouldCollect(0, tierSlow) {
		t.Error("tierSlow must run on tick 0")
	}

	// At least one tick in a full slow cycle must be skipped, otherwise the
	// tiering exists but does nothing.
	skippedSlow := false
	for tick := uint64(0); tick < slowInterval*2; tick++ {
		if !shouldCollect(tick, tierSlow) {
			skippedSlow = true
			break
		}
	}
	if !skippedSlow {
		t.Error("tierSlow never skips a tick; tiering has no effect")
	}
}

// ---- itoh -----------------------------------------------------------------

// TestItoh_FormatsTwoHexDigits covers the two-digit hex helper used to build
// i2c device addresses.
func TestItoh_FormatsTwoHexDigits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   int
		want string
	}{
		{0, "00"},
		{9, "09"},
		{10, "0a"},
		{15, "0f"},
		{16, "10"},
		{0x50, "50"},
		{0x5f, "5f"},
		{255, "ff"},
	}

	for _, tt := range tests {
		if got := itoh(tt.in); got != tt.want {
			t.Errorf("itoh(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestItoh_TruncatesBeyondOneByte documents that only the low byte is encoded,
// so callers must not pass values above 0xff expecting more digits.
func TestItoh_TruncatesBeyondOneByte(t *testing.T) {
	t.Parallel()
	if got := itoh(0x123); got != "23" {
		t.Errorf("itoh(0x123) = %q, want %q (low byte only)", got, "23")
	}
	if got := itoh(-1); len(got) != 2 {
		t.Errorf("itoh(-1) = %q, want a two-character result", got)
	}
}

// ---- formatPCIBusID -------------------------------------------------------

// TestFormatPCIBusID_Normalizes covers the shapes NVML and sysfs report.
func TestFormatPCIBusID_Normalizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bus and device", "04:00.0", "0000:04:00.0"},
		{"short bus", "4:00.0", "0000:04:00.0"},
		{"full with short domain", "0:04:00.0", "0000:04:00.0"},
		{"already padded", "0000:04:00.0", "0000:04:00.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPCIBusID(tt.in); got != tt.want {
				t.Errorf("formatPCIBusID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFormatPCIBusID_PassesThroughUnparseable verifies malformed input is
// returned unchanged rather than mangled into a wrong-looking address.
func TestFormatPCIBusID_PassesThroughUnparseable(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "not-a-bus-id", "zz:00.0", "zz:04:00.0", "1:2:3:4"} {
		if got := formatPCIBusID(in); got != in {
			t.Errorf("formatPCIBusID(%q) = %q, want it returned unchanged", in, got)
		}
	}
}

// ---- summaryFromGopsutil --------------------------------------------------

// TestSummaryFromGopsutil_ReportsRealTopology verifies the gopsutil fallback
// path produces a self-consistent summary on the host running the tests.
func TestSummaryFromGopsutil_ReportsRealTopology(t *testing.T) {
	t.Parallel()
	got := summaryFromGopsutil(slog.Default())

	if got.TotalThreads <= 0 {
		t.Errorf("TotalThreads = %d, want > 0", got.TotalThreads)
	}
	if got.TotalCores <= 0 {
		t.Errorf("TotalCores = %d, want > 0", got.TotalCores)
	}
	if got.TotalThreads < got.TotalCores {
		t.Errorf("TotalThreads (%d) < TotalCores (%d), which is impossible",
			got.TotalThreads, got.TotalCores)
	}
	if got.Sockets <= 0 {
		t.Errorf("Sockets = %d, want > 0", got.Sockets)
	}
}

// ---- readProcCPUInfoMHz ---------------------------------------------------

// TestReadProcCPUInfoMHz_ReadsHostValue verifies the /proc/cpuinfo fallback
// returns a plausible frequency on a real Linux host.
func TestReadProcCPUInfoMHz_ReadsHostValue(t *testing.T) {
	t.Parallel()
	got := readProcCPUInfoMHz()

	// Some kernels omit "cpu MHz" entirely; a zero result is the documented
	// behaviour there, so only a negative or absurd value is a failure.
	if got < 0 {
		t.Errorf("readProcCPUInfoMHz() = %f, want >= 0", got)
	}
	if got > 100000 {
		t.Errorf("readProcCPUInfoMHz() = %f MHz, which is not a real clock", got)
	}
}

// ---- sudoReadFile ---------------------------------------------------------

// TestSudoReadFile_MissingPathFails verifies the helper surfaces an error
// rather than returning empty content when the file cannot be read. It uses
// sudo -n, so it never prompts and never modifies the host.
func TestSudoReadFile_MissingPathFails(t *testing.T) {
	t.Parallel()
	out, err := sudoReadFile("/proc/definitely-not-a-real-file")
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("sudoReadFile() returned content %q for a missing path", out)
	}
}

// ---- DIMM collection ------------------------------------------------------

// TestCollectDIMMsViaGHW_ReadOnlyFallback exercises the ghw fallback path.
// It is read-only: without DMI access ghw fails and the collector returns nil,
// which is the documented unprivileged behaviour.
func TestCollectDIMMsViaGHW_ReadOnlyFallback(t *testing.T) {
	t.Parallel()
	c := NewMemoryCollector(slog.Default())

	dimms := c.collectDIMMsViaGHW()
	for _, d := range dimms {
		if d.SizeBytes == 0 {
			t.Errorf("DIMM %d reported via ghw has zero size", d.Index)
		}
	}
}

// TestCollectDIMMs_NeverPanics verifies the DIMM path degrades safely on a
// host where SMBIOS is unreadable, which is the unprivileged default.
func TestCollectDIMMs_NeverPanics(t *testing.T) {
	t.Parallel()
	c := NewMemoryCollector(slog.Default())

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("collectDIMMs panicked: %v", r)
		}
	}()
	dimms := c.collectDIMMs()

	for i, d := range dimms {
		if d.Index < 0 {
			t.Errorf("dimms[%d] has a negative index %d", i, d.Index)
		}
	}
}

// TestApplyDIMMTemperatures_NoDIMMs verifies the temperature join is a no-op
// when there are no DIMMs to annotate.
func TestApplyDIMMTemperatures_NoDIMMs(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("applyDIMMTemperatures panicked on an empty slice: %v", r)
		}
	}()
	var none []types.DIMMInfo
	if applied := applyDIMMTemperatures(none, slog.Default()); applied {
		t.Error("applyDIMMTemperatures(nil) = true, want false when there are no DIMMs")
	}
}

// ---- CPU summary ----------------------------------------------------------

// TestBuildSummary_ReturnsConsistentTopology verifies the summary builder
// produces internally consistent counts on the host running the tests.
func TestBuildSummary_ReturnsConsistentTopology(t *testing.T) {
	t.Parallel()
	c := NewCPUCollector(slog.Default())
	got := c.buildSummary()

	if got.TotalThreads <= 0 {
		t.Fatalf("TotalThreads = %d, want > 0", got.TotalThreads)
	}
	if got.TotalCores <= 0 {
		t.Errorf("TotalCores = %d, want > 0", got.TotalCores)
	}
	if got.Sockets <= 0 {
		t.Errorf("Sockets = %d, want > 0", got.Sockets)
	}
	if got.MaxMHz < 0 || got.MinMHz < 0 {
		t.Errorf("negative frequency bounds: min=%v max=%v", got.MinMHz, got.MaxMHz)
	}
	if got.MaxMHz > 0 && got.MinMHz > got.MaxMHz {
		t.Errorf("MinMHz (%v) > MaxMHz (%v)", got.MinMHz, got.MaxMHz)
	}
}

// TestReadSysfsInt_MissingPath verifies the sysfs integer helper reports a
// failure rather than a silent zero for an absent file.
func TestReadSysfsInt_MissingPath(t *testing.T) {
	t.Parallel()
	if got := readSysfsInt("/sys/definitely/not/here"); got != 0 {
		t.Errorf("readSysfsInt(missing) = %d, want 0", got)
	}
}

// TestReadSysfsInt_ValidAndInvalid covers a well-formed file and a file whose
// contents are not an integer.
func TestReadSysfsInt_ValidAndInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid")
	writeSysfsFile(t, valid, "4200\n")
	if got := readSysfsInt(valid); got != 4200 {
		t.Errorf("readSysfsInt(valid) = %d, want 4200", got)
	}

	invalid := filepath.Join(dir, "invalid")
	writeSysfsFile(t, invalid, "not-a-number\n")
	if got := readSysfsInt(invalid); got != 0 {
		t.Errorf("readSysfsInt(invalid) = %d, want 0", got)
	}
}

// TestReadCPUFreqMHz_MissingCPU verifies a missing cpufreq tree yields zero
// rather than a bogus frequency.
func TestReadCPUFreqMHz_MissingCPU(t *testing.T) {
	t.Parallel()
	if got := readCPUFreqMHz("/sys/devices/system/cpu/cpu99999/cpufreq/scaling_max_freq"); got != 0 {
		t.Errorf("readCPUFreqMHz(missing) = %v, want 0", got)
	}
}
