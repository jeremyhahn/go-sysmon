package monitor_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// stubSnapshotter is a test double that returns a fixed snapshot or error.
type stubSnapshotter struct {
	mu       sync.Mutex
	snapshot *types.Snapshot
	err      error
	calls    int
}

func (s *stubSnapshotter) Snapshot() (*types.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.snapshot, nil
}

func (s *stubSnapshotter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newStub(hostname string) *stubSnapshotter {
	return &stubSnapshotter{
		snapshot: &types.Snapshot{
			Host: types.HostInfo{Hostname: hostname},
		},
	}
}

// ---- New ----------------------------------------------------------------

func TestNew_ValidInterval(t *testing.T) {
	stub := newStub("host")
	m, err := monitor.New(stub, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil monitor")
	}
}

func TestNew_ZeroInterval(t *testing.T) {
	stub := newStub("host")
	_, err := monitor.New(stub, 0)
	if err == nil {
		t.Fatal("expected error for zero interval")
	}
	var ivErr *types.InvalidIntervalError
	if !errors.As(err, &ivErr) {
		t.Fatalf("expected InvalidIntervalError, got %T: %v", err, err)
	}
}

func TestNew_NegativeInterval(t *testing.T) {
	stub := newStub("host")
	_, err := monitor.New(stub, -time.Second)
	var ivErr *types.InvalidIntervalError
	if !errors.As(err, &ivErr) {
		t.Fatalf("expected InvalidIntervalError, got %T: %v", err, err)
	}
}

// ---- Start / Stop -------------------------------------------------------

func TestStart_BecomesRunning(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, 50*time.Millisecond)

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	// Wait for at least one poll to happen.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if stub.callCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("snapshotter was never called after Start")
}

func TestStart_AlreadyRunning(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, 50*time.Millisecond)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	err := m.Start()
	if err == nil {
		t.Fatal("expected error on double Start")
	}
	var alreadyErr *types.MonitorAlreadyRunningError
	if !errors.As(err, &alreadyErr) {
		t.Fatalf("expected MonitorAlreadyRunningError, got %T: %v", err, err)
	}
}

func TestStop_WhenRunning(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, 20*time.Millisecond)
	_ = m.Start()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Record calls immediately after stop.
	before := stub.callCount()
	time.Sleep(60 * time.Millisecond)
	after := stub.callCount()

	if after > before+1 {
		t.Errorf("poll loop still running after Stop: calls jumped from %d to %d", before, after)
	}
}

func TestStop_WhenNotRunning(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Second)

	err := m.Stop()
	if err == nil {
		t.Fatal("expected error stopping a non-running monitor")
	}
	var notRunErr *types.MonitorNotRunningError
	if !errors.As(err, &notRunErr) {
		t.Fatalf("expected MonitorNotRunningError, got %T: %v", err, err)
	}
}

// ---- Snapshot -----------------------------------------------------------

func TestSnapshot_BeforeAnyPoll(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Hour) // long interval – won't tick in test

	_, err := m.Snapshot()
	if err == nil {
		t.Fatal("expected error before any snapshot is available")
	}
	var notRunErr *types.MonitorNotRunningError
	if !errors.As(err, &notRunErr) {
		t.Fatalf("expected MonitorNotRunningError, got %T: %v", err, err)
	}
}

func TestSnapshot_AfterPoll(t *testing.T) {
	stub := newStub("expected-host")
	m, _ := monitor.New(stub, 20*time.Millisecond)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	var snap *types.Snapshot
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s, err := m.Snapshot()
		if err == nil {
			snap = s
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if snap == nil {
		t.Fatal("snapshot never became available")
	}
	if snap.Host.Hostname != "expected-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "expected-host")
	}
}

// ---- Subscribe / Unsubscribe --------------------------------------------

func TestSubscribe_ReceivesSnapshots(t *testing.T) {
	stub := newStub("streamed")
	m, _ := monitor.New(stub, 20*time.Millisecond)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)

	select {
	case snap := <-sub.Ch:
		if snap == nil {
			t.Fatal("received nil snapshot")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for snapshot")
	}
}

func TestSubscribe_UniqueIDs(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Hour)
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	a := m.Subscribe()
	b := m.Subscribe()
	defer m.Unsubscribe(a.ID)
	defer m.Unsubscribe(b.ID)

	if a.ID == b.ID {
		t.Errorf("expected unique IDs, both are %d", a.ID)
	}
}

func TestUnsubscribe_ClosesDone(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Hour)

	sub := m.Subscribe()
	m.Unsubscribe(sub.ID)

	select {
	case <-sub.Done:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Done channel not closed after Unsubscribe")
	}
}

func TestUnsubscribe_Idempotent(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Hour)

	sub := m.Subscribe()
	m.Unsubscribe(sub.ID)

	// Second call must not panic.
	m.Unsubscribe(sub.ID)
}

func TestStop_ClosesSubscriberDone(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, 20*time.Millisecond)
	_ = m.Start()

	sub := m.Subscribe()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-sub.Done:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Done channel not closed after Stop")
	}
}

// ---- Multiple subscribers -----------------------------------------------

func TestBroadcast_AllSubscribersReceive(t *testing.T) {
	stub := newStub("broadcast")
	m, _ := monitor.New(stub, 20*time.Millisecond)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	const count = 5
	subs := make([]*monitor.Subscriber, count)
	for i := range subs {
		subs[i] = m.Subscribe()
		t.Cleanup(func() { m.Unsubscribe(subs[i].ID) })
	}

	for i, sub := range subs {
		select {
		case snap := <-sub.Ch:
			if snap == nil {
				t.Errorf("subscriber %d received nil snapshot", i)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("subscriber %d timed out", i)
		}
	}
}

// ---- Slow consumer (drop) -----------------------------------------------

func TestBroadcast_SlowSubscriberDoesNotBlock(t *testing.T) {
	stub := newStub("slow")
	// Fast interval to fill up the subscriber's buffer quickly.
	m, _ := monitor.New(stub, 5*time.Millisecond)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	// Register a subscriber but never read from it.
	_ = m.Subscribe()

	// The monitor should continue polling without deadlocking.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if stub.callCount() < 5 {
		t.Errorf("expected monitor to keep polling with slow subscriber, got %d calls", stub.callCount())
	}
}

// ---- SetInterval --------------------------------------------------------

func TestSetInterval_Valid(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Second)

	if err := m.SetInterval(5 * time.Second); err != nil {
		t.Fatalf("SetInterval: unexpected error: %v", err)
	}
}

func TestSetInterval_InvalidZero(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Second)

	err := m.SetInterval(0)
	if err == nil {
		t.Fatal("expected error for zero interval")
	}
	var ivErr *types.InvalidIntervalError
	if !errors.As(err, &ivErr) {
		t.Fatalf("expected InvalidIntervalError, got %T: %v", err, err)
	}
}

func TestSetInterval_InvalidNegative(t *testing.T) {
	stub := newStub("host")
	m, _ := monitor.New(stub, time.Second)

	err := m.SetInterval(-time.Second)
	if err == nil {
		t.Fatal("expected error for negative interval")
	}
	var ivErr *types.InvalidIntervalError
	if !errors.As(err, &ivErr) {
		t.Fatalf("expected InvalidIntervalError, got %T: %v", err, err)
	}
}

func TestSetInterval_WhileRunning(t *testing.T) {
	stub := newStub("host")
	// Start with a slow interval so the initial tick doesn't fire during setup.
	m, _ := monitor.New(stub, time.Hour)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	// Switch to a fast interval; the monitor should start polling.
	if err := m.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if stub.callCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("monitor did not poll after SetInterval while running")
}

// ---- Collection error tolerance -----------------------------------------

func TestStart_CollectionErrorDoesNotStop(t *testing.T) {
	stub := &stubSnapshotter{err: errors.New("disk unavailable")}
	m, _ := monitor.New(stub, 20*time.Millisecond)
	_ = m.Start()
	t.Cleanup(func() { m.Stop() }) //nolint:errcheck

	time.Sleep(100 * time.Millisecond)

	// Monitor should still be running and calling the snapshotter.
	if stub.callCount() < 2 {
		t.Errorf("expected repeated calls on error, got %d", stub.callCount())
	}
}
