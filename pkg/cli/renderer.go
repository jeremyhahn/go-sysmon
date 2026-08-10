package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const separator = "═══════════════════════════════════════════════════"

// RenderOverview writes a summary of all system metrics to w.
func RenderOverview(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	host := snap.Host
	cpu := snap.CPUSummary
	mem := snap.Memory
	la := snap.LoadAvg
	proc := snap.Processes

	fmt.Fprintf(w, "System Monitor - %s\n", host.Hostname)
	fmt.Fprintln(w, separator)

	// Host line
	fmt.Fprintf(w, "%-12s%s (%s %s, %s, %s)\n",
		"Host:",
		host.Hostname,
		host.Platform,
		host.PlatformVersion,
		host.KernelVersion,
		host.KernelArch,
	)
	if host.BoardName != "" {
		boardStr := host.BoardName
		if host.BoardVendor != "" {
			boardStr = host.BoardVendor + " " + boardStr
		}
		if host.BoardVersion != "" {
			boardStr += " (v" + host.BoardVersion + ")"
		}
		fmt.Fprintf(w, "%-12s%s\n", "Board:", boardStr)
	}
	if host.BIOSVersion != "" {
		biosStr := host.BIOSVersion
		if host.BIOSVendor != "" {
			biosStr = host.BIOSVendor + " " + biosStr
		}
		if host.BIOSDate != "" {
			biosStr += " (" + host.BIOSDate + ")"
		}
		fmt.Fprintf(w, "%-12s%s\n", "BIOS:", biosStr)
	}
	fmt.Fprintf(w, "%-12s%s\n", "Uptime:", formatDuration(host.Uptime))
	fmt.Fprintf(w, "%-12s%.2f  %.2f  %.2f\n\n", "Load Avg:", la.Load1, la.Load5, la.Load15)

	// CPU block - use first CPU entry for model if available
	model := "Unknown"
	if len(snap.CPUs) > 0 {
		model = snap.CPUs[0].ModelName
	}
	avgUsage := averageCPUUsage(snap.CPUs)
	fmt.Fprintf(w, "%-12s%s\n", "CPU:", model)
	fmt.Fprintf(w, "%-12s%d Cores / %d Threads | Usage: %s\n\n",
		"",
		cpu.TotalCores,
		cpu.TotalThreads,
		formatPercent(avgUsage),
	)

	// Memory block
	swapStr := fmt.Sprintf("%s / %s", formatBytes(mem.SwapUsedBytes), formatBytes(mem.SwapTotalBytes))
	fmt.Fprintf(w, "%-12s%s / %s (%s) | Swap: %s\n",
		"Memory:",
		formatBytes(mem.UsedBytes),
		formatBytes(mem.TotalBytes),
		formatPercent(mem.UsedPercent),
		swapStr,
	)
	if len(mem.DIMMs) > 0 {
		d := mem.DIMMs[0]
		fmt.Fprintf(w, "%-12s%dx %s @ %d MT/s (%s)\n\n",
			"",
			len(mem.DIMMs),
			d.Type,
			d.SpeedMTs,
			d.Manufacturer,
		)
	} else {
		fmt.Fprintln(w)
	}

	// Storage block
	totalDiskBytes, usedDiskBytes := aggregateDiskUsage(snap.Disks)
	diskPct := 0.0
	if totalDiskBytes > 0 {
		diskPct = float64(usedDiskBytes) / float64(totalDiskBytes) * 100
	}
	allHealthy := allDisksHealthy(snap.Disks)
	healthStr := "All healthy"
	if !allHealthy {
		healthStr = "Issues detected"
	}
	driveNames := diskNames(snap.Disks)
	fmt.Fprintf(w, "%-12s%d disks | Total: %s | Used: %s (%s)\n",
		"Storage:",
		len(snap.Disks),
		formatBytes(totalDiskBytes),
		formatBytes(usedDiskBytes),
		formatPercent(diskPct),
	)
	fmt.Fprintf(w, "%-12s%s | Drives: %s\n\n",
		"",
		healthStr,
		driveNames,
	)

	// GPU block
	if len(snap.GPUs) > 0 {
		fmt.Fprintf(w, "%-12s%d GPUs\n", "GPU:", len(snap.GPUs))
		for _, g := range snap.GPUs {
			memStr := fmt.Sprintf("%d / %d MiB", g.MemoryUsedMiB, g.MemoryTotalMiB)
			eccStr := "off"
			if g.ECCEnabled {
				eccStr = "on"
			}
			fmt.Fprintf(w, "%-12s%s | Util: %.0f%% | Mem: %s | Temp: %.0f°C | ECC: %s\n",
				"",
				g.Name,
				g.GPUUtilPercent,
				memStr,
				g.TemperatureGPU,
				eccStr,
			)
		}
		fmt.Fprintln(w)
	}

	// Network block
	physCount, loCount := countNetworkTypes(snap.Networks)
	primary := primaryInterface(snap.Networks)
	primaryStr := "none"
	if primary != nil {
		primaryStr = fmt.Sprintf("%s (%d Mbps, %s)",
			primary.Name,
			primary.Speed,
			upString(primary.IsUp),
		)
	}
	fmt.Fprintf(w, "%-12s%d interfaces | %d physical | %d loopback\n",
		"Network:",
		len(snap.Networks),
		physCount,
		loCount,
	)
	fmt.Fprintf(w, "%-12sPrimary: %s\n\n", "", primaryStr)

	// Processes block
	fmt.Fprintf(w, "%-12s%d total | %d running | %d sleeping | %d idle | %d zombie\n",
		"Processes:",
		proc.Total,
		proc.Running,
		proc.Sleeping,
		proc.Idle,
		proc.Zombie,
	)

	s := snap.Sensors

	if len(s.ThermalZones) > 0 {
		parts := make([]string, 0, len(s.ThermalZones))
		for _, z := range s.ThermalZones {
			parts = append(parts, fmt.Sprintf("%s: %.1f°C (%s)", z.Name, z.TempCelsius, z.Type))
		}
		fmt.Fprintf(w, "%-12s%s\n", "Thermal:", strings.Join(parts, " | "))
	}

	if len(s.Fans) > 0 {
		parts := make([]string, 0, len(s.Fans))
		for _, f := range s.Fans {
			parts = append(parts, fmt.Sprintf("%s: %d RPM", f.Label, f.RPM))
		}
		fmt.Fprintf(w, "%-12s%s\n", "Fans:", strings.Join(parts, " | "))
	}

	psi := s.PSI
	fmt.Fprintf(w, "%-12sCPU: %.2f%% | Memory: %.2f%% | IO: %.2f%%\n",
		"PSI:",
		psi.CPU.SomeAvg10,
		psi.Memory.SomeAvg10,
		psi.IO.SomeAvg10,
	)

	return ew.Err()
}

// RenderHost writes detailed host information to w.
func RenderHost(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	h := snap.Host
	bootTime := time.Unix(int64(h.BootTime), 0).UTC()

	fmt.Fprintln(w, "Host Information")
	fmt.Fprintln(w, separator)
	fmt.Fprintf(w, "%-12s%s\n", "Hostname:", h.Hostname)
	fmt.Fprintf(w, "%-12s%s (%s)\n", "OS:", h.OS, h.Platform)
	fmt.Fprintf(w, "%-12s%s\n", "Version:", h.PlatformVersion)
	fmt.Fprintf(w, "%-12s%s (%s)\n", "Kernel:", h.KernelVersion, h.KernelArch)
	fmt.Fprintf(w, "%-12s%s\n", "Uptime:", formatDuration(h.Uptime))
	fmt.Fprintf(w, "%-12s%s\n", "Boot Time:", bootTime.Format("2006-01-02 15:04:05 UTC"))

	return ew.Err()
}

// RenderCPU writes CPU information with per-core usage to w.
func RenderCPU(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	sum := snap.CPUSummary
	cpus := snap.CPUs

	model := "Unknown"
	vendor := "Unknown"
	cacheSize := int32(0)
	microcode := ""
	if len(cpus) > 0 {
		model = cpus[0].ModelName
		vendor = cpus[0].VendorID
		cacheSize = cpus[0].CacheSize
		microcode = cpus[0].Microcode
	}

	fmt.Fprintln(w, "CPU Information")
	fmt.Fprintln(w, separator)
	fmt.Fprintf(w, "%-16s%s\n", "Model:", model)
	fmt.Fprintf(w, "%-16s%s\n", "Vendor:", vendor)
	fmt.Fprintf(w, "%-16s%d Sockets x %d Core/Socket x %d Thread/Core\n",
		"Topology:", sum.Sockets, sum.CoresPerSocket, sum.ThreadsPerCore)
	fmt.Fprintf(w, "%-16s%d Cores / %d Threads\n", "Total:", sum.TotalCores, sum.TotalThreads)
	fmt.Fprintf(w, "%-16s%.0f - %.0f MHz\n", "Frequency:", sum.MinMHz, sum.MaxMHz)
	fmt.Fprintf(w, "%-16s%d KB\n", "Cache:", cacheSize)
	if microcode != "" {
		fmt.Fprintf(w, "%-16s%s\n", "Microcode:", microcode)
	}

	if len(cpus) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Per-Core Usage:")
		for _, c := range cpus {
			bar := formatBar(c.UsagePercent, 20)
			if c.TemperatureCelsius > 0 {
				fmt.Fprintf(w, "  Core %2d:  %s  %5.1f%%  %.1f°C\n", c.Index, bar, c.UsagePercent, c.TemperatureCelsius)
			} else {
				fmt.Fprintf(w, "  Core %2d:  %s  %5.1f%%\n", c.Index, bar, c.UsagePercent)
			}
		}
		avg := averageCPUUsage(cpus)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%-16s%s\n", "Average:", formatPercent(avg))
	}

	s := snap.Sensors

	if len(s.CoreTemps) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "CPU Temperatures:")
		fmt.Fprintf(w, "  %4s  %4s  %-10s  %8s  %8s  %8s\n", "Pkg", "Core", "Label", "Temp", "High", "Crit")
		for _, t := range s.CoreTemps {
			highStr := "-"
			critStr := "-"
			if t.HighCelsius > 0 {
				highStr = fmt.Sprintf("%.1f°C", t.HighCelsius)
			}
			if t.CritCelsius > 0 {
				critStr = fmt.Sprintf("%.1f°C", t.CritCelsius)
			}
			fmt.Fprintf(w, "  %4d  %4d  %-10s  %7.1f°C  %8s  %8s\n",
				t.PackageID, t.CoreID, t.Label, t.TempCelsius, highStr, critStr)
		}
	}

	if len(s.CoreVoltages) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "CPU Voltages:")
		fmt.Fprintf(w, "  %7s  %-10s  %9s  %s\n", "Channel", "Label", "Voltage", "Source")
		for _, v := range s.CoreVoltages {
			fmt.Fprintf(w, "  %7d  %-10s  %7.3f V  %s\n",
				v.Channel, v.Label, v.VoltageV, v.HwmonName)
		}
	}

	if len(s.PackagePower) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Package Power (RAPL):")
		fmt.Fprintf(w, "  %-16s  %8s  %8s\n", "Package", "Power", "Max")
		for _, p := range s.PackagePower {
			maxStr := "-"
			if p.MaxPowerW > 0 {
				maxStr = fmt.Sprintf("%.1f W", p.MaxPowerW)
			}
			fmt.Fprintf(w, "  %-16s  %6.1f W  %8s\n", p.PackageName, p.PowerW, maxStr)
		}
	}

	hasThrottle := false
	for _, th := range s.ThermalThrottle {
		if th.CoreThrottleCount > 0 || th.PackageThrottleCount > 0 {
			hasThrottle = true
			break
		}
	}
	if hasThrottle {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Thermal Throttle Events:")
		fmt.Fprintf(w, "  %4s  %12s  %15s\n", "CPU", "Core Events", "Package Events")
		for _, th := range s.ThermalThrottle {
			fmt.Fprintf(w, "  %4d  %12d  %15d\n",
				th.CPU, th.CoreThrottleCount, th.PackageThrottleCount)
		}
	}

	return ew.Err()
}

// RenderMemory writes memory information including DIMMs to w.
func RenderMemory(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	m := snap.Memory

	fmt.Fprintln(w, "Memory Information")
	fmt.Fprintln(w, separator)
	fmt.Fprintf(w, "%-12s%s used / %s total (%s)\n",
		"RAM:",
		formatBytes(m.UsedBytes),
		formatBytes(m.TotalBytes),
		formatPercent(m.UsedPercent),
	)
	fmt.Fprintf(w, "%-12s%s\n", "Available:", formatBytes(m.AvailableBytes))
	fmt.Fprintf(w, "%-12s%s\n", "Free:", formatBytes(m.FreeBytes))
	fmt.Fprintf(w, "%-12sBuffers: %s | Cached: %s | Shared: %s | Slab: %s\n",
		"",
		formatBytes(m.BuffersBytes),
		formatBytes(m.CachedBytes),
		formatBytes(m.SharedBytes),
		formatBytes(m.SlabBytes),
	)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-12s%s used / %s total (%s)\n",
		"Swap:",
		formatBytes(m.SwapUsedBytes),
		formatBytes(m.SwapTotalBytes),
		formatPercent(m.SwapPercent),
	)

	if len(m.DIMMs) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Memory Modules (DIMMs):")

		headers := []string{"Slot", "Bank", "Size", "Type", "Speed", "Configured", "Rank", "Voltage", "Temp", "Manufacturer", "Part Number"}
		rows := make([][]string, 0, len(m.DIMMs))
		totalCapacity := uint64(0)
		for _, d := range m.DIMMs {
			totalCapacity += d.SizeBytes
			tempStr := "-"
			if d.Temperature > 0 {
				tempStr = fmt.Sprintf("%.0f°C", d.Temperature)
			}
			rows = append(rows, []string{
				d.Location,
				d.BankLocator,
				formatBytes(d.SizeBytes),
				d.Type,
				fmt.Sprintf("%d MT/s", d.SpeedMTs),
				fmt.Sprintf("%d MT/s", d.ConfiguredSpeedMTs),
				fmt.Sprintf("%d", d.Rank),
				fmt.Sprintf("%.1fV", d.ConfiguredVoltage),
				tempStr,
				d.Manufacturer,
				d.PartNumber,
			})
		}
		// indent the table
		var sb strings.Builder
		formatTable(&sb, headers, rows)
		for _, line := range strings.Split(strings.TrimRight(sb.String(), "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintf(w, "Total Capacity: %s\n", formatBytes(totalCapacity))
	}

	return ew.Err()
}

// RenderStorage writes disk information with SMART health to w.
func RenderStorage(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	fmt.Fprintln(w, "Storage Information")
	fmt.Fprintln(w, separator)

	for _, disk := range snap.Disks {
		fmt.Fprintln(w)
		renderDisk(w, disk)
	}

	return ew.Err()
}

// RenderNetwork writes network interface information to w.
func RenderNetwork(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	fmt.Fprintln(w, "Network Interfaces")
	fmt.Fprintln(w, separator)

	for _, iface := range snap.Networks {
		fmt.Fprintln(w)
		renderInterface(w, iface)
	}

	return ew.Err()
}

// RenderGPU writes GPU information to w. When the snapshot contains no GPUs
// a brief message is printed instead.
func RenderGPU(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	fmt.Fprintln(w, "GPU Information")
	fmt.Fprintln(w, separator)

	if len(snap.GPUs) == 0 {
		fmt.Fprintln(w, "No GPUs detected.")
		return ew.Err()
	}

	for i, g := range snap.GPUs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		renderGPU(w, g)
	}
	return ew.Err()
}

// renderGPU writes formatted output for a single GPU.
func renderGPU(w io.Writer, g types.GPUInfo) {
	fmt.Fprintf(w, "\nGPU %d: %s\n", g.Index, g.Name)

	// Driver / VBIOS / compute mode / perf state
	fmt.Fprintf(w, "  %-14s%s | VBIOS: %s | Compute: %s | State: %s\n",
		"Driver:",
		g.DriverVersion,
		g.VBIOSVersion,
		g.ComputeMode,
		g.PerfState,
	)
	if g.UUID != "" {
		fmt.Fprintf(w, "  %-14s%s\n", "UUID:", g.UUID)
	}

	// PCIe topology
	fmt.Fprintf(w, "  %-14s%s (Gen%d x%d / Gen%d x%d)\n",
		"PCI:",
		g.PCIBusID,
		g.PCIeGenCurrent, g.PCIeWidthCurrent,
		g.PCIeGenMax, g.PCIeWidthMax,
	)

	// Utilisation section
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Utilization:")
	fmt.Fprintf(w, "    %-12s%s  %5.1f%%\n",
		"GPU:", formatBar(g.GPUUtilPercent, 20), g.GPUUtilPercent)
	fmt.Fprintf(w, "    %-12s%s  %5.1f%%\n",
		"Memory:", formatBar(g.MemoryUtilPercent, 20), g.MemoryUtilPercent)
	fmt.Fprintf(w, "    %-12s%s  %5.1f%%\n",
		"Encoder:", formatBar(g.EncoderPercent, 20), g.EncoderPercent)
	fmt.Fprintf(w, "    %-12s%s  %5.1f%%\n",
		"Decoder:", formatBar(g.DecoderPercent, 20), g.DecoderPercent)

	// Memory section
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-14s%d MiB / %d MiB (%.1f%%)\n",
		"Memory:",
		g.MemoryUsedMiB,
		g.MemoryTotalMiB,
		g.MemoryPercent,
	)
	fmt.Fprintf(w, "  %s\n", strings.Repeat(" ", 16)+formatBar(g.MemoryPercent, 20))

	// Thermals
	fmt.Fprintln(w)
	if g.TemperatureMemory > 0 {
		fmt.Fprintf(w, "  %-14s%.0f°C (GPU) | %.0f°C (Mem) | Fan: %.0f%%\n",
			"Temperature:", g.TemperatureGPU, g.TemperatureMemory, g.FanSpeedPercent)
	} else {
		fmt.Fprintf(w, "  %-14s%.0f°C (GPU) | Fan: %.0f%%\n",
			"Temperature:", g.TemperatureGPU, g.FanSpeedPercent)
	}

	// Power
	powerPct := 0.0
	if g.PowerLimitW > 0 {
		powerPct = g.PowerDrawW / g.PowerLimitW * 100
	}
	fmt.Fprintf(w, "  %-14s%.1f W / %.1f W (%.1f%%)\n",
		"Power:", g.PowerDrawW, g.PowerLimitW, powerPct)
	fmt.Fprintf(w, "  %s\n", strings.Repeat(" ", 16)+formatBar(powerPct, 20))

	// Clocks
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-14sGraphics: %d / %d MHz | Memory: %d / %d MHz | Video: %d MHz\n",
		"Clocks:",
		g.ClockGraphicsMHz, g.ClockMaxGfxMHz,
		g.ClockMemoryMHz, g.ClockMaxMemMHz,
		g.ClockVideoMHz,
	)

	// PCIe throughput
	fmt.Fprintf(w, "  %-14s↑ %.0f MB/s TX | ↓ %.0f MB/s RX\n",
		"PCIe:", g.PCIeTxMBps, g.PCIeRxMBps)

	// ECC
	eccStr := "Disabled"
	if g.ECCEnabled {
		eccStr = fmt.Sprintf("Enabled (SBE: %d, DBE: %d)", g.ECCSingleBit, g.ECCDoubleBit)
	}
	fmt.Fprintf(w, "  %-14s%s\n", "ECC:", eccStr)
}

// renderDisk writes formatted output for a single disk.
func renderDisk(w io.Writer, d types.DiskInfo) {
	// Header line
	typeStr := diskTypeString(d)
	fwStr := ""
	if d.FirmwareVersion != "" {
		fwStr = fmt.Sprintf(", FW: %s", d.FirmwareVersion)
	}
	nvmeStr := ""
	if d.NVMeVersion != "" {
		nvmeStr = fmt.Sprintf("NVMe %s", d.NVMeVersion)
	}
	headerDetail := typeStr
	if nvmeStr != "" {
		headerDetail = nvmeStr + ", FW: " + d.FirmwareVersion
		fwStr = ""
	}
	_ = fwStr
	fmt.Fprintf(w, "/dev/%s - %s (%s%s)\n", d.Name, d.Model, headerDetail, fwStr)

	// Type and size
	fmt.Fprintf(w, "  %-14s%s | Size: %s\n", "Type:", typeStr, formatBytes(d.SizeBytes))

	// Usage bar
	bar := formatBar(d.UsedPercent, 20)
	fmt.Fprintf(w, "  %-14s%s  %s (%s / %s)\n",
		"Usage:",
		bar,
		formatPercent(d.UsedPercent),
		formatBytes(d.UsedBytes),
		formatBytes(d.TotalBytes),
	)

	// Live I/O and queue depth, with the peaks seen since startup. Peaks are
	// what expose a burst that the current sample happens to fall between.
	fmt.Fprintf(w, "  %-14sread %s/s | write %s/s | queue %d | util %s\n",
		"I/O:",
		formatBytes(d.ReadBytesRate),
		formatBytes(d.WriteBytesRate),
		d.QueueLength,
		formatPercent(d.UtilPercent),
	)
	fmt.Fprintf(w, "  %-14sread %s/s | write %s/s | queue %d | util %s | used %s\n",
		"Peak:",
		formatBytes(d.PeakReadBytesRate),
		formatBytes(d.PeakWriteBytesRate),
		d.PeakQueueLength,
		formatPercent(d.PeakUtilPercent),
		formatPercent(d.PeakUsedPercent),
	)
	if d.AvgQueueLength > 0 {
		fmt.Fprintf(w, "  %-14s%.2f\n", "Avg queue:", d.AvgQueueLength)
	}

	// SMART health
	if d.SMARTEnabled {
		healthLabel := "Healthy"
		healthMark := "✓"
		if !d.SMARTHealthy {
			healthLabel = "FAILED"
			healthMark = "✗"
		}
		fmt.Fprintf(w, "  %-14s%s %s\n", "SMART:", healthLabel, healthMark)
	}

	// NVMe-specific metrics
	if d.WearLevelPercent > 0 || d.AvailableSparePercent > 0 {
		wearBar := formatBar(float64(d.WearLevelPercent), 20)
		fmt.Fprintf(w, "  %-14s%s  %3d%% used (%d%% remaining)\n",
			"Wear Level:",
			wearBar,
			d.WearLevelPercent,
			100-d.WearLevelPercent,
		)
		fmt.Fprintf(w, "  %-14s%d%% available (threshold: %d%%)\n",
			"Spare:",
			d.AvailableSparePercent,
			d.SpareThresholdPercent,
		)
	}

	if d.EstimatedHoursLeft > 0 {
		fmt.Fprintf(w, "  %-14s%s remaining\n", "Est. Life:", formatHours(d.EstimatedHoursLeft))
	}

	if d.Temperature > 0 {
		fmt.Fprintf(w, "  %-14s%.0f°C\n", "Temperature:", d.Temperature)
	}

	if d.PowerOnHours > 0 {
		fmt.Fprintf(w, "  %-14s%d hours (%d days)\n",
			"Power On:",
			d.PowerOnHours,
			d.PowerOnHours/24,
		)
	}

	if d.PowerCycles > 0 {
		fmt.Fprintf(w, "  %-14s%d\n", "Power Cycles:", d.PowerCycles)
	}

	if d.DataUnitsWritten > 0 || d.DataUnitsRead > 0 {
		fmt.Fprintf(w, "  %-14s%s | Data Read: %s\n",
			"Data Written:",
			formatDataUnits(d.DataUnitsWritten),
			formatDataUnits(d.DataUnitsRead),
		)
	}

	fmt.Fprintf(w, "  %-14s%d | Log Entries: %d | Unsafe Shutdowns: %d\n",
		"Media Errors:",
		d.MediaErrors,
		d.ErrorLogEntries,
		d.UnsafeShutdowns,
	)

	if len(d.Partitions) > 0 {
		fmt.Fprintf(w, "  Partitions:\n")
		for _, p := range d.Partitions {
			mp := p.Mountpoint
			if mp == "" {
				mp = "-"
			}
			dev := p.Device
			if len(dev) > 0 && dev[0] != '/' {
				dev = "/dev/" + dev
			}
			fmt.Fprintf(w, "    %-20s%-16s%-10s\n", dev, mp, p.Fstype)
		}
	}
}

// renderInterface writes formatted output for a single network interface.
func renderInterface(w io.Writer, iface types.NetworkInfo) {
	// Kind comes from sysfs and is more specific than the physical/virtual
	// split (bridge, bond, wifi, veth, tun, vlan).
	ifType := iface.Kind
	if ifType == "" {
		ifType = "virtual"
		if iface.IsLoopback {
			ifType = "loopback"
		} else if !iface.IsVirtual {
			ifType = "ethernet"
		}
	}

	upLabel := "DOWN"
	if iface.IsUp {
		upLabel = "UP"
	}
	// IsUp is the administrative IFF_UP flag; OperState is what the link is
	// actually doing, so a cable-less NIC reads "UP/down".
	if iface.OperState != "" && iface.OperState != "up" {
		upLabel = fmt.Sprintf("%s/%s", upLabel, iface.OperState)
	}

	fmt.Fprintf(w, "%s [%s] - %s\n", iface.Name, upLabel, ifType)

	if iface.HardwareAddr != "" {
		fmt.Fprintf(w, "  %-12s%s\n", "MAC:", iface.HardwareAddr)
	}

	if len(iface.Addresses) > 0 {
		fmt.Fprintf(w, "  %-12s%s\n", "IPs:", strings.Join(iface.Addresses, ", "))
	}

	if len(iface.DNSServers) > 0 {
		fmt.Fprintf(w, "  %-12s%s\n", "DNS:", strings.Join(iface.DNSServers, ", "))
	}
	if len(iface.DNSSearch) > 0 {
		fmt.Fprintf(w, "  %-12s%s\n", "Search:", strings.Join(iface.DNSSearch, ", "))
	}

	if len(iface.BridgePorts) > 0 {
		fmt.Fprintf(w, "  %-12s%s\n", "Ports:", strings.Join(iface.BridgePorts, ", "))
	}
	if iface.BondMode != "" {
		fmt.Fprintf(w, "  %-12s%s\n", "Bond mode:", iface.BondMode)
	}
	if len(iface.BondSlaves) > 0 {
		fmt.Fprintf(w, "  %-12s%s\n", "Slaves:", strings.Join(iface.BondSlaves, ", "))
	}
	if iface.BondMaster != "" {
		fmt.Fprintf(w, "  %-12s%s\n", "Master:", iface.BondMaster)
	}

	if iface.Wireless != nil {
		fmt.Fprintf(w, "  %-12squality %.0f | signal %.0f dBm | noise %.0f dBm\n",
			"Wireless:",
			iface.Wireless.LinkQuality,
			iface.Wireless.SignalLevelDBm,
			iface.Wireless.NoiseLevelDBm,
		)
	}

	if iface.Speed > 0 {
		duplexStr := ""
		if iface.Duplex != "" {
			duplexStr = fmt.Sprintf(" (%s duplex)", iface.Duplex)
		}
		fmt.Fprintf(w, "  %-12s%d Mbps%s\n", "Speed:", iface.Speed, duplexStr)
	}

	if iface.Driver != "" {
		fmt.Fprintf(w, "  %-12s%s\n", "Driver:", iface.Driver)
	}

	fmt.Fprintf(w, "  %-12s%d\n", "MTU:", iface.MTU)

	fmt.Fprintf(w, "  %-12s↑ %s sent (%s pkts) | ↓ %s recv (%s pkts)\n",
		"Traffic:",
		formatBytes(iface.BytesSent),
		formatNumber(iface.PacketsSent),
		formatBytes(iface.BytesRecv),
		formatNumber(iface.PacketsRecv),
	)

	fmt.Fprintf(w, "  %-12s%d in / %d out | Drops: %d in / %d out\n",
		"Errors:",
		iface.ErrorsIn,
		iface.ErrorsOut,
		iface.DropsIn,
		iface.DropsOut,
	)
}

// averageCPUUsage computes the mean usage across all CPU entries.
func averageCPUUsage(cpus []types.CPUInfo) float64 {
	if len(cpus) == 0 {
		return 0
	}
	total := 0.0
	for _, c := range cpus {
		total += c.UsagePercent
	}
	return total / float64(len(cpus))
}

// aggregateDiskUsage sums total and used bytes across all disks.
func aggregateDiskUsage(disks []types.DiskInfo) (total, used uint64) {
	for _, d := range disks {
		total += d.TotalBytes
		used += d.UsedBytes
	}
	return total, used
}

// allDisksHealthy returns true when all SMART-enabled disks report healthy.
func allDisksHealthy(disks []types.DiskInfo) bool {
	for _, d := range disks {
		if d.SMARTEnabled && !d.SMARTHealthy {
			return false
		}
	}
	return true
}

// diskNames returns the space-separated device names for all disks.
func diskNames(disks []types.DiskInfo) string {
	names := make([]string, len(disks))
	for i, d := range disks {
		names[i] = d.Name
	}
	return strings.Join(names, " ")
}

// countNetworkTypes counts physical and loopback interfaces.
func countNetworkTypes(ifaces []types.NetworkInfo) (physical, loopback int) {
	for _, iface := range ifaces {
		switch {
		case iface.IsLoopback:
			loopback++
		case !iface.IsVirtual:
			physical++
		}
	}
	return physical, loopback
}

// primaryInterface returns the first non-loopback, non-virtual UP interface.
func primaryInterface(ifaces []types.NetworkInfo) *types.NetworkInfo {
	for i := range ifaces {
		iface := &ifaces[i]
		if iface.IsUp && !iface.IsLoopback && !iface.IsVirtual {
			return iface
		}
	}
	return nil
}

// upString converts a boolean up/down state to a string.
func upString(up bool) string {
	if up {
		return "UP"
	}
	return "DOWN"
}

// diskTypeString returns a human-readable disk type string.
func diskTypeString(d types.DiskInfo) string {
	if d.DriveType != "" {
		return d.DriveType
	}
	if d.Rotational {
		return "HDD"
	}
	return "SSD/NVMe"
}

// formatNumber formats an integer with comma separators.
func formatNumber(n uint64) string {
	s := fmt.Sprintf("%d", n)
	result := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// RenderContainers writes the containers running on the host to w.
func RenderContainers(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	fmt.Fprintln(w, "Containers")
	fmt.Fprintln(w, separator)

	v := snap.Virt
	if len(v.Containers) == 0 {
		if v.CgroupVersion == "" {
			fmt.Fprintln(w, "No cgroup filesystem found; container metrics are unavailable.")
		} else {
			fmt.Fprintln(w, "No containers detected.")
		}
		return ew.Err()
	}

	fmt.Fprintf(w, "%-12s%d running | cgroup %s | runtime %s\n\n",
		"Summary:", len(v.Containers), v.CgroupVersion, strings.Join(v.Runtimes, ", "))

	rows := make([][]string, 0, len(v.Containers))
	for _, c := range v.Containers {
		memStr := formatBytes(c.MemoryBytes)
		if c.MemoryLimitBytes > 0 {
			memStr = fmt.Sprintf("%s / %s (%s)",
				formatBytes(c.MemoryBytes),
				formatBytes(c.MemoryLimitBytes),
				formatPercent(c.MemoryPercent))
		}
		limitStr := "-"
		if c.CPULimitCores > 0 {
			limitStr = fmt.Sprintf("%.2g core", c.CPULimitCores)
		}
		throttleStr := "-"
		if c.ThrottledPercent > 0 {
			throttleStr = formatPercent(c.ThrottledPercent)
		}

		rows = append(rows, []string{
			strconv.Itoa(c.Index),
			c.Name,
			c.ShortID,
			formatPercent(c.CPUPercent),
			limitStr,
			throttleStr,
			memStr,
			formatBytes(c.MemoryPeakBytes),
			strconv.FormatUint(c.PIDs, 10),
			formatBytes(c.ReadBytesRate) + "/s",
			formatBytes(c.WriteBytesRate) + "/s",
			formatDuration(c.UptimeSeconds),
		})
	}
	formatTable(w, []string{
		"IDX", "NAME", "ID", "CPU", "LIMIT", "THROTTLED",
		"MEMORY", "PEAK", "PIDS", "READ", "WRITE", "UPTIME",
	}, rows)
	fmt.Fprintln(w, "\nCPU is a percentage of one core, as docker stats reports it:")
	fmt.Fprintln(w, "200% means two cores busy, not two hundred percent of the machine.")

	// Anything that explains a slow or restarting container is called out
	// rather than left for the reader to spot in the table.
	renderContainerWarnings(w, v.Containers)

	return ew.Err()
}

// renderContainerWarnings prints the conditions that explain degraded
// behaviour: throttling, OOM kills, stall pressure and swap.
func renderContainerWarnings(w io.Writer, containers []types.ContainerInfo) {
	var lines []string

	for _, c := range containers {
		if c.ThrottledPercent > 0 {
			lines = append(lines, fmt.Sprintf(
				"  %-14s CPU throttled in %s of periods (%d/%d), %.1fs stopped",
				c.Name, formatPercent(c.ThrottledPercent), c.NrThrottled, c.NrPeriods,
				float64(c.ThrottledUsec)/1e6))
		}
		if c.OOMKills > 0 {
			lines = append(lines, fmt.Sprintf(
				"  %-14s %d process(es) killed by the OOM killer", c.Name, c.OOMKills))
		}
		if c.MemoryLimitBytes > 0 && c.MemoryPeakBytes >= c.MemoryLimitBytes {
			lines = append(lines, fmt.Sprintf(
				"  %-14s peaked at its memory limit (%s)", c.Name, formatBytes(c.MemoryPeakBytes)))
		}
		if c.CPUPressure >= 5 {
			lines = append(lines, fmt.Sprintf(
				"  %-14s CPU stall pressure %s", c.Name, formatPercent(c.CPUPressure)))
		}
		if c.MemoryPressure >= 5 {
			lines = append(lines, fmt.Sprintf(
				"  %-14s memory stall pressure %s", c.Name, formatPercent(c.MemoryPressure)))
		}
		if c.IOPressure >= 5 {
			lines = append(lines, fmt.Sprintf(
				"  %-14s I/O stall pressure %s", c.Name, formatPercent(c.IOPressure)))
		}
		if c.SwapBytes > 0 {
			lines = append(lines, fmt.Sprintf(
				"  %-14s swapping (%s)", c.Name, formatBytes(c.SwapBytes)))
		}
	}

	if len(lines) == 0 {
		return
	}

	fmt.Fprintf(w, "\nAttention:\n")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// RenderVMs writes the virtual machines running on the host to w.
func RenderVMs(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	fmt.Fprintln(w, "Virtual Machines")
	fmt.Fprintln(w, separator)

	vms := snap.Virt.VMs
	if len(vms) == 0 {
		fmt.Fprintln(w, "No virtual machines detected.")
		return ew.Err()
	}

	for i, vm := range vms {
		if i > 0 {
			fmt.Fprintln(w)
		}
		renderVM(w, vm)
	}

	return ew.Err()
}

// renderVM writes a single virtual machine's detail block.
func renderVM(w io.Writer, vm types.VMInfo) {
	fmt.Fprintf(w, "%s [pid %d] - %s\n", vm.Name, vm.PID, vm.Hypervisor)

	if vm.UUID != "" {
		fmt.Fprintf(w, "  %-14s%s\n", "UUID:", vm.UUID)
	}
	if vm.Accelerator != "" {
		fmt.Fprintf(w, "  %-14s%s\n", "Accelerator:", vm.Accelerator)
	}
	if vm.VCPUs > 0 {
		fmt.Fprintf(w, "  %-14s%d\n", "vCPUs:", vm.VCPUs)
	}

	// Configured guest RAM against what the hypervisor actually occupies on
	// the host; the difference is guest memory not yet faulted in.
	fmt.Fprintf(w, "  %-14s%s configured | %s resident on host\n",
		"Memory:", formatBytes(vm.MemoryBytes), formatBytes(vm.RSSBytes))
	// CPUPercent is a percentage of one core, the convention top and docker
	// stats use, so a 24-vCPU guest can legitimately reach 2400. Lead with the
	// share of the guest's own allocation, which lands on a 0-100 scale.
	if vm.VCPUs > 0 {
		fmt.Fprintf(w, "  %-14s%s of %d vCPUs (%.1f cores busy)\n",
			"CPU:", formatPercent(vm.CPUPercent/float64(vm.VCPUs)), vm.VCPUs, vm.CPUPercent/100)
	} else {
		fmt.Fprintf(w, "  %-14s%.1f cores busy\n", "CPU:", vm.CPUPercent/100)
	}

	if len(vm.MACAddresses) > 0 {
		fmt.Fprintf(w, "  %-14s%s\n", "MAC:", strings.Join(vm.MACAddresses, ", "))
	}
	if vm.VCPUThreads > 0 {
		threadNote := ""
		if vm.VCPUs > 0 && vm.VCPUThreads != vm.VCPUs {
			threadNote = " (does not match the configured vCPU count)"
		}
		fmt.Fprintf(w, "  %-14s%d vCPU of %d total%s\n",
			"Threads:", vm.VCPUThreads, vm.ThreadCount, threadNote)
	}
	if vm.UptimeSeconds > 0 {
		fmt.Fprintf(w, "  %-14s%s\n", "Uptime:", formatDuration(vm.UptimeSeconds))
	}
	if len(vm.TapInterfaces) > 0 {
		fmt.Fprintf(w, "  %-14s%s (in %s/s, out %s/s)\n",
			"Host NICs:", strings.Join(vm.TapInterfaces, ", "),
			formatBytes(vm.NetRxRate), formatBytes(vm.NetTxRate))
	}
	if vm.CgroupPath != "" {
		fmt.Fprintf(w, "  %-14sread %s/s | write %s/s (%s / %s lifetime)\n",
			"Disk I/O:",
			formatBytes(vm.DiskReadRate), formatBytes(vm.DiskWriteRate),
			formatBytes(vm.DiskReadBytes), formatBytes(vm.DiskWriteBytes))
		fmt.Fprintf(w, "  %-14s%s current | %s peak (cgroup charge, includes host page cache)\n",
			"Guest memory:", formatBytes(vm.MemoryCurrentBytes), formatBytes(vm.MemoryPeakBytes))
		if vm.CPUPressure > 0 || vm.IOPressure > 0 {
			fmt.Fprintf(w, "  %-14sCPU %s | I/O %s\n",
				"Stalled:", formatPercent(vm.CPUPressure), formatPercent(vm.IOPressure))
		}
	}

	if len(vm.DiskImages) > 0 {
		sizeNote := ""
		if vm.DiskImageBytes > 0 {
			sizeNote = fmt.Sprintf(" (%s)", formatBytes(vm.DiskImageBytes))
		}
		fmt.Fprintf(w, "  %-14s%s%s\n", "Disks:", strings.Join(vm.DiskImages, ", "), sizeNote)
	}
}

// RenderImages writes container image inventory and runtime storage usage to w.
func RenderImages(w io.Writer, snap *types.Snapshot) error {
	ew := newErrWriter(w)
	w = ew

	if snap == nil {
		return &types.CollectorError{Collector: "cli", Cause: errNilSnapshot}
	}

	fmt.Fprintln(w, "Container Images")
	fmt.Fprintln(w, separator)

	r := snap.Virt.Runtime
	if !r.Available {
		fmt.Fprintln(w, "No container runtime API reachable.")
		fmt.Fprintln(w, "Image inventory lives in the runtime's database; its storage")
		fmt.Fprintln(w, "directory is root-owned, so there is no unprivileged alternative.")
		return ew.Err()
	}

	// Where everything lives, which is the first thing to check when a
	// filesystem fills up: the root directory is frequently relocated.
	fmt.Fprintf(w, "%-14s%s %s\n", "Engine:", r.Engine, r.Version)
	fmt.Fprintf(w, "%-14s%s\n", "Storage:", r.RootDir)
	fmt.Fprintf(w, "%-14s%s on %s\n", "Driver:", r.StorageDriver, r.BackingFilesystem)
	fmt.Fprintf(w, "%-14s%d running, %d stopped, %d paused (%d total)\n\n",
		"Containers:", r.ContainersRunning, r.ContainersStopped, r.ContainersPaused, r.ContainersTotal)

	// Space accounting, with what a prune would return.
	usageRows := [][]string{
		{"Images", strconv.Itoa(r.ImagesTotal), formatBytes(r.LayersBytes),
			fmt.Sprintf("%s (%d unused, %d dangling)", formatBytes(r.ReclaimableBytes), r.UnusedImages, r.DanglingImages)},
		{"Volumes", strconv.Itoa(r.VolumesCount), formatBytes(r.VolumesBytes),
			fmt.Sprintf("%s (%d unused)", formatBytes(r.VolumesReclaimableBytes), r.VolumesUnused)},
		{"Build cache", strconv.Itoa(r.BuildCacheEntries), formatBytes(r.BuildCacheBytes),
			formatBytes(r.BuildCacheReclaimableBytes)},
	}
	formatTable(w, []string{"TYPE", "COUNT", "SIZE", "RECLAIMABLE"}, usageRows)

	total := r.LayersBytes + r.VolumesBytes + r.BuildCacheBytes
	reclaim := r.ReclaimableBytes + r.VolumesReclaimableBytes + r.BuildCacheReclaimableBytes
	fmt.Fprintf(w, "\n%-14s%s total, %s reclaimable\n", "Footprint:", formatBytes(total), formatBytes(reclaim))

	if len(r.Images) == 0 {
		return ew.Err()
	}

	fmt.Fprintln(w, "\nImages (largest first)")
	rows := make([][]string, 0, len(r.Images))
	for _, img := range r.Images {
		tag := "<none>"
		if len(img.Tags) > 0 {
			tag = img.Tags[0]
		}
		use := "-"
		if img.InUse {
			use = strconv.Itoa(img.Containers)
		}
		rows = append(rows, []string{
			img.ShortID,
			tag,
			formatBytes(img.SizeBytes),
			formatBytes(img.SharedSizeBytes),
			use,
		})
	}
	formatTable(w, []string{"ID", "TAG", "SIZE", "SHARED", "USED BY"}, rows)

	if r.ImagesFiltered > 0 {
		fmt.Fprintf(w, "\n%d image(s) hidden by --filter/--limit/--unused; the totals above cover all of them.\n",
			r.ImagesFiltered)
	}
	if r.ImagesTruncated > 0 {
		fmt.Fprintf(w, "\n%d smaller image(s) not sent by the collector; the totals above cover all of them.\n",
			r.ImagesTruncated)
	}

	// Shared layers mean per-image sizes overlap, so the column does not sum
	// to the footprint. Say so rather than let it look like an arithmetic bug.
	fmt.Fprintln(w, "\nImage sizes include shared layers, so they do not sum to the total;")
	fmt.Fprintln(w, "the reclaimable estimate is capped at the deduplicated size on disk.")

	return ew.Err()
}
