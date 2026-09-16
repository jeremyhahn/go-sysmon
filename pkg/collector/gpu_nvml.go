//go:build nvml

// NVIDIA GPU collection through NVML, built only with the `nvml` tag.
//
// NVML has no pure-Go implementation: go-nvml's generated bindings are cgo, so
// importing this package unconditionally made `CGO_ENABLED=0 go build` fail for
// every consumer whether it wanted GPU metrics or not, which blocked embedding
// the collector in a static or FIPS binary. The default build gets
// gpu_nonvml.go instead, which reports what sysfs knows about the same cards.
//
// The binaries this project ships are built WITH the tag (see SERVER_TAGS and
// DESKTOP_TAGS in the Makefile), so nothing about go-sysmon's own behaviour
// changes.

package collector

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

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

// nvmlInitialized records that nvml.Init succeeded. It is package-level for the
// same reason nvmlMu is: the library's loaded state belongs to the process, not
// to a collector, so two GPUCollectors must not each believe they have to
// initialise it. It also has to leave GPUCollector, because a field only the
// tagged build uses is an unused field in the default one.
var nvmlInitialized atomic.Bool

func (c *GPUCollector) collectNvidiaGPUs(ctx context.Context, cards []drmCard) []types.GPUInfo {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	lib := nvml.New()

	if !nvmlInitialized.Load() {
		if ret := lib.Init(); ret != nvml.SUCCESS {
			c.logger.WarnContext(ctx, "NVML init failed, skipping NVIDIA GPUs",
				"error", nvml.ErrorString(ret))
			return nil
		}
		nvmlInitialized.Store(true)
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
