package collector

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withAccelRoot points the collector at a synthetic accel class tree so tests
// exercise accelerators the build host does not have.
func withAccelRoot(t *testing.T, root string) {
	t.Helper()
	orig := accelRoot
	accelRoot = root
	t.Cleanup(func() { accelRoot = orig })
}

// withPCIIDs points the identifier lookup at a synthetic database and resets
// the one-time parse, which would otherwise carry between cases.
func withPCIIDs(t *testing.T, paths ...string) {
	t.Helper()
	origPaths := pciIDsPaths
	origVendors, origDevices, origClasses := pciVendors, pciDevices, pciClasses
	// A sync.Once must never be copied, so the guard is replaced with a fresh
	// one on the way in and on the way out rather than saved and restored.
	pciIDsPaths, pciIDsOnce = paths, sync.Once{}
	t.Cleanup(func() {
		pciIDsPaths, pciIDsOnce = origPaths, sync.Once{}
		pciVendors, pciDevices, pciClasses = origVendors, origDevices, origClasses
	})
}

// makeAccelDevice builds an accel class entry whose "device" child holds the
// given attributes, mirroring the real /sys/class/accel/accelN/device layout.
func makeAccelDevice(t *testing.T, root, name string, attrs map[string]string) string {
	t.Helper()
	dev := filepath.Join(root, name, "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dev, err)
	}
	for k, v := range attrs {
		writeSysfs(t, filepath.Join(dev, k), v)
	}
	return dev
}

func newTestNPUCollector(t *testing.T) *NPUCollector {
	t.Helper()
	resetWarnOnce()
	return NewNPUCollector(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// TestEnumerateAccelDevices_SortsAndFiltersEntries verifies only accelN
// directories are enumerated and that the order is deterministic.
func TestEnumerateAccelDevices_SortsAndFiltersEntries(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"accel1", "accel0", "accel10", "version", "card0"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	withAccelRoot(t, root)

	got, err := enumerateAccelDevices()
	if err != nil {
		t.Fatalf("enumerateAccelDevices: %v", err)
	}
	want := []string{"accel0", "accel1", "accel10"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestEnumerateAccelDevices_NoAccelClass verifies a host with no accelerator
// support reports no devices rather than an error. A kernel without the accel
// subsystem is the common case, not a fault.
func TestEnumerateAccelDevices_NoAccelClass(t *testing.T) {
	withAccelRoot(t, filepath.Join(t.TempDir(), "absent"))

	got, err := enumerateAccelDevices()
	if err != nil {
		t.Fatalf("expected no error for a missing accel class, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no devices, got %v", got)
	}
}

// TestEnumerateAccelDevices_UnreadableRoot verifies an accel class that exists
// but cannot be read is reported as an error, distinct from one that is absent.
func TestEnumerateAccelDevices_UnreadableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withAccelRoot(t, root)

	if _, err := enumerateAccelDevices(); err == nil {
		t.Error("expected an error reading a non-directory accel root")
	}
}

// TestNPUCollect_IntelVPU verifies a complete Intel accelerator is reported
// from generic sysfs plus the driver attribute table.
func TestNPUCollect_IntelVPU(t *testing.T) {
	root := t.TempDir()
	makeAccelDevice(t, root, "accel0", map[string]string{
		"vendor":                       "0x8086",
		"device":                       "0xad1d",
		"class":                        "0x120000",
		"label":                        "Onboard - Other",
		"local_cpulist":                "0-23",
		"irq":                          "315",
		"numa_node":                    "-1",
		"power_state":                  "D0",
		"uevent":                       "DRIVER=intel_vpu\nPCI_SLOT_NAME=0000:00:0b.0",
		"current_link_speed":           "Unknown",
		"current_link_width":           "0",
		"max_link_speed":               "Unknown",
		"max_link_width":               "255",
		"npu_busy_time_us":             "4113461775",
		"npu_current_frequency_mhz":    "1400",
		"npu_max_frequency_mhz":        "1600",
		"npu_memory_utilization":       "68260425728",
		"sched_mode":                   "HW",
		"power/runtime_status":         "active",
		"power/runtime_active_time":    "1000",
		"power/runtime_suspended_time": "9000",
		"power/autosuspend_delay_ms":   "10",
		"power/runtime_usage":          "1",
	})
	withAccelRoot(t, root)
	withProcRoot(t, t.TempDir())
	withPCIIDs(t, writePCIIDs(t))

	c := newTestNPUCollector(t)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	npus := c.Info()
	if len(npus) != 1 {
		t.Fatalf("expected 1 accelerator, got %d", len(npus))
	}
	n := npus[0]

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"Device", n.Device, "accel0"},
		{"Driver", n.Driver, "intel_vpu"},
		{"Vendor", n.Vendor, "Intel Corporation"},
		{"Name", n.Name, "Arrow Lake NPU"},
		{"DeviceClass", n.DeviceClass, "Processing accelerators"},
		{"PCIBusID", n.PCIBusID, "0000:00:0b.0"},
		{"LocalCPUList", n.LocalCPUList, "0-23"},
		{"IRQ", n.IRQ, 315},
		{"NumaNode", n.NumaNode, -1},
		{"State", n.State, "active"},
		{"PowerState", n.PowerState, "D0"},
		{"BusyTimeUs", n.BusyTimeUs, uint64(4113461775)},
		{"MemoryUtilizationBytes", n.MemoryUtilizationBytes, uint64(68260425728)},
		{"SchedMode", n.SchedMode, "HW"},
		{"FrequencyMHz", n.FrequencyMHz, uint32(1400)},
		{"FrequencyMaxMHz", n.FrequencyMaxMHz, uint32(1600)},
		{"CountersStale", n.CountersStale, false},
		// An integrated accelerator is not on a real PCIe link: 255 is the
		// specification's "unknown" encoding and must not surface as lanes.
		{"PCIeWidthMax", n.PCIeWidthMax, 0},
		{"AutosuspendDelayMs", n.AutosuspendDelayMs, int64(10)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	// The first collection has no interval to measure, so the instantaneous
	// power state stands in for residency.
	if n.ResidencyPercent != 100 {
		t.Errorf("ResidencyPercent = %v, want 100 for an active device on the first sample", n.ResidencyPercent)
	}
	if n.Engines == nil || n.Clients == nil {
		t.Error("Engines and Clients must be empty slices, not nil, so the wire shape is stable")
	}
}

// TestNPUCollect_SuspendedDeviceIsLeftAsleep verifies the collector does not
// read the driver attributes of a suspended accelerator, and instead carries
// the previous values forward labelled with their age.
//
// This is the whole point of the design: reading intel_vpu's attributes resumes
// the device, so a monitor that polls them keeps the NPU powered up forever.
func TestNPUCollect_SuspendedDeviceIsLeftAsleep(t *testing.T) {
	root := t.TempDir()
	dev := makeAccelDevice(t, root, "accel0", map[string]string{
		"vendor":                       "0x8086",
		"device":                       "0xad1d",
		"uevent":                       "DRIVER=intel_vpu",
		"npu_busy_time_us":             "500",
		"npu_current_frequency_mhz":    "1400",
		"power/runtime_status":         "active",
		"power/runtime_active_time":    "1000",
		"power/runtime_suspended_time": "1000",
	})
	withAccelRoot(t, root)
	withProcRoot(t, t.TempDir())
	withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))

	c := newTestNPUCollector(t)
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	if got := c.Info()[0].BusyTimeUs; got != 500 {
		t.Fatalf("BusyTimeUs = %d, want 500 while awake", got)
	}

	// The device goes to sleep, and its counters move on underneath us. A
	// collector that reads them anyway would both wake the device and report
	// the new value; this one must report neither.
	writeSysfs(t, filepath.Join(dev, "power/runtime_status"), "suspended")
	writeSysfs(t, filepath.Join(dev, "npu_busy_time_us"), "999999")
	writeSysfs(t, filepath.Join(dev, "npu_current_frequency_mhz"), "0")

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	n := c.Info()[0]
	if n.BusyTimeUs != 500 {
		t.Errorf("BusyTimeUs = %d, want the cached 500: a suspended device must not be read", n.BusyTimeUs)
	}
	if n.FrequencyMHz != 1400 {
		t.Errorf("FrequencyMHz = %d, want the cached 1400", n.FrequencyMHz)
	}
	if !n.CountersStale {
		t.Error("CountersStale = false, want true: the figures are carried over, and saying so is the point")
	}
	if n.State != "suspended" {
		t.Errorf("State = %q, want %q", n.State, "suspended")
	}
}

// TestNPUCollect_NoDevices verifies a host with no accelerators reports an
// empty slice without error, so every consumer can iterate unconditionally.
func TestNPUCollect_NoDevices(t *testing.T) {
	withAccelRoot(t, t.TempDir())
	withProcRoot(t, t.TempDir())

	c := newTestNPUCollector(t)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if npus := c.Info(); npus == nil || len(npus) != 0 {
		t.Errorf("expected an empty non-nil slice, got %#v", npus)
	}
}

// TestNPUCollect_UnknownDriverStillReported verifies an accelerator whose
// driver has no attribute table is still reported from generic sysfs. This is
// the property that makes the collector work on hardware it has never seen.
func TestNPUCollect_UnknownDriverStillReported(t *testing.T) {
	root := t.TempDir()
	makeAccelDevice(t, root, "accel0", map[string]string{
		"vendor":                    "0x17cb",
		"device":                    "0xa100",
		"uevent":                    "DRIVER=qaic",
		"power/runtime_status":      "active",
		"power/runtime_active_time": "5000",
		"current_link_speed":        "16.0 GT/s PCIe",
		"current_link_width":        "8",
	})
	withAccelRoot(t, root)
	withProcRoot(t, t.TempDir())
	withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))

	c := newTestNPUCollector(t)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	n := c.Info()[0]
	if n.Driver != "qaic" {
		t.Errorf("Driver = %q, want %q", n.Driver, "qaic")
	}
	if n.Vendor != "Qualcomm Inc" {
		t.Errorf("Vendor = %q, want the built-in fallback %q", n.Vendor, "Qualcomm Inc")
	}
	if n.State != "active" || n.ResidencyPercent != 100 {
		t.Errorf("State = %q, ResidencyPercent = %v; generic metrics must work without a driver table", n.State, n.ResidencyPercent)
	}
	if n.PCIeWidthCurrent != 8 || n.PCIeGenCurrent != 4 {
		t.Errorf("PCIe = gen%d x%d, want gen4 x8", n.PCIeGenCurrent, n.PCIeWidthCurrent)
	}
	if n.BusyTimeUs != 0 {
		t.Errorf("BusyTimeUs = %d, want 0: no attribute table means no counters, not invented ones", n.BusyTimeUs)
	}
}

// TestNPUResidency_FromCounterDeltas verifies residency uses the device's own
// runtime-PM accounting as the denominator.
func TestNPUResidency_FromCounterDeltas(t *testing.T) {
	last := npuSample{activeMs: 1000, suspendedMs: 4000}
	got := npuResidency("active", 1300, 4700, last, true)
	if want := 30.0; got != want {
		t.Errorf("npuResidency = %v, want %v (300ms awake of a 1000ms interval)", got, want)
	}
}

// TestNPUResidency_DegradedInputs verifies the cases with no usable interval:
// a first sample, a counter that went backwards, and a device with no runtime
// PM at all.
func TestNPUResidency_DegradedInputs(t *testing.T) {
	cases := []struct {
		name        string
		state       string
		activeMs    uint64
		suspendedMs uint64
		last        npuSample
		hasLast     bool
		want        float64
	}{
		{"first sample active", "active", 10, 20, npuSample{}, false, 100},
		{"first sample suspended", "suspended", 10, 20, npuSample{}, false, 0},
		{"no runtime pm", "", 0, 0, npuSample{}, false, 0},
		{"unsupported runtime pm", "unsupported", 0, 0, npuSample{}, true, 0},
		// A device that was removed and re-added restarts its counters. That
		// must read as no information, never as a negative rate.
		{"counters reset", "active", 5, 5, npuSample{activeMs: 900, suspendedMs: 900}, true, 100},
		// Counters that did not move at all leave no interval to divide by, so
		// the instantaneous state stands in.
		{"idle interval", "suspended", 100, 100, npuSample{activeMs: 100, suspendedMs: 100}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := npuResidency(tc.state, tc.activeMs, tc.suspendedMs, tc.last, tc.hasLast)
			if got != tc.want {
				t.Errorf("npuResidency = %v, want %v", got, tc.want)
			}
			if got < 0 || got > 100 {
				t.Errorf("npuResidency = %v, outside 0-100", got)
			}
		})
	}
}

// TestPCIeLinkWidth verifies lane counts pass through and the specification's
// "unknown" encoding does not.
func TestPCIeLinkWidth(t *testing.T) {
	cases := map[uint64]int{0: 0, 1: 1, 8: 8, 16: 16, 255: 0, 4096: 0}
	for raw, want := range cases {
		if got := pcieLinkWidth(raw); got != want {
			t.Errorf("pcieLinkWidth(%d) = %d, want %d", raw, got, want)
		}
	}
}

// TestNPUIdentity_PCIDatabase verifies model and class names resolve from the
// hwdata database, which is what lets an unknown accelerator still be named.
func TestNPUIdentity_PCIDatabase(t *testing.T) {
	withPCIIDs(t, writePCIIDs(t))
	dev := t.TempDir()
	writeSysfs(t, filepath.Join(dev, "class"), "0x120000")

	vendor, name, class := npuIdentity(dev, "0x8086", "0xad1d", "intel_vpu", "Onboard - Other")
	if vendor != "Intel Corporation" {
		t.Errorf("vendor = %q, want %q", vendor, "Intel Corporation")
	}
	if name != "Arrow Lake NPU" {
		t.Errorf("name = %q, want %q", name, "Arrow Lake NPU")
	}
	if class != "Processing accelerators" {
		t.Errorf("class = %q, want %q", class, "Processing accelerators")
	}
}

// TestNPUIdentity_WithoutDatabase verifies naming degrades in useful steps when
// the identifier database is absent or the device is unknown to it.
func TestNPUIdentity_WithoutDatabase(t *testing.T) {
	withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))
	dev := t.TempDir()

	cases := []struct {
		name       string
		vendorID   string
		deviceID   string
		driver     string
		label      string
		wantVendor string
		wantName   string
	}{
		{"known vendor", "0x8086", "0xffff", "intel_vpu", "", "Intel Corporation", "Intel Corporation accelerator"},
		// Firmware writes "Onboard - Other" when it has nothing to say, and
		// naming a device after it tells the reader nothing.
		{"useless label ignored", "0x1da3", "0xffff", "habanalabs", "Onboard - Other", "Habana Labs Ltd.", "Habana Labs Ltd. accelerator"},
		{"useful label kept", "0xffff", "0xffff", "rocket", "NPU 0", "", "NPU 0"},
		{"driver as last resort", "", "", "rocket", "", "", "rocket"},
		{"nothing at all", "", "", "", "", "", "accelerator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vendor, name, class := npuIdentity(dev, tc.vendorID, tc.deviceID, tc.driver, tc.label)
			if vendor != tc.wantVendor {
				t.Errorf("vendor = %q, want %q", vendor, tc.wantVendor)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if class == "" {
				t.Error("class must never be empty")
			}
		})
	}
}

// TestNPUEngineRates_FromDeltas verifies engine busy time becomes a percentage
// of the sampling interval and that the busiest engine sets device utilisation.
func TestNPUEngineRates_FromDeltas(t *testing.T) {
	cur := map[string]uint64{"npu": 700_000_000, "copy": 100_000_000}
	prev := map[string]uint64{"npu": 200_000_000, "copy": 50_000_000}

	engines, util := npuEngineRates(cur, prev, true, time.Second)
	if len(engines) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(engines))
	}
	// Sorted by name, so "copy" precedes "npu".
	if engines[0].Name != "copy" || engines[1].Name != "npu" {
		t.Errorf("engines not sorted by name: %v, %v", engines[0].Name, engines[1].Name)
	}
	if got, want := engines[1].BusyPercent, 50.0; got != want {
		t.Errorf("npu BusyPercent = %v, want %v", got, want)
	}
	if got, want := engines[0].BusyPercent, 5.0; got != want {
		t.Errorf("copy BusyPercent = %v, want %v", got, want)
	}
	if util != 50 {
		t.Errorf("util = %v, want 50 (the busiest engine)", util)
	}
}

// TestNPUEngineRates_DegradedInputs verifies the rate never goes negative when
// a client exits and never exceeds a full interval.
func TestNPUEngineRates_DegradedInputs(t *testing.T) {
	// A client that exits takes its accumulated time with it, so the total
	// falls. That must read as no activity, not as a negative rate.
	engines, util := npuEngineRates(map[string]uint64{"npu": 10}, map[string]uint64{"npu": 900}, true, time.Second)
	if engines[0].BusyPercent != 0 || util != 0 {
		t.Errorf("a falling total gave %v%%, want 0", engines[0].BusyPercent)
	}

	// A client that appears mid-interval brings its whole history with it.
	engines, util = npuEngineRates(map[string]uint64{"npu": 9_000_000_000}, nil, true, time.Second)
	if engines[0].BusyPercent != 100 || util != 100 {
		t.Errorf("an oversized delta gave %v%%, want it clamped to 100", engines[0].BusyPercent)
	}

	// No previous sample and no interval: report the counter, claim no rate.
	engines, util = npuEngineRates(map[string]uint64{"npu": 500}, nil, false, 0)
	if engines[0].BusyPercent != 0 || util != 0 {
		t.Errorf("first sample gave %v%%, want 0", engines[0].BusyPercent)
	}
	if engines[0].BusyNs != 500 {
		t.Errorf("BusyNs = %d, want the raw counter 500", engines[0].BusyNs)
	}

	if engines, util := npuEngineRates(nil, nil, true, time.Second); len(engines) != 0 || util != 0 {
		t.Errorf("no engines gave %v, %v", engines, util)
	} else if engines == nil {
		t.Error("expected an empty slice rather than nil, so the wire shape is stable")
	}
}

// TestNPUClients_RateComesFromTheDelta covers the arithmetic the fdinfo values
// require: they are cumulative for the life of the file descriptor, so a rate is
// a difference between samples and never the total divided by one interval.
func TestNPUClients_RateComesFromTheDelta(t *testing.T) {
	// A client with a long history that did 500 ms of work in this 1 s window.
	samples := []npuClientSample{{
		pid: 20, name: "busy", key: "7",
		engines: map[string]uint64{"npu": 90_400_000_000, "copy": 100_000_000},
	}}
	prev := map[string]uint64{"7": 90_000_000_000}

	clients, totals := npuClients(samples, prev, true, time.Second)
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if got, want := clients[0].BusyPercent, 50.0; got != want {
		t.Errorf("BusyPercent = %v, want %v -- the lifetime total must not be the numerator", got, want)
	}
	// The counter itself stays raw, summed across engines.
	if got, want := clients[0].BusyNs, uint64(90_500_000_000); got != want {
		t.Errorf("BusyNs = %d, want the cumulative %d", got, want)
	}
	if got, want := totals["7"], uint64(90_500_000_000); got != want {
		t.Errorf("returned total = %d, want %d for the next sample to difference against", got, want)
	}
}

// TestNPUClients_IdleClientReportsZero is the defect this arithmetic fixes: a
// client that has done a great deal of work historically and nothing in this
// window must read 0%, not 100%. Dividing the lifetime total by the interval
// pinned every such client at 100% on every sample forever.
func TestNPUClients_IdleClientReportsZero(t *testing.T) {
	samples := []npuClientSample{{
		pid: 1240079, name: "ovms", key: "7",
		engines: map[string]uint64{"npu": 90_000_000_000},
	}}
	prev := map[string]uint64{"7": 90_000_000_000} // did not advance

	clients, _ := npuClients(samples, prev, true, time.Second)
	if clients[0].BusyPercent != 0 {
		t.Errorf("BusyPercent = %v, want 0 for a client that did no work in the window",
			clients[0].BusyPercent)
	}
}

// TestNPUClients_OrdersByBusiestNow verifies the list leads with the process
// responsible for CURRENT load. Ordering by the cumulative total put a
// long-running idle client above the one actually working.
func TestNPUClients_OrdersByBusiestNow(t *testing.T) {
	samples := []npuClientSample{
		// Huge history, idle now.
		{pid: 10, name: "was-busy", key: "a", engines: map[string]uint64{"npu": 90_000_000_000}},
		// Small history, working now.
		{pid: 20, name: "busy-now", key: "b", engines: map[string]uint64{"npu": 700_000_000}},
	}
	prev := map[string]uint64{"a": 90_000_000_000, "b": 300_000_000}

	clients, _ := npuClients(samples, prev, true, time.Second)
	if clients[0].Name != "busy-now" {
		t.Errorf("first client = %q, want the one busy now; order was %q then %q",
			clients[0].Name, clients[0].Name, clients[1].Name)
	}
	if got, want := clients[0].BusyPercent, 40.0; got != want {
		t.Errorf("BusyPercent = %v, want %v", got, want)
	}
	if clients[1].BusyPercent != 0 {
		t.Errorf("idle client BusyPercent = %v, want 0", clients[1].BusyPercent)
	}
}

// TestNPUClients_DegradedInputs covers every case with no usable rate: no
// clients, no previous sample, a client seen for the first time, no interval,
// and a total that went backwards.
func TestNPUClients_DegradedInputs(t *testing.T) {
	sample := []npuClientSample{{pid: 1, key: "x", engines: map[string]uint64{"npu": 5_000_000_000}}}

	t.Run("no clients", func(t *testing.T) {
		got, totals := npuClients(nil, nil, true, time.Second)
		if got == nil || len(got) != 0 {
			t.Errorf("npuClients(nil) = %#v, want an empty non-nil slice", got)
		}
		if totals != nil {
			t.Errorf("totals = %v, want nil", totals)
		}
	})

	t.Run("first sample of the run", func(t *testing.T) {
		got, _ := npuClients(sample, nil, false, time.Second)
		if got[0].BusyPercent != 0 {
			t.Errorf("BusyPercent = %v, want 0 with no previous sample", got[0].BusyPercent)
		}
		if got[0].BusyNs != 5_000_000_000 {
			t.Errorf("BusyNs = %d, want the raw counter reported anyway", got[0].BusyNs)
		}
	})

	t.Run("client seen for the first time", func(t *testing.T) {
		// A previous sample exists, but not for this client: its rate over a
		// window we did not observe it in is not knowable.
		got, _ := npuClients(sample, map[string]uint64{"someone-else": 1}, true, time.Second)
		if got[0].BusyPercent != 0 {
			t.Errorf("BusyPercent = %v, want 0 for a newly appeared client", got[0].BusyPercent)
		}
	})

	t.Run("zero interval", func(t *testing.T) {
		got, _ := npuClients(sample, map[string]uint64{"x": 0}, true, 0)
		if got[0].BusyPercent != 0 {
			t.Errorf("BusyPercent = %v, want 0 with no interval to divide by", got[0].BusyPercent)
		}
	})

	t.Run("total went backwards", func(t *testing.T) {
		// A driver reusing a client id can produce this; it is not a negative rate.
		got, _ := npuClients(sample, map[string]uint64{"x": 9_000_000_000}, true, time.Second)
		if got[0].BusyPercent != 0 {
			t.Errorf("BusyPercent = %v, want 0 when the total falls", got[0].BusyPercent)
		}
	})

	t.Run("delta larger than the interval is clamped", func(t *testing.T) {
		// Several engines busy in parallel can exceed wall time on one device.
		got, _ := npuClients(
			[]npuClientSample{{pid: 1, key: "x", engines: map[string]uint64{"a": 3_000_000_000, "b": 3_000_000_000}}},
			map[string]uint64{"x": 0}, true, time.Second)
		if got[0].BusyPercent != 100 {
			t.Errorf("BusyPercent = %v, want it clamped to 100", got[0].BusyPercent)
		}
	})
}

// TestReadAccelFdinfo_UsageStats verifies the standard DRM usage-stats keys are
// parsed, including the unit suffixes the interface permits.
func TestReadAccelFdinfo_UsageStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fdinfo")
	if err := os.WriteFile(path, []byte(
		"pos:\t0\n"+
			"drm-driver:\tintel_vpu\n"+
			"drm-pdev:\t0000:00:0b.0\n"+
			"drm-client-id:\t42\n"+
			"drm-engine-npu:\t1234567 ns\n"+
			"drm-engine-copy:\t89 ns\n"+
			"drm-total-memory:\t2048 KiB\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sample, pdev, ok := readAccelFdinfo(path)
	if !ok {
		t.Fatal("readAccelFdinfo reported failure on a valid file")
	}
	if pdev != "0000:00:0b.0" {
		t.Errorf("pdev = %q, want %q", pdev, "0000:00:0b.0")
	}
	if sample.clientID != "42" {
		t.Errorf("clientID = %q, want %q", sample.clientID, "42")
	}
	if got, want := sample.engines["npu"], uint64(1234567); got != want {
		t.Errorf("engines[npu] = %d, want %d", got, want)
	}
	if got, want := sample.memoryBytes, uint64(2048*1024); got != want {
		t.Errorf("memoryBytes = %d, want %d", got, want)
	}
}

// TestReadAccelFdinfo_BadInput verifies an unreadable file is reported as a
// failure and that malformed values contribute nothing rather than garbage.
func TestReadAccelFdinfo_BadInput(t *testing.T) {
	if _, _, ok := readAccelFdinfo(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("expected failure reading a missing fdinfo file")
	}

	path := filepath.Join(t.TempDir(), "fdinfo")
	if err := os.WriteFile(path, []byte(
		"garbage without a colon\n"+
			"drm-engine-npu:\tnot-a-number ns\n"+
			"drm-total-memory:\t\n"+
			"drm-total-vram:\t3 MiB\n"+
			"drm-total-gtt:\t1 MiB\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	sample, pdev, ok := readAccelFdinfo(path)
	if !ok {
		t.Fatal("a readable file with bad values is still readable")
	}
	if pdev != "" {
		t.Errorf("pdev = %q, want empty", pdev)
	}
	if sample.engines["npu"] != 0 {
		t.Errorf("engines[npu] = %d, want 0 for an unparseable value", sample.engines["npu"])
	}
	// With no drm-total-memory, the per-region totals stand in for it.
	if got, want := sample.memoryBytes, uint64(4*1024*1024); got != want {
		t.Errorf("memoryBytes = %d, want %d (summed regions)", got, want)
	}
}

// TestParseFdinfoBytes verifies each unit the usage-stats interface permits,
// and that an unparseable value contributes zero.
func TestParseFdinfoBytes(t *testing.T) {
	cases := map[string]uint64{
		"1024":      1024,
		"2 KiB":     2048,
		"3 MiB":     3 * 1024 * 1024,
		"1 GiB":     1024 * 1024 * 1024,
		"12 ns":     12,
		"":          0,
		"abc":       0,
		"-5":        0,
		"7 parsecs": 7,
	}
	for in, want := range cases {
		if got := parseFdinfoBytes(in); got != want {
			t.Errorf("parseFdinfoBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestCollectAccelClients_FindsOpenDevices verifies a process holding an
// accelerator open is attributed its usage, and that several file descriptors
// belonging to one client are counted once.
func TestCollectAccelClients_FindsOpenDevices(t *testing.T) {
	proc := t.TempDir()
	pidDir := filepath.Join(proc, "1234")
	fdinfo := "drm-pdev:\t0000:00:0b.0\ndrm-client-id:\t7\ndrm-engine-npu:\t250000000 ns\n"
	for _, fd := range []string{"3", "4"} {
		if err := os.MkdirAll(filepath.Join(pidDir, "fd"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// A dangling symlink is exactly what /proc/pid/fd holds for a device
		// node this test does not have.
		if err := os.Symlink("/dev/accel/accel0", filepath.Join(pidDir, "fd", fd)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		writeSysfs(t, filepath.Join(pidDir, "fdinfo", fd), fdinfo)
	}
	writeSysfs(t, filepath.Join(pidDir, "comm"), "inference")

	// A process holding something that is not an accelerator must be ignored.
	other := filepath.Join(proc, "5678", "fd")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("/dev/dri/renderD128", filepath.Join(other, "3")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A non-numeric /proc entry is not a process.
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withProcRoot(t, proc)

	c := newTestNPUCollector(t)
	byDevice := c.collectAccelClients()

	for _, key := range []string{"accel0", "0000:00:0b.0"} {
		samples := byDevice[key]
		if len(samples) != 1 {
			t.Fatalf("byDevice[%q] has %d samples, want 1: two descriptors of one client are one client", key, len(samples))
		}
		if samples[0].pid != 1234 || samples[0].name != "inference" {
			t.Errorf("sample = pid %d %q, want pid 1234 \"inference\"", samples[0].pid, samples[0].name)
		}
		if got, want := samples[0].engines["npu"], uint64(250000000); got != want {
			t.Errorf("engines[npu] = %d, want %d", got, want)
		}
	}
}

// TestCollectAccelClients_UnreadableProc verifies an unreadable process table
// yields no clients rather than a failure, because the device's own metrics do
// not depend on it.
func TestCollectAccelClients_UnreadableProc(t *testing.T) {
	withProcRoot(t, filepath.Join(t.TempDir(), "absent"))

	c := newTestNPUCollector(t)
	if got := c.collectAccelClients(); len(got) != 0 {
		t.Errorf("expected no clients, got %v", got)
	}
}

// TestNPUDevfreq verifies SoC-style clock reporting, which needs no driver
// table and no awake device.
func TestNPUDevfreq(t *testing.T) {
	dev := t.TempDir()
	node := filepath.Join(dev, "devfreq", "fdab0000.npu")
	writeSysfs(t, filepath.Join(node, "cur_freq"), "800000000")
	writeSysfs(t, filepath.Join(node, "max_freq"), "1000000000")
	writeSysfs(t, filepath.Join(node, "governor"), "userspace")

	cur, max, governor := npuDevfreq(dev)
	if cur != 800 || max != 1000 {
		t.Errorf("devfreq = %d/%d MHz, want 800/1000", cur, max)
	}
	if governor != "userspace" {
		t.Errorf("governor = %q, want %q", governor, "userspace")
	}
}

// TestNPUDevfreq_Absent verifies a device without devfreq reports nothing
// rather than failing.
func TestNPUDevfreq_Absent(t *testing.T) {
	cur, max, governor := npuDevfreq(t.TempDir())
	if cur != 0 || max != 0 || governor != "" {
		t.Errorf("expected zero values, got %d/%d %q", cur, max, governor)
	}
}

// TestReadIntelVPUCounters verifies the Intel attribute table, and that a
// device missing those attributes yields zeros rather than failing.
func TestReadIntelVPUCounters(t *testing.T) {
	dev := t.TempDir()
	writeSysfs(t, filepath.Join(dev, "npu_busy_time_us"), "4113461775")
	writeSysfs(t, filepath.Join(dev, "npu_current_frequency_mhz"), "1600")
	writeSysfs(t, filepath.Join(dev, "npu_max_frequency_mhz"), "1600")
	writeSysfs(t, filepath.Join(dev, "npu_memory_utilization"), "68260425728")
	writeSysfs(t, filepath.Join(dev, "sched_mode"), "HW")

	got := readIntelVPUCounters(dev)
	if got.busyTimeUs != 4113461775 || got.freqMHz != 1600 || got.freqMaxMHz != 1600 {
		t.Errorf("counters = %+v, want the written values", got)
	}
	if got.memoryBytes != 68260425728 || got.schedMode != "HW" {
		t.Errorf("counters = %+v, want the written values", got)
	}
	if !got.read {
		t.Error("read = false, want true")
	}

	empty := readIntelVPUCounters(t.TempDir())
	if empty.busyTimeUs != 0 || empty.freqMHz != 0 || empty.schedMode != "" {
		t.Errorf("counters = %+v, want zeros for a device with no attributes", empty)
	}
}

// TestLookupPCINames_MissingDatabase verifies naming falls through cleanly when
// no identifier database is installed, which is common on minimal images.
func TestLookupPCINames_MissingDatabase(t *testing.T) {
	withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))

	vendor, device, class := lookupPCINames("0x8086", "0xad1d", "0x120000")
	if vendor != "" || device != "" || class != "" {
		t.Errorf("got %q/%q/%q, want empty strings", vendor, device, class)
	}
}

// TestLookupPCINames_IgnoresSubsystemEntries verifies the parser does not
// mistake a subsystem line for a device, which would misname hardware.
func TestLookupPCINames_IgnoresSubsystemEntries(t *testing.T) {
	withPCIIDs(t, writePCIIDs(t))

	if _, device, _ := lookupPCINames("0x8086", "0x7e21", ""); device != "" {
		t.Errorf("device = %q, want empty: 7e21 is a subsystem ID, not a device", device)
	}
	if _, device, _ := lookupPCINames("0x1002", "0xad1d", ""); device != "" {
		t.Errorf("device = %q, want empty: ad1d belongs to vendor 8086, not 1002", device)
	}
}

// writePCIIDs writes a miniature identifier database in the real file's format,
// including the tab-indented device and doubly-indented subsystem lines that
// the parser must tell apart.
func writePCIIDs(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pci.ids")
	body := "# comment line\n" +
		"1002  Advanced Micro Devices, Inc. [AMD/ATI]\n" +
		"\t1681  Rembrandt\n" +
		"8086  Intel Corporation\n" +
		"\tad1d  Arrow Lake NPU\n" +
		"\t\t1462 7e21  Onboard NPU\n" +
		"\tad03  Signal processing controller\n" +
		"C 03  Display controller\n" +
		"\t00  VGA compatible controller\n" +
		"C 12  Processing accelerators\n" +
		"\t00  Processing accelerators\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pci.ids: %v", err)
	}
	return path
}

// TestNPUInfoWireShape guards the field every consumer keys on. The dashboard,
// the CLI, the applet and the snapshot differ all identify an accelerator by
// Index, and the differ's arrayKeys entry for "npus" names it explicitly.
func TestNPUInfoWireShape(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"accel0", "accel1"} {
		makeAccelDevice(t, root, name, map[string]string{
			"uevent":               "DRIVER=intel_vpu",
			"power/runtime_status": "suspended",
		})
	}
	withAccelRoot(t, root)
	withProcRoot(t, t.TempDir())
	withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))

	c := newTestNPUCollector(t)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	npus := c.Info()
	if len(npus) != 2 {
		t.Fatalf("expected 2 accelerators, got %d", len(npus))
	}
	seen := make(map[int]struct{}, len(npus))
	for _, n := range npus {
		if _, dup := seen[n.Index]; dup {
			t.Errorf("duplicate Index %d: the snapshot differ would merge these two devices", n.Index)
		}
		seen[n.Index] = struct{}{}
	}
	if npus[0].Index != 0 || npus[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1 in enumeration order", npus[0].Index, npus[1].Index)
	}
}

// TestNPUEngineTotals_SumsAcrossClients verifies an engine's busy time is the
// sum of every client using it, which is what makes a device-level utilisation
// figure possible from per-client accounting.
func TestNPUEngineTotals_SumsAcrossClients(t *testing.T) {
	samples := []npuClientSample{
		{pid: 1, engines: map[string]uint64{"npu": 100, "copy": 5}},
		{pid: 2, engines: map[string]uint64{"npu": 300}},
		{pid: 3, engines: nil},
	}
	totals := npuEngineTotals(samples)
	if got, want := totals["npu"], uint64(400); got != want {
		t.Errorf("totals[npu] = %d, want %d", got, want)
	}
	if got, want := totals["copy"], uint64(5); got != want {
		t.Errorf("totals[copy] = %d, want %d", got, want)
	}
	if len(totals) != 2 {
		t.Errorf("totals has %d engines, want 2", len(totals))
	}
}

// TestNPUEngineTotals_NoClients verifies no clients yields no engines, so the
// caller reports an idle device rather than inventing one.
func TestNPUEngineTotals_NoClients(t *testing.T) {
	if got := npuEngineTotals(nil); got != nil {
		t.Errorf("npuEngineTotals(nil) = %v, want nil", got)
	}
	if got := npuEngineTotals([]npuClientSample{}); got != nil {
		t.Errorf("npuEngineTotals(empty) = %v, want nil", got)
	}
}

// TestClampPercent verifies the bound applied to every computed rate. The
// negative case is defensive -- safeSub should make it unreachable -- and is
// tested so it stays a clamp rather than becoming a negative percentage if a
// caller ever subtracts without it.
func TestClampPercent(t *testing.T) {
	cases := map[float64]float64{-1: 0, -0.5: 0, 0: 0, 42.5: 42.5, 100: 100, 100.1: 100, 1e9: 100}
	for in, want := range cases {
		if got := clampPercent(in); got != want {
			t.Errorf("clampPercent(%v) = %v, want %v", in, got, want)
		}
	}
}

// TestSplitPCIEntry_RejectsNonHexIdentifiers verifies the database parser will
// not accept a line whose first field is not a hex identifier, which would
// otherwise attach a stray name to a real device ID.
func TestSplitPCIEntry_RejectsNonHexIdentifiers(t *testing.T) {
	if id, name, ok := splitPCIEntry("ad1d  Arrow Lake NPU"); !ok || id != "ad1d" || name != "Arrow Lake NPU" {
		t.Errorf("splitPCIEntry of a valid line gave %q/%q/%v", id, name, ok)
	}
	for _, line := range []string{"", "nothex  Some Device", "onlyonefield", "  ", "zzzz  Bad"} {
		if _, _, ok := splitPCIEntry(line); ok {
			t.Errorf("splitPCIEntry(%q) accepted a line it should reject", line)
		}
	}
}

// TestUeventValue verifies the uevent fallback used to identify a driver when
// no driver symlink is present, and that a missing key reads as absent.
func TestUeventValue(t *testing.T) {
	dev := t.TempDir()
	writeSysfs(t, filepath.Join(dev, "uevent"), "DRIVER=intel_vpu\nPCI_CLASS=120000\nPCI_ID=8086:AD1D")

	if got := ueventValue(dev, "DRIVER"); got != "intel_vpu" {
		t.Errorf("DRIVER = %q, want %q", got, "intel_vpu")
	}
	if got := ueventValue(dev, "PCI_ID"); got != "8086:AD1D" {
		t.Errorf("PCI_ID = %q, want %q", got, "8086:AD1D")
	}
	if got := ueventValue(dev, "MODALIAS"); got != "" {
		t.Errorf("absent key = %q, want empty", got)
	}
	if got := ueventValue(t.TempDir(), "DRIVER"); got != "" {
		t.Errorf("missing uevent file = %q, want empty", got)
	}
}

// TestNPUCollect_UnreadableAccelClass verifies an accel class that cannot be
// read leaves the collector reporting no accelerators rather than failing the
// whole snapshot. One broken subsystem must not cost the others their data.
func TestNPUCollect_UnreadableAccelClass(t *testing.T) {
	root := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withAccelRoot(t, root)
	withProcRoot(t, t.TempDir())

	c := newTestNPUCollector(t)
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect must absorb an unreadable accel class, got %v", err)
	}
	if npus := c.Info(); npus == nil || len(npus) != 0 {
		t.Errorf("Info() = %#v, want an empty non-nil slice", npus)
	}
}

// TestAccelDevicePath verifies the class entry resolves to the bus device
// directory, and that an unresolvable path still yields something to read from
// rather than an empty string that would silently read the wrong tree.
func TestAccelDevicePath(t *testing.T) {
	root := t.TempDir()
	// The real tree links the class entry's "device" child at the parent bus
	// device; a plain directory, as tests build, resolves to itself.
	realDev := filepath.Join(root, "devices", "0000:00:0b.0")
	if err := os.MkdirAll(realDev, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "class", "accel0"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(realDev, filepath.Join(root, "class", "accel0", "device")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	withAccelRoot(t, filepath.Join(root, "class"))

	got, err := filepath.EvalSymlinks(realDev)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if resolved := accelDevicePath("accel0"); resolved != got {
		t.Errorf("accelDevicePath = %q, want %q", resolved, got)
	}

	// A device that no longer exists leaves the unresolved path, whose reads
	// simply return nothing.
	if resolved := accelDevicePath("accel9"); resolved != filepath.Join(root, "class", "accel9", "device") {
		t.Errorf("accelDevicePath of a missing device = %q, want the unresolved path", resolved)
	}
}

// TestProcessComm verifies a process name is read and that a process which
// exited mid-walk contributes no name rather than failing the scan.
func TestProcessComm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comm")
	if err := os.WriteFile(path, []byte("inference\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := processComm(path); got != "inference" {
		t.Errorf("processComm = %q, want %q", got, "inference")
	}
	if got := processComm(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("processComm of a missing file = %q, want empty", got)
	}
}

// TestLookupPCINames_ReadStoppedEarly verifies that a pci.ids read which fails
// partway keeps what it parsed, leaves what it never reached unresolved, and
// says so in the log rather than presenting a truncated database as complete.
// A line longer than the scanner's buffer is what stops it.
func TestLookupPCINames_ReadStoppedEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pci.ids")
	body := "8086  Intel Corporation\n" +
		"\t" + strings.Repeat("x", bufio.MaxScanTokenSize) + "\n" +
		"1002  Advanced Micro Devices, Inc. [AMD/ATI]\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pci.ids: %v", err)
	}
	withPCIIDs(t, path)

	var logs bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	if vendor, _, _ := lookupPCINames("0x8086", "", ""); vendor != "Intel Corporation" {
		t.Errorf("vendor before the failure = %q, want %q", vendor, "Intel Corporation")
	}
	if vendor, _, _ := lookupPCINames("0x1002", "", ""); vendor != "" {
		t.Errorf("vendor past the failure = %q, want unresolved", vendor)
	}
	if !strings.Contains(logs.String(), "pci.ids read stopped early") || !strings.Contains(logs.String(), path) {
		t.Errorf("expected a warning naming %s, got log %q", path, logs.String())
	}
}
