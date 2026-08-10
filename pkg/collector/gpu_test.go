// White-box tests for the GPU collector helpers.
// Lives in package collector to access unexported symbols.
package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// ---------------------------------------------------------------------------
// GPUCollector.Collect tests
// ---------------------------------------------------------------------------

// TestGPUCollector_NVMLUnavailable verifies that Collect succeeds and returns
// no error when NVML is unavailable (libnvidia-ml.so not present). NVIDIA GPUs
// are silently skipped; AMD/Intel GPUs from sysfs are still collected if
// present.
func TestGPUCollector_NVMLUnavailable(t *testing.T) {
	c := NewGPUCollector(newNopLogger())
	err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect returned error when NVML unavailable: %v", err)
	}
	// Just verify no panic and no error; GPU count depends on the host.
}

// TestGPUCollector_Info verifies that Info always returns a non-nil slice.
func TestGPUCollector_Info(t *testing.T) {
	t.Parallel()
	c := NewGPUCollector(newNopLogger())
	gpus := c.Info()
	if gpus == nil {
		t.Error("Info() returned nil slice before Collect")
	}
}

// TestGPUCollector_CancelledContext verifies that a pre-cancelled context
// causes Collect to return nil without panicking.
func TestGPUCollector_CancelledContext(t *testing.T) {
	t.Parallel()
	c := NewGPUCollector(newNopLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Should not panic regardless of whether NVML is present.
	_ = c.Collect(ctx)
}

// ---------------------------------------------------------------------------
// pcieGenFromSpeed tests
// ---------------------------------------------------------------------------

func TestPcieGenFromSpeed_KnownValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  int
	}{
		{"2.5 GT/s", 1},
		{"5.0 GT/s", 2},
		{"8.0 GT/s", 3},
		{"16.0 GT/s", 4},
		{"32.0 GT/s", 5},
		{"64.0 GT/s", 6},
	}
	for _, tc := range cases {
		got := pcieGenFromSpeed(tc.input)
		if got != tc.want {
			t.Errorf("pcieGenFromSpeed(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestPcieGenFromSpeed_InvalidInput(t *testing.T) {
	t.Parallel()
	cases := []string{"", "unknown", "N/A", "garbage"}
	for _, s := range cases {
		got := pcieGenFromSpeed(s)
		if got != 0 {
			t.Errorf("pcieGenFromSpeed(%q) = %d, want 0", s, got)
		}
	}
}

// ---------------------------------------------------------------------------
// parseActiveDPMClock tests
// ---------------------------------------------------------------------------

func TestParseActiveDPMClock_ActiveEntry(t *testing.T) {
	t.Parallel()
	input := `0: 300Mhz
1: 600Mhz
2: 900Mhz
7: 1411Mhz *
`
	got := parseActiveDPMClock([]byte(input))
	if got != 1411 {
		t.Errorf("parseActiveDPMClock = %d, want 1411", got)
	}
}

func TestParseActiveDPMClock_FirstEntry(t *testing.T) {
	t.Parallel()
	// Active entry is the first line.
	input := "0: 300Mhz *\n1: 600Mhz\n"
	got := parseActiveDPMClock([]byte(input))
	if got != 300 {
		t.Errorf("parseActiveDPMClock = %d, want 300", got)
	}
}

func TestParseActiveDPMClock_NoActive(t *testing.T) {
	t.Parallel()
	// No line ends with '*'; should return 0.
	input := "0: 300Mhz\n1: 600Mhz\n"
	got := parseActiveDPMClock([]byte(input))
	if got != 0 {
		t.Errorf("parseActiveDPMClock with no active = %d, want 0", got)
	}
}

func TestParseActiveDPMClock_Empty(t *testing.T) {
	t.Parallel()
	got := parseActiveDPMClock([]byte{})
	if got != 0 {
		t.Errorf("parseActiveDPMClock empty = %d, want 0", got)
	}
}

func TestParseActiveDPMClock_UppercaseMHz(t *testing.T) {
	t.Parallel()
	// Some kernels emit "MHz" rather than "Mhz".
	input := "3: 1200MHz *\n"
	got := parseActiveDPMClock([]byte(input))
	if got != 1200 {
		t.Errorf("parseActiveDPMClock (MHz) = %d, want 1200", got)
	}
}

// ---------------------------------------------------------------------------
// normalisePCIBusID tests
// ---------------------------------------------------------------------------

func TestNormalisePCIBusID_NvmlFormat(t *testing.T) {
	t.Parallel()
	// NVML returns "0000:02:00.0" (4-digit domain).
	got := normalisePCIBusID("0000:02:00.0")
	want := "02:00.0"
	if got != want {
		t.Errorf("normalisePCIBusID(%q) = %q, want %q", "0000:02:00.0", got, want)
	}
}

func TestNormalisePCIBusID_UeventFormat(t *testing.T) {
	t.Parallel()
	uevent := "DRIVER=nvidia\nPCI_SLOT_NAME=0000:02:00.0\nPCI_ID=10DE:2BB1\n"
	got := normalisePCIBusID(uevent)
	want := "02:00.0"
	if got != want {
		t.Errorf("normalisePCIBusID(uevent) = %q, want %q", got, want)
	}
}

func TestNormalisePCIBusID_AlreadyNormal(t *testing.T) {
	t.Parallel()
	got := normalisePCIBusID("02:00.0")
	want := "02:00.0"
	if got != want {
		t.Errorf("normalisePCIBusID(%q) = %q, want %q", "02:00.0", got, want)
	}
}

// ---------------------------------------------------------------------------
// nvmlBusIDString tests
// ---------------------------------------------------------------------------

func TestNvmlBusIDString_NullTerminated(t *testing.T) {
	t.Parallel()
	// Simulates NVML's 32-element C char array: content followed by nulls.
	// The element type is int8 because cgo maps C.char that way.
	var b [32]int8
	for i, c := range []byte("0000:02:00.0") {
		b[i] = int8(c)
	}
	got := nvmlBusIDString(b[:])
	want := "0000:02:00.0"
	if got != want {
		t.Errorf("nvmlBusIDString = %q, want %q", got, want)
	}
}

func TestNvmlBusIDString_EmptyArray(t *testing.T) {
	t.Parallel()
	var b [32]int8
	got := nvmlBusIDString(b[:])
	if got != "" {
		t.Errorf("nvmlBusIDString of all-zero array = %q, want %q", got, "")
	}
}

// ---------------------------------------------------------------------------
// nvmlComputeModeString tests
// ---------------------------------------------------------------------------

func TestNvmlComputeModeString_Known(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode nvml.ComputeMode
		want string
	}{
		{nvml.COMPUTEMODE_DEFAULT, "Default"},
		{nvml.COMPUTEMODE_EXCLUSIVE_THREAD, "Exclusive Thread"},
		{nvml.COMPUTEMODE_PROHIBITED, "Prohibited"},
		{nvml.COMPUTEMODE_EXCLUSIVE_PROCESS, "Exclusive Process"},
	}
	for _, tc := range cases {
		got := nvmlComputeModeString(tc.mode)
		if got != tc.want {
			t.Errorf("nvmlComputeModeString(%d) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestNvmlComputeModeString_Unknown(t *testing.T) {
	t.Parallel()
	got := nvmlComputeModeString(nvml.ComputeMode(99))
	if got != "Unknown" {
		t.Errorf("nvmlComputeModeString(99) = %q, want %q", got, "Unknown")
	}
}

// ---------------------------------------------------------------------------
// nvmlPerfStateString tests
// ---------------------------------------------------------------------------

func TestNvmlPerfStateString_P0(t *testing.T) {
	t.Parallel()
	got := nvmlPerfStateString(nvml.PSTATE_0)
	if got != "P0" {
		t.Errorf("nvmlPerfStateString(PSTATE_0) = %q, want %q", got, "P0")
	}
}

func TestNvmlPerfStateString_P15(t *testing.T) {
	t.Parallel()
	got := nvmlPerfStateString(nvml.PSTATE_15)
	if got != "P15" {
		t.Errorf("nvmlPerfStateString(PSTATE_15) = %q, want %q", got, "P15")
	}
}

func TestNvmlPerfStateString_Unknown(t *testing.T) {
	t.Parallel()
	got := nvmlPerfStateString(nvml.PSTATE_UNKNOWN)
	if got != "Unknown" {
		t.Errorf("nvmlPerfStateString(PSTATE_UNKNOWN) = %q, want %q", got, "Unknown")
	}
}

// ---------------------------------------------------------------------------
// collectAMDGPU sysfs tests (mock filesystem)
// ---------------------------------------------------------------------------

// buildAMDSysfs creates a fake sysfs tree for an AMD GPU card under dir and
// returns the cardPath. All mandatory files are written with representative
// values.
func buildAMDSysfs(t *testing.T, dir string) string {
	t.Helper()

	cardPath := filepath.Join(dir, "card0")
	devPath := filepath.Join(cardPath, "device")
	hwmonPath := filepath.Join(devPath, "hwmon", "hwmon0")

	for _, d := range []string{devPath, hwmonPath} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}

	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	// PCI identity.
	write(filepath.Join(devPath, "vendor"), "0x1002\n")
	write(filepath.Join(devPath, "uevent"),
		"DRIVER=amdgpu\nPCI_SLOT_NAME=0000:04:00.0\nPCI_ID=1002:67DF\n")

	// Utilisation.
	write(filepath.Join(devPath, "gpu_busy_percent"), "72\n")
	write(filepath.Join(devPath, "mem_busy_percent"), "38\n")

	// VRAM: 8 GiB total, 2 GiB used (in bytes).
	const gib = uint64(1024 * 1024 * 1024)
	write(filepath.Join(devPath, "mem_info_vram_total"), "8589934592\n") // 8 GiB
	write(filepath.Join(devPath, "mem_info_vram_used"), "2147483648\n")  // 2 GiB

	// Hwmon: temperature (65000 mC = 65°C), fan (1800/3000), power (120 W cap 200 W).
	write(filepath.Join(hwmonPath, "temp1_input"), "65000\n")
	write(filepath.Join(hwmonPath, "fan1_input"), "1800\n")
	write(filepath.Join(hwmonPath, "fan1_max"), "3000\n")
	write(filepath.Join(hwmonPath, "power1_input"), "120000000\n") // 120 W
	write(filepath.Join(hwmonPath, "power1_cap"), "200000000\n")   // 200 W

	// Clocks.
	write(filepath.Join(devPath, "pp_dpm_sclk"),
		"0: 300Mhz\n1: 600Mhz\n7: 1411Mhz *\n")
	write(filepath.Join(devPath, "pp_dpm_mclk"),
		"0: 300Mhz\n1: 2000Mhz *\n")

	// PCIe.
	write(filepath.Join(devPath, "current_link_speed"), "8.0 GT/s PCIe\n")
	write(filepath.Join(devPath, "current_link_width"), "16\n")
	write(filepath.Join(devPath, "max_link_speed"), "8.0 GT/s PCIe\n")
	write(filepath.Join(devPath, "max_link_width"), "16\n")

	// VBIOS / UUID.
	write(filepath.Join(devPath, "vbios_version"), "113-1E3871U-O4C\n")
	write(filepath.Join(devPath, "unique_id"), "deadbeef12345678\n")

	_ = gib
	return cardPath
}

func TestCollectAMDGPU_Fields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cardPath := buildAMDSysfs(t, dir)

	g := collectAMDGPU(cardPath, 2)

	if g.Index != 2 {
		t.Errorf("Index = %d, want 2", g.Index)
	}
	if g.DriverVersion != "amdgpu" {
		t.Errorf("DriverVersion = %q, want %q", g.DriverVersion, "amdgpu")
	}
	if g.ComputeMode != "AMD" {
		t.Errorf("ComputeMode = %q, want %q", g.ComputeMode, "AMD")
	}
	if g.PCIBusID != "0000:04:00.0" {
		t.Errorf("PCIBusID = %q, want %q", g.PCIBusID, "0000:04:00.0")
	}
	if g.GPUUtilPercent != 72 {
		t.Errorf("GPUUtilPercent = %v, want 72", g.GPUUtilPercent)
	}
	if g.MemoryUtilPercent != 38 {
		t.Errorf("MemoryUtilPercent = %v, want 38", g.MemoryUtilPercent)
	}
	if g.MemoryTotalMiB != 8192 {
		t.Errorf("MemoryTotalMiB = %d, want 8192", g.MemoryTotalMiB)
	}
	if g.MemoryUsedMiB != 2048 {
		t.Errorf("MemoryUsedMiB = %d, want 2048", g.MemoryUsedMiB)
	}
	if g.MemoryFreeMiB != 6144 {
		t.Errorf("MemoryFreeMiB = %d, want 6144", g.MemoryFreeMiB)
	}
	wantMemPct := float64(2048) / float64(8192) * 100
	if diff := g.MemoryPercent - wantMemPct; diff > 0.01 || diff < -0.01 {
		t.Errorf("MemoryPercent = %v, want %v", g.MemoryPercent, wantMemPct)
	}
	if g.TemperatureGPU != 65.0 {
		t.Errorf("TemperatureGPU = %v, want 65.0", g.TemperatureGPU)
	}
	wantFan := float64(1800) / float64(3000) * 100
	if diff := g.FanSpeedPercent - wantFan; diff > 0.01 || diff < -0.01 {
		t.Errorf("FanSpeedPercent = %v, want %v", g.FanSpeedPercent, wantFan)
	}
	if g.PowerDrawW != 120.0 {
		t.Errorf("PowerDrawW = %v, want 120.0", g.PowerDrawW)
	}
	if g.PowerLimitW != 200.0 {
		t.Errorf("PowerLimitW = %v, want 200.0", g.PowerLimitW)
	}
	if g.ClockGraphicsMHz != 1411 {
		t.Errorf("ClockGraphicsMHz = %d, want 1411", g.ClockGraphicsMHz)
	}
	if g.ClockMemoryMHz != 2000 {
		t.Errorf("ClockMemoryMHz = %d, want 2000", g.ClockMemoryMHz)
	}
	if g.PCIeGenCurrent != 3 {
		t.Errorf("PCIeGenCurrent = %d, want 3", g.PCIeGenCurrent)
	}
	if g.PCIeWidthCurrent != 16 {
		t.Errorf("PCIeWidthCurrent = %d, want 16", g.PCIeWidthCurrent)
	}
	if g.VBIOSVersion != "113-1E3871U-O4C" {
		t.Errorf("VBIOSVersion = %q, want %q", g.VBIOSVersion, "113-1E3871U-O4C")
	}
	if g.UUID != "deadbeef12345678" {
		t.Errorf("UUID = %q, want %q", g.UUID, "deadbeef12345678")
	}
}

// TestCollectAMDGPU_DefaultName verifies that "AMD GPU" is used when no
// sysfs label is present.
func TestCollectAMDGPU_DefaultName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cardPath := buildAMDSysfs(t, dir)
	// No label file written → name should default to "AMD GPU".
	g := collectAMDGPU(cardPath, 0)
	if g.Name != "AMD GPU" {
		t.Errorf("Name = %q, want %q", g.Name, "AMD GPU")
	}
}
