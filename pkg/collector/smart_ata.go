//go:build linux

package collector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"unsafe"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ATA ioctl and command constants from linux/hdreg.h.
//
// HDIO_DRIVE_CMD = 0x031F
// Sends a single ATA command via the legacy IDE ioctl interface.
// The argument is a 4-byte header followed by a sector buffer.
// Layout: [ATA_CMD, SECTOR_COUNT, FEATURE, LBA_MID, LBA_HIGH, ...]
// For SMART READ DATA:
//
//	buf[0] = 0xB0  (WIN_SMART)
//	buf[1] = 0x01  (sector count)
//	buf[2] = 0xD0  (SMART_READ_DATA sub-command in Features register)
//	buf[3] = 0x4F  (LBA mid / Cyl Low)
//	buf[4] = 0xC2  (LBA high / Cyl High)
//
// The kernel writes one sector (512 bytes) of SMART data starting at buf[4].
const hdioDriverCmd uintptr = 0x031F

// ATA SMART command constants.
const (
	ataCmdSMART                uint8 = 0xB0
	smartCmdReadData           uint8 = 0xD0 // ATA SMART READ DATA (in features register)
	smartCylLow                uint8 = 0x4F
	smartCylHigh               uint8 = 0xC2
	smartSectorCount           uint8 = 0x01
	smartDataSize                    = 512
	smartAttrTableOffset             = 2  // offset of attribute table in SMART data
	smartAttrEntrySize               = 12 // bytes per ATA SMART attribute entry
	smartAttrCount                   = 30 // maximum attributes in SMART data
	smartAttrIDOffset                = 0  // attribute ID within entry
	smartAttrFlagsOffset             = 1  // flags (2 bytes)
	smartAttrValueOffset             = 3  // normalized value
	smartAttrWorstOffset             = 4  // worst seen value
	smartAttrRawOffset               = 5  // raw value (6 bytes, little-endian)
	smartAttrThreshTableOffset       = 2  // offset of threshold table in threshold data
	smartAttrThreshEntrySize         = 12 // bytes per threshold entry
	smartAttrThreshValueOffset       = 1  // threshold value within entry
)

// ATA SMART READ THRESHOLDS sub-command.
const smartCmdReadThresholds uint8 = 0xD1

// ataAttr is a parsed ATA SMART attribute.
type ataAttr struct {
	ID        uint8
	Flags     uint16
	Value     uint8
	Worst     uint8
	Raw       uint64 // lower 6 bytes of raw, little-endian
	Threshold uint8
}

// wellKnownATAAttrNames maps standard ATA SMART attribute IDs to human-readable
// names.  Vendor-specific attributes not in this map are labelled "Unknown_Attr".
var wellKnownATAAttrNames = map[uint8]string{
	1:   "Raw_Read_Error_Rate",
	2:   "Throughput_Performance",
	3:   "Spin_Up_Time",
	4:   "Start_Stop_Count",
	5:   "Reallocated_Sector_Ct",
	7:   "Seek_Error_Rate",
	8:   "Seek_Time_Performance",
	9:   "Power_On_Hours",
	10:  "Spin_Retry_Count",
	11:  "Calibration_Retry_Count",
	12:  "Power_Cycle_Count",
	13:  "Read_Soft_Error_Rate",
	100: "Erase_Erase_Count",
	103: "Translation_Table_Rebuild",
	170: "Available_Reservd_Space",
	171: "Program_Fail_Count_Chip",
	172: "Erase_Fail_Count_Chip",
	173: "Wear_Levelling_Count",
	174: "Unexpected_Power_Loss_Ct",
	175: "Program_Fail_Count_Chip",
	176: "Erase_Fail_Count_Chip",
	177: "Wear_Leveling_Count",
	178: "Used_Rsvd_Blk_Cnt_Chip",
	179: "Used_Rsvd_Blk_Cnt_Tot",
	180: "Unused_Rsvd_Blk_Cnt_Tot",
	181: "Program_Fail_Cnt_Total",
	182: "Erase_Fail_Count_Total",
	183: "Runtime_Bad_Block",
	184: "End-to-End_Error",
	185: "Head_Stability",
	186: "Induced_Op-Vibration_Det",
	187: "Reported_Uncorrect",
	188: "Command_Timeout",
	189: "High_Fly_Writes",
	190: "Airflow_Temperature_Cel",
	191: "G-Sense_Error_Rate",
	192: "Power-Off_Retract_Count",
	193: "Load_Cycle_Count",
	194: "Temperature_Celsius",
	195: "Hardware_ECC_Recovered",
	196: "Reallocated_Event_Count",
	197: "Current_Pending_Sector",
	198: "Offline_Uncorrectable",
	199: "UDMA_CRC_Error_Count",
	200: "Multi_Zone_Error_Rate",
	201: "Soft_Read_Error_Rate",
	202: "Data_Address_Mark_Errs",
	203: "Run_Out_Cancel",
	204: "Soft_ECC_Correction",
	205: "Thermal_Asperity_Rate",
	206: "Flying_Height",
	207: "Spin_High_Current",
	208: "Spin_Buzz",
	209: "Offline_Seek_Performnce",
	210: "Unknown_Attribute",
	211: "Unknown_Attribute",
	212: "Unknown_Attribute",
	220: "Disk_Shift",
	221: "G-Sense_Error_Rate",
	222: "Loaded_Hours",
	223: "Load_Retry_Count",
	224: "Load_Friction",
	225: "Load_Cycle_Count",
	226: "Load-in_Time",
	227: "Torq-amp_Count",
	228: "Power-off_Retract_Count",
	230: "Head_Amplitude",
	231: "Temperature_Celsius",
	232: "Available_Reservd_Space",
	233: "Media_Wearout_Indicator",
	235: "Unknown_Attribute",
	240: "Head_Flying_Hours",
	241: "Total_LBAs_Written",
	242: "Total_LBAs_Read",
	244: "Unknown_Attribute",
	245: "Unknown_Attribute",
	246: "Unknown_Attribute",
	247: "Unknown_Attribute",
	248: "Unknown_Attribute",
	249: "Unknown_Attribute",
	250: "Read_Error_Retry_Rate",
	251: "Unknown_Attribute",
	252: "Unknown_Attribute",
	254: "Free_Fall_Sensor",
}

// attrFlagsPrefail indicates the attribute is pre-failure (vs old-age).
const attrFlagsPrefail uint16 = 0x0001

// readATASMART reads ATA SMART data from the given block device path
// (e.g. /dev/sda) using the HDIO_DRIVE_CMD ioctl and populates info.
func readATASMART(devPath string, info *types.DiskInfo) error {
	f, err := os.OpenFile(devPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) {
			return &ErrSMARTPermission{DevPath: devPath}
		}
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close ATA device", "path", devPath, "err", err)
		}
	}()

	fd := f.Fd()

	// Read SMART attribute data. Try the legacy HDIO_DRIVE_CMD ioctl first (it
	// is cheapest); if the controller rejects it, fall back to SCSI ATA
	// PASS-THROUGH(16) over SG_IO, which covers AHCI/libata and USB-bridged
	// disks that HDIO_DRIVE_CMD does not. read() reports which sub-command path
	// succeeded so the threshold read can use the same one.
	dataBuf, read, err := ataReadSMARTData(fd, devPath, smartCmdReadData)
	if err != nil {
		return err
	}

	// Read SMART threshold data (same structure, different sub-command) via the
	// method that just worked; thresholds are optional, so a failure here only
	// drops the per-attribute threshold, not the whole reading.
	threshBuf, threshErr := read(fd, devPath, smartCmdReadThresholds)

	if err := validateSMARTSector(devPath, dataBuf); err != nil {
		return err
	}

	attrs := parseATASMARTAttrs(dataBuf, threshBuf, threshErr == nil)
	if len(attrs) == 0 {
		// No decodable attribute means we learned nothing about this drive.
		// Applying the empty set would set SMARTEnabled and SMARTHealthy from
		// an absence of evidence and quietly suppress the unhealthy alert.
		return &ErrSMARTNoAttributes{DevPath: devPath}
	}

	applyATASMARTAttrs(attrs, info)
	return nil
}

// validateSMARTSector rejects a 512-byte SMART payload that cannot be genuine.
//
// An all-zero sector is what a bridge returns when it answers the command
// without asking the drive. The trailing checksum byte is defined so that the
// whole sector sums to zero modulo 256; a mismatch means the bytes were not the
// ones the drive sent.
func validateSMARTSector(devPath string, data []byte) error {
	if len(data) != smartDataSize {
		return &ErrSMARTShortTransfer{DevPath: devPath, Op: "SMART READ DATA", Want: smartDataSize, Got: len(data)}
	}

	var sum byte
	allZero := true
	for _, b := range data {
		sum += b
		if b != 0 {
			allZero = false
		}
	}
	if allZero {
		return &ErrSMARTEmptySector{DevPath: devPath}
	}
	if sum != 0 {
		return &ErrSMARTChecksum{DevPath: devPath, Sum: sum}
	}
	return nil
}

// ErrSMARTNoAttributes reports a structurally valid sector that decoded to no
// attributes, which tells us nothing about the drive's health.
type ErrSMARTNoAttributes struct{ DevPath string }

func (e *ErrSMARTNoAttributes) Error() string {
	return "smart: no attributes decoded from " + e.DevPath
}

// ErrSMARTEmptySector reports an all-zero payload: a transfer that completed
// without the drive ever answering.
type ErrSMARTEmptySector struct{ DevPath string }

func (e *ErrSMARTEmptySector) Error() string {
	return "smart: empty SMART sector from " + e.DevPath
}

// ErrSMARTChecksum reports a payload whose trailing checksum does not clear.
type ErrSMARTChecksum struct {
	DevPath string
	Sum     byte
}

func (e *ErrSMARTChecksum) Error() string {
	return fmt.Sprintf("smart: SMART sector from %s failed its checksum (sum %d, want 0)", e.DevPath, e.Sum)
}

// ataSMARTReader issues one ATA SMART sub-command and returns its 512-byte
// response. ataHDIOCommand and ataSGIOCommand both satisfy it.
type ataSMARTReader func(fd uintptr, devPath string, subCmd uint8) ([]byte, error)

// ataReadSMARTData reads a SMART sub-command, trying HDIO_DRIVE_CMD first and
// falling back to SG_IO ATA PASS-THROUGH(16). It returns the data, the reader
// that succeeded (so the caller can reuse it for the paired threshold read),
// and an error only when both paths fail. A permission error from opening the
// device short-circuits: no fallback recovers it.
func ataReadSMARTData(fd uintptr, devPath string, subCmd uint8) ([]byte, ataSMARTReader, error) {
	buf, hdioErr := ataHDIOCommand(fd, devPath, subCmd)
	if hdioErr == nil {
		return buf, ataHDIOCommand, nil
	}

	buf, sgErr := ataSGIOCommand(fd, devPath, subCmd)
	if sgErr == nil {
		return buf, ataSGIOCommand, nil
	}

	return nil, nil, &ErrSMARTUnavailable{DevPath: devPath, HDIOErr: hdioErr, SGIOErr: sgErr}
}

// ErrSMARTUnavailable reports that both ATA SMART transports failed for a
// device: the legacy HDIO_DRIVE_CMD ioctl and the SG_IO ATA PASS-THROUGH(16)
// fallback. Both underlying errors are retained and unwrappable so callers can
// inspect either (e.g. errors.As for *ErrSMARTIoctl or *ErrSMARTPassthrough).
type ErrSMARTUnavailable struct {
	DevPath string
	HDIOErr error
	SGIOErr error
}

func (e *ErrSMARTUnavailable) Error() string {
	return fmt.Sprintf("smart: no ATA transport succeeded on %s (hdio: %v; sg_io: %v)",
		e.DevPath, e.HDIOErr, e.SGIOErr)
}

// Unwrap returns the SG_IO error, the more informative of the two on the
// controllers this fallback exists for.
func (e *ErrSMARTUnavailable) Unwrap() error { return e.SGIOErr }

// applyATASMARTAttrs translates decoded ATA attributes into the disk record,
// lifting the well-known IDs into their dedicated fields. It is separate from
// readATASMART so it can be exercised without a privileged ioctl.
func applyATASMARTAttrs(attrs []ataAttr, info *types.DiskInfo) {
	for _, a := range attrs {
		switch a.ID {
		case 9: // Power_On_Hours
			info.PowerOnHours = uint64(a.Raw & 0xFFFFFF) // lower 3 bytes are hours
		case 12: // Power_Cycle_Count
			info.PowerCycles = a.Raw & 0xFFFFFF
		case 194, 231: // Temperature_Celsius (choose first non-zero)
			if info.Temperature == 0 {
				info.Temperature = float64(a.Raw & 0xFF)
			}
		}

		name, ok := wellKnownATAAttrNames[a.ID]
		if !ok {
			name = fmt.Sprintf("Unknown_Attr_%d", a.ID)
		}

		attrType := "old-age"
		if a.Flags&attrFlagsPrefail != 0 {
			attrType = "pre-fail"
		}
		failing := a.Value <= a.Threshold && a.Threshold > 0

		info.SMARTAttrs = append(info.SMARTAttrs, types.SMARTAttr{
			ID:        a.ID,
			Name:      name,
			Value:     int64(a.Value),
			Worst:     int64(a.Worst),
			Threshold: int64(a.Threshold),
			RawValue:  int64(a.Raw),
			Type:      attrType,
			Failing:   failing,
		})
	}

	info.SMARTEnabled = true
	// A drive is considered healthy if no attribute is failing.
	info.SMARTHealthy = true
	for _, attr := range info.SMARTAttrs {
		if attr.Failing {
			info.SMARTHealthy = false
			break
		}
	}
}

// ataHDIOCommand sends a single ATA SMART sub-command via HDIO_DRIVE_CMD and
// returns the 512-byte response buffer.
func ataHDIOCommand(fd uintptr, devPath string, subCmd uint8) ([]byte, error) {
	// Buffer layout for HDIO_DRIVE_CMD:
	// byte 0: ATA command (0xB0 = WIN_SMART)
	// byte 1: sector count
	// byte 2: features / sub-command
	// byte 3: LBA mid (cyl low)
	// byte 4: LBA high (cyl high)
	// bytes 5+: response data written by kernel
	//
	// We use a flat [4 + 512]byte array to match the kernel interface.
	var buf [4 + smartDataSize]byte
	buf[0] = ataCmdSMART
	buf[1] = smartSectorCount
	buf[2] = subCmd
	buf[3] = smartCylLow

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		fd,
		hdioDriverCmd,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if errno != 0 {
		return nil, &ErrSMARTIoctl{
			DevPath: devPath,
			Op:      fmt.Sprintf("HDIO_DRIVE_CMD(0x%02X)", subCmd),
			Errno:   errno,
		}
	}

	// Response data starts at byte 4.
	data := make([]byte, smartDataSize)
	copy(data, buf[4:])
	return data, nil
}

// parseATASMARTAttrs parses the 512-byte ATA SMART attribute data and, if
// thresholds are provided, the 512-byte threshold data.
func parseATASMARTAttrs(data, threshData []byte, hasThresh bool) []ataAttr {
	// Threshold map: attr ID → threshold value.
	threshMap := make(map[uint8]uint8)
	if hasThresh && len(threshData) >= smartAttrThreshTableOffset+smartAttrCount*smartAttrThreshEntrySize {
		for i := range smartAttrCount {
			base := smartAttrThreshTableOffset + i*smartAttrThreshEntrySize
			id := threshData[base]
			if id == 0 {
				continue
			}
			threshMap[id] = threshData[base+smartAttrThreshValueOffset]
		}
	}

	if len(data) < smartAttrTableOffset+smartAttrCount*smartAttrEntrySize {
		return nil
	}

	attrs := make([]ataAttr, 0, smartAttrCount)
	for i := range smartAttrCount {
		base := smartAttrTableOffset + i*smartAttrEntrySize
		id := data[base+smartAttrIDOffset]
		if id == 0 {
			continue
		}

		flags := binary.LittleEndian.Uint16(data[base+smartAttrFlagsOffset : base+smartAttrFlagsOffset+2])
		value := data[base+smartAttrValueOffset]
		worst := data[base+smartAttrWorstOffset]

		// Raw is 6 bytes little-endian; read as uint64 (lower 48 bits).
		rawBytes := data[base+smartAttrRawOffset : base+smartAttrRawOffset+6]
		raw := uint64(rawBytes[0]) |
			uint64(rawBytes[1])<<8 |
			uint64(rawBytes[2])<<16 |
			uint64(rawBytes[3])<<24 |
			uint64(rawBytes[4])<<32 |
			uint64(rawBytes[5])<<40

		attrs = append(attrs, ataAttr{
			ID:        id,
			Flags:     flags,
			Value:     value,
			Worst:     worst,
			Raw:       raw,
			Threshold: threshMap[id],
		})
	}
	return attrs
}
