package collector

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// The SCSI commands a drive behind a RAID controller answers.
//
// A SAS drive has no ATA SMART. Its health lives in SCSI log pages, and the one
// that matters is Informational Exceptions (0x2F): a drive that has decided it
// is failing reports an additional sense code there, and a drive that has not
// reports zeroes. That is the same answer `smartctl` prints as
// "SMART Health Status: OK".
//
// These are all pure: a CDB is bytes, a log page is bytes, and neither needs a
// controller to be verified.

// SCSI opcodes used here.
const (
	scsiInquiry  uint8 = 0x12
	scsiLogSense uint8 = 0x4D
)

// SCSI log pages used here.
const (
	logPageInformationalExceptions uint8 = 0x2F
	logPageTemperature             uint8 = 0x0D
)

// VPD pages used here.
const vpdUnitSerialNumber uint8 = 0x80

// Standard INQUIRY response offsets.
const (
	inquiryDeviceType = 0
	inquiryVendor     = 8
	inquiryVendorLen  = 8
	inquiryProduct    = 16
	inquiryProductLen = 16
	inquiryLen        = 36
)

// scsiDeviceTypeDirectAccess is peripheral device type 0x00: an ordinary block
// device. The enclosure processor a backplane presents alongside the drives is
// type 0x0D, and counting it as a drive would credit an array with a member
// that is a circuit board.
const scsiDeviceTypeDirectAccess uint8 = 0x00

// Log page header and parameter offsets (SPC).
const (
	logPageCode       = 0
	logPageLength     = 2 // u16, bytes following the 4-byte header
	logPageHeaderLen  = 4
	logParamCode      = 0 // u16
	logParamLength    = 3 // u8, bytes following the 4-byte parameter header
	logParamHeaderLen = 4
)

// Informational Exceptions parameter 0x0000 payload.
const (
	ieASC  = 0
	ieASCQ = 1
	ieTemp = 2
	ieLen  = 3
)

// temperatureNotAvailable is what a drive reports for a temperature it does not
// have. Recording it as 255 °C would raise every thermal alert at once.
const temperatureNotAvailable uint8 = 0xFF

// ErrSCSIResponse reports a reply that is not the structure it should be.
type ErrSCSIResponse struct {
	Op     string
	Detail string
}

func (e *ErrSCSIResponse) Error() string {
	return fmt.Sprintf("scsi: %s response is unusable: %s", e.Op, e.Detail)
}

// buildInquiryCDB builds an INQUIRY. A zero page requests the standard data;
// any other value requests that vital product data page.
func buildInquiryCDB(page uint8, allocLen int) []byte {
	var evpd uint8
	if page != 0 {
		evpd = 1
	}
	return []byte{scsiInquiry, evpd, page, byte(allocLen >> 8), byte(allocLen), 0}
}

// buildLogSenseCDB builds a LOG SENSE for one page.
//
// PC=01 in byte 2 asks for current cumulative values, which is what a health
// page is meaningful as; the default of threshold values would return the
// limits rather than the readings.
func buildLogSenseCDB(page uint8, allocLen int) []byte {
	const currentCumulativeValues = 0x40
	return []byte{
		scsiLogSense, 0x00, currentCumulativeValues | page,
		0, 0, 0,
		byte(allocLen >> 8), byte(allocLen),
		0, 0,
	}
}

// parseInquiry decodes a standard INQUIRY response.
func parseInquiry(data []byte) (vendor, product string, devType uint8, err error) {
	if len(data) < inquiryLen {
		return "", "", 0, &ErrSCSIResponse{
			Op:     "INQUIRY",
			Detail: fmt.Sprintf("%d bytes, need %d", len(data), inquiryLen),
		}
	}
	vendor = strings.TrimSpace(string(data[inquiryVendor : inquiryVendor+inquiryVendorLen]))
	product = strings.TrimSpace(string(data[inquiryProduct : inquiryProduct+inquiryProductLen]))
	return vendor, product, data[inquiryDeviceType] & 0x1F, nil
}

// parseUnitSerial decodes VPD page 0x80.
//
// The serial is the only identifier a drive behind a controller keeps across a
// reboot: it has no device node, and its device ID and slot are properties of
// where it is plugged in rather than of the drive.
func parseUnitSerial(data []byte) (string, error) {
	if len(data) < 4 {
		return "", &ErrSCSIResponse{Op: "INQUIRY VPD 0x80", Detail: "shorter than its own header"}
	}
	if data[1] != vpdUnitSerialNumber {
		return "", &ErrSCSIResponse{
			Op:     "INQUIRY VPD 0x80",
			Detail: fmt.Sprintf("is page 0x%02x, not the unit serial number page", data[1]),
		}
	}
	length := int(binary.BigEndian.Uint16(data[2:]))
	if length == 0 || 4+length > len(data) {
		return "", &ErrSCSIResponse{
			Op:     "INQUIRY VPD 0x80",
			Detail: fmt.Sprintf("declares %d bytes of serial, %d present", length, len(data)-4),
		}
	}
	return strings.TrimSpace(string(data[4 : 4+length])), nil
}

// logPageParameter returns one parameter's payload from a SCSI log page.
//
// Parameters are a self-describing chain, and every length in it comes from the
// drive. Walking it without checking each length against the bytes present is
// how a decoder walks off the end of a buffer a drive controls.
func logPageParameter(data []byte, page uint8, param uint16) ([]byte, error) {
	op := fmt.Sprintf("LOG SENSE 0x%02X", page)
	if len(data) < logPageHeaderLen {
		return nil, &ErrSCSIResponse{Op: op, Detail: "shorter than its own header"}
	}
	if got := data[logPageCode] & 0x3F; got != page {
		return nil, &ErrSCSIResponse{
			Op:     op,
			Detail: fmt.Sprintf("is page 0x%02x, not the one asked for", got),
		}
	}

	declared := int(binary.BigEndian.Uint16(data[logPageLength:]))
	end := logPageHeaderLen + declared
	if end > len(data) {
		end = len(data)
	}

	for off := logPageHeaderLen; off+logParamHeaderLen <= end; {
		entry := data[off:end]
		length := int(entry[logParamLength])
		if logParamHeaderLen+length > len(entry) {
			return nil, &ErrSCSIResponse{
				Op:     op,
				Detail: fmt.Sprintf("parameter at offset %d declares %d bytes, %d remain", off, length, len(entry)-logParamHeaderLen),
			}
		}
		if binary.BigEndian.Uint16(entry[logParamCode:]) == param {
			return entry[logParamHeaderLen : logParamHeaderLen+length], nil
		}
		// The cursor advances by at least the parameter header, so a
		// zero-length parameter costs a wasted step and cannot stall the walk.
		off += logParamHeaderLen + length
	}
	return nil, &ErrSCSIResponse{Op: op, Detail: fmt.Sprintf("has no parameter 0x%04x", param)}
}

// scsiHealth is what a SAS drive says about itself.
type scsiHealth struct {
	// Failing is the drive's own verdict: a non-zero additional sense code in
	// the Informational Exceptions page is the drive reporting that it expects
	// to fail. Zero is what `smartctl` prints as "SMART Health Status: OK".
	Failing bool
	ASC     uint8
	ASCQ    uint8
	// TemperatureC is 0 when the drive did not report one.
	TemperatureC float64
}

// parseInformationalExceptions decodes log page 0x2F.
func parseInformationalExceptions(data []byte) (scsiHealth, error) {
	value, err := logPageParameter(data, logPageInformationalExceptions, 0x0000)
	if err != nil {
		return scsiHealth{}, err
	}
	if len(value) < ieLen {
		return scsiHealth{}, &ErrSCSIResponse{
			Op:     "LOG SENSE 0x2F",
			Detail: fmt.Sprintf("informational exceptions parameter is %d bytes, need %d", len(value), ieLen),
		}
	}

	health := scsiHealth{ASC: value[ieASC], ASCQ: value[ieASCQ]}
	health.Failing = health.ASC != 0 || health.ASCQ != 0
	if t := value[ieTemp]; t != temperatureNotAvailable {
		health.TemperatureC = float64(t)
	}
	return health, nil
}

// parseTemperatureLog decodes log page 0x0D, which reports a temperature in
// whole degrees Celsius. It returns 0 when the drive says it has none.
func parseTemperatureLog(data []byte) (float64, error) {
	value, err := logPageParameter(data, logPageTemperature, 0x0000)
	if err != nil {
		return 0, err
	}
	if len(value) < 2 {
		return 0, &ErrSCSIResponse{
			Op:     "LOG SENSE 0x0D",
			Detail: fmt.Sprintf("temperature parameter is %d bytes, need 2", len(value)),
		}
	}
	if value[1] == temperatureNotAvailable {
		return 0, nil
	}
	return float64(value[1]), nil
}
