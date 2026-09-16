//go:build !nvml

// NVIDIA GPU collection without NVML, which is the default build.
//
// NVML has no pure-Go implementation, so importing it unconditionally made
// `CGO_ENABLED=0 go build` fail for every consumer of pkg/collector whether it
// wanted GPU metrics or not -- which is what blocked embedding this collector in
// a static, cross-compiled or FIPS binary (#8).
//
// This is NOT a stub. An NVIDIA card still has an identity and a link in sysfs,
// and its model name resolves from the hwdata pci.ids database, so the default
// build reports the card rather than pretending there is none. What it cannot
// report is everything only the driver's own interface knows -- memory,
// utilisation, clocks, ECC -- and those stay at their zero values, which is
// already how this collector says "unavailable" for AMD and Intel.
//
// The binaries this project ships are built WITH the tag, so go-sysmon's own
// dashboards are unchanged.

package collector

import (
	"context"
	"path/filepath"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// collectNvidiaGPUs reports each NVIDIA card from sysfs alone.
//
// It takes the same batched shape as the NVML implementation it replaces -- one
// call for every NVIDIA card -- so the caller in gpu.go is identical under both
// tags and neither build has a special case.
func (c *GPUCollector) collectNvidiaGPUs(ctx context.Context, cards []drmCard) []types.GPUInfo {
	// Said once per process rather than per cycle: the build cannot change while
	// it runs, and an operator wondering why the memory and utilisation columns
	// are empty needs the reason exactly once.
	warnOnce(c.logger, "gpu:nvml:absent",
		"built without the nvml tag: NVIDIA cards report identity, link and any hwmon readings, but not memory, utilisation, clocks or ECC",
		"cards", len(cards),
		"fix", "build with -tags nvml for full NVIDIA support")

	gpus := make([]types.GPUInfo, 0, len(cards))
	for _, card := range cards {
		devPath := filepath.Join(card.cardPath, "device")

		g := types.GPUInfo{Index: card.index}
		g.PCIBusID = sysfsPCIBusID(devPath)

		// The same identity resolution the accelerator collector uses: pci.ids
		// names the model, and the firmware label then the driver stand in when
		// that database is not installed.
		vendorID := readSysfsString(filepath.Join(devPath, "vendor"))
		deviceID := readSysfsString(filepath.Join(devPath, "device"))
		vendor, name, _ := lookupPCINames(vendorID, deviceID, "")
		switch {
		case name != "":
			g.Name = name
		case sysfsDeviceName(devPath) != "":
			g.Name = sysfsDeviceName(devPath)
		case vendor != "":
			g.Name = vendor + " GPU"
		default:
			g.Name = "NVIDIA GPU"
		}
		// DriverVersion carries the bound driver rather than a version: without
		// NVML there is no version to report, and naming the driver at least
		// tells nvidia from nouveau, which is what decides whether the hwmon
		// readings below exist.
		g.DriverVersion = sysfsDriverName(devPath)
		g.ComputeMode = "NVIDIA"

		g.PCIeGenCurrent = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "current_link_speed")))
		g.PCIeGenMax = pcieGenFromSpeed(readSysfsString(filepath.Join(devPath, "max_link_speed")))
		g.PCIeWidthCurrent = pcieLinkWidth(readSysfsUint64(filepath.Join(devPath, "current_link_width")))
		g.PCIeWidthMax = pcieLinkWidth(readSysfsUint64(filepath.Join(devPath, "max_link_width")))

		// nouveau registers an hwmon node; the proprietary driver does not. Where
		// it exists these are real readings, so they are taken rather than left
		// to NVML-only fields.
		if hwmonPath := sysfsHwmonPath(devPath); hwmonPath != "" {
			g.TemperatureGPU = float64(readSysfsUint64(filepath.Join(hwmonPath, "temp1_input"))) / 1000.0
			g.PowerDrawW = float64(readSysfsUint64(filepath.Join(hwmonPath, "power1_input"))) / 1_000_000.0
			if fanMax := readSysfsUint64(filepath.Join(hwmonPath, "fan1_max")); fanMax > 0 {
				g.FanSpeedPercent = float64(readSysfsUint64(filepath.Join(hwmonPath, "fan1_input"))) / float64(fanMax) * 100
			}
		}

		gpus = append(gpus, g)
	}
	return gpus
}
