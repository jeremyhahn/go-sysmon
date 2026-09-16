package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// accelNodePrefix is the device-node path every accel-class character device
// lives under, and so the marker that identifies an open accelerator file
// descriptor.
const accelNodePrefix = "/dev/accel/"

// npuClientSample is one process's use of an accelerator as reported by the
// DRM usage-stats fdinfo interface.
type npuClientSample struct {
	pid      int
	name     string
	clientID string
	// key identifies this client across samples so its cumulative engine time
	// can be differenced. It is drm-client-id where the driver publishes one --
	// the identity the spec provides, stable across the client's file
	// descriptors -- and pid:fd where it does not, which is the best available
	// and is stable for as long as the descriptor is.
	key         string
	engines     map[string]uint64 // engine name -> cumulative busy nanoseconds
	memoryBytes uint64
}

// collectAccelClients finds every process holding an accel device open and
// reads its usage from fdinfo, returning samples keyed by both the PCI bus ID
// the driver reports and the device node name, so a caller can match on either.
//
// This is the interface standardised in the kernel's DRM usage-stats
// documentation, so it yields real per-process compute time from any driver
// that implements it, with no per-vendor code. What it cannot do is see other
// users' processes without privilege: readlink on another user's file
// descriptor fails, and those clients are invisible rather than wrong. The
// device's own residency figure does not depend on this, so an unprivileged
// monitor still reports the accelerator's state correctly.
func (c *NPUCollector) collectAccelClients() map[string][]npuClientSample {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		warnOnce(c.logger, "npu:proc", "cannot read process table for NPU clients", "error", err)
		return nil
	}

	// One client may hold several file descriptors on the same device, each
	// reporting identical totals. drm-client-id identifies the client behind
	// them, so counting it once keeps the totals honest.
	seen := make(map[string]struct{})
	byDevice := make(map[string][]npuClientSample)

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(procRoot, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// Not ours to read, or the process exited mid-walk. Either way
			// there is nothing to report and nothing wrong.
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, accelNodePrefix) {
				continue
			}
			sample, pdev, ok := readAccelFdinfo(filepath.Join(procRoot, e.Name(), "fdinfo", fd.Name()))
			if !ok {
				continue
			}
			sample.pid = pid
			sample.name = processComm(filepath.Join(procRoot, e.Name(), "comm"))

			key := sample.clientID
			if key == "" {
				// A driver that omits drm-client-id leaves the file descriptor
				// as the only identity available.
				key = e.Name() + ":" + fd.Name()
			}
			if _, dup := seen[pdev+"/"+key]; dup {
				continue
			}
			seen[pdev+"/"+key] = struct{}{}
			sample.key = key

			node := strings.TrimPrefix(target, accelNodePrefix)
			byDevice[node] = append(byDevice[node], sample)
			if pdev != "" {
				byDevice[pdev] = append(byDevice[pdev], sample)
			}
		}
	}
	return byDevice
}

// readAccelFdinfo parses one fdinfo file for DRM usage-stats keys, returning
// the sample and the PCI bus ID the driver reported.
func readAccelFdinfo(path string) (sample npuClientSample, pdev string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return npuClientSample{}, "", false
	}
	sample.engines = make(map[string]uint64)
	var totalMemory, regionMemory uint64

	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || !strings.HasPrefix(key, "drm-") {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case key == "drm-client-id":
			sample.clientID = value
		case key == "drm-pdev":
			pdev = formatPCIBusID(value)
		case strings.HasPrefix(key, "drm-engine-"):
			// "drm-engine-npu: 1234567 ns"
			sample.engines[strings.TrimPrefix(key, "drm-engine-")] = parseFdinfoNumber(value)
		case key == "drm-total-memory":
			totalMemory = parseFdinfoBytes(value)
		case strings.HasPrefix(key, "drm-total-"):
			regionMemory += parseFdinfoBytes(value)
		}
	}
	// drm-total-memory, where a driver reports it, already covers every region;
	// summing the per-region keys as well would double-count.
	sample.memoryBytes = totalMemory
	if sample.memoryBytes == 0 {
		sample.memoryBytes = regionMemory
	}
	return sample, pdev, true
}

// parseFdinfoNumber reads the leading integer of an fdinfo value, discarding
// any unit suffix.
func parseFdinfoNumber(value string) uint64 {
	field, _, _ := strings.Cut(value, " ")
	v, err := strconv.ParseUint(field, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseFdinfoBytes reads an fdinfo memory value, honouring the KiB and MiB
// suffixes the usage-stats interface permits. Values carry no suffix when they
// are already in bytes.
func parseFdinfoBytes(value string) uint64 {
	field, unit, _ := strings.Cut(value, " ")
	v, err := strconv.ParseUint(field, 10, 64)
	if err != nil {
		return 0
	}
	switch strings.TrimSpace(unit) {
	case "KiB":
		return v * 1024
	case "MiB":
		return v * 1024 * 1024
	case "GiB":
		return v * 1024 * 1024 * 1024
	default:
		return v
	}
}

// processComm reads a process name from its comm file.
func processComm(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
