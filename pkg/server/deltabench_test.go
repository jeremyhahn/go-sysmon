package server

import (
	"compress/gzip"
	"encoding/json"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// benchSnapshot builds a snapshot of a size this tool actually produces: a
// 24-core host with a thousand processes is about 270 kB of JSON, and three
// quarters of that is the process list.
func benchSnapshot(uptime uint64, busy int) *types.Snapshot {
	return benchSnapshotSized(uptime, busy, 1000)
}

// benchSnapshotSized varies the process count, which is what the size of a
// snapshot is mostly made of: a monitoring appliance runs a couple of hundred
// processes and a workstation a thousand, and the cost of diffing follows.
func benchSnapshotSized(uptime uint64, busy, processCount int) *types.Snapshot {
	return benchSnapshotOrdered(uptime, busy, processCount, 0)
}

// benchSnapshotOrdered also rotates the process list.
//
// The real list is ordered by CPU usage and therefore reorders on almost every
// tick, which is the case that decides whether this whole design is worth
// anything: matched by position, a rotation makes every surviving process look
// changed. A fixture that always lists processes in the same order would be
// diffed just as sparsely with the identities ignored, and would prove nothing.
func benchSnapshotOrdered(uptime uint64, busy, processCount, rotate int) *types.Snapshot {
	cpus := make([]types.CPUInfo, 24)
	for i := range cpus {
		cpus[i] = types.CPUInfo{
			Index: i, CoreID: "0", ModelName: "AMD Ryzen 9 9950X 16-Core Processor",
			UsagePercent: float64((int(uptime)*7 + i) % 100),
			Flags:        []string{"fpu", "vme", "de", "pse", "tsc", "msr", "pae", "mce", "cx8", "apic"},
		}
	}
	processes := make([]types.ProcessDetail, processCount)
	for i := range processes {
		cpu := 0.1
		if i == busy {
			cpu = 91.5
		}
		processes[i] = types.ProcessDetail{
			PID: int32(i + 1), Name: "worker", Username: "jhahn", Status: "sleep",
			CPUPercent: cpu, MemoryBytes: uint64(1<<20) + uint64(i),
		}
	}
	if rotate > 0 && processCount > 0 {
		at := rotate % processCount
		processes = append(processes[at:], processes[:at]...)
	}
	return &types.Snapshot{
		Host:      types.HostInfo{Hostname: "gpu-host", Uptime: uptime},
		CPUs:      cpus,
		Processes: types.ProcessSummary{Total: len(processes), Processes: processes},
		Disks:     []types.DiskInfo{{Name: "nvme0n1", TotalBytes: 1 << 42, UsedPercent: 41}},
		Networks:  []types.NetworkInfo{{Name: "eno1", IsUp: true, BytesRecvRate: float64(uptime)}},
	}
}

// BenchmarkFrameEncoding compares what one tick costs under each protocol.
//
// The old stream encoded a whole snapshot separately for every connection. The
// hub encodes once, diffs once, and hands the same bytes to everyone -- so the
// comparison that matters is "one full encode per client" against "one full
// encode plus one decode and diff, for all clients".
func BenchmarkFrameEncoding(b *testing.B) {
	before := benchSnapshot(100, 3)
	after := benchSnapshot(101, 7)

	// What the old stream paid for ONE connection on ONE tick: encode the
	// snapshot, then push it through that connection's gzip stream.
	b.Run("full-per-client", func(b *testing.B) {
		b.ReportAllocs()
		var sink discardWriter
		gz := gzip.NewWriter(&sink)
		for i := 0; i < b.N; i++ {
			raw, err := json.Marshal(after)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := gz.Write(raw); err != nil {
				b.Fatal(err)
			}
			if err := gz.Flush(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(raw)))
		}
	})

	// The same, without compression, for a client that did not ask for it.
	b.Run("full-per-client-plain", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw, err := json.Marshal(after)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(raw)))
		}
	})

	// And what one delta costs a connection once the hub has built it.
	b.Run("delta-per-client", func(b *testing.B) {
		rawBefore, err := json.Marshal(before)
		if err != nil {
			b.Fatal(err)
		}
		rawAfter, err := json.Marshal(after)
		if err != nil {
			b.Fatal(err)
		}
		base, err := delta.Decode(rawBefore)
		if err != nil {
			b.Fatal(err)
		}
		current, err := delta.Decode(rawAfter)
		if err != nil {
			b.Fatal(err)
		}
		patch, err := patchEnvelope(1, base, current)
		if err != nil {
			b.Fatal(err)
		}

		b.ResetTimer()
		b.ReportAllocs()
		var sink discardWriter
		gz := gzip.NewWriter(&sink)
		for i := 0; i < b.N; i++ {
			if _, err := gz.Write(patch); err != nil {
				b.Fatal(err)
			}
			if err := gz.Flush(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(patch)))
		}
	})

	// The appliance case: a few hundred processes rather than a thousand.
	b.Run("diff-once-small-host", func(b *testing.B) {
		b.ReportAllocs()
		small := benchSnapshotSized(100, 3, 150)
		smallAfter := benchSnapshotSized(101, 7, 150)
		rawBefore, err := json.Marshal(small)
		if err != nil {
			b.Fatal(err)
		}
		base, err := delta.Decode(rawBefore)
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < b.N; i++ {
			raw, err := json.Marshal(smallAfter)
			if err != nil {
				b.Fatal(err)
			}
			current, err := delta.Decode(raw)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := patchEnvelope(uint64(i), base, current); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("diff-once", func(b *testing.B) {
		b.ReportAllocs()
		rawBefore, err := json.Marshal(before)
		if err != nil {
			b.Fatal(err)
		}
		base, err := delta.Decode(rawBefore)
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < b.N; i++ {
			raw, err := json.Marshal(after)
			if err != nil {
				b.Fatal(err)
			}
			current, err := delta.Decode(raw)
			if err != nil {
				b.Fatal(err)
			}
			patch, err := patchEnvelope(uint64(i), base, current)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(patch)))
		}
	})
}

// TestDeltaFrameSize pins the saving this protocol exists for.
//
// It is an assertion rather than a benchmark because the ratio is the feature:
// a change that quietly stopped the diff from being sparse -- an array losing
// its identity, a field that re-encodes differently every tick -- would leave
// every other test passing while the stream went back to sending whole
// snapshots.
func TestDeltaFrameSize(t *testing.T) {
	for _, processes := range []int{150, 1000} {
		before := benchSnapshotOrdered(100, 3, processes, 0)
		after := benchSnapshotOrdered(101, 7, processes, processes/3)

		rawBefore, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		rawAfter, err := json.Marshal(after)
		if err != nil {
			t.Fatal(err)
		}
		base, err := delta.Decode(rawBefore)
		if err != nil {
			t.Fatal(err)
		}
		current, err := delta.Decode(rawAfter)
		if err != nil {
			t.Fatal(err)
		}
		patch, err := patchEnvelope(1, base, current)
		if err != nil {
			t.Fatal(err)
		}

		share := 100 * float64(len(patch)) / float64(len(rawAfter))
		t.Logf("%d processes: full %d bytes, delta %d bytes (%.2f%%)",
			processes, len(rawAfter), len(patch), share)

		// One busy process and a moved uptime. Anything approaching a tenth of
		// the snapshot means the diff has stopped being sparse.
		if share > 10 {
			t.Errorf("%d processes: the delta is %.1f%% of the snapshot; the diff is "+
				"no longer sparse", processes, share)
		}
	}
}

// discardWriter counts bytes without keeping them, so a benchmark measures the
// compressor rather than the allocator behind it.
type discardWriter struct{ n int }

func (w *discardWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}
