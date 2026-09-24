//go:build unix

package fleet

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// Wall time is the wrong instrument for an aggregation cycle, and reporting it
// alone caused a real mistake rather than a theoretical one.
//
// BenchmarkPoller_Refresh fetches every peer concurrently, so its ns/op is
// roughly the slowest round trip and understates the CPU by the concurrency
// factor. #76 took that 4.0 ms/op, scaled it to mon0, and concluded aggregation
// was far too small to explain that node's 228 ms/s. Measured on the deployed
// node instead -- sampling /proc every 50 ms and folding the series on the poll
// period -- one cycle costs 744 ms of CPU and aggregation is 94% of the
// process's CPU. The reasoning was sound; the number it started from was a wall
// clock.
//
// So the figure that matters is reported here rather than left to be derived.
//
//	make bench-fleet

// processCPU reports the CPU this process has consumed, user plus system,
// across every thread.
//
// RUSAGE_SELF covers the whole process, and the fixture's peers are httptest
// servers inside it, so each cycle is also charged for encoding and writing the
// bodies a real peer would have written on its own CPU. The result is therefore
// an upper bound on the aggregator's own cost, which is the honest direction
// for it to err in.
func processCPU(tb testing.TB) time.Duration {
	tb.Helper()

	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		tb.Fatalf("getrusage(RUSAGE_SELF): %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkPoller_RefreshCPU measures one aggregation cycle in CPU time.
//
// It reports two custom metrics beside the usual ones:
//
//	ms_cpu/op    CPU consumed by one aggregation cycle
//	ms_cpu/s@3s  the same amortised over the deployed 3s poll interval
//
// The second is the number to compare against a node's measured CPU. Comparing
// ns/op against it is the error this benchmark exists to prevent.
func BenchmarkPoller_RefreshCPU(b *testing.B) {
	for _, shape := range pollShapes {
		b.Run(shape.name, func(b *testing.B) {
			p, cfg, written := benchFleet(b, benchPeerCount, shape)

			// Warm the peers so connection setup and the first decode's
			// allocations are not charged to the measured cycles.
			benchCycle(p, cfg, shape)
			written.Store(0)

			cycles := 0
			start := processCPU(b)
			b.ResetTimer()
			for b.Loop() {
				benchCycle(p, cfg, shape)
				cycles++
			}
			b.StopTimer()
			used := processCPU(b) - start

			if cycles == 0 {
				b.Fatal("no cycles ran; the benchmark measured nothing")
			}
			perCycle := float64(used) / float64(cycles)
			b.ReportMetric(perCycle/float64(time.Millisecond), "ms_cpu/op")
			b.ReportMetric(perCycle/float64(time.Millisecond)/3.0, "ms_cpu/s@3s")
			b.ReportMetric(float64(written.Load())/float64(cycles)/1024, "KB/cycle")
		})
	}
}

// TestProcessCPU_Advances is the guard on the benchmark's headline figure: a
// processCPU that returned zero, or stood still, would leave the benchmark
// reporting a confidently wrong number, which is the defect class #76 is about.
func TestProcessCPU_Advances(t *testing.T) {
	before := processCPU(t)
	if before <= 0 {
		t.Fatalf("processCPU returned %v before any work; want a positive duration", before)
	}

	// Busy work, a chunk per check, until the CPU clock moves. Burning for a
	// fixed wall-clock window instead would let a starved runner schedule the
	// loop so little that the clock had not visibly moved when it ended.
	sink := 0
	await.Until(t, "processCPU to advance under busy work", func(context.Context) bool {
		for range 1 << 20 {
			sink++
		}
		return processCPU(t) > before
	})
	if sink == 0 {
		t.Fatal("busy loop did not run")
	}
}

// TestProcessCPU_IsTheWholeProcessRusage is the other half: the figure must be
// the kernel's CPU accounting for the whole process -- user plus system, every
// thread -- read between two independent readings of that same accounting. A
// processCPU tracking the wall clock lands outside the bracket, as does one
// reporting only the calling thread, while the runtime's other threads have
// used CPU. Waiting for wall time to pass without CPU would show the first
// directly, but only by sleeping, and how much CPU the runtime's own
// goroutines accrue meanwhile is up to the scheduler.
func TestProcessCPU_IsTheWholeProcessRusage(t *testing.T) {
	rusage := func() time.Duration {
		var ru syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
			t.Fatalf("getrusage(RUSAGE_SELF): %v", err)
		}
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}

	low := rusage()
	got := processCPU(t)
	high := rusage()
	if got < low || got > high {
		t.Errorf("processCPU = %v, outside the process's CPU time read around it [%v, %v]", got, low, high)
	}
	if got <= 0 {
		t.Errorf("processCPU = %v; the process has used CPU to get here", got)
	}
}
