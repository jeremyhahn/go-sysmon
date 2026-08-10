package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeSysfs creates a sysfs-style file, making its parent directories.
func writeSysfs(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func withDRMRoot(t *testing.T, root string) {
	t.Helper()
	orig := drmRoot
	drmRoot = root
	t.Cleanup(func() { drmRoot = orig })
}

// makeDRMCard creates a card directory with the given PCI vendor ID and any
// extra device-level attributes.
func makeDRMCard(t *testing.T, root, name, vendor string, attrs map[string]string) {
	t.Helper()
	dev := filepath.Join(root, name, "device")
	writeSysfs(t, filepath.Join(dev, "vendor"), vendor)
	writeSysfs(t, filepath.Join(dev, "uevent"), "PCI_SLOT_NAME=0000:03:00.0")
	for k, v := range attrs {
		writeSysfs(t, filepath.Join(dev, k), v)
	}
}

// TestEnumerateDRMCards_IgnoresConnectors verifies only card devices are
// returned. Every connected display adds a "cardN-DP-1" style entry beside the
// card itself, and counting those would multiply the GPU list.
func TestEnumerateDRMCards_IgnoresConnectors(t *testing.T) {
	root := t.TempDir()
	withDRMRoot(t, root)

	for _, name := range []string{"card0", "card1", "card0-DP-1", "card1-HDMI-A-1", "renderD128", "version"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}

	cards, err := enumerateDRMCards()
	if err != nil {
		t.Fatalf("enumerateDRMCards() error = %v", err)
	}
	if len(cards) != 2 || cards[0] != "card0" || cards[1] != "card1" {
		t.Errorf("got %v, want [card0 card1]", cards)
	}
}

// TestEnumerateDRMCards_NoDRMSubsystem verifies a kernel built without DRM is
// not treated as an error. Headless servers and containers routinely have no
// /sys/class/drm at all.
func TestEnumerateDRMCards_NoDRMSubsystem(t *testing.T) {
	withDRMRoot(t, filepath.Join(t.TempDir(), "absent"))

	cards, err := enumerateDRMCards()
	if err != nil {
		t.Errorf("enumerateDRMCards() error = %v, want nil for a missing subsystem", err)
	}
	if len(cards) != 0 {
		t.Errorf("got %v, want no cards", cards)
	}
}

// TestGPUCollect_AMDAndIntelCards verifies both sysfs-based vendors are
// collected and that the result is ordered by DRM index.
func TestGPUCollect_AMDAndIntelCards(t *testing.T) {
	root := t.TempDir()
	withDRMRoot(t, root)

	makeDRMCard(t, root, "card0", vendorAMD, map[string]string{
		"gpu_busy_percent":    "42",
		"mem_busy_percent":    "17",
		"mem_info_vram_total": "8589934592", // 8 GiB
		"mem_info_vram_used":  "2147483648", // 2 GiB
	})
	makeDRMCard(t, root, "card1", vendorIntel, nil)

	c := NewGPUCollector(quietLogger())
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	gpus := c.Info()
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2: %+v", len(gpus), gpus)
	}
	if gpus[0].Index != 0 || gpus[1].Index != 1 {
		t.Errorf("indices = %d, %d; want 0, 1", gpus[0].Index, gpus[1].Index)
	}

	amd := gpus[0]
	if amd.ComputeMode != "AMD" {
		t.Errorf("card0 ComputeMode = %q, want AMD", amd.ComputeMode)
	}
	if amd.GPUUtilPercent != 42 {
		t.Errorf("card0 GPUUtilPercent = %v, want 42", amd.GPUUtilPercent)
	}
	if amd.MemoryTotalMiB != 8192 || amd.MemoryUsedMiB != 2048 {
		t.Errorf("card0 VRAM = %d used of %d MiB, want 2048 of 8192",
			amd.MemoryUsedMiB, amd.MemoryTotalMiB)
	}
	if amd.MemoryPercent != 25 {
		t.Errorf("card0 MemoryPercent = %v, want 25", amd.MemoryPercent)
	}
	if amd.PCIBusID != "0000:03:00.0" {
		t.Errorf("card0 PCIBusID = %q, want 0000:03:00.0", amd.PCIBusID)
	}

	if gpus[1].ComputeMode != "Intel" {
		t.Errorf("card1 ComputeMode = %q, want Intel", gpus[1].ComputeMode)
	}
}

// TestGPUCollect_UnknownVendorIsSkipped verifies a card from a vendor with no
// collector is left out rather than reported with empty metrics.
func TestGPUCollect_UnknownVendorIsSkipped(t *testing.T) {
	root := t.TempDir()
	withDRMRoot(t, root)

	makeDRMCard(t, root, "card0", "0xffff", nil) // no such vendor
	makeDRMCard(t, root, "card1", vendorAMD, map[string]string{"gpu_busy_percent": "5"})

	c := NewGPUCollector(quietLogger())
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	gpus := c.Info()
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1 (the unknown vendor should be skipped): %+v", len(gpus), gpus)
	}
	if gpus[0].ComputeMode != "AMD" {
		t.Errorf("kept the wrong card: %+v", gpus[0])
	}
}

// TestGPUCollect_NoCards verifies a host with no GPU reports an empty list
// rather than nil, which would serialise as JSON null.
func TestGPUCollect_NoCards(t *testing.T) {
	withDRMRoot(t, t.TempDir())

	c := NewGPUCollector(quietLogger())
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if gpus := c.Info(); gpus == nil || len(gpus) != 0 {
		t.Errorf("Info() = %v, want an empty non-nil slice", gpus)
	}
}

// TestGPUCollect_UnreadableDRMRoot verifies an unreadable DRM directory leaves
// the collector reporting no GPUs instead of failing the whole snapshot.
func TestGPUCollect_UnreadableDRMRoot(t *testing.T) {
	// A regular file where a directory is expected makes ReadDir fail with
	// something other than "not exist".
	notADir := filepath.Join(t.TempDir(), "drm")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withDRMRoot(t, notADir)

	c := NewGPUCollector(quietLogger())
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if gpus := c.Info(); len(gpus) != 0 {
		t.Errorf("Info() = %v, want no GPUs", gpus)
	}
}
