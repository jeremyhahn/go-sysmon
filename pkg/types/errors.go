package types

import (
	"strconv"
	"time"
)

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

// NoSnapshotYetError indicates the poll loop is running and has not completed
// its first collection.
//
// It exists because the absence of a snapshot has two causes and only one of
// them is a fault. The loop collects on its first tick rather than on Start,
// so every healthy service passes through this state on the way up, for as
// long as one collection takes -- SMBIOS, every GPU, every disk and the whole
// process table on a cold cache. Reporting that as "monitor is not running"
// sends the reader after a lifecycle bug that does not exist, which it has
// done in a test helper, in a red CI job (2302a7e), and in the systemd
// watchdog's own log line on every start on every node in the fleet.
type NoSnapshotYetError struct{}

func (e *NoSnapshotYetError) Error() string {
	return "monitor has not collected a snapshot yet"
}

// MonitorAlreadyRunningError indicates the monitor is already started.
type MonitorAlreadyRunningError struct{}

func (e *MonitorAlreadyRunningError) Error() string {
	return "monitor is already running"
}

// DecoratorInstalledError indicates a monitor already has a different
// decorator. A monitor's snapshots describe one node, so they carry one
// decoration: a second one would silently replace the first's build, name,
// thresholds and alert history for every reader of that monitor.
type DecoratorInstalledError struct{}

func (e *DecoratorInstalledError) Error() string {
	return "monitor already has a different decorator; remove it first"
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

// NPUIndexNotFoundError indicates that the requested NPU index does not exist.
type NPUIndexNotFoundError struct {
	Index     int
	Available int
}

func (e *NPUIndexNotFoundError) Error() string {
	return "NPU index " + strconv.Itoa(e.Index) + " not found; " + strconv.Itoa(e.Available) + " NPU(s) available"
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

// CgroupReadError indicates a cgroup interface file could not be read. The
// path is carried because "which file" is the whole diagnosis: a process can
// be in cgroup v2, v1, or neither, and each reads differently.
type CgroupReadError struct {
	Path  string
	Cause error
}

func (e *CgroupReadError) Error() string {
	return "read cgroup file " + e.Path + ": " + e.Cause.Error()
}

func (e *CgroupReadError) Unwrap() error {
	return e.Cause
}

// CgroupParseError indicates a cgroup interface file held something other than
// a byte count or the literal "max". The value is carried verbatim rather than
// summarised, because a kernel writing something unexpected there is the fact
// worth seeing.
type CgroupParseError struct {
	Path  string
	Value string
}

func (e *CgroupParseError) Error() string {
	return "cgroup file " + e.Path + " holds " + strconv.Quote(e.Value) + "; want a byte count or \"max\""
}

// NotifySocketError indicates the systemd notification socket could not be
// opened. The address is carried because the two failure modes -- a filesystem
// path that does not exist and an abstract name that was not translated --
// are indistinguishable without it.
type NotifySocketError struct {
	Addr  string
	Cause error
}

func (e *NotifySocketError) Error() string {
	return "dial notify socket " + strconv.Quote(e.Addr) + ": " + e.Cause.Error()
}

func (e *NotifySocketError) Unwrap() error { return e.Cause }

// NotifySendError indicates a notification datagram could not be delivered.
type NotifySendError struct {
	Message string
	Cause   error
}

func (e *NotifySendError) Error() string {
	return "send notification " + strconv.Quote(e.Message) + ": " + e.Cause.Error()
}

func (e *NotifySendError) Unwrap() error { return e.Cause }

// StaleCollectionError indicates the monitor's most recent snapshot is older
// than the watchdog is willing to accept. Both numbers are carried because the
// limit is derived from a runtime-settable interval, so "how old" means nothing
// without "older than what".
type StaleCollectionError struct {
	Age   time.Duration
	Limit time.Duration
}

func (e *StaleCollectionError) Error() string {
	return "last snapshot is " + e.Age.Round(time.Second).String() +
		" old, older than the " + e.Limit.String() + " the watchdog allows"
}

// SelfRequestError indicates the server answered its own watchdog request with
// something other than 200. It is distinct from a transport failure: reaching
// the socket and being refused says the listener is alive and the handler is
// not, which is a different fault from not reaching it at all.
type SelfRequestError struct {
	Addr   string
	Status int
}

func (e *SelfRequestError) Error() string {
	return "self request to " + e.Addr + " returned status " + strconv.Itoa(e.Status)
}
