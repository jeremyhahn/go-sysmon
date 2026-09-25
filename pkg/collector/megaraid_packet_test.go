//go:build linux && (amd64 || arm64) && !nomegaraid

package collector

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// The ioctl packet's layout is the whole of this file's risk. struct
// megasas_iocpacket is __packed, so nothing about it can be inferred from Go's
// own alignment, and a field written at the wrong offset does not fail loudly
// -- the controller acts on whatever it finds there.
//
// The offsets asserted here were produced by a C compiler against the driver's
// own structure definitions, not read off a header by eye:
//
//	sizeof(struct megasas_iocpacket)                          = 404
//	offsetof(iocpacket, frame)                                = 20
//	offsetof(iocpacket, sgl)                                  = 148
//	offsetof(pthru_frame, cdb)                                = 32
//	offsetof(pthru_frame, sgl)                                = 48
//	offsetof(pthru_frame, sense_buf_phys_addr_lo)             = 24
//	offsetof(dcmd_frame, opcode)                              = 24
//	offsetof(dcmd_frame, mbox)                                = 28
//	offsetof(dcmd_frame, sgl)                                 = 40
//	_IOWR('M', 1, sizeof(struct megasas_iocpacket))           = 0xC1944D01
//
// The ioctl number encodes the structure size, so a wrong size is rejected with
// ENOTTY rather than acted on -- which is the failure mode to want, and the
// reason this constant is worth pinning rather than trusting.

func TestIOCPacketLayout_MatchesTheDriverStructure(t *testing.T) {
	layout := map[string]struct{ got, want int }{
		"sizeof(iocpacket)":   {iocPacketLen, 404},
		"offsetof(frame)":     {iocFrame, 20},
		"offsetof(sgl)":       {iocSGL, 148},
		"sizeof(iovec)":       {iocSGEntry, 16},
		"pthru.cdb":           {pthruCDB, 32},
		"pthru.sgl":           {pthruSGL, 48},
		"pthru.sense_addr_lo": {pthruSenseAddrLo, 24},
		"dcmd.opcode":         {dcmdOpcode, 24},
		"dcmd.mbox":           {dcmdMbox, 28},
		"dcmd.sgl":            {dcmdSGL, 40},
	}
	for name, tc := range layout {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", name, tc.got, tc.want)
		}
	}

	// The frame union is 128 bytes and the scatter list follows it; a packet
	// that cannot hold both is one the driver will read past.
	if iocFrame+128 != iocSGL {
		t.Errorf("frame at %d + 128 != sgl at %d", iocFrame, iocSGL)
	}
	if iocSGL+iocSGEntries*iocSGEntry != iocPacketLen {
		t.Errorf("sgl at %d + %d entries != packet length %d", iocSGL, iocSGEntries, iocPacketLen)
	}
	if megasasIOCFirmware != 0xC1944D01 {
		t.Errorf("MEGASAS_IOC_FIRMWARE = 0x%08X, want 0xC1944D01", megasasIOCFirmware)
	}
}

func TestBuildPassthroughPacket_PlacesEveryField(t *testing.T) {
	cdb := buildInquiryCDB(0, inquiryLen)
	pkt, err := buildPassthroughPacket(7, 9, cdb, inquiryLen, "INQUIRY")
	if err != nil {
		t.Fatalf("buildPassthroughPacket() error = %v", err)
	}

	if len(pkt) != iocPacketLen {
		t.Fatalf("packet is %d bytes, want %d", len(pkt), iocPacketLen)
	}
	if got := binary.LittleEndian.Uint16(pkt[iocHostNo:]); got != 7 {
		t.Errorf("host number = %d, want 7", got)
	}

	frame := pkt[iocFrame:]
	if frame[pthruCmd] != mfiCmdPDSCSIIO {
		t.Errorf("command = 0x%02x, want 0x%02x", frame[pthruCmd], mfiCmdPDSCSIIO)
	}
	// 0xFF, not zero. A driver that returns without writing a status must not
	// leave a packet that reads as success.
	if frame[pthruCmdStatus] != mfiStatusInvalid {
		t.Errorf("initial status = 0x%02x, want 0x%02x", frame[pthruCmdStatus], mfiStatusInvalid)
	}
	if frame[pthruTargetID] != 9 {
		t.Errorf("target = %d, want 9", frame[pthruTargetID])
	}
	if frame[pthruCDBLen] != byte(len(cdb)) {
		t.Errorf("cdb length = %d, want %d", frame[pthruCDBLen], len(cdb))
	}
	if got := frame[pthruCDB]; got != scsiInquiry {
		t.Errorf("cdb was not copied into the frame: first byte 0x%02x", got)
	}
	if got := binary.LittleEndian.Uint16(frame[pthruFlags:]); got != mfiFrameDirRead {
		t.Errorf("direction = 0x%04x, want read", got)
	}
	if got := binary.LittleEndian.Uint32(frame[pthruDataXferLen:]); got != inquiryLen {
		t.Errorf("transfer length = %d, want %d", got, inquiryLen)
	}

	// The kernel reads the sense DESTINATION out of the frame at this offset
	// after the command completes, so the packet has to declare where that is.
	if got := binary.LittleEndian.Uint32(pkt[iocSenseOff:]); got != pthruSenseAddrOff {
		t.Errorf("sense offset = %d, want %d", got, pthruSenseAddrOff)
	}
	if got := binary.LittleEndian.Uint32(pkt[iocSGLOff:]); got != pthruSGL {
		t.Errorf("scatter list offset = %d, want %d", got, pthruSGL)
	}
	if got := binary.LittleEndian.Uint32(pkt[iocSGECount:]); got != 1 {
		t.Errorf("scatter entries = %d, want 1", got)
	}
}

// TestBuildPassthroughPacket_NoDataNoScatterList covers a command that returns
// nothing. Declaring one scatter entry with no buffer would have the kernel map
// a zero-length transfer, which is a different command from the one asked for.
func TestBuildPassthroughPacket_NoDataNoScatterList(t *testing.T) {
	pkt, err := buildPassthroughPacket(0, 0, []byte{0x00, 0, 0, 0, 0, 0}, 0, "TEST UNIT READY")
	if err != nil {
		t.Fatalf("buildPassthroughPacket() error = %v", err)
	}

	if got := binary.LittleEndian.Uint32(pkt[iocSGECount:]); got != 0 {
		t.Errorf("scatter entries = %d, want 0 for a command with no data", got)
	}
	if got := pkt[iocFrame+pthruSGECount]; got != 0 {
		t.Errorf("frame scatter count = %d, want 0", got)
	}
}

func TestBuildDCMDPacket_CarriesOpcodeAndMailbox(t *testing.T) {
	pkt := buildDCMDPacket(3, dcmdConfigRead, []byte{0x05, 0x06}, 1024)

	frame := pkt[iocFrame:]
	if frame[dcmdCmd] != mfiCmdDCMD {
		t.Errorf("command = 0x%02x, want 0x%02x", frame[dcmdCmd], mfiCmdDCMD)
	}
	if frame[dcmdCmdStatus] != mfiStatusInvalid {
		t.Errorf("initial status = 0x%02x, want 0x%02x", frame[dcmdCmdStatus], mfiStatusInvalid)
	}
	if got := binary.LittleEndian.Uint32(frame[dcmdOpcode:]); got != dcmdConfigRead {
		t.Errorf("opcode = 0x%08x, want 0x%08x", got, dcmdConfigRead)
	}
	if frame[dcmdMbox] != 0x05 || frame[dcmdMbox+1] != 0x06 {
		t.Errorf("mailbox = % x, want the bytes passed", frame[dcmdMbox:dcmdMbox+2])
	}
	if got := binary.LittleEndian.Uint32(pkt[iocSGLOff:]); got != dcmdSGL {
		t.Errorf("scatter list offset = %d, want %d (the DCMD frame's, not the passthrough's)", got, dcmdSGL)
	}
	if got := binary.LittleEndian.Uint32(frame[dcmdDataXferLen:]); got != 1024 {
		t.Errorf("transfer length = %d, want 1024", got)
	}
	// A DCMD has no sense buffer; declaring one would have the kernel write to
	// a pointer nothing set.
	if got := binary.LittleEndian.Uint32(pkt[iocSenseLen:]); got != 0 {
		t.Errorf("sense length = %d, want 0 for a management command", got)
	}
}

// A drive that answers "error" is not a drive that answered.
//
// The MFI status says the CONTROLLER carried the command; the SCSI status says
// what the DRIVE made of it. Reading only the first treated a CHECK CONDITION
// as a successful reading, and on a check condition the buffer holds no partial
// data -- it holds nothing. A drive on its way out is exactly the one that
// returns it, which makes this the worst possible place to be optimistic.
func TestParseSenseKey_BothFormats(t *testing.T) {
	tests := []struct {
		name  string
		sense []byte
		want  senseKey
	}{
		{
			// Fixed format, and the reading that matters: 0x5D/0x10 is
			// "hardware impending failure", which is a drive telling us it
			// expects to die.
			name: "fixed format",
			sense: func() []byte {
				b := make([]byte, megaraidSenseLen)
				b[0], b[2], b[12], b[13] = 0x70, 0x01, 0x5D, 0x10
				return b
			}(),
			want: senseKey{Key: 0x01, ASC: 0x5D, ASCQ: 0x10, Valid: true},
		},
		{
			name:  "descriptor format",
			sense: append([]byte{0x72, 0x03, 0x11, 0x04}, make([]byte, 28)...),
			want:  senseKey{Key: 0x03, ASC: 0x11, ASCQ: 0x04, Valid: true},
		},
		{
			// Nothing decodable. Reporting three zeroes would present an
			// absence as a diagnosis of "no error", which is the opposite of
			// what a failed command means.
			name:  "unknown response code",
			sense: append([]byte{0x00, 0x00, 0x00, 0x00}, make([]byte, 28)...),
			want:  senseKey{},
		},
		{
			name:  "truncated",
			sense: []byte{0x70, 0x00},
			want:  senseKey{},
		},
		{
			name:  "fixed format cut short of its ASC",
			sense: []byte{0x70, 0x00, 0x01, 0x00, 0, 0, 0, 0},
			want:  senseKey{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSenseKey(tc.sense); got != tc.want {
				t.Errorf("parseSenseKey() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSCSIStatusError_SaysWhatTheDriveSaid keeps the message actionable. A status
// byte alone does not tell an operator which drive is dying or why.
func TestSCSIStatusError_SaysWhatTheDriveSaid(t *testing.T) {
	withSense := &SCSIStatusError{
		Op: "LOG SENSE 0x2F", DeviceID: 7, Status: 0x02,
		Sense: senseKey{Key: 0x01, ASC: 0x5D, ASCQ: 0x10, Valid: true},
	}
	msg := withSense.Error()
	for _, want := range []string{"LOG SENSE 0x2F", "device 7", "0x02", "5D", "10"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}

	// Undecodable sense says so rather than printing zeroes that read as
	// "no error" on a command that failed.
	bare := &SCSIStatusError{Op: "INQUIRY", DeviceID: 3, Status: 0x08}
	if !strings.Contains(bare.Error(), "no decodable sense") {
		t.Errorf("error %q should say the sense could not be decoded", bare.Error())
	}
}

// TestCheckSCSIStatus_ADriveThatAnsweredErrorDidNotAnswer is the decision the
// transport was missing.
//
// The MFI status says the controller carried the command; this says what the
// drive made of it. They are different questions, and reading only the first
// meant a CHECK CONDITION -- "that command failed, here is why" -- was
// accepted as a reading. The buffer on a check condition holds nothing, not a
// partial answer.
func TestCheckSCSIStatus_ADriveThatAnsweredErrorDidNotAnswer(t *testing.T) {
	packetWith := func(status uint8) []byte {
		pkt, err := buildPassthroughPacket(0, 7, buildInquiryCDB(0, inquiryLen), inquiryLen, "INQUIRY")
		if err != nil {
			t.Fatalf("buildPassthroughPacket() error = %v", err)
		}
		pkt[iocFrame+pthruSCSIStatus] = status
		return pkt
	}
	sense := make([]byte, megaraidSenseLen)
	sense[0], sense[2], sense[12], sense[13] = 0x70, 0x01, 0x5D, 0x10

	// GOOD is the only status that is a reading.
	if err := checkSCSIStatus(packetWith(scsiStatusGood), sense, "INQUIRY", 7); err != nil {
		t.Errorf("a GOOD status was rejected: %v", err)
	}

	for _, status := range []uint8{
		0x02, // CHECK CONDITION -- the one a failing drive returns
		0x08, // BUSY
		0x18, // RESERVATION CONFLICT
		0x28, // TASK SET FULL
	} {
		err := checkSCSIStatus(packetWith(status), sense, "INQUIRY", 7)
		if err == nil {
			t.Errorf("SCSI status 0x%02X was accepted as a successful reading", status)
			continue
		}
		var scsiErr *SCSIStatusError
		if !errors.As(err, &scsiErr) {
			t.Errorf("status 0x%02X produced %T, want *SCSIStatusError", status, err)
			continue
		}
		// The drive's own account has to survive into the error, or the log
		// says a command failed without saying the drive expects to die.
		if scsiErr.Sense.ASC != 0x5D {
			t.Errorf("sense not carried through for status 0x%02X: %+v", status, scsiErr.Sense)
		}
	}

	// A packet too short to hold the status is not a GOOD packet.
	if err := checkSCSIStatus([]byte{0x00}, sense, "INQUIRY", 7); err == nil {
		t.Error("a truncated packet was read as a successful reading")
	}
}

// A device ID that does not fit the frame is refused, not narrowed.
//
// The MFI passthrough frame carries an 8-bit target. Narrowing a uint16 device
// ID to it sends every command for drive 0x0101 to drive 0x01 -- and if that
// target exists, its serial, SMART result and temperature come back and are
// recorded against the wrong member.
//
// That is a different species from the absences this package guards against
// everywhere else. Those show a missing reading as fine; this reports ONE
// DRIVE'S HEALTH AS ANOTHER'S -- a specific, positive, wrong answer carrying a
// real serial, which nothing downstream could detect and which would represent
// a failing drive with a healthy sibling's data.
func TestBuildPassthroughPacket_RefusesAnUnaddressableDevice(t *testing.T) {
	cdb := buildInquiryCDB(0, inquiryLen)

	for _, deviceID := range []uint16{0x0100, 0x0101, 0xFFFF} {
		pkt, err := buildPassthroughPacket(0, deviceID, cdb, inquiryLen, "INQUIRY")
		if err == nil {
			// The assertion that matters: it must not have quietly become a
			// packet aimed at a different drive.
			t.Errorf("device %d produced a packet aimed at target %d",
				deviceID, pkt[iocFrame+pthruTargetID])
			continue
		}
		var unaddressable *MegaRAIDUnaddressableError
		if !errors.As(err, &unaddressable) {
			t.Errorf("device %d produced %T, want *MegaRAIDUnaddressableError", deviceID, err)
			continue
		}
		if unaddressable.DeviceID != deviceID {
			t.Errorf("error names device %d, want %d", unaddressable.DeviceID, deviceID)
		}
	}

	// The boundary is inclusive: 255 fits in the field and must still work.
	for _, deviceID := range []uint16{0, 9, 64, 255} {
		pkt, err := buildPassthroughPacket(0, deviceID, cdb, inquiryLen, "INQUIRY")
		if err != nil {
			t.Errorf("device %d was refused: %v", deviceID, err)
			continue
		}
		if got := uint16(pkt[iocFrame+pthruTargetID]); got != deviceID {
			t.Errorf("device %d was addressed as %d", deviceID, got)
		}
	}
}
