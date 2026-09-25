//go:build linux && (amd64 || arm64) && !nomegaraid

package collector

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Talking to the drives behind a MegaRAID controller.
//
// A hardware RAID volume hides its members. The kernel publishes the volume as
// one SCSI disk -- /dev/sda on ci-node-1 -- and the physical drives that make
// it up have no device node, no name in /sys/block and no I/O counters. They
// are reachable only by asking the controller to carry a command to them, which
// is what this file does.
//
// The transport is the MFI ("MegaRAID Firmware Interface") passthrough: one
// ioctl on /dev/megaraid_sas_ioctl_node carrying a frame that names a physical
// drive by its device ID and holds an ordinary SCSI CDB. Whatever the CDB says
// reaches the drive. That means the SAS members answer SCSI LOG SENSE, and the
// SATA members answer the very same ATA PASS-THROUGH(16) command already built
// in smart_ata_sgio.go -- tunnelled one layer deeper.
//
// MEASURED ON ci-node-1, 2026-09-02, and worth stating because the issue that
// prompted this assumed otherwise: this path needs CAP_SYS_ADMIN and
// CAP_DAC_OVERRIDE (to open the 0600 root-owned node and to satisfy the
// driver's check), and it does NOT need CAP_SYS_RAWIO. It never touches the
// SG_IO or HDIO paths that capability governs -- including ataHDIOCommand,
// which crash-loops that host (#20). Both capabilities are already granted
// there, so nothing about the deployment changes to make this work.
//
// Every structure offset below was verified against a C compiler rather than
// read off a header by eye; see megaraid_packet_test.go, which pins them.
//
// THE LAYOUT IS THE 64-BIT ABI, which is what the build tag says. struct iovec
// is 16 bytes, the packet is 404, and the ioctl number encodes that size. On a
// 32-bit target every one of those differs, so the file is built only for the
// architectures whose layout it actually describes; megaraid_unsupported.go
// stands in elsewhere.

// megasasIOCFirmware is MEGASAS_IOC_FIRMWARE, _IOWR('M', 1, struct
// megasas_iocpacket). The size is baked into the ioctl number, so a wrong
// struct size is not a subtle corruption -- the driver rejects it outright with
// ENOTTY, which is the failure mode to want.
const megasasIOCFirmware uintptr = 0xC1944D01

// megaraidIoctlNode is the controller's management node. It is NOT /dev/bus/0:
// that path does not exist, and is smartctl's own synthetic naming for this
// node. Following it is why `smartctl -d megaraid,0 /dev/sda` fails in a way
// that reads as "this hardware cannot report".
//
// A variable so tests can point the session at a node that is absent, or at
// one that refuses the controller command, without a controller on the host.
var megaraidIoctlNode = "/dev/megaraid_sas_ioctl_node"

// Layout of struct megasas_iocpacket, which is __packed. It is assembled as a
// byte slice rather than as a Go struct because Go has no packed attribute and
// the trailing iovec array lands on an odd alignment; writing offsets makes the
// layout explicit and testable without hardware.
const (
	iocHostNo    = 0x00 // u16
	iocSGLOff    = 0x04 // u32, offset of the scatter list WITHIN the frame
	iocSGECount  = 0x08 // u32
	iocSenseOff  = 0x0C // u32, offset of the sense pointer WITHIN the frame
	iocSenseLen  = 0x10 // u32
	iocFrame     = 0x14 // 128 bytes
	iocSGL       = 0x94 // 16 × struct iovec
	iocSGEntries = 16   // MAX_IOCTL_SGE
	iocSGEntry   = 16   // sizeof(struct iovec) on 64-bit
	iocPacketLen = 404  // sizeof(struct megasas_iocpacket)
)

// Layout of struct megasas_pthru_frame, relative to the frame.
const (
	pthruCmd          = 0x00 // u8
	pthruSenseLen     = 0x01 // u8
	pthruCmdStatus    = 0x02 // u8
	pthruSCSIStatus   = 0x03 // u8
	pthruTargetID     = 0x04 // u8
	pthruLUN          = 0x05 // u8
	pthruCDBLen       = 0x06 // u8
	pthruSGECount     = 0x07 // u8
	pthruFlags        = 0x10 // u16
	pthruTimeout      = 0x12 // u16
	pthruDataXferLen  = 0x14 // u32
	pthruSenseAddrLo  = 0x18 // u32, and where the user sense pointer goes
	pthruCDB          = 0x20 // 16 bytes
	pthruSGL          = 0x30 // union megasas_sgl
	pthruSenseAddrOff = pthruSenseAddrLo
)

// Layout of struct megasas_dcmd_frame, relative to the frame.
const (
	dcmdCmd         = 0x00 // u8
	dcmdCmdStatus   = 0x02 // u8
	dcmdSGECount    = 0x07 // u8
	dcmdFlags       = 0x10 // u16
	dcmdTimeout     = 0x12 // u16
	dcmdDataXferLen = 0x14 // u32
	dcmdOpcode      = 0x18 // u32
	dcmdMbox        = 0x1C // 12 bytes
	dcmdSGL         = 0x28 // union megasas_sgl
)

// MFI command and flag values.
const (
	mfiCmdPDSCSIIO  uint8  = 0x04 // MFI_CMD_PD_SCSI_IO
	mfiCmdDCMD      uint8  = 0x05 // MFI_CMD_DCMD
	mfiFrameDirRead uint16 = 0x0010
	// mfiStatusInvalid is written to cmd_status before the call so a driver
	// that returns without touching it cannot be read as success.
	mfiStatusInvalid uint8 = 0xFF
	mfiStatusOK      uint8 = 0x00
)

// dcmdConfigRead is MR_DCMD_CFG_READ: the whole controller configuration in one
// reply -- the arrays of physical drives, and the volumes built on them. It is
// the only place that says which drives make up which volume.
const dcmdConfigRead uint32 = 0x04010000

// megaraidConfigBufferLen is the buffer offered for a configuration reply. The
// reply declares its own length; this is only the ceiling.
const megaraidConfigBufferLen = 64 * 1024

// megaraidDriver is the SCSI low-level driver name this controller reports.
const megaraidDriver = "megaraid_sas"

// megaraidSenseLen is the SCSI sense buffer size requested from the controller.
const megaraidSenseLen = 32

// megaraidTimeoutSeconds bounds one passthrough command at the firmware.
const megaraidTimeoutSeconds uint16 = 10

// MegaRAIDStatusError reports a passthrough the controller accepted and did not
// complete. Status is the MFI command status.
type MegaRAIDStatusError struct {
	Op       string
	DeviceID uint16
	Status   uint8
}

func (e *MegaRAIDStatusError) Error() string {
	return fmt.Sprintf("megaraid: %s for device %d returned MFI status 0x%02X", e.Op, e.DeviceID, e.Status)
}

// MegaRAIDIoctlError reports a failed MEGASAS_IOC_FIRMWARE call.
type MegaRAIDIoctlError struct {
	Op    string
	Errno syscall.Errno
}

func (e *MegaRAIDIoctlError) Error() string {
	return fmt.Sprintf("megaraid: MEGASAS_IOC_FIRMWARE (%s) failed: %v", e.Op, e.Errno)
}

// Unwrap exposes the errno so a refusal is visible to errors.Is.
func (e *MegaRAIDIoctlError) Unwrap() error { return e.Errno }

// megaraidMaxTargetID is the largest device ID the MFI passthrough frame can
// carry: target_id is a single byte in the structure the driver reads.
const megaraidMaxTargetID = 0xFF

// MegaRAIDUnaddressableError reports a drive whose device ID does not fit the
// frame field that must carry it.
//
// It exists so the alternative cannot happen. Narrowing a uint16 device ID to
// the frame's uint8 sends every command for drive 0x0101 to drive 0x01 -- and
// if that target exists, its serial, its SMART result and its temperature come
// back and are recorded against the wrong member.
//
// That is a different species from every other failure this package guards
// against. The rest are an absence rendered as health: a missing reading shown
// as fine. This one is ONE DRIVE'S HEALTH REPORTED AS ANOTHER'S -- a positive,
// specific, wrong answer with a real serial attached, which nothing downstream
// could detect and which would represent a failing drive with a healthy
// sibling's data.
//
// A width a device gave us is not ours to narrow silently.
type MegaRAIDUnaddressableError struct {
	Op       string
	DeviceID uint16
}

func (e *MegaRAIDUnaddressableError) Error() string {
	return fmt.Sprintf("megaraid: %s cannot address device %d: the passthrough frame carries an 8-bit target and %d does not fit",
		e.Op, e.DeviceID, e.DeviceID)
}

// buildPassthroughPacket assembles a MEGASAS_IOC_FIRMWARE packet carrying one
// SCSI CDB to a physical drive behind the controller.
//
// It is pure apart from the two userspace pointers the caller patches in, which
// is what lets the wire format be verified without a RAID controller to hand.
// It returns an error rather than truncating a device ID that does not fit the
// frame: see MegaRAIDUnaddressableError.
func buildPassthroughPacket(hostNo, deviceID uint16, cdb []byte, dataLen int, op string) ([]byte, error) {
	if deviceID > megaraidMaxTargetID {
		return nil, &MegaRAIDUnaddressableError{Op: op, DeviceID: deviceID}
	}
	pkt := make([]byte, iocPacketLen)
	binary.LittleEndian.PutUint16(pkt[iocHostNo:], hostNo)

	frame := pkt[iocFrame:]
	frame[pthruCmd] = mfiCmdPDSCSIIO
	// 0xFF, not 0: a driver that returns without writing a status must not be
	// readable as success.
	frame[pthruCmdStatus] = mfiStatusInvalid
	frame[pthruTargetID] = uint8(deviceID)
	frame[pthruLUN] = 0
	frame[pthruCDBLen] = uint8(len(cdb))
	frame[pthruSenseLen] = megaraidSenseLen
	binary.LittleEndian.PutUint16(frame[pthruFlags:], mfiFrameDirRead)
	binary.LittleEndian.PutUint16(frame[pthruTimeout:], megaraidTimeoutSeconds)
	binary.LittleEndian.PutUint32(frame[pthruDataXferLen:], uint32(dataLen))
	copy(frame[pthruCDB:pthruCDB+16], cdb)

	if dataLen > 0 {
		frame[pthruSGECount] = 1
		binary.LittleEndian.PutUint32(pkt[iocSGECount:], 1)
		binary.LittleEndian.PutUint32(pkt[iocSGLOff:], pthruSGL)
	}
	// The kernel reads the DESTINATION for the sense data out of the frame at
	// this offset -- from the copy of the packet we hand it, after the command
	// completes. So the frame must carry our own pointer there, not a
	// placeholder; the caller patches it in.
	binary.LittleEndian.PutUint32(pkt[iocSenseOff:], pthruSenseAddrOff)
	binary.LittleEndian.PutUint32(pkt[iocSenseLen:], megaraidSenseLen)
	return pkt, nil
}

// buildDCMDPacket assembles a packet carrying a management command to the
// controller itself rather than to a drive.
func buildDCMDPacket(hostNo uint16, opcode uint32, mbox []byte, dataLen int) []byte {
	pkt := make([]byte, iocPacketLen)
	binary.LittleEndian.PutUint16(pkt[iocHostNo:], hostNo)

	frame := pkt[iocFrame:]
	frame[dcmdCmd] = mfiCmdDCMD
	frame[dcmdCmdStatus] = mfiStatusInvalid
	frame[dcmdSGECount] = 1
	binary.LittleEndian.PutUint16(frame[dcmdFlags:], mfiFrameDirRead)
	binary.LittleEndian.PutUint16(frame[dcmdTimeout:], megaraidTimeoutSeconds)
	binary.LittleEndian.PutUint32(frame[dcmdDataXferLen:], uint32(dataLen))
	binary.LittleEndian.PutUint32(frame[dcmdOpcode:], opcode)
	copy(frame[dcmdMbox:dcmdMbox+12], mbox)

	binary.LittleEndian.PutUint32(pkt[iocSGECount:], 1)
	binary.LittleEndian.PutUint32(pkt[iocSGLOff:], dcmdSGL)
	return pkt
}

// megaraidIssue sends one assembled packet, with data as the single scatter
// entry, and returns the MFI command status the controller reported.
//
// The two pointer patches happen here and nowhere else, so every use of unsafe
// in this file is in one place: the scatter list entry the kernel reads to find
// our buffer, and the sense destination it reads to write sense data back.
func megaraidIssue(fd uintptr, pkt []byte, statusOff int, data, sense []byte, op string) (uint8, error) {
	// Written byte-wise rather than through a *uint64. struct
	// megasas_iocpacket is __packed, so the scatter list lands at offset 148 --
	// four modulo eight -- and the sense pointer at 44. Creating a typed
	// pointer at an address the ABI deliberately misaligns relies on unaligned
	// typed stores; encoding the bytes does not, and costs nothing.
	//
	// The GC cannot see a pointer stored this way, which is why the buffers are
	// kept alive explicitly below. The kernel reads them during the ioctl and
	// nothing else refers to them afterwards.
	if len(data) > 0 {
		binary.LittleEndian.PutUint64(pkt[iocSGL:], uint64(uintptr(unsafe.Pointer(&data[0]))))
		binary.LittleEndian.PutUint64(pkt[iocSGL+8:], uint64(len(data)))
	}
	if len(sense) > 0 {
		off := iocFrame + int(binary.LittleEndian.Uint32(pkt[iocSenseOff:]))
		binary.LittleEndian.PutUint64(pkt[off:], uint64(uintptr(unsafe.Pointer(&sense[0]))))
	}

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, megasasIOCFirmware,
		uintptr(unsafe.Pointer(&pkt[0])))

	// The buffers must outlive the call: the kernel writes into them, and
	// nothing else in this function refers to them afterwards.
	keepAlive(data)
	keepAlive(sense)

	if errno != 0 {
		return 0, &MegaRAIDIoctlError{Op: op, Errno: errno}
	}
	return pkt[iocFrame+statusOff], nil
}

// megaraidSCSI carries one SCSI CDB to a physical drive behind the controller
// and returns the response bytes.
func megaraidSCSI(fd uintptr, hostNo, deviceID uint16, cdb []byte, dataLen int, op string) ([]byte, error) {
	pkt, err := buildPassthroughPacket(hostNo, deviceID, cdb, dataLen, op)
	if err != nil {
		return nil, err
	}
	data := make([]byte, dataLen)
	sense := make([]byte, megaraidSenseLen)

	status, issueErr := megaraidIssue(fd, pkt, pthruCmdStatus, data, sense, op)
	if issueErr != nil {
		return nil, issueErr
	}
	if status != mfiStatusOK {
		return nil, &MegaRAIDStatusError{Op: op, DeviceID: deviceID, Status: status}
	}

	// The MFI status says the CONTROLLER carried the command. The SCSI status
	// says what the DRIVE made of it, and they are different questions: a
	// drive can answer CHECK CONDITION -- "that command failed, here is why"
	// -- while the controller reports it delivered the message perfectly.
	//
	// Reading only the first was treating a drive that answered "error" as a
	// drive that answered. On a check condition the buffer holds no partial
	// reading; it holds nothing, and a drive on its way out is exactly the one
	// that returns it.
	if err := checkSCSIStatus(pkt, sense, op, deviceID); err != nil {
		return nil, err
	}
	return data, nil
}

// checkSCSIStatus reports whether the drive accepted the command, reading the
// SCSI status the controller wrote back into the frame.
//
// It is a function over the packet bytes rather than a condition inside the
// ioctl path, for the same reason buildHDIOArgs and hdioResponse are: the
// decision cannot be exercised without a drive that is failing on cue, and
// "did the drive answer" is not something to leave untested.
func checkSCSIStatus(pkt, sense []byte, op string, deviceID uint16) error {
	if len(pkt) < iocFrame+pthruSCSIStatus+1 {
		return &MegaRAIDShortResponseError{Op: op, Want: iocFrame + pthruSCSIStatus + 1, Got: len(pkt)}
	}
	status := pkt[iocFrame+pthruSCSIStatus]
	if status == scsiStatusGood {
		return nil
	}
	return &SCSIStatusError{Op: op, DeviceID: deviceID, Status: status, Sense: parseSenseKey(sense)}
}

// scsiStatusGood is SCSI status GOOD. Anything else -- CHECK CONDITION (0x02),
// BUSY (0x08), RESERVATION CONFLICT (0x18) -- means the command did not
// produce data.
const scsiStatusGood uint8 = 0x00

// senseKey is the drive's own account of why a command failed, decoded far
// enough to be actionable in a log line.
type senseKey struct {
	Key  uint8
	ASC  uint8
	ASCQ uint8
	// Valid is false when the sense buffer held nothing decodable, which is
	// itself worth saying rather than reporting three zeroes as a diagnosis.
	Valid bool
}

// parseSenseKey decodes SCSI sense data in either the fixed or the descriptor
// format. It is pure, so the layout can be checked without a drive that is
// failing on cue.
func parseSenseKey(sense []byte) senseKey {
	if len(sense) < 4 {
		return senseKey{}
	}
	switch sense[0] & 0x7F {
	case 0x70, 0x71: // fixed format: key in byte 2, ASC/ASCQ at 12/13
		if len(sense) < 14 {
			return senseKey{}
		}
		return senseKey{Key: sense[2] & 0x0F, ASC: sense[12], ASCQ: sense[13], Valid: true}
	case 0x72, 0x73: // descriptor format: key, ASC and ASCQ in bytes 1-3
		return senseKey{Key: sense[1] & 0x0F, ASC: sense[2], ASCQ: sense[3], Valid: true}
	}
	return senseKey{}
}

// SCSIStatusError reports a command the controller delivered and the drive
// refused.
type SCSIStatusError struct {
	Op       string
	DeviceID uint16
	Status   uint8
	Sense    senseKey
}

func (e *SCSIStatusError) Error() string {
	if !e.Sense.Valid {
		return fmt.Sprintf("megaraid: %s on device %d returned SCSI status 0x%02X with no decodable sense",
			e.Op, e.DeviceID, e.Status)
	}
	return fmt.Sprintf("megaraid: %s on device %d returned SCSI status 0x%02X (sense key 0x%X, ASC/ASCQ %02X/%02X)",
		e.Op, e.DeviceID, e.Status, e.Sense.Key, e.Sense.ASC, e.Sense.ASCQ)
}

// megaraidDCMD sends a management command to the controller and returns its
// reply.
func megaraidDCMD(fd uintptr, hostNo uint16, opcode uint32, mbox []byte, dataLen int, op string) ([]byte, error) {
	pkt := buildDCMDPacket(hostNo, opcode, mbox, dataLen)
	data := make([]byte, dataLen)

	status, err := megaraidIssue(fd, pkt, dcmdCmdStatus, data, nil, op)
	if err != nil {
		return nil, err
	}
	if status != mfiStatusOK {
		return nil, &MegaRAIDStatusError{Op: op, Status: status}
	}
	return data, nil
}

// openMegaRAIDNode opens the controller management node.
func openMegaRAIDNode() (*os.File, error) {
	f, err := os.OpenFile(megaraidIoctlNode, os.O_RDWR, 0)
	if err != nil {
		return nil, smartOpenError(megaraidIoctlNode, err)
	}
	return f, nil
}

// keepAlive stops the compiler concluding a buffer is dead while the kernel is
// still writing into it. It takes a slice rather than the usual any so the call
// sites read as what they are.
func keepAlive(b []byte) {
	if len(b) > 0 {
		runtime.KeepAlive(&b[0])
	}
}
