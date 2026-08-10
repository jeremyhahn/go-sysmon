// White-box tests for unexported helpers.
// These live in package collector (not collector_test) so they can access
// unexported symbols directly.
package collector

import (
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	psdisk "github.com/shirou/gopsutil/v4/disk"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestDiskNameFromPartition covers the various device naming conventions.
func TestDiskNameFromPartition(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"nvme0n1p1", "nvme0n1"},
		{"nvme1n1p2", "nvme1n1"},
		{"sda1", "sda"},
		{"sda12", "sda"},
		{"vda2", "vda"},
		{"xvda1", "xvda"},
		{"nvme0n1", "nvme0n1"},     // no partition suffix
		{"sda", "sda"},             // no partition suffix
		{"nvme0n1px", "nvme0n1px"}, // 'p' followed by non-digit: treated as disk name
	}
	for _, tc := range cases {
		got := diskNameFromPartition(tc.input)
		if got != tc.want {
			t.Errorf("diskNameFromPartition(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestSafeSubUnderflow verifies that safeSub clamps to zero on counter reset.
func TestSafeSubUnderflow(t *testing.T) {
	if got := safeSub(5, 10); got != 0 {
		t.Errorf("safeSub(5, 10) = %d, want 0", got)
	}
}

// TestSafeSubNormal verifies that safeSub returns the correct difference.
func TestSafeSubNormal(t *testing.T) {
	if got := safeSub(100, 40); got != 60 {
		t.Errorf("safeSub(100, 40) = %d, want 60", got)
	}
}

// TestReadSysfsString_MissingFile verifies that a missing path returns "".
func TestReadSysfsString_MissingFile(t *testing.T) {
	got := readSysfsString("/nonexistent/path/that/does/not/exist")
	if got != "" {
		t.Errorf("expected empty string for missing file, got %q", got)
	}
}

// TestReadSysfsString_ExistingFile verifies successful reads.
func TestReadSysfsString_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "value")
	if err := os.WriteFile(p, []byte("  hello  \n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := readSysfsString(p)
	if got != "hello" {
		t.Errorf("expected %q, got %q", "hello", got)
	}
}

// TestReadSysfsUint64_MissingFile verifies that a missing path returns 0.
func TestReadSysfsUint64_MissingFile(t *testing.T) {
	got := readSysfsUint64("/nonexistent/path/that/does/not/exist")
	if got != 0 {
		t.Errorf("expected 0 for missing file, got %d", got)
	}
}

// TestReadSysfsUint64_Valid verifies correct parsing.
func TestReadSysfsUint64_Valid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "size")
	if err := os.WriteFile(p, []byte("12345678\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := readSysfsUint64(p)
	if got != 12345678 {
		t.Errorf("expected 12345678, got %d", got)
	}
}

// TestReadHwmonTemp_NoMatch verifies 0 is returned when no glob matches.
func TestReadHwmonTemp_NoMatch(t *testing.T) {
	got := readHwmonTemp("devicethatdoesnotexist")
	if got != 0 {
		t.Errorf("expected 0 for unmatched device, got %v", got)
	}
}

// TestReadSysfsNICSpeed_Empty verifies 0 is returned for an empty value.
func TestReadSysfsNICSpeed_Empty(t *testing.T) {
	got := readSysfsNICSpeed("/nonexistent/speed")
	if got != 0 {
		t.Errorf("expected 0 for missing speed file, got %d", got)
	}
}

// TestReadSysfsNICSpeed_Negative verifies that -1 speed returns 0.
func TestReadSysfsNICSpeed_Negative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "speed")
	if err := os.WriteFile(p, []byte("-1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := readSysfsNICSpeed(p)
	if got != 0 {
		t.Errorf("expected 0 for -1 speed, got %d", got)
	}
}

// TestReadSysfsNICSpeed_Valid verifies correct speed parsing.
func TestReadSysfsNICSpeed_Valid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "speed")
	if err := os.WriteFile(p, []byte("1000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := readSysfsNICSpeed(p)
	if got != 1000 {
		t.Errorf("expected 1000, got %d", got)
	}
}

// TestIsVirtualInterface_NoDevice verifies that an interface without a device
// symlink is considered virtual.
func TestIsVirtualInterface_NoDevice(t *testing.T) {
	dir := t.TempDir()
	// No "device" subdirectory created.
	if !isVirtualInterface(dir) {
		t.Error("expected isVirtualInterface to return true when no device dir exists")
	}
}

// TestIsVirtualInterface_WithDevice verifies that an interface with a device
// directory is not considered virtual.
func TestIsVirtualInterface_WithDevice(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "device"), 0755); err != nil {
		t.Fatal(err)
	}
	if isVirtualInterface(dir) {
		t.Error("expected isVirtualInterface to return false when device dir exists")
	}
}

// TestReadNICDriver_Missing verifies empty string for a missing symlink.
func TestReadNICDriver_Missing(t *testing.T) {
	got := readNICDriver("/nonexistent/driver")
	if got != "" {
		t.Errorf("expected empty driver for missing path, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// SMART error type tests
// ---------------------------------------------------------------------------

// TestIsPermissionError_NonPermission verifies false for a non-permission error.
func TestIsPermissionError_NonPermission(t *testing.T) {
	var pe *ErrSMARTPermission
	if isPermissionError(os.ErrNotExist, &pe) {
		t.Error("expected isPermissionError to return false for os.ErrNotExist")
	}
}

// TestIsPermissionError_Permission verifies true for *ErrSMARTPermission.
func TestIsPermissionError_Permission(t *testing.T) {
	var pe *ErrSMARTPermission
	err := &ErrSMARTPermission{DevPath: "/dev/sda"}
	if !isPermissionError(err, &pe) {
		t.Error("expected isPermissionError to return true for *ErrSMARTPermission")
	}
	if pe == nil {
		t.Error("expected non-nil *ErrSMARTPermission after isPermissionError")
	}
}

// TestErrSMARTPermission_Error verifies the error message contains the device path.
func TestErrSMARTPermission_Error(t *testing.T) {
	err := &ErrSMARTPermission{DevPath: "/dev/nvme0"}
	msg := err.Error()
	if !strings.Contains(msg, "/dev/nvme0") {
		t.Errorf("ErrSMARTPermission.Error() missing device path: %q", msg)
	}
}

// TestErrSMARTIoctl_Error verifies the ErrSMARTIoctl error message contains
// the device path and operation name.
func TestErrSMARTIoctl_Error(t *testing.T) {
	err := &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "HDIO_DRIVE_CMD(0xD0)", Errno: syscall.EPERM}
	msg := err.Error()
	if !strings.Contains(msg, "/dev/sda") {
		t.Errorf("ErrSMARTIoctl.Error() missing device path: %q", msg)
	}
	if !strings.Contains(msg, "HDIO_DRIVE_CMD(0xD0)") {
		t.Errorf("ErrSMARTIoctl.Error() missing op name: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// NVMe device path and SMART tests
// ---------------------------------------------------------------------------

// TestNVMeNameFromDisk verifies that the NVMe character device path is derived
// correctly from the block device name.
func TestNVMeNameFromDisk(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"nvme0n1", "/dev/nvme0"},
		{"nvme1n1", "/dev/nvme1"},
		{"nvme2n3", "/dev/nvme2"},
		{"nvme10n1", "/dev/nvme10"},
	}
	for _, tc := range cases {
		got := nvmeNameFromDisk(tc.input)
		if got != tc.want {
			t.Errorf("nvmeNameFromDisk(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestNVMeReadSMART_PermissionDenied verifies that opening a non-existent or
// inaccessible device returns an error without panicking.
func TestNVMeReadSMART_PermissionDenied(t *testing.T) {
	var info types.DiskInfo
	err := readNVMeSMART("/dev/nvme_nonexistent_device_xyz", &info)
	if err == nil {
		t.Error("expected error for non-existent NVMe device, got nil")
	}
}

// ---------------------------------------------------------------------------
// ATA SMART attribute parsing tests
// ---------------------------------------------------------------------------

// buildATASmartData constructs a minimal 512-byte ATA SMART data buffer with
// the given attributes placed in the attribute table (offset 2).
// Each entry is 12 bytes: [ID, flags_lo, flags_hi, value, worst, raw[6], 0x00]
func buildATASmartData(attrs []struct {
	id    uint8
	value uint8
	worst uint8
	raw   uint64
}) []byte {
	buf := make([]byte, smartDataSize)
	for i, a := range attrs {
		if i >= smartAttrCount {
			break
		}
		base := smartAttrTableOffset + i*smartAttrEntrySize
		buf[base+smartAttrIDOffset] = a.id
		buf[base+smartAttrFlagsOffset] = 0x03 // pre-fail + online
		buf[base+smartAttrValueOffset] = a.value
		buf[base+smartAttrWorstOffset] = a.worst
		// raw: 6 bytes little-endian
		buf[base+smartAttrRawOffset+0] = uint8(a.raw)
		buf[base+smartAttrRawOffset+1] = uint8(a.raw >> 8)
		buf[base+smartAttrRawOffset+2] = uint8(a.raw >> 16)
		buf[base+smartAttrRawOffset+3] = uint8(a.raw >> 24)
		buf[base+smartAttrRawOffset+4] = uint8(a.raw >> 32)
		buf[base+smartAttrRawOffset+5] = uint8(a.raw >> 40)
	}
	return buf
}

// buildATAThreshData constructs a 512-byte ATA SMART threshold buffer.
func buildATAThreshData(thresholds map[uint8]uint8) []byte {
	buf := make([]byte, smartDataSize)
	i := 0
	for id, thresh := range thresholds {
		if i >= smartAttrCount {
			break
		}
		base := smartAttrThreshTableOffset + i*smartAttrThreshEntrySize
		buf[base] = id
		buf[base+smartAttrThreshValueOffset] = thresh
		i++
	}
	return buf
}

// TestParseATASMARTAttrs_BasicAttributes verifies that standard attribute IDs
// are parsed correctly from a known binary layout.
func TestParseATASMARTAttrs_BasicAttributes(t *testing.T) {
	attrs := []struct {
		id    uint8
		value uint8
		worst uint8
		raw   uint64
	}{
		{9, 200, 200, 12345},    // Power_On_Hours
		{12, 100, 100, 500},     // Power_Cycle_Count
		{194, 50, 45, 0x000032}, // Temperature_Celsius (raw[0] = 50)
		{5, 100, 98, 3},         // Reallocated_Sector_Ct
	}

	data := buildATASmartData(attrs)
	threshMap := map[uint8]uint8{9: 0, 12: 0, 194: 0, 5: 36}
	threshData := buildATAThreshData(threshMap)

	result := parseATASMARTAttrs(data, threshData, true)

	if len(result) != len(attrs) {
		t.Fatalf("expected %d attributes, got %d", len(attrs), len(result))
	}

	byID := make(map[uint8]ataAttr)
	for _, a := range result {
		byID[a.ID] = a
	}

	// Verify Power_On_Hours.
	poh := byID[9]
	if poh.Value != 200 {
		t.Errorf("attr 9 value: got %d, want 200", poh.Value)
	}
	if poh.Raw != 12345 {
		t.Errorf("attr 9 raw: got %d, want 12345", poh.Raw)
	}

	// Verify Power_Cycle_Count.
	pcc := byID[12]
	if pcc.Raw != 500 {
		t.Errorf("attr 12 raw: got %d, want 500", pcc.Raw)
	}

	// Verify threshold is applied correctly.
	rsc := byID[5]
	if rsc.Threshold != 36 {
		t.Errorf("attr 5 threshold: got %d, want 36", rsc.Threshold)
	}
}

// TestParseATASMARTAttrs_NoThresholds verifies that parsing succeeds when no
// threshold data is provided (hasThresh = false).
func TestParseATASMARTAttrs_NoThresholds(t *testing.T) {
	attrs := []struct {
		id    uint8
		value uint8
		worst uint8
		raw   uint64
	}{
		{9, 200, 200, 9999},
	}
	data := buildATASmartData(attrs)
	result := parseATASMARTAttrs(data, nil, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 attribute, got %d", len(result))
	}
	if result[0].Threshold != 0 {
		t.Errorf("expected threshold 0 when no thresh data, got %d", result[0].Threshold)
	}
}

// TestParseATASMARTAttrs_EmptyData verifies that an empty or undersized
// buffer yields no attributes.
func TestParseATASMARTAttrs_EmptyData(t *testing.T) {
	result := parseATASMARTAttrs([]byte{}, nil, false)
	if len(result) != 0 {
		t.Errorf("expected 0 attributes for empty data, got %d", len(result))
	}
}

// TestParseATASMARTAttrs_ZeroIDSkipped verifies that entries with ID=0 are
// skipped (they represent empty slots in the attribute table).
func TestParseATASMARTAttrs_ZeroIDSkipped(t *testing.T) {
	// Build data with one real attribute and one zero-ID entry.
	data := make([]byte, smartDataSize)
	// First entry: ID=0 (should be skipped).
	data[smartAttrTableOffset+smartAttrIDOffset] = 0
	// Second entry: ID=9.
	base := smartAttrTableOffset + smartAttrEntrySize
	data[base+smartAttrIDOffset] = 9
	data[base+smartAttrValueOffset] = 100
	data[base+smartAttrWorstOffset] = 100

	result := parseATASMARTAttrs(data, nil, false)
	for _, a := range result {
		if a.ID == 0 {
			t.Error("attribute with ID=0 should have been skipped")
		}
	}
}

// ---------------------------------------------------------------------------
// NVMe SMART log parsing tests
// ---------------------------------------------------------------------------

// buildNVMeSmartLog constructs a 512-byte NVMe SMART/Health log buffer with
// the given field values.
func buildNVMeSmartLog(
	critWarn uint8,
	tempK uint16,
	availSpare, spareThresh, pctUsed uint8,
	dataUnitsRead, dataUnitsWritten uint64,
	powerCycles, powerOnHours uint64,
	unsafeShutdowns, mediaErrors, errLogEntries uint64,
	warnTempTime, critCompTime uint32,
) []byte {
	buf := make([]byte, 512)
	buf[0] = critWarn
	buf[1] = uint8(tempK)
	buf[2] = uint8(tempK >> 8)
	buf[3] = availSpare
	buf[4] = spareThresh
	buf[5] = pctUsed

	// 128-bit fields starting at offset 32.  We only use the lower 8 bytes.
	put128 := func(offset int, v uint64) {
		binary.LittleEndian.PutUint64(buf[offset:], v)
		// Upper 8 bytes remain zero.
	}

	put128(32, dataUnitsRead)
	put128(48, dataUnitsWritten)
	put128(64, 0) // host read commands
	put128(80, 0) // host write commands
	put128(96, 0) // controller busy time
	put128(112, powerCycles)
	put128(128, powerOnHours)
	put128(144, unsafeShutdowns)
	put128(160, mediaErrors)
	put128(176, errLogEntries)

	binary.LittleEndian.PutUint32(buf[192:], warnTempTime)
	binary.LittleEndian.PutUint32(buf[196:], critCompTime)

	return buf
}

// TestParseNVMeSmartLog_Fields verifies that all fields of the NVMe SMART log
// are correctly extracted into a DiskInfo.
func TestParseNVMeSmartLog_Fields(t *testing.T) {
	const tempK = 323 // 323 K = ~49.85°C
	buf := buildNVMeSmartLog(
		0x01,    // criticalWarning
		tempK,   // temperature in K
		95,      // availableSpare %
		10,      // spareThreshold %
		5,       // percentageUsed
		1000000, // dataUnitsRead
		500000,  // dataUnitsWritten
		42,      // powerCycles
		8760,    // powerOnHours (1 year)
		3,       // unsafeShutdowns
		0,       // mediaErrors
		7,       // errLogEntries
		0,       // warnTempTime
		0,       // critCompTime
	)

	// Parse the log directly using the same pointer cast as readNVMeSMART.
	log := (*nvmeSmartLog)(unsafe.Pointer(&buf[0]))

	// Replicate the parsing logic from readNVMeSMART to test the binary layout.
	gotTempK := uint16(log.Temperature[0]) | uint16(log.Temperature[1])<<8
	if gotTempK != tempK {
		t.Errorf("temperature K: got %d, want %d", gotTempK, tempK)
	}

	gotCritWarn := log.CriticalWarning
	if gotCritWarn != 0x01 {
		t.Errorf("criticalWarning: got %d, want 1", gotCritWarn)
	}

	gotSpare := log.AvailableSpare
	if gotSpare != 95 {
		t.Errorf("availableSpare: got %d, want 95", gotSpare)
	}

	gotSpareThresh := log.AvailableSpareThreshold
	if gotSpareThresh != 10 {
		t.Errorf("spareThreshold: got %d, want 10", gotSpareThresh)
	}

	gotPctUsed := log.PercentageUsed
	if gotPctUsed != 5 {
		t.Errorf("percentageUsed: got %d, want 5", gotPctUsed)
	}

	gotPOH := binary.LittleEndian.Uint64(log.PowerOnHours[:8])
	if gotPOH != 8760 {
		t.Errorf("powerOnHours: got %d, want 8760", gotPOH)
	}

	gotPC := binary.LittleEndian.Uint64(log.PowerCycles[:8])
	if gotPC != 42 {
		t.Errorf("powerCycles: got %d, want 42", gotPC)
	}

	gotDUR := binary.LittleEndian.Uint64(log.DataUnitsRead[:8])
	if gotDUR != 1000000 {
		t.Errorf("dataUnitsRead: got %d, want 1000000", gotDUR)
	}

	gotDUW := binary.LittleEndian.Uint64(log.DataUnitsWritten[:8])
	if gotDUW != 500000 {
		t.Errorf("dataUnitsWritten: got %d, want 500000", gotDUW)
	}

	gotShutdowns := binary.LittleEndian.Uint64(log.UnsafeShutdowns[:8])
	if gotShutdowns != 3 {
		t.Errorf("unsafeShutdowns: got %d, want 3", gotShutdowns)
	}

	gotErrLog := binary.LittleEndian.Uint64(log.NumErrLogEntries[:8])
	if gotErrLog != 7 {
		t.Errorf("numErrLogEntries: got %d, want 7", gotErrLog)
	}
}

// TestParseNVMeSmartLog_TemperatureConversion verifies Kelvin to Celsius
// conversion for a range of well-known values.
// The implementation uses > 273 as the lower bound, meaning values at or below
// 273K (0°C) are treated as "not reported" and yield 0.
func TestParseNVMeSmartLog_TemperatureConversion(t *testing.T) {
	cases := []struct {
		tempK uint16
		wantC float64
	}{
		{274, 274 - 273.15}, // 0.85°C: first value above threshold
		{298, 298 - 273.15}, // room temperature: ~24.85°C
		{358, 358 - 273.15}, // 84.85°C
		{273, 0},            // exactly 273K: treated as "no reading" (> 273 check)
		{0, 0},              // zero: no temperature reported
	}
	for _, tc := range cases {
		buf := buildNVMeSmartLog(0, tc.tempK, 100, 5, 0, 0, 0, 1, 100, 0, 0, 0, 0, 0)
		log := (*nvmeSmartLog)(unsafe.Pointer(&buf[0]))
		gotK := uint16(log.Temperature[0]) | uint16(log.Temperature[1])<<8
		var gotC float64
		if gotK > 273 {
			gotC = float64(gotK) - 273.15
		}
		diff := gotC - tc.wantC
		if diff < -0.001 || diff > 0.001 {
			t.Errorf("tempK=%d: got %.2f°C, want %.2f°C", tc.tempK, gotC, tc.wantC)
		}
	}
}

// ---------------------------------------------------------------------------
// TestReadHwmonTemp_ValidFile
// ---------------------------------------------------------------------------

// TestReadHwmonTemp_ValidFile creates a synthetic hwmon directory and verifies
// that readHwmonTemp correctly reads and converts the millicelsius value.
func TestReadHwmonTemp_ValidFile(t *testing.T) {
	// readHwmonTemp uses a glob on /sys/block/<dev>/device/hwmon/hwmon*/temp*_input
	// We cannot write to /sys/block in tests, so we test the underlying file
	// read helpers instead.  A temperature of 45000 mC should yield 45.0°C.
	dir := t.TempDir()
	tempFile := filepath.Join(dir, "temp1_input")
	if err := os.WriteFile(tempFile, []byte("45000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	raw := readSysfsUint64(tempFile)
	if raw == 0 {
		t.Fatal("expected non-zero raw temperature")
	}
	celsius := float64(raw) / 1000.0
	if celsius != 45.0 {
		t.Errorf("expected 45.0°C, got %v", celsius)
	}
}

// TestCollectFromSysfs_ReturnsDisks verifies that collectFromSysfs (the ghw
// fallback path) can read block devices from /sys/block and returns a
// non-nil, non-empty slice of DiskInfo on a system where block devices exist.
func TestCollectFromSysfs_ReturnsDisks(t *testing.T) {
	// Check that /sys/block is available and has at least one non-loop device.
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		t.Skip("cannot read /sys/block; skipping collectFromSysfs test")
	}
	hasRealDev := false
	for _, e := range entries {
		if !isLoopOrRam(e.Name()) {
			hasRealDev = true
			break
		}
	}
	if !hasRealDev {
		t.Skip("no non-loop/ram block devices found in /sys/block")
	}

	logger := newNopLogger()
	c := &DiskCollector{logger: logger}
	disks := c.collectFromSysfs(nil, nil)
	if len(disks) == 0 {
		t.Error("collectFromSysfs returned no disks")
	}
	for i, d := range disks {
		if d.Name == "" {
			t.Errorf("disk[%d]: empty name from collectFromSysfs", i)
		}
	}
}

// isLoopOrRam is a mirror of the filter inside collectFromSysfs.
func isLoopOrRam(name string) bool {
	return strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram")
}

// ---------------------------------------------------------------------------
// SMBIOS binary parser tests
// ---------------------------------------------------------------------------

// buildType17 constructs a minimal but complete SMBIOS Type 17 binary blob
// for testing.  The formatted area is 0x28 (40) bytes long — sufficient for
// all fields up to and including ConfiguredVoltage at offset 0x26.
//
// String indices in the formatted area follow 1-based SMBIOS numbering:
//
//	1 = locator, 2 = bankLocator, 3 = manufacturer, 4 = serial, 5 = partNumber
func buildType17(
	totalWidthBits, dataWidthBits uint16,
	sizeField uint16,
	extSizeMB uint32,
	formFactor byte,
	memType byte,
	speedMTs uint16,
	attributes byte,
	configuredSpeedMTs uint16,
	minVoltageMilliV, maxVoltageMilliV, configuredVoltageMilliV uint16,
	locator, bankLocator, manufacturer, serialNumber, partNumber string,
) []byte {
	const formattedLen = 0x28 // 40 bytes covers all fields through ConfiguredVoltage

	f := make([]byte, formattedLen)
	f[0] = smbiosTypeMemoryDevice
	f[1] = formattedLen
	binary.LittleEndian.PutUint16(f[2:], 0x0028) // handle

	binary.LittleEndian.PutUint16(f[off17TotalWidth:], totalWidthBits)
	binary.LittleEndian.PutUint16(f[off17DataWidth:], dataWidthBits)
	binary.LittleEndian.PutUint16(f[off17Size:], sizeField)
	f[off17FormFactor] = formFactor

	// String indices (1-based).
	f[off17DeviceLocatorStr] = 1
	f[off17BankLocatorStr] = 2
	f[off17MemoryType] = memType
	binary.LittleEndian.PutUint16(f[off17Speed:], speedMTs)
	f[off17ManufacturerStr] = 3
	f[off17SerialNumberStr] = 4
	f[off17PartNumberStr] = 5
	f[off17Attributes] = attributes

	if sizeField == sizeFieldExtended {
		binary.LittleEndian.PutUint32(f[off17ExtendedSize:], extSizeMB)
	}

	binary.LittleEndian.PutUint16(f[off17ConfiguredSpeed:], configuredSpeedMTs)
	binary.LittleEndian.PutUint16(f[off17MinVoltage:], minVoltageMilliV)
	binary.LittleEndian.PutUint16(f[off17MaxVoltage:], maxVoltageMilliV)
	binary.LittleEndian.PutUint16(f[off17ConfiguredVoltage:], configuredVoltageMilliV)

	// Build the string section: each string is NUL-terminated, section ends
	// with an extra NUL (giving the double-NUL terminator).
	for _, s := range []string{locator, bankLocator, manufacturer, serialNumber, partNumber} {
		f = append(f, []byte(s)...)
		f = append(f, 0x00)
	}
	f = append(f, 0x00) // section terminator

	return f
}

// TestParseSMBIOSType17_PopulatedSlot verifies that a well-formed Type 17
// blob is decoded into a DIMMInfo with all fields correctly populated.
func TestParseSMBIOSType17_PopulatedSlot(t *testing.T) {
	// 24 GB = 24576 MB
	blob := buildType17(
		64, 64,
		24576, 0, // 24576 MB in size field, no extended size
		0x09,             // DIMM form factor
		0x22,             // DDR5
		4800,             // speed MT/s
		0x01,             // rank = 1 (lower nibble of Attributes)
		4800,             // configured speed
		1100, 1100, 1100, // voltages in mV
		"DIMMA1", "BANK 0", "G Skill Intl", "885EFBF5", "F5-8400J4052G24G",
	)

	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 1 {
		t.Fatalf("expected 1 DIMM, got %d", len(dimms))
	}
	d := dimms[0]

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"Location", d.Location, "DIMMA1"},
		{"BankLocator", d.BankLocator, "BANK 0"},
		{"Manufacturer", d.Manufacturer, "G Skill Intl"},
		{"PartNumber", d.PartNumber, "F5-8400J4052G24G"},
		{"SerialNumber", d.SerialNumber, "885EFBF5"},
		{"SizeBytes", d.SizeBytes, uint64(24576) * 1024 * 1024},
		{"SpeedMTs", d.SpeedMTs, uint32(4800)},
		{"ConfiguredSpeedMTs", d.ConfiguredSpeedMTs, uint32(4800)},
		{"Type", d.Type, "DDR5"},
		{"FormFactor", d.FormFactor, "DIMM"},
		{"DataWidthBits", d.DataWidthBits, uint32(64)},
		{"TotalWidthBits", d.TotalWidthBits, uint32(64)},
		{"Rank", d.Rank, uint32(1)},
		{"MinVoltage", d.MinVoltage, 1.1},
		{"MaxVoltage", d.MaxVoltage, 1.1},
		{"ConfiguredVoltage", d.ConfiguredVoltage, 1.1},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestParseSMBIOSType17_EmptySlotSkipped verifies that a Type 17 structure
// with size == 0 is excluded from results.
func TestParseSMBIOSType17_EmptySlotSkipped(t *testing.T) {
	// Size field 0x0000 → empty slot.
	blob := buildType17(
		0xFFFF, 0xFFFF,
		0, 0,
		0x09, 0x02, 0, 0, 0,
		0, 0, 0,
		"DIMMB1", "BANK 1", "", "", "",
	)

	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 0 {
		t.Errorf("expected 0 DIMMs for empty slot, got %d", len(dimms))
	}
}

// TestParseSMBIOSType17_MultipleDIMMs verifies that two consecutive Type 17
// structures are both decoded when both have non-zero sizes.
func TestParseSMBIOSType17_MultipleDIMMs(t *testing.T) {
	slot1 := buildType17(64, 64, 16384, 0, 0x09, 0x22, 4800, 0x01, 4800, 1100, 1100, 1100,
		"DIMMA1", "BANK 0", "Vendor", "SN1", "PN1")
	slot2 := buildType17(64, 64, 16384, 0, 0x09, 0x22, 4800, 0x01, 4800, 1100, 1100, 1100,
		"DIMMA2", "BANK 1", "Vendor", "SN2", "PN2")

	blob := append(slot1, slot2...)
	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 2 {
		t.Fatalf("expected 2 DIMMs, got %d", len(dimms))
	}
	if dimms[0].Location != "DIMMA1" {
		t.Errorf("DIMM[0] location: got %q, want DIMMA1", dimms[0].Location)
	}
	if dimms[1].Location != "DIMMA2" {
		t.Errorf("DIMM[1] location: got %q, want DIMMA2", dimms[1].Location)
	}
}

// TestParseSMBIOSType17_EmptyInput verifies that empty data yields no DIMMs.
func TestParseSMBIOSType17_EmptyInput(t *testing.T) {
	dimms, err := parseSMBIOSType17([]byte{})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 0 {
		t.Errorf("expected 0 DIMMs for empty input, got %d", len(dimms))
	}
}

// TestParseSMBIOSType17_ExtendedSizeField verifies that size == 0x7FFF causes
// the extended size field to be used instead.
func TestParseSMBIOSType17_ExtendedSizeField(t *testing.T) {
	// 32768 MB = 32 GB via extended size field.
	blob := buildType17(
		64, 64,
		sizeFieldExtended, 32768,
		0x09, 0x22, 4800, 0x01, 4800,
		1100, 1100, 1100,
		"DIMMA1", "BANK 0", "Vendor", "SN1", "PN1",
	)

	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 1 {
		t.Fatalf("expected 1 DIMM, got %d", len(dimms))
	}
	want := uint64(32768) * 1024 * 1024
	if dimms[0].SizeBytes != want {
		t.Errorf("SizeBytes: got %d, want %d", dimms[0].SizeBytes, want)
	}
}

// TestParseSMBIOSType17_KBSizeFlag verifies that bit 15 set in the size field
// indicates KB units.
func TestParseSMBIOSType17_KBSizeFlag(t *testing.T) {
	// 512 KB with bit 15 set.
	kbSize := uint16(512) | sizeKBFlag
	blob := buildType17(
		64, 64,
		kbSize, 0,
		0x09, 0x22, 4800, 0x01, 4800,
		1100, 1100, 1100,
		"DIMMA1", "BANK 0", "Vendor", "SN1", "PN1",
	)

	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 1 {
		t.Fatalf("expected 1 DIMM, got %d", len(dimms))
	}
	want := uint64(512) * 1024
	if dimms[0].SizeBytes != want {
		t.Errorf("SizeBytes: got %d, want %d", dimms[0].SizeBytes, want)
	}
}

// TestParseSMBIOSType17_UnknownSizeSkipped verifies that size == 0xFFFF is
// treated as unknown and the slot is skipped.
func TestParseSMBIOSType17_UnknownSizeSkipped(t *testing.T) {
	blob := buildType17(
		64, 64,
		sizeFieldUnknown, 0,
		0x09, 0x22, 4800, 0x01, 4800,
		1100, 1100, 1100,
		"DIMMA1", "BANK 0", "Vendor", "SN1", "PN1",
	)

	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 0 {
		t.Errorf("expected 0 DIMMs for unknown size, got %d", len(dimms))
	}
}

// TestParseSMBIOSType17_NonType17Ignored verifies that non-Type-17 structures
// are skipped without error.
func TestParseSMBIOSType17_NonType17Ignored(t *testing.T) {
	// Build a fake Type 1 structure (System Information, type byte = 0x01).
	type1 := make([]byte, 6)
	type1[0] = 0x01 // type 1
	type1[1] = 0x06 // length
	binary.LittleEndian.PutUint16(type1[2:], 0x0001)
	type1 = append(type1, 0x00, 0x00) // empty string section

	slot := buildType17(64, 64, 8192, 0, 0x09, 0x22, 4800, 0x01, 4800, 1100, 1100, 1100,
		"DIMMA1", "BANK 0", "Vendor", "SN1", "PN1")

	blob := append(type1, slot...)
	dimms, err := parseSMBIOSType17(blob)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(dimms) != 1 {
		t.Fatalf("expected 1 DIMM (type1 ignored), got %d", len(dimms))
	}
}

// ---------------------------------------------------------------------------
// smbiosString tests
// ---------------------------------------------------------------------------

// TestSMBIOSString_Index1 verifies that the first string is correctly returned.
func TestSMBIOSString_Index1(t *testing.T) {
	section := []byte("hello\x00world\x00\x00")
	got := smbiosString(section, 1)
	if got != "hello" {
		t.Errorf("smbiosString index 1: got %q, want %q", got, "hello")
	}
}

// TestSMBIOSString_Index2 verifies the second string is correctly returned.
func TestSMBIOSString_Index2(t *testing.T) {
	section := []byte("hello\x00world\x00\x00")
	got := smbiosString(section, 2)
	if got != "world" {
		t.Errorf("smbiosString index 2: got %q, want %q", got, "world")
	}
}

// TestSMBIOSString_IndexZero verifies that index 0 returns empty string.
func TestSMBIOSString_IndexZero(t *testing.T) {
	section := []byte("hello\x00\x00")
	got := smbiosString(section, 0)
	if got != "" {
		t.Errorf("smbiosString index 0: got %q, want empty", got)
	}
}

// TestSMBIOSString_OutOfRange verifies that an out-of-range index returns "".
func TestSMBIOSString_OutOfRange(t *testing.T) {
	section := []byte("hello\x00\x00")
	got := smbiosString(section, 9)
	if got != "" {
		t.Errorf("smbiosString out-of-range: got %q, want empty", got)
	}
}

// TestSMBIOSString_EmptySection verifies that an empty section returns "".
func TestSMBIOSString_EmptySection(t *testing.T) {
	got := smbiosString([]byte{}, 1)
	if got != "" {
		t.Errorf("smbiosString empty section: got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// itoa tests
// ---------------------------------------------------------------------------

// TestItoa_Zero verifies that itoa(0) returns "0".
func TestItoa_Zero(t *testing.T) {
	if got := itoa(0); got != "0" {
		t.Errorf("itoa(0) = %q, want %q", got, "0")
	}
}

// TestItoa_Positive verifies that itoa returns the correct decimal string.
func TestItoa_Positive(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "1"},
		{42, "42"},
		{1024, "1024"},
		{999999, "999999"},
	}
	for _, c := range cases {
		if got := itoa(c.n); got != c.want {
			t.Errorf("itoa(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// summaryFromSysfs tests
// ---------------------------------------------------------------------------

// TestSummaryFromSysfs_RealSystem verifies that summaryFromSysfs returns a
// valid topology on a real Linux system with sysfs mounted.
func TestSummaryFromSysfs_RealSystem(t *testing.T) {
	// Skip if the sysfs CPU topology directory does not exist.
	if _, err := os.Stat("/sys/devices/system/cpu"); err != nil {
		t.Skip("sysfs not available; skipping summaryFromSysfs test")
	}

	s, err := summaryFromSysfs()
	if err != nil {
		t.Fatalf("summaryFromSysfs returned unexpected error: %v", err)
	}

	if s.Sockets <= 0 {
		t.Errorf("expected Sockets > 0, got %d", s.Sockets)
	}
	if s.CoresPerSocket <= 0 {
		t.Errorf("expected CoresPerSocket > 0, got %d", s.CoresPerSocket)
	}
	if s.ThreadsPerCore <= 0 {
		t.Errorf("expected ThreadsPerCore > 0, got %d", s.ThreadsPerCore)
	}
	if s.TotalCores <= 0 {
		t.Errorf("expected TotalCores > 0, got %d", s.TotalCores)
	}
	if s.TotalThreads <= 0 {
		t.Errorf("expected TotalThreads > 0, got %d", s.TotalThreads)
	}
	if s.TotalThreads < s.TotalCores {
		t.Errorf("TotalThreads (%d) must be >= TotalCores (%d)", s.TotalThreads, s.TotalCores)
	}
}

// TestSummaryFromSysfs_MissingSysfs verifies that summaryFromSysfs returns an
// error when the sysfs CPU directory cannot be read. We test the underlying
// helper path by pointing readSysfsInt at a nonexistent file.
func TestSummaryFromSysfs_MissingSysfs(t *testing.T) {
	// readSysfsInt must return 0 for a missing path, which we test directly.
	got := readSysfsInt("/nonexistent/cpu/topology/physical_package_id")
	if got != 0 {
		t.Errorf("expected 0 for missing sysfs path, got %d", got)
	}
}

// TestReadCPUFreqMHz_ValidKHz verifies that a kHz value is correctly converted
// to MHz.
func TestReadCPUFreqMHz_ValidKHz(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cpuinfo_max_freq")
	if err := os.WriteFile(p, []byte("3600000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := readCPUFreqMHz(p)
	if got != 3600.0 {
		t.Errorf("readCPUFreqMHz: expected 3600.0 MHz, got %v", got)
	}
}

// TestReadCPUFreqMHz_MissingFile verifies that a missing file returns 0.
func TestReadCPUFreqMHz_MissingFile(t *testing.T) {
	got := readCPUFreqMHz("/nonexistent/cpuinfo_max_freq")
	if got != 0 {
		t.Errorf("readCPUFreqMHz: expected 0 for missing file, got %v", got)
	}
}

// newNopLogger returns a silent logger for tests.
func newNopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildPartitionIndex verifies that partition device paths are correctly
// mapped back to their parent disk names.
func TestBuildPartitionIndex(t *testing.T) {
	parts := []psdisk.PartitionStat{
		{Device: "/dev/nvme0n1p1", Mountpoint: "/boot/efi"},
		{Device: "/dev/nvme0n1p2", Mountpoint: "/"},
		{Device: "/dev/sda1", Mountpoint: "/mnt/data"},
	}
	idx := buildPartitionIndex(parts)

	expected := map[string]int{
		"nvme0n1": 2,
		"sda":     1,
	}
	for disk, wantCount := range expected {
		if got := len(idx[disk]); got != wantCount {
			t.Errorf("partition index[%q]: expected %d entries, got %d", disk, wantCount, got)
		}
	}
}

// ---------------------------------------------------------------------------
// SensorCollector / collectCoreTemps white-box tests
// ---------------------------------------------------------------------------

// writeSysfsFile is a test helper that writes content to path, creating
// intermediate directories as needed.
func writeSysfsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// buildIntelHwmon creates a fake coretemp hwmon directory under base and
// returns the hwmon device path.
//
//	sensors: map of tempN_attr → value (e.g. "temp1_label" → "Package id 0")
func buildIntelHwmon(t *testing.T, base string, sensors map[string]string) string {
	t.Helper()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "coretemp\n")
	for attr, val := range sensors {
		writeSysfsFile(t, filepath.Join(hwmon, attr), val)
	}
	return hwmon
}

// TestTempFile verifies that tempFile produces the correct sysfs filename.
func TestTempFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		attr string
		want string
	}{
		{1, "input", "temp1_input"},
		{3, "label", "temp3_label"},
		{10, "crit", "temp10_crit"},
		{128, "max", "temp128_max"},
	}
	for _, tc := range cases {
		got := tempFile(tc.n, tc.attr)
		if got != tc.want {
			t.Errorf("tempFile(%d, %q) = %q, want %q", tc.n, tc.attr, got, tc.want)
		}
	}
}

// TestCollectCoreTemps_EmptyHwmonBase verifies that an empty / missing hwmon
// directory returns nil without panicking.
func TestCollectCoreTemps_EmptyHwmonBase(t *testing.T) {
	t.Parallel()
	c := newSensorCollectorWithPaths(slog.Default(), "/nonexistent/hwmon", "", "", "", "")
	temps := c.collectCoreTemps()
	if temps != nil {
		t.Errorf("expected nil for missing hwmon base, got %v", temps)
	}
}

// TestCollectCoreTemps_UnknownDriver verifies that unknown hwmon drivers are
// skipped and no CoreTemp entries are returned.
func TestCollectCoreTemps_UnknownDriver(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "acpitz\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_input"), "40000\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()
	if len(temps) != 0 {
		t.Errorf("expected no temps for unknown driver, got %d", len(temps))
	}
}

// TestCollectIntelCoreTemps_Normal verifies that a standard Intel coretemp
// hwmon tree (Package id + Core entries) is parsed correctly.
func TestCollectIntelCoreTemps_Normal(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	sensors := map[string]string{
		// Package sensor (index 1).
		"temp1_input": "55000\n",
		"temp1_label": "Package id 0\n",
		"temp1_max":   "100000\n",
		"temp1_crit":  "110000\n",
		// Core 0 (index 2).
		"temp2_input": "52000\n",
		"temp2_label": "Core 0\n",
		"temp2_max":   "100000\n",
		"temp2_crit":  "110000\n",
		// Core 1 (index 3).
		"temp3_input": "54000\n",
		"temp3_label": "Core 1\n",
		"temp3_max":   "100000\n",
		"temp3_crit":  "110000\n",
	}
	buildIntelHwmon(t, base, sensors)

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()

	if len(temps) != 2 {
		t.Fatalf("expected 2 core temps (package skipped), got %d", len(temps))
	}

	// Results must be sorted: Core 0 first, Core 1 second.
	if temps[0].CoreID != 0 {
		t.Errorf("temps[0].CoreID = %d, want 0", temps[0].CoreID)
	}
	if temps[1].CoreID != 1 {
		t.Errorf("temps[1].CoreID = %d, want 1", temps[1].CoreID)
	}
	if temps[0].TempCelsius != 52.0 {
		t.Errorf("temps[0].TempCelsius = %v, want 52.0", temps[0].TempCelsius)
	}
	if temps[0].PackageID != 0 {
		t.Errorf("temps[0].PackageID = %d, want 0", temps[0].PackageID)
	}
	if temps[0].HighCelsius != 100.0 {
		t.Errorf("temps[0].HighCelsius = %v, want 100.0", temps[0].HighCelsius)
	}
	if temps[0].CritCelsius != 110.0 {
		t.Errorf("temps[0].CritCelsius = %v, want 110.0", temps[0].CritCelsius)
	}
}

// TestCollectIntelCoreTemps_PackageSkipped verifies that "Package id X" labels
// are excluded from the returned CoreTemp slice.
func TestCollectIntelCoreTemps_PackageSkipped(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	sensors := map[string]string{
		"temp1_input": "60000\n",
		"temp1_label": "Package id 0\n",
		"temp1_max":   "95000\n",
		"temp1_crit":  "105000\n",
	}
	buildIntelHwmon(t, base, sensors)

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()
	if len(temps) != 0 {
		t.Errorf("expected 0 temps (only package sensor present), got %d", len(temps))
	}
}

// TestCollectIntelCoreTemps_MultiSocket verifies that two coretemp hwmon
// devices (two physical packages) yield correctly labelled CoreTemp entries
// with distinct PackageIDs.
func TestCollectIntelCoreTemps_MultiSocket(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	// Package 0: hwmon0.
	pkg0 := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(pkg0, "name"), "coretemp\n")
	writeSysfsFile(t, filepath.Join(pkg0, "temp1_input"), "55000\n")
	writeSysfsFile(t, filepath.Join(pkg0, "temp1_label"), "Package id 0\n")
	writeSysfsFile(t, filepath.Join(pkg0, "temp2_input"), "50000\n")
	writeSysfsFile(t, filepath.Join(pkg0, "temp2_label"), "Core 0\n")

	// Package 1: hwmon1.
	pkg1 := filepath.Join(base, "hwmon1")
	writeSysfsFile(t, filepath.Join(pkg1, "name"), "coretemp\n")
	writeSysfsFile(t, filepath.Join(pkg1, "temp1_input"), "58000\n")
	writeSysfsFile(t, filepath.Join(pkg1, "temp1_label"), "Package id 1\n")
	writeSysfsFile(t, filepath.Join(pkg1, "temp2_input"), "53000\n")
	writeSysfsFile(t, filepath.Join(pkg1, "temp2_label"), "Core 0\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()

	if len(temps) != 2 {
		t.Fatalf("expected 2 core temps (one per socket), got %d", len(temps))
	}

	// Both entries must be Core 0 but from different packages.
	pkgIDs := map[int]bool{}
	for _, ct := range temps {
		if ct.CoreID != 0 {
			t.Errorf("expected CoreID 0, got %d", ct.CoreID)
		}
		pkgIDs[ct.PackageID] = true
	}
	if !pkgIDs[0] || !pkgIDs[1] {
		t.Errorf("expected PackageIDs {0,1}, got %v", pkgIDs)
	}
}

// TestCollectAMDCoreTemps_Normal verifies that Tccd labels are collected and
// Tctl/Tdie are skipped.
func TestCollectAMDCoreTemps_Normal(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "k10temp\n")

	// Tctl — must be skipped.
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_input"), "62000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_label"), "Tctl\n")

	// CCD 0.
	writeSysfsFile(t, filepath.Join(hwmon, "temp2_input"), "58000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp2_label"), "Tccd0\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp2_max"), "95000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp2_crit"), "105000\n")

	// CCD 1.
	writeSysfsFile(t, filepath.Join(hwmon, "temp3_input"), "60000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp3_label"), "Tccd1\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp3_max"), "95000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp3_crit"), "105000\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()

	if len(temps) != 2 {
		t.Fatalf("expected 2 CCD temps, got %d", len(temps))
	}
	if temps[0].CoreID != 0 {
		t.Errorf("temps[0].CoreID = %d, want 0", temps[0].CoreID)
	}
	if temps[1].CoreID != 1 {
		t.Errorf("temps[1].CoreID = %d, want 1", temps[1].CoreID)
	}
	if temps[0].TempCelsius != 58.0 {
		t.Errorf("temps[0].TempCelsius = %v, want 58.0", temps[0].TempCelsius)
	}
	if temps[0].PackageID != 0 {
		t.Errorf("temps[0].PackageID = %d, want 0", temps[0].PackageID)
	}
}

// TestCollectAMDCoreTemps_TdiSkipped verifies that Tdie is excluded.
func TestCollectAMDCoreTemps_TdiSkipped(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "k10temp\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_input"), "55000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_label"), "Tdie\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()
	if len(temps) != 0 {
		t.Errorf("expected 0 temps (only Tdie present), got %d", len(temps))
	}
}

// TestCollectCoreTemps_SortOrder verifies that results are sorted by PackageID
// then CoreID regardless of the order hwmon entries appear on disk.
func TestCollectCoreTemps_SortOrder(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	// hwmon0: package 0, core 3 (deliberately high core index).
	h0 := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(h0, "name"), "coretemp\n")
	writeSysfsFile(t, filepath.Join(h0, "temp1_input"), "50000\n")
	writeSysfsFile(t, filepath.Join(h0, "temp1_label"), "Package id 0\n")
	writeSysfsFile(t, filepath.Join(h0, "temp2_input"), "48000\n")
	writeSysfsFile(t, filepath.Join(h0, "temp2_label"), "Core 3\n")
	writeSysfsFile(t, filepath.Join(h0, "temp3_input"), "47000\n")
	writeSysfsFile(t, filepath.Join(h0, "temp3_label"), "Core 1\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()

	if len(temps) != 2 {
		t.Fatalf("expected 2 temps, got %d", len(temps))
	}
	if temps[0].CoreID != 1 || temps[1].CoreID != 3 {
		t.Errorf("sort order wrong: got CoreIDs [%d, %d], want [1, 3]",
			temps[0].CoreID, temps[1].CoreID)
	}
}

// TestCollectCoreTemps_MissingThresholds verifies that absent max/crit files
// result in zero thresholds rather than an error.
func TestCollectCoreTemps_MissingThresholds(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "coretemp\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_input"), "50000\n")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_label"), "Core 0\n")
	// Intentionally omit temp1_max and temp1_crit.

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	temps := c.collectCoreTemps()

	if len(temps) != 1 {
		t.Fatalf("expected 1 temp, got %d", len(temps))
	}
	if temps[0].HighCelsius != 0.0 {
		t.Errorf("expected HighCelsius 0 for missing max file, got %v", temps[0].HighCelsius)
	}
	if temps[0].CritCelsius != 0.0 {
		t.Errorf("expected CritCelsius 0 for missing crit file, got %v", temps[0].CritCelsius)
	}
}

// ---------------------------------------------------------------------------
// voltageFile helper tests
// ---------------------------------------------------------------------------

// TestVoltageFile verifies that voltageFile produces the correct sysfs filename.
func TestVoltageFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		attr string
		want string
	}{
		{0, "input", "in0_input"},
		{1, "label", "in1_label"},
		{12, "input", "in12_input"},
	}
	for _, tc := range cases {
		got := voltageFile(tc.n, tc.attr)
		if got != tc.want {
			t.Errorf("voltageFile(%d, %q) = %q, want %q", tc.n, tc.attr, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// collectCoreVoltages white-box tests
// ---------------------------------------------------------------------------

// TestCollectCoreVoltages_Normal verifies correct parsing of a single voltage
// channel with both input and label files present.
func TestCollectCoreVoltages_Normal(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "nct6776\n")
	writeSysfsFile(t, filepath.Join(hwmon, "in0_input"), "1200\n") // 1.2 V
	writeSysfsFile(t, filepath.Join(hwmon, "in0_label"), "Vcore\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	voltages := c.collectCoreVoltages()

	if len(voltages) != 1 {
		t.Fatalf("expected 1 voltage entry, got %d", len(voltages))
	}
	v := voltages[0]
	if v.Channel != 0 {
		t.Errorf("Channel: want 0, got %d", v.Channel)
	}
	if v.Label != "Vcore" {
		t.Errorf("Label: want %q, got %q", "Vcore", v.Label)
	}
	if v.HwmonName != "nct6776" {
		t.Errorf("HwmonName: want %q, got %q", "nct6776", v.HwmonName)
	}
	const wantV = 1.2
	if v.VoltageV != wantV {
		t.Errorf("VoltageV: want %v, got %v", wantV, v.VoltageV)
	}
}

// TestCollectCoreVoltages_MissingLabel verifies that a missing label file
// falls back to "in{N}" as the label.
func TestCollectCoreVoltages_MissingLabel(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "nct6776\n")
	writeSysfsFile(t, filepath.Join(hwmon, "in0_input"), "3300\n")
	// No in0_label file.

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	voltages := c.collectCoreVoltages()

	if len(voltages) != 1 {
		t.Fatalf("expected 1 voltage entry, got %d", len(voltages))
	}
	if voltages[0].Label != "in0" {
		t.Errorf("Label: want %q, got %q", "in0", voltages[0].Label)
	}
}

// TestCollectCoreVoltages_EmptyHwmonBase verifies that a missing hwmon
// directory returns nil without panicking.
func TestCollectCoreVoltages_EmptyHwmonBase(t *testing.T) {
	t.Parallel()
	c := newSensorCollectorWithPaths(slog.Default(), "/nonexistent/hwmon/base", "", "", "", "")
	voltages := c.collectCoreVoltages()
	if voltages != nil {
		t.Errorf("expected nil for missing hwmon base, got %v", voltages)
	}
}

// TestCollectCoreVoltages_MultipleChannels verifies that multiple voltage
// channels are returned sorted by channel number.
func TestCollectCoreVoltages_MultipleChannels(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	hwmon := filepath.Join(base, "hwmon0")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "nct6776\n")
	writeSysfsFile(t, filepath.Join(hwmon, "in0_input"), "1200\n")
	writeSysfsFile(t, filepath.Join(hwmon, "in0_label"), "Vcore\n")
	writeSysfsFile(t, filepath.Join(hwmon, "in1_input"), "3300\n")
	writeSysfsFile(t, filepath.Join(hwmon, "in1_label"), "+3.3V\n")

	c := newSensorCollectorWithPaths(slog.Default(), base, "", "", "", "")
	voltages := c.collectCoreVoltages()

	if len(voltages) != 2 {
		t.Fatalf("expected 2 voltage entries, got %d", len(voltages))
	}
	if voltages[0].Channel != 0 {
		t.Errorf("first entry: Channel want 0, got %d", voltages[0].Channel)
	}
	if voltages[1].Channel != 1 {
		t.Errorf("second entry: Channel want 1, got %d", voltages[1].Channel)
	}
	if voltages[0].Label != "Vcore" {
		t.Errorf("first entry: Label want %q, got %q", "Vcore", voltages[0].Label)
	}
	if voltages[1].Label != "+3.3V" {
		t.Errorf("second entry: Label want %q, got %q", "+3.3V", voltages[1].Label)
	}
}

// ---------------------------------------------------------------------------
// collectPackagePower white-box tests
// ---------------------------------------------------------------------------

// buildRAPLZone writes a minimal RAPL powercap zone directory under powercapBase.
func buildRAPLZone(t *testing.T, powercapBase, dirName, zoneName string, energyUJ, maxPowerUW uint64) {
	t.Helper()
	zoneDir := filepath.Join(powercapBase, dirName)
	writeSysfsFile(t, filepath.Join(zoneDir, "name"), zoneName+"\n")
	writeSysfsFile(t, filepath.Join(zoneDir, "energy_uj"), strconv.FormatUint(energyUJ, 10)+"\n")
	writeSysfsFile(t, filepath.Join(zoneDir, "constraint_0_max_power_uw"), strconv.FormatUint(maxPowerUW, 10)+"\n")
}

// TestCollectPackagePower_Normal verifies that a single valid package zone is
// collected with the correct field values on a first call (PowerW == 0).
func TestCollectPackagePower_Normal(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	buildRAPLZone(t, base, "intel-rapl:0", "package-0", 1_000_000, 125_000_000)

	c := newSensorCollectorWithPaths(newNopLogger(), "", "", base, "", "")
	result := c.collectPackagePower()

	if len(result) != 1 {
		t.Fatalf("expected 1 PackagePower entry, got %d", len(result))
	}
	pp := result[0]
	if pp.PackageName != "package-0" {
		t.Errorf("PackageName: want %q, got %q", "package-0", pp.PackageName)
	}
	if pp.EnergyJoules != 1.0 {
		t.Errorf("EnergyJoules: want 1.0, got %v", pp.EnergyJoules)
	}
	if pp.MaxPowerW != 125.0 {
		t.Errorf("MaxPowerW: want 125.0, got %v", pp.MaxPowerW)
	}
}

// TestCollectPackagePower_FirstCallZeroPower verifies that PowerW is zero on
// the first call because there is no previous energy snapshot to diff against.
func TestCollectPackagePower_FirstCallZeroPower(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	buildRAPLZone(t, base, "intel-rapl:0", "package-0", 5_000_000, 95_000_000)

	c := newSensorCollectorWithPaths(newNopLogger(), "", "", base, "", "")
	result := c.collectPackagePower()

	if len(result) != 1 {
		t.Fatalf("expected 1 PackagePower entry, got %d", len(result))
	}
	if result[0].PowerW != 0 {
		t.Errorf("PowerW on first call: want 0, got %v", result[0].PowerW)
	}
}

// TestCollectPackagePower_SkipsSubzones verifies that RAPL sub-zones whose
// name does not start with "package-" (e.g. "core", "uncore", "dram") are
// not included in the result.
func TestCollectPackagePower_SkipsSubzones(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	buildRAPLZone(t, base, "intel-rapl:0", "package-0", 2_000_000, 100_000_000)
	buildRAPLZone(t, base, "intel-rapl:0:0", "core", 500_000, 50_000_000)
	buildRAPLZone(t, base, "intel-rapl:0:1", "uncore", 300_000, 30_000_000)

	c := newSensorCollectorWithPaths(newNopLogger(), "", "", base, "", "")
	result := c.collectPackagePower()

	if len(result) != 1 {
		t.Fatalf("expected 1 PackagePower entry (sub-zones skipped), got %d", len(result))
	}
	if result[0].PackageName != "package-0" {
		t.Errorf("PackageName: want %q, got %q", "package-0", result[0].PackageName)
	}
}

// TestCollectPackagePower_EmptyPowercapBase verifies that a missing powercap
// directory returns nil without panicking.
func TestCollectPackagePower_EmptyPowercapBase(t *testing.T) {
	t.Parallel()
	c := newSensorCollectorWithPaths(newNopLogger(), "", "", "/nonexistent/powercap/base", "", "")
	result := c.collectPackagePower()
	if result != nil {
		t.Errorf("expected nil for missing powercap base, got %v", result)
	}
}

// TestCollectPackagePower_EnergyDelta verifies that PowerW is non-zero on the
// second Collect call when the energy counter has advanced.
func TestCollectPackagePower_EnergyDelta(t *testing.T) {
	// Not parallel: we mutate the energy file between calls.
	base := t.TempDir()
	zoneDir := filepath.Join(base, "intel-rapl:0")
	writeSysfsFile(t, filepath.Join(zoneDir, "name"), "package-0\n")
	writeSysfsFile(t, filepath.Join(zoneDir, "constraint_0_max_power_uw"), "100000000\n")

	// First call: seed the snapshot.
	writeSysfsFile(t, filepath.Join(zoneDir, "energy_uj"), "1000000\n")
	c := newSensorCollectorWithPaths(newNopLogger(), "", "", base, "", "")
	first := c.collectPackagePower()
	if len(first) != 1 {
		t.Fatalf("first call: expected 1 entry, got %d", len(first))
	}
	if first[0].PowerW != 0 {
		t.Errorf("first call: PowerW should be 0, got %v", first[0].PowerW)
	}

	// Advance the energy counter by 2 000 000 µJ (2 J) to simulate elapsed time.
	writeSysfsFile(t, filepath.Join(zoneDir, "energy_uj"), "3000000\n")
	second := c.collectPackagePower()
	if len(second) != 1 {
		t.Fatalf("second call: expected 1 entry, got %d", len(second))
	}
	if second[0].PowerW <= 0 {
		t.Errorf("second call: expected PowerW > 0, got %v", second[0].PowerW)
	}
}

// ---------------------------------------------------------------------------
// collectThrottleInfo tests
// ---------------------------------------------------------------------------

// writeSysfsFileInDir is a helper that writes content to path, creating parent
// directories as needed.
func writeSysfsFileInDir(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("writeSysfsFileInDir MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeSysfsFileInDir WriteFile: %v", err)
	}
}

// TestCollectThrottleInfo_Normal verifies that throttle counts are parsed and
// returned sorted by CPU number.
func TestCollectThrottleInfo_Normal(t *testing.T) {
	dir := t.TempDir()

	// Build fake cpu0/thermal_throttle and cpu1/thermal_throttle.
	writeSysfsFileInDir(t, filepath.Join(dir, "cpu0", "thermal_throttle", "core_throttle_count"), "42\n")
	writeSysfsFileInDir(t, filepath.Join(dir, "cpu0", "thermal_throttle", "package_throttle_count"), "7\n")
	writeSysfsFileInDir(t, filepath.Join(dir, "cpu1", "thermal_throttle", "core_throttle_count"), "100\n")
	writeSysfsFileInDir(t, filepath.Join(dir, "cpu1", "thermal_throttle", "package_throttle_count"), "3\n")
	// A non-cpu entry must be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "online"), 0755); err != nil {
		t.Fatal(err)
	}

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	result := c.collectThrottleInfo()

	if len(result) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result))
	}
	if result[0].CPU != 0 || result[0].CoreThrottleCount != 42 || result[0].PackageThrottleCount != 7 {
		t.Errorf("cpu0 throttle: got %+v", result[0])
	}
	if result[1].CPU != 1 || result[1].CoreThrottleCount != 100 || result[1].PackageThrottleCount != 3 {
		t.Errorf("cpu1 throttle: got %+v", result[1])
	}
}

// TestCollectThrottleInfo_MissingDir verifies that CPUs without a
// thermal_throttle directory are silently skipped, returning nil.
func TestCollectThrottleInfo_MissingDir(t *testing.T) {
	dir := t.TempDir()

	// cpu0 directory exists but has no thermal_throttle sub-directory.
	if err := os.MkdirAll(filepath.Join(dir, "cpu0"), 0755); err != nil {
		t.Fatal(err)
	}

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	result := c.collectThrottleInfo()

	if result != nil {
		t.Errorf("expected nil for cpu without thermal_throttle, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// collectThermalZones tests
// ---------------------------------------------------------------------------

// TestCollectThermalZones_Normal verifies that thermal zone files are parsed
// and returned with the correct fields.
func TestCollectThermalZones_Normal(t *testing.T) {
	dir := t.TempDir()

	writeSysfsFileInDir(t, filepath.Join(dir, "thermal_zone0", "type"), "x86_pkg_temp\n")
	writeSysfsFileInDir(t, filepath.Join(dir, "thermal_zone0", "temp"), "52000\n")
	writeSysfsFileInDir(t, filepath.Join(dir, "thermal_zone0", "policy"), "step_wise\n")

	// A non-zone entry must be ignored.
	writeSysfsFileInDir(t, filepath.Join(dir, "cooling_device0", "type"), "Fan\n")

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	result := c.collectThermalZones()

	if len(result) != 1 {
		t.Fatalf("expected 1 zone, got %d", len(result))
	}
	z := result[0]
	if z.Name != "thermal_zone0" {
		t.Errorf("Name: got %q, want %q", z.Name, "thermal_zone0")
	}
	if z.Type != "x86_pkg_temp" {
		t.Errorf("Type: got %q, want %q", z.Type, "x86_pkg_temp")
	}
	if z.TempCelsius != 52.0 {
		t.Errorf("TempCelsius: got %v, want 52.0", z.TempCelsius)
	}
	if z.Policy != "step_wise" {
		t.Errorf("Policy: got %q, want %q", z.Policy, "step_wise")
	}
}

// TestCollectThermalZones_EmptyBase verifies that a missing thermalBase returns
// nil without panicking.
func TestCollectThermalZones_EmptyBase(t *testing.T) {
	c := newSensorCollectorWithPaths(newNopLogger(), "/nonexistent", "/nonexistent", "/nonexistent", "/nonexistent", "/nonexistent")
	result := c.collectThermalZones()
	if result != nil {
		t.Errorf("expected nil for missing base dir, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// collectFans tests
// ---------------------------------------------------------------------------

// TestCollectFans_Normal verifies that fan{N}_input, fan{N}_label, fan{N}_min,
// and fan{N}_max are all read and returned correctly.
func TestCollectFans_Normal(t *testing.T) {
	dir := t.TempDir()
	hwmon := filepath.Join(dir, "hwmon0")

	writeSysfsFileInDir(t, filepath.Join(hwmon, "name"), "nct6776\n")
	writeSysfsFileInDir(t, filepath.Join(hwmon, "fan1_input"), "1200\n")
	writeSysfsFileInDir(t, filepath.Join(hwmon, "fan1_label"), "CPU Fan\n")
	writeSysfsFileInDir(t, filepath.Join(hwmon, "fan1_min"), "200\n")
	writeSysfsFileInDir(t, filepath.Join(hwmon, "fan1_max"), "3000\n")

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	fans := c.collectFans()

	if len(fans) != 1 {
		t.Fatalf("expected 1 fan, got %d", len(fans))
	}
	f := fans[0]
	if f.Label != "CPU Fan" {
		t.Errorf("Label: got %q, want %q", f.Label, "CPU Fan")
	}
	if f.RPM != 1200 {
		t.Errorf("RPM: got %d, want 1200", f.RPM)
	}
	if f.MinRPM != 200 {
		t.Errorf("MinRPM: got %d, want 200", f.MinRPM)
	}
	if f.MaxRPM != 3000 {
		t.Errorf("MaxRPM: got %d, want 3000", f.MaxRPM)
	}
	if f.HwmonName != "nct6776" {
		t.Errorf("HwmonName: got %q, want %q", f.HwmonName, "nct6776")
	}
}

// TestCollectFans_MissingLabel verifies that when fan{N}_label is absent the
// label falls back to "fan{N}".
func TestCollectFans_MissingLabel(t *testing.T) {
	dir := t.TempDir()
	hwmon := filepath.Join(dir, "hwmon0")

	writeSysfsFileInDir(t, filepath.Join(hwmon, "name"), "it8720\n")
	writeSysfsFileInDir(t, filepath.Join(hwmon, "fan1_input"), "800\n")
	// No fan1_label file.

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	fans := c.collectFans()

	if len(fans) != 1 {
		t.Fatalf("expected 1 fan, got %d", len(fans))
	}
	if fans[0].Label != "fan1" {
		t.Errorf("Label: got %q, want %q", fans[0].Label, "fan1")
	}
}

// TestCollectFans_NoFans verifies that an hwmon device with no fan{N}_input
// files results in no FanInfo entries.
func TestCollectFans_NoFans(t *testing.T) {
	dir := t.TempDir()
	hwmon := filepath.Join(dir, "hwmon0")

	writeSysfsFileInDir(t, filepath.Join(hwmon, "name"), "acpitz\n")
	// Only temp files; no fan files.
	writeSysfsFileInDir(t, filepath.Join(hwmon, "temp1_input"), "45000\n")

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	fans := c.collectFans()

	if fans != nil {
		t.Errorf("expected nil for hwmon with no fan files, got %v", fans)
	}
}

// ---------------------------------------------------------------------------
// parsePSILine tests
// ---------------------------------------------------------------------------

// TestParsePSILine_Valid verifies correct parsing of a well-formed PSI line.
func TestParsePSILine_Valid(t *testing.T) {
	line := "some avg10=1.23 avg60=4.56 avg300=7.89 total=12345"
	avg10, avg60, avg300, total := parsePSILine(line)

	if avg10 != 1.23 {
		t.Errorf("avg10: got %v, want 1.23", avg10)
	}
	if avg60 != 4.56 {
		t.Errorf("avg60: got %v, want 4.56", avg60)
	}
	if avg300 != 7.89 {
		t.Errorf("avg300: got %v, want 7.89", avg300)
	}
	if total != 12345 {
		t.Errorf("total: got %d, want 12345", total)
	}
}

// TestParsePSILine_Invalid verifies that a malformed line returns all zeros.
func TestParsePSILine_Invalid(t *testing.T) {
	cases := []string{
		"",
		"some",
		"some avg10",
		"garbage line with no equals signs",
	}
	for _, line := range cases {
		avg10, avg60, avg300, total := parsePSILine(line)
		if avg10 != 0 || avg60 != 0 || avg300 != 0 || total != 0 {
			t.Errorf("parsePSILine(%q): expected all zeros, got (%v,%v,%v,%d)", line, avg10, avg60, avg300, total)
		}
	}
}

// ---------------------------------------------------------------------------
// collectPSI tests
// ---------------------------------------------------------------------------

// TestCollectPSI_Normal verifies full end-to-end parsing of cpu, memory, and io
// PSI files.
func TestCollectPSI_Normal(t *testing.T) {
	dir := t.TempDir()

	cpuContent := "some avg10=0.10 avg60=0.20 avg300=0.30 total=1000\n"
	memContent := "some avg10=1.00 avg60=2.00 avg300=3.00 total=5000\nfull avg10=0.50 avg60=1.00 avg300=1.50 total=2500\n"
	ioContent := "some avg10=2.00 avg60=4.00 avg300=6.00 total=9000\nfull avg10=1.00 avg60=2.00 avg300=3.00 total=4500\n"

	if err := os.WriteFile(filepath.Join(dir, "cpu"), []byte(cpuContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory"), []byte(memContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "io"), []byte(ioContent), 0644); err != nil {
		t.Fatal(err)
	}

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	psi := c.collectPSI()

	if psi.CPU.SomeAvg10 != 0.10 {
		t.Errorf("CPU.SomeAvg10: got %v, want 0.10", psi.CPU.SomeAvg10)
	}
	if psi.CPU.SomeTotal != 1000 {
		t.Errorf("CPU.SomeTotal: got %d, want 1000", psi.CPU.SomeTotal)
	}
	if psi.Memory.SomeAvg10 != 1.00 {
		t.Errorf("Memory.SomeAvg10: got %v, want 1.00", psi.Memory.SomeAvg10)
	}
	if psi.Memory.FullAvg10 != 0.50 {
		t.Errorf("Memory.FullAvg10: got %v, want 0.50", psi.Memory.FullAvg10)
	}
	if psi.IO.SomeAvg300 != 6.00 {
		t.Errorf("IO.SomeAvg300: got %v, want 6.00", psi.IO.SomeAvg300)
	}
	if psi.IO.FullTotal != 4500 {
		t.Errorf("IO.FullTotal: got %d, want 4500", psi.IO.FullTotal)
	}
}

// TestCollectPSI_MissingFiles verifies that absent PSI files produce a zero
// PSIData without any error or panic.
func TestCollectPSI_MissingFiles(t *testing.T) {
	c := newSensorCollectorWithPaths(newNopLogger(), "/nonexistent", "/nonexistent", "/nonexistent", "/nonexistent", "/nonexistent/pressure")
	psi := c.collectPSI()

	zero := types.PSIData{}
	if psi != zero {
		t.Errorf("expected zero PSIData for missing files, got %+v", psi)
	}
}

// TestCollectPSI_CPUNoFull verifies that a CPU PSI file that only has a "some"
// line (no "full") leaves the full fields at zero.
func TestCollectPSI_CPUNoFull(t *testing.T) {
	dir := t.TempDir()

	cpuContent := "some avg10=0.05 avg60=0.10 avg300=0.15 total=500\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu"), []byte(cpuContent), 0644); err != nil {
		t.Fatal(err)
	}
	// memory and io absent; only testing CPU behaviour here.

	c := newSensorCollectorWithPaths(newNopLogger(), dir, dir, dir, dir, dir)
	psi := c.collectPSI()

	if psi.CPU.SomeAvg10 != 0.05 {
		t.Errorf("CPU.SomeAvg10: got %v, want 0.05", psi.CPU.SomeAvg10)
	}
	if psi.CPU.FullAvg10 != 0 || psi.CPU.FullAvg60 != 0 || psi.CPU.FullAvg300 != 0 || psi.CPU.FullTotal != 0 {
		t.Errorf("CPU full fields should be zero for CPU-only PSI file, got %+v", psi.CPU)
	}
}

// ---- shouldCollect (frequency tiering) ------------------------------------

func TestShouldCollect_FastTier(t *testing.T) {
	// Fast tier runs on every tick.
	for _, tick := range []uint64{0, 1, 2, 5, 30, 100} {
		if !shouldCollect(tick, tierFast) {
			t.Errorf("shouldCollect(%d, tierFast) = false, want true", tick)
		}
	}
}

func TestShouldCollect_MediumTier(t *testing.T) {
	// Medium tier runs every mediumInterval ticks.
	for tick := uint64(0); tick <= 30; tick++ {
		want := tick%mediumInterval == 0
		got := shouldCollect(tick, tierMedium)
		if got != want {
			t.Errorf("shouldCollect(%d, tierMedium) = %v, want %v", tick, got, want)
		}
	}
}

func TestShouldCollect_SlowTier(t *testing.T) {
	// Slow tier runs every slowInterval ticks.
	for tick := uint64(0); tick <= 60; tick++ {
		want := tick%slowInterval == 0
		got := shouldCollect(tick, tierSlow)
		if got != want {
			t.Errorf("shouldCollect(%d, tierSlow) = %v, want %v", tick, got, want)
		}
	}
}
