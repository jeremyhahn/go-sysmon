package collector

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
)

// These benchmarks read the real host, deliberately. The cost of a collection
// tick scales with how much host there is to read -- above all the size of the
// process table -- so a synthetic fixture would measure the parser rather than
// the thing that made a tick cost seconds on the fleet (issue #65). Every
// benchmark reports the host's process count, so a number is never quoted
// without the population it came from.
//
//	make bench-collector
//
// The numbers are only comparable within one host. Comparing them across hosts
// is the mistake that put an Atom D510's figures in the deployment unit.

func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// procCount is the number of processes visible in /proc, which is the
// denominator for every number these benchmarks produce.
func procCount() float64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return float64(n)
}

// benchCollect runs one collector's Collect in a loop and reports the process
// count alongside. A collector that errors on this host is not a benchmark
// failure -- a machine with no GPU or no hardware sensors is a legitimate
// target -- but a collector that errors is also not measuring anything, so it
// is skipped rather than reported as fast.
func benchCollect(b *testing.B, collect func() error) {
	b.Helper()

	if err := collect(); err != nil {
		b.Skipf("collector unavailable on this host: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if err := collect(); err != nil {
			b.Fatalf("collect: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(procCount(), "procs")
}

// The tiers below mirror SystemCollector.Snapshot. What matters is not any one
// figure but the ratio between them: the fast tier runs every tick, the medium
// tier every mediumInterval ticks, and the slow tier every slowInterval, so a
// medium collector costing twenty times a fast one still costs four times as
// much per unit of wall time.

// ---- fast tier: every tick ----------------------------------------------

func BenchmarkHostCollector_Collect(b *testing.B) {
	c := NewHostCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkCPUCollector_Collect(b *testing.B) {
	c := NewCPUCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkMemoryCollector_Collect(b *testing.B) {
	c := NewMemoryCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkNetworkCollector_Collect(b *testing.B) {
	c := NewNetworkCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkLoadCollector_Collect(b *testing.B) {
	c := NewLoadCollector(benchLogger())
	benchCollect(b, c.Collect)
}

// ---- medium tier: every mediumInterval ticks ----------------------------

// BenchmarkProcessCollector_Collect measures one full walk of the process
// table: the single largest component of a tick, and the source of the 197 KB
// of process list in a 223 KB snapshot.
func BenchmarkProcessCollector_Collect(b *testing.B) {
	c := NewProcessCollector(benchLogger())
	benchCollect(b, c.Collect)
}

// BenchmarkVirtCollector_Collect measures the second walk of the same process
// table. VirtCollector lists every process to find the handful running a
// hypervisor, so on a host with no guests this is the cost of proving a
// negative.
func BenchmarkVirtCollector_Collect(b *testing.B) {
	c := NewVirtCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkSensorCollector_Collect(b *testing.B) {
	c := NewSensorCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkGPUCollector_Collect(b *testing.B) {
	c := NewGPUCollector(benchLogger())
	ctx := context.Background()
	benchCollect(b, func() error { return c.Collect(ctx) })
}

// ---- slow tier: every slowInterval ticks --------------------------------

// BenchmarkDiskCollector_Collect is the SMART and RAID path. On a host whose
// drives answer SMART this is the most expensive single collector in the
// process, which is why it is tiered slowest.
func BenchmarkDiskCollector_Collect(b *testing.B) {
	c := NewDiskCollector(benchLogger())
	benchCollect(b, c.Collect)
}

func BenchmarkRuntimeCollector_Collect(b *testing.B) {
	c := NewRuntimeCollector(benchLogger())
	benchCollect(b, c.Collect)
}

// BenchmarkNPUCollector_Collect is dominated by the walk over /proc looking for
// processes holding an accelerator open, which is what the DRM usage-stats
// interface costs: a readlink per file descriptor on the system. The device's
// own sysfs reads are a handful of small files. It is tiered slowest for a
// different reason -- reading a driver's attributes resumes a suspended
// accelerator -- but this is the cost that makes the tier worth having anyway.
func BenchmarkNPUCollector_Collect(b *testing.B) {
	c := NewNPUCollector(benchLogger())
	benchCollect(b, c.Collect)
}

// ---- the whole tick ------------------------------------------------------

// BenchmarkSystemCollector_Snapshot measures a tick as the server takes it,
// with tiering on, so the result is the AVERAGE across the tier cycle and not
// the cost of any single tick. The medium tick is several times this; see the
// individual collectors above for where it goes.
func BenchmarkSystemCollector_Snapshot(b *testing.B) {
	c := NewSystemCollector(benchLogger())
	ctx := context.Background()
	benchCollect(b, func() error {
		_, err := c.Snapshot(ctx)
		return err
	})
}

// BenchmarkSystemCollector_SnapshotUntiered measures the tick with every
// collector running, which is what the one-shot CLI commands do and what the
// medium and slow ticks approach.
func BenchmarkSystemCollector_SnapshotUntiered(b *testing.B) {
	c := NewSystemCollector(benchLogger())
	c.SetTiering(false)
	ctx := context.Background()
	benchCollect(b, func() error {
		_, err := c.Snapshot(ctx)
		return err
	})
}
