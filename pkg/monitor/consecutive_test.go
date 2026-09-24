package monitor_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// countingSnapshotter returns a new snapshot on every call, numbered by its
// timestamp -- the Nth collection is N seconds past epoch0 -- so a test can
// tell which collection it is looking at, from a baseline too.
type countingSnapshotter struct {
	n atomic.Uint64
}

var epoch0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func (c *countingSnapshotter) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Timestamp: epoch0.Add(time.Duration(c.n.Add(1)) * time.Second)}, nil
}

// collection is which collection s is.
func collection(s *types.Snapshot) int64 {
	return int64(s.Timestamp.Sub(epoch0) / time.Second)
}

// consecutive returns the monitor's published pair, failing the test on error.
func consecutive(t *testing.T, m *monitor.Monitor) (prev, cur *types.Snapshot) {
	t.Helper()
	prev, cur, err := m.Consecutive()
	if err != nil {
		t.Fatalf("Consecutive: %v", err)
	}
	return prev, cur
}

// TestConsecutive_PairsEachSnapshotWithTheOneBeforeIt is the property a swap
// rate is computed from: the two snapshots are adjacent collections of the
// same run, never two ticks apart and never mismatched.
func TestConsecutive_PairsEachSnapshotWithTheOneBeforeIt(t *testing.T) {
	m := newMonitor(t, &countingSnapshotter{}, time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)

	// A reader racing the publications: whatever instant it reads at, the
	// pair it gets is two adjacent collections, never mismatched.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			prev, cur, err := m.Consecutive()
			if err == nil && prev != nil && collection(cur) != collection(prev)+1 {
				t.Errorf("Consecutive() = collection %d then %d; want adjacent collections",
					collection(prev), collection(cur))
				return
			}
		}
	}()

	// Collection n has been published once a subscriber receives it, and
	// collection n+1 cannot start before the next tick: the pair is exactly
	// (n-1, n).
	sub := m.Subscribe()
	t.Cleanup(func() { m.Unsubscribe(sub.ID) })
	for n := int64(1); n <= 20; n++ {
		ticker.tick(t)
		await.Recv(t, "the collection to be published", sub.Ch)
		prev, cur := consecutive(t, m)
		if collection(cur) != n {
			t.Fatalf("Consecutive() current = collection %d, want %d", collection(cur), n)
		}
		switch {
		case n == 1 && prev != nil:
			t.Fatalf("the first collection was paired with collection %d", collection(prev))
		case n > 1 && (prev == nil || collection(prev) != n-1):
			t.Fatalf("collection %d was paired with %v; want collection %d", n, prev, n-1)
		}
	}
	close(stop)
	await.Recv(t, "the concurrent reader to finish", done)

	snap, err := m.Snapshot()
	if err != nil || snap == nil {
		t.Fatalf("Snapshot() = %v, %v; want the current reading", snap, err)
	}
}

// TestConsecutive_ARestartHasNoPrevious: after a restart the first collection
// must not be paired with the last one of the run before, which could be any
// length of time earlier. A rate over that gap would be a number describing no
// interval anyone chose.
func TestConsecutive_ARestartHasNoPrevious(t *testing.T) {
	m := newMonitor(t, &countingSnapshotter{}, time.Hour)
	ticker := useManualTicker(m)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Three collections; Stop lets the third, already started, finish.
	for range 3 {
		ticker.tick(t)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if prev, cur := consecutive(t, m); prev == nil || collection(cur) != 3 {
		t.Fatalf("the first run ended on %v paired with %v; want collection 3 with a previous", cur, prev)
	}

	// Exactly one collection in the restarted run: Stop lets the one the tick
	// started finish, and nothing can start another.
	startMonitor(t, m)
	ticker.tick(t)
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	prev, cur := consecutive(t, m)
	if collection(cur) != 4 {
		t.Fatalf("Consecutive() current = collection %d, want the restarted run's first, 4", collection(cur))
	}
	if prev != nil {
		t.Fatalf("after a restart, collection %d was paired with %d from the previous run",
			collection(cur), collection(prev))
	}
}

// TestConsecutive_Errors: the same two answers as Snapshot when there is no
// reading at all.
func TestConsecutive_Errors(t *testing.T) {
	m, err := monitor.New(&countingSnapshotter{}, time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var notRunning *types.MonitorNotRunningError
	if _, _, err := m.Consecutive(); !errors.As(err, &notRunning) {
		t.Errorf("Consecutive() on a monitor never started = %v; want MonitorNotRunningError", err)
	}

	// No tick is delivered, so nothing can have been collected.
	useManualTicker(m)
	startMonitor(t, m)
	var noneYet *types.NoSnapshotYetError
	if _, _, err := m.Consecutive(); !errors.As(err, &noneYet) {
		t.Errorf("Consecutive() before the first collection = %v; want NoSnapshotYetError", err)
	}
}

// TestConsecutive_KeepsOnlyABaselineOfThePrevious: the earlier reading is kept
// for its counters, not whole -- a busy host's process list is not held twice.
func TestConsecutive_KeepsOnlyABaselineOfThePrevious(t *testing.T) {
	s := &fullSnapshotter{}
	m := newMonitor(t, s, time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)
	// The third tick is taken only once the second collection is published.
	for range 3 {
		ticker.tick(t)
	}

	prev, cur := consecutive(t, m)
	if len(cur.Processes.Processes) == 0 {
		t.Fatal("the current snapshot lost its process list")
	}
	if len(prev.Processes.Processes) != 0 || len(prev.CPUs) != 0 {
		t.Errorf("previous reading kept %d processes and %d CPUs; want only its baseline",
			len(prev.Processes.Processes), len(prev.CPUs))
	}
	if prev.Host.Hostname != "" || prev.Host.BootTime != cur.Host.BootTime {
		t.Errorf("baseline host = %+v; want only its boot time", prev.Host)
	}
	if prev.Timestamp.IsZero() || prev.Memory.SwapOutPages == 0 || !prev.Timestamp.Before(cur.Timestamp) {
		t.Errorf("baseline = %v, swap-out %d; want the earlier reading's time and counters",
			prev.Timestamp, prev.Memory.SwapOutPages)
	}
}

// fullSnapshotter returns snapshots with a process list, CPUs and counters
// that advance, a second apart.
type fullSnapshotter struct {
	n atomic.Uint64
}

func (f *fullSnapshotter) Snapshot() (*types.Snapshot, error) {
	n := f.n.Add(1)
	return &types.Snapshot{
		Timestamp: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Second),
		Host:      types.HostInfo{Hostname: "busy", BootTime: 1_790_000_000},
		CPUs:      []types.CPUInfo{{Flags: []string{"fpu"}}},
		Memory:    types.MemoryInfo{SwapOutPages: 100 * n, SwapCountersAvailable: true},
		Processes: types.ProcessSummary{Total: 1, Processes: []types.ProcessDetail{{PID: 1}}},
	}, nil
}
