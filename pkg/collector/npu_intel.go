package collector

import (
	"path/filepath"
)

// npuDriverReader reads the sysfs attributes one accel driver invents for
// itself. devPath is the bus device directory holding them.
type npuDriverReader func(devPath string) npuCounters

// npuDriverReaders dispatches on the bound driver name. A driver absent from
// this table is not a failure: identity, residency, hwmon, devfreq and
// per-client usage all come from interfaces every accel driver shares, so an
// unrecognised accelerator still reports everything the kernel exposes
// generically, and gains its extra attributes when someone with that hardware
// adds a row.
var npuDriverReaders = map[string]npuDriverReader{
	"intel_vpu": readIntelVPUCounters,
}

// readIntelVPUCounters reads the attributes intel_vpu exposes for Intel NPUs
// (Meteor Lake onwards; measured on Arrow Lake). Every read here can resume a
// runtime-suspended device, so the caller must only call it while the
// accelerator is already awake.
//
// intel_vpu does NOT implement the DRM usage-stats fdinfo interface. Measured
// 2026-09-16 against a live client -- an OpenVINO model server holding
// /dev/accel/accel0 -- whose fdinfo carried pos/flags/mnt_id/ino and not one
// drm- key. So util_percent is structurally zero on Intel NPUs however busy the
// hardware is, and the client list is the available signal: the collector still
// reports WHICH processes hold the accelerator, just not how much time they
// spent on it. That costs privilege, because readlink on another user's file
// descriptors does not work unprivileged.
//
// npu_busy_time_us is returned as the raw counter it is. The driver documents
// it as time spent executing jobs, but on Arrow Lake it was measured advancing
// at 98.8% of resumed time with no client holding the device, and its lifetime
// total sat within 2% of runtime_active_time -- it follows D0 residency, not
// work. types.NPUInfo explains why no percentage is derived from it.
func readIntelVPUCounters(devPath string) npuCounters {
	return npuCounters{
		busyTimeUs:  readSysfsUint64(filepath.Join(devPath, "npu_busy_time_us")),
		memoryBytes: readSysfsUint64(filepath.Join(devPath, "npu_memory_utilization")),
		schedMode:   readSysfsString(filepath.Join(devPath, "sched_mode")),
		freqMHz:     uint32(readSysfsUint64(filepath.Join(devPath, "npu_current_frequency_mhz"))),
		freqMaxMHz:  uint32(readSysfsUint64(filepath.Join(devPath, "npu_max_frequency_mhz"))),
		read:        true,
	}
}
