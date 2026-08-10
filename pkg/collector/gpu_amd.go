package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// collectAMDGPU reads GPU metrics for an AMD amdgpu card from sysfs.
// cardPath is the /sys/class/drm/cardN directory. index is the DRM card index
// assigned by the parent collector. All reads are best-effort; missing or
// unreadable files contribute zero values.
func collectAMDGPU(cardPath string, index int) types.GPUInfo {
	devPath := filepath.Join(cardPath, "device")

	g := types.GPUInfo{
		Index:         index,
		DriverVersion: "amdgpu",
		ComputeMode:   "AMD",
	}

	// PCI bus ID from uevent.
	g.PCIBusID = amdPCIBusID(devPath)

	// Device name from sysfs label, falling back to a generic name.
	g.Name = sysfsDeviceName(devPath)
	if g.Name == "" {
		g.Name = "AMD GPU"
	}

	// Utilisation.
	g.GPUUtilPercent = float64(readSysfsUint64(filepath.Join(devPath, "gpu_busy_percent")))
	g.MemoryUtilPercent = float64(readSysfsUint64(filepath.Join(devPath, "mem_busy_percent")))

	// VRAM (bytes → MiB).
	vramTotal := readSysfsUint64(filepath.Join(devPath, "mem_info_vram_total"))
	vramUsed := readSysfsUint64(filepath.Join(devPath, "mem_info_vram_used"))
	g.MemoryTotalMiB = vramTotal / (1024 * 1024)
	g.MemoryUsedMiB = vramUsed / (1024 * 1024)
	if g.MemoryTotalMiB > 0 {
		g.MemoryFreeMiB = g.MemoryTotalMiB - g.MemoryUsedMiB
		g.MemoryPercent = float64(g.MemoryUsedMiB) / float64(g.MemoryTotalMiB) * 100
	}

	// Hwmon metrics: temperature, fan, power.
	hwmonPath := amdHwmonPath(devPath)
	if hwmonPath != "" {
		// Temperature: millidegrees → Celsius.
		rawTemp := readSysfsUint64(filepath.Join(hwmonPath, "temp1_input"))
		g.TemperatureGPU = float64(rawTemp) / 1000.0

		// Fan: fan1_input / fan1_max * 100.
		fanCurrent := readSysfsUint64(filepath.Join(hwmonPath, "fan1_input"))
		fanMax := readSysfsUint64(filepath.Join(hwmonPath, "fan1_max"))
		if fanMax > 0 {
			g.FanSpeedPercent = float64(fanCurrent) / float64(fanMax) * 100
		}

		// Power: microwatts → watts.
		rawPower := readSysfsUint64(filepath.Join(hwmonPath, "power1_input"))
		g.PowerDrawW = float64(rawPower) / 1_000_000.0

		rawCap := readSysfsUint64(filepath.Join(hwmonPath, "power1_cap"))
		g.PowerLimitW = float64(rawCap) / 1_000_000.0
	}

	// Clocks: pp_dpm_sclk / pp_dpm_mclk.
	sclkData, err := os.ReadFile(filepath.Join(devPath, "pp_dpm_sclk"))
	if err == nil {
		g.ClockGraphicsMHz = parseActiveDPMClock(sclkData)
	}
	mclkData, err := os.ReadFile(filepath.Join(devPath, "pp_dpm_mclk"))
	if err == nil {
		g.ClockMemoryMHz = parseActiveDPMClock(mclkData)
	}

	// PCIe link speed and width.
	g.PCIeGenCurrent = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "current_link_speed")))
	g.PCIeWidthCurrent = int(readSysfsUint64(filepath.Join(devPath, "current_link_width")))
	g.PCIeGenMax = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "max_link_speed")))
	g.PCIeWidthMax = int(readSysfsUint64(filepath.Join(devPath, "max_link_width")))

	// VBIOS and UUID.
	g.VBIOSVersion = readSysfsString(filepath.Join(devPath, "vbios_version"))
	g.UUID = readSysfsString(filepath.Join(devPath, "unique_id"))

	return g
}

// amdHwmonPath returns the first hwmon subdirectory found under
// devPath/hwmon/hwmon* or an empty string if none exists.
func amdHwmonPath(devPath string) string {
	pattern := filepath.Join(devPath, "hwmon", "hwmon*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// amdPCIBusID reads the PCI slot name from the device uevent file and returns
// it in "domain:bus:device.function" form (e.g. "0000:04:00.0").
func amdPCIBusID(devPath string) string {
	uevent := readSysfsString(filepath.Join(devPath, "uevent"))
	for _, line := range strings.Split(uevent, "\n") {
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			raw := strings.TrimPrefix(line, "PCI_SLOT_NAME=")
			return formatPCIBusID(strings.TrimSpace(raw))
		}
	}
	return ""
}

// formatPCIBusID ensures the bus ID has a 4-digit domain prefix, e.g.
// converts "4:00.0" → "0000:04:00.0" and passes through "0000:04:00.0"
// unchanged.
func formatPCIBusID(s string) string {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 2:
		// bus:device.function — prepend default domain.
		bus, err := strconv.ParseUint(parts[0], 16, 16)
		if err != nil {
			return s
		}
		return fmt.Sprintf("0000:%02x:%s", bus, parts[1])
	case 3:
		// domain:bus:device.function — pad domain to 4 digits.
		domain, err := strconv.ParseUint(parts[0], 16, 32)
		if err != nil {
			return s
		}
		bus, err := strconv.ParseUint(parts[1], 16, 16)
		if err != nil {
			return s
		}
		return fmt.Sprintf("%04x:%02x:%s", domain, bus, parts[2])
	default:
		return s
	}
}
