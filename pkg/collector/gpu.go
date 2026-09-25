package collector

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// drmCardPattern matches DRM card directories (card0, card1, ...) and skips
// connector entries such as card1-DP-1.
var drmCardPattern = regexp.MustCompile(`^card\d+$`)

// vendorID constants for PCI vendor identification.
const (
	vendorNVIDIA = "0x10de"
	vendorAMD    = "0x1002"
	vendorIntel  = "0x8086"
)

// GPUCollector collects GPU metrics for NVIDIA, AMD, and Intel GPUs.
//
// AMD and Intel GPUs are read from sysfs. NVIDIA GPUs go through NVML in the
// `nvml` build and through sysfs otherwise -- see gpu_nvml.go and
// gpu_nonvml.go, which supply the two implementations of collectNvidiaGPUs. When
// no GPUs are found, Collect is a no-op and Info returns an empty slice without
// error.
type GPUCollector struct {
	logger *slog.Logger
	gpus   atomic.Pointer[[]types.GPUInfo]
}

// NewGPUCollector returns a new GPUCollector ready for use.
func NewGPUCollector(logger *slog.Logger) *GPUCollector {
	c := &GPUCollector{logger: logger}
	empty := []types.GPUInfo{}
	c.gpus.Store(&empty)
	return c
}

// drmCard holds the identity of a single DRM card enumerated from sysfs.
type drmCard struct {
	name     string // e.g. "card1"
	cardPath string // e.g. "/sys/class/drm/card1"
	vendor   string // lowercased hex vendor ID, e.g. "0x10de"
	index    int    // DRM enumeration index (0-based, sorted by card name)
}

// Collect enumerates all DRM cards, identifies their vendor, and collects
// GPU metrics using the appropriate method per vendor. Results are stored
// atomically and returned by Info.
func (c *GPUCollector) Collect(ctx context.Context) error {
	cards, err := enumerateDRMCards()
	if err != nil {
		c.logger.WarnContext(ctx, "failed to enumerate DRM cards", "error", err)
		empty := []types.GPUInfo{}
		c.gpus.Store(&empty)
		return nil
	}
	if len(cards) == 0 {
		empty := []types.GPUInfo{}
		c.gpus.Store(&empty)
		return nil
	}

	// Separate cards by vendor so NVIDIA cards can be batched via NVML.
	var nvidiaCards []drmCard
	var otherCards []drmCard

	for i, name := range cards {
		cardPath := filepath.Join(drmRoot, name)
		vendor := readSysfsString(filepath.Join(cardPath, "device/vendor"))
		vendor = strings.ToLower(strings.TrimSpace(vendor))
		card := drmCard{name: name, cardPath: cardPath, vendor: vendor, index: i}
		if vendor == vendorNVIDIA {
			nvidiaCards = append(nvidiaCards, card)
		} else {
			otherCards = append(otherCards, card)
		}
	}

	gpus := make([]types.GPUInfo, 0, len(cards))

	// Collect NVIDIA GPUs via NVML (batched).
	if len(nvidiaCards) > 0 {
		nvidiaGPUs := c.collectNvidiaGPUs(ctx, nvidiaCards)
		gpus = append(gpus, nvidiaGPUs...)
	}

	// Collect AMD and Intel GPUs via sysfs.
	for _, card := range otherCards {
		switch card.vendor {
		case vendorAMD:
			g := collectAMDGPU(card.cardPath, card.index)
			gpus = append(gpus, g)
		case vendorIntel:
			g := collectIntelGPU(card.cardPath, card.index)
			gpus = append(gpus, g)
		default:
			// A card's PCI vendor cannot change while the process runs, so
			// this is a permanent condition and belongs in warnOnce. It was
			// previously logged every collection cycle, which on a server with
			// a BMC display chip (a Matrox G200, vendor 0x102b, is the common
			// one) produced a warning every few seconds forever.
			warnOnce(c.logger, "gpu:vendor:"+card.name+":"+card.vendor,
				"unknown GPU vendor, skipping",
				"card", card.name, "vendor", card.vendor,
				"note", "this is logged once per card per run")
		}
	}

	// Sort by DRM index so the slice is deterministically ordered.
	sort.Slice(gpus, func(i, j int) bool {
		return gpus[i].Index < gpus[j].Index
	})

	c.gpus.Store(&gpus)
	return nil
}

// Info returns the most recently collected GPU information.
func (c *GPUCollector) Info() []types.GPUInfo {
	return *c.gpus.Load()
}

// drmRoot is where the kernel exposes graphics devices. It is a variable so
// tests can enumerate a synthetic tree containing vendors the build host does
// not have.
var drmRoot = "/sys/class/drm"

// enumerateDRMCards returns the sorted list of DRM card names found in
// drmRoot (e.g. ["card0", "card1"]). Connector entries such as
// "card1-DP-1" are excluded.
func enumerateDRMCards() ([]string, error) {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &types.SysfsReadError{Path: drmRoot, Cause: err}
	}
	var cards []string
	for _, e := range entries {
		if drmCardPattern.MatchString(e.Name()) {
			cards = append(cards, e.Name())
		}
	}
	sort.Strings(cards)
	return cards, nil
}

// normalisePCIBusID extracts and normalises a PCI bus address from either an
// NVML "0000:02:00.0" string or a uevent file containing
// "PCI_SLOT_NAME=0000:02:00.0". Returns the canonical "bus:device.function"
// form (e.g. "02:00.0") with any domain prefix stripped so that NVML
// and sysfs uevent addresses compare equally.
func normalisePCIBusID(s string) string {
	s = strings.TrimSpace(s)
	// uevent format: "PCI_SLOT_NAME=0000:04:00.0"
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			s = strings.TrimPrefix(line, "PCI_SLOT_NAME=")
			break
		}
	}
	// Strip the domain prefix regardless of length. Both NVML
	// ("0000:02:00.0") and sysfs ("0000:02:00.0") include a domain
	// segment before bus:device.function. The canonical form is just
	// "bus:device.function" (2 colon-separated parts).
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) >= 3 {
		// Keep only the last two segments: bus and device.function.
		s = strings.Join(parts[len(parts)-2:], ":")
	}
	return strings.ToLower(s)
}

// pcieGenFromSpeed converts a PCIe link speed string (from sysfs
// current_link_speed / max_link_speed) to a PCIe generation number.
// Known mappings:
//
//	"2.5 GT/s"       → 1
//	"5.0 GT/s"       → 2
//	"8.0 GT/s"       → 3
//	"16.0 GT/s"      → 4
//	"32.0 GT/s"      → 5
//	"64.0 GT/s"      → 6
//
// The sysfs files on some kernels include a trailing " PCIe" qualifier, e.g.
// "8.0 GT/s PCIe". Both forms are handled.
func pcieGenFromSpeed(speed string) int {
	// Strip optional trailing qualifiers such as " PCIe".
	s := strings.TrimSpace(speed)
	if idx := strings.Index(s, " GT/s"); idx >= 0 {
		s = s[:idx]
	} else {
		return 0
	}
	s = strings.TrimSpace(s)
	// Some kernels omit the decimal, so parse as float to be safe.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	switch {
	case f <= 2.6:
		return 1
	case f <= 5.1:
		return 2
	case f <= 8.1:
		return 3
	case f <= 16.1:
		return 4
	case f <= 32.1:
		return 5
	case f <= 64.1:
		return 6
	default:
		return 0
	}
}

// parseActiveDPMClock parses the content of AMD sysfs pp_dpm_sclk or
// pp_dpm_mclk files and returns the currently active clock frequency in MHz.
// The active entry is indicated by a trailing " *" on the line.
//
// Example input:
//
//	0: 300Mhz
//	1: 600Mhz
//	7: 1411Mhz *
func parseActiveDPMClock(data []byte) uint32 {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasSuffix(line, "*") {
			continue
		}
		// Strip the " *" suffix and any trailing whitespace.
		line = strings.TrimSuffix(line, "*")
		line = strings.TrimSpace(line)
		// Format: "<index>: <freq>Mhz" or "<index>: <freq>MHz".
		if idx := strings.Index(line, ":"); idx >= 0 {
			line = strings.TrimSpace(line[idx+1:])
		}
		// Strip MHz/Mhz suffix.
		line = strings.TrimSuffix(line, "MHz")
		line = strings.TrimSuffix(line, "Mhz")
		line = strings.TrimSpace(line)
		v, err := strconv.ParseUint(line, 10, 32)
		if err != nil {
			continue
		}
		return uint32(v)
	}
	return 0
}

// sysfsDeviceName attempts to read a human-readable device name for a PCI
// device directly from sysfs, avoiding any external tool invocation.
//
// It tries, in order:
//  1. /sys/bus/pci/devices/<busID>/label — present on some systems/firmware.
//  2. The PCI_ID field from the device uevent combined with a minimal
//     vendor/device hex string as a last resort.
//
// Returns an empty string if no useful name can be determined.
func sysfsDeviceName(devPath string) string {
	// Try the label file first (firmware-provided human name).
	if label := readSysfsString(filepath.Join(devPath, "label")); label != "" {
		return label
	}
	return ""
}
