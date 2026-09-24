package collector

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jaypipes/ghw"
	ghwblock "github.com/jaypipes/ghw/pkg/block"
	psdisk "github.com/shirou/gopsutil/v4/disk"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// diskIOSnapshot records a device's cumulative I/O counters and the instant
// they were sampled, so rates can be expressed per second.
type diskIOSnapshot struct {
	ReadBytes  uint64
	WriteBytes uint64
	IoTime     uint64
	WeightedIo uint64
	At         time.Time
}

// diskPeaks records the highest values seen for a device since the process
// started. Peaks are what reveal a short burst that a point-in-time sample
// lands between.
type diskPeaks struct {
	ReadBytesRate  uint64
	WriteBytesRate uint64
	QueueLength    uint64
	UtilPercent    float64
	UsedPercent    float64
}

// DiskCollector collects disk, partition, I/O, SMART, and temperature data.
type DiskCollector struct {
	disks  atomic.Pointer[[]types.DiskInfo]
	logger *slog.Logger

	// prevIO and peaks are only touched from Collect, which the monitor calls
	// from a single goroutine.
	prevIO map[string]diskIOSnapshot
	peaks  map[string]diskPeaks

	// megaraid is the hardware RAID passthrough's state for the collect cycle
	// in progress. Its type is declared per target, beside the code that uses
	// it: the 64-bit Linux build keeps its controller sessions there, and every
	// other target has an empty type whose begin and end do nothing.
	megaraid megaraidCycle
}

// NewDiskCollector returns a new DiskCollector.
func NewDiskCollector(logger *slog.Logger) *DiskCollector {
	c := &DiskCollector{
		logger: logger,
		prevIO: make(map[string]diskIOSnapshot),
		peaks:  make(map[string]diskPeaks),
	}
	c.disks.Store(&[]types.DiskInfo{})
	return c
}

// sysBlockRoot is the sysfs root for block devices. It is a variable so tests
// can point the collector at a synthetic tree.
var sysBlockRoot = "/sys/block"

// readInflight returns the number of I/O requests currently queued or in
// flight for devName, read from /sys/block/<dev>/inflight. The file holds two
// counters, reads and writes; their sum is the device queue depth.
func readInflight(devName string) uint64 {
	raw := readSysfsString(filepath.Join(sysBlockRoot, devName, "inflight"))
	if raw == "" {
		return 0
	}
	fields := strings.Fields(raw)
	if len(fields) < 2 {
		return 0
	}
	var total uint64
	for _, f := range fields[:2] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0
		}
		total += v
	}
	return total
}

// applyIORates fills in per-second rates, queue depth, utilisation and the
// running peaks for one device.
func (c *DiskCollector) applyIORates(info *types.DiskInfo, now time.Time) {
	// The rate and peak maps are lazily created so a zero-valued DiskCollector
	// stays usable rather than panicking on first write.
	if c.prevIO == nil {
		c.prevIO = make(map[string]diskIOSnapshot)
	}
	if c.peaks == nil {
		c.peaks = make(map[string]diskPeaks)
	}

	info.QueueLength = readInflight(info.Name)

	if prev, ok := c.prevIO[info.Name]; ok {
		elapsed := now.Sub(prev.At).Seconds()
		if elapsed > 0 {
			if info.ReadBytes >= prev.ReadBytes {
				info.ReadBytesRate = uint64(float64(info.ReadBytes-prev.ReadBytes) / elapsed)
			}
			if info.WriteBytes >= prev.WriteBytes {
				info.WriteBytesRate = uint64(float64(info.WriteBytes-prev.WriteBytes) / elapsed)
			}
			// io_time and weighted_io are milliseconds of wall-clock time the
			// device spent busy; util is that share of the sample window and
			// avgqu-sz is the weighted delta over the same window.
			if info.IoTime >= prev.IoTime {
				info.UtilPercent = float64(info.IoTime-prev.IoTime) / (elapsed * 1000) * 100
				if info.UtilPercent > 100 {
					info.UtilPercent = 100
				}
			}
			if info.WeightedIo >= prev.WeightedIo {
				info.AvgQueueLength = float64(info.WeightedIo-prev.WeightedIo) / (elapsed * 1000)
			}
		}
	}

	c.prevIO[info.Name] = diskIOSnapshot{
		ReadBytes:  info.ReadBytes,
		WriteBytes: info.WriteBytes,
		IoTime:     info.IoTime,
		WeightedIo: info.WeightedIo,
		At:         now,
	}

	peak := c.peaks[info.Name]
	if info.ReadBytesRate > peak.ReadBytesRate {
		peak.ReadBytesRate = info.ReadBytesRate
	}
	if info.WriteBytesRate > peak.WriteBytesRate {
		peak.WriteBytesRate = info.WriteBytesRate
	}
	if info.QueueLength > peak.QueueLength {
		peak.QueueLength = info.QueueLength
	}
	if info.UtilPercent > peak.UtilPercent {
		peak.UtilPercent = info.UtilPercent
	}
	if info.UsedPercent > peak.UsedPercent {
		peak.UsedPercent = info.UsedPercent
	}
	c.peaks[info.Name] = peak

	info.PeakReadBytesRate = peak.ReadBytesRate
	info.PeakWriteBytesRate = peak.WriteBytesRate
	info.PeakQueueLength = peak.QueueLength
	info.PeakUtilPercent = peak.UtilPercent
	info.PeakUsedPercent = peak.UsedPercent
}

// Collect refreshes all disk metrics.
func (c *DiskCollector) Collect() error {
	blockInfo, err := ghw.Block()
	if err != nil {
		warnOnce(c.logger, "disk:block-info", "disk: could not collect block device info (may need root)", "error", err)
	}

	partitions, err := psdisk.Partitions(false)
	if err != nil {
		warnOnce(c.logger, "disk:partitions", "disk: could not list partitions", "error", err)
	}

	ioCounters, err := psdisk.IOCounters()
	if err != nil {
		warnOnce(c.logger, "disk:io-counters", "disk: could not collect I/O counters", "error", err)
		ioCounters = nil
	}

	// Index partitions by device name prefix so we can associate them with
	// their parent disk (e.g. /dev/nvme0n1p1 → nvme0n1).
	partitionsByDisk := buildPartitionIndex(partitions)

	// One controller session for the whole cycle: the configuration is read
	// once however many volumes are built on it.
	c.megaraid.begin()
	defer c.megaraid.end()

	var result []types.DiskInfo

	if blockInfo != nil && len(blockInfo.Disks) > 0 {
		result = c.disksFromBlockInfo(blockInfo.Disks, partitionsByDisk, ioCounters)
	} else {
		// Fall back to block devices visible through sysfs when ghw is unavailable.
		result = c.collectFromSysfs(partitionsByDisk, ioCounters)
	}

	c.disks.Store(&result)
	return nil
}

// disksFromBlockInfo turns ghw's block device enumeration into disk records.
//
// It is separate from Collect so the path that actually runs in production can
// be tested: ghw.Block() reads the host, so everything downstream of it used to
// be reachable only on real hardware, and the sysfs fallback -- the path that
// runs almost nowhere -- was the only one with a test.
func (c *DiskCollector) disksFromBlockInfo(
	disks []*ghwblock.Disk,
	partitionsByDisk map[string][]psdisk.PartitionStat,
	ioCounters map[string]psdisk.IOCountersStat,
) []types.DiskInfo {
	result := make([]types.DiskInfo, 0, len(disks))
	for _, d := range disks {
		// ghw enumerates every name in /sys/block, including the ones the
		// kernel created with nothing behind them. The sysfs fallback has
		// always dropped loop and ram devices; this is the same idea on the
		// path that runs, and by medium rather than by name.
		if hasNoMedium(d.Name) {
			continue
		}
		info := c.buildDiskInfo(d.Name, d.Model, d.SerialNumber, d.Vendor,
			d.SizeBytes, d.DriveType.String(), d.StorageController.String(),
			d.StorageController.String(), isRotational(d.Name),
			partitionsByDisk[d.Name], ioCounters)
		info.Index = len(result)
		result = append(result, info)
	}
	return result
}

// buildDiskInfo constructs a DiskInfo, collecting usage, SMART and temperature
// data as best-effort additions.
func (c *DiskCollector) buildDiskInfo(
	name, model, serial, vendor string,
	sizeBytes uint64,
	driveType, controller, transport string,
	rotational bool,
	parts []psdisk.PartitionStat,
	ioCounters map[string]psdisk.IOCountersStat,
) types.DiskInfo {
	info := types.DiskInfo{
		Name:       name,
		Model:      model,
		Serial:     serial,
		Vendor:     vendor,
		SizeBytes:  sizeBytes,
		DriveType:  driveType,
		Controller: controller,
		Transport:  transport,
		Rotational: rotational,
	}

	// Partitions and aggregate usage.
	var totalBytes, usedBytes, freeBytes uint64
	seenMounts := make(map[string]bool)
	for _, p := range parts {
		opts := strings.Join(p.Opts, ",")
		info.Partitions = append(info.Partitions, types.PartitionInfo{
			Device:     p.Device,
			Mountpoint: p.Mountpoint,
			Fstype:     p.Fstype,
			Opts:       opts,
		})
		if p.Mountpoint != "" && !seenMounts[p.Mountpoint] {
			seenMounts[p.Mountpoint] = true
			usage, err := psdisk.Usage(p.Mountpoint)
			if err == nil {
				totalBytes += usage.Total
				usedBytes += usage.Used
				freeBytes += usage.Free
			}
		}
	}
	info.TotalBytes = totalBytes
	info.UsedBytes = usedBytes
	info.FreeBytes = freeBytes
	if totalBytes > 0 {
		info.UsedPercent = float64(usedBytes) / float64(totalBytes) * 100
	}

	// I/O counters.
	if ctr, ok := ioCounters[name]; ok {
		info.ReadCount = ctr.ReadCount
		info.WriteCount = ctr.WriteCount
		info.ReadBytes = ctr.ReadBytes
		info.WriteBytes = ctr.WriteBytes
		info.IoTime = ctr.IoTime
		info.WeightedIo = ctr.WeightedIO
	}

	// Rates, queue depth, utilisation and peaks.
	c.applyIORates(&info, time.Now())

	// SMART (best-effort).
	c.applySMART(name, &info)

	// RAID (md array) status, best-effort. Reads world-readable sysfs, so it
	// works even when the service is unprivileged and SMART is not.
	c.applyRAID(name, &info)

	// Hardware RAID, for a device that is a controller's virtual disk. Defers
	// to md above, and fills in the member drives that have no other way to be
	// seen.
	c.applyHardwareRAID(name, &info)

	// Temperature via hwmon sysfs (best-effort).
	if info.Temperature == 0 {
		temp, err := readHwmonTemp(name)
		if err != nil {
			c.logger.Debug("disk: hwmon temperature unavailable", "device", name, "error", err)
		}
		info.Temperature = temp
	}

	return info
}

// applySMART populates SMART fields using direct ioctl calls.
// It detects whether the device is NVMe or ATA and dispatches accordingly.
// Permission errors are logged as warnings; the disk entry is still returned
// without SMART data so the rest of the system state is visible -- but it now
// carries SMARTStatus saying which kind of absence it is, because "no reading"
// covers four unrelated situations that used to render identically.
func (c *DiskCollector) applySMART(devName string, info *types.DiskInfo) {
	// A device with no hardware under it has nothing to ask. md arrays were
	// already skipped here to spare the ioctls; the sysfs test generalises
	// that to device-mapper nodes, unbound nbd and loop devices, and anything
	// else virtual, without a list of name prefixes to keep in step with the
	// kernel. The name test is kept because it holds even where sysfs does not
	// answer.
	virtual, known := isVirtualBlockDevice(devName)
	if isMDName(devName) || (known && virtual) {
		info.SMARTStatus = types.SMARTNotApplicable
		info.SMARTReason = "a virtual block device: there is no physical medium to ask"
		return
	}
	if strings.HasPrefix(devName, "nvme") {
		c.applyNVMeSMART(devName, info)
	} else {
		c.applyATASMART(devName, info)
	}
}

// recordSMARTReading sets the status that goes with a successful read. It is
// derived from SMARTHealthy rather than set alongside it so the status and the
// boolean cannot disagree.
func recordSMARTReading(info *types.DiskInfo) {
	if info.SMARTHealthy {
		info.SMARTStatus = types.SMARTOK
		return
	}
	info.SMARTStatus = types.SMARTFailing
}

// applyNVMeSMART reads SMART data from an NVMe device via ioctl.
func (c *DiskCollector) applyNVMeSMART(devName string, info *types.DiskInfo) {
	charDev := nvmeNameFromDisk(devName)
	c.applySMARTRead(devName, info, func() error { return readNVMeSMART(charDev, info) })
}

// applyATASMART reads SMART data from an ATA device via ioctl.
func (c *DiskCollector) applyATASMART(devName string, info *types.DiskInfo) {
	devPath := "/dev/" + devName
	c.applySMARTRead(devName, info, func() error { return readATASMART(devPath, info) })
}

// applySMARTRead runs one device's SMART read and records what came of it,
// whichever transport it used. Both callers need identical handling of the
// three outcomes -- a reading, a refusal worth warning about once, and a
// failure to classify -- and having it once is what keeps the status and the
// warning from drifting apart.
func (c *DiskCollector) applySMARTRead(devName string, info *types.DiskInfo, read func() error) {
	err := read()
	if err == nil {
		recordSMARTReading(info)
		return
	}

	classifySMARTFailure(devName, err, info)

	var permErr *ErrSMARTPermission
	if isPermissionError(err, &permErr) {
		// A privilege denial cannot resolve itself mid-run; warn once per
		// device so the condition is visible without repeating.
		warnOnce(c.logger, "disk:smart:"+devName,
			"disk: SMART unavailable", "device", devName,
			"hint", "run with elevated privileges or add user to 'disk' group",
			"note", "this is logged once per device per run")
		return
	}
	c.logger.Debug("disk: SMART read failed", "device", devName, "error", err)
}

// isPermissionError returns true when err is an *ErrSMARTPermission and
// populates dst.
func isPermissionError(err error, dst **ErrSMARTPermission) bool {
	if pe, ok := err.(*ErrSMARTPermission); ok {
		*dst = pe
		return true
	}
	return false
}

// collectFromSysfs enumerates block devices from /sys/block when ghw is
// unavailable or returns no disks.
func (c *DiskCollector) collectFromSysfs(
	partitionsByDisk map[string][]psdisk.PartitionStat,
	ioCounters map[string]psdisk.IOCountersStat,
) []types.DiskInfo {
	entries, err := os.ReadDir(sysBlockRoot)
	if err != nil {
		warnOnce(c.logger, "disk:sys-block", "disk: could not read /sys/block", "error", err)
		return nil
	}

	result := make([]types.DiskInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		if hasNoMedium(name) {
			continue
		}
		sizeBytes := readSysfsUint64(filepath.Join(sysBlockRoot, name, "size")) * 512
		model := readSysfsString(filepath.Join(sysBlockRoot, name, "device", "model"))
		serial := readSysfsString(filepath.Join(sysBlockRoot, name, "device", "serial"))
		vendor := readSysfsString(filepath.Join(sysBlockRoot, name, "device", "vendor"))

		info := c.buildDiskInfo(name, strings.TrimSpace(model), strings.TrimSpace(serial),
			strings.TrimSpace(vendor), sizeBytes,
			"", "", "", isRotational(name),
			partitionsByDisk[name], ioCounters)
		info.Index = len(result)
		result = append(result, info)
	}
	return result
}

// buildPartitionIndex maps a disk base name (e.g. "nvme0n1") to all
// PartitionStats whose device path matches that disk.
func buildPartitionIndex(partitions []psdisk.PartitionStat) map[string][]psdisk.PartitionStat {
	idx := make(map[string][]psdisk.PartitionStat)
	for _, p := range partitions {
		base := filepath.Base(p.Device)
		disk := diskNameFromPartition(base)
		idx[disk] = append(idx[disk], p)
	}
	return idx
}

// pSuffixFamilies are device families whose own names end in digits, so a
// partition is marked with a "p<N>" suffix instead: nvme0n1p1, mmcblk0p2,
// md0p1, dm-0, loop0p1. Stripping trailing digits from these would corrupt the
// device name itself — md0 would become "md" and match no disk, which silently
// hid the filesystem usage of every md array and of the SD card that is root on
// most single-board machines.
var pSuffixFamilies = []string{"nvme", "mmcblk", "md", "dm-", "loop"}

// diskNameFromPartition derives the parent disk name from a partition device
// name. Examples: nvme0n1p1 → nvme0n1, mmcblk0p2 → mmcblk0, sda1 → sda,
// vda2 → vda. A whole device that carries a filesystem directly (md0, sdb)
// maps to itself.
func diskNameFromPartition(part string) string {
	for _, prefix := range pSuffixFamilies {
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		if base, ok := stripPartitionSuffix(part); ok {
			return base
		}
		// No "p<N>" suffix: the name is itself a whole device.
		return part
	}

	// SCSI/VirtIO/IDE: trailing digits are the partition number (sda1 → sda).
	i := len(part) - 1
	for i >= 0 && part[i] >= '0' && part[i] <= '9' {
		i--
	}
	if i >= 0 && i < len(part)-1 {
		return part[:i+1]
	}
	return part
}

// stripPartitionSuffix removes a trailing "p<digits>" partition marker,
// reporting whether one was present.
func stripPartitionSuffix(part string) (string, bool) {
	idx := strings.LastIndex(part, "p")
	if idx <= 0 || idx+1 >= len(part) {
		return part, false
	}
	for _, ch := range part[idx+1:] {
		if ch < '0' || ch > '9' {
			return part, false
		}
	}
	return part[:idx], true
}

// isRotational reads the rotational flag from sysfs.
func isRotational(devName string) bool {
	// Via sysBlockRoot, not a literal /sys/block: a test that points the
	// collector at a fixture tree was still reading the host through this one
	// function, so its answer came from whatever machine happened to run it.
	val := readSysfsString(filepath.Join(sysBlockRoot, devName, "queue", "rotational"))
	return strings.TrimSpace(val) == "1"
}

// readSysfsString reads a single-value sysfs file and returns its trimmed
// content, or an empty string on any error.
func readSysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readSysfsUint64 parses a sysfs file as a uint64.
func readSysfsUint64(path string) uint64 {
	var v uint64
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	// A sysfs attribute that does not hold a number yields the zero value,
	// which is the documented behaviour of this helper.
	if _, err := fmt.Sscan(strings.TrimSpace(string(data)), &v); err != nil {
		return 0
	}
	return v
}

// readHwmonTemp attempts to read a temperature in milli-Celsius from the
// hwmon sysfs entries associated with the given block device name, and returns
// it in Celsius. 0 means the device exposes no hwmon temperature, which is what
// DiskInfo.Temperature documents for a drive that did not report one.
// NVMe devices expose hwmon directly under device/ (e.g. device/hwmon8/),
// while SCSI/ATA devices may use device/hwmon/hwmon*/.
//
// The error is a *types.SysfsReadError when a pattern cannot be searched at
// all: the device name is part of the glob, and a name holding a glob
// metacharacter such as '[' makes the pattern malformed rather than unmatched.
func readHwmonTemp(devName string) (float64, error) {
	deviceDir := filepath.Join(sysBlockRoot, devName, "device")
	patterns := []string{
		filepath.Join(deviceDir, "hwmon*", "temp1_input"),
		filepath.Join(deviceDir, "hwmon", "hwmon*", "temp1_input"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return 0, &types.SysfsReadError{Path: pattern, Cause: err}
		}
		if len(matches) == 0 {
			continue
		}
		raw := readSysfsUint64(matches[0])
		if raw == 0 {
			continue
		}
		return float64(raw) / 1000.0, nil
	}
	return 0, nil
}

// Info returns the most recently collected DiskInfo slice.
func (c *DiskCollector) Info() []types.DiskInfo {
	return *c.disks.Load()
}
