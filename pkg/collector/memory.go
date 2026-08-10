package collector

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jaypipes/ghw"
	psmem "github.com/shirou/gopsutil/v4/mem"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// smbiosTablePath is the raw SMBIOS table. It is a variable rather than a
// constant so tests can supply a fixture table and exercise the parser without
// root, which reading the real one requires.
var smbiosTablePath = "/sys/firmware/dmi/tables/DMI"

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
}

// NewMemoryCollector returns a new MemoryCollector.
func NewMemoryCollector(logger *slog.Logger) *MemoryCollector {
	c := &MemoryCollector{logger: logger}
	c.info.Store(&types.MemoryInfo{})
	return c
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
	}

	if sw != nil {
		info.SwapTotalBytes = sw.Total
		info.SwapUsedBytes = sw.Used
		info.SwapFreeBytes = sw.Free
		info.SwapPercent = sw.UsedPercent
	}

	info.DIMMs = c.collectDIMMs()
	info.TempSensorDetected = applyDIMMTemperatures(info.DIMMs, c.logger)

	c.info.Store(info)
	return nil
}

// collectDIMMs reads DIMM information from the SMBIOS binary table, falling
// back to ghw if the table cannot be read (e.g. missing root privileges).
func (c *MemoryCollector) collectDIMMs() []types.DIMMInfo {
	// Attempt direct read first (succeeds when running as root).
	data, err := os.ReadFile(smbiosTablePath)
	if err != nil {
		// Fall back to a privileged read. Report the original permission
		// error if sudo is simply not installed -- which is the normal case
		// in a container -- because "sudo: executable file not found"
		// describes the fallback rather than the actual problem.
		directErr := err
		data, err = sudoReadFile(smbiosTablePath)
		if err != nil && errors.Is(err, exec.ErrNotFound) {
			err = directErr
		}
	}
	if err != nil {
		// Unreadable without root and unchanging for the process lifetime,
		// so report it once rather than on every collection cycle.
		warnOnce(c.logger, "memory:smbios",
			"memory: cannot read SMBIOS table, falling back to ghw",
			"path", smbiosTablePath, "error", err,
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

// dimmTempModules lists kernel modules to attempt loading when no DIMM
// temperature sensors are found in hwmon. Loading requires root privileges
// and is best-effort; failure is silently ignored.
var dimmTempModules = []string{"jc42", "spd5118"}

// applyDIMMTemperatures scans hwmon for DIMM temperature sensors (jc42 or
// spd5118 drivers) and assigns temperatures to DIMMs by index order.
// These sensors appear at /sys/class/hwmon/hwmonN/ with name "jc42" or
// "spd5118" and expose temp1_input in millidegrees Celsius.
// Returns true if at least one DIMM thermal sensor was detected.
func applyDIMMTemperatures(dimms []types.DIMMInfo, logger *slog.Logger) bool {
	if len(dimms) == 0 {
		return false
	}

	temps := scanDIMMHwmonTemps()
	if len(temps) == 0 {
		// No sensors found; attempt to load kernel modules, then instantiate
		// spd5118 devices on SMBus adapters for DDR5 systems where the kernel
		// lacks CONFIG_SENSORS_SPD5118_DETECT=y, and retry.
		//
		// This runs at most once per process. Memory is collected on the fast
		// tier, so retrying meant forking modprobe and writing to every SMBus
		// adapter once a second forever on a machine whose modules are simply
		// not available. One attempt settles it: if the sensors do not appear,
		// they will not appear on the ten-thousandth try either.
		probeDIMMSensorsOnce(logger)
		temps = scanDIMMHwmonTemps()
	}

	if len(temps) == 0 {
		warnOnce(nil, "memory:no-dimm-temps",
			"memory: no DIMM temperature sensors detected in hwmon",
			"hint", "ensure jc42 (DDR3/DDR4) or spd5118 (DDR5) kernel modules are loaded; "+
				"DDR5 may require CONFIG_SENSORS_SPD5118_DETECT=y in the kernel config",
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

// probeDIMMTempSensors loads the DIMM thermal kernel modules and instantiates
// spd5118 devices. It is a variable because it mutates host state (modprobe
// and i2c new_device writes), so tests replace it with a no-op.
var probeDIMMTempSensors = func(logger *slog.Logger) {
	loadDIMMTempModules(logger)
	instantiateSPD5118Devices(logger)
}

// dimmProbeOnce guards the sensor probe so it runs at most once per process.
var dimmProbeOnce sync.Once

// probeDIMMSensorsOnce runs the sensor probe the first time it is called and
// does nothing thereafter.
func probeDIMMSensorsOnce(logger *slog.Logger) {
	dimmProbeOnce.Do(func() { probeDIMMTempSensors(logger) })
}

// resetDIMMProbeOnce re-arms the probe. It exists for tests, which would
// otherwise see the guard consumed by an earlier case.
func resetDIMMProbeOnce() {
	dimmProbeOnce = sync.Once{}
}

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

// loadDIMMTempModules attempts best-effort loading of DIMM thermal sensor
// kernel modules. Requires root privileges; failures are logged at debug level.
func loadDIMMTempModules(logger *slog.Logger) {
	for _, mod := range dimmTempModules {
		// #nosec G204 -- mod comes from the fixed dimmTempModules list in this
		// package. There is no caller-supplied input on this path.
		out, err := exec.Command("modprobe", mod).CombinedOutput()
		if err != nil {
			logger.Debug("memory: could not load kernel module",
				"module", mod, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}
}

// smbusAdapterNames contains substrings that identify SMBus host adapters in
// the i2c device name file. Matching is case-insensitive.
var smbusAdapterNames = []string{"smbus", "piix4", "i801", "nforce2"}

// instantiateSPD5118Devices iterates over all i2c adapters in sysfs and, for
// each SMBus adapter, attempts to instantiate spd5118 devices at the DDR5 SPD
// EEPROM addresses (0x50–0x57). This is required on kernels built without
// CONFIG_SENSORS_SPD5118_DETECT=y, where the driver will not probe the bus
// automatically. Writing to new_device is best-effort; a write failure simply
// means no DIMM is present at that address and is silently ignored.
func instantiateSPD5118Devices(logger *slog.Logger) {
	const i2cBusRoot = "/sys/bus/i2c/devices"

	entries, err := os.ReadDir(i2cBusRoot)
	if err != nil {
		logger.Debug("memory: cannot read i2c devices directory", "error", err)
		return
	}

	for _, e := range entries {
		name := e.Name()
		// Only consider i2c-N adapter directories (not individual devices like "7-0050").
		if !strings.HasPrefix(name, "i2c-") {
			continue
		}
		busNum := name[4:] // strip "i2c-" prefix

		adapterPath := filepath.Join(i2cBusRoot, name)
		adapterName := strings.ToLower(readSysfsString(filepath.Join(adapterPath, "name")))

		isSMBus := false
		for _, keyword := range smbusAdapterNames {
			if strings.Contains(adapterName, keyword) {
				isSMBus = true
				break
			}
		}
		if !isSMBus {
			continue
		}

		logger.Debug("memory: found SMBus adapter, probing DDR5 SPD addresses",
			"bus", busNum, "adapter", adapterName)

		for addr := 0x50; addr <= 0x57; addr++ {
			// Sysfs device directory name format: "{bus}-00{addr_hex}", e.g. "7-0050".
			deviceDir := busNum + "-00" + itoh(addr)
			devicePath := filepath.Join(adapterPath, deviceDir)

			if _, statErr := os.Stat(devicePath); statErr == nil {
				// Device already exists at this address; skip instantiation.
				continue
			}

			newDevicePath := filepath.Join(adapterPath, "new_device")
			payload := []byte("spd5118 0x" + itoh(addr) + "\n")

			if writeErr := os.WriteFile(newDevicePath, payload, 0200); writeErr == nil {
				logger.Debug("memory: instantiated spd5118 device",
					"bus", busNum, "addr", "0x"+itoh(addr))
			}
			// Write errors are expected when no DIMM is present; silently ignored.
		}
	}
}

// itoh converts a non-negative integer to a zero-padded two-character
// lowercase hexadecimal string (e.g. 0x50 → "50", 0x07 → "07").
func itoh(n int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(n>>4)&0xf], hex[n&0xf]})
}

// sudoReadFile reads a file using sudo -n cat, used when direct access requires root.
func sudoReadFile(path string) ([]byte, error) {
	// #nosec G204 -- path is the package-level smbiosTablePath, a constant in
	// every build; only tests reassign it, to a fixture under t.TempDir().
	return exec.Command("sudo", "-n", "cat", path).Output()
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
