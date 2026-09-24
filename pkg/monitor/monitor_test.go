package monitor_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
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

// newMonitor returns a monitor over s polling every interval.
func newMonitor(t *testing.T, s monitor.Snapshotter, interval time.Duration) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(s, interval)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	return m
}

// startMonitor starts m and stops it when the test ends. A test that stops
// the monitor itself is fine: the cleanup accepts a monitor already stopped.
func startMonitor(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		var notRunning *types.MonitorNotRunningError
		if err := m.Stop(); err != nil && !errors.As(err, &notRunning) {
			t.Errorf("Stop: %v", err)
		}
	})
}

// stopMonitor stops m, failing the test if Stop fails or does not return. It
// runs Stop on its own goroutine so a poll loop wedged in a broadcast fails
// the test instead of hanging it.
func stopMonitor(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- m.Stop() }()
	if err := await.Recv(t, "Stop to return", result); err != nil {
		t.Fatalf("Stop: %v", err)
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
	m := newMonitor(t, stub, time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)

	// The loop taking a tick proves it is running; stopping it then lets the
	// collection that tick started finish, so the count below is final.
	ticker.tick(t)
	stopMonitor(t, m)

	if got := stub.callCount(); got != 1 {
		t.Fatalf("snapshotter called %d times for one tick, want 1", got)
	}
}

func TestStart_AlreadyRunning(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)
	startMonitor(t, m)

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
	m := newMonitor(t, stub, time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)

	// The second tick is taken only once the first collection has finished,
	// and the loop runs the collection the second tick starts before it can
	// notice Stop: two collections, exactly.
	ticker.tick(t)
	ticker.tick(t)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The loop stops its ticker only on its way out, so this proves it has
	// exited -- nothing is left that could collect again.
	if !ticker.isStopped() {
		t.Fatal("Stop returned while the poll loop was still running")
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("snapshotter called %d times, want the 2 the ticks started", got)
	}
}

func TestStop_WhenNotRunning(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Second)

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

// TestSnapshot_NeverStarted covers the first of Snapshot's three answers:
// nothing is collecting and nothing has been collected.
func TestSnapshot_NeverStarted(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)

	_, err := m.Snapshot()
	if err == nil {
		t.Fatal("expected error before any snapshot is available")
	}
	var notRunErr *types.MonitorNotRunningError
	if !errors.As(err, &notRunErr) {
		t.Fatalf("expected MonitorNotRunningError, got %T: %v", err, err)
	}
	var noneYet *types.NoSnapshotYetError
	if errors.As(err, &noneYet) {
		t.Fatal("a monitor that was never started reported that it is merely warming up")
	}
}

// TestSnapshot_StartedAndAwaitingFirstCollection covers the second answer, and
// is the case #33 exists for: the loop is running, no tick has fired, and the
// honest report is "not yet", not "not running". Deterministic, because the
// loop collects on a tick rather than on Start and this test delivers none.
func TestSnapshot_StartedAndAwaitingFirstCollection(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)
	// No tick is ever delivered, so no collection can have happened.
	useManualTicker(m)
	startMonitor(t, m)

	snap, err := m.Snapshot()
	if snap != nil {
		t.Fatalf("Snapshot() = %v, want nothing before the first tick", snap)
	}
	var noneYet *types.NoSnapshotYetError
	if !errors.As(err, &noneYet) {
		t.Fatalf("Snapshot() error = %T (%v), want *types.NoSnapshotYetError", err, err)
	}
	// The distinction is the whole change: the old type must NOT also match,
	// or a caller telling the two apart still cannot.
	var notRunning *types.MonitorNotRunningError
	if errors.As(err, &notRunning) {
		t.Error("a running monitor still reports itself as not running")
	}
}

func TestSnapshot_AfterPoll(t *testing.T) {
	m := newMonitor(t, newStub("expected-host"), time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)
	sub := m.Subscribe()
	t.Cleanup(func() { m.Unsubscribe(sub.ID) })

	// The reading is stored before it is broadcast, so once a subscriber has
	// it Snapshot must answer with it.
	ticker.tick(t)
	await.Recv(t, "the first snapshot", sub.Ch)

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() after a collection: %v", err)
	}
	if snap.Host.Hostname != "expected-host" {
		t.Errorf("hostname = %q, want %q", snap.Host.Hostname, "expected-host")
	}
}

// ---- Subscribe / Unsubscribe --------------------------------------------

func TestSubscribe_ReceivesSnapshots(t *testing.T) {
	// A real ticker: this is the path production runs.
	m := newMonitor(t, newStub("streamed"), 20*time.Millisecond)
	startMonitor(t, m)

	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)

	if snap := await.Recv(t, "a snapshot", sub.Ch); snap == nil {
		t.Fatal("received nil snapshot")
	}
}

func TestSubscribe_UniqueIDs(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)

	a := m.Subscribe()
	b := m.Subscribe()
	defer m.Unsubscribe(a.ID)
	defer m.Unsubscribe(b.ID)

	if a.ID == b.ID {
		t.Errorf("expected unique IDs, both are %d", a.ID)
	}
}

func TestUnsubscribe_ClosesDone(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)

	sub := m.Subscribe()
	m.Unsubscribe(sub.ID)

	// Unsubscribe closes Done before it returns.
	select {
	case <-sub.Done:
	default:
		t.Fatal("Done channel not closed after Unsubscribe")
	}
}

func TestUnsubscribe_Idempotent(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)

	sub := m.Subscribe()
	m.Unsubscribe(sub.ID)

	// Second call must not panic.
	m.Unsubscribe(sub.ID)
}

func TestStop_ClosesSubscriberDone(t *testing.T) {
	m := newMonitor(t, newStub("host"), 20*time.Millisecond)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sub := m.Subscribe()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Stop closes its run's subscribers before it returns.
	select {
	case <-sub.Done:
	default:
		t.Fatal("Done channel not closed after Stop")
	}
}

// ---- Multiple subscribers -----------------------------------------------

func TestBroadcast_AllSubscribersReceive(t *testing.T) {
	m := newMonitor(t, newStub("broadcast"), 20*time.Millisecond)
	startMonitor(t, m)

	const count = 5
	subs := make([]*monitor.Subscriber, count)
	for i := range subs {
		subs[i] = m.Subscribe()
		t.Cleanup(func() { m.Unsubscribe(subs[i].ID) })
	}

	for i, sub := range subs {
		if snap := await.Recv(t, "a snapshot for every subscriber", sub.Ch); snap == nil {
			t.Errorf("subscriber %d received nil snapshot", i)
		}
	}
}

// ---- Slow consumer (drop) -----------------------------------------------

func TestBroadcast_SlowSubscriberDoesNotBlock(t *testing.T) {
	stub := newStub("slow")
	m := newMonitor(t, stub, time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)

	// Register a subscriber but never read from it.
	sub := m.Subscribe()
	t.Cleanup(func() { m.Unsubscribe(sub.ID) })

	// Twice the buffer and then some: a broadcast that blocked on the full
	// channel would stall the loop at the buffer size, and the next tick would
	// never be taken. Each tick is taken only once the previous collection's
	// broadcast has returned.
	const collections = 2*monitor.SubscriberBufSize + 1
	for range collections {
		ticker.tick(t)
	}
	stopMonitor(t, m)

	if got := stub.callCount(); got != collections {
		t.Errorf("snapshotter called %d times, want %d", got, collections)
	}
	// Dropped rather than queued: the buffer holds the most recent readings.
	if got := len(sub.Ch); got != monitor.SubscriberBufSize {
		t.Errorf("slow subscriber holds %d snapshots, want a full buffer of %d",
			got, monitor.SubscriberBufSize)
	}
}

// ---- SetInterval --------------------------------------------------------

func TestSetInterval_Valid(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Second)

	if err := m.SetInterval(5 * time.Second); err != nil {
		t.Fatalf("SetInterval: unexpected error: %v", err)
	}
}

func TestSetInterval_InvalidZero(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Second)

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
	m := newMonitor(t, newStub("host"), time.Second)

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
	// A real ticker, an hour long: nothing is collected unless the running
	// loop applies the new interval. Were it ignored, the wait below would
	// reach its failure bound, not an hour.
	m := newMonitor(t, newStub("host"), time.Hour)
	startMonitor(t, m)
	sub := m.Subscribe()
	t.Cleanup(func() { m.Unsubscribe(sub.ID) })

	if err := m.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}

	await.Recv(t, "a collection at the new interval", sub.Ch)
}

// TestSetInterval_ResetsTheRunningTicker pins the mechanism behind the test
// above exactly: the running loop resets its ticker to the new interval.
func TestSetInterval_ResetsTheRunningTicker(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)

	if err := m.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := await.Recv(t, "the ticker to be reset", ticker.resets); got != 20*time.Millisecond {
		t.Errorf("ticker reset to %v, want 20ms", got)
	}
}

// ---- Collection error tolerance -----------------------------------------

func TestStart_CollectionErrorDoesNotStop(t *testing.T) {
	stub := &stubSnapshotter{err: errors.New("disk unavailable")}
	m := newMonitor(t, stub, time.Hour)
	ticker := useManualTicker(m)
	startMonitor(t, m)

	// Every tick after the first is taken only if the loop survived the
	// failed collection before it.
	const collections = 3
	for range collections {
		ticker.tick(t)
	}
	if !m.Running() {
		t.Error("Running() is false after failed collections")
	}
	stopMonitor(t, m)

	if got := stub.callCount(); got != collections {
		t.Errorf("snapshotter called %d times, want %d: one per tick despite the errors",
			got, collections)
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
	m := newMonitor(t, newStub("host"), time.Hour)
	ticker := useManualTicker(m)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The second tick is taken only once the first collection is published.
	ticker.tick(t)
	ticker.tick(t)
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
	// is the other half of the trap. Stop has returned and no loop is running,
	// so nothing is left that could close it: checking now is conclusive.
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	select {
	case <-sub.Done:
		t.Error("a subscription taken after Stop was closed; Running() would then be unnecessary")
	default:
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
	m := newMonitor(t, blocking, time.Hour)
	ticker := useManualTicker(m)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The poll loop is now inside a collection and cannot exit.
	ticker.tick(t)
	await.Recv(t, "the poll loop to enter a collection", blocking.entered)

	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop() }()
	// Running turns false in the same step that marks the stop in progress,
	// so from here Stop is committed and waiting on the held collection.
	await.Until(t, "Stop to begin", func(context.Context) bool { return !m.Running() })

	// A Start now must not be able to slip in front of the sweep that is
	// still to come: it has to wait for the stop to finish.
	started := make(chan error, 1)
	go func() { started <- m.Start() }()
	await.Until(t, "Start to return or to wait for the stop", func(context.Context) bool {
		return len(started) > 0 || await.Parked("sync.(*Cond).Wait(", "monitor.(*Monitor).Start(") > 0
	})
	select {
	case err := <-started:
		t.Fatalf("Start returned (%v) while Stop was still closing subscribers; "+
			"a subscription taken now would be swept by the outgoing stop", err)
	default:
	}
	// The collection is still held, so Stop cannot have finished either.
	select {
	case <-stopped:
		t.Fatal("Stop finished while the poll loop was still held inside a collection")
	default:
	}

	// And the rest of the API is NOT blocked behind it. The wait above is for
	// a collection to finish, and a collection is a syscall into /proc, SMART
	// or NVML that can hang; a stop that held the lock through it would make
	// one wedged collector freeze every Running() in the process -- including
	// the delta hub's, which would stall the stream instead of ending it. The
	// answer it gives is also the true one: the monitor is not collecting.
	answered := make(chan bool, 1)
	go func() { answered <- m.Running() }()
	if running := await.Recv(t, "Running() to answer during a stop", answered); running {
		t.Error("Running() reports true while a stop is in progress")
	}

	close(blocking.release)

	if err := await.Recv(t, "Stop to return", stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := await.Recv(t, "the waiting Start to return", started); err != nil {
		t.Fatalf("Start after Stop completed: %v", err)
	}
	if !m.Running() {
		t.Fatal("the monitor is not running after the Start that followed the Stop")
	}

	// And the generation that is now running is intact: a subscription taken
	// against it was not closed by the stop that preceded it. That stop has
	// returned, so its sweep is over and checking now is conclusive.
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	select {
	case <-sub.Done:
		t.Fatal("a subscription against the running monitor was closed by the previous stop")
	default:
	}
	stopMonitor(t, m)
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
	startMonitor(t, m)

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
	m := newMonitor(t, newStub("host"), time.Hour)
	ticker := useManualTicker(m)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The second tick is taken only once the first collection is published.
	ticker.tick(t)
	ticker.tick(t)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Still readable while it is stopped, which is the deliberate half.
	if _, err := m.Snapshot(); err != nil {
		t.Fatalf("Snapshot() after Stop = %v; the final reading is meant to survive", err)
	}

	// The restarted loop is given no tick, so it cannot have collected before
	// the check below: this is the clearing rather than a race won.
	startMonitor(t, m)

	// NoSnapshotYet, not NotRunning: the loop is collecting, it simply has not
	// finished. Calling that "not running" is what #33 was about.
	var noneYet *types.NoSnapshotYetError
	if snap, err := m.Snapshot(); !errors.As(err, &noneYet) {
		t.Fatalf("Snapshot() right after a restart = %v, %v; want it to report that "+
			"this run has collected nothing, not the reading from before the stop",
			snap, err)
	}
}

// ---- The published snapshot carries no null lists (#151) -----------------

// TestPublish_FillsNilListsBeforeAnyoneSeesTheSnapshot: whatever a Snapshotter
// returns -- the collector, the serve wrapper that attaches alerts, or a
// library caller's own -- the snapshot a subscriber receives and the one
// Snapshot answers with encode every list as [], never null.
func TestPublish_FillsNilListsBeforeAnyoneSeesTheSnapshot(t *testing.T) {
	stub := &stubSnapshotter{snapshot: &types.Snapshot{
		NPUs: []types.NPUInfo{{Name: "accel0"}}, // no engines, no clients
	}}
	m := newMonitor(t, stub, 20*time.Millisecond)
	startMonitor(t, m)

	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)

	received := await.Recv(t, "a snapshot", sub.Ch)
	latest, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for name, snap := range map[string]*types.Snapshot{"subscriber": received, "Snapshot": latest} {
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		nulls, err := typestest.NullSlices(raw, snap)
		if err != nil {
			t.Fatalf("%s: NullSlices: %v", name, err)
		}
		if len(nulls) != 0 {
			t.Errorf("%s: lists encoded as null: %v", name, nulls)
		}
	}
}
