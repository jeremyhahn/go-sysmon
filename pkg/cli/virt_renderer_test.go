package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// virtSnapshot returns a snapshot with one container and one VM.
func virtSnapshot() *types.Snapshot {
	return &types.Snapshot{
		Virt: types.VirtInfo{
			CgroupVersion: "v2",
			Runtimes:      []string{"docker"},
			Containers: []types.ContainerInfo{{
				Index:            0,
				ID:               "004c327801b2b84b8fcb0b6a340d8a5b",
				ShortID:          "004c327801b2",
				Name:             "webui",
				Runtime:          "docker",
				CPUPercent:       12.5,
				MemoryBytes:      116629504,
				MemoryLimitBytes: 268435456,
				MemoryPercent:    43.4,
				PIDs:             30,
				ReadBytesRate:    1024,
				WriteBytesRate:   2048,
			}},
			VMs: []types.VMInfo{{
				Index:         0,
				Name:          "vstation-01",
				UUID:          "383ccd65-823e-45a6-aeb6-936d00e29214",
				Hypervisor:    "qemu/kvm",
				Accelerator:   "kvm",
				PID:           3561,
				VCPUs:         24,
				MemoryBytes:   67108864000,
				RSSBytes:      48000000000,
				CPUPercent:    386.5,
				MACAddresses:  []string{"52:54:00:a0:bd:a5"},
				TapInterfaces: []string{"vnet0"},
				DiskImages:    []string{"/var/lib/libvirt/images/vstation-01.img"},
			}},
		},
	}
}

// ---- RenderContainers -----------------------------------------------------

func TestRenderContainers_RendersTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderContainers(&buf, virtSnapshot()); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Containers", "webui", "004c327801b2", "docker",
		"cgroup v2", "NAME", "PIDS", "30",
		// Diagnostic columns added for operational visibility.
		"LIMIT", "THROTTLED", "PEAK", "UPTIME",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderContainers_NoContainers(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	snap := &types.Snapshot{Virt: types.VirtInfo{CgroupVersion: "v2"}}

	if err := RenderContainers(&buf, snap); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}
	if !strings.Contains(buf.String(), "No containers detected") {
		t.Errorf("expected an empty-state message; got:\n%s", buf.String())
	}
}

// TestRenderContainers_NoCgroupFilesystem distinguishes "no containers" from
// "cannot measure containers", which are different problems for the reader.
func TestRenderContainers_NoCgroupFilesystem(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderContainers(&buf, &types.Snapshot{}); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}
	if !strings.Contains(buf.String(), "No cgroup filesystem") {
		t.Errorf("expected a cgroup-unavailable message; got:\n%s", buf.String())
	}
}

func TestRenderContainers_NilSnapshot(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := RenderContainers(&buf, nil)
	if err == nil {
		t.Fatal("RenderContainers(nil) = nil, want an error")
	}
	var collErr *types.CollectorError
	if !errors.As(err, &collErr) {
		t.Errorf("error = %T, want *types.CollectorError", err)
	}
}

func TestRenderContainers_PropagatesWriteError(t *testing.T) {
	t.Parallel()
	broken := errors.New("write: broken pipe")
	err := RenderContainers(&errAfterN{n: 0, fail: broken}, virtSnapshot())
	if !errors.Is(err, broken) {
		t.Errorf("RenderContainers() error = %v, want %v", err, broken)
	}
}

// ---- RenderVMs ------------------------------------------------------------

func TestRenderVMs_RendersDetail(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderVMs(&buf, virtSnapshot()); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Virtual Machines", "vstation-01", "qemu/kvm", "kvm",
		"383ccd65-823e-45a6-aeb6-936d00e29214", "24",
		"52:54:00:a0:bd:a5", "vnet0", "vstation-01.img",
		"configured", "resident on host",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRenderVMs_NoVMs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderVMs(&buf, &types.Snapshot{}); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}
	if !strings.Contains(buf.String(), "No virtual machines detected") {
		t.Errorf("expected an empty-state message; got:\n%s", buf.String())
	}
}

func TestRenderVMs_NilSnapshot(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderVMs(&buf, nil); err == nil {
		t.Fatal("RenderVMs(nil) = nil, want an error")
	}
}

func TestRenderVMs_PropagatesWriteError(t *testing.T) {
	t.Parallel()
	broken := errors.New("write: broken pipe")
	err := RenderVMs(&errAfterN{n: 0, fail: broken}, virtSnapshot())
	if !errors.Is(err, broken) {
		t.Errorf("RenderVMs() error = %v, want %v", err, broken)
	}
}

// TestRenderVMs_MinimalVM covers a guest with none of the optional fields, so
// every conditional block is skipped.
func TestRenderVMs_MinimalVM(t *testing.T) {
	t.Parallel()
	snap := &types.Snapshot{Virt: types.VirtInfo{
		VMs: []types.VMInfo{{Name: "bare", Hypervisor: "qemu/kvm", PID: 1}},
	}}

	var buf bytes.Buffer
	if err := RenderVMs(&buf, snap); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}
	if !strings.Contains(buf.String(), "bare") {
		t.Errorf("output missing the VM name; got:\n%s", buf.String())
	}
}

// TestRenderContainers_UnlimitedMemory verifies a container with no memory
// cap renders a plain size rather than a nonsensical percentage.
func TestRenderContainers_UnlimitedMemory(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	snap.Virt.Containers[0].MemoryLimitBytes = 0
	snap.Virt.Containers[0].MemoryPercent = 0

	var buf bytes.Buffer
	if err := RenderContainers(&buf, snap); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}
	if strings.Contains(buf.String(), " / 0 B") {
		t.Errorf("unlimited memory rendered a zero limit; got:\n%s", buf.String())
	}
}

// TestRenderContainers_WarnsOnThrottlingAndOOM verifies the attention block
// surfaces the conditions that explain a slow or restarting container.
func TestRenderContainers_WarnsOnThrottlingAndOOM(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	c := &snap.Virt.Containers[0]
	c.NrPeriods = 100
	c.NrThrottled = 40
	c.ThrottledPercent = 40
	c.ThrottledUsec = 2_500_000
	c.OOMKills = 2
	c.CPUPressure = 12
	c.MemoryPressure = 7
	c.IOPressure = 6
	c.SwapBytes = 1 << 20

	var buf bytes.Buffer
	if err := RenderContainers(&buf, snap); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Attention:", "throttled", "OOM killer",
		"CPU stall pressure", "memory stall pressure", "I/O stall pressure", "swapping",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("attention block missing %q; got:\n%s", want, out)
		}
	}
}

// TestRenderContainers_NoWarningsWhenHealthy verifies a clean container
// produces no attention block, so the section means something when it appears.
func TestRenderContainers_NoWarningsWhenHealthy(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderContainers(&buf, virtSnapshot()); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}
	if strings.Contains(buf.String(), "Attention:") {
		t.Errorf("healthy containers produced an attention block:\n%s", buf.String())
	}
}

// TestRenderContainers_WarnsAtMemoryLimit covers the peak-at-limit condition,
// which explains an OOM that has not happened yet.
func TestRenderContainers_WarnsAtMemoryLimit(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	snap.Virt.Containers[0].MemoryPeakBytes = snap.Virt.Containers[0].MemoryLimitBytes

	var buf bytes.Buffer
	if err := RenderContainers(&buf, snap); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}
	if !strings.Contains(buf.String(), "peaked at its memory limit") {
		t.Errorf("expected a memory-limit warning; got:\n%s", buf.String())
	}
}

// TestRenderVMs_ReportsThreadsAndNetwork covers the operational fields added
// to the VM detail block.
func TestRenderVMs_ReportsThreadsAndNetwork(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	vm := &snap.Virt.VMs[0]
	vm.VCPUThreads = 24
	vm.ThreadCount = 94
	vm.UptimeSeconds = 3600
	vm.NetRxRate = 1024
	vm.NetTxRate = 2048
	vm.DiskImageBytes = 1 << 30

	var buf bytes.Buffer
	if err := RenderVMs(&buf, snap); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{"Threads:", "24 vCPU of 94", "Uptime:", "Host NICs:", "in "} {
		if !strings.Contains(out, want) {
			t.Errorf("VM output missing %q; got:\n%s", want, out)
		}
	}
}

// TestRenderVMs_WarnsOnVCPUThreadMismatch verifies a topology mismatch is
// called out rather than shown as two unrelated numbers.
func TestRenderVMs_WarnsOnVCPUThreadMismatch(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	snap.Virt.VMs[0].VCPUs = 24
	snap.Virt.VMs[0].VCPUThreads = 12
	snap.Virt.VMs[0].ThreadCount = 40

	var buf bytes.Buffer
	if err := RenderVMs(&buf, snap); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}
	if !strings.Contains(buf.String(), "does not match") {
		t.Errorf("expected a vCPU mismatch note; got:\n%s", buf.String())
	}
}

// ---- RenderImages ---------------------------------------------------------

// runtimeSnapshot returns a snapshot with populated runtime storage data.
func runtimeSnapshot() *types.Snapshot {
	snap := virtSnapshot()
	snap.Virt.Runtime = types.RuntimeInfo{
		Available:         true,
		Engine:            "docker",
		Version:           "29.1.3",
		RootDir:           "/data/docker",
		StorageDriver:     "overlay2",
		BackingFilesystem: "extfs",
		ImagesTotal:       83,
		ContainersTotal:   9,
		ContainersRunning: 6,
		ContainersStopped: 3,
		LayersBytes:       79 << 30,
		ReclaimableBytes:  70 << 30,
		DanglingImages:    49,
		UnusedImages:      75,
		VolumesCount:      63,
		VolumesUnused:     53,
		VolumesBytes:      208 << 30,
		BuildCacheEntries: 376,
		BuildCacheBytes:   180 << 30,
		Images: []types.ImageInfo{
			{Index: 0, ShortID: "1780f2264268", Tags: []string{"app:latest"}, SizeBytes: 11 << 30, Containers: 2, InUse: true},
			{Index: 1, ShortID: "2bfdb3e95b72", SizeBytes: 10 << 30, Dangling: true},
		},
	}
	return snap
}

func TestRenderImages_ReportsStorageLayout(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderImages(&buf, runtimeSnapshot()); err != nil {
		t.Fatalf("RenderImages() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Container Images", "docker 29.1.3",
		// The storage root is the point of the command: it is routinely
		// relocated and is the first thing to check when a disk fills.
		"/data/docker", "overlay2", "extfs",
		"RECLAIMABLE", "Build cache", "Footprint:",
		"app:latest", "1780f2264268", "<none>",
		"shared layers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("images output missing %q; got:\n%s", want, out)
		}
	}
}

// TestRenderImages_Unavailable verifies the command explains why there is no
// data rather than printing an empty table.
func TestRenderImages_Unavailable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderImages(&buf, &types.Snapshot{}); err != nil {
		t.Fatalf("RenderImages() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No container runtime API reachable") {
		t.Errorf("expected an explanation; got:\n%s", out)
	}
	if !strings.Contains(out, "root-owned") {
		t.Errorf("expected the reason to be stated; got:\n%s", out)
	}
}

func TestRenderImages_TruncationIsDisclosed(t *testing.T) {
	t.Parallel()
	snap := runtimeSnapshot()
	snap.Virt.Runtime.ImagesTruncated = 12

	var buf bytes.Buffer
	if err := RenderImages(&buf, snap); err != nil {
		t.Fatalf("RenderImages() error = %v", err)
	}
	if !strings.Contains(buf.String(), "12 smaller image(s) not sent by the collector") {
		t.Errorf("truncation was not disclosed; got:\n%s", buf.String())
	}
}

func TestRenderImages_NilSnapshot(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderImages(&buf, nil); err == nil {
		t.Fatal("RenderImages(nil) = nil, want an error")
	}
}

func TestRenderImages_PropagatesWriteError(t *testing.T) {
	t.Parallel()
	broken := errors.New("write: broken pipe")
	if err := RenderImages(&errAfterN{n: 0, fail: broken}, runtimeSnapshot()); !errors.Is(err, broken) {
		t.Errorf("RenderImages() error = %v, want %v", err, broken)
	}
}

// ---- CPU percentage presentation ------------------------------------------

// TestRenderVMs_CPUShownAgainstAllocation is the regression test for a CPU
// figure that reads as impossible. The underlying value is a percentage of one
// core (top's convention), so a 24-vCPU guest can reach 2400. The renderer must
// lead with the share of the guest's own allocation, which stays on 0-100.
func TestRenderVMs_CPUShownAgainstAllocation(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	snap.Virt.VMs[0].VCPUs = 24
	snap.Virt.VMs[0].CPUPercent = 1770 // 17.7 cores busy

	var buf bytes.Buffer
	if err := RenderVMs(&buf, snap); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "of 24 vCPUs") {
		t.Errorf("CPU not expressed against the guest's allocation; got:\n%s", out)
	}
	if !strings.Contains(out, "17.7 cores busy") {
		t.Errorf("core count missing; got:\n%s", out)
	}
	// 1770/24 = 73.75, so the headline figure must be plausible, not 1770.
	if strings.Contains(out, "1770") {
		t.Errorf("raw percent-of-one-core leaked into the headline; got:\n%s", out)
	}
}

// TestRenderVMs_CPUWithoutVCPUCount covers a guest whose vCPU count could not
// be parsed: fall back to a core count rather than a meaningless percentage.
func TestRenderVMs_CPUWithoutVCPUCount(t *testing.T) {
	t.Parallel()
	snap := virtSnapshot()
	snap.Virt.VMs[0].VCPUs = 0
	snap.Virt.VMs[0].CPUPercent = 350

	var buf bytes.Buffer
	if err := RenderVMs(&buf, snap); err != nil {
		t.Fatalf("RenderVMs() error = %v", err)
	}
	if !strings.Contains(buf.String(), "3.5 cores busy") {
		t.Errorf("expected a core count fallback; got:\n%s", buf.String())
	}
}

// TestRenderContainers_ExplainsCPUConvention verifies the table says what its
// CPU column means, since values above 100 are legitimate there too.
func TestRenderContainers_ExplainsCPUConvention(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderContainers(&buf, virtSnapshot()); err != nil {
		t.Fatalf("RenderContainers() error = %v", err)
	}
	if !strings.Contains(buf.String(), "percentage of one core") {
		t.Errorf("CPU convention not explained; got:\n%s", buf.String())
	}
}

// TestRenderImages_FilteringIsDisclosed verifies a list shortened by CLI flags
// says so, so a filtered view is never mistaken for the whole inventory.
func TestRenderImages_FilteringIsDisclosed(t *testing.T) {
	t.Parallel()
	snap := runtimeSnapshot()
	snap.Virt.Runtime.ImagesFiltered = 104

	var buf bytes.Buffer
	if err := RenderImages(&buf, snap); err != nil {
		t.Fatalf("RenderImages() error = %v", err)
	}
	if !strings.Contains(buf.String(), "104 image(s) hidden by") {
		t.Errorf("filtering was not disclosed; got:\n%s", buf.String())
	}
}
