//go:build linux

package collector

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"unsafe"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// HDIO_DRIVE_CMD, from linux/hdreg.h.
//
// The argument is a FOUR-byte block followed by the response buffer:
//
//	args[0]  ATA command
//	args[1]  LBA low / sector number
//	args[2]  feature register
//	args[3]  SECTOR COUNT  <- the kernel copies back 512 x this many bytes
//	args[4:] the response
//
// It is not a set of ATA registers. There is no LBA mid or LBA high here, and
// nowhere to put the 0x4F/0xC2 SMART signature -- the kernel's ata_cmd_ioctl
// synthesises the ATA PASS-THROUGH itself from these four bytes.
//
// BYTE 3 IS A SIZE CONTRACT, NOT AN ADDRESS. It says how much the kernel will
// write into the caller's buffer, so a wrong value there is not a failed probe;
// it is an overrun. This code used to put the SMART signature's 0x4F in it,
// which asked for 79 sectors -- 40,448 bytes into a 516-byte stack buffer --
// and the next allocation died inside runtime.mallocgc. It read as a hardware
// quirk on the one machine whose controller accepts this ioctl (#20).
//
// The comment that produced it described a five-register ATA layout, which is
// true of ATA and false of this interface, and contradicted itself twice: it
// called the block four bytes and then listed five fields, and it placed
// LBA high at buf[4] while also saying the response starts there.
const hdioDriverCmd uintptr = 0x031F

// Offsets within the HDIO_DRIVE_CMD argument block.
const (
	hdioArgCommand     = 0
	hdioArgLBALow      = 1
	hdioArgFeature     = 2
	hdioArgSectorCount = 3
	hdioArgsLen        = 4
)

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
		return smartOpenError(devPath, err)
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

// Unwrap returns both transport errors.
//
// It returned only the SG_IO one -- "the more informative of the two on the
// controllers this fallback exists for" -- while the doc comment above claimed
// both were inspectable. That was merely untidy until classification started
// asking errors.Is(err, syscall.EPERM) through this wrapper: an HDIO refusal
// paired with an SG_IO failure of any other kind was then invisible, and a
// device we were refused would have been reported as one that declined to
// answer. Returning both is what makes the sentence above true.
func (e *ErrSMARTUnavailable) Unwrap() []error { return []error{e.HDIOErr, e.SGIOErr} }

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
	var buf [hdioArgsLen + smartDataSize]byte
	copy(buf[:hdioArgsLen], buildHDIOArgs(subCmd))

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

	return hdioResponse(buf[:]), nil
}

// hdioResponse copies the response out of a completed HDIO_DRIVE_CMD buffer.
//
// The kernel writes its answer after the four-byte argument block, so where the
// data starts and how much of it there is are both consequences of the same
// constant that decides how much the kernel writes. Reading the response at a
// different offset from the one the sector count was sized against is the same
// class of mistake as #20 itself, one step later, so it is a function with a
// test rather than a slice expression in the middle of an ioctl.
func hdioResponse(buf []byte) []byte {
	data := make([]byte, smartDataSize)
	// A buffer that cannot hold a whole sector yields a zeroed one rather than
	// a partial copy. A partly-filled sector would be rejected downstream by
	// the checksum anyway, but it would be rejected as a corrupt reading from
	// the drive rather than as the caller's mistake it actually is.
	if len(buf) >= hdioArgsLen+smartDataSize {
		copy(data, buf[hdioArgsLen:])
	}
	return data
}

// buildHDIOArgs builds the four-byte argument block for one ATA SMART
// sub-command through HDIO_DRIVE_CMD.
//
// It is a pure function for the same reason buildSMART16CDB is one: the wire
// format is the whole of the risk here, and byte 3 in particular decides how
// much the kernel writes into the caller's buffer. A wrong value cannot be
// caught by any check downstream of the ioctl, because by then it has already
// been written.
func buildHDIOArgs(subCmd uint8) []byte {
	args := make([]byte, hdioArgsLen)
	args[hdioArgCommand] = ataCmdSMART
	// No LBA. The SMART signature (0x4F/0xC2) belongs in the ATA
	// PASS-THROUGH(16) CDB, which the kernel builds itself from these bytes;
	// putting it here is what asked for 79 sectors.
	args[hdioArgLBALow] = 0
	args[hdioArgFeature] = subCmd
	// One sector: 512 bytes, which is the size of the buffer we hand over.
	args[hdioArgSectorCount] = smartSectorCount
	return args
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
