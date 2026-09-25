package collector

import (
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// logPage assembles a SCSI log page around one parameter, so a decoder can be
// exercised without a drive.
func logPage(page uint8, param uint16, value []byte) []byte {
	body := []byte{byte(param >> 8), byte(param), 0x00, byte(len(value))}
	body = append(body, value...)
	return append([]byte{page, 0x00, byte(len(body) >> 8), byte(len(body))}, body...)
}

// ---- CDBs -----------------------------------------------------------------

func TestBuildInquiryCDB_SetsEVPDOnlyForAPage(t *testing.T) {
	std := buildInquiryCDB(0, 36)
	if std[0] != scsiInquiry || std[1] != 0 || std[2] != 0 || std[4] != 36 {
		t.Errorf("standard INQUIRY cdb = % x", std)
	}

	// EVPD must be set, or the drive answers with the standard data and the
	// decoder reads a device type where it expects a page code.
	vpd := buildInquiryCDB(vpdUnitSerialNumber, 64)
	if vpd[1] != 1 {
		t.Errorf("VPD INQUIRY cdb = % x; EVPD must be set when a page is asked for", vpd)
	}
	if vpd[2] != vpdUnitSerialNumber {
		t.Errorf("VPD INQUIRY page = 0x%02x, want 0x%02x", vpd[2], vpdUnitSerialNumber)
	}
}

func TestBuildLogSenseCDB_AsksForCurrentValues(t *testing.T) {
	cdb := buildLogSenseCDB(logPageInformationalExceptions, 64)
	if cdb[0] != scsiLogSense {
		t.Errorf("opcode = 0x%02x, want 0x%02x", cdb[0], scsiLogSense)
	}
	// Byte 2 carries both the page code and the page control. Without PC=01
	// the drive returns threshold values -- the limits rather than the
	// readings -- which decode perfectly and mean something else entirely.
	if cdb[2]&0x3F != logPageInformationalExceptions {
		t.Errorf("page = 0x%02x, want 0x%02x", cdb[2]&0x3F, logPageInformationalExceptions)
	}
	if cdb[2]&0xC0 != 0x40 {
		t.Errorf("page control = 0x%02x, want 0x40 (current cumulative values)", cdb[2]&0xC0)
	}
	if int(cdb[7]) != 64 {
		t.Errorf("allocation length = %d, want 64", cdb[7])
	}
}

// ---- INQUIRY --------------------------------------------------------------

func TestParseInquiry_ReadsIdentityAndDeviceType(t *testing.T) {
	data := make([]byte, inquiryLen)
	data[0] = 0x0D // an enclosure processor, not a drive
	copy(data[inquiryVendor:], "DP      ")
	copy(data[inquiryProduct:], "BP14G+EXP       ")

	vendor, product, devType, err := parseInquiry(data)
	if err != nil {
		t.Fatalf("parseInquiry() error = %v", err)
	}
	if vendor != "DP" || product != "BP14G+EXP" {
		t.Errorf("identity = %q/%q, want DP/BP14G+EXP", vendor, product)
	}
	// The device type is what keeps a backplane's enclosure processor from
	// being counted as a member of an array. ci-node-1 has one, at device 64.
	if devType != 0x0D {
		t.Errorf("device type = 0x%02x, want 0x0D", devType)
	}
	if devType == scsiDeviceTypeDirectAccess {
		t.Error("an enclosure processor was reported as a block device")
	}
}

func TestParseInquiry_RefusesAShortResponse(t *testing.T) {
	if _, _, _, err := parseInquiry(make([]byte, inquiryLen-1)); err == nil {
		t.Error("parseInquiry() accepted a response too short to hold the fields it read")
	}
}

func TestParseUnitSerial_ReadsTheDeclaredLength(t *testing.T) {
	serial := "L0J4VK8J"
	page := append([]byte{0x00, vpdUnitSerialNumber, 0x00, byte(len(serial))}, []byte(serial)...)
	// Trailing bytes past the declared length must not be swept into the
	// serial: a drive that pads its page would otherwise get an identity that
	// changes with the allocation length we asked for.
	page = append(page, 0x00, 0x00, 0x00)

	got, err := parseUnitSerial(page)
	if err != nil {
		t.Fatalf("parseUnitSerial() error = %v", err)
	}
	if got != serial {
		t.Errorf("parseUnitSerial() = %q, want %q", got, serial)
	}
}

func TestParseUnitSerial_RefusesWhatIsNotThatPage(t *testing.T) {
	tests := []struct {
		name string
		page []byte
	}{
		{"too short for a header", []byte{0x00, 0x80}},
		{"a different VPD page", []byte{0x00, 0x83, 0x00, 0x04, 'a', 'b', 'c', 'd'}},
		{"declares more than it carries", []byte{0x00, 0x80, 0x00, 0x40, 'a'}},
		{"declares nothing", []byte{0x00, 0x80, 0x00, 0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := parseUnitSerial(tc.page); err == nil {
				t.Errorf("parseUnitSerial() = %q, want an error", got)
			}
		})
	}
}

// ---- log pages ------------------------------------------------------------

func TestParseInformationalExceptions_ZeroSenseIsAHealthyDrive(t *testing.T) {
	// What a well drive reports, and what smartctl prints as
	// "SMART Health Status: OK".
	page := logPage(logPageInformationalExceptions, 0x0000, []byte{0x00, 0x00, 33})

	health, err := parseInformationalExceptions(page)
	if err != nil {
		t.Fatalf("parseInformationalExceptions() error = %v", err)
	}
	if health.Failing {
		t.Error("a drive reporting no exception was read as failing")
	}
	if health.TemperatureC != 33 {
		t.Errorf("temperature = %v, want 33", health.TemperatureC)
	}
}

func TestParseInformationalExceptions_AnySenseCodeIsAFailure(t *testing.T) {
	// 0x5D/0x10 is "hardware impending failure". Neither byte alone decides
	// it: a drive setting only the qualifier must still read as failing.
	for _, tc := range []struct{ asc, ascq uint8 }{{0x5D, 0x10}, {0x5D, 0x00}, {0x00, 0x10}} {
		page := logPage(logPageInformationalExceptions, 0x0000, []byte{tc.asc, tc.ascq, 40})
		health, err := parseInformationalExceptions(page)
		if err != nil {
			t.Fatalf("parseInformationalExceptions() error = %v", err)
		}
		if !health.Failing {
			t.Errorf("ASC/ASCQ %02x/%02x was read as healthy", tc.asc, tc.ascq)
		}
	}
}

func TestParseInformationalExceptions_UnavailableTemperatureIsNotAReading(t *testing.T) {
	// 0xFF means the drive has no temperature to give. Recording it as 255 °C
	// would raise every thermal alert the product has at once.
	page := logPage(logPageInformationalExceptions, 0x0000,
		[]byte{0x00, 0x00, temperatureNotAvailable})

	health, err := parseInformationalExceptions(page)
	if err != nil {
		t.Fatalf("parseInformationalExceptions() error = %v", err)
	}
	if health.TemperatureC != 0 {
		t.Errorf("temperature = %v, want 0 for a drive that reported none", health.TemperatureC)
	}
}

func TestParseTemperatureLog_ReadsTheDedicatedPage(t *testing.T) {
	page := logPage(logPageTemperature, 0x0000, []byte{0x00, 34})
	got, err := parseTemperatureLog(page)
	if err != nil {
		t.Fatalf("parseTemperatureLog() error = %v", err)
	}
	if got != 34 {
		t.Errorf("parseTemperatureLog() = %v, want 34", got)
	}

	unavailable := logPage(logPageTemperature, 0x0000, []byte{0x00, temperatureNotAvailable})
	if got, err := parseTemperatureLog(unavailable); err != nil || got != 0 {
		t.Errorf("parseTemperatureLog(unavailable) = (%v, %v), want (0, nil)", got, err)
	}
}

// TestLogPageParameter_WalksTheChainSafely is the important one. Every length
// in a log page comes from the drive, and following them without checking is
// how a decoder reads past the end of a buffer the drive controls.
func TestLogPageParameter_WalksTheChainSafely(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"shorter than its own header", []byte{0x2F, 0x00}},
		{
			name: "a parameter declaring more than remains",
			data: []byte{0x2F, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x40, 0x01, 0x02},
		},
		{"a page that is not the one asked for", logPage(0x0D, 0x0000, []byte{0, 1, 2})},
		{"no such parameter", logPage(0x2F, 0x0007, []byte{0, 0, 0})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := logPageParameter(tc.data, logPageInformationalExceptions, 0x0000); err == nil {
				t.Error("logPageParameter() accepted a page that does not describe itself")
			}
		})
	}
}

// TestLogPageParameter_FindsALaterParameter checks the walk actually walks:
// a decoder that only ever read the first parameter would pass every test
// above and return the wrong reading on a real drive.
func TestLogPageParameter_FindsALaterParameter(t *testing.T) {
	first := []byte{0x00, 0x00, 0x00, 0x02, 0xAA, 0xBB}
	second := []byte{0x00, 0x05, 0x00, 0x03, 0x11, 0x22, 0x33}
	body := append(append([]byte{}, first...), second...)
	page := append([]byte{logPageTemperature, 0x00, byte(len(body) >> 8), byte(len(body))}, body...)

	value, err := logPageParameter(page, logPageTemperature, 0x0005)
	if err != nil {
		t.Fatalf("logPageParameter() error = %v", err)
	}
	if len(value) != 3 || value[0] != 0x11 {
		t.Errorf("logPageParameter() = % x, want the second parameter's value", value)
	}
}

// TestSCSIResponseError_SaysWhichCommand keeps the message useful: a decoder
// error that does not name the command is a line in a log nobody can act on.
func TestSCSIResponseError_SaysWhichCommand(t *testing.T) {
	_, err := parseInformationalExceptions([]byte{0x0D, 0x00, 0x00, 0x00})
	if err == nil {
		t.Fatal("wrong page accepted")
	}
	if !strings.Contains(err.Error(), "0x2F") {
		t.Errorf("error = %q; it must name the command that failed", err)
	}
}

// TestLogPageParameter_AZeroLengthParameterTerminates covers a malformed chain
// that could have stalled the walk. It does not: the cursor advances by the
// parameter header whatever the declared length says, so the page runs out.
// The point is that it ENDS and reports not finding what was asked for, rather
// than returning a value read from a misaligned offset.
func TestLogPageParameter_AZeroLengthParameterTerminates(t *testing.T) {
	page := []byte{
		logPageInformationalExceptions, 0x00, 0x00, 0x08,
		0x00, 0x01, 0x00, 0x00, // parameter 0x0001, length 0
		0x00, 0x02, 0x00, 0x00, // parameter 0x0002, length 0
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := logPageParameter(page, logPageInformationalExceptions, 0x0000); err == nil {
			t.Error("logPageParameter() found a parameter that is not in the page")
		}
	}()
	await.Recv(t, "logPageParameter() to terminate on a page of zero-length parameters", done)
}
