package collector

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// fakeIPMI is a BMC's SDR repository and drive-slot sensors, answering the
// way ipmi_devintf delivers responses: completion code first.
type fakeIPMI struct {
	records  [][]byte
	readings map[uint8][]byte

	resv     uint16
	cancels  int                      // answer this many body reads with "reservation cancelled"
	loop     bool                     // point every record at itself, so the chain never ends
	override func(data []byte) []byte // answer every command with this
	exchErr  error                    // fail every exchange
	closeErr error

	sent   []ipmiCommand
	closed bool
}

var fakeIPMIHandlers = map[ipmiCommand]func(f *fakeIPMI, data []byte) []byte{
	{ipmiNetFnStorage, ipmiCmdReserveSDR}:      (*fakeIPMI).reserve,
	{ipmiNetFnStorage, ipmiCmdGetSDR}:          (*fakeIPMI).getSDR,
	{ipmiNetFnSensor, ipmiCmdGetSensorReading}: (*fakeIPMI).reading,
}

func (f *fakeIPMI) exchange(_ context.Context, _, netfn, cmd uint8, data []byte) ([]byte, error) {
	f.sent = append(f.sent, ipmiCommand{netfn, cmd})
	if f.exchErr != nil {
		return nil, f.exchErr
	}
	if f.override != nil {
		return f.override(data), nil
	}
	h, ok := fakeIPMIHandlers[ipmiCommand{netfn, cmd}]
	if !ok {
		return []byte{0xC1}, nil // invalid command
	}
	return h(f, data), nil
}

func (f *fakeIPMI) Close() error {
	f.closed = true
	return f.closeErr
}

func (f *fakeIPMI) reserve([]byte) []byte {
	f.resv++
	return []byte{0, byte(f.resv), byte(f.resv >> 8)}
}

func (f *fakeIPMI) getSDR(data []byte) []byte {
	if len(data) != 6 {
		return []byte{0xC7} // request data length invalid
	}
	resv := uint16(data[0]) | uint16(data[1])<<8
	id := uint16(data[2]) | uint16(data[3])<<8
	off, count := int(data[4]), int(data[5])
	if off > 0 && f.cancels > 0 {
		f.cancels--
		return []byte{ipmiCCReservationCancelled}
	}
	if off > 0 && resv != f.resv {
		return []byte{ipmiCCReservationCancelled}
	}
	for i, rec := range f.records {
		// ID 0 is the first record, whatever its own ID.
		if uint16(rec[0])|uint16(rec[1])<<8 != id && (id != 0 || i != 0) {
			continue
		}
		next := uint16(sdrLastRecord)
		if f.loop {
			next = id
		} else if i+1 < len(f.records) {
			next = uint16(f.records[i+1][0]) | uint16(f.records[i+1][1])<<8
		}
		out := []byte{0, byte(next), byte(next >> 8)}
		return append(out, rec[min(off, len(rec)):min(off+count, len(rec))]...)
	}
	return []byte{0xCB} // record not present
}

func (f *fakeIPMI) reading(data []byte) []byte {
	if r, ok := f.readings[data[0]]; ok {
		return r
	}
	return []byte{0xCB} // sensor not present
}

// loadIPMIFixture reads a testdata/driveslot fixture of "sdr" and "reading"
// lines of hex bytes.
func loadIPMIFixture(t *testing.T, name string) *fakeIPMI {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "driveslot", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})

	f := &fakeIPMI{readings: map[uint8][]byte{}}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		b, err := hex.DecodeString(strings.Join(fields[1:], ""))
		if err != nil {
			t.Fatalf("%s: %q: %v", name, sc.Text(), err)
		}
		switch fields[0] {
		case "sdr":
			f.records = append(f.records, b)
		case "reading":
			f.readings[b[0]] = b[1:]
		default:
			t.Fatalf("%s: unknown line %q", name, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return f
}

// slotsByID indexes slots for assertions.
func slotsByID(slots []types.DriveSlot) map[string]types.DriveSlot {
	m := make(map[string]types.DriveSlot, len(slots))
	for _, s := range slots {
		m[s.ID] = s
	}
	return m
}

func TestIPMIDriveSlots_ReadsEveryBayAndTheFault(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	slots, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	got := slotsByID(slots)
	if len(slots) != 4 || len(got) != 4 {
		t.Fatalf("driveSlots() = %d slots %v, want Drive 0, Drive 1, Drive 2 and Bay 3", len(slots), slots)
	}

	want := map[string]types.DriveSlot{
		"Drive 0": {ID: "Drive 0", Slot: "Drive 0", Present: true, Health: types.DriveSlotHealthOK, State: "Drive Presence"},
		"Drive 1": {ID: "Drive 1", Slot: "Drive 1", Present: true, Health: types.DriveSlotHealthOK, Rebuilding: true,
			State: "Drive Presence, Rebuild/Remap In Progress"},
		"Drive 2": {ID: "Drive 2", Slot: "Drive 2", Health: types.DriveSlotHealthUnknown},
		"Bay 3": {ID: "Bay 3", Slot: "Bay 3", Present: true, Health: types.DriveSlotHealthCritical, Fault: true,
			State: "Drive Presence, Drive Fault"},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("slot %q = %+v, want %+v", id, got[id], w)
		}
	}
}

func TestIPMIDriveSlots_PutsOnlyReadsOnTheWire(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	if _, err := (&ipmiClient{t: f}).driveSlots(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.sent {
		if _, ok := ipmiReadCommands[c]; !ok {
			t.Errorf("sent netfn 0x%02X command 0x%02X, which is not on the read list", c.netfn, c.cmd)
		}
	}
}

func TestIPMIClient_RefusesACommandOffTheReadList(t *testing.T) {
	f := &fakeIPMI{}
	// Clear SEL (storage 0x47) and Chassis Control (0x00 0x02) both change the BMC.
	for _, c := range []ipmiCommand{{ipmiNetFnStorage, 0x47}, {0x00, 0x02}} {
		_, err := (&ipmiClient{t: f}).do(context.Background(), 0, c.netfn, c.cmd, nil)
		var refused *IPMICommandRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("do(0x%02X, 0x%02X) error = %v, want IPMICommandRefusedError", c.netfn, c.cmd, err)
		}
	}
	if len(f.sent) != 0 {
		t.Errorf("the transport saw %v; a refused command must never reach it", f.sent)
	}
}

func TestIPMIDriveSlots_TruncatedRecordIsAnError(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-truncated.txt")
	_, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	var trunc *SDRTruncatedError
	if !errors.As(err, &trunc) {
		t.Fatalf("driveSlots() error = %v, want SDRTruncatedError", err)
	}
	if trunc.RecordID != 2 || trunc.Want != 39 || trunc.Got != 20 {
		t.Errorf("SDRTruncatedError = %+v, want record 2, 20 of 39 bytes", trunc)
	}
	if !strings.Contains(err.Error(), "0x0002") {
		t.Errorf("error %q does not name the record", err)
	}
}

func TestIPMIDriveSlots_RereservesOnceWhenTheRepositoryChanges(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	f.cancels = 1
	slots, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	if len(slots) != 4 {
		t.Errorf("driveSlots() = %d slots, want 4", len(slots))
	}
	if f.resv != 2 {
		t.Errorf("reservations taken = %d, want 2", f.resv)
	}
}

func TestIPMIDriveSlots_RepeatedCancellationIsAnError(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	f.cancels = 2
	_, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	var ce *IPMICompletionError
	if !errors.As(err, &ce) || ce.Code != ipmiCCReservationCancelled {
		t.Errorf("driveSlots() error = %v, want the second cancellation", err)
	}
}

func TestIPMIDriveSlots_UnreadableSensorIsReportedNotFatal(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	delete(f.readings, 0x32)
	f.readings[0x30] = []byte{0x00, 0x00, 0xE0} // reading unavailable
	slots, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	got := slotsByID(slots)
	for _, id := range []string{"Drive 0", "Drive 2"} {
		if got[id].State != "reading unavailable" || got[id].Health != types.DriveSlotHealthUnknown {
			t.Errorf("slot %q = %+v, want an unknown slot with reading unavailable", id, got[id])
		}
	}
}

func TestIPMIDriveSlots_FailedReadingIsAnError(t *testing.T) {
	cases := map[string]struct {
		reading []byte
		want    any
	}{
		"completion code": {[]byte{0xC3}, new(*IPMICompletionError)},
		"no completion":   {[]byte{}, new(*IPMIResponseError)},
		"no flags":        {[]byte{0x00, 0x00}, new(*IPMIResponseError)},
		"no state bits":   {[]byte{0x00, 0x00, 0xC0}, new(*IPMIResponseError)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
			f.readings[0x40] = tc.reading
			_, err := (&ipmiClient{t: f}).driveSlots(context.Background())
			if err == nil || !errors.As(err, tc.want) {
				t.Errorf("driveSlots() error = %v, want %T", err, tc.want)
			}
		})
	}
}

func TestIPMIDriveSlots_NoSlotSensors(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	f.records = f.records[:1] // the temperature sensor alone
	_, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	var none *NoDriveSlotsError
	if !errors.As(err, &none) || none.Source != types.DriveSlotSourceIPMI {
		t.Fatalf("driveSlots() error = %v, want NoDriveSlotsError from IPMI", err)
	}
	if !strings.Contains(err.Error(), "0x0D") {
		t.Errorf("error %q does not name the sensor type", err)
	}
}

func TestIPMIDriveSlots_EndlessRecordChainStops(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	f.loop = true
	_, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	var limit *SDRWalkLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("driveSlots() error = %v, want SDRWalkLimitError", err)
	}
}

func TestIPMIDriveSlots_TransportFailureIsReturned(t *testing.T) {
	cause := &IPMITimeoutError{NetFn: ipmiNetFnStorage, Cmd: ipmiCmdReserveSDR}
	_, err := (&ipmiClient{t: &fakeIPMI{exchErr: cause}}).driveSlots(context.Background())
	if !errors.Is(err, cause) {
		t.Errorf("driveSlots() error = %v, want the transport's %v", err, cause)
	}
}

func TestIPMIClient_ShortResponses(t *testing.T) {
	cases := map[string]struct {
		handler func(data []byte) []byte
		call    func(c *ipmiClient) error
		want    any
	}{
		"reservation without an ID": {
			handler: func([]byte) []byte { return []byte{0x00, 0x01} },
			call: func(c *ipmiClient) error {
				_, err := c.reserve(context.Background())
				return err
			},
			want: new(*IPMIResponseError),
		},
		"Get SDR without a next ID": {
			handler: func([]byte) []byte { return []byte{0x00, 0xFF} },
			call: func(c *ipmiClient) error {
				_, _, err := c.readRecord(context.Background(), 1, 2)
				return err
			},
			want: new(*SDRTruncatedError),
		},
		"header shorter than five bytes": {
			handler: func([]byte) []byte { return []byte{0x00, 0xFF, 0xFF, 0x02, 0x00, 0x51} },
			call: func(c *ipmiClient) error {
				_, _, err := c.readRecord(context.Background(), 1, 2)
				return err
			},
			want: new(*SDRTruncatedError),
		},
		"record longer than Get SDR addresses": {
			handler: func([]byte) []byte { return []byte{0x00, 0xFF, 0xFF, 0x02, 0x00, 0x51, 0x02, 0xFF} },
			call: func(c *ipmiClient) error {
				_, _, err := c.readRecord(context.Background(), 1, 2)
				return err
			},
			want: new(*IPMIResponseError),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tc.call(&ipmiClient{t: &fakeIPMI{override: tc.handler}}); err == nil || !errors.As(err, tc.want) {
				t.Errorf("error = %v, want %T", err, tc.want)
			}
		})
	}
}

// slotRecord is the compact "Drive 0" record of the fixture, to vary.
func slotRecord(t *testing.T) []byte {
	t.Helper()
	return loadIPMIFixture(t, "ipmi-dellemc02.txt").records[1]
}

func TestParseSlotSensors_SkipsWhatItCannotRead(t *testing.T) {
	cases := map[string]func(rec []byte) []byte{
		"another controller owns it": func(rec []byte) []byte { rec[5] = 0x82; return rec },
		"not a drive slot":           func(rec []byte) []byte { rec[12] = 0x02; return rec },
		"unknown reading type":       func(rec []byte) []byte { rec[13] = 0x03; return rec },
		"too short for its ID":       func(rec []byte) []byte { return rec[:20] },
		"not a sensor record":        func(rec []byte) []byte { rec[3] = 0x12; return rec },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseSlotSensors(mutate(slotRecord(t))); len(got) != 0 {
				t.Errorf("parseSlotSensors() = %+v, want none", got)
			}
		})
	}
}

func TestParseSlotSensors_NamesAndSharing(t *testing.T) {
	rec := slotRecord(t)
	rec[31] = 0x07 // unicode ID string, which the reader does not decode
	got := parseSlotSensors(rec)
	if len(got) != 1 || got[0].name != "sensor 0x30" {
		t.Errorf("non-ASCII ID: parseSlotSensors() = %+v, want one sensor named by number", got)
	}

	rec = slotRecord(t)
	rec[23] = 0x13 // three sensors, alphabetic modifier
	rec[24] = 0x00
	got = parseSlotSensors(rec)
	var names []string
	for _, s := range got {
		names = append(names, s.name)
	}
	if strings.Join(names, ",") != "Drive 0A,Drive 0B,Drive 0C" || got[2].number != 0x32 {
		t.Errorf("shared record: parseSlotSensors() = %+v, want Drive 0A..0C on sensors 0x30..0x32", got)
	}
}

func TestIPMIDriveSlots_DuplicateNamesAreMadeUnique(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	dup := append([]byte(nil), f.records[1]...)
	dup[0], dup[7] = 0x06, 0x41
	f.records = append(f.records, dup)
	f.readings[0x41] = []byte{0x00, 0x00, 0xC0, 0x01, 0x00}
	slots, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := slotsByID(slots)["Drive 0 #2"]; !ok {
		t.Errorf("driveSlots() = %+v, want the second Drive 0 as Drive 0 #2", slots)
	}
}

func TestDecodeSlotStates_Health(t *testing.T) {
	cases := map[string]struct {
		states uint16
		want   types.DriveSlot
	}{
		"empty bay": {0, types.DriveSlot{}},
		"healthy hot spare": {slotPresence | slotHotSpare, types.DriveSlot{Present: true, Health: types.DriveSlotHealthOK,
			State: "Drive Presence, Hot Spare"}},
		"predictive failure": {slotPresence | slotPredictive, types.DriveSlot{Present: true, FailurePredicted: true,
			Health: types.DriveSlotHealthWarning, State: "Drive Presence, Predictive Failure"}},
		"in a critical array": {slotPresence | slotCriticalArray, types.DriveSlot{Present: true,
			Health: types.DriveSlotHealthWarning, State: "Drive Presence, In Critical Array"}},
		"in a failed array": {slotPresence | slotFailedArray, types.DriveSlot{Present: true, Fault: true,
			Health: types.DriveSlotHealthCritical, State: "Drive Presence, In Failed Array"}},
		"rebuild aborted": {slotPresence | slotRebuildAborted, types.DriveSlot{Present: true,
			Health: types.DriveSlotHealthWarning, State: "Drive Presence, Rebuild/Remap Aborted"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got types.DriveSlot
			decodeSlotStates(&got, tc.states)
			if got != tc.want {
				t.Errorf("decodeSlotStates(%#x) = %+v, want %+v", tc.states, got, tc.want)
			}
		})
	}
}

func TestIPMIDriveSlots_PresenceOnlySensor(t *testing.T) {
	f := loadIPMIFixture(t, "ipmi-dellemc02.txt")
	f.records[1][13] = readingTypePresence
	f.readings[0x30] = []byte{0x00, 0x00, 0xC0, 0x02}
	slots, err := (&ipmiClient{t: f}).driveSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := slotsByID(slots)["Drive 0"]
	if !got.Present || got.State != "Device Present" || got.Health != types.DriveSlotHealthUnknown {
		t.Errorf("presence sensor = %+v, want present with no health claim", got)
	}

	var absent types.DriveSlot
	decodePresenceStates(&absent, 0x01)
	if absent.Present || absent.State != "Device Absent" {
		t.Errorf("decodePresenceStates(absent) = %+v", absent)
	}
}

func TestIPMIErrors_SayWhatFailed(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"refused":     {&IPMICommandRefusedError{NetFn: 0x0A, Cmd: 0x47}, "netfn 0x0A command 0x47"},
		"completion":  {&IPMICompletionError{Command: "Get SDR", Code: 0xC5}, "Get SDR failed with completion code 0xC5"},
		"response":    {&IPMIResponseError{Command: "Get SDR", Reason: "is short"}, "Get SDR response is short"},
		"walk limit":  {&SDRWalkLimitError{Limit: 3}, "after 3 records"},
		"timeout":     {&IPMITimeoutError{NetFn: 0x04, Cmd: 0x2D}, "no response to netfn 0x04 command 0x2D"},
		"ioctl":       {&IPMIIoctlError{Op: "poll", Err: os.ErrClosed}, "poll: file already closed"},
		"unsupported": {&IPMIUnsupportedError{}, "linux/amd64 and linux/arm64 only"},
	}
	for name, tc := range cases {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: %q does not contain %q", name, tc.err, tc.want)
		}
	}
	if !errors.Is(&IPMIIoctlError{Err: os.ErrClosed}, os.ErrClosed) {
		t.Error("IPMIIoctlError does not unwrap to its cause")
	}
}
