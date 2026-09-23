package monitor_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

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

// waitConsecutive polls until the monitor has a previous snapshot to report.
func waitConsecutive(t *testing.T, m *monitor.Monitor) (prev, cur *types.Snapshot) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p, c, err := m.Consecutive()
		if err == nil && p != nil {
			return p, c
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the monitor never published two consecutive snapshots")
	return nil, nil
}

// TestConsecutive_PairsEachSnapshotWithTheOneBeforeIt is the property a swap
// rate is computed from: the two snapshots are adjacent collections of the
// same run, never two ticks apart and never mismatched.
func TestConsecutive_PairsEachSnapshotWithTheOneBeforeIt(t *testing.T) {
	m, err := monitor.New(&countingSnapshotter{}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop() }()

	for range 20 {
		prev, cur := waitConsecutive(t, m)
		if collection(cur) != collection(prev)+1 {
			t.Fatalf("Consecutive() = collection %d then %d; want adjacent collections",
				collection(prev), collection(cur))
		}
	}

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
	m, err := monitor.New(&countingSnapshotter{}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitConsecutive(t, m)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A long interval, so the restarted run collects exactly once, on its
	// first tick, within the test.
	if err := m.SetInterval(50 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer func() { _ = m.Stop() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		prev, cur, err := m.Consecutive()
		if err == nil {
			if prev != nil && collection(cur) != collection(prev)+1 {
				t.Fatalf("after a restart, collection %d was paired with %d from the previous run",
					collection(cur), collection(prev))
			}
			if prev == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("never observed the restarted run's first collection")
		}
		time.Sleep(time.Millisecond)
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

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop() }()
	var noneYet *types.NoSnapshotYetError
	if _, _, err := m.Consecutive(); !errors.As(err, &noneYet) {
		t.Errorf("Consecutive() before the first collection = %v; want NoSnapshotYetError", err)
	}
}

// TestConsecutive_KeepsOnlyABaselineOfThePrevious: the earlier reading is kept
// for its counters, not whole -- a busy host's process list is not held twice.
func TestConsecutive_KeepsOnlyABaselineOfThePrevious(t *testing.T) {
	s := &fullSnapshotter{}
	m, err := monitor.New(s, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop() }()

	prev, cur := waitConsecutive(t, m)
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
