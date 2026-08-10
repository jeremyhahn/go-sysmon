package types

import "strconv"

// CollectorError indicates a failure during metrics collection.
type CollectorError struct {
	Collector string
	Cause     error
}

func (e *CollectorError) Error() string {
	return "collector " + e.Collector + ": " + e.Cause.Error()
}

func (e *CollectorError) Unwrap() error {
	return e.Cause
}

// MonitorNotRunningError indicates the monitor is not started.
type MonitorNotRunningError struct{}

func (e *MonitorNotRunningError) Error() string {
	return "monitor is not running"
}

// MonitorAlreadyRunningError indicates the monitor is already started.
type MonitorAlreadyRunningError struct{}

func (e *MonitorAlreadyRunningError) Error() string {
	return "monitor is already running"
}

// StreamUnsupportedError indicates that the http.ResponseWriter handling an
// event-stream request cannot flush, so server-sent events cannot be delivered.
// Every ResponseWriter in net/http supports flushing; this signals a wrapping
// middleware that dropped the capability.
type StreamUnsupportedError struct{}

func (e *StreamUnsupportedError) Error() string {
	return "response writer does not support streaming"
}

// TLSConfigError indicates that the server's TLS configuration could not be
// built, for example because a certificate or key file failed to load.
type TLSConfigError struct {
	Message string
	Cause   error
}

func (e *TLSConfigError) Error() string {
	if e.Cause == nil {
		return "tls config: " + e.Message
	}
	return "tls config: " + e.Message + ": " + e.Cause.Error()
}

func (e *TLSConfigError) Unwrap() error {
	return e.Cause
}

// InvalidIntervalError indicates an invalid polling interval.
type InvalidIntervalError struct {
	Message string
}

func (e *InvalidIntervalError) Error() string {
	return "invalid interval: " + e.Message
}

// ServerStartError indicates a server startup failure.
type ServerStartError struct {
	Cause error
}

func (e *ServerStartError) Error() string {
	return "server start: " + e.Cause.Error()
}

func (e *ServerStartError) Unwrap() error {
	return e.Cause
}

// GUIUnavailableError indicates the desktop GUI is not available.
type GUIUnavailableError struct{}

func (e *GUIUnavailableError) Error() string {
	return "GUI unavailable: binary built without desktop support"
}

// SysfsReadError indicates a failure reading a sysfs file.
type SysfsReadError struct {
	Path  string
	Cause error
}

func (e *SysfsReadError) Error() string {
	return "sysfs read " + e.Path + ": " + e.Cause.Error()
}

func (e *SysfsReadError) Unwrap() error {
	return e.Cause
}

// SysfsTopologyError indicates that sysfs CPU topology data is missing or invalid.
type SysfsTopologyError struct {
	Reason string
}

func (e *SysfsTopologyError) Error() string {
	return "sysfs topology: " + e.Reason
}

// SMARTReadError indicates a SMART data read failure.
type SMARTReadError struct {
	Device string
	Cause  error
}

func (e *SMARTReadError) Error() string {
	return "smart read " + e.Device + ": " + e.Cause.Error()
}

func (e *SMARTReadError) Unwrap() error {
	return e.Cause
}

// PSIParseError indicates a failure parsing PSI data.
type PSIParseError struct {
	Resource string
	Cause    error
}

func (e *PSIParseError) Error() string {
	return "psi parse " + e.Resource + ": " + e.Cause.Error()
}

func (e *PSIParseError) Unwrap() error {
	return e.Cause
}

// RAPLReadError indicates a failure reading RAPL power data.
type RAPLReadError struct {
	Path  string
	Cause error
}

func (e *RAPLReadError) Error() string {
	return "rapl read " + e.Path + ": " + e.Cause.Error()
}

func (e *RAPLReadError) Unwrap() error {
	return e.Cause
}

// GPUIndexNotFoundError indicates that the requested GPU index does not exist.
type GPUIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *GPUIndexNotFoundError) Error() string {
	return "GPU index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " GPU(s) available"
}

// DIMMIndexNotFoundError indicates that the requested DIMM index does not exist.
type DIMMIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *DIMMIndexNotFoundError) Error() string {
	return "DIMM index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " DIMM(s) available"
}

// DiskIndexNotFoundError indicates that the requested disk index does not exist.
type DiskIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *DiskIndexNotFoundError) Error() string {
	return "disk index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " disk(s) available"
}

// CPUIndexNotFoundError indicates that the requested CPU index does not exist.
type CPUIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *CPUIndexNotFoundError) Error() string {
	return "CPU index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " CPU(s) available"
}

// NetworkIndexNotFoundError indicates that the requested network interface index does not exist.
type NetworkIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *NetworkIndexNotFoundError) Error() string {
	return "network index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " interface(s) available"
}

// SMBIOSParseError indicates a failure parsing the SMBIOS binary table.
type SMBIOSParseError struct {
	Reason string
	Cause  error
}

func (e *SMBIOSParseError) Error() string {
	if e.Cause != nil {
		return "smbios parse: " + e.Reason + ": " + e.Cause.Error()
	}
	return "smbios parse: " + e.Reason
}

func (e *SMBIOSParseError) Unwrap() error {
	return e.Cause
}

// ContainerIndexNotFoundError indicates that the requested container index does not exist.
type ContainerIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *ContainerIndexNotFoundError) Error() string {
	return "container index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " container(s) available"
}

// VMIndexNotFoundError indicates that the requested VM index does not exist.
type VMIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *VMIndexNotFoundError) Error() string {
	return "vm index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " vm(s) available"
}

// LogFileError indicates the log file could not be opened or created.
type LogFileError struct {
	Path  string
	Cause error
}

func (e *LogFileError) Error() string {
	return "log file " + e.Path + ": " + e.Cause.Error()
}

func (e *LogFileError) Unwrap() error {
	return e.Cause
}

// InvalidLogLevelError indicates an unrecognised --log-level value.
type InvalidLogLevelError struct {
	Level string
}

func (e *InvalidLogLevelError) Error() string {
	return "invalid log level " + e.Level + "; want debug, info, warn or error"
}

// InvalidLogFormatError indicates an unrecognised --log-format value.
type InvalidLogFormatError struct {
	Format string
}

func (e *InvalidLogFormatError) Error() string {
	return "invalid log format " + e.Format + "; want text or json"
}

// InvalidSortKeyError indicates an unrecognised --sort value.
type InvalidSortKeyError struct {
	Key   string
	Valid string
}

func (e *InvalidSortKeyError) Error() string {
	return "invalid sort key " + e.Key + "; want one of " + e.Valid
}
