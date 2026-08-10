package collector

import (
	"encoding/binary"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// putLE64 writes a little-endian value into the low 8 bytes of a 16-byte NVMe
// counter field, which is how the drive reports them.
func putLE64(dst []byte, v uint64) {
	binary.LittleEndian.PutUint64(dst[:8], v)
}

// healthyNVMeLog returns a log page describing a drive in good condition.
func healthyNVMeLog() *nvmeSmartLog {
	log := &nvmeSmartLog{}
	log.Temperature[0], log.Temperature[1] = 0x24, 0x01 // 292 K ≈ 18.85 °C
	log.AvailableSpare = 100
	log.AvailableSpareThreshold = 10
	log.PercentageUsed = 4
	putLE64(log.DataUnitsRead[:], 1_000_000)
	putLE64(log.DataUnitsWritten[:], 2_000_000)
	putLE64(log.PowerCycles[:], 42)
	putLE64(log.PowerOnHours[:], 8_000)
	putLE64(log.UnsafeShutdowns[:], 3)
	putLE64(log.MediaErrors[:], 0)
	putLE64(log.NumErrLogEntries[:], 7)
	log.WarningTempTime = 11
	log.CriticalCompTime = 0
	return log
}

// TestApplyNVMeSMARTLog_HealthyDrive verifies every counter lands in the right
// field and that wear is turned into a remaining-life estimate.
func TestApplyNVMeSMARTLog_HealthyDrive(t *testing.T) {
	var info types.DiskInfo
	applyNVMeSMARTLog(healthyNVMeLog(), &info)

	if !info.SMARTEnabled || !info.SMARTHealthy {
		t.Errorf("SMARTEnabled=%v SMARTHealthy=%v, want both true", info.SMARTEnabled, info.SMARTHealthy)
	}
	if got, want := info.Temperature, 292-273.15; got < want-0.01 || got > want+0.01 {
		t.Errorf("Temperature = %v, want %v", got, want)
	}
	if info.AvailableSparePercent != 100 || info.SpareThresholdPercent != 10 {
		t.Errorf("spare = %d%% (threshold %d%%), want 100%% / 10%%",
			info.AvailableSparePercent, info.SpareThresholdPercent)
	}
	if info.WearLevelPercent != 4 || info.LifeRemainingPercent != 96 {
		t.Errorf("wear = %d%%, life remaining = %d%%, want 4%% / 96%%",
			info.WearLevelPercent, info.LifeRemainingPercent)
	}
	if info.PowerOnHours != 8000 || info.PowerCycles != 42 {
		t.Errorf("PowerOnHours = %d, PowerCycles = %d, want 8000 / 42", info.PowerOnHours, info.PowerCycles)
	}
	if info.DataUnitsRead != 1_000_000 || info.DataUnitsWritten != 2_000_000 {
		t.Errorf("data units = %d read / %d written, want 1000000 / 2000000",
			info.DataUnitsRead, info.DataUnitsWritten)
	}
	if info.UnsafeShutdowns != 3 || info.ErrorLogEntries != 7 || info.MediaErrors != 0 {
		t.Errorf("unsafe=%d errlog=%d media=%d, want 3 / 7 / 0",
			info.UnsafeShutdowns, info.ErrorLogEntries, info.MediaErrors)
	}
	if info.WarningTempTime != 11 || info.CriticalTempTime != 0 {
		t.Errorf("temp times = %d / %d, want 11 / 0", info.WarningTempTime, info.CriticalTempTime)
	}
	// 8000 hours consumed 4% of life, so ~96/4 × 8000 hours should remain.
	if want := uint64(8000 * 96 / 4); info.EstimatedHoursLeft != want {
		t.Errorf("EstimatedHoursLeft = %d, want %d", info.EstimatedHoursLeft, want)
	}
}

// TestApplyNVMeSMARTLog_DegradedDrive verifies a critical warning marks the
// drive unhealthy, and that the edge cases which would divide by zero or
// produce a nonsense temperature are handled.
func TestApplyNVMeSMARTLog_DegradedDrive(t *testing.T) {
	log := &nvmeSmartLog{}
	log.CriticalWarning = 0x04 // reliability degraded
	log.AvailableSpare = 2
	log.AvailableSpareThreshold = 10
	// PercentageUsed stays 0, and Temperature stays 0 K — both below the
	// guards in the decoder.
	putLE64(log.PowerOnHours[:], 50_000)
	putLE64(log.MediaErrors[:], 128)

	var info types.DiskInfo
	applyNVMeSMARTLog(log, &info)

	if info.SMARTHealthy {
		t.Error("SMARTHealthy = true, want false for a drive reporting a critical warning")
	}
	if info.CriticalWarning != 0x04 {
		t.Errorf("CriticalWarning = %d, want 4", info.CriticalWarning)
	}
	if info.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 when the drive reports no reading", info.Temperature)
	}
	if info.EstimatedHoursLeft != 0 {
		t.Errorf("EstimatedHoursLeft = %d, want 0 when no wear has been reported",
			info.EstimatedHoursLeft)
	}
	if info.MediaErrors != 128 {
		t.Errorf("MediaErrors = %d, want 128", info.MediaErrors)
	}
}

// identifyData builds an Identify Controller buffer with the firmware revision
// and version register set.
func identifyData(firmware string, major, minor, tertiary uint32) []byte {
	buf := make([]byte, nvmeIdentifyDataLen)
	copy(buf[64:72], []byte(firmware))
	for i := 64 + len(firmware); i < 72; i++ {
		buf[i] = ' '
	}
	binary.LittleEndian.PutUint32(buf[80:84], major<<16|minor<<8|tertiary)
	return buf
}

func TestApplyNVMeIdentify_FirmwareAndVersion(t *testing.T) {
	tests := []struct {
		name                   string
		firmware               string
		major, minor, tertiary uint32
		wantFirmware, wantNVMe string
	}{
		{"three-part version", "3B2QGXA7", 1, 4, 0, "3B2QGXA7", "1.4"},
		{"tertiary reported", "EXF7201Q", 2, 0, 1, "EXF7201Q", "2.0.1"},
		{"padded firmware", "1B2QE", 1, 3, 0, "1B2QE", "1.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var info types.DiskInfo
			applyNVMeIdentify(identifyData(tt.firmware, tt.major, tt.minor, tt.tertiary), &info)

			if info.FirmwareVersion != tt.wantFirmware {
				t.Errorf("FirmwareVersion = %q, want %q", info.FirmwareVersion, tt.wantFirmware)
			}
			if info.NVMeVersion != tt.wantNVMe {
				t.Errorf("NVMeVersion = %q, want %q", info.NVMeVersion, tt.wantNVMe)
			}
		})
	}
}

// TestApplyNVMeIdentify_UnusableBuffers verifies a short or blank buffer leaves
// the disk record untouched rather than panicking or inventing a version.
func TestApplyNVMeIdentify_UnusableBuffers(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"short buffer", make([]byte, 40)},
		{"empty buffer", nil},
		{"all zeroes", make([]byte, nvmeIdentifyDataLen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := types.DiskInfo{FirmwareVersion: "existing"}
			applyNVMeIdentify(tt.buf, &info)

			if info.FirmwareVersion != "existing" {
				t.Errorf("FirmwareVersion = %q, want the previous value to survive", info.FirmwareVersion)
			}
			if info.NVMeVersion != "" {
				t.Errorf("NVMeVersion = %q, want empty", info.NVMeVersion)
			}
		})
	}
}

// TestApplyATASMARTAttrs_WellKnownIDs verifies the standard attribute IDs are
// lifted into their dedicated fields and every attribute is also retained.
func TestApplyATASMARTAttrs_WellKnownIDs(t *testing.T) {
	attrs := []ataAttr{
		{ID: 9, Value: 95, Worst: 95, Threshold: 0, Raw: 27_000, Flags: 0x0002},
		{ID: 12, Value: 99, Worst: 99, Threshold: 20, Raw: 615, Flags: 0x0002},
		{ID: 194, Value: 65, Worst: 45, Threshold: 0, Raw: 35, Flags: 0x0002},
		{ID: 5, Value: 100, Worst: 100, Threshold: 10, Raw: 0, Flags: attrFlagsPrefail},
	}

	var info types.DiskInfo
	applyATASMARTAttrs(attrs, &info)

	if info.PowerOnHours != 27_000 {
		t.Errorf("PowerOnHours = %d, want 27000", info.PowerOnHours)
	}
	if info.PowerCycles != 615 {
		t.Errorf("PowerCycles = %d, want 615", info.PowerCycles)
	}
	if info.Temperature != 35 {
		t.Errorf("Temperature = %v, want 35", info.Temperature)
	}
	if len(info.SMARTAttrs) != len(attrs) {
		t.Fatalf("kept %d attributes, want %d", len(info.SMARTAttrs), len(attrs))
	}
	if !info.SMARTEnabled || !info.SMARTHealthy {
		t.Errorf("SMARTEnabled=%v SMARTHealthy=%v, want both true", info.SMARTEnabled, info.SMARTHealthy)
	}

	byID := make(map[uint8]types.SMARTAttr, len(info.SMARTAttrs))
	for _, a := range info.SMARTAttrs {
		byID[a.ID] = a
	}
	if got := byID[5].Name; got != "Reallocated_Sector_Ct" {
		t.Errorf("attribute 5 name = %q, want Reallocated_Sector_Ct", got)
	}
	if got := byID[5].Type; got != "pre-fail" {
		t.Errorf("attribute 5 type = %q, want pre-fail", got)
	}
	if got := byID[9].Type; got != "old-age" {
		t.Errorf("attribute 9 type = %q, want old-age", got)
	}
}

// TestApplyATASMARTAttrs_FailingAndUnknown verifies an attribute at or below
// its threshold marks the drive unhealthy, and that an ID with no known name
// still produces a usable label.
func TestApplyATASMARTAttrs_FailingAndUnknown(t *testing.T) {
	attrs := []ataAttr{
		{ID: 5, Value: 8, Worst: 8, Threshold: 10, Raw: 4096, Flags: attrFlagsPrefail},
		{ID: 200, Value: 100, Worst: 100, Threshold: 0, Raw: 0},
		// Threshold 0 means "no threshold", so a zero value must not be
		// reported as failing.
		{ID: 190, Value: 0, Worst: 0, Threshold: 0, Raw: 0},
	}

	var info types.DiskInfo
	applyATASMARTAttrs(attrs, &info)

	if info.SMARTHealthy {
		t.Error("SMARTHealthy = true, want false when an attribute is below its threshold")
	}

	byID := make(map[uint8]types.SMARTAttr, len(info.SMARTAttrs))
	for _, a := range info.SMARTAttrs {
		byID[a.ID] = a
	}
	if !byID[5].Failing {
		t.Error("attribute 5 should be failing: value 8 is below threshold 10")
	}
	if byID[190].Failing {
		t.Error("attribute 190 should not be failing: a zero threshold means no threshold")
	}
	if got := byID[200].Name; got == "" {
		t.Error("an attribute with no well-known name should still be labelled")
	}
}
