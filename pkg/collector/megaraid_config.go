package collector

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Decoding a MegaRAID controller's configuration.
//
// MR_DCMD_CFG_READ returns the whole configuration in one reply: the arrays of
// physical drives, and the virtual disks built on top of them. It is the only
// place that says WHICH drives make up WHICH volume, which is what nesting the
// members under the right volume requires.
//
// Everything here is pure decoding, so it is tested against a real controller's
// reply rather than a synthetic one: testdata/megaraid-config-dellemc01.bin is
// 1120 bytes captured from ci-node-1's PERC H740P Mini on 2026-09-02, holding
// two arrays (four 10K SAS drives, two SATA SSDs) and the two volumes built on
// them. The offsets below were established against that capture with the answer
// already known from `smartctl --scan-open` and the machine's own build notes,
// and the test asserts the decoder reproduces that answer.

// Offsets in the MR_CONFIG reply header.
const (
	confSize       = 0x00 // u32, total length of the configuration
	confArrayCount = 0x04 // u16
	confArraySize  = 0x06 // u16, bytes per array entry
	confLDCount    = 0x08 // u16
	confLDSize     = 0x0A // u16, bytes per volume entry
	confHeaderLen  = 0x20 // arrays begin here
)

// Offsets within one array entry.
const (
	arrayNumDrives   = 0x08 // u16
	arrayMembers     = 0x20 // members begin here
	arrayMemberLen   = 8
	arrayMemberDev   = 0 // u16 device ID, how the firmware addresses the drive
	arrayMemberState = 4 // u16, MR_PD_STATE
	arrayMemberEncl  = 6 // u8
	arrayMemberSlot  = 7 // u8
	// arrayMaxMembers is what the entry has room for: the member list runs to
	// the end of the entry, so a count larger than this is firmware nonsense.
	arrayMaxMembers = 32
)

// Offsets within one volume entry.
const (
	volTargetID  = 0x00 // u8, and the SCSI target the kernel sees it as
	volName      = 0x04 // 16 bytes, user-assigned
	volNameLen   = 16
	volSpanDepth = 0x16 // u8
	volRAIDLevel = 0x20 // u8, packed primary/secondary
	volNumDrives = 0x24 // u8
	volState     = 0x26 // u8
	volSpans     = 0x40 // spans begin here
	volSpanLen   = 24
	volSpanArray = 16 // u16 within a span: which array it is built on
	// volMaxSpans is the room the entry has for spans.
	volMaxSpans = 8
)

// mrPDStateOnline is MR_PD_STATE_ONLINE: a drive in service as part of its
// array.
//
// It is the only physical-drive state named here, and the only one this decoder
// acts on, because it is the only one measured: all six drives on ci-node-1
// report 0x18 while both of its arrays are optimal. Counting ONLY this value as
// active means an unrecognised state is reported as not active, which errs
// toward saying an array has fewer working drives than it has -- the direction
// that raises attention rather than suppresses it.
const mrPDStateOnline uint16 = 0x18

// MegaRAID logical drive states (MR_LD_STATE).
const (
	mrLDOffline           uint8 = 0
	mrLDPartiallyDegraded uint8 = 1
	mrLDDegraded          uint8 = 2
	mrLDOptimal           uint8 = 3
)

// megaraidMember is one physical drive in an array, named the only way it can
// be: by the device ID the firmware addresses it with. It has no block device
// and no name in /sys/block.
type megaraidMember struct {
	DeviceID uint16
	// State is the controller's own verdict on the drive, which is a different
	// question from whether the drive answers a health command: a failed drive
	// can still reply, and a working one can refuse a command it does not
	// implement.
	State     uint16
	Enclosure uint8
	Slot      uint8
}

// Online reports a drive the controller considers in service.
func (m megaraidMember) Online() bool { return m.State == mrPDStateOnline }

// megaraidArray is a set of physical drives the controller treats as one unit.
type megaraidArray struct {
	Members []megaraidMember
}

// megaraidVolume is a virtual disk the controller presents to the kernel as an
// ordinary SCSI disk.
type megaraidVolume struct {
	// TargetID is both the volume's identity to the controller and the SCSI
	// target number the kernel sees, which is what lets a block device be
	// matched to the volume it is.
	TargetID  uint8
	Name      string
	RAIDLevel uint8
	State     uint8
	NumDrives uint8
	SpanDepth uint8
	// ArrayRefs index into megaraidConfig.Arrays, one per span.
	ArrayRefs []uint16
}

// megaraidConfig is a controller's whole configuration.
type megaraidConfig struct {
	Arrays  []megaraidArray
	Volumes []megaraidVolume
}

// MegaRAIDShortResponseError reports a controller reply too small to hold the
// structure it claims to be.
//
// It lives with the pure decoders rather than with the ioctl transport because
// that is what raises it, and because this file is compiled on every target --
// a shared type that lived only in the 64-bit file was one of two symbols that
// broke the unsupported build.
type MegaRAIDShortResponseError struct {
	Op   string
	Want int
	Got  int
}

func (e *MegaRAIDShortResponseError) Error() string {
	return fmt.Sprintf("megaraid: %s returned %d bytes, need at least %d", e.Op, e.Got, e.Want)
}

// MegaRAIDConfigError reports a configuration reply that does not describe itself
// consistently.
type MegaRAIDConfigError struct {
	Detail string
}

func (e *MegaRAIDConfigError) Error() string {
	return "megaraid: controller configuration is not self-consistent: " + e.Detail
}

// parseControllerConfig decodes an MR_DCMD_CFG_READ reply.
//
// Every count and every entry size in the reply comes from firmware, so each is
// checked against the bytes actually present before it is used to index. A
// controller that reports more arrays than it sent is a controller we decline
// to describe, not one we read past the end of.
func parseControllerConfig(reply []byte) (*megaraidConfig, error) {
	if len(reply) < confHeaderLen {
		return nil, &MegaRAIDShortResponseError{Op: "CFG_READ", Want: confHeaderLen, Got: len(reply)}
	}

	// The declared length is bounded from BOTH ends before it is used to
	// slice, and the lower bound is the one that was missing: a reply
	// declaring one byte truncated a 64 KiB buffer to reply[:1], and the very
	// next read walked off it and panicked the collector.
	//
	// That is #20's lesson arriving by another route, in the code written to
	// deal with #20's aftermath. A length that comes from outside is an
	// ASSERTION, NOT A FACT -- the kernel's argsize and this controller's
	// declared size are both numbers a device handed us, and both were used to
	// size memory before being checked against what is possible.
	declared := int(binary.LittleEndian.Uint32(reply[confSize:]))
	if declared < confHeaderLen || declared > len(reply) {
		return nil, &MegaRAIDConfigError{Detail: fmt.Sprintf(
			"declares %d bytes; %d were returned and %d is the smallest possible configuration",
			declared, len(reply), confHeaderLen)}
	}
	reply = reply[:declared]

	arrayCount := int(binary.LittleEndian.Uint16(reply[confArrayCount:]))
	arraySize := int(binary.LittleEndian.Uint16(reply[confArraySize:]))
	ldCount := int(binary.LittleEndian.Uint16(reply[confLDCount:]))
	ldSize := int(binary.LittleEndian.Uint16(reply[confLDSize:]))

	if arraySize < arrayMembers || ldSize < volSpans+volSpanLen {
		return nil, &MegaRAIDConfigError{Detail: fmt.Sprintf(
			"entry sizes too small to hold their own fields (array %d, volume %d)", arraySize, ldSize)}
	}

	arraysEnd := confHeaderLen + arrayCount*arraySize
	volumesEnd := arraysEnd + ldCount*ldSize
	if volumesEnd > len(reply) {
		return nil, &MegaRAIDConfigError{Detail: fmt.Sprintf(
			"%d arrays and %d volumes need %d bytes, %d present", arrayCount, ldCount, volumesEnd, len(reply))}
	}

	cfg := &megaraidConfig{
		Arrays:  make([]megaraidArray, 0, arrayCount),
		Volumes: make([]megaraidVolume, 0, ldCount),
	}

	for i := 0; i < arrayCount; i++ {
		entry := reply[confHeaderLen+i*arraySize : confHeaderLen+(i+1)*arraySize]
		cfg.Arrays = append(cfg.Arrays, parseArray(entry))
	}
	for i := 0; i < ldCount; i++ {
		entry := reply[arraysEnd+i*ldSize : arraysEnd+(i+1)*ldSize]
		cfg.Volumes = append(cfg.Volumes, parseVolume(entry))
	}
	return cfg, nil
}

// parseArray decodes one array entry. The entry is already bounded by the
// caller, so the member count is clamped to the room the entry has.
func parseArray(entry []byte) megaraidArray {
	count := int(binary.LittleEndian.Uint16(entry[arrayNumDrives:]))
	room := (len(entry) - arrayMembers) / arrayMemberLen
	if room > arrayMaxMembers {
		room = arrayMaxMembers
	}
	if count > room {
		count = room
	}

	members := make([]megaraidMember, 0, count)
	for i := 0; i < count; i++ {
		m := entry[arrayMembers+i*arrayMemberLen:]
		members = append(members, megaraidMember{
			DeviceID:  binary.LittleEndian.Uint16(m[arrayMemberDev:]),
			State:     binary.LittleEndian.Uint16(m[arrayMemberState:]),
			Enclosure: m[arrayMemberEncl],
			Slot:      m[arrayMemberSlot],
		})
	}
	return megaraidArray{Members: members}
}

// parseVolume decodes one virtual disk entry, including which arrays its spans
// are built on.
func parseVolume(entry []byte) megaraidVolume {
	vol := megaraidVolume{
		TargetID:  entry[volTargetID],
		Name:      strings.TrimRight(string(entry[volName:volName+volNameLen]), "\x00 "),
		RAIDLevel: entry[volRAIDLevel],
		State:     entry[volState],
		NumDrives: entry[volNumDrives],
		SpanDepth: entry[volSpanDepth],
	}

	spans := int(vol.SpanDepth)
	room := (len(entry) - volSpans) / volSpanLen
	if room > volMaxSpans {
		room = volMaxSpans
	}
	if spans > room {
		spans = room
	}
	for i := 0; i < spans; i++ {
		span := entry[volSpans+i*volSpanLen:]
		vol.ArrayRefs = append(vol.ArrayRefs, binary.LittleEndian.Uint16(span[volSpanArray:]))
	}
	return vol
}

// MembersOf returns every physical drive making up a volume, once each.
//
// EACH DISTINCT ARRAY IS VISITED ONCE, not each span, and the difference is not
// theoretical. ci-node-1's RAID10 has spanDepth 2 and BOTH spans reference the
// same four-drive array -- the controller models the volume as two spans laid
// over one array rather than as two arrays -- so walking spans returns every
// drive twice. A synthetic fixture built from "RAID10 means two arrays" would
// have agreed with the buggy version and passed.
//
// A span whose array reference does not exist contributes nothing rather than a
// guess: a volume reported with fewer members than it has is a smaller claim
// than a volume credited with somebody else's drives.
func (c *megaraidConfig) MembersOf(vol megaraidVolume) []megaraidMember {
	var members []megaraidMember
	seen := make(map[uint16]bool, len(vol.ArrayRefs))
	for _, ref := range vol.ArrayRefs {
		if int(ref) >= len(c.Arrays) || seen[ref] {
			continue
		}
		seen[ref] = true
		members = append(members, c.Arrays[ref].Members...)
	}
	return members
}

// megaraidLevels names the RAID levels seen on real hardware. The byte packs
// the primary and secondary levels together, and only the values actually
// measured are named -- an unrecognised one is reported as the number it is
// rather than guessed at, since a volume labelled with the wrong level is worse
// than one labelled with a number.
var megaraidLevels = map[uint8]string{
	0x00: "raid0",
	0x01: "raid1",
	0x05: "raid5",
	0x06: "raid6",
	0x11: "raid10",
}

// RAIDLevelName renders a volume's level for display.
func (v megaraidVolume) RAIDLevelName() string {
	if name, ok := megaraidLevels[v.RAIDLevel]; ok {
		return name
	}
	return fmt.Sprintf("raid-level-0x%02x", v.RAIDLevel)
}

// ArrayState maps a controller volume state onto the md vocabulary the rest of
// the system already speaks, so there is no second list of "which states mean
// the array is not serving I/O" for pkg/alert and the fleet roll-up to disagree
// over.
//
// An unrecognised state deliberately becomes "", which alert.RAIDNotOperational
// already treats as not operational: a volume whose state we cannot read is
// reported rather than assumed well.
func (v megaraidVolume) ArrayState() string {
	switch v.State {
	case mrLDOptimal, mrLDDegraded, mrLDPartiallyDegraded:
		return "active"
	case mrLDOffline:
		return "broken"
	default:
		return ""
	}
}

// Degraded reports a volume running with reduced redundancy. Offline is not
// degraded -- it is not serving I/O at all, which ArrayState says instead.
func (v megaraidVolume) Degraded() bool {
	return v.State == mrLDDegraded || v.State == mrLDPartiallyDegraded
}

// volumeRAIDInfo describes a hardware volume in the same shape an md array is
// described in, so the alert rules, the metrics and the fleet roll-up need no
// second notion of what a RAID array is.
//
// Members is deliberately left empty: RAIDMember names a block device, and
// these drives are not block devices. They are on DiskInfo.Members instead.
func volumeRAIDInfo(vol megaraidVolume, drives []megaraidMember) *types.RAIDInfo {
	// Active means the CONTROLLER considers the drive in service. It is not
	// "the drive answered a health command", which was the first version of
	// this and is wrong in both directions: a failed drive can still answer,
	// and a working one can refuse a command it does not implement. Conflating
	// them would have made sysmon_raid_active_devices say a degraded array was
	// whole, and a whole one degraded.
	//
	// It is counted over the drives the CONFIGURATION declares, not over the
	// members we managed to decode, and that is the second reason this
	// signature takes the config's own list. Service state is a fact the
	// controller already told us; whether we could then talk to the drive is a
	// fact about US. Counting decoded members conflated the two, so a drive we
	// failed to identify -- or the backplane's enclosure processor, skipped
	// because it is not a drive -- silently reduced the in-sync ratio and made
	// our own decode gap look like a degraded array.
	active := 0
	for _, d := range drives {
		if d.Online() {
			active++
		}
	}
	return &types.RAIDInfo{
		Level:         vol.RAIDLevelName(),
		State:         vol.ArrayState(),
		Degraded:      vol.Degraded(),
		TotalDevices:  int(vol.NumDrives),
		ActiveDevices: active,
		// SYNC STATE IS NOT REPORTED, and the empty string is how that is
		// said. This used to be "idle" and 100 percent, which is what a
		// healthy md array reports and is therefore what "nothing wrong"
		// looks like in this struct -- so a volume in the middle of a rebuild
		// would have been published, drawn and exported as a COMPLETED one.
		//
		// A rebuilding array is one drive from data loss. It is the moment
		// this product exists for, and claiming it finished is the worst
		// available answer. Nothing here asks the controller about rebuild,
		// consistency-check or background-init progress, so nothing here may
		// say anything about it.
		//
		// The consumers already handle the absence: pkg/alert only raises
		// raid_resync for a non-empty, non-idle action, so the rule is
		// honestly silent rather than silently satisfied; DiskPanel treats ""
		// exactly as "idle" and draws no progress; and pkg/metrics omits the
		// series entirely rather than exporting a percentage nobody measured.
		//
		// A rebuild is still not invisible, and it is not invisible because of
		// something measured rather than something assumed: a member that is
		// not MR_PD_STATE_ONLINE reports Online false, which raises a critical
		// raid_member_offline naming the bay. That says what can be proved --
		// this drive is not in service -- instead of guessing at a percentage.
		SyncAction:          "",
		SyncProgressPercent: 0,
	}
}
