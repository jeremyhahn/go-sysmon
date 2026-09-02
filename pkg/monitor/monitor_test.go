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

// ---- Running -------------------------------------------------------------

// TestRunning_FollowsTheLifecycle. The interesting value is the last one: a
// stopped monitor still answers Snapshot with what it last collected and still
// hands out subscriptions, so "is there anything behind this" cannot be
// inferred from either and has to be asked.
func TestRunning_FollowsTheLifecycle(t *testing.T) {
	m, err := monitor.New(newStub("host"), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.Running() {
		t.Error("Running() is true before Start")
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.Running() {
		t.Error("Running() is false while the poll loop is collecting")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.Running() {
		t.Error("Running() is true after Stop")
	}
}

// TestRunning_IsFalseWhileSnapshotStillAnswers pins the pair that made the
// accessor necessary, because a consumer reading Snapshot alone cannot tell a
// live reading from the last one before shutdown -- and a delta stream that
// published it labelled a measurement from before the service stopped as
// current, indefinitely.
func TestRunning_IsFalseWhileSnapshotStillAnswers(t *testing.T) {
	m, err := monitor.New(newStub("host"), 10*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := m.Snapshot(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the monitor never collected a snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if snap, err := m.Snapshot(); err != nil || snap == nil {
		t.Fatalf("Snapshot() after Stop = %v, %v; the cached reading is meant to survive", snap, err)
	}
	if m.Running() {
		t.Fatal("Running() = true after Stop, so a consumer cannot tell the cached " +
			"reading from a live one")
	}

	// And a subscription taken now is never written to and never closed, which
	// is the other half of the trap.
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	select {
	case <-sub.Done:
		t.Error("a subscription taken after Stop was closed; Running() would then be unnecessary")
	case <-time.After(50 * time.Millisecond):
	}
}

// blockingSnapshotter holds the poll loop inside a collection until it is
// released, so a test can be certain where the loop is rather than hoping.
type blockingSnapshotter struct {
	entered chan struct{} // one signal per collection entered
	release chan struct{} // closed to let collections finish
}

func newBlockingSnapshotter() *blockingSnapshotter {
	return &blockingSnapshotter{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingSnapshotter) Snapshot() (*types.Snapshot, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return &types.Snapshot{Host: types.HostInfo{Hostname: "blocked"}}, nil
}

// TestStop_IsAtomicAgainstAConcurrentStart.
//
// Stop used to release the lock before waiting for the poll loop and before
// closing subscribers, so a Start racing it could establish a new running
// monitor while the old Stop was still finishing. A subscriber taken against
// that new generation -- correctly, with Running() true -- was then closed by
// the outgoing Stop's sweep, which does not distinguish generations. The delta
// hub reads a closed subscription as "the monitor stopped", so it ended the
// stream and dropped every connected dashboard on a monitor that was running.
//
// The loop is held inside a collection so this is deterministic rather than a
// race the test hopes to hit: Stop is definitely still waiting when Start is
// called, which is exactly the window.
func TestStop_IsAtomicAgainstAConcurrentStart(t *testing.T) {
	blocking := newBlockingSnapshotter()
	m, err := monitor.New(blocking, time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The poll loop is now inside a collection and cannot exit.
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the poll loop never collected")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop() }()

	// Stop has taken the lock and is waiting for the loop. A Start now must
	// not be able to slip in front of the sweep that is still to come.
	started := make(chan error, 1)
	go func() {
		// Give Stop time to reach its wait; it cannot proceed past it.
		time.Sleep(50 * time.Millisecond)
		started <- m.Start()
	}()

	select {
	case err := <-started:
		t.Fatalf("Start returned (%v) while Stop was still closing subscribers; "+
			"a subscription taken now would be swept by the outgoing stop", err)
	case <-stopped:
		t.Fatal("Stop finished while the poll loop was still held inside a collection")
	case <-time.After(200 * time.Millisecond):
		// Both are blocked, which is the invariant.
	}

	// And the rest of the API is NOT blocked behind it. The wait above is for
	// a collection to finish, and a collection is a syscall into /proc, SMART
	// or NVML that can hang; a stop that held the lock through it would make
	// one wedged collector freeze every Running() in the process -- including
	// the delta hub's, which would stall the stream instead of ending it. The
	// answer it gives is also the true one: the monitor is not collecting.
	answered := make(chan bool, 1)
	go func() { answered <- m.Running() }()
	select {
	case running := <-answered:
		if running {
			t.Error("Running() reports true while a stop is in progress")
		}
	case <-time.After(time.Second):
		t.Fatal("Running() blocked behind a stop that is waiting on a hung collector")
	}

	close(blocking.release)

	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start after Stop completed: %v", err)
	}
	if !m.Running() {
		t.Fatal("the monitor is not running after the Start that followed the Stop")
	}

	// And the generation that is now running is intact: a subscription taken
	// against it was not closed by the stop that preceded it.
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	select {
	case <-sub.Done:
		t.Fatal("a subscription against the running monitor was closed by the previous stop")
	case <-time.After(50 * time.Millisecond):
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
}

// TestGeneration_AdvancesOnEveryStart. The number itself means nothing; what
// matters is that a value stamped before a stop cannot match one read after
// the start, which is what lets a consumer tell "running again" from "running
// the same collection".
func TestGeneration_AdvancesOnEveryStart(t *testing.T) {
	m, err := monitor.New(newStub("host"), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := m.Generation(); got != 0 {
		t.Errorf("Generation() = %d before any Start, want 0", got)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := m.Generation()
	if first == 0 {
		t.Fatal("Generation() did not advance on the first Start")
	}
	if got := m.Generation(); got != first {
		t.Errorf("Generation() changed to %d while the same run was collecting", got)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := m.Generation(); got != first {
		t.Errorf("Generation() = %d after Stop, want the run it is leaving (%d)", got, first)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if got := m.Generation(); got == first {
		t.Fatalf("Generation() = %d after a restart, the same as before it; a value "+
			"stamped before the stop would still match", got)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
}

// TestGeneration_DoesNotAdvanceOnARefusedStart. A Start on a running monitor
// is an error and must not look like a restart to anything comparing the
// number, or a cache would be thrown away on every stray call.
func TestGeneration_DoesNotAdvanceOnARefusedStart(t *testing.T) {
	m, err := monitor.New(newStub("host"), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop() }()

	before := m.Generation()
	var already *types.MonitorAlreadyRunningError
	if err := m.Start(); !errors.As(err, &already) {
		t.Fatalf("second Start on a running monitor = %v, want MonitorAlreadyRunningError", err)
	}
	if got := m.Generation(); got != before {
		t.Errorf("Generation() advanced to %d on a Start that was refused", got)
	}
}

// TestStart_DiscardsThePreviousRunsSnapshot.
//
// Stop deliberately leaves the last collection readable, so a caller can see
// the final state of a monitor that has finished. Start must not let it
// survive into the next run: Snapshot() means "the most recent collection",
// and immediately after a restart the most recent collection of THIS run has
// not happened yet.
//
// It is not an abstract point. The delta hub publishes Snapshot() as the first
// frame of a new stream so a client connecting between ticks is not blank;
// with the old reading still there, a restarted service served a pre-restart
// measurement as the first frame, stamped with the new generation and marked
// current.
func TestStart_DiscardsThePreviousRunsSnapshot(t *testing.T) {
	m, err := monitor.New(newStub("host"), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := m.Snapshot(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the monitor never collected a snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Still readable while it is stopped, which is the deliberate half.
	if _, err := m.Snapshot(); err != nil {
		t.Fatalf("Snapshot() after Stop = %v; the final reading is meant to survive", err)
	}

	// Long enough that the restarted loop cannot have collected before the
	// check below, so this is the clearing rather than a race won.
	if err := m.SetInterval(10 * time.Second); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer func() { _ = m.Stop() }()

	var notRunning *types.MonitorNotRunningError
	if snap, err := m.Snapshot(); !errors.As(err, &notRunning) {
		t.Fatalf("Snapshot() right after a restart = %v, %v; want it to report that "+
			"this run has collected nothing, not the reading from before the stop",
			snap, err)
	}
}
