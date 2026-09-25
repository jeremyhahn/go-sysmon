//go:build linux

package collector

import (
	"fmt"
	"log/slog"
	"os"
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
	return sgIODataIn(fd, devPath, cdb, fmt.Sprintf("SG_IO ATA PASS-THROUGH(16) SMART(0x%02X)", subCmd))
}

// sgIODataIn issues one 16-byte SCSI command that reads a single 512-byte
// sector from the device, and returns it. Both the SMART sub-commands and
// IDENTIFY DEVICE are exactly this shape, and the three ways such a command
// can look successful without being are subtle enough to be worth having in
// one place rather than two.
func sgIODataIn(fd uintptr, devPath string, cdb [16]byte, op string) ([]byte, error) {
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
		return nil, &SMARTIoctlError{DevPath: devPath, Op: op, Errno: errno}
	}

	// info's low bit reflects command status independently of errno; a set bit
	// (e.g. the drive raised a check condition) means no valid data was
	// returned, so report it as a failed passthrough rather than trusting the
	// buffer.
	if hdr.info&sgInfoOKMask != sgInfoOK {
		return nil, &SMARTPassthroughError{
			DevPath:      devPath,
			Op:           op,
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
		return nil, &SMARTShortTransferError{
			DevPath: devPath,
			Op:      op,
			Want:    smartDataSize,
			Got:     smartDataSize - int(hdr.resid),
		}
	}

	return data, nil
}

// SMARTShortTransferError reports a passthrough that succeeded at the command
// level but moved fewer bytes than the sector it was asked for.
type SMARTShortTransferError struct {
	DevPath string
	Op      string
	Want    int
	Got     int
}

func (e *SMARTShortTransferError) Error() string {
	return fmt.Sprintf("smart: %s on %s returned %d of %d bytes", e.Op, e.DevPath, e.Got, e.Want)
}

// SMARTPassthroughError is returned when an SG_IO ATA passthrough completes at the
// ioctl level but the drive or adapter reports a non-OK status, so the response
// buffer holds no valid SMART data.
type SMARTPassthroughError struct {
	DevPath      string
	Op           string
	HostStatus   uint16
	DriverStatus uint16
	Status       uint8
}

func (e *SMARTPassthroughError) Error() string {
	return fmt.Sprintf("smart: %s on %s reported non-OK status (scsi=0x%02x host=0x%04x driver=0x%04x)",
		e.Op, e.DevPath, e.Status, e.HostStatus, e.DriverStatus)
}

// ATA IDENTIFY DEVICE, and why it is worth a second command.
//
// A SMART read that fails tells us the reading did not happen; it does not say
// whether the drive has SMART at all. IDENTIFY DEVICE does, in one bit, and
// the difference is the difference between "we could not read this drive" and
// "this drive has nothing to read" -- which is exactly the distinction #45 is
// about. Measured on ci-node-1: the DELLBOSS boot volume opens, accepts
// IDENTIFY, and reports no SMART support, while every other failure on that
// host is the kernel refusing the caller.
const ataCmdIdentify uint8 = 0xEC

// IDENTIFY DEVICE word offsets (ATA/ATAPI Command Set).
const (
	// identifyWordCommandSets1 is word 82: bit 0 is the SMART feature set.
	identifyWordCommandSets1 = 82
	// identifyWordCommandSets2 is word 83, whose top two bits say whether
	// words 82-84 carry meaning at all: bit 15 clear and bit 14 set.
	identifyWordCommandSets2  = 83
	identifySMARTSupportedBit = 1 << 0
	identifyValidMask         = 0xC000
	identifyValidValue        = 0x4000
)

// buildIdentify16CDB builds the ATA PASS-THROUGH(16) command descriptor block
// for IDENTIFY DEVICE: one 512-byte sector, PIO Data-In. Unlike a SMART
// command it carries no signature in LBA mid/high -- IDENTIFY takes none, and
// setting one makes drives reject the command.
//
// It is a pure function so the wire format can be verified without hardware.
func buildIdentify16CDB() [16]byte {
	return [16]byte{
		ataPassThrough16, // 0: ATA PASS-THROUGH(16)
		0x08,             // 1: PROTOCOL=4 (PIO Data-In) << 1; EXTEND=0
		0x0E,             // 2: T_LENGTH=2 (sector_count), BYT_BLOK=1, T_DIR=1 (from device)
		0x00,             // 3: features (15:8)
		0x00,             // 4: features (7:0)
		0x00,             // 5: sector_count (15:8)
		0x01,             // 6: sector_count (7:0) = 1 sector
		0x00,             // 7: lba_low (15:8)
		0x00,             // 8: lba_low (7:0)
		0x00,             // 9: lba_mid (15:8)
		0x00,             // 10: lba_mid (7:0)
		0x00,             // 11: lba_high (15:8)
		0x00,             // 12: lba_high (7:0)
		0x00,             // 13: device
		ataCmdIdentify,   // 14: command = 0xEC (IDENTIFY DEVICE)
		0x00,             // 15: control
	}
}

// identifyWord returns word n of an IDENTIFY DEVICE response. The words are
// little-endian 16-bit.
func identifyWord(id []byte, n int) uint16 {
	return uint16(id[2*n]) | uint16(id[2*n+1])<<8
}

// identifySMARTSupported decodes whether a drive implements the SMART feature
// set from its IDENTIFY DEVICE response.
//
// It returns an error rather than false when the answer cannot be trusted.
// Words 82-84 are only meaningful when word 83 says so, and a drive that
// leaves them unset must not be recorded as having declared it has no SMART --
// that would turn "the drive did not say" into "the drive said no", which is
// the same substitution of an absence for an answer this whole change exists
// to stop.
func identifySMARTSupported(id []byte) (bool, error) {
	if len(id) < 2*(identifyWordCommandSets2+1) {
		return false, &SMARTShortTransferError{
			Op:   "IDENTIFY DEVICE",
			Want: 2 * (identifyWordCommandSets2 + 1),
			Got:  len(id),
		}
	}
	if identifyWord(id, identifyWordCommandSets2)&identifyValidMask != identifyValidValue {
		return false, &IdentifyIndeterminateError{}
	}
	return identifyWord(id, identifyWordCommandSets1)&identifySMARTSupportedBit != 0, nil
}

// IdentifyIndeterminateError reports an IDENTIFY DEVICE response whose command
// set words are marked invalid, so it cannot say whether SMART is supported.
type IdentifyIndeterminateError struct{}

func (e *IdentifyIndeterminateError) Error() string {
	return "smart: IDENTIFY DEVICE reports its command set words as invalid; SMART support is indeterminate"
}

// ataSMARTSupported asks a device whether it implements SMART, using IDENTIFY
// DEVICE over SG_IO only. It deliberately does not try HDIO_DRIVE_CMD: that
// path segfaults the service on at least one deployed host (go-sysmon #20),
// and this probe runs only after a read has already failed, where a crash
// would be the worst possible way to report bad news.
func ataSMARTSupported(devPath string) (bool, error) {
	f, err := os.OpenFile(devPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close ATA device", "path", devPath, "err", err)
		}
	}()

	data, err := sgIODataIn(f.Fd(), devPath, buildIdentify16CDB(), "SG_IO ATA PASS-THROUGH(16) IDENTIFY DEVICE")
	if err != nil {
		return false, err
	}
	return identifySMARTSupported(data)
}
