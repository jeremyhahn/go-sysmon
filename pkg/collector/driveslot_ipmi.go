package collector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// IPMI network functions, commands and codes the drive-slot reader uses
// (IPMI v2.0, sections 33, 35 and 42).
const (
	ipmiNetFnSensor  = 0x04
	ipmiNetFnStorage = 0x0A

	ipmiCmdGetSensorReading = 0x2D
	ipmiCmdReserveSDR       = 0x22
	ipmiCmdGetSDR           = 0x23

	// ipmiCCReservationCancelled means the SDR repository changed under the
	// walk; a fresh reservation reads it again.
	ipmiCCReservationCancelled = 0xC5

	// ipmiBMCAddress is the BMC's own IPMB address. A sensor another
	// controller owns needs a bridged request, which this reader does not
	// send.
	ipmiBMCAddress = 0x20

	sdrHeaderLen   = 5
	sdrChunk       = 16
	sdrLastRecord  = 0xFFFF
	sdrMaxRecords  = 1024
	sdrFullSensor  = 0x01
	sdrCompact     = 0x02
	sdrMaxOffset   = 0xFF
	sdrASCII8      = 0x03
	sdrIDLenMask   = 0x1F
	sdrShareMask   = 0x0F
	sdrModAlpha    = 0x01
	sdrModOffMask  = 0x7F
	sensorTypeSlot = 0x0D

	// Get Sensor Reading flags: the reading is not valid yet.
	sensorReadingUnavailable = 0x20
	// Get Sensor Reading flags: the BMC is scanning the sensor. When clear,
	// its state bits may be stale.
	sensorScanningEnabled = 0x40

	// Event/reading types of a drive-slot sensor: the sensor-specific
	// offsets of type 0x0D, and the generic present/absent pair some BMCs
	// use instead.
	readingTypeSensorSpecific = 0x6F
	readingTypePresence       = 0x08
)

// ipmiCommand is a network function and command pair.
type ipmiCommand struct {
	netfn, cmd uint8
}

// ipmiReadCommands is every command the client will put on the wire, by
// name. None of them changes the BMC: Reserve SDR Repository only hands out a
// token that lets reads detect a concurrent change.
var ipmiReadCommands = map[ipmiCommand]string{
	{ipmiNetFnStorage, ipmiCmdReserveSDR}:      "Reserve SDR Repository",
	{ipmiNetFnStorage, ipmiCmdGetSDR}:          "Get SDR",
	{ipmiNetFnSensor, ipmiCmdGetSensorReading}: "Get Sensor Reading",
}

// sdrIDOffsets is where each sensor record type keeps its ID string
// type/length byte.
var sdrIDOffsets = map[uint8]int{
	sdrFullSensor: 47,
	sdrCompact:    31,
}

// sdrShareOffsets is where each record type keeps its sensor record sharing
// bytes. Only compact records share.
var sdrShareOffsets = map[uint8]int{
	sdrCompact: 23,
}

// slotStateNames names the sensor-specific offsets of sensor type 0x0D.
var slotStateNames = []string{
	"Drive Presence",
	"Drive Fault",
	"Predictive Failure",
	"Hot Spare",
	"Consistency Check In Progress",
	"In Critical Array",
	"In Failed Array",
	"Rebuild/Remap In Progress",
	"Rebuild/Remap Aborted",
}

// Offsets of sensor type 0x0D.
const (
	slotPresence = 1 << iota
	slotFault
	slotPredictive
	slotHotSpare
	slotConsistencyCheck
	slotCriticalArray
	slotFailedArray
	slotRebuild
	slotRebuildAborted
)

// slotDecoders turns a sensor's asserted states into a slot, by event/reading
// type.
var slotDecoders = map[uint8]func(s *types.DriveSlot, states uint16){
	readingTypeSensorSpecific: decodeSlotStates,
	readingTypePresence:       decodePresenceStates,
}

// ipmiUnreadable are the completion codes that mean this one sensor has no
// reading now, rather than that the conversation failed.
var ipmiUnreadable = map[uint8]bool{
	0xCB: true, // requested sensor, data or record not present
	0xCD: true, // cannot execute command for this sensor
	0xD5: true, // not supported in the present state
}

// ipmiTransport sends one request to the BMC and returns its response,
// completion code first.
type ipmiTransport interface {
	exchange(ctx context.Context, lun, netfn, cmd uint8, data []byte) ([]byte, error)
	Close() error
}

// ipmiClient reads the drive-slot sensors over a transport. Every request
// goes through do, which refuses any command not on the read list.
type ipmiClient struct {
	t ipmiTransport
}

// sdrSensor is the part of a sensor record the reader needs.
type sdrSensor struct {
	name        string
	number      uint8
	lun         uint8
	readingType uint8
}

// driveSlots lists the BMC's drive-slot sensors and reads each.
func (c *ipmiClient) driveSlots(ctx context.Context) ([]types.DriveSlot, error) {
	sensors, err := c.slotSensors(ctx)
	if err != nil {
		return nil, err
	}
	if len(sensors) == 0 {
		return nil, &NoDriveSlotsError{Source: types.DriveSlotSourceIPMI}
	}
	slots := make([]types.DriveSlot, 0, len(sensors))
	for _, s := range sensors {
		slot, err := c.reading(ctx, s)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

// do sends one command from the read list and returns the response data
// after a successful completion code.
func (c *ipmiClient) do(ctx context.Context, lun, netfn, cmd uint8, data []byte) ([]byte, error) {
	name, ok := ipmiReadCommands[ipmiCommand{netfn, cmd}]
	if !ok {
		return nil, &IPMICommandRefusedError{NetFn: netfn, Cmd: cmd}
	}
	resp, err := c.t.exchange(ctx, lun, netfn, cmd, data)
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 {
		return nil, &IPMIResponseError{Command: name, Reason: "has no completion code"}
	}
	if resp[0] != 0 {
		return nil, &IPMICompletionError{Command: name, Code: resp[0]}
	}
	return resp[1:], nil
}

// slotSensors walks the SDR repository for the drive-slot sensors the BMC
// owns, each with a unique name.
func (c *ipmiClient) slotSensors(ctx context.Context) ([]sdrSensor, error) {
	resv, err := c.reserve(ctx)
	if err != nil {
		return nil, err
	}
	var sensors []sdrSensor
	names := map[string]int{}
	id := uint16(0)
	for n := 0; id != sdrLastRecord; n++ {
		if n == sdrMaxRecords {
			return nil, &SDRWalkLimitError{Limit: sdrMaxRecords}
		}
		rec, next, err := c.record(ctx, &resv, id)
		if err != nil {
			return nil, err
		}
		for _, s := range parseSlotSensors(rec) {
			names[s.name]++
			if k := names[s.name]; k > 1 {
				s.name += " #" + strconv.Itoa(k)
			}
			sensors = append(sensors, s)
		}
		id = next
	}
	return sensors, nil
}

// reserve takes an SDR repository reservation.
func (c *ipmiClient) reserve(ctx context.Context) (uint16, error) {
	b, err := c.do(ctx, 0, ipmiNetFnStorage, ipmiCmdReserveSDR, nil)
	if err != nil {
		return 0, err
	}
	if len(b) < 2 {
		return 0, &IPMIResponseError{Command: "Reserve SDR Repository", Reason: "has no reservation ID"}
	}
	return uint16(b[0]) | uint16(b[1])<<8, nil
}

// record reads one SDR record, taking a new reservation once if the
// repository changed during the read.
func (c *ipmiClient) record(ctx context.Context, resv *uint16, id uint16) ([]byte, uint16, error) {
	rec, next, err := c.readRecord(ctx, *resv, id)
	var ce *IPMICompletionError
	if !errors.As(err, &ce) || ce.Code != ipmiCCReservationCancelled {
		return rec, next, err
	}
	if *resv, err = c.reserve(ctx); err != nil {
		return nil, 0, err
	}
	return c.readRecord(ctx, *resv, id)
}

// readRecord reads a record's header, then its body in chunks small enough
// for every BMC's message buffer. ID 0 asks for the first record; the body
// is read by the ID the header gives.
func (c *ipmiClient) readRecord(ctx context.Context, resv, id uint16) ([]byte, uint16, error) {
	hdr, next, err := c.getSDR(ctx, resv, id, 0, sdrHeaderLen)
	if err != nil {
		return nil, 0, err
	}
	if len(hdr) < sdrHeaderLen {
		return nil, 0, &SDRTruncatedError{RecordID: id, Want: sdrHeaderLen, Got: len(hdr)}
	}
	id = uint16(hdr[0]) | uint16(hdr[1])<<8
	total := sdrHeaderLen + int(hdr[4])
	if total-1 > sdrMaxOffset {
		return nil, 0, &IPMIResponseError{Command: "Get SDR", Reason: "describes a record longer than Get SDR can address"}
	}
	rec := make([]byte, sdrHeaderLen, total)
	copy(rec, hdr)
	for len(rec) < total {
		n := min(sdrChunk, total-len(rec))
		b, _, err := c.getSDR(ctx, resv, id, len(rec), n)
		if err != nil {
			return nil, 0, err
		}
		if len(b) < n {
			return nil, 0, &SDRTruncatedError{RecordID: id, Want: total, Got: len(rec) + len(b)}
		}
		rec = append(rec, b[:n]...)
	}
	return rec, next, nil
}

// getSDR reads count bytes of record id from offset, and returns them with
// the ID of the record that follows.
func (c *ipmiClient) getSDR(ctx context.Context, resv, id uint16, offset, count int) ([]byte, uint16, error) {
	req := []byte{byte(resv), byte(resv >> 8), byte(id), byte(id >> 8), byte(offset), byte(count)}
	b, err := c.do(ctx, 0, ipmiNetFnStorage, ipmiCmdGetSDR, req)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 2 {
		return nil, 0, &SDRTruncatedError{RecordID: id, Want: offset + count, Got: offset}
	}
	return b[2:], uint16(b[0]) | uint16(b[1])<<8, nil
}

// parseSlotSensors returns the drive-slot sensors a record describes: none
// for any other record, one for a full record, and one per shared sensor for
// a compact record.
func parseSlotSensors(rec []byte) []sdrSensor {
	idOff, ok := sdrIDOffsets[rec[3]]
	if !ok || len(rec) <= idOff {
		return nil
	}
	owner, sensorType, readingType := rec[5], rec[12], rec[13]
	if owner != ipmiBMCAddress || sensorType != sensorTypeSlot {
		return nil
	}
	if _, ok := slotDecoders[readingType]; !ok {
		return nil
	}

	base := sdrSensor{number: rec[7], lun: rec[6] & 0x03, readingType: readingType}
	name := sdrIDString(rec, idOff)

	count, modType, modOff := 1, uint8(0), 0
	if off, ok := sdrShareOffsets[rec[3]]; ok {
		if n := int(rec[off] & sdrShareMask); n > 1 {
			count = n
		}
		modType = (rec[off] >> 4) & 0x03
		modOff = int(rec[off+1] & sdrModOffMask)
	}

	sensors := make([]sdrSensor, 0, count)
	for i := range count {
		s := base
		s.number += uint8(i)
		s.name = name
		if count > 1 {
			s.name += shareModifier(modType, modOff+i)
		}
		s.name = strings.TrimSpace(s.name)
		if s.name == "" {
			s.name = fmt.Sprintf("sensor 0x%02X", s.number)
		}
		sensors = append(sensors, s)
	}
	return sensors
}

// sdrIDString decodes a record's ID string when it is 8-bit ASCII, the
// encoding every BMC in the fleet uses. Any other encoding returns "", and
// the caller names the sensor by number.
func sdrIDString(rec []byte, off int) string {
	tl := rec[off]
	if tl>>6 != sdrASCII8 {
		return ""
	}
	end := min(off+1+int(tl&sdrIDLenMask), len(rec))
	return strings.TrimRight(string(rec[off+1:end]), "\x00")
}

// shareModifier is the suffix a shared sensor's instance adds to the record's
// ID string: a number, or a letter.
func shareModifier(modType uint8, n int) string {
	if modType == sdrModAlpha {
		return string(rune('A' + n%26))
	}
	return strconv.Itoa(n)
}

// reading takes one sensor's current state.
func (c *ipmiClient) reading(ctx context.Context, s sdrSensor) (types.DriveSlot, error) {
	slot := types.DriveSlot{ID: s.name, Slot: s.name}
	b, err := c.do(ctx, s.lun, ipmiNetFnSensor, ipmiCmdGetSensorReading, []byte{s.number})
	var ce *IPMICompletionError
	if errors.As(err, &ce) && ipmiUnreadable[ce.Code] {
		slot.State = "reading unavailable"
		return slot, nil
	}
	if err != nil {
		return slot, err
	}
	if len(b) < 2 {
		return slot, &IPMIResponseError{Command: "Get Sensor Reading", Reason: "has no reading flags"}
	}
	if b[1]&sensorReadingUnavailable != 0 {
		slot.State = "reading unavailable"
		return slot, nil
	}
	if b[1]&sensorScanningEnabled == 0 {
		slot.State = "sensor scanning disabled"
		return slot, nil
	}
	if len(b) < 3 {
		return slot, &IPMIResponseError{Command: "Get Sensor Reading", Reason: "has no state bits"}
	}
	states := uint16(b[2])
	if len(b) > 3 {
		states |= uint16(b[3]&0x7F) << 8
	}
	slotDecoders[s.readingType](&slot, states)
	return slot, nil
}

// decodeSlotStates maps the sensor-specific offsets of sensor type 0x0D.
func decodeSlotStates(s *types.DriveSlot, states uint16) {
	s.Present = states&slotPresence != 0
	s.Fault = states&(slotFault|slotFailedArray) != 0
	s.FailurePredicted = states&slotPredictive != 0
	s.Rebuilding = states&slotRebuild != 0

	var asserted []string
	for i, name := range slotStateNames {
		if states&(1<<i) != 0 {
			asserted = append(asserted, name)
		}
	}
	s.State = strings.Join(asserted, ", ")

	switch {
	case !s.Present:
		s.Health = types.DriveSlotHealthUnknown
	case s.Fault:
		s.Health = types.DriveSlotHealthCritical
	case states&(slotPredictive|slotCriticalArray|slotRebuildAborted) != 0:
		s.Health = types.DriveSlotHealthWarning
	default:
		s.Health = types.DriveSlotHealthOK
	}
}

// decodePresenceStates maps the generic device present/absent pair: offset 0
// is absent, offset 1 is present. It says nothing about health.
func decodePresenceStates(s *types.DriveSlot, states uint16) {
	s.Present = states&0x02 != 0
	if s.Present {
		s.State = "Device Present"
	} else {
		s.State = "Device Absent"
	}
}

// IPMICommandRefusedError reports a command the client would not send:
// anything that is not one of its three reads.
type IPMICommandRefusedError struct {
	NetFn, Cmd uint8
}

func (e *IPMICommandRefusedError) Error() string {
	return fmt.Sprintf("refused to send netfn 0x%02X command 0x%02X: not a read the drive-slot collector uses", e.NetFn, e.Cmd)
}

// IPMICompletionError reports a command the BMC answered with a nonzero
// completion code.
type IPMICompletionError struct {
	Command string
	Code    uint8
}

func (e *IPMICompletionError) Error() string {
	return fmt.Sprintf("%s failed with completion code 0x%02X", e.Command, e.Code)
}

// IPMIResponseError reports a response too short for its command.
type IPMIResponseError struct {
	Command string
	Reason  string
}

func (e *IPMIResponseError) Error() string {
	return fmt.Sprintf("%s response %s", e.Command, e.Reason)
}

// SDRTruncatedError reports an SDR record shorter than its header claims.
type SDRTruncatedError struct {
	RecordID  uint16
	Want, Got int
}

func (e *SDRTruncatedError) Error() string {
	return fmt.Sprintf("SDR record 0x%04X is truncated: %d of %d bytes", e.RecordID, e.Got, e.Want)
}

// SDRWalkLimitError reports a repository whose record chain never ends.
type SDRWalkLimitError struct {
	Limit int
}

func (e *SDRWalkLimitError) Error() string {
	return fmt.Sprintf("SDR walk stopped after %d records", e.Limit)
}

// IPMITimeoutError reports a BMC that did not answer in time.
type IPMITimeoutError struct {
	NetFn, Cmd uint8
}

func (e *IPMITimeoutError) Error() string {
	return fmt.Sprintf("no response to netfn 0x%02X command 0x%02X", e.NetFn, e.Cmd)
}

// IPMIIoctlError reports the kernel refusing an IPMI device request.
type IPMIIoctlError struct {
	Op  string
	Err error
}

func (e *IPMIIoctlError) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }

func (e *IPMIIoctlError) Unwrap() error { return e.Err }

// IPMIUnsupportedError reports a build target with no in-band IPMI reader.
type IPMIUnsupportedError struct{}

func (e *IPMIUnsupportedError) Error() string {
	return "in-band IPMI is read on linux/amd64 and linux/arm64 only"
}
