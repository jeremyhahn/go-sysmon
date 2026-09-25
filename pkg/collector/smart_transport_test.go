//go:build linux

package collector

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// notADevice returns a regular file: it opens like a device node, and every
// ioctl on it is refused with ENOTTY, which is what a node that does not
// speak the protocol answers.
func notADevice(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sdz")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadATASMART_NodeThatAnswersNeitherTransport verifies both ATA
// transports are tried, and a node that refuses both reports both refusals
// rather than an empty reading.
func TestReadATASMART_NodeThatAnswersNeitherTransport(t *testing.T) {
	var info types.DiskInfo
	err := readATASMART(notADevice(t), &info)

	var unavailable *SMARTUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("readATASMART() error = %v, want a *SMARTUnavailableError", err)
	}
	var hdio *SMARTIoctlError
	if !errors.As(unavailable.HDIOErr, &hdio) || !strings.HasPrefix(hdio.Op, "HDIO_DRIVE_CMD") {
		t.Errorf("HDIOErr = %v, want the HDIO_DRIVE_CMD refusal", unavailable.HDIOErr)
	}
	if unavailable.SGIOErr == nil {
		t.Error("SGIOErr = nil, want the SG_IO fallback's refusal kept")
	}
	if !errors.Is(err, syscall.ENOTTY) {
		t.Errorf("errors.Is(err, ENOTTY) = false for %v; the errno must be reachable through the wrapper", err)
	}
	if info.SMARTEnabled {
		t.Error("SMARTEnabled set from a reading that never happened")
	}
}

func TestReadATASMART_MissingNode(t *testing.T) {
	var info types.DiskInfo
	err := readATASMART(filepath.Join(t.TempDir(), "absent"), &info)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readATASMART(missing) error = %v, want it to report the node is absent", err)
	}
}

// TestReadNVMeSMART_NodeThatIsNotNVMe verifies the SMART log request fails
// typed, naming the command, and leaves the record untouched.
func TestReadNVMeSMART_NodeThatIsNotNVMe(t *testing.T) {
	var info types.DiskInfo
	err := readNVMeSMART(notADevice(t), &info)

	var ioctlErr *SMARTIoctlError
	if !errors.As(err, &ioctlErr) || ioctlErr.Op != "GetLogPage(SMART)" {
		t.Fatalf("readNVMeSMART() error = %v, want a GetLogPage(SMART) *SMARTIoctlError", err)
	}
	if !errors.Is(err, syscall.ENOTTY) {
		t.Errorf("errors.Is(err, ENOTTY) = false for %v", err)
	}
	if info.SMARTEnabled {
		t.Error("SMARTEnabled set from a log page that was never read")
	}
}

func TestReadNVMeSMART_MissingNode(t *testing.T) {
	var info types.DiskInfo
	err := readNVMeSMART(filepath.Join(t.TempDir(), "absent"), &info)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readNVMeSMART(missing) error = %v, want it to report the node is absent", err)
	}
}

// validSector returns a SMART sector with one attribute and a checksum that
// clears.
func validSector() []byte {
	data := make([]byte, smartDataSize)
	data[0] = 0x10 // data structure revision
	data[2] = 5    // attribute ID: reallocated sectors
	var sum byte
	for _, b := range data[:smartDataSize-1] {
		sum += b
	}
	data[smartDataSize-1] = -sum
	return data
}

func TestValidateSMARTSector(t *testing.T) {
	if err := validateSMARTSector("/dev/sda", validSector()); err != nil {
		t.Fatalf("validateSMARTSector(valid) = %v, want nil", err)
	}

	short := validSector()[:100]
	var shortErr *SMARTShortTransferError
	if err := validateSMARTSector("/dev/sda", short); !errors.As(err, &shortErr) || shortErr.Got != 100 {
		t.Errorf("validateSMARTSector(short) = %v, want a *SMARTShortTransferError of 100 bytes", err)
	}

	var emptyErr *SMARTEmptySectorError
	err := validateSMARTSector("/dev/sda", make([]byte, smartDataSize))
	if !errors.As(err, &emptyErr) || !strings.Contains(err.Error(), "/dev/sda") {
		t.Errorf("validateSMARTSector(zeros) = %v, want a *SMARTEmptySectorError naming the device", err)
	}

	corrupt := validSector()
	corrupt[10] ^= 0xFF
	var sumErr *SMARTChecksumError
	err = validateSMARTSector("/dev/sda", corrupt)
	if !errors.As(err, &sumErr) || sumErr.Sum == 0 || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("validateSMARTSector(corrupt) = %v, want a *SMARTChecksumError with a non-zero sum", err)
	}
}
