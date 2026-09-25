package collector

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jaypipes/ghw"
	psmem "github.com/shirou/gopsutil/v4/mem"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// smbiosTablePath is the raw SMBIOS table. It is a variable rather than a
// constant so tests can supply a fixture table and exercise the parser without
// root, which reading the real one requires.
var smbiosTablePath = "/sys/firmware/dmi/tables/DMI"

// vmstatPath is the kernel's cumulative VM event counters. It is a variable
// rather than a constant for the same reason as smbiosTablePath: tests feed a
// fixture instead of reading the host, whose counters move under the test and
// cannot express the absent case at all.
var vmstatPath = "/proc/vmstat"

const (
	// SMBIOS structure type for Memory Device.
	smbiosTypeMemoryDevice = 17

	// SMBIOS Type 17 field offsets (0-based from structure start).
	// All offsets are relative to the beginning of the formatted area.
	off17ArrayHandle       = 0x04
	off17TotalWidth        = 0x08
	off17DataWidth         = 0x0A
	off17Size              = 0x0C
	off17FormFactor        = 0x0E
	off17DeviceSet         = 0x0F
	off17DeviceLocatorStr  = 0x10
	off17BankLocatorStr    = 0x11
	off17MemoryType        = 0x12
	off17TypeDetail        = 0x13
	off17Speed             = 0x15
	off17ManufacturerStr   = 0x17
	off17SerialNumberStr   = 0x18
	off17AssetTagStr       = 0x19
	off17PartNumberStr     = 0x1A
	off17Attributes        = 0x1B
	off17ExtendedSize      = 0x1C
	off17ConfiguredSpeed   = 0x20
	off17MinVoltage        = 0x22
	off17MaxVoltage        = 0x24
	off17ConfiguredVoltage = 0x26

	// Size field sentinel: if bit 15 is set, size is in KB; otherwise MB.
	// 0x7FFF means size is in the extended size field.
	sizeFieldUnknown  = 0xFFFF
	sizeFieldExtended = 0x7FFF
	sizeKBFlag        = 0x8000
)

// memoryFormFactors maps SMBIOS form factor byte values to human-readable strings.
var memoryFormFactors = map[byte]string{
	0x01: "Other",
	0x02: "Unknown",
	0x03: "SIMM",
	0x04: "SIP",
	0x05: "Chip",
	0x06: "DIP",
	0x07: "ZIP",
	0x08: "Proprietary Card",
	0x09: "DIMM",
	0x0A: "TSOP",
	0x0B: "Row of chips",
	0x0C: "RIMM",
	0x0D: "SODIMM",
	0x0E: "SRIMM",
	0x0F: "FB-DIMM",
	0x10: "Die",
}

// memoryTypes maps SMBIOS memory type byte values to human-readable strings.
var memoryTypes = map[byte]string{
	0x01: "Other",
	0x02: "Unknown",
	0x03: "DRAM",
	0x04: "EDRAM",
	0x05: "VRAM",
	0x06: "SRAM",
	0x07: "RAM",
	0x08: "ROM",
	0x09: "FLASH",
	0x0A: "EEPROM",
	0x0B: "FEPROM",
	0x0C: "EPROM",
	0x0D: "CDRAM",
	0x0E: "3DRAM",
	0x0F: "SDRAM",
	0x10: "SGRAM",
	0x11: "RDRAM",
	0x12: "DDR",
	0x13: "DDR2",
	0x14: "DDR2 FB-DIMM",
	0x18: "DDR3",
	0x19: "FBD2",
	0x1A: "DDR4",
	0x1B: "LPDDR",
	0x1C: "LPDDR2",
	0x1D: "LPDDR3",
	0x1E: "LPDDR4",
	0x1F: "Logical non-volatile device",
	0x20: "HBM",
	0x21: "HBM2",
	0x22: "DDR5",
	0x23: "LPDDR5",
	0x24: "HBM3",
}

// MemoryCollector collects virtual memory, swap, and physical DIMM information.
type MemoryCollector struct {
	info   atomic.Pointer[types.MemoryInfo]
	logger *slog.Logger

	// dimms is the module inventory: slot, size, speed, manufacturer, part
	// number. It comes from the SMBIOS type-17 table, which firmware generates
	// at boot and never rewrites, so it is read once at construction.
	//
	// Only the topology is fixed. Temperatures are read from hwmon on every
	// collection and applied onto a COPY of this, because they are the one
	// thing here that changes -- and they sit in a different sysfs tree for a
	// reason.
	dimms []types.DIMMInfo
}

// NewMemoryCollector returns a new MemoryCollector, reading the module
// inventory as it does so.
func NewMemoryCollector(logger *slog.Logger) *MemoryCollector {
	c := &MemoryCollector{logger: logger}
	c.info.Store(&types.MemoryInfo{})
	c.dimms = c.collectDIMMs()
	return c
}

// dimmsWithoutTemperatures returns a copy of the cached inventory for this
// collection to write temperatures into.
//
// A copy rather than the cached slice itself: applyDIMMTemperatures writes into
// the elements it is given, and handing it the cache would leave last tick's
// readings on the modules that no longer have a sensor -- a temperature that
// stops being measured would go on being displayed.
func (c *MemoryCollector) dimmsWithoutTemperatures() []types.DIMMInfo {
	if len(c.dimms) == 0 {
		return nil
	}
	out := make([]types.DIMMInfo, len(c.dimms))
	copy(out, c.dimms)
	return out
}

// Collect refreshes memory statistics.
func (c *MemoryCollector) Collect() error {
	vm, err := psmem.VirtualMemory()
	if err != nil {
		return &types.CollectorError{Collector: "memory", Cause: err}
	}

	sw, err := psmem.SwapMemory()
	if err != nil {
		// Swap may simply not be configured; treat as warning only.
		warnOnce(c.logger, "memory:swap", "memory: could not collect swap info", "error", err)
	}

	info := &types.MemoryInfo{
		TotalBytes:     vm.Total,
		UsedBytes:      vm.Used,
		AvailableBytes: vm.Available,
		FreeBytes:      vm.Free,
		UsedPercent:    vm.UsedPercent,
		BuffersBytes:   vm.Buffers,
		CachedBytes:    vm.Cached,
		SharedBytes:    vm.Shared,
		SlabBytes:      vm.Slab,
		// A property of the host, not of the swap reading, so it is reported
		// whether or not the counters below could be read.
		PageSizeBytes: uint64(os.Getpagesize()),
	}

	if sw != nil {
		info.SwapTotalBytes = sw.Total
		info.SwapUsedBytes = sw.Used
		info.SwapFreeBytes = sw.Free
		info.SwapPercent = sw.UsedPercent
	}

	if in, out, ok := readSwapCounters(vmstatPath); ok {
		info.SwapInPages = in
		info.SwapOutPages = out
		info.SwapCountersAvailable = true
	} else {
		warnOnce(c.logger, "memory:vmstat",
			"memory: swap paging counters unavailable; reporting occupancy only",
			"path", vmstatPath)
	}

	info.DIMMs = c.dimmsWithoutTemperatures()
	info.TempSensorDetected = applyDIMMTemperatures(info.DIMMs, c.logger)

	c.info.Store(info)
	return nil
}

// readSwapCounters reads the cumulative swap paging counters -- pswpin and
// pswpout, in pages since boot -- from a /proc/vmstat-shaped file. It reports
// ok only when BOTH counters were present and parsed; a partial or unreadable
// read yields zeros with ok false, because zero is also what a host that has
// never paged reports and the two must stay distinguishable.
//
// gopsutil's SwapMemory does surface these as Sin/Sout, and this is not an
// oversight of that: it scales them by a hardcoded 4096 rather than the host's
// page size, and it cannot say whether the values were read or merely defaulted
// -- which is the entire point of the counters here.
func readSwapCounters(path string) (in, out uint64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}

	var haveIn, haveOut bool
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), " ")
		if !found {
			continue
		}
		switch key {
		case "pswpin":
			in, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			haveIn = err == nil
		case "pswpout":
			out, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			haveOut = err == nil
		}
	}

	if !haveIn || !haveOut {
		return 0, 0, false
	}
	return in, out, true
}

// collectDIMMs reads DIMM information from the SMBIOS binary table, and from
// ghw when the table cannot be read.
//
// It reads only what the process may read. The table is 0400 root:root, and
// go-sysmon never escalates to get at it -- no sudo, no setuid helper: granting
// access is the administrator's decision, and `sysmon doctor` says how.
func (c *MemoryCollector) collectDIMMs() []types.DIMMInfo {
	data, err := os.ReadFile(smbiosTablePath)
	if err != nil {
		// Unreadable without root and unchanging for the process lifetime,
		// so report it once rather than on every collection cycle.
		warnOnce(c.logger, "memory:smbios",
			"memory: cannot read the SMBIOS table; module details come from ghw where it has them",
			"path", smbiosTablePath, "error", err,
			"hint", "run `sysmon doctor` for how to grant read access",
			"note", "this is logged once per run")
		return c.collectDIMMsViaGHW()
	}

	dimms, parseErr := parseSMBIOSType17(data)
	if parseErr != nil {
		warnOnce(c.logger, "memory:smbios-parse", "memory: SMBIOS parse failed, falling back to ghw", "error", parseErr)
		return c.collectDIMMsViaGHW()
	}
	if len(dimms) == 0 {
		return c.collectDIMMsViaGHW()
	}
	return dimms
}

// dimmTempDrivers lists the hwmon driver names that provide DIMM thermal data.
var dimmTempDrivers = []string{"jc42", "spd5118"}

// applyDIMMTemperatures reads the DIMM temperature sensors the kernel already
// exposes in hwmon (the jc42 or spd5118 drivers) and assigns them to DIMMs by
// index order. These sensors appear at /sys/class/hwmon/hwmonN/ with name
// "jc42" or "spd5118" and expose temp1_input in millidegrees Celsius.
// Returns true if at least one DIMM thermal sensor was found.
//
// It only reads. go-sysmon never loads a kernel module or instantiates an
// SMBus device to make a sensor appear: monitoring must not change the state
// of the hardware it watches. Enabling the sensors is the administrator's
// decision; docs/collector/sensors.md says how.
func applyDIMMTemperatures(dimms []types.DIMMInfo, logger *slog.Logger) bool {
	if len(dimms) == 0 {
		return false
	}

	temps := scanDIMMHwmonTemps()
	if len(temps) == 0 {
		warnOnce(logger, "memory:no-dimm-temps",
			"memory: no DIMM temperature sensors are exposed in hwmon",
			"hint", "load jc42 (DDR3/DDR4) or spd5118 (DDR5) to enable them; see docs/collector/sensors.md",
			"note", "this is logged once per run")
		return false
	}

	// Match sensors to DIMMs by positional index.
	for i := range dimms {
		if i >= len(temps) {
			break
		}
		dimms[i].Temperature = temps[i]
	}
	return true
}

// hwmonClassRoot is the hwmon sysfs root. It is a variable so tests can point
// the DIMM sensor scan at a synthetic tree.
var hwmonClassRoot = "/sys/class/hwmon"

// scanDIMMHwmonTemps reads DIMM temperature values from hwmon sensors.
func scanDIMMHwmonTemps() []float64 {
	entries, err := os.ReadDir(hwmonClassRoot)
	if err != nil {
		return nil
	}

	driverSet := make(map[string]struct{}, len(dimmTempDrivers))
	for _, d := range dimmTempDrivers {
		driverSet[d] = struct{}{}
	}

	var temps []float64
	for _, e := range entries {
		hwmonPath := filepath.Join(hwmonClassRoot, e.Name())
		name := readSysfsString(filepath.Join(hwmonPath, "name"))
		if _, ok := driverSet[name]; !ok {
			continue
		}
		raw := readSysfsUint64(filepath.Join(hwmonPath, "temp1_input"))
		if raw == 0 {
			continue
		}
		temps = append(temps, float64(raw)/1000.0)
	}
	return temps
}

// parseSMBIOSType17 walks the raw SMBIOS table data and returns one DIMMInfo
// for each Type 17 (Memory Device) structure that reports a non-zero size.
func parseSMBIOSType17(data []byte) ([]types.DIMMInfo, *types.SMBIOSParseError) {
	var dimms []types.DIMMInfo

	pos := 0
	for pos+4 <= len(data) {
		structType := data[pos]
		length := int(data[pos+1])

		if length < 4 {
			return nil, &types.SMBIOSParseError{Reason: "structure length < 4 at offset " + itoa(pos)}
		}
		if pos+length > len(data) {
			break
		}

		// Extract the string table that follows the formatted area.
		// The string section is terminated by a double NUL (two consecutive 0x00 bytes).
		stringsStart := pos + length
		stringsEnd := stringsStart
		for stringsEnd+1 < len(data) {
			if data[stringsEnd] == 0x00 && data[stringsEnd+1] == 0x00 {
				stringsEnd += 2
				break
			}
			stringsEnd++
		}

		if structType == smbiosTypeMemoryDevice {
			formatted := data[pos : pos+length]
			stringSection := data[stringsStart:stringsEnd]
			dimm, ok := decodeType17(formatted, stringSection)
			if ok {
				dimm.Index = len(dimms)
				dimms = append(dimms, dimm)
			}
		}

		pos = stringsEnd
	}

	return dimms, nil
}

// decodeType17 decodes a single SMBIOS Type 17 formatted area and its string
// section into a DIMMInfo. Returns false when the slot is empty (size == 0).
func decodeType17(f []byte, strSection []byte) (types.DIMMInfo, bool) {
	var d types.DIMMInfo

	// Size field is at offset 0x0C (2 bytes, little-endian).
	if len(f) < off17Size+2 {
		return d, false
	}

	sizeField := binary.LittleEndian.Uint16(f[off17Size:])

	switch sizeField {
	case 0, sizeFieldUnknown:
		// Empty slot or unknown size — skip.
		return d, false
	case sizeFieldExtended:
		// Use the extended size field at offset 0x1C (4 bytes, little-endian, in MB).
		if len(f) >= off17ExtendedSize+4 {
			extMB := uint64(binary.LittleEndian.Uint32(f[off17ExtendedSize:]))
			d.SizeBytes = extMB * 1024 * 1024
		}
	default:
		if sizeField&sizeKBFlag != 0 {
			// Bit 15 set → size in KB.
			d.SizeBytes = uint64(sizeField&^sizeKBFlag) * 1024
		} else {
			// Size in MB.
			d.SizeBytes = uint64(sizeField) * 1024 * 1024
		}
	}

	if d.SizeBytes == 0 {
		return d, false
	}

	// Width fields.
	if len(f) >= off17TotalWidth+2 {
		tw := binary.LittleEndian.Uint16(f[off17TotalWidth:])
		if tw != 0xFFFF {
			d.TotalWidthBits = uint32(tw)
		}
	}
	if len(f) >= off17DataWidth+2 {
		dw := binary.LittleEndian.Uint16(f[off17DataWidth:])
		if dw != 0xFFFF {
			d.DataWidthBits = uint32(dw)
		}
	}

	// Form factor.
	if len(f) >= off17FormFactor+1 {
		if name, ok := memoryFormFactors[f[off17FormFactor]]; ok {
			d.FormFactor = name
		}
	}

	// Strings: device locator, bank locator.
	if len(f) >= off17DeviceLocatorStr+1 {
		d.Location = smbiosString(strSection, f[off17DeviceLocatorStr])
	}
	if len(f) >= off17BankLocatorStr+1 {
		d.BankLocator = smbiosString(strSection, f[off17BankLocatorStr])
	}

	// Memory type.
	if len(f) >= off17MemoryType+1 {
		if name, ok := memoryTypes[f[off17MemoryType]]; ok {
			d.Type = name
		}
	}

	// Speed (MT/s).
	if len(f) >= off17Speed+2 {
		spd := binary.LittleEndian.Uint16(f[off17Speed:])
		if spd != 0 && spd != 0xFFFF {
			d.SpeedMTs = uint32(spd)
		}
	}

	// Manufacturer, serial, part number strings.
	if len(f) >= off17ManufacturerStr+1 {
		d.Manufacturer = smbiosString(strSection, f[off17ManufacturerStr])
	}
	if len(f) >= off17SerialNumberStr+1 {
		d.SerialNumber = smbiosString(strSection, f[off17SerialNumberStr])
	}
	if len(f) >= off17PartNumberStr+1 {
		d.PartNumber = strings.TrimSpace(smbiosString(strSection, f[off17PartNumberStr]))
	}

	// Rank (lower nibble of Attributes byte).
	if len(f) >= off17Attributes+1 {
		rank := f[off17Attributes] & 0x0F
		if rank != 0 {
			d.Rank = uint32(rank)
		}
	}

	// Configured memory speed.
	if len(f) >= off17ConfiguredSpeed+2 {
		cs := binary.LittleEndian.Uint16(f[off17ConfiguredSpeed:])
		if cs != 0 && cs != 0xFFFF {
			d.ConfiguredSpeedMTs = uint32(cs)
		}
	}

	// Voltages (millivolts stored as uint16; divide by 1000 for volts).
	if len(f) >= off17MinVoltage+2 {
		mv := binary.LittleEndian.Uint16(f[off17MinVoltage:])
		if mv != 0 {
			d.MinVoltage = float64(mv) / 1000.0
		}
	}
	if len(f) >= off17MaxVoltage+2 {
		mv := binary.LittleEndian.Uint16(f[off17MaxVoltage:])
		if mv != 0 {
			d.MaxVoltage = float64(mv) / 1000.0
		}
	}
	if len(f) >= off17ConfiguredVoltage+2 {
		mv := binary.LittleEndian.Uint16(f[off17ConfiguredVoltage:])
		if mv != 0 {
			d.ConfiguredVoltage = float64(mv) / 1000.0
		}
	}

	return d, true
}

// smbiosString extracts the n-th (1-based) NUL-terminated string from the
// SMBIOS string section. Returns an empty string for index 0 or out-of-range.
func smbiosString(strSection []byte, n byte) string {
	if n == 0 || len(strSection) == 0 {
		return ""
	}
	idx := byte(1)
	start := 0
	for i, b := range strSection {
		if b == 0x00 {
			if idx == n {
				return string(strSection[start:i])
			}
			idx++
			start = i + 1
		}
	}
	return ""
}

// itoa converts a non-negative integer to its decimal string representation
// without importing strconv, keeping the binary parsing self-contained.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// collectDIMMsViaGHW is the fallback path using the ghw library.
func (c *MemoryCollector) collectDIMMsViaGHW() []types.DIMMInfo {
	memInfo, err := ghw.Memory()
	if err != nil {
		warnOnce(c.logger, "memory:dimm-info", "memory: could not collect DIMM info (may need root)", "error", err)
		return nil
	}

	if len(memInfo.Modules) == 0 {
		return nil
	}

	dimms := make([]types.DIMMInfo, 0, len(memInfo.Modules))
	for i, m := range memInfo.Modules {
		sizeBytes := uint64(0)
		if m.SizeBytes > 0 {
			sizeBytes = uint64(m.SizeBytes)
		}
		dimms = append(dimms, types.DIMMInfo{
			Index:        i,
			Location:     m.Location,
			Manufacturer: m.Vendor,
			SerialNumber: m.SerialNumber,
			SizeBytes:    sizeBytes,
		})
	}
	return dimms
}

// Info returns the most recently collected MemoryInfo.
func (c *MemoryCollector) Info() types.MemoryInfo {
	return *c.info.Load()
}

// CheckSMBIOSAccess reports whether this process can read the SMBIOS table the
// module inventory comes from: nil when it can, otherwise the error opening
// it, which is fs.ErrPermission when access has not been granted and
// fs.ErrNotExist on firmware that publishes no table.
func CheckSMBIOSAccess() error {
	f, err := os.Open(smbiosTablePath)
	if err != nil {
		return &types.SysfsReadError{Path: smbiosTablePath, Cause: err}
	}
	if err := f.Close(); err != nil {
		return &types.SysfsReadError{Path: smbiosTablePath, Cause: err}
	}
	return nil
}
