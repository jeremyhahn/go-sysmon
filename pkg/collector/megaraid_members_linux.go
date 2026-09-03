//go:build linux && (amd64 || arm64) && !nomegaraid

package collector

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Reading the drives that make up a hardware RAID volume.
//
// This is the layer that turns "sda is a PERC volume" into the six drives
// behind it, each with its own identity and its own answer about its health.
// Everything below goes through one ioctl on the controller's management node;
// see megaraid_linux.go for the transport and megaraid_config.go for how the
// controller describes which drives belong to which volume.

// megaraidIdentity is what a drive says about itself.
//
// It is read every cycle and NOT cached. Caching it by device ID was the
// obvious optimisation and the wrong one: a device ID names a slot on the
// controller, not the drive in it, so a replaced drive would keep reporting
// under the drive it replaced until the process restarted -- and its serial is
// the Prometheus series identity, so the history would look continuous across
// a hardware change. That is precisely the failure the serial label exists to
// prevent, reintroduced one layer up. Two commands per drive on the slow
// collection tier is not worth it.
type megaraidIdentity struct {
	Vendor  string
	Model   string
	Serial  string
	DevType uint8
}

// isATA reports a SATA drive behind the controller, which answers ATA SMART
// rather than SCSI log pages. The controller reports the bridge's vendor as
// "ATA", which is how smartctl decides to use -d sat+megaraid rather than
// -d megaraid for the same drive.
func (id megaraidIdentity) isATA() bool {
	return strings.EqualFold(id.Vendor, "ATA")
}

// megaraidController is one controller.
type megaraidController struct {
	hostNo uint16
}

// readIdentity asks a drive what it is.
func (c *megaraidController) readIdentity(fd uintptr, deviceID uint16) (megaraidIdentity, error) {
	data, err := megaraidSCSI(fd, c.hostNo, deviceID, buildInquiryCDB(0, inquiryLen), inquiryLen, "INQUIRY")
	if err != nil {
		return megaraidIdentity{}, err
	}
	vendor, model, devType, err := parseInquiry(data)
	if err != nil {
		return megaraidIdentity{}, err
	}
	id := megaraidIdentity{Vendor: vendor, Model: model, DevType: devType}

	// The serial is a separate page, and a drive that will not give one is
	// still a drive: the identity is kept without it rather than discarded.
	const serialPageLen = 64
	if page, err := megaraidSCSI(fd, c.hostNo, deviceID,
		buildInquiryCDB(vpdUnitSerialNumber, serialPageLen), serialPageLen, "INQUIRY VPD 0x80"); err == nil {
		if serial, err := parseUnitSerial(page); err == nil {
			id.Serial = serial
		}
	}

	return id, nil
}

// readMemberHealth asks one drive how it is, by whichever protocol it speaks,
// and records the answer on the member.
func (c *megaraidController) readMemberHealth(fd uintptr, member *types.DiskMember, id megaraidIdentity) {
	deviceID := uint16(member.DeviceID)

	if id.isATA() {
		c.readATAMemberHealth(fd, member, deviceID)
		return
	}
	c.readSCSIMemberHealth(fd, member, deviceID)
}

// readATAMemberHealth tunnels the ATA SMART command already built for direct
// SATA disks through the controller. The 512-byte sector that comes back is the
// same sector, so it is decoded by the same code.
func (c *megaraidController) readATAMemberHealth(fd uintptr, member *types.DiskMember, deviceID uint16) {
	readData := buildSMART16CDB(smartCmdReadData)
	data, err := megaraidSCSI(fd, c.hostNo, deviceID, readData[:], smartDataSize, "ATA SMART READ DATA")
	if err != nil {
		member.SMARTStatus = types.SMARTUnreadable
		member.SMARTReason = err.Error()
		return
	}
	if err := validateSMARTSector("megaraid device "+strconv.Itoa(int(deviceID)), data); err != nil {
		member.SMARTStatus = types.SMARTUnreadable
		member.SMARTReason = err.Error()
		return
	}

	// Thresholds are optional: without them an attribute has no pass/fail line,
	// which costs the per-attribute verdict and not the reading.
	readThresholds := buildSMART16CDB(smartCmdReadThresholds)
	thresholds, thrErr := megaraidSCSI(fd, c.hostNo, deviceID, readThresholds[:], smartDataSize,
		"ATA SMART READ THRESHOLDS")

	attrs := parseATASMARTAttrs(data, thresholds, thrErr == nil)
	if len(attrs) == 0 {
		// An empty attribute set is an absence of evidence, and applying it
		// would set "healthy" from having learned nothing.
		member.SMARTStatus = types.SMARTUnreadable
		member.SMARTReason = "the drive answered with no decodable SMART attributes"
		return
	}

	// applyATASMARTAttrs works on a DiskInfo, which is the shape the decoder
	// has always written into; the two fields a member has are copied across
	// rather than duplicating the decoder for a second type.
	var scratch types.DiskInfo
	applyATASMARTAttrs(attrs, &scratch)
	member.Temperature = scratch.Temperature
	member.SMARTEnabled = scratch.SMARTEnabled
	member.SMARTHealthy = scratch.SMARTHealthy
	if member.SMARTHealthy {
		member.SMARTStatus = types.SMARTOK
		return
	}
	member.SMARTStatus = types.SMARTFailing
}

// readSCSIMemberHealth asks a SAS drive for its Informational Exceptions log,
// which is where a drive that expects to fail says so.
func (c *megaraidController) readSCSIMemberHealth(fd uintptr, member *types.DiskMember, deviceID uint16) {
	const logPageLen = 64

	page, err := megaraidSCSI(fd, c.hostNo, deviceID,
		buildLogSenseCDB(logPageInformationalExceptions, logPageLen), logPageLen, "LOG SENSE 0x2F")
	if err != nil {
		member.SMARTStatus = types.SMARTUnreadable
		member.SMARTReason = err.Error()
		return
	}
	health, err := parseInformationalExceptions(page)
	if err != nil {
		member.SMARTStatus = types.SMARTUnreadable
		member.SMARTReason = err.Error()
		return
	}

	member.SMARTEnabled = true
	member.SMARTHealthy = !health.Failing
	member.Temperature = health.TemperatureC
	if health.Failing {
		member.SMARTStatus = types.SMARTFailing
		member.SMARTReason = "the drive reports an informational exception: " +
			"additional sense code " + strconv.Itoa(int(health.ASC)) + "/" + strconv.Itoa(int(health.ASCQ))
	} else {
		member.SMARTStatus = types.SMARTOK
	}

	// The exceptions page carries a temperature only on drives that choose to
	// put one there; the dedicated page is asked for when it did not.
	if member.Temperature == 0 {
		if tempPage, err := megaraidSCSI(fd, c.hostNo, deviceID,
			buildLogSenseCDB(logPageTemperature, logPageLen), logPageLen, "LOG SENSE 0x0D"); err == nil {
			if t, err := parseTemperatureLog(tempPage); err == nil {
				member.Temperature = t
			}
		}
	}
}

// readVolumeMembers returns the drives making up one volume, each with its
// identity and its own answer about its health.
func (c *megaraidController) readVolumeMembers(fd uintptr, cfg *megaraidConfig, vol megaraidVolume) []types.DiskMember {
	drives := cfg.MembersOf(vol)
	members := make([]types.DiskMember, 0, len(drives))

	for _, d := range drives {
		member := types.DiskMember{
			DeviceID:  int(d.DeviceID),
			Enclosure: int(d.Enclosure),
			Slot:      int(d.Slot),
			Online:    d.Online(),
		}

		id, err := c.readIdentity(fd, d.DeviceID)
		if err != nil {
			// The drive is in the volume whether or not it answered, so it is
			// reported -- with the reason it said nothing rather than omitted,
			// which would quietly shrink the array.
			member.SMARTStatus = types.SMARTUnreadable
			member.SMARTReason = err.Error()
			members = append(members, member)
			continue
		}
		// The backplane's enclosure processor is not a drive. It never appears
		// in an array, but checking costs nothing and crediting an array with a
		// circuit board would be hard to notice.
		if id.DevType != scsiDeviceTypeDirectAccess {
			continue
		}

		member.Vendor, member.Model, member.Serial = id.Vendor, id.Model, id.Serial
		c.readMemberHealth(fd, &member, id)
		members = append(members, member)
	}
	return members
}

// scsiTargetID returns the SCSI target number a block device is, which is how a
// device is matched to the controller volume it is.
//
// /sys/block/<dev>/device resolves to a path ending in host:channel:id:lun, and
// the id is the controller's target ID for the volume -- so sda ending
// 0:2:0:0 is target 0, which the controller calls volume 0.
func scsiTargetID(name string) (uint8, bool) {
	target := resolvedDevicePath(name)
	if target == "" {
		return 0, false
	}
	fields := strings.Split(target[strings.LastIndex(target, "/")+1:], ":")
	if len(fields) != 4 {
		return 0, false
	}
	id, err := strconv.ParseUint(fields[2], 10, 8)
	if err != nil {
		return 0, false
	}
	return uint8(id), true
}

// megaraidSession is one collect cycle's use of a controller: the open
// management node and the configuration read from it.
//
// It exists so the configuration is read ONCE per cycle rather than once per
// disk. A host with two volumes would otherwise read the whole controller
// configuration twice to answer the same question, and a controller that
// cannot be opened would be tried again for every disk on it.
type megaraidSession struct {
	file   *os.File
	config *megaraidConfig
	ctrl   *megaraidController
	// err is sticky: a controller that failed once in a cycle is not asked
	// again in that cycle, so a failure costs one log line and not one per
	// disk.
	err error
}

// close releases the management node. It is safe on a session that never
// opened one.
func (s *megaraidSession) close() {
	if s == nil || s.file == nil {
		return
	}
	if err := s.file.Close(); err != nil {
		slog.Debug("close megaraid node", "err", err)
	}
	s.file = nil
}

// beginMegaRAID starts a collect cycle's controller sessions.
func (c *DiskCollector) beginMegaRAID() {
	c.megaraidSessions = make(map[uint16]*megaraidSession)
}

// endMegaRAID closes every controller opened during the cycle.
func (c *DiskCollector) endMegaRAID() {
	for _, s := range c.megaraidSessions {
		s.close()
	}
	c.megaraidSessions = nil
}

// megaraidFor returns the session for one controller, opening it and reading
// its configuration the first time it is asked for in a cycle.
func (c *DiskCollector) megaraidFor(hostNo uint16) *megaraidSession {
	if c.megaraidSessions == nil {
		c.megaraidSessions = make(map[uint16]*megaraidSession)
	}
	if s, ok := c.megaraidSessions[hostNo]; ok {
		return s
	}

	s := &megaraidSession{}
	c.megaraidSessions[hostNo] = s

	f, err := openMegaRAIDNode()
	if err != nil {
		s.err = err
		return s
	}
	s.file = f

	reply, err := megaraidDCMD(f.Fd(), hostNo, dcmdConfigRead, nil, megaraidConfigBufferLen, "CFG_READ")
	if err != nil {
		s.err = err
		return s
	}
	cfg, err := parseControllerConfig(reply)
	if err != nil {
		s.err = err
		return s
	}
	s.config = cfg

	s.ctrl = &megaraidController{hostNo: hostNo}
	return s
}

// applyHardwareRAID fills in the member drives and the array health of a block
// device that is a hardware RAID volume.
//
// It runs after applyRAID and defers to it: a device that is an md array is an
// md array, and md is read from world-readable sysfs while this needs an ioctl
// and two capabilities.
func (c *DiskCollector) applyHardwareRAID(name string, info *types.DiskInfo) {
	if info.RAID != nil {
		return
	}
	host, hostNo, ok := scsiHost(name)
	if !ok {
		return
	}
	if readSysfsString(filepath.Join(sysClassSCSIHost, host, "proc_name")) != megaraidDriver {
		return
	}
	target, ok := scsiTargetID(name)
	if !ok {
		return
	}

	session := c.megaraidFor(hostNo)
	if session.err != nil {
		// One warning per controller per run, not one per disk per cycle: a
		// controller that cannot be opened will not start working mid-run.
		warnOnce(c.logger, "disk:megaraid:"+host,
			"disk: RAID controller members unavailable", "host", host, "error", session.err,
			"hint", "reading drives behind a MegaRAID controller needs CAP_SYS_ADMIN and CAP_DAC_OVERRIDE",
			"note", "this is logged once per controller per run")
		return
	}

	for _, vol := range session.config.Volumes {
		if vol.TargetID != target {
			continue
		}
		members := session.ctrl.readVolumeMembers(session.file.Fd(), session.config, vol)
		info.Members = members
		// The array's own shape comes from the configuration, not from what we
		// managed to decode: a drive we could not talk to is still a drive the
		// controller has in service.
		info.RAID = volumeRAIDInfo(vol, session.config.MembersOf(vol))

		// The volume still has no SMART of its own, so the status stays
		// behind_controller -- but the reason can now say where the answer
		// went instead of only that it is not here.
		info.SMARTStatus = types.SMARTBehindController
		info.SMARTReason = fmt.Sprintf(
			"a %s volume on a %s controller: SMART belongs to its %d member drives, which are reported under this device",
			vol.RAIDLevelName(), megaraidDriver, len(members))
		return
	}
}
