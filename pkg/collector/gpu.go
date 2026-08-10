package collector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// drmCardPattern matches DRM card directories (card0, card1, ...) and skips
// connector entries such as card1-DP-1.
var drmCardPattern = regexp.MustCompile(`^card\d+$`)

// vendorID constants for PCI vendor identification.
const (
	vendorNVIDIA = "0x10de"
	vendorAMD    = "0x1002"
	vendorIntel  = "0x8086"
)

// GPUCollector collects GPU metrics for NVIDIA, AMD, and Intel GPUs.
// NVIDIA GPUs are queried via the NVML library. AMD and Intel GPUs are read
// from sysfs. When no GPUs are found, Collect is a no-op and Info returns an
// empty slice without error.
type GPUCollector struct {
	logger          *slog.Logger
	gpus            atomic.Pointer[[]types.GPUInfo]
	nvmlInitialized atomic.Bool
}

// NewGPUCollector returns a new GPUCollector ready for use.
func NewGPUCollector(logger *slog.Logger) *GPUCollector {
	c := &GPUCollector{logger: logger}
	empty := []types.GPUInfo{}
	c.gpus.Store(&empty)
	return c
}

// drmCard holds the identity of a single DRM card enumerated from sysfs.
type drmCard struct {
	name     string // e.g. "card1"
	cardPath string // e.g. "/sys/class/drm/card1"
	vendor   string // lowercased hex vendor ID, e.g. "0x10de"
	index    int    // DRM enumeration index (0-based, sorted by card name)
}

// Collect enumerates all DRM cards, identifies their vendor, and collects
// GPU metrics using the appropriate method per vendor. Results are stored
// atomically and returned by Info.
func (c *GPUCollector) Collect(ctx context.Context) error {
	cards, err := enumerateDRMCards()
	if err != nil {
		c.logger.WarnContext(ctx, "failed to enumerate DRM cards", "error", err)
		empty := []types.GPUInfo{}
		c.gpus.Store(&empty)
		return nil
	}
	if len(cards) == 0 {
		empty := []types.GPUInfo{}
		c.gpus.Store(&empty)
		return nil
	}

	// Separate cards by vendor so NVIDIA cards can be batched via NVML.
	var nvidiaCards []drmCard
	var otherCards []drmCard

	for i, name := range cards {
		cardPath := filepath.Join(drmRoot, name)
		vendor := readSysfsString(filepath.Join(cardPath, "device/vendor"))
		vendor = strings.ToLower(strings.TrimSpace(vendor))
		card := drmCard{name: name, cardPath: cardPath, vendor: vendor, index: i}
		if vendor == vendorNVIDIA {
			nvidiaCards = append(nvidiaCards, card)
		} else {
			otherCards = append(otherCards, card)
		}
	}

	gpus := make([]types.GPUInfo, 0, len(cards))

	// Collect NVIDIA GPUs via NVML (batched).
	if len(nvidiaCards) > 0 {
		nvidiaGPUs := c.collectNvidiaGPUs(ctx, nvidiaCards)
		gpus = append(gpus, nvidiaGPUs...)
	}

	// Collect AMD and Intel GPUs via sysfs.
	for _, card := range otherCards {
		switch card.vendor {
		case vendorAMD:
			g := collectAMDGPU(card.cardPath, card.index)
			gpus = append(gpus, g)
		case vendorIntel:
			g := collectIntelGPU(card.cardPath, card.index)
			gpus = append(gpus, g)
		default:
			c.logger.WarnContext(ctx, "unknown GPU vendor, skipping",
				"card", card.name, "vendor", card.vendor)
		}
	}

	// Sort by DRM index so the slice is deterministically ordered.
	sort.Slice(gpus, func(i, j int) bool {
		return gpus[i].Index < gpus[j].Index
	})

	c.gpus.Store(&gpus)
	return nil
}

// Info returns the most recently collected GPU information.
func (c *GPUCollector) Info() []types.GPUInfo {
	return *c.gpus.Load()
}

// drmRoot is where the kernel exposes graphics devices. It is a variable so
// tests can enumerate a synthetic tree containing vendors the build host does
// not have.
var drmRoot = "/sys/class/drm"

// enumerateDRMCards returns the sorted list of DRM card names found in
// drmRoot (e.g. ["card0", "card1"]). Connector entries such as
// "card1-DP-1" are excluded.
func enumerateDRMCards() ([]string, error) {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", drmRoot, err)
	}
	var cards []string
	for _, e := range entries {
		if drmCardPattern.MatchString(e.Name()) {
			cards = append(cards, e.Name())
		}
	}
	sort.Strings(cards)
	return cards, nil
}

// collectNvidiaGPUs uses the NVML library to collect data for all NVIDIA DRM
// cards. NVML is initialized lazily on first call and kept alive across
// collection cycles for efficiency. It matches NVML devices to DRM cards via
// PCI bus ID.
// nvmlMu serialises access to NVML. The go-nvml library keeps its loaded
// symbol table in package-level state that is not safe for concurrent
// initialisation, so two SystemCollector instances collecting at the same time
// race inside the library itself. The GPU tier runs infrequently, so holding a
// process-wide lock here costs nothing.
var nvmlMu sync.Mutex

func (c *GPUCollector) collectNvidiaGPUs(ctx context.Context, cards []drmCard) []types.GPUInfo {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	lib := nvml.New()

	if !c.nvmlInitialized.Load() {
		if ret := lib.Init(); ret != nvml.SUCCESS {
			c.logger.WarnContext(ctx, "NVML init failed, skipping NVIDIA GPUs",
				"error", nvml.ErrorString(ret))
			return nil
		}
		c.nvmlInitialized.Store(true)
	}

	count, ret := lib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		c.logger.WarnContext(ctx, "NVML DeviceGetCount failed", "error", nvml.ErrorString(ret))
		return nil
	}
	if count == 0 {
		return nil
	}

	// Collect driver version once — it applies to all NVIDIA GPUs.
	driverVersion, ret := lib.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		driverVersion = ""
	}

	// Build a map from normalised PCI bus ID → DRM card index so we can
	// assign the correct DRM index to each NVML device.
	busToIndex := make(map[string]int, len(cards))
	for _, card := range cards {
		busID := normalisePCIBusID(readSysfsString(
			filepath.Join(card.cardPath, "device/uevent"),
		))
		if busID != "" {
			busToIndex[busID] = card.index
		}
	}

	gpus := make([]types.GPUInfo, 0, count)
	for i := range count {
		dev, ret := lib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			c.logger.WarnContext(ctx, "NVML DeviceGetHandleByIndex failed",
				"index", i, "error", nvml.ErrorString(ret))
			continue
		}

		g := types.GPUInfo{
			Index:         i,
			DriverVersion: driverVersion,
		}

		// Identity fields.
		if name, ret := dev.GetName(); ret == nvml.SUCCESS {
			g.Name = name
		}
		if uuid, ret := dev.GetUUID(); ret == nvml.SUCCESS {
			g.UUID = uuid
		}
		if serial, ret := dev.GetSerial(); ret == nvml.SUCCESS {
			g.Serial = serial
		}
		if vbios, ret := dev.GetVbiosVersion(); ret == nvml.SUCCESS {
			g.VBIOSVersion = vbios
		}

		// PCI bus ID.
		if pci, ret := dev.GetPciInfo(); ret == nvml.SUCCESS {
			// BusId is a null-terminated byte array in "domain:bus:device.function" form.
			g.PCIBusID = nvmlBusIDString(pci.BusId[:])
		}

		// Memory (bytes → MiB).
		if mem, ret := dev.GetMemoryInfo(); ret == nvml.SUCCESS {
			g.MemoryTotalMiB = mem.Total / (1024 * 1024)
			g.MemoryUsedMiB = mem.Used / (1024 * 1024)
			g.MemoryFreeMiB = mem.Free / (1024 * 1024)
			if g.MemoryTotalMiB > 0 {
				g.MemoryPercent = float64(g.MemoryUsedMiB) / float64(g.MemoryTotalMiB) * 100
			}
		}

		// Utilization rates.
		if util, ret := dev.GetUtilizationRates(); ret == nvml.SUCCESS {
			g.GPUUtilPercent = float64(util.Gpu)
			g.MemoryUtilPercent = float64(util.Memory)
		}

		// Encoder/decoder utilization.
		if enc, _, ret := dev.GetEncoderUtilization(); ret == nvml.SUCCESS {
			g.EncoderPercent = float64(enc)
		}
		if dec, _, ret := dev.GetDecoderUtilization(); ret == nvml.SUCCESS {
			g.DecoderPercent = float64(dec)
		}

		// Temperature.
		if temp, ok := nvmlGPUTemperature(dev); ok {
			g.TemperatureGPU = temp
		}
		if temp, ok := nvmlMemoryTemperature(dev); ok {
			g.TemperatureMemory = temp
		}

		// Fan speed.
		if fan, ret := dev.GetFanSpeed(); ret == nvml.SUCCESS {
			g.FanSpeedPercent = float64(fan)
		}

		// Power (milliwatts → watts).
		if pwr, ret := dev.GetPowerUsage(); ret == nvml.SUCCESS {
			g.PowerDrawW = float64(pwr) / 1000.0
		}
		if lim, ret := dev.GetPowerManagementLimit(); ret == nvml.SUCCESS {
			g.PowerLimitW = float64(lim) / 1000.0
		}
		if _, maxLim, ret := dev.GetPowerManagementLimitConstraints(); ret == nvml.SUCCESS {
			g.PowerMaxW = float64(maxLim) / 1000.0
		}

		// PCIe generation and width.
		if gen, ret := dev.GetCurrPcieLinkGeneration(); ret == nvml.SUCCESS {
			g.PCIeGenCurrent = gen
		}
		if gen, ret := dev.GetMaxPcieLinkGeneration(); ret == nvml.SUCCESS {
			g.PCIeGenMax = gen
		}
		if width, ret := dev.GetCurrPcieLinkWidth(); ret == nvml.SUCCESS {
			g.PCIeWidthCurrent = width
		}
		if width, ret := dev.GetMaxPcieLinkWidth(); ret == nvml.SUCCESS {
			g.PCIeWidthMax = width
		}

		// PCIe throughput (KB/s → MB/s).
		if rx, ret := dev.GetPcieThroughput(nvml.PCIE_UTIL_RX_BYTES); ret == nvml.SUCCESS {
			g.PCIeRxMBps = float64(rx) / 1024.0
		}
		if tx, ret := dev.GetPcieThroughput(nvml.PCIE_UTIL_TX_BYTES); ret == nvml.SUCCESS {
			g.PCIeTxMBps = float64(tx) / 1024.0
		}

		// Clocks.
		if clk, ret := dev.GetClockInfo(nvml.CLOCK_GRAPHICS); ret == nvml.SUCCESS {
			g.ClockGraphicsMHz = clk
		}
		if clk, ret := dev.GetClockInfo(nvml.CLOCK_MEM); ret == nvml.SUCCESS {
			g.ClockMemoryMHz = clk
		}
		if clk, ret := dev.GetClockInfo(nvml.CLOCK_VIDEO); ret == nvml.SUCCESS {
			g.ClockVideoMHz = clk
		}
		if clk, ret := dev.GetMaxClockInfo(nvml.CLOCK_GRAPHICS); ret == nvml.SUCCESS {
			g.ClockMaxGfxMHz = clk
		}
		if clk, ret := dev.GetMaxClockInfo(nvml.CLOCK_MEM); ret == nvml.SUCCESS {
			g.ClockMaxMemMHz = clk
		}

		// Compute mode.
		if mode, ret := dev.GetComputeMode(); ret == nvml.SUCCESS {
			g.ComputeMode = nvmlComputeModeString(mode)
		}

		// Performance state (P0–P15).
		if pstate, ret := dev.GetPerformanceState(); ret == nvml.SUCCESS {
			g.PerfState = nvmlPerfStateString(pstate)
		}

		// ECC mode and error counters.
		g.ECCEnabled, g.ECCSingleBit, g.ECCDoubleBit = nvmlCollectECC(dev)

		// Running process count.
		if procs, ret := dev.GetComputeRunningProcesses(); ret == nvml.SUCCESS {
			g.ProcessCount = len(procs)
		}

		// Remap DRM index by PCI bus ID if available.
		if normID := normalisePCIBusID(g.PCIBusID); normID != "" {
			if idx, ok := busToIndex[normID]; ok {
				g.Index = idx
			}
		}

		gpus = append(gpus, g)
	}
	return gpus
}

// nvmlBusIDString converts a null-terminated NVML BusId byte array to a
// printable string, e.g. "0000:02:00.0".
// nvmlBusIDString converts NVML's null-terminated C character array into a Go
// string. The element type is int8 because cgo maps C.char that way on
// platforms with a signed char, which is why this takes []int8 rather than
// []byte.
func nvmlBusIDString(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// nvmlComputeModeString converts an NVML ComputeMode to its human-readable
// label, mirroring nvidia-smi output.
func nvmlComputeModeString(mode nvml.ComputeMode) string {
	switch mode {
	case nvml.COMPUTEMODE_DEFAULT:
		return "Default"
	case nvml.COMPUTEMODE_EXCLUSIVE_THREAD:
		return "Exclusive Thread"
	case nvml.COMPUTEMODE_PROHIBITED:
		return "Prohibited"
	case nvml.COMPUTEMODE_EXCLUSIVE_PROCESS:
		return "Exclusive Process"
	default:
		return "Unknown"
	}
}

// nvmlPerfStateString converts an NVML Pstates value to its "P<N>" string
// representation (e.g. nvml.PSTATE_0 → "P0").
func nvmlPerfStateString(ps nvml.Pstates) string {
	n := int(ps)
	if n >= 0 && n <= 15 {
		return "P" + strconv.Itoa(n)
	}
	return "Unknown"
}

// nvmlCollectECC reads ECC mode and error counters from an NVML device.
// It returns (eccEnabled, singleBitErrors, doubleBitErrors).
//
// Detection tries multiple NVML APIs in order because different GPU
// generations expose ECC support differently:
//
//  1. GetEccMode — works for GPUs with togglable ECC.
//  2. GetDefaultEccMode — returns FEATURE_ENABLED on always-on ECC hardware
//     even when GetEccMode returns NOT_SUPPORTED.
//  3. GetTotalEccErrors — if error counters are readable, ECC is active.
//  4. GetRemappedRows — Ampere/Hopper/Blackwell always-on ECC reports row
//     remapping instead of traditional counters; a SUCCESS return confirms
//     ECC hardware is present.
func nvmlCollectECC(dev nvml.Device) (enabled bool, sbe, dbe uint64) {
	// 1. Try togglable ECC mode.
	if current, _, ret := dev.GetEccMode(); ret == nvml.SUCCESS {
		enabled = current == nvml.FEATURE_ENABLED
	}

	// 2. Try default ECC mode (always-on hardware reports ENABLED here).
	if !enabled {
		if defaultMode, ret := dev.GetDefaultEccMode(); ret == nvml.SUCCESS {
			enabled = defaultMode == nvml.FEATURE_ENABLED
		}
	}

	// 3. Read error counters (volatile first, then aggregate).
	for _, counter := range []nvml.EccCounterType{nvml.VOLATILE_ECC, nvml.AGGREGATE_ECC} {
		if corr, r := dev.GetTotalEccErrors(nvml.MEMORY_ERROR_TYPE_CORRECTED, counter); r == nvml.SUCCESS {
			sbe = corr
			if !enabled {
				enabled = true
			}
			if uncorr, r2 := dev.GetTotalEccErrors(nvml.MEMORY_ERROR_TYPE_UNCORRECTED, counter); r2 == nvml.SUCCESS {
				dbe = uncorr
			}
			return
		}
	}

	// 4. Try remapped rows (Ampere+ always-on ECC).
	if !enabled {
		if _, _, _, _, ret := dev.GetRemappedRows(); ret == nvml.SUCCESS {
			enabled = true
		}
	}

	return
}

// nvmlGPUTemperature reads the GPU die temperature in degrees Celsius.
//
// NVML 13.0 deprecated GetTemperature in favour of the versioned
// GetTemperatureV, but that entry point exists only in 13.0 and later drivers.
// go-nvml resolves symbols from libnvidia-ml at runtime, so on an older driver
// the versioned call fails with ERROR_FUNCTION_NOT_FOUND rather than failing to
// build. Both are tried, newest first, so the collector prefers the supported
// API without dropping support for drivers still in the field.
func nvmlGPUTemperature(dev nvml.Device) (float64, bool) {
	// V1 leaves SensorType at its zero value, which is TEMPERATURE_GPU.
	if t, ret := dev.GetTemperatureV().V1(); ret == nvml.SUCCESS {
		return float64(t.Temperature), true
	}
	if t, ret := dev.GetTemperature(nvml.TEMPERATURE_GPU); ret == nvml.SUCCESS {
		return float64(t), true
	}
	return 0, false
}

// nvmlMemoryTemperature reads the memory (VRAM) temperature in degrees Celsius.
//
// This is deliberately not a GetTemperature call. nvmlTemperatureSensors_t
// defines exactly one sensor, TEMPERATURE_GPU; its other member,
// TEMPERATURE_COUNT, is the enum's bound rather than a second sensor, and
// passing it returns ERROR_INVALID_ARGUMENT. Memory temperature is reachable
// only through the field-value API, as FI_DEV_MEMORY_TEMP.
//
// Most consumer cards do not implement the field, so a false return is the
// normal result rather than a failure worth logging.
func nvmlMemoryTemperature(dev nvml.Device) (float64, bool) {
	values := []nvml.FieldValue{{FieldId: nvml.FI_DEV_MEMORY_TEMP}}
	return nvmlFieldNumber(dev.GetFieldValues(values), values)
}

// nvmlFieldNumber interprets the outcome of a single-field GetFieldValues
// batch. The batch return and the per-field return are separate: the call as a
// whole succeeds even when the one field in it is unsupported, so a value is
// only trustworthy when both say SUCCESS.
func nvmlFieldNumber(ret nvml.Return, values []nvml.FieldValue) (float64, bool) {
	if ret != nvml.SUCCESS || len(values) == 0 {
		return 0, false
	}
	if nvml.Return(values[0].NvmlReturn) != nvml.SUCCESS {
		return 0, false
	}
	return nvmlFieldValueNumber(values[0])
}

// nvmlFieldValueNumber decodes a field value's eight-byte union according to
// the type NVML tagged it with. The bytes are raw memory handed back by the C
// library, so they are read in the host's byte order. An unrecognised type
// yields false rather than a misinterpreted number.
func nvmlFieldValueNumber(v nvml.FieldValue) (float64, bool) {
	b := v.Value[:]
	switch nvml.ValueType(v.ValueType) {
	case nvml.VALUE_TYPE_DOUBLE:
		return math.Float64frombits(binary.NativeEndian.Uint64(b)), true
	case nvml.VALUE_TYPE_UNSIGNED_INT:
		return float64(binary.NativeEndian.Uint32(b)), true
	case nvml.VALUE_TYPE_UNSIGNED_LONG, nvml.VALUE_TYPE_UNSIGNED_LONG_LONG:
		return float64(binary.NativeEndian.Uint64(b)), true
	case nvml.VALUE_TYPE_SIGNED_INT:
		return float64(int32(binary.NativeEndian.Uint32(b))), true
	case nvml.VALUE_TYPE_SIGNED_LONG_LONG:
		return float64(int64(binary.NativeEndian.Uint64(b))), true
	case nvml.VALUE_TYPE_UNSIGNED_SHORT:
		return float64(binary.NativeEndian.Uint16(b)), true
	default:
		return 0, false
	}
}

// normalisePCIBusID extracts and normalises a PCI bus address from either an
// NVML "0000:02:00.0" string or a uevent file containing
// "PCI_SLOT_NAME=0000:02:00.0". Returns the canonical "bus:device.function"
// form (e.g. "02:00.0") with any domain prefix stripped so that NVML
// and sysfs uevent addresses compare equally.
func normalisePCIBusID(s string) string {
	s = strings.TrimSpace(s)
	// uevent format: "PCI_SLOT_NAME=0000:04:00.0"
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			s = strings.TrimPrefix(line, "PCI_SLOT_NAME=")
			break
		}
	}
	// Strip the domain prefix regardless of length. Both NVML
	// ("0000:02:00.0") and sysfs ("0000:02:00.0") include a domain
	// segment before bus:device.function. The canonical form is just
	// "bus:device.function" (2 colon-separated parts).
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) >= 3 {
		// Keep only the last two segments: bus and device.function.
		s = strings.Join(parts[len(parts)-2:], ":")
	}
	return strings.ToLower(s)
}

// pcieGenFromSpeed converts a PCIe link speed string (from sysfs
// current_link_speed / max_link_speed) to a PCIe generation number.
// Known mappings:
//
//	"2.5 GT/s"       → 1
//	"5.0 GT/s"       → 2
//	"8.0 GT/s"       → 3
//	"16.0 GT/s"      → 4
//	"32.0 GT/s"      → 5
//	"64.0 GT/s"      → 6
//
// The sysfs files on some kernels include a trailing " PCIe" qualifier, e.g.
// "8.0 GT/s PCIe". Both forms are handled.
func pcieGenFromSpeed(speed string) int {
	// Strip optional trailing qualifiers such as " PCIe".
	s := strings.TrimSpace(speed)
	if idx := strings.Index(s, " GT/s"); idx >= 0 {
		s = s[:idx]
	} else {
		return 0
	}
	s = strings.TrimSpace(s)
	// Some kernels omit the decimal, so parse as float to be safe.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	switch {
	case f <= 2.6:
		return 1
	case f <= 5.1:
		return 2
	case f <= 8.1:
		return 3
	case f <= 16.1:
		return 4
	case f <= 32.1:
		return 5
	case f <= 64.1:
		return 6
	default:
		return 0
	}
}

// parseActiveDPMClock parses the content of AMD sysfs pp_dpm_sclk or
// pp_dpm_mclk files and returns the currently active clock frequency in MHz.
// The active entry is indicated by a trailing " *" on the line.
//
// Example input:
//
//	0: 300Mhz
//	1: 600Mhz
//	7: 1411Mhz *
func parseActiveDPMClock(data []byte) uint32 {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasSuffix(line, "*") {
			continue
		}
		// Strip the " *" suffix and any trailing whitespace.
		line = strings.TrimSuffix(line, "*")
		line = strings.TrimSpace(line)
		// Format: "<index>: <freq>Mhz" or "<index>: <freq>MHz".
		if idx := strings.Index(line, ":"); idx >= 0 {
			line = strings.TrimSpace(line[idx+1:])
		}
		// Strip MHz/Mhz suffix.
		line = strings.TrimSuffix(line, "MHz")
		line = strings.TrimSuffix(line, "Mhz")
		line = strings.TrimSpace(line)
		v, err := strconv.ParseUint(line, 10, 32)
		if err != nil {
			continue
		}
		return uint32(v)
	}
	return 0
}

// sysfsDeviceName attempts to read a human-readable device name for a PCI
// device directly from sysfs, avoiding any external tool invocation.
//
// It tries, in order:
//  1. /sys/bus/pci/devices/<busID>/label — present on some systems/firmware.
//  2. The PCI_ID field from the device uevent combined with a minimal
//     vendor/device hex string as a last resort.
//
// Returns an empty string if no useful name can be determined.
func sysfsDeviceName(devPath string) string {
	// Try the label file first (firmware-provided human name).
	if label := readSysfsString(filepath.Join(devPath, "label")); label != "" {
		return label
	}
	return ""
}
