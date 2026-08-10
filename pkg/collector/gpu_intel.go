package collector

import (
	"path/filepath"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// collectIntelGPU reads GPU metrics for an Intel i915 card from sysfs.
// cardPath is the /sys/class/drm/cardN directory. index is the DRM card index
// assigned by the parent collector. All reads are best-effort; missing or
// unreadable files contribute zero values.
//
// Intel iGPUs expose only limited sysfs metrics. Temperature, fan speed, and
// power draw are generally not available via sysfs without additional kernel
// interfaces (e.g. Intel PMC or RAPL), so those fields are left at zero.
func collectIntelGPU(cardPath string, index int) types.GPUInfo {
	cardName := filepath.Base(cardPath) // e.g. "card1"
	devPath := filepath.Join(cardPath, "device")

	g := types.GPUInfo{
		Index:         index,
		DriverVersion: "i915",
		ComputeMode:   "Intel",
	}

	// PCI bus ID from uevent.
	g.PCIBusID = amdPCIBusID(devPath) // same uevent format as AMD

	// Device name from sysfs label, falling back to a generic name.
	g.Name = sysfsDeviceName(devPath)
	if g.Name == "" {
		g.Name = "Intel GPU"
	}

	// Clock frequencies via GT (graphics tile) sysfs nodes.
	// Path: /sys/class/drm/cardN/device/drm/cardN/gt_cur_freq_mhz
	gtBase := filepath.Join(devPath, "drm", cardName)
	g.ClockGraphicsMHz = intelGTFreqMHz(gtBase, "gt_cur_freq_mhz", "gt_act_freq_mhz")
	g.ClockMaxGfxMHz = uint32(readSysfsUint64(filepath.Join(gtBase, "gt_max_freq_mhz")))

	// PCIe link speed and width (same sysfs nodes as AMD).
	g.PCIeGenCurrent = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "current_link_speed")))
	g.PCIeWidthCurrent = int(readSysfsUint64(filepath.Join(devPath, "current_link_width")))
	g.PCIeGenMax = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "max_link_speed")))
	g.PCIeWidthMax = int(readSysfsUint64(filepath.Join(devPath, "max_link_width")))

	return g
}

// intelGTFreqMHz reads the current GT frequency from gtBase, trying each
// filename in order and returning the first non-zero value.
func intelGTFreqMHz(gtBase string, filenames ...string) uint32 {
	for _, name := range filenames {
		v := readSysfsUint64(filepath.Join(gtBase, name))
		if v > 0 {
			return uint32(v)
		}
	}
	return 0
}
