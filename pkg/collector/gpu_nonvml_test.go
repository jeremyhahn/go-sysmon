//go:build !nvml

package collector

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// nvidiaCard builds a DRM card tree for an NVIDIA GPU and returns the drmCard
// the collector would hand to collectNvidiaGPUs.
func nvidiaCard(t *testing.T, attrs map[string]string) drmCard {
	t.Helper()
	root := t.TempDir()
	cardPath := filepath.Join(root, "card0")
	dev := filepath.Join(cardPath, "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for k, v := range attrs {
		writeSysfs(t, filepath.Join(dev, k), v)
	}
	return drmCard{name: "card0", cardPath: cardPath, vendor: vendorNVIDIA, index: 0}
}

func noNVMLCollector(t *testing.T) *GPUCollector {
	t.Helper()
	resetWarnOnce()
	return NewGPUCollector(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// TestCollectNvidiaGPUs_NoNVML_ReportsWhatSysfsKnows covers the property that
// makes this an implementation rather than a stub: without NVML the card is still
// identified, named and linked, so a consumer built without cgo sees the hardware
// instead of an empty list.
func TestCollectNvidiaGPUs_NoNVML_ReportsWhatSysfsKnows(t *testing.T) {
	withPCIIDs(t, writeNvidiaPCIIDs(t))
	card := nvidiaCard(t, map[string]string{
		"vendor":             "0x10de",
		"device":             "0x2684",
		"uevent":             "DRIVER=nvidia\nPCI_SLOT_NAME=0000:01:00.0",
		"current_link_speed": "16.0 GT/s PCIe",
		"current_link_width": "16",
		"max_link_speed":     "16.0 GT/s PCIe",
		"max_link_width":     "16",
	})

	got := noNVMLCollector(t).collectNvidiaGPUs(context.Background(), []drmCard{card})
	if len(got) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(got))
	}
	g := got[0]
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"Name", g.Name, "AD102 [GeForce RTX 4090]"},
		{"PCIBusID", g.PCIBusID, "0000:01:00.0"},
		{"DriverVersion", g.DriverVersion, "nvidia"},
		{"ComputeMode", g.ComputeMode, "NVIDIA"},
		{"PCIeGenCurrent", g.PCIeGenCurrent, 4},
		{"PCIeWidthCurrent", g.PCIeWidthCurrent, 16},
		{"PCIeGenMax", g.PCIeGenMax, 4},
		{"PCIeWidthMax", g.PCIeWidthMax, 16},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	// The NVML-only fields stay at zero, which is how this collector already
	// says "unavailable" for AMD and Intel. Reporting anything else here would
	// be inventing numbers no interface supplied.
	if g.MemoryTotalMiB != 0 || g.GPUUtilPercent != 0 || g.ClockGraphicsMHz != 0 || g.ECCEnabled {
		t.Errorf("an NVML-only field was populated without NVML: %+v", g)
	}
}

// TestCollectNvidiaGPUs_NoNVML_DegradedInputs covers a card whose sysfs says
// almost nothing, and a host with no pci.ids database. Neither may produce a
// nameless row or a bogus link.
func TestCollectNvidiaGPUs_NoNVML_DegradedInputs(t *testing.T) {
	t.Run("no pci.ids, no label", func(t *testing.T) {
		withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))
		card := nvidiaCard(t, map[string]string{"vendor": "0x10de", "device": "0x2684"})
		got := noNVMLCollector(t).collectNvidiaGPUs(context.Background(), []drmCard{card})
		// The built-in vendor table still names the maker.
		if got[0].Name == "" {
			t.Error("Name is empty; a card with no database entry must still be identifiable")
		}
	})

	t.Run("firmware label stands in for the model", func(t *testing.T) {
		withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))
		card := nvidiaCard(t, map[string]string{"label": "Tesla T4"})
		got := noNVMLCollector(t).collectNvidiaGPUs(context.Background(), []drmCard{card})
		if got[0].Name != "Tesla T4" {
			t.Errorf("Name = %q, want the firmware label", got[0].Name)
		}
	})

	t.Run("bare card", func(t *testing.T) {
		withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))
		card := nvidiaCard(t, map[string]string{})
		got := noNVMLCollector(t).collectNvidiaGPUs(context.Background(), []drmCard{card})
		if len(got) != 1 {
			t.Fatalf("a card with no attributes must still be reported, got %d", len(got))
		}
		if got[0].Name != "NVIDIA GPU" {
			t.Errorf("Name = %q, want the generic fallback", got[0].Name)
		}
		// An unreadable width must not become the all-ones sentinel's 255 lanes.
		if got[0].PCIeWidthCurrent != 0 || got[0].PCIeWidthMax != 0 {
			t.Errorf("PCIe width = %d/%d, want 0/0 when unreadable",
				got[0].PCIeWidthCurrent, got[0].PCIeWidthMax)
		}
	})

	t.Run("no cards", func(t *testing.T) {
		got := noNVMLCollector(t).collectNvidiaGPUs(context.Background(), nil)
		if len(got) != 0 {
			t.Errorf("expected no GPUs, got %d", len(got))
		}
	})
}

// TestCollectNvidiaGPUs_NoNVML_HwmonIsRead verifies the readings that DO exist
// without NVML are taken: nouveau registers an hwmon node, and leaving it unread
// would discard a real temperature in favour of a zero.
func TestCollectNvidiaGPUs_NoNVML_HwmonIsRead(t *testing.T) {
	withPCIIDs(t, filepath.Join(t.TempDir(), "absent"))
	card := nvidiaCard(t, map[string]string{
		"uevent":                    "DRIVER=nouveau",
		"hwmon/hwmon3/temp1_input":  "61000",
		"hwmon/hwmon3/power1_input": "45000000",
		"hwmon/hwmon3/fan1_input":   "1200",
		"hwmon/hwmon3/fan1_max":     "2400",
	})

	g := noNVMLCollector(t).collectNvidiaGPUs(context.Background(), []drmCard{card})[0]
	if g.TemperatureGPU != 61 {
		t.Errorf("TemperatureGPU = %v, want 61", g.TemperatureGPU)
	}
	if g.PowerDrawW != 45 {
		t.Errorf("PowerDrawW = %v, want 45", g.PowerDrawW)
	}
	if g.FanSpeedPercent != 50 {
		t.Errorf("FanSpeedPercent = %v, want 50", g.FanSpeedPercent)
	}
	if g.DriverVersion != "nouveau" {
		t.Errorf("DriverVersion = %q, want nouveau -- it is what says whether hwmon exists", g.DriverVersion)
	}
}

// writeNvidiaPCIIDs writes a miniature identifier database containing the card
// the first test names.
func writeNvidiaPCIIDs(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pci.ids")
	body := "10de  NVIDIA Corporation\n" +
		"\t2684  AD102 [GeForce RTX 4090]\n" +
		"8086  Intel Corporation\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pci.ids: %v", err)
	}
	return path
}
