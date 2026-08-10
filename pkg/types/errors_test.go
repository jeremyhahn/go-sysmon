package types_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// errSentinel is a simple error used as a wrapped cause throughout the tests.
var errSentinel = errors.New("underlying cause")

// ---- CollectorError -------------------------------------------------------

func TestCollectorError_Error(t *testing.T) {
	e := &types.CollectorError{Collector: "cpu", Cause: errSentinel}
	want := "collector cpu: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestCollectorError_Unwrap(t *testing.T) {
	e := &types.CollectorError{Collector: "cpu", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(CollectorError, errSentinel) = false, want true")
	}
}

func TestCollectorError_As(t *testing.T) {
	wrapped := &types.CollectorError{Collector: "disk", Cause: errSentinel}
	err := fmt.Errorf("wrap: %w", wrapped)

	var target *types.CollectorError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap CollectorError")
	}
	if target.Collector != "disk" {
		t.Errorf("Collector = %q, want %q", target.Collector, "disk")
	}
}

func TestCollectorError_EmptyCause(t *testing.T) {
	cause := errors.New("io timeout")
	e := &types.CollectorError{Collector: "network", Cause: cause}
	want := "collector network: io timeout"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// ---- MonitorNotRunningError -----------------------------------------------

func TestMonitorNotRunningError_Error(t *testing.T) {
	e := &types.MonitorNotRunningError{}
	want := "monitor is not running"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestMonitorNotRunningError_As(t *testing.T) {
	var e *types.MonitorNotRunningError
	err := &types.MonitorNotRunningError{}
	if !errors.As(err, &e) {
		t.Error("errors.As failed for MonitorNotRunningError")
	}
}

// ---- MonitorAlreadyRunningError -------------------------------------------

func TestMonitorAlreadyRunningError_Error(t *testing.T) {
	e := &types.MonitorAlreadyRunningError{}
	want := "monitor is already running"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestMonitorAlreadyRunningError_As(t *testing.T) {
	var target *types.MonitorAlreadyRunningError
	err := &types.MonitorAlreadyRunningError{}
	if !errors.As(err, &target) {
		t.Error("errors.As failed for MonitorAlreadyRunningError")
	}
}

// ---- StreamUnsupportedError -----------------------------------------------

func TestStreamUnsupportedError_Error(t *testing.T) {
	e := &types.StreamUnsupportedError{}
	want := "response writer does not support streaming"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestStreamUnsupportedError_As(t *testing.T) {
	err := fmt.Errorf("outer: %w", &types.StreamUnsupportedError{})

	var target *types.StreamUnsupportedError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap StreamUnsupportedError")
	}
}

// ---- TLSConfigError -------------------------------------------------------

func TestTLSConfigError_Error(t *testing.T) {
	tests := map[string]struct {
		err  *types.TLSConfigError
		want string
	}{
		"with cause": {
			err:  &types.TLSConfigError{Message: "load key pair", Cause: errSentinel},
			want: "tls config: load key pair: underlying cause",
		},
		"without cause": {
			err:  &types.TLSConfigError{Message: "no certificate source"},
			want: "tls config: no certificate source",
		},
	}

	for name, tc := range tests {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: Error() = %q, want %q", name, got, tc.want)
		}
	}
}

func TestTLSConfigError_Unwrap(t *testing.T) {
	e := &types.TLSConfigError{Message: "load key pair", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(TLSConfigError, errSentinel) = false, want true")
	}

	// A configuration error with no underlying cause must unwrap to nil rather
	// than panicking.
	bare := &types.TLSConfigError{Message: "no certificate source"}
	if bare.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil when no cause was recorded", bare.Unwrap())
	}
}

func TestTLSConfigError_As(t *testing.T) {
	wrapped := &types.TLSConfigError{Message: "load key pair", Cause: errSentinel}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.TLSConfigError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap TLSConfigError")
	}
}

// ---- InvalidIntervalError -------------------------------------------------

func TestInvalidIntervalError_Error(t *testing.T) {
	e := &types.InvalidIntervalError{Message: "must be positive"}
	want := "invalid interval: must be positive"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestInvalidIntervalError_As(t *testing.T) {
	var target *types.InvalidIntervalError
	err := &types.InvalidIntervalError{Message: "zero not allowed"}
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed for InvalidIntervalError")
	}
	if target.Message != "zero not allowed" {
		t.Errorf("Message = %q, want %q", target.Message, "zero not allowed")
	}
}

func TestInvalidIntervalError_NoUnwrap(t *testing.T) {
	e := &types.InvalidIntervalError{Message: "some message"}
	// InvalidIntervalError does not wrap an error; errors.Is against an
	// unrelated errSentinel must return false.
	if errors.Is(e, errSentinel) {
		t.Error("errors.Is(InvalidIntervalError, errSentinel) = true, want false")
	}
}

// ---- ServerStartError -----------------------------------------------------

func TestServerStartError_Error(t *testing.T) {
	e := &types.ServerStartError{Cause: errSentinel}
	want := "server start: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestServerStartError_Unwrap(t *testing.T) {
	e := &types.ServerStartError{Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(ServerStartError, errSentinel) = false, want true")
	}
}

func TestServerStartError_As(t *testing.T) {
	wrapped := &types.ServerStartError{Cause: errSentinel}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.ServerStartError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap ServerStartError")
	}
}

// ---- GUIUnavailableError --------------------------------------------------

func TestGUIUnavailableError_Error(t *testing.T) {
	e := &types.GUIUnavailableError{}
	want := "GUI unavailable: binary built without desktop support"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestGUIUnavailableError_As(t *testing.T) {
	var target *types.GUIUnavailableError
	err := &types.GUIUnavailableError{}
	if !errors.As(err, &target) {
		t.Error("errors.As failed for GUIUnavailableError")
	}
}

func TestGUIUnavailableError_NoUnwrap(t *testing.T) {
	e := &types.GUIUnavailableError{}
	if errors.Is(e, errSentinel) {
		t.Error("errors.Is(GUIUnavailableError, errSentinel) = true, want false")
	}
}

// ---- SMARTReadError -------------------------------------------------------

func TestSMARTReadError_Error(t *testing.T) {
	e := &types.SMARTReadError{Device: "/dev/sda", Cause: errSentinel}
	want := "smart read /dev/sda: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSMARTReadError_Unwrap(t *testing.T) {
	e := &types.SMARTReadError{Device: "/dev/nvme0", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(SMARTReadError, errSentinel) = false, want true")
	}
}

func TestSMARTReadError_As(t *testing.T) {
	wrapped := &types.SMARTReadError{Device: "/dev/sdb", Cause: errSentinel}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.SMARTReadError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap SMARTReadError")
	}
	if target.Device != "/dev/sdb" {
		t.Errorf("Device = %q, want %q", target.Device, "/dev/sdb")
	}
}

// ---- SMBIOSParseError -----------------------------------------------------

func TestSMBIOSParseError_ErrorWithCause(t *testing.T) {
	e := &types.SMBIOSParseError{Reason: "truncated structure", Cause: errSentinel}
	want := "smbios parse: truncated structure: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSMBIOSParseError_ErrorWithoutCause(t *testing.T) {
	e := &types.SMBIOSParseError{Reason: "no data"}
	want := "smbios parse: no data"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSMBIOSParseError_Unwrap(t *testing.T) {
	e := &types.SMBIOSParseError{Reason: "bad magic", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(SMBIOSParseError, errSentinel) = false, want true")
	}
}

func TestSMBIOSParseError_As(t *testing.T) {
	wrapped := &types.SMBIOSParseError{Reason: "offset out of range", Cause: errSentinel}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.SMBIOSParseError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap SMBIOSParseError")
	}
	if target.Reason != "offset out of range" {
		t.Errorf("Reason = %q, want %q", target.Reason, "offset out of range")
	}
}

// ---- GPUIndexNotFoundError ------------------------------------------------

func TestGPUIndexNotFoundError_Error(t *testing.T) {
	e := &types.GPUIndexNotFoundError{Index: 5, Available: 3}
	want := "GPU index 5 not found; 3 GPU(s) available"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestGPUIndexNotFoundError_As(t *testing.T) {
	wrapped := &types.GPUIndexNotFoundError{Index: 2, Available: 1}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.GPUIndexNotFoundError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap GPUIndexNotFoundError")
	}
	if target.Index != 2 {
		t.Errorf("Index = %d, want %d", target.Index, 2)
	}
	if target.Available != 1 {
		t.Errorf("Available = %d, want %d", target.Available, 1)
	}
}

func TestGPUIndexNotFoundError_NoUnwrap(t *testing.T) {
	e := &types.GPUIndexNotFoundError{Index: 0, Available: 0}
	// GPUIndexNotFoundError does not wrap an error; errors.Is against an
	// unrelated errSentinel must return false.
	if errors.Is(e, errSentinel) {
		t.Error("errors.Is(GPUIndexNotFoundError, errSentinel) = true, want false")
	}
}

// ---- PSIParseError --------------------------------------------------------

func TestPSIParseError_Error(t *testing.T) {
	t.Parallel()
	e := &types.PSIParseError{Resource: "cpu", Cause: errSentinel}
	want := "psi parse cpu: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestPSIParseError_Unwrap(t *testing.T) {
	t.Parallel()
	e := &types.PSIParseError{Resource: "memory", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(PSIParseError, errSentinel) = false, want true")
	}
}

func TestPSIParseError_Is(t *testing.T) {
	t.Parallel()
	wrapped := &types.PSIParseError{Resource: "io", Cause: errSentinel}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.PSIParseError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap PSIParseError")
	}
	if target.Resource != "io" {
		t.Errorf("Resource = %q, want %q", target.Resource, "io")
	}
}

// ---- RAPLReadError --------------------------------------------------------

func TestRAPLReadError_Error(t *testing.T) {
	t.Parallel()
	e := &types.RAPLReadError{Path: "/sys/class/powercap/intel-rapl:0/energy_uj", Cause: errSentinel}
	want := "rapl read /sys/class/powercap/intel-rapl:0/energy_uj: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestRAPLReadError_Unwrap(t *testing.T) {
	t.Parallel()
	e := &types.RAPLReadError{Path: "/sys/class/powercap/intel-rapl:1/energy_uj", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(RAPLReadError, errSentinel) = false, want true")
	}
}

func TestRAPLReadError_Is(t *testing.T) {
	t.Parallel()
	wrapped := &types.RAPLReadError{Path: "/sys/class/powercap/intel-rapl:0/max_energy_range_uj", Cause: errSentinel}
	err := fmt.Errorf("outer: %w", wrapped)

	var target *types.RAPLReadError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap RAPLReadError")
	}
	if target.Path != "/sys/class/powercap/intel-rapl:0/max_energy_range_uj" {
		t.Errorf("Path = %q, want %q", target.Path, "/sys/class/powercap/intel-rapl:0/max_energy_range_uj")
	}
}

// ---- SysfsReadError -------------------------------------------------------

func TestSysfsReadError_Error(t *testing.T) {
	e := &types.SysfsReadError{Path: "/sys/class/hwmon/hwmon0/temp1_input", Cause: errSentinel}
	want := "sysfs read /sys/class/hwmon/hwmon0/temp1_input: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSysfsReadError_Unwrap(t *testing.T) {
	e := &types.SysfsReadError{Path: "/sys/x", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(SysfsReadError, errSentinel) = false, want true")
	}

	var target *types.SysfsReadError
	if !errors.As(fmt.Errorf("wrapped: %w", e), &target) {
		t.Fatal("errors.As failed to unwrap SysfsReadError")
	}
	if target.Path != "/sys/x" {
		t.Errorf("Path = %q, want %q", target.Path, "/sys/x")
	}
}

// ---- SysfsTopologyError ---------------------------------------------------

func TestSysfsTopologyError_Error(t *testing.T) {
	e := &types.SysfsTopologyError{Reason: "no core_id file"}
	want := "sysfs topology: no core_id file"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSysfsTopologyError_EmptyReason(t *testing.T) {
	e := &types.SysfsTopologyError{}
	if got := e.Error(); got != "sysfs topology: " {
		t.Errorf("Error() = %q, want %q", got, "sysfs topology: ")
	}
}

// ---- index-not-found errors -----------------------------------------------

// TestIndexNotFoundErrors_Error covers the four parallel index errors, which
// share a message shape and differ only in the noun they report.
func TestIndexNotFoundErrors_Error(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "DIMM",
			err:  &types.DIMMIndexNotFoundError{Index: 4, Available: 4},
			want: "DIMM index 4 not found; 4 DIMM(s) available",
		},
		{
			name: "disk",
			err:  &types.DiskIndexNotFoundError{Index: 9, Available: 6},
			want: "disk index 9 not found; 6 disk(s) available",
		},
		{
			name: "CPU",
			err:  &types.CPUIndexNotFoundError{Index: 24, Available: 24},
			want: "CPU index 24 not found; 24 CPU(s) available",
		},
		{
			name: "network",
			err:  &types.NetworkIndexNotFoundError{Index: 10, Available: 10},
			want: "network index 10 not found; 10 interface(s) available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIndexNotFoundErrors_ZeroAvailable covers the boundary where nothing was
// collected, so the message must still read correctly rather than being empty.
func TestIndexNotFoundErrors_ZeroAvailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "DIMM",
			err:  &types.DIMMIndexNotFoundError{Index: 0, Available: 0},
			want: "DIMM index 0 not found; 0 DIMM(s) available",
		},
		{
			name: "disk",
			err:  &types.DiskIndexNotFoundError{Index: -1, Available: 0},
			want: "disk index -1 not found; 0 disk(s) available",
		},
		{
			name: "CPU",
			err:  &types.CPUIndexNotFoundError{Index: 0, Available: 0},
			want: "CPU index 0 not found; 0 CPU(s) available",
		},
		{
			name: "network",
			err:  &types.NetworkIndexNotFoundError{Index: 0, Available: 0},
			want: "network index 0 not found; 0 interface(s) available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- virtualization index errors ------------------------------------------

func TestContainerIndexNotFoundError_Error(t *testing.T) {
	e := &types.ContainerIndexNotFoundError{Index: 9, Available: 5}
	want := "container index 9 not found; 5 container(s) available"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestContainerIndexNotFoundError_ZeroAvailable(t *testing.T) {
	e := &types.ContainerIndexNotFoundError{Index: 0, Available: 0}
	want := "container index 0 not found; 0 container(s) available"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestVMIndexNotFoundError_Error(t *testing.T) {
	e := &types.VMIndexNotFoundError{Index: 3, Available: 1}
	want := "vm index 3 not found; 1 vm(s) available"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestVMIndexNotFoundError_NegativeIndex(t *testing.T) {
	e := &types.VMIndexNotFoundError{Index: -1, Available: 0}
	want := "vm index -1 not found; 0 vm(s) available"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// ---- logging errors -------------------------------------------------------

func TestLogFileError_Error(t *testing.T) {
	e := &types.LogFileError{Path: "/var/log/sysmon.log", Cause: errSentinel}
	want := "log file /var/log/sysmon.log: underlying cause"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestLogFileError_Unwrap(t *testing.T) {
	e := &types.LogFileError{Path: "/x", Cause: errSentinel}
	if !errors.Is(e, errSentinel) {
		t.Error("errors.Is(LogFileError, errSentinel) = false, want true")
	}

	var target *types.LogFileError
	if !errors.As(fmt.Errorf("wrapped: %w", e), &target) {
		t.Fatal("errors.As failed to unwrap LogFileError")
	}
	if target.Path != "/x" {
		t.Errorf("Path = %q, want /x", target.Path)
	}
}

// TestInvalidLogLevelError_ListsValidValues verifies the message tells the
// user what to use instead, not merely that they were wrong.
func TestInvalidLogLevelError_ListsValidValues(t *testing.T) {
	e := &types.InvalidLogLevelError{Level: "verbose"}
	got := e.Error()
	if !strings.Contains(got, "verbose") {
		t.Errorf("Error() = %q, want it to echo the rejected value", got)
	}
	for _, want := range []string{"debug", "info", "warn", "error"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to list %q", got, want)
		}
	}
}

func TestInvalidLogFormatError_ListsValidValues(t *testing.T) {
	e := &types.InvalidLogFormatError{Format: "yaml"}
	got := e.Error()
	if !strings.Contains(got, "yaml") {
		t.Errorf("Error() = %q, want it to echo the rejected value", got)
	}
	for _, want := range []string{"text", "json"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to list %q", got, want)
		}
	}
}

func TestInvalidSortKeyError_ListsValidKeys(t *testing.T) {
	e := &types.InvalidSortKeyError{Key: "bogus", Valid: "size, tag, id"}
	got := e.Error()
	if !strings.Contains(got, "bogus") {
		t.Errorf("Error() = %q, want it to echo the rejected key", got)
	}
	for _, want := range []string{"size", "tag", "id"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to list %q", got, want)
		}
	}
}
