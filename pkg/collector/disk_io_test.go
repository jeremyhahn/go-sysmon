package collector

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withSysBlockRoot points the disk collector at a synthetic /sys/block.
func withSysBlockRoot(t *testing.T, root string) {
	t.Helper()
	original := sysBlockRoot
	sysBlockRoot = root
	t.Cleanup(func() { sysBlockRoot = original })
}

// ---- readInflight ---------------------------------------------------------

func TestReadInflight_SumsReadsAndWrites(t *testing.T) {
	root := t.TempDir()
	withSysBlockRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "nvme0n1", "inflight"), "       3        5\n")

	if got := readInflight("nvme0n1"); got != 8 {
		t.Errorf("readInflight() = %d, want 8 (3 reads + 5 writes)", got)
	}
}

func TestReadInflight_MalformedOrMissing(t *testing.T) {
	root := t.TempDir()
	withSysBlockRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "bad", "inflight"), "notanumber x\n")
	writeSysfsFile(t, filepath.Join(root, "short", "inflight"), "3\n")

	for _, dev := range []string{"bad", "short", "absent"} {
		if got := readInflight(dev); got != 0 {
			t.Errorf("readInflight(%q) = %d, want 0", dev, got)
		}
	}
}

// ---- applyIORates ---------------------------------------------------------

// TestApplyIORates_FirstSampleHasNoRates verifies a device seen for the first
// time reports no rate instead of its lifetime counters.
func TestApplyIORates_FirstSampleHasNoRates(t *testing.T) {
	withSysBlockRoot(t, t.TempDir())
	c := NewDiskCollector(slog.Default())

	info := types.DiskInfo{Name: "sda", ReadBytes: 1 << 30, WriteBytes: 1 << 30, IoTime: 5000}
	c.applyIORates(&info, time.Now())

	if info.ReadBytesRate != 0 || info.WriteBytesRate != 0 {
		t.Errorf("first sample rates = (%d, %d), want (0, 0)", info.ReadBytesRate, info.WriteBytesRate)
	}
	if info.UtilPercent != 0 {
		t.Errorf("first sample util = %f, want 0", info.UtilPercent)
	}
}

// TestApplyIORates_ComputesPerSecondRates verifies the delta is divided by
// elapsed time, so the value does not change with the polling interval.
func TestApplyIORates_ComputesPerSecondRates(t *testing.T) {
	withSysBlockRoot(t, t.TempDir())
	c := NewDiskCollector(slog.Default())

	start := time.Now()
	first := types.DiskInfo{Name: "sda"}
	c.applyIORates(&first, start)

	// Two seconds later, 200MB read and 100MB written, busy for 1 second.
	second := types.DiskInfo{
		Name:       "sda",
		ReadBytes:  200 << 20,
		WriteBytes: 100 << 20,
		IoTime:     1000,
		WeightedIo: 4000,
	}
	c.applyIORates(&second, start.Add(2*time.Second))

	if want := uint64(100 << 20); second.ReadBytesRate != want {
		t.Errorf("ReadBytesRate = %d, want %d (200MB over 2s)", second.ReadBytesRate, want)
	}
	if want := uint64(50 << 20); second.WriteBytesRate != want {
		t.Errorf("WriteBytesRate = %d, want %d (100MB over 2s)", second.WriteBytesRate, want)
	}
	// 1000ms busy in a 2000ms window is 50% utilisation.
	if second.UtilPercent < 49.9 || second.UtilPercent > 50.1 {
		t.Errorf("UtilPercent = %f, want ~50", second.UtilPercent)
	}
	// 4000ms of weighted time over a 2s window is an average queue of 2.
	if second.AvgQueueLength < 1.99 || second.AvgQueueLength > 2.01 {
		t.Errorf("AvgQueueLength = %f, want ~2", second.AvgQueueLength)
	}
}

// TestApplyIORates_UtilIsClamped guards against a util above 100% when a
// multi-queue device reports more busy time than wall-clock time.
func TestApplyIORates_UtilIsClamped(t *testing.T) {
	withSysBlockRoot(t, t.TempDir())
	c := NewDiskCollector(slog.Default())

	start := time.Now()
	first := types.DiskInfo{Name: "sda"}
	c.applyIORates(&first, start)

	second := types.DiskInfo{Name: "sda", IoTime: 10000} // 10s busy in a 1s window
	c.applyIORates(&second, start.Add(time.Second))

	if second.UtilPercent != 100 {
		t.Errorf("UtilPercent = %f, want it clamped to 100", second.UtilPercent)
	}
}

// TestApplyIORates_CounterResetYieldsZero verifies a counter that goes
// backwards (device re-enumerated) does not produce a huge bogus rate.
func TestApplyIORates_CounterResetYieldsZero(t *testing.T) {
	withSysBlockRoot(t, t.TempDir())
	c := NewDiskCollector(slog.Default())

	start := time.Now()
	first := types.DiskInfo{Name: "sda", ReadBytes: 1 << 30, WriteBytes: 1 << 30}
	c.applyIORates(&first, start)

	second := types.DiskInfo{Name: "sda", ReadBytes: 0, WriteBytes: 0}
	c.applyIORates(&second, start.Add(time.Second))

	if second.ReadBytesRate != 0 || second.WriteBytesRate != 0 {
		t.Errorf("after a counter reset rates = (%d, %d), want (0, 0)",
			second.ReadBytesRate, second.WriteBytesRate)
	}
}

// TestApplyIORates_PeaksPersistAfterBurst is the regression test for peak
// tracking: once a burst has passed, the peak must survive even though the
// current rate has fallen back to zero.
func TestApplyIORates_PeaksPersistAfterBurst(t *testing.T) {
	root := t.TempDir()
	withSysBlockRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "sda", "inflight"), "4 6\n")

	c := NewDiskCollector(slog.Default())
	start := time.Now()

	first := types.DiskInfo{Name: "sda"}
	c.applyIORates(&first, start)

	// A burst: 500MB written in one second.
	burst := types.DiskInfo{Name: "sda", WriteBytes: 500 << 20, UsedPercent: 80}
	c.applyIORates(&burst, start.Add(time.Second))
	burstRate := burst.WriteBytesRate
	if burstRate == 0 {
		t.Fatal("burst produced no write rate")
	}

	// Quiet again: same counters, so the current rate returns to zero.
	quiet := types.DiskInfo{Name: "sda", WriteBytes: 500 << 20, UsedPercent: 10}
	c.applyIORates(&quiet, start.Add(2*time.Second))

	if quiet.WriteBytesRate != 0 {
		t.Errorf("current WriteBytesRate = %d, want 0 once the burst ended", quiet.WriteBytesRate)
	}
	if quiet.PeakWriteBytesRate != burstRate {
		t.Errorf("PeakWriteBytesRate = %d, want the burst rate %d", quiet.PeakWriteBytesRate, burstRate)
	}
	if quiet.PeakQueueLength != 10 {
		t.Errorf("PeakQueueLength = %d, want 10 from the inflight file", quiet.PeakQueueLength)
	}
	if quiet.PeakUsedPercent != 80 {
		t.Errorf("PeakUsedPercent = %f, want 80 even though usage fell to 10", quiet.PeakUsedPercent)
	}
}

// TestApplyIORates_ZeroValueCollector verifies a DiskCollector built without
// its constructor does not panic on first use.
func TestApplyIORates_ZeroValueCollector(t *testing.T) {
	withSysBlockRoot(t, t.TempDir())
	c := &DiskCollector{logger: slog.Default()}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("zero-valued DiskCollector panicked: %v", r)
		}
	}()

	info := types.DiskInfo{Name: "sda"}
	c.applyIORates(&info, time.Now())
}
