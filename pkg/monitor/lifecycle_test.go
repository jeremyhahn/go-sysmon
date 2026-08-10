package monitor_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// lifecycleStub is a trivial Snapshotter for lifecycle tests.
type lifecycleStub struct{}

func (lifecycleStub) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Host: types.HostInfo{Hostname: "lifecycle"}}, nil
}

// TestSetInterval_ConcurrentWithStart is the regression test for a data race
// between SetInterval writing the interval and the poll loop reading it. Run
// under -race, the old plain time.Duration field failed here.
func TestSetInterval_ConcurrentWithStart(t *testing.T) {
	for i := 0; i < 50; i++ {
		m, err := monitor.New(lifecycleStub{}, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("monitor.New: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if startErr := m.Start(); startErr != nil {
				t.Errorf("Start: %v", startErr)
			}
		}()
		go func() {
			defer wg.Done()
			if setErr := m.SetInterval(20 * time.Millisecond); setErr != nil {
				t.Errorf("SetInterval: %v", setErr)
			}
		}()
		wg.Wait()

		if stopErr := m.Stop(); stopErr != nil {
			t.Errorf("Stop: %v", stopErr)
		}
	}
}

// TestStartStop_Concurrent is the regression test for the lifecycle race where
// Stop could observe running == true before Start assigned cancel, calling a
// nil CancelFunc. Exactly one of the two calls must succeed.
func TestStartStop_Concurrent(t *testing.T) {
	for i := 0; i < 200; i++ {
		m, err := monitor.New(lifecycleStub{}, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("monitor.New: %v", err)
		}

		var wg sync.WaitGroup
		var startErr, stopErr error
		wg.Add(2)
		go func() { defer wg.Done(); startErr = m.Start() }()
		go func() { defer wg.Done(); stopErr = m.Stop() }()
		wg.Wait()

		// Either Stop ran first and failed (nothing to stop), or Start ran
		// first and both succeeded. Both failing would mean Start was lost.
		if startErr != nil {
			t.Fatalf("Start failed: %v", startErr)
		}
		var notRunning *types.MonitorNotRunningError
		if stopErr != nil && !errors.As(stopErr, &notRunning) {
			t.Fatalf("Stop failed with an unexpected error: %v", stopErr)
		}

		// Leave nothing running for the next iteration.
		if stopErr != nil {
			if err := m.Stop(); err != nil {
				t.Fatalf("second Stop failed: %v", err)
			}
		}
	}
}

// TestStop_WaitsForPollLoop verifies Stop does not return while the poll loop
// is still able to broadcast. After Stop, a subscriber must never receive a
// further snapshot.
func TestStop_WaitsForPollLoop(t *testing.T) {
	m, err := monitor.New(lifecycleStub{}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sub := m.Subscribe()

	// Let a few snapshots flow.
	time.Sleep(30 * time.Millisecond)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Drain whatever was already buffered.
	for {
		select {
		case <-sub.Ch:
			continue
		default:
		}
		break
	}

	// Nothing new may arrive now that the loop has exited.
	select {
	case snap, ok := <-sub.Ch:
		if ok && snap != nil {
			t.Error("received a snapshot after Stop returned; the poll loop outlived Stop")
		}
	case <-time.After(50 * time.Millisecond):
	}

	select {
	case <-sub.Done:
	default:
		t.Error("subscriber Done was not closed by Stop")
	}
}

// TestInterval_ReflectsSetInterval verifies the accessor reports the value the
// poll loop will use.
func TestInterval_ReflectsSetInterval(t *testing.T) {
	m, err := monitor.New(lifecycleStub{}, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if got := m.Interval(); got != 100*time.Millisecond {
		t.Errorf("Interval() = %v, want 100ms", got)
	}

	if err := m.SetInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := m.Interval(); got != 250*time.Millisecond {
		t.Errorf("Interval() after SetInterval = %v, want 250ms", got)
	}
}

// TestSetInterval_RejectsNonPositive covers the validation guard.
func TestSetInterval_RejectsNonPositive(t *testing.T) {
	m, err := monitor.New(lifecycleStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}

	for _, d := range []time.Duration{0, -time.Second} {
		err := m.SetInterval(d)
		if err == nil {
			t.Fatalf("SetInterval(%v) = nil, want an error", d)
		}
		var invalid *types.InvalidIntervalError
		if !errors.As(err, &invalid) {
			t.Errorf("SetInterval(%v) error = %T, want *types.InvalidIntervalError", d, err)
		}
	}
	if got := m.Interval(); got != time.Second {
		t.Errorf("a rejected SetInterval changed the interval to %v", got)
	}
}
