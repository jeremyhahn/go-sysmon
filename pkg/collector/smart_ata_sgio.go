//go:build linux

package collector

import (
	"fmt"
	"syscall"
	"unsafe"
)

// SG_IO passthrough for ATA SMART.
//
// The legacy HDIO_DRIVE_CMD ioctl in smart_ata.go is the fastest path, but many
// controllers reject it (it returns EIO or ENOTTY): SATA disks behind an AHCI
// controller with the libata SAT layer, and — importantly for small hosts like
// a Raspberry Pi — SATA and NVMe disks behind a USB bridge. Those devices do
// answer SMART through SCSI ATA PASS-THROUGH(16) issued over the SG_IO ioctl,
// which is exactly what `smartctl -d sat` does. This file adds that path so ATA
// SMART works on the wide range of controllers HDIO_DRIVE_CMD does not cover.
//
// The 512-byte payloads returned here are byte-identical in layout to the ones
// HDIO_DRIVE_CMD returns, so the existing parse/apply/decode code is reused
// unchanged.

// sgIO is the SG_IO ioctl request number (scsi/sg.h: SG_IO).
const sgIO uintptr = 0x2285

// dxferFromDev is SG_DXFER_FROM_DEV: a data-in transfer (device → host).
const dxferFromDev int32 = -3

// sgInfoOKMask / sgInfoOK: the low bit of sg_io_hdr.info reports command status
// independent of errno (scsi/sg.h). A clear bit means the command completed OK.
const (
	sgInfoOKMask uint32 = 0x1
	sgInfoOK     uint32 = 0x0
)

// ataPassThrough16 is the SCSI opcode for ATA PASS-THROUGH(16).
const ataPassThrough16 uint8 = 0x85

// sgTimeoutMillis bounds a single passthrough command.
const sgTimeoutMillis uint32 = 5000

// senseBufLen is the SCSI sense buffer size requested from the kernel.
const senseBufLen = 32

// sgIoHdr mirrors the kernel sg_io_hdr_t (scsi/sg.h). Field order, sizes and
// alignment must match the C struct exactly; the pointer fields size correctly
// on both 64-bit (amd64/arm64) and 32-bit (armv7) targets.
type sgIoHdr struct {
	interfaceID    int32
	dxferDirection int32
	cmdLen         uint8
	mxSbLen        uint8
	iovecCount     uint16
	dxferLen       uint32
	dxferp         *byte
	cmdp           *byte
	sbp            *byte
	timeout        uint32
	flags          uint32
	packID         int32
	usrPtr         *byte
	status         uint8
	maskedStatus   uint8
	msgStatus      uint8
	sbLenWr        uint8
	hostStatus     uint16
	driverStatus   uint16
	resid          int32
	duration       uint32
	info           uint32
}

// buildSMART16CDB builds the 16-byte ATA PASS-THROUGH(16) command descriptor
// block for an ATA SMART sub-command (READ DATA 0xD0 or READ THRESHOLDS 0xD1).
// It reads exactly one 512-byte sector into the host via PIO Data-In and pins
// the SMART signature (LBA mid 0x4F, LBA high 0xC2) the drive checks.
//
// It is a pure function so the wire format can be verified without hardware.
func buildSMART16CDB(subCmd uint8) [16]byte {
	return [16]byte{
		ataPassThrough16, // 0: ATA PASS-THROUGH(16)
		0x08,             // 1: PROTOCOL=4 (PIO Data-In) << 1; EXTEND=0
		0x0E,             // 2: T_LENGTH=2 (sector_count), BYT_BLOK=1, T_DIR=1 (from device)
		0x00,             // 3: features (15:8)
		subCmd,           // 4: features (7:0) — SMART sub-command
		0x00,             // 5: sector_count (15:8)
		0x01,             // 6: sector_count (7:0) = 1 sector
		0x00,             // 7: lba_low (15:8)
		0x00,             // 8: lba_low (7:0)
		0x00,             // 9: lba_mid (15:8)
		smartCylLow,      // 10: lba_mid (7:0) = 0x4F
		0x00,             // 11: lba_high (15:8)
		smartCylHigh,     // 12: lba_high (7:0) = 0xC2
		0x00,             // 13: device
		ataCmdSMART,      // 14: command = 0xB0 (SMART)
		0x00,             // 15: control
	}
}

// ataSGIOCommand issues one ATA SMART sub-command through SCSI ATA
// PASS-THROUGH(16) over SG_IO and returns the 512-byte response buffer. It is
// the fallback used when HDIO_DRIVE_CMD is unavailable on the controller.
func ataSGIOCommand(fd uintptr, devPath string, subCmd uint8) ([]byte, error) {
	cdb := buildSMART16CDB(subCmd)
	data := make([]byte, smartDataSize)
	sense := make([]byte, senseBufLen)

	hdr := sgIoHdr{
		interfaceID:    'S',
		dxferDirection: dxferFromDev,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        senseBufLen,
		dxferLen:       smartDataSize,
		dxferp:         &data[0],
		cmdp:           &cdb[0],
		sbp:            &sense[0],
		timeout:        sgTimeoutMillis,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		fd,
		sgIO,
		uintptr(unsafe.Pointer(&hdr)),
	)
	if errno != 0 {
		return nil, &ErrSMARTIoctl{
			DevPath: devPath,
			Op:      fmt.Sprintf("SG_IO ATA PASS-THROUGH(16) SMART(0x%02X)", subCmd),
			Errno:   errno,
		}
	}

	// info's low bit reflects command status independently of errno; a set bit
	// (e.g. the drive raised a check condition) means no valid data was
	// returned, so report it as a failed passthrough rather than trusting the
	// buffer.
	if hdr.info&sgInfoOKMask != sgInfoOK {
		return nil, &ErrSMARTPassthrough{
			DevPath:      devPath,
			Op:           fmt.Sprintf("SG_IO ATA PASS-THROUGH(16) SMART(0x%02X)", subCmd),
			HostStatus:   hdr.hostStatus,
			DriverStatus: hdr.driverStatus,
			Status:       hdr.status,
		}
	}

	// A completed command is not the same as a complete transfer. USB-SATA
	// bridges in particular can report success while returning fewer bytes than
	// asked for; the untouched remainder of the buffer is zeroes, which parse
	// into no attributes at all and would otherwise be applied as a healthy
	// reading of a drive we never actually heard from.
	if hdr.resid != 0 {
		return nil, &ErrSMARTShortTransfer{
			DevPath: devPath,
			Op:      fmt.Sprintf("SG_IO ATA PASS-THROUGH(16) SMART(0x%02X)", subCmd),
			Want:    smartDataSize,
			Got:     smartDataSize - int(hdr.resid),
		}
	}

	return data, nil
}

// ErrSMARTShortTransfer reports a passthrough that succeeded at the command
// level but moved fewer bytes than the sector it was asked for.
type ErrSMARTShortTransfer struct {
	DevPath string
	Op      string
	Want    int
	Got     int
}

func (e *ErrSMARTShortTransfer) Error() string {
	return fmt.Sprintf("smart: %s on %s returned %d of %d bytes", e.Op, e.DevPath, e.Got, e.Want)
}

// ErrSMARTPassthrough is returned when an SG_IO ATA passthrough completes at the
// ioctl level but the drive or adapter reports a non-OK status, so the response
// buffer holds no valid SMART data.
type ErrSMARTPassthrough struct {
	DevPath      string
	Op           string
	HostStatus   uint16
	DriverStatus uint16
	Status       uint8
}

func (e *ErrSMARTPassthrough) Error() string {
	return fmt.Sprintf("smart: %s on %s reported non-OK status (scsi=0x%02x host=0x%04x driver=0x%04x)",
		e.Op, e.DevPath, e.Status, e.HostStatus, e.DriverStatus)
}
