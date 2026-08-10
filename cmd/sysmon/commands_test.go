//go:build !desktop

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- test fixtures --------------------------------------------------------

// fixtureSnapshot returns a snapshot with one entry in every section, so each
// command has something to render without touching real hardware.
func fixtureSnapshot() *types.Snapshot {
	return &types.Snapshot{
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Host: types.HostInfo{
			Hostname:      "fixture-host",
			OS:            "linux",
			Platform:      "testdist",
			KernelVersion: "6.0.0-test",
			KernelArch:    "x86_64",
			Uptime:        3600,
		},
		CPUSummary: types.CPUSummary{
			Sockets: 1, CoresPerSocket: 2, ThreadsPerCore: 1,
			TotalCores: 2, TotalThreads: 2, MaxMHz: 4000, MinMHz: 800,
		},
		CPUs: []types.CPUInfo{
			{Index: 0, ModelName: "Fixture CPU", CoreID: "0", PhysicalID: "0", Mhz: 3200, UsagePercent: 12.5, TemperatureCelsius: 45},
			{Index: 1, ModelName: "Fixture CPU", CoreID: "1", PhysicalID: "0", Mhz: 3300, UsagePercent: 7.5, TemperatureCelsius: 43},
		},
		Memory: types.MemoryInfo{
			TotalBytes: 16 << 30, UsedBytes: 8 << 30, AvailableBytes: 8 << 30,
			FreeBytes: 4 << 30, UsedPercent: 50,
			DIMMs: []types.DIMMInfo{{Index: 0, Location: "DIMM0", Manufacturer: "Fixture", SizeBytes: 8 << 30, Type: "DDR5", SpeedMTs: 4800}},
		},
		Disks: []types.DiskInfo{
			{Index: 0, Name: "nvme0n1", Model: "Fixture SSD", SizeBytes: 512 << 30, TotalBytes: 500 << 30, UsedBytes: 250 << 30, UsedPercent: 50},
		},
		Networks: []types.NetworkInfo{
			{Index: 0, Name: "eth0", HardwareAddr: "aa:bb:cc:dd:ee:ff", Addresses: []string{"10.0.0.2/24"}, MTU: 1500, IsUp: true, Speed: 1000},
		},
		GPUs: []types.GPUInfo{
			{Index: 0, Name: "Fixture GPU", DriverVersion: "1.0", MemoryTotalMiB: 8192, MemoryUsedMiB: 1024, TemperatureGPU: 50},
		},
		LoadAvg:   types.LoadAverage{Load1: 1.5, Load5: 1.0, Load15: 0.5},
		Processes: types.ProcessSummary{Total: 100, Running: 1, Sleeping: 60, Idle: 38, Zombie: 1},
	}
}

// withFixtureSnapshot swaps collectSnapshot for the duration of a test.
func withFixtureSnapshot(t *testing.T, snap *types.Snapshot, err error) {
	t.Helper()
	original := collectSnapshot
	collectSnapshot = func() (*types.Snapshot, error) {
		if err != nil {
			return nil, err
		}
		// Return a copy so a command that mutates the snapshot (the --index
		// filters do) cannot leak state into the next test.
		clone := *snap
		return &clone, nil
	}
	t.Cleanup(func() { collectSnapshot = original })
}

// newTestCmd returns a cobra command whose output is captured in buf.
func newTestCmd(buf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd
}

// resetGlobals restores the package-level flag state between tests.
func resetGlobals(t *testing.T) {
	t.Helper()
	origJSON, origRefresh := jsonOutput, refreshInterval
	origCPU, origGPU := cpuIndex, gpuIndex
	origMem, origDisk, origNet := memoryIndex, storageIndex, networkIndex
	origCtr, origVM := containerIndex, vmIndex
	t.Cleanup(func() {
		jsonOutput, refreshInterval = origJSON, origRefresh
		cpuIndex, gpuIndex = origCPU, origGPU
		memoryIndex, storageIndex, networkIndex = origMem, origDisk, origNet
		containerIndex, vmIndex = origCtr, origVM
	})
	jsonOutput, refreshInterval = false, 0
	cpuIndex, gpuIndex, memoryIndex, storageIndex, networkIndex = -1, -1, -1, -1, -1
	containerIndex, vmIndex = -1, -1
}

// commandCases enumerates every one-shot subcommand along with a string that
// must appear in its human-readable output.
func commandCases() []struct {
	name     string
	run      func(*cobra.Command, []string) error
	wantText string
	jsonKey  string
} {
	return []struct {
		name     string
		run      func(*cobra.Command, []string) error
		wantText string
		jsonKey  string
	}{
		{"overview", runOverviewCmd, "fixture-host", "host"},
		{"host", runHostCmd, "fixture-host", "hostname"},
		{"cpu", runCPUCmd, "Fixture CPU", "cpu_summary"},
		{"memory", runMemoryCmd, "Memory", "total_bytes"},
		{"storage", runStorageCmd, "nvme0n1", ""},
		{"network", runNetworkCmd, "eth0", ""},
		{"gpu", runGPUCmd, "Fixture GPU", "gpus"},
	}
}

// ---- text rendering -------------------------------------------------------

// TestCommands_RenderText verifies each subcommand renders its section.
func TestCommands_RenderText(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals(t)
			withFixtureSnapshot(t, fixtureSnapshot(), nil)

			var buf bytes.Buffer
			cmd := newTestCmd(&buf)

			if err := tc.run(cmd, nil); err != nil {
				t.Fatalf("%s: error = %v, want nil", tc.name, err)
			}
			if !strings.Contains(buf.String(), tc.wantText) {
				t.Errorf("%s output missing %q; got:\n%s", tc.name, tc.wantText, buf.String())
			}
		})
	}
}

// TestCommands_RenderJSON verifies each subcommand emits parseable JSON that
// contains its expected key when --json is set.
func TestCommands_RenderJSON(t *testing.T) {
	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals(t)
			jsonOutput = true
			withFixtureSnapshot(t, fixtureSnapshot(), nil)

			var buf bytes.Buffer
			cmd := newTestCmd(&buf)

			if err := tc.run(cmd, nil); err != nil {
				t.Fatalf("%s --json: error = %v, want nil", tc.name, err)
			}

			// storage and network emit a bare array; the rest emit an object.
			if tc.jsonKey == "" {
				var arr []map[string]any
				if err := json.Unmarshal(buf.Bytes(), &arr); err != nil {
					t.Fatalf("%s --json: output is not a valid JSON array: %v\nraw:\n%s", tc.name, err, buf.String())
				}
				if len(arr) == 0 {
					t.Errorf("%s --json: array is empty, want the fixture entry", tc.name)
				}
				return
			}

			var parsed map[string]any
			if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
				t.Fatalf("%s --json: output is not valid JSON: %v\nraw:\n%s", tc.name, err, buf.String())
			}
			if _, ok := parsed[tc.jsonKey]; !ok {
				t.Errorf("%s --json: missing key %q; got keys %v", tc.name, tc.jsonKey, keysOf(parsed))
			}
		})
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCommands_CollectorFailure verifies every subcommand propagates a
// collection failure instead of rendering a partial or empty report.
func TestCommands_CollectorFailure(t *testing.T) {
	wantErr := &types.CollectorError{Collector: "fixture", Cause: errors.New("device gone")}

	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals(t)
			withFixtureSnapshot(t, nil, wantErr)

			var buf bytes.Buffer
			cmd := newTestCmd(&buf)

			err := tc.run(cmd, nil)
			if err == nil {
				t.Fatalf("%s: error = nil, want the collector failure to propagate", tc.name)
			}
			var collErr *types.CollectorError
			if !errors.As(err, &collErr) {
				t.Errorf("%s: error = %T (%v), want *types.CollectorError", tc.name, err, err)
			}
		})
	}
}

// ---- index filters --------------------------------------------------------

// TestCommands_IndexFilters checks the --index flag narrows output to one
// entry for each command that supports it.
func TestCommands_IndexFilters(t *testing.T) {
	tests := []struct {
		name  string
		set   func()
		run   func(*cobra.Command, []string) error
		want  string
		avoid string
	}{
		{"cpu", func() { cpuIndex = 1 }, runCPUCmd, "7.5%", "12.5%"},
		{"gpu", func() { gpuIndex = 0 }, runGPUCmd, "Fixture GPU", ""},
		{"storage", func() { storageIndex = 0 }, runStorageCmd, "nvme0n1", ""},
		{"network", func() { networkIndex = 0 }, runNetworkCmd, "eth0", ""},
		{"memory", func() { memoryIndex = 0 }, runMemoryCmd, "DIMM0", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobals(t)
			tt.set()
			withFixtureSnapshot(t, fixtureSnapshot(), nil)

			var buf bytes.Buffer
			cmd := newTestCmd(&buf)

			if err := tt.run(cmd, nil); err != nil {
				t.Fatalf("%s --index: error = %v, want nil", tt.name, err)
			}
			out := buf.String()
			if !strings.Contains(out, tt.want) {
				t.Errorf("%s --index output missing %q; got:\n%s", tt.name, tt.want, out)
			}
			if tt.avoid != "" && strings.Contains(out, tt.avoid) {
				t.Errorf("%s --index output should not contain %q; got:\n%s", tt.name, tt.avoid, out)
			}
		})
	}
}

// TestCommands_IndexOutOfRange verifies an out-of-range --index returns the
// typed not-found error rather than rendering an empty section.
func TestCommands_IndexOutOfRange(t *testing.T) {
	tests := []struct {
		name string
		set  func()
		run  func(*cobra.Command, []string) error
	}{
		{"cpu", func() { cpuIndex = 99 }, runCPUCmd},
		{"gpu", func() { gpuIndex = 99 }, runGPUCmd},
		{"storage", func() { storageIndex = 99 }, runStorageCmd},
		{"network", func() { networkIndex = 99 }, runNetworkCmd},
		{"memory", func() { memoryIndex = 99 }, runMemoryCmd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobals(t)
			tt.set()
			withFixtureSnapshot(t, fixtureSnapshot(), nil)

			var buf bytes.Buffer
			cmd := newTestCmd(&buf)

			if err := tt.run(cmd, nil); err == nil {
				t.Fatalf("%s --index=99: error = nil, want a not-found error", tt.name)
			}
		})
	}
}

// ---- version --------------------------------------------------------------

func TestRunVersionCmd_Text(t *testing.T) {
	resetGlobals(t)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runVersionCmd(cmd, nil); err != nil {
		t.Fatalf("runVersionCmd() error = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), version) {
		t.Errorf("version output missing %q; got:\n%s", version, buf.String())
	}
}

func TestRunVersionCmd_JSON(t *testing.T) {
	resetGlobals(t)
	jsonOutput = true

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runVersionCmd(cmd, nil); err != nil {
		t.Fatalf("runVersionCmd() error = %v, want nil", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\nraw:\n%s", err, buf.String())
	}
	if _, ok := parsed["version"]; !ok {
		t.Errorf("version --json missing 'version' key; got %v", keysOf(parsed))
	}
}

// ---- runWithRefresh -------------------------------------------------------

// TestRunWithRefresh_OneShot verifies the render function runs exactly once
// when --refresh is disabled.
func TestRunWithRefresh_OneShot(t *testing.T) {
	resetGlobals(t)
	withFixtureSnapshot(t, fixtureSnapshot(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	calls := 0
	err := runWithRefresh(cmd, func(_ *cobra.Command, snap *types.Snapshot) error {
		calls++
		if snap == nil {
			t.Error("render received a nil snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("runWithRefresh() error = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("render called %d times, want 1", calls)
	}
}

// TestRunWithRefresh_CollectorError verifies a collection failure aborts
// before the render function is ever invoked.
func TestRunWithRefresh_CollectorError(t *testing.T) {
	resetGlobals(t)
	withFixtureSnapshot(t, nil, errors.New("collector down"))

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	calls := 0
	err := runWithRefresh(cmd, func(*cobra.Command, *types.Snapshot) error {
		calls++
		return nil
	})
	if err == nil {
		t.Fatal("runWithRefresh() error = nil, want the collector error")
	}
	if calls != 0 {
		t.Errorf("render called %d times, want 0 when collection fails", calls)
	}
}

// TestRunWithRefresh_RenderError verifies a render failure is propagated.
func TestRunWithRefresh_RenderError(t *testing.T) {
	resetGlobals(t)
	withFixtureSnapshot(t, fixtureSnapshot(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	wantErr := errors.New("render exploded")
	err := runWithRefresh(cmd, func(*cobra.Command, *types.Snapshot) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Errorf("runWithRefresh() error = %v, want %v", err, wantErr)
	}
}

// ---- render failures ------------------------------------------------------

// brokenWriter fails every write, standing in for a closed stdout.
type brokenWriter struct{ err error }

func (w brokenWriter) Write([]byte) (int, error) { return 0, w.err }

// TestCommands_RenderFailurePropagates verifies each subcommand surfaces a
// write failure rather than exiting 0 with truncated output. This is the
// command-level half of the errWriter change in pkg/cli.
func TestCommands_RenderFailurePropagates(t *testing.T) {
	broken := errors.New("write: broken pipe")

	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals(t)
			withFixtureSnapshot(t, fixtureSnapshot(), nil)

			cmd := &cobra.Command{Use: "test"}
			cmd.SetOut(brokenWriter{err: broken})
			cmd.SetErr(brokenWriter{err: broken})

			if err := tc.run(cmd, nil); err == nil {
				t.Errorf("%s: error = nil, want the write failure to propagate", tc.name)
			}
		})
	}
}

// TestCommands_JSONEncodeFailurePropagates verifies the --json paths also
// report a dead output stream.
func TestCommands_JSONEncodeFailurePropagates(t *testing.T) {
	broken := errors.New("write: broken pipe")

	for _, tc := range commandCases() {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals(t)
			jsonOutput = true
			withFixtureSnapshot(t, fixtureSnapshot(), nil)

			cmd := &cobra.Command{Use: "test"}
			cmd.SetOut(brokenWriter{err: broken})
			cmd.SetErr(brokenWriter{err: broken})

			if err := tc.run(cmd, nil); err == nil {
				t.Errorf("%s --json: error = nil, want the write failure to propagate", tc.name)
			}
		})
	}
}

// TestRunVersionCmd_RenderFailure covers the version command's write-error path.
func TestRunVersionCmd_RenderFailure(t *testing.T) {
	resetGlobals(t)

	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(brokenWriter{err: errors.New("write: broken pipe")})

	if err := runVersionCmd(cmd, nil); err == nil {
		t.Error("runVersionCmd() error = nil, want the write failure to propagate")
	}
}

// ---- real collector wiring ------------------------------------------------

// TestCollectSnapshot_RealCollector exercises the production collectSnapshot,
// which every other test in this package replaces with a fixture. Without it
// the default wiring — construct a SystemCollector, disable tiering, take one
// snapshot — would never be verified at the unit level.
//
// It only reads /proc and /sys, so it neither modifies the host nor blocks.
func TestCollectSnapshot_RealCollector(t *testing.T) {
	snap, err := collectSnapshot()
	if err != nil {
		t.Fatalf("collectSnapshot() error = %v, want nil", err)
	}
	if snap == nil {
		t.Fatal("collectSnapshot() = nil, want a snapshot")
	}

	// Assert only on facts true of any Linux host, so the test stays portable.
	if snap.Host.Hostname == "" {
		t.Error("snapshot has an empty hostname")
	}
	if snap.CPUSummary.TotalThreads <= 0 {
		t.Errorf("total_threads = %d, want > 0", snap.CPUSummary.TotalThreads)
	}
	if snap.Memory.TotalBytes == 0 {
		t.Error("memory.total_bytes = 0, want the machine's RAM size")
	}
	if len(snap.CPUs) == 0 {
		t.Error("snapshot reports no CPUs")
	}
	if snap.Timestamp.IsZero() {
		t.Error("snapshot timestamp is zero")
	}
}

// ---- virtualization commands ----------------------------------------------

// virtFixture adds container and VM data to the shared fixture snapshot.
func virtFixture() *types.Snapshot {
	snap := fixtureSnapshot()
	snap.Virt = types.VirtInfo{
		CgroupVersion: "v2",
		Runtimes:      []string{"docker"},
		Containers: []types.ContainerInfo{
			{Index: 0, ID: "aaaa1111bbbb2222", ShortID: "aaaa1111bbbb", Name: "fixture-ctr", Runtime: "docker", MemoryBytes: 1 << 20, PIDs: 7},
			{Index: 1, ID: "cccc3333dddd4444", ShortID: "cccc3333dddd", Name: "other-ctr", Runtime: "docker", MemoryBytes: 2 << 20, PIDs: 3},
		},
		VMs: []types.VMInfo{
			{Index: 0, Name: "fixture-vm", Hypervisor: "qemu/kvm", PID: 999, VCPUs: 4, MemoryBytes: 4 << 30},
		},
	}
	return snap
}

func TestRunContainersCmd_Text(t *testing.T) {
	resetGlobals(t)
	withFixtureSnapshot(t, virtFixture(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runContainersCmd(cmd, nil); err != nil {
		t.Fatalf("runContainersCmd() error = %v", err)
	}
	if !strings.Contains(buf.String(), "fixture-ctr") {
		t.Errorf("output missing the container name; got:\n%s", buf.String())
	}
}

func TestRunContainersCmd_JSON(t *testing.T) {
	resetGlobals(t)
	jsonOutput = true
	withFixtureSnapshot(t, virtFixture(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runContainersCmd(cmd, nil); err != nil {
		t.Fatalf("runContainersCmd() error = %v", err)
	}
	var parsed struct {
		CgroupVersion string           `json:"cgroup_version"`
		Containers    []map[string]any `json:"containers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("containers --json is not valid JSON: %v\nraw:\n%s", err, buf.String())
	}
	if parsed.CgroupVersion != "v2" || len(parsed.Containers) != 2 {
		t.Errorf("parsed = %+v, want cgroup v2 and 2 containers", parsed)
	}
}

func TestRunContainersCmd_IndexFilterAndOutOfRange(t *testing.T) {
	resetGlobals(t)
	containerIndex = 1
	withFixtureSnapshot(t, virtFixture(), nil)

	var buf bytes.Buffer
	if err := runContainersCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runContainersCmd() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "other-ctr") || strings.Contains(out, "fixture-ctr") {
		t.Errorf("--index 1 did not narrow the output; got:\n%s", out)
	}

	containerIndex = 99
	var buf2 bytes.Buffer
	err := runContainersCmd(newTestCmd(&buf2), nil)
	if err == nil {
		t.Fatal("--index 99 = nil, want a not-found error")
	}
	var notFound *types.ContainerIndexNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("error = %T, want *types.ContainerIndexNotFoundError", err)
	}
}

func TestRunVMsCmd_TextAndJSON(t *testing.T) {
	resetGlobals(t)
	withFixtureSnapshot(t, virtFixture(), nil)

	var buf bytes.Buffer
	if err := runVMsCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runVMsCmd() error = %v", err)
	}
	if !strings.Contains(buf.String(), "fixture-vm") {
		t.Errorf("output missing the VM name; got:\n%s", buf.String())
	}

	jsonOutput = true
	var jbuf bytes.Buffer
	if err := runVMsCmd(newTestCmd(&jbuf), nil); err != nil {
		t.Fatalf("runVMsCmd() --json error = %v", err)
	}
	var parsed struct {
		VMs []map[string]any `json:"vms"`
	}
	if err := json.Unmarshal(jbuf.Bytes(), &parsed); err != nil {
		t.Fatalf("vms --json is not valid JSON: %v\nraw:\n%s", err, jbuf.String())
	}
	if len(parsed.VMs) != 1 {
		t.Errorf("got %d vms, want 1", len(parsed.VMs))
	}
}

func TestRunVMsCmd_IndexOutOfRange(t *testing.T) {
	resetGlobals(t)
	vmIndex = 99
	withFixtureSnapshot(t, virtFixture(), nil)

	var buf bytes.Buffer
	err := runVMsCmd(newTestCmd(&buf), nil)
	if err == nil {
		t.Fatal("--index 99 = nil, want a not-found error")
	}
	var notFound *types.VMIndexNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("error = %T, want *types.VMIndexNotFoundError", err)
	}
}

func TestRunVirtCmds_CollectorFailure(t *testing.T) {
	wantErr := &types.CollectorError{Collector: "fixture", Cause: errors.New("gone")}

	for name, run := range map[string]func(*cobra.Command, []string) error{
		"containers": runContainersCmd,
		"vms":        runVMsCmd,
	} {
		t.Run(name, func(t *testing.T) {
			resetGlobals(t)
			withFixtureSnapshot(t, nil, wantErr)

			var buf bytes.Buffer
			if err := run(newTestCmd(&buf), nil); err == nil {
				t.Errorf("%s: error = nil, want the collector failure to propagate", name)
			}
		})
	}
}

// ---- images command -------------------------------------------------------

// withRuntimeSnapshot swaps the runtime-enabled collector for a fixture.
func withRuntimeSnapshot(t *testing.T, snap *types.Snapshot, err error) {
	t.Helper()
	original := collectSnapshotWithRuntime
	collectSnapshotWithRuntime = func() (*types.Snapshot, error) {
		if err != nil {
			return nil, err
		}
		clone := *snap
		return &clone, nil
	}
	t.Cleanup(func() { collectSnapshotWithRuntime = original })
}

func imagesFixture() *types.Snapshot {
	snap := fixtureSnapshot()
	snap.Virt.Runtime = types.RuntimeInfo{
		Available: true, Engine: "docker", Version: "29.1.3",
		RootDir: "/data/docker", StorageDriver: "overlay2", BackingFilesystem: "extfs",
		ImagesTotal: 2, LayersBytes: 1 << 30,
		Images: []types.ImageInfo{{ShortID: "aaaa1111bbbb", Tags: []string{"app:v1"}, SizeBytes: 1 << 29, InUse: true, Containers: 1}},
	}
	return snap
}

func TestRunImagesCmd_Text(t *testing.T) {
	resetGlobals(t)
	withRuntimeSnapshot(t, imagesFixture(), nil)

	var buf bytes.Buffer
	if err := runImagesCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runImagesCmd() error = %v", err)
	}
	for _, want := range []string{"Container Images", "/data/docker", "app:v1"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output missing %q; got:\n%s", want, buf.String())
		}
	}
}

func TestRunImagesCmd_JSON(t *testing.T) {
	resetGlobals(t)
	jsonOutput = true
	withRuntimeSnapshot(t, imagesFixture(), nil)

	var buf bytes.Buffer
	if err := runImagesCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runImagesCmd() error = %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("images --json is not valid JSON: %v\nraw:\n%s", err, buf.String())
	}
	for _, key := range []string{"available", "root_dir", "images", "layers_bytes"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("images --json missing %q; got %v", key, keysOf(parsed))
		}
	}
}

func TestRunImagesCmd_CollectorFailure(t *testing.T) {
	resetGlobals(t)
	withRuntimeSnapshot(t, nil, errors.New("daemon unreachable"))

	var buf bytes.Buffer
	if err := runImagesCmd(newTestCmd(&buf), nil); err == nil {
		t.Error("runImagesCmd() error = nil, want the collection failure to propagate")
	}
}

func TestRunImagesCmd_RenderFailure(t *testing.T) {
	resetGlobals(t)
	withRuntimeSnapshot(t, imagesFixture(), nil)

	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(brokenWriter{err: errors.New("write: broken pipe")})

	if err := runImagesCmd(cmd, nil); err == nil {
		t.Error("runImagesCmd() error = nil, want the write failure to propagate")
	}
}

// ---- doctor ---------------------------------------------------------------

// TestRunDoctorCmd_ReportsEveryCheck verifies every prerequisite is reported
// with a status marker.
func TestRunDoctorCmd_ReportsEveryCheck(t *testing.T) {
	resetGlobals(t)

	var buf bytes.Buffer
	if err := runDoctorCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runDoctorCmd() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{"sysmon doctor", "build", "desktop GUI", "SMART", "container metrics", "image inventory"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q; got:\n%s", want, out)
		}
	}
}

// TestRunDoctorCmd_JSON verifies the machine-readable form carries a status
// and detail for every check.
func TestRunDoctorCmd_JSON(t *testing.T) {
	resetGlobals(t)
	jsonOutput = true

	var buf bytes.Buffer
	if err := runDoctorCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runDoctorCmd() error = %v", err)
	}

	var parsed struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\nraw:\n%s", err, buf.String())
	}
	if len(parsed.Checks) == 0 {
		t.Fatal("doctor --json reported no checks")
	}
	for _, c := range parsed.Checks {
		if c.Name == "" || c.Detail == "" {
			t.Errorf("incomplete check entry: %+v", c)
		}
		switch c.Status {
		case "ok", "degraded", "unavailable":
		default:
			t.Errorf("check %q has unknown status %q", c.Name, c.Status)
		}
	}
}

// TestRunDoctorCmd_RenderFailure verifies a dead output stream is reported
// rather than silently swallowed.
func TestRunDoctorCmd_RenderFailure(t *testing.T) {
	resetGlobals(t)

	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(brokenWriter{err: errors.New("write: broken pipe")})

	if err := runDoctorCmd(cmd, nil); err == nil {
		t.Error("runDoctorCmd() error = nil, want the write failure to propagate")
	}
}

// TestCheckDesktopGUI_AlwaysActionable verifies the GUI check never reports a
// problem without telling the user how to fix it.
func TestCheckDesktopGUI_AlwaysActionable(t *testing.T) {
	got := checkDesktopGUI()

	if got.Name != "desktop GUI" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Status == "unavailable" && got.Fix == "" {
		t.Error("an unavailable GUI must come with an install command")
	}
}

// withMissingLibs makes the desktop-library lookup report the given libraries
// as absent, so the advice branch can be tested on a machine that has them.
func withMissingLibs(t *testing.T, libs []collector.DesktopLib) {
	t.Helper()
	original := missingDesktopLibs
	missingDesktopLibs = func() []collector.DesktopLib { return libs }
	t.Cleanup(func() { missingDesktopLibs = original })
}

// TestCheckDesktopGUI_MissingLibsGivesInstallCommand covers the branch a user
// on a headless box actually hits.
func TestCheckDesktopGUI_MissingLibsGivesInstallCommand(t *testing.T) {
	withMissingLibs(t, []collector.DesktopLib{
		{SOName: "libwebkit2gtk-4.1.so.0", Debian: "libwebkit2gtk-4.1-0", Fedora: "webkit2gtk4.1", Arch: "webkit2gtk-4.1"},
	})

	got := checkDesktopGUI()
	if got.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", got.Status)
	}
	if !strings.Contains(got.Detail, "libwebkit2gtk-4.1.so.0") {
		t.Errorf("Detail does not name the missing library: %q", got.Detail)
	}
	if got.Fix == "" {
		t.Error("no install command offered for a missing library")
	}
}

// TestExplainNoGUI_NamesMissingLibraries verifies the fallback notice tells a
// user exactly what to install rather than only that a GUI is unavailable.
func TestExplainNoGUI_NamesMissingLibraries(t *testing.T) {
	withMissingLibs(t, []collector.DesktopLib{
		{SOName: "libgtk-3.so.0", Debian: "libgtk-3-0", Fedora: "gtk3", Arch: "gtk3"},
	})

	var buf bytes.Buffer
	explainNoGUI(&buf)

	out := buf.String()
	for _, want := range []string{"No GUI in this build", "sysmon serve", "libgtk-3.so.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("notice missing %q; got:\n%s", want, out)
		}
	}
}

// TestExplainNoGUI_LibsPresentRecommendsDesktopBuild covers the other branch:
// the libraries are there, so the answer is to build the desktop binary.
func TestExplainNoGUI_LibsPresentRecommendsDesktopBuild(t *testing.T) {
	withMissingLibs(t, nil)

	var buf bytes.Buffer
	explainNoGUI(&buf)

	out := buf.String()
	if !strings.Contains(out, "make build-desktop") {
		t.Errorf("notice does not point at the desktop build; got:\n%s", out)
	}
}

// TestRunStartupChecks_CountsProblems verifies the startup report counts only
// checks that are not ok, so a healthy machine stays quiet.
func TestRunStartupChecks_CountsProblems(t *testing.T) {
	// Force a known problem so the count is deterministic regardless of the
	// machine the tests run on.
	withMissingLibs(t, []collector.DesktopLib{
		{SOName: "libwebkit2gtk-4.1.so.0", Debian: "libwebkit2gtk-4.1-0", Fedora: "webkit2gtk4.1", Arch: "webkit2gtk-4.1"},
	})

	if got := runStartupChecks(); got < 1 {
		t.Errorf("runStartupChecks() = %d, want at least the injected problem", got)
	}
}
