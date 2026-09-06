//go:build unix

package fleet

import (
	"syscall"
	"testing"
	"time"
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
	p, cfg := benchFleet(b, benchPeerCount)

	// Warm the peers so connection setup and the first decode's allocations are
	// not charged to the measured cycles.
	p.refresh(cfg)

	cycles := 0
	start := processCPU(b)
	b.ResetTimer()
	for b.Loop() {
		p.refresh(cfg)
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
}

// TestProcessCPU_Advances is the guard on the benchmark's headline figure: a
// processCPU that returned wall time, or zero, would leave the benchmark
// reporting a confidently wrong number, which is the defect class #76 is about.
func TestProcessCPU_Advances(t *testing.T) {
	before := processCPU(t)
	if before <= 0 {
		t.Fatalf("processCPU returned %v before any work; want a positive duration", before)
	}

	// Busy work rather than a sleep: sleeping advances the wall clock and not
	// the CPU clock, so this also distinguishes the two.
	deadline := time.Now().Add(20 * time.Millisecond)
	sink := 0
	for time.Now().Before(deadline) {
		sink++
	}
	if sink == 0 {
		t.Fatal("busy loop did not run")
	}

	after := processCPU(t)
	if after <= before {
		t.Errorf("processCPU did not advance across %d iterations of busy work: before=%v after=%v",
			sink, before, after)
	}
}

// TestProcessCPU_DoesNotAdvanceWhileSleeping is the other half: a sleeping
// process burns wall time and no CPU, so a processCPU tracking the wall clock
// fails here even though it would pass the advancing test above.
func TestProcessCPU_DoesNotAdvanceWhileSleeping(t *testing.T) {
	before := processCPU(t)
	time.Sleep(50 * time.Millisecond)
	after := processCPU(t)

	// Not an equality assertion: the runtime's own goroutines keep running, so
	// a little CPU accrues. It must be far below the wall time that elapsed.
	if grew := after - before; grew > 25*time.Millisecond {
		t.Errorf("processCPU grew %v while sleeping 50ms; it is tracking wall time, not CPU", grew)
	}
}
