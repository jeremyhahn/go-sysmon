package collector

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Classifying an absence.
//
// Every device this file looks at has one thing in common: there is no SMART
// reading for it. What differs is why, and the why is the whole of the answer.
// A device-mapper node has nothing to ask; a PERC volume has drives behind it
// that answer perfectly well; a DELLBOSS card answers that it implements no
// SMART at all; and any ATA device on a host that withholds CAP_SYS_RAWIO is
// simply refused. Measured on ci-node-1 (2026-09-02), all four were true of
// different devices on the same machine at the same moment, and all four
// rendered as "SMART N/A".
//
// Everything here reads world-readable sysfs, so the classification works on an
// unprivileged service -- which matters most exactly when the reason is that
// the service is unprivileged.

// sysClassSCSIHost is the sysfs directory naming each SCSI host's low-level
// driver. It is a variable so tests can point at a synthetic tree.
var sysClassSCSIHost = "/sys/class/scsi_host"

// raidControllerDrivers are SCSI low-level drivers that present hardware RAID
// volumes to the kernel. A block device served by one of these may be a
// virtual disk whose member drives are reachable only through the controller.
//
// It is a list because the kernel exposes no "this is a RAID volume" flag. The
// list is consulted only after a SMART read has already failed, which is what
// keeps it safe: a controller in HBA/JBOD mode passes its disks through and
// they answer normally, so they never reach this code, and a driver missing
// from the list degrades to the generic "unreadable" rather than to a wrong
// answer.
var raidControllerDrivers = map[string]string{
	"megaraid_sas": "LSI/Broadcom MegaRAID (Dell PERC)",
	"aacraid":      "Adaptec/Dell PERC",
	"hpsa":         "HP Smart Array",
	"smartpqi":     "HPE Smart Array",
	"arcmsr":       "Areca",
	"3w-9xxx":      "3ware/LSI",
}

// blockSizeSectors returns a block device's size in 512-byte sectors from
// sysfs, and whether sysfs could answer at all.
//
// The second return is not a formality. /sys/block/<dev>/size reads 0 for a
// device the kernel has named but put no medium behind, and treating an
// unreadable /sys as "0" would read as "every device is empty" and empty the
// disk list on any host without sysfs mounted.
func blockSizeSectors(name string) (uint64, bool) {
	raw := readSysfsString(filepath.Join(sysBlockRoot, name, "size"))
	if raw == "" {
		return 0, false
	}
	sectors, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return sectors, true
}

// hasNoMedium reports whether the kernel has a block device node for name with
// nothing behind it.
//
// Any host with the nbd module loaded gets sixteen such stubs at boot. On
// ci-node-1 they sorted ahead of every real disk, so the disk panel opened on
// nbd0 through nbd15 -- zero bytes, zero rate, no SMART -- and the owner
// reported the machine as showing no disk activity while sdb was writing
// 70 MB/s eighteen rows further down.
//
// The test is whether the kernel put a medium behind the name, not a list of
// names to hide. On that host nbd0 still carried lifetime I/O counters from a
// since-disconnected export, so filtering on "has it ever done I/O" would have
// kept precisely the emptiest one.
func hasNoMedium(name string) bool {
	sectors, known := blockSizeSectors(name)
	return known && sectors == 0
}

// isVirtualBlockDevice reports whether name is a block device with no hardware
// under it, and whether sysfs could answer.
//
// Only a device backed by hardware has a "device" link in sysfs. Device-mapper
// nodes, md arrays, unbound nbd and loop devices have none, which makes this a
// structural test rather than a list of name prefixes to keep in step with the
// kernel.
func isVirtualBlockDevice(name string) (virtual, known bool) {
	if _, err := os.Stat(filepath.Join(sysBlockRoot, name)); err != nil {
		// sysfs cannot say. Make no claim; let the probe decide.
		return false, false
	}
	if _, err := os.Stat(filepath.Join(sysBlockRoot, name, "device")); err != nil {
		return true, true
	}
	return false, true
}

// scsiHostDriver returns the low-level SCSI driver serving a block device --
// "megaraid_sas", "ahci", "mpt3sas" -- or an empty string when the device is
// not a SCSI disk or sysfs cannot say.
//
// /sys/block/<dev>/device resolves to a path containing the SCSI host as a
// component (.../host0/target0:2:0/0:2:0:0), and
// /sys/class/scsi_host/host0/proc_name names the driver.
func scsiHostDriver(name string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(sysBlockRoot, name, "device"))
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(target, string(os.PathSeparator)) {
		if !strings.HasPrefix(part, "host") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(part, "host")); err != nil {
			continue
		}
		return readSysfsString(filepath.Join(sysClassSCSIHost, part, "proc_name"))
	}
	return ""
}

// raidControllerFor returns a human name for the RAID controller serving a
// block device, or an empty string when it is not on one.
func raidControllerFor(name string) string {
	return raidControllerDrivers[scsiHostDriver(name)]
}

// A refusal has two shapes, and they have different remedies. Conflating them
// produces confident advice that cannot work: on ci-node-1 the device node
// opens perfectly well -- the service is in the 'disk' group -- and it is the
// passthrough ioctl that the kernel refuses, so "add the user to the disk
// group" is exactly the wrong thing to tell the operator there.
type refusal int

const (
	notRefused refusal = iota
	// refusedAtOpen: the device node could not be opened. File permissions,
	// so group membership is the remedy.
	refusedAtOpen
	// refusedAtIoctl: the node opened and the kernel rejected the passthrough
	// itself. That is a capability check, which no group membership satisfies.
	refusedAtIoctl
)

// reason returns the sentence to show the operator, naming the remedy that
// actually applies.
func (r refusal) reason() string {
	switch r {
	case refusedAtOpen:
		return "refused: the device node could not be opened -- add the service user to the 'disk' group, or run as root"
	case refusedAtIoctl:
		return "refused by the kernel: the device node opens, but the SMART passthrough itself is rejected. This needs CAP_SYS_RAWIO; group membership does not grant it"
	default:
		return ""
	}
}

// classifyRefusal reports whether err bottoms out in the kernel refusing the
// caller rather than the hardware declining to answer, and at which step.
func classifyRefusal(err error) refusal {
	var perm *ErrSMARTPermission
	if errors.As(err, &perm) {
		return refusedAtOpen
	}
	if errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, os.ErrPermission) {
		return refusedAtIoctl
	}
	return notRefused
}

// probeATASMARTSupported is indirected so tests can classify a failure without
// opening a device node on the machine running them.
var probeATASMARTSupported = ataSMARTSupported

// smartOpenError converts a failure to open a device node into the error the
// classifier expects, so both the ATA and NVMe paths agree on what a refusal
// looks like. They had the same three lines copied between them.
//
// os.ErrPermission alone covers every ordinary authorization denial the open
// can produce, including EPERM -- which is worth stating, because it does not
// look that way. (A seccomp filter can synthesise any errno it likes; no
// finite check classifies that, and nothing here pretends to.) A review
// of this code reported that a device-cgroup or LSM denial returns EPERM from
// open(2) and would fall through unwrapped to be reported as an ioctl-time
// refusal, naming a capability that could not help. Measured instead of
// argued: errors.Is(&os.PathError{Err: syscall.EPERM}, os.ErrPermission) is
// true, because Go maps both EPERM and EACCES onto ErrPermission. Adding an
// explicit syscall.EPERM arm changes nothing, and a test proves it: mutating
// this to check EACCES alone is what fails.
func smartOpenError(devPath string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return &ErrSMARTPermission{DevPath: devPath}
	}
	return err
}

// transportCompleted reports whether the command got through and something
// produced a response, however unusable -- as opposed to never being carried
// at all.
//
// It deliberately does NOT claim the drive answered, and the difference is not
// pedantry: ErrSMARTEmptySector exists precisely because an all-zero payload is
// what a bridge returns when it answers the command WITHOUT asking the drive,
// and a short transfer proves the transfer completed, not who filled it. The
// only thing this predicate is used to decide is whether a RAID controller's
// opacity is still a candidate explanation, and each of these four rules that
// out: bytes came back, so the controller was not what stood in the way.
func transportCompleted(err error) bool {
	var (
		checksum *ErrSMARTChecksum
		empty    *ErrSMARTEmptySector
		short    *ErrSMARTShortTransfer
		noAttrs  *ErrSMARTNoAttributes
	)
	return errors.As(err, &checksum) ||
		errors.As(err, &empty) ||
		errors.As(err, &short) ||
		errors.As(err, &noAttrs)
}

// classifySMARTFailure records why a device produced no SMART reading.
//
// A refusal is reported first, and the reason it is first is not that it is
// more specific but that it is a fault in how the service was deployed and it
// is fixable. Deciding on the controller first looked right -- a PERC volume
// on a host without CAP_SYS_RAWIO is genuinely both -- until you notice that
// the controller test only knows the SCSI host driver, not whether THIS device
// is a volume. On a megaraid_sas controller in JBOD mode, a passthrough disk
// that is merely refused would have been labelled "SMART via controller",
// which is a claim about member drives that do not exist, and it would have
// buried the fixable fault behind it.
//
// The controller fact is not lost: it rides on the reason, because on a volume
// the capability alone will not produce a reading and the operator should know
// that before granting one.
//
// Measured on ci-node-1, which is what makes the ordering safe rather than
// merely defensible: a PERC volume on a host that DOES hold CAP_SYS_RAWIO
// fails with "unsupported scsi opcode" -- a check condition, not EPERM -- so
// behind_controller is still reached on every host where it is the whole
// truth.
func classifySMARTFailure(devName string, err error, info *types.DiskInfo) {
	controller := raidControllerFor(devName)

	if refused := classifyRefusal(err); refused != notRefused {
		info.SMARTStatus = types.SMARTUnreadable
		info.SMARTReason = refused.reason()
		if controller != "" {
			info.SMARTReason += ". Note this device is served by a " + controller +
				" controller: if it is a volume rather than a passthrough disk, SMART belongs to its member drives and no capability will produce a reading here"
		}
		return
	}
	// Bytes came back, so the controller was not what stood in the way. Naming
	// it anyway would send the reader looking for member drives instead of at
	// the device in front of them.
	if controller != "" && !transportCompleted(err) {
		info.SMARTStatus = types.SMARTBehindController
		info.SMARTReason = "served by a " + controller + " controller and did not answer SMART directly." +
			" If it is a volume, SMART belongs to its member drives behind the controller rather than to the volume"
		return
	}
	if supported, probeErr := probeATASMARTSupported("/dev/" + devName); probeErr == nil && !supported {
		info.SMARTStatus = types.SMARTUnsupported
		info.SMARTReason = "the device answered, and reports that it does not implement the SMART feature set"
		return
	}
	info.SMARTStatus = types.SMARTUnreadable
	info.SMARTReason = err.Error()
}
