//go:build linux

package collector

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// NVMe ioctl constants derived from the Linux kernel uapi/linux/nvme_ioctl.h.
//
// NVME_IOCTL_ADMIN_CMD is _IOWR('N', 0x41, struct nvme_admin_cmd).
// The kernel struct nvme_admin_cmd is 72 bytes on 64-bit systems.
//
// _IOWR(type, nr, size) = (3 << 30) | (type << 8) | nr | (size << 16)
//
//	3      = read+write direction (IOWR)
//	'N'    = 0x4E
//	0x41   = admin command number
//	72     = sizeof(struct nvme_admin_cmd)
const nvmeIoctlAdminCmd uintptr = 0xC0484E41

// nvme admin command opcodes.
const (
	nvmeAdminOpGetLogPage     uint8  = 0x02
	nvmeAdminOpIdentify       uint8  = 0x06
	nvmeLogPageSMARTHealth    uint32 = 0x02
	nvmeSmartLogSize          uint32 = 512
	nvmeIdentifyDataLen       uint32 = 4096
	nvmeIdentifyCNSController uint32 = 1
	nvmeNSIDBroadcast         uint32 = 0xFFFFFFFF
)

// nvmeAdminCmd mirrors the kernel struct nvme_admin_cmd (72 bytes, packed).
// See linux/nvme_ioctl.h.
type nvmeAdminCmd struct {
	Opcode      uint8
	Flags       uint8
	Rsvd1       uint16
	Nsid        uint32
	Cdw2        uint32
	Cdw3        uint32
	Metadata    uint64
	Addr        uint64
	MetadataLen uint32
	DataLen     uint32
	Cdw10       uint32
	Cdw11       uint32
	Cdw12       uint32
	Cdw13       uint32
	Cdw14       uint32
	Cdw15       uint32
	TimeoutMs   uint32
	Result      uint32
}

// nvmeSmartLog is the NVMe SMART / Health Information Log (Log ID 0x02).
// Defined in NVMe Base Specification, section 5.14.1.2.
// Total size: 512 bytes. All multi-byte fields are little-endian.
// 128-bit counters occupy 16 bytes; we read only the lower 8 bytes.
type nvmeSmartLog struct {
	CriticalWarning         uint8
	Temperature             [2]uint8 // Kelvin, little-endian
	AvailableSpare          uint8
	AvailableSpareThreshold uint8
	PercentageUsed          uint8
	EnduranceGroupCritWarn  uint8
	_                       [25]uint8 // reserved
	DataUnitsRead           [16]uint8 // 128-bit, units of 512 KiB
	DataUnitsWritten        [16]uint8
	HostReadCommands        [16]uint8
	HostWriteCommands       [16]uint8
	ControllerBusyTime      [16]uint8
	PowerCycles             [16]uint8
	PowerOnHours            [16]uint8
	UnsafeShutdowns         [16]uint8
	MediaErrors             [16]uint8
	NumErrLogEntries        [16]uint8
	WarningTempTime         uint32
	CriticalCompTime        uint32
	// remaining bytes to pad to 512 are not needed
}

// ErrSMARTPermission is returned when the device cannot be opened due to
// insufficient privileges.
type ErrSMARTPermission struct {
	DevPath string
}

func (e *ErrSMARTPermission) Error() string {
	return fmt.Sprintf("SMART data unavailable for %s: permission denied "+
		"(run with elevated privileges or add user to 'disk' group)", e.DevPath)
}

// ErrSMARTIoctl is returned when an ioctl call fails.
type ErrSMARTIoctl struct {
	DevPath string
	Op      string
	Errno   syscall.Errno
}

func (e *ErrSMARTIoctl) Error() string {
	return fmt.Sprintf("SMART ioctl %s on %s failed: %v", e.Op, e.DevPath, e.Errno)
}

// nvmeNameFromDisk returns the NVMe character device path for a disk name.
// For a disk like "nvme0n1" the admin device is "/dev/nvme0".
// NVMe block devices follow the pattern nvme<ctrl>n<ns>, so we strip
// everything from the namespace separator 'n' onward, where the 'n' is
// preceded only by the controller number digits.
func nvmeNameFromDisk(devName string) string {
	// Walk past the "nvme" prefix, then past the controller digits,
	// and strip the remaining "n<ns>" suffix.
	const prefix = "nvme"
	if !strings.HasPrefix(devName, prefix) {
		return "/dev/" + devName
	}
	rest := devName[len(prefix):]
	// rest is like "0n1" — skip leading digits (controller number).
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	// rest[:i] is the controller number; stop here.
	return "/dev/" + prefix + rest[:i]
}

// readNVMeSMART reads the NVMe SMART/Health log and Identify Controller data
// from the given NVMe character device (e.g. /dev/nvme0) and populates the
// relevant fields of info.  It returns the first error that prevents any data
// from being collected; partial results are written directly into info.
func readNVMeSMART(devPath string, info *types.DiskInfo) error {
	f, err := os.OpenFile(devPath, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) {
			return &ErrSMARTPermission{DevPath: devPath}
		}
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close NVMe device", "path", devPath, "err", err)
		}
	}()

	fd := f.Fd()

	// --- SMART / Health Information Log (Log ID 0x02) ---
	var smartBuf [nvmeSmartLogSize]byte
	smartCmd := nvmeAdminCmd{
		Opcode:  nvmeAdminOpGetLogPage,
		Nsid:    nvmeNSIDBroadcast,
		Addr:    uint64(uintptr(unsafe.Pointer(&smartBuf[0]))),
		DataLen: nvmeSmartLogSize,
		// Cdw10: Log ID in bits [7:0]; NUMDL (number of dwords - 1) in bits [31:16].
		// (512 bytes / 4 bytes per dword) - 1 = 127 = 0x7F
		Cdw10: nvmeLogPageSMARTHealth | ((nvmeSmartLogSize/4 - 1) << 16),
	}
	if errno := nvmeIoctl(fd, &smartCmd); errno != 0 {
		return &ErrSMARTIoctl{DevPath: devPath, Op: "GetLogPage(SMART)", Errno: errno}
	}

	applyNVMeSMARTLog((*nvmeSmartLog)(unsafe.Pointer(&smartBuf[0])), info)

	// --- Identify Controller (CNS 0x01) ---
	var identBuf [nvmeIdentifyDataLen]byte
	identCmd := nvmeAdminCmd{
		Opcode:  nvmeAdminOpIdentify,
		Nsid:    0,
		Addr:    uint64(uintptr(unsafe.Pointer(&identBuf[0]))),
		DataLen: nvmeIdentifyDataLen,
		Cdw10:   nvmeIdentifyCNSController,
	}
	if errno := nvmeIoctl(fd, &identCmd); errno != 0 {
		// Identify failure is non-fatal; SMART data is already populated.
		return nil
	}

	applyNVMeIdentify(identBuf[:], info)

	return nil
}

// applyNVMeSMARTLog translates a decoded SMART/Health log page into the disk
// record. It is separate from readNVMeSMART so the decoding can be exercised
// without the privileged ioctl that fetches the page.
func applyNVMeSMARTLog(log *nvmeSmartLog, info *types.DiskInfo) {
	// Temperature: two bytes, little-endian, in Kelvin.
	tempK := uint16(log.Temperature[0]) | uint16(log.Temperature[1])<<8
	if tempK > 273 {
		info.Temperature = float64(tempK) - 273.15
	}

	info.CriticalWarning = int(log.CriticalWarning)
	info.AvailableSparePercent = int(log.AvailableSpare)
	info.SpareThresholdPercent = int(log.AvailableSpareThreshold)
	info.WearLevelPercent = int(log.PercentageUsed)
	info.LifeRemainingPercent = 100 - int(log.PercentageUsed)
	info.DataUnitsRead = binary.LittleEndian.Uint64(log.DataUnitsRead[:8])
	info.DataUnitsWritten = binary.LittleEndian.Uint64(log.DataUnitsWritten[:8])
	info.PowerCycles = binary.LittleEndian.Uint64(log.PowerCycles[:8])
	info.PowerOnHours = binary.LittleEndian.Uint64(log.PowerOnHours[:8])
	info.UnsafeShutdowns = binary.LittleEndian.Uint64(log.UnsafeShutdowns[:8])
	info.MediaErrors = binary.LittleEndian.Uint64(log.MediaErrors[:8])
	info.ErrorLogEntries = binary.LittleEndian.Uint64(log.NumErrLogEntries[:8])
	info.WarningTempTime = uint64(log.WarningTempTime)
	info.CriticalTempTime = uint64(log.CriticalCompTime)

	if info.PowerOnHours > 0 && info.WearLevelPercent > 0 {
		info.EstimatedHoursLeft = (info.PowerOnHours * uint64(100-info.WearLevelPercent)) /
			uint64(info.WearLevelPercent)
	}

	info.SMARTEnabled = true
	info.SMARTHealthy = log.CriticalWarning == 0
}

// applyNVMeIdentify reads the firmware revision and NVMe specification version
// out of an Identify Controller data structure.
func applyNVMeIdentify(ident []byte, info *types.DiskInfo) {
	if len(ident) < 84 {
		return
	}

	// Firmware Revision: bytes 64-71, ASCII, right-padded with spaces.
	fw := strings.TrimRight(string(ident[64:72]), " \x00")
	if fw != "" {
		info.FirmwareVersion = fw
	}

	// NVMe Version Register (VR): bytes 80-83, little-endian uint32.
	// Format: MJR[31:16] | MNR[15:8] | TER[7:0]
	vr := binary.LittleEndian.Uint32(ident[80:84])
	major := (vr >> 16) & 0xFFFF
	minor := (vr >> 8) & 0xFF
	tertiary := vr & 0xFF
	if major == 0 {
		return
	}
	if tertiary > 0 {
		info.NVMeVersion = fmt.Sprintf("%d.%d.%d", major, minor, tertiary)
	} else {
		info.NVMeVersion = fmt.Sprintf("%d.%d", major, minor)
	}
}

// nvmeIoctl issues NVME_IOCTL_ADMIN_CMD on the given file descriptor.
func nvmeIoctl(fd uintptr, cmd *nvmeAdminCmd) syscall.Errno {
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		fd,
		nvmeIoctlAdminCmd,
		uintptr(unsafe.Pointer(cmd)),
	)
	if errno != 0 {
		return errno
	}
	return 0
}
