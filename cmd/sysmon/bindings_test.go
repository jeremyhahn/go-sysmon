package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- helpers --------------------------------------------------------------

// newTestMonitor creates and starts a monitor backed by a static snapshot.
func newTestMonitor(t *testing.T, hostname string) *monitor.Monitor {
	t.Helper()
	stub := &monitorStub{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: hostname}},
	}
	m, err := monitor.New(stub, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("monitor.Stop: %v", err)
		}
	})
	return m
}

// monitorStub is a minimal monitor.Snapshotter.
type monitorStub struct {
	snapshot *types.Snapshot
	err      error
	// polled, when set, is signalled on every collection. It holds one
	// signal and the send never blocks, so the monitor is never held up by a
	// test that has stopped listening; each signal received was sent by a
	// collection after the previous one was received.
	polled chan struct{}
}

func (s *monitorStub) Snapshot() (*types.Snapshot, error) {
	if s.polled != nil {
		select {
		case s.polled <- struct{}{}:
		default:
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.snapshot, nil
}

// waitSnapshot returns once the monitor has published a snapshot.
//
// It subscribes before checking, so a snapshot published after the check is
// delivered to it and one published before is answered by Snapshot: the
// monitor stores a reading before broadcasting it.
func waitSnapshot(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	if _, err := m.Snapshot(); err == nil {
		return
	}
	await.Recv(t, "the monitor's first snapshot", sub.Ch)
}

// ---- NewMonitorBinding ----------------------------------------------------

func TestNewMonitorBinding_NotNil(t *testing.T) {
	m := newTestMonitor(t, "binding-host")
	b := NewMonitorBinding(m)
	if b == nil {
		t.Fatal("NewMonitorBinding returned nil")
	}
}

func TestNewMonitorBinding_StoresMonitor(t *testing.T) {
	m := newTestMonitor(t, "store-host")
	b := NewMonitorBinding(m)
	// Verify the binding is usable – GetSnapshot delegates to the monitor.
	waitSnapshot(t, m)
	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot after NewMonitorBinding: %v", err)
	}
	if snap.Host.Hostname != "store-host" {
		t.Errorf("Hostname = %q, want %q", snap.Host.Hostname, "store-host")
	}
}

// TestNewMonitorBinding_NeedsNoCollector drives the binding's whole exported
// surface against a stub snapshotter, which is what #34 asked for.
//
// The constructor used to demand a *collector.SystemCollector for a field
// nothing read, so every test of this type instantiated the real machine to
// satisfy it. The seam is the monitor's own Snapshotter, reached only through
// the monitor: there is no second collector to inject because the binding no
// longer has one.
func TestNewMonitorBinding_NeedsNoCollector(t *testing.T) {
	m := newTestMonitor(t, "no-collector-host")
	b := NewMonitorBinding(m)
	waitSnapshot(t, m)

	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Host.Hostname != "no-collector-host" {
		t.Errorf("Hostname = %q, want the stub's", snap.Host.Hostname)
	}
	if err := b.SetInterval(500); err != nil {
		t.Fatalf("SetInterval(500): %v", err)
	}
	if got := b.monitor(); got != m {
		t.Error("SetInterval replaced the monitor; it must retune the running one")
	}
	if got := m.Interval(); got != 500*time.Millisecond {
		t.Errorf("Interval() = %v, want 500ms", got)
	}
	if _, ok := b.GetVersion()["version"]; !ok {
		t.Error("GetVersion() has no version key")
	}
}

// TestNewMonitorBinding_SurfacesACollectionFailure is the error counterpart: a
// monitor whose snapshotter only ever fails collects nothing, and the binding
// reports that rather than inventing a reading. It also pins the direction of
// the dependency -- the binding has no collector of its own to ask, so the
// only way it can fail is the monitor's.
func TestNewMonitorBinding_SurfacesACollectionFailure(t *testing.T) {
	stub := &monitorStub{err: errors.New("collector unavailable"), polled: make(chan struct{}, 1)}
	m, err := monitor.New(stub, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("monitor.Stop: %v", err)
		}
	})
	b := NewMonitorBinding(m)

	// Several failed collections, so this is the failure rather than a
	// warm-up.
	for range 3 {
		await.Recv(t, "a collection attempt", stub.polled)
	}

	snap, err := b.GetSnapshot()
	if err == nil {
		t.Fatalf("GetSnapshot() = %v, nil; want an error when nothing collected", snap)
	}
	if snap != nil {
		t.Errorf("GetSnapshot() returned %v alongside an error", snap)
	}
}

// ---- GetVersion -----------------------------------------------------------

func TestGetVersion_ContainsExpectedKeys(t *testing.T) {
	m := newTestMonitor(t, "ver-host")
	b := NewMonitorBinding(m)
	v := b.GetVersion()

	for _, key := range []string{"version", "gitCommit", "buildDate"} {
		if _, ok := v[key]; !ok {
			t.Errorf("GetVersion() missing key %q", key)
		}
	}
}

func TestGetVersion_ValuesAreStrings(t *testing.T) {
	m := newTestMonitor(t, "ver-host2")
	b := NewMonitorBinding(m)
	v := b.GetVersion()

	// Values must be non-empty strings (the package-level vars are "dev" /
	// "unknown" in test builds).
	for key, val := range v {
		if val == "" {
			t.Errorf("GetVersion()[%q] is empty", key)
		}
	}
}

// ---- GetSnapshot ----------------------------------------------------------

func TestGetSnapshot_ReturnsData(t *testing.T) {
	m := newTestMonitor(t, "snap-host")
	waitSnapshot(t, m)
	b := NewMonitorBinding(m)

	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot() error = %v, want nil", err)
	}
	if snap == nil {
		t.Fatal("GetSnapshot() returned nil snapshot")
	}
	if snap.Host.Hostname != "snap-host" {
		t.Errorf("Hostname = %q, want %q", snap.Host.Hostname, "snap-host")
	}
}

func TestGetSnapshot_MonitorNotStarted(t *testing.T) {
	stub := &monitorStub{
		snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "h"}},
	}
	m, err := monitor.New(stub, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	// Do NOT start the monitor – no snapshot is available yet.
	b := NewMonitorBinding(m)

	_, err = b.GetSnapshot()
	if err == nil {
		t.Fatal("GetSnapshot() error = nil, want MonitorNotRunningError")
	}
	var notRunning *types.MonitorNotRunningError
	if !errors.As(err, &notRunning) {
		t.Errorf("error type = %T, want *types.MonitorNotRunningError", err)
	}
}

// ---- SetInterval ----------------------------------------------------------

func TestSetInterval_ValidValue(t *testing.T) {
	m := newTestMonitor(t, "interval-host")
	b := NewMonitorBinding(m)
	waitSnapshot(t, m)

	if err := b.SetInterval(500); err != nil {
		t.Errorf("SetInterval(500) error = %v, want nil", err)
	}

	// The reading that existed before the change is still there immediately
	// after it. This used to need a thirty-second wait, because SetInterval
	// REBUILT the monitor on the real SystemCollector and the test had to sit
	// through a genuine first collection -- SMBIOS, every GPU, every disk, the
	// whole process table (issue #34's flake). Adjusting the cadence in place
	// removes the wait because it removes the restart: there is nothing to
	// collect again (#97).
	snap, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot straight after SetInterval: %v", err)
	}
	if snap == nil {
		t.Fatal("GetSnapshot returned nil after SetInterval")
	}
}

func TestSetInterval_ZeroIsInvalid(t *testing.T) {
	m := newTestMonitor(t, "interval-zero-host")
	b := NewMonitorBinding(m)

	err := b.SetInterval(0)
	if err == nil {
		t.Fatal("SetInterval(0) error = nil, want InvalidIntervalError")
	}
	var invalid *types.InvalidIntervalError
	if !errors.As(err, &invalid) {
		t.Errorf("error type = %T, want *types.InvalidIntervalError", err)
	}
}

func TestSetInterval_NegativeIsInvalid(t *testing.T) {
	m := newTestMonitor(t, "interval-neg-host")
	b := NewMonitorBinding(m)

	err := b.SetInterval(-100)
	if err == nil {
		t.Fatal("SetInterval(-100) error = nil, want InvalidIntervalError")
	}
	var invalid *types.InvalidIntervalError
	if !errors.As(err, &invalid) {
		t.Errorf("error type = %T, want *types.InvalidIntervalError", err)
	}
}

// TestSetInterval_AdjustsInPlace pins the behaviour the server has always had
// and the desktop did not: the same control changes the cadence and keeps
// everything else.
//
// It used to stop the monitor, construct a new one and start it, so a user
// changing the polling rate also silently discarded every reading the monitor
// had accumulated -- a second, unnamed action riding on a control called
// "polling interval" (#97).
func TestSetInterval_AdjustsInPlace(t *testing.T) {
	m := newTestMonitor(t, "in-place-host")
	waitSnapshot(t, m)
	b := NewMonitorBinding(m)

	before, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot before SetInterval: %v", err)
	}

	if err := b.SetInterval(100); err != nil {
		t.Fatalf("SetInterval(100): %v", err)
	}

	if got := b.monitor(); got != m {
		t.Error("SetInterval replaced the monitor; it must adjust the running one")
	}
	if got := m.Interval(); got != 100*time.Millisecond {
		t.Errorf("Interval() = %v after SetInterval(100), want 100ms", got)
	}

	// No gap: the reading collected before the change is still being served
	// the instant after it, rather than the monitor starting from nothing.
	after, err := b.GetSnapshot()
	if err != nil {
		t.Fatalf("GetSnapshot straight after SetInterval: %v", err)
	}
	if after == nil {
		t.Fatal("the monitor had no snapshot immediately after an interval change")
	}
	if before != nil && after.Host.Hostname != before.Host.Hostname {
		t.Errorf("the served snapshot changed identity across an interval change: %q -> %q",
			before.Host.Hostname, after.Host.Hostname)
	}
}

// TestStreamToFrontend_SendsDeltasNotSnapshots is #98's contract on the desktop
// side: the window gets one whole snapshot and patches after it.
//
// It emitted the entire snapshot on every tick, which on a ten-node fleet host
// measured 367,126 bytes per frame against a delta averaging 7,842 -- 47 times
// the bytes for identical information, on a machine that already had a correct
// delta hub it never called.
func TestStreamToFrontend_SendsDeltasNotSnapshots(t *testing.T) {
	var mu sync.Mutex
	var names []string
	var sizes []int

	m := newTestMonitor(t, "delta-host")
	waitSnapshot(t, m)
	b := NewMonitorBinding(m)
	b.ctx = context.Background()
	// Observed per binding, so a stream started by another test cannot be
	// mistaken for this one's -- which it was, until this test failed in the
	// package run and passed alone.
	framed := make(chan struct{}, 1)
	b.emit = func(_ context.Context, name string, payload []byte) {
		mu.Lock()
		names = append(names, name)
		sizes = append(sizes, len(payload))
		mu.Unlock()
		select {
		case framed <- struct{}{}:
		default:
		}
	}

	go b.streamToFrontend(context.Background())
	t.Cleanup(func() {
		b.mu.RLock()
		cancel := b.streamCancel
		b.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
	})

	for {
		mu.Lock()
		enough := len(names) >= 3
		mu.Unlock()
		if enough {
			break
		}
		await.Recv(t, "another frame", framed)
	}

	mu.Lock()
	defer mu.Unlock()
	if names[0] != "full" {
		t.Errorf("first frame is %q, want full -- there is nothing to patch yet", names[0])
	}
	for i, name := range names[1:] {
		if name != "full" {
			continue
		}
		t.Errorf("frame %d is a full snapshot; the whole measurement is being resent every tick", i+1)
	}
	for i, size := range sizes[1:] {
		if size >= sizes[0] {
			t.Errorf("frame %d is %d bytes against a first frame of %d; it is not a patch", i+1, size, sizes[0])
		}
	}
}

// ---- Resync -----------------------------------------------------------------

// endStream cancels b's current stream and waits for it to exit.
func endStream(t *testing.T, b *MonitorBinding) {
	t.Helper()
	b.mu.RLock()
	cancel, done := b.streamCancel, b.streamDone
	b.mu.RUnlock()
	if cancel == nil {
		t.Fatal("the binding has no stream to end")
	}
	cancel()
	await.Recv(t, "the stream to exit", done)
}

// TestResync_WaitsForTheOldStreamToExit: a resync must not start the new
// stream until the old one has let go of the flag, because a stream started
// while the flag is held returns at once and leaves the window with none.
//
// It used to poll the flag for up to 100ms and then start the new stream
// regardless, so an old stream slower than that to exit -- one inside an emit
// on a busy host -- cost the window its stream. Here the old stream is held
// inside an emit until the test releases it.
//
// Resync itself returns at once, since the old stream may be inside an emit
// that cancellation cannot interrupt. It runs in a synctest bubble so "nothing
// streams while the old stream is held" is exact rather than a guess:
// synctest.Wait returns only once every goroutine in the bubble is blocked, so
// anything that could emit has had its chance. Were the new stream started
// without waiting, it would find the flag held and return, and no full frame
// would follow the release.
func TestResync_WaitsForTheOldStreamToExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, err := monitor.New(&monitorStub{snapshot: &types.Snapshot{Host: types.HostInfo{Hostname: "resync-host"}}}, time.Second)
		if err != nil {
			t.Fatalf("monitor.New: %v", err)
		}
		if err := m.Start(); err != nil {
			t.Fatalf("monitor.Start: %v", err)
		}
		defer func() {
			if err := m.Stop(); err != nil {
				t.Errorf("monitor.Stop: %v", err)
			}
		}()

		b := NewMonitorBinding(m)
		held := make(chan struct{})    // the old stream is inside its first emit
		release := make(chan struct{}) // closed to let that emit return
		frames := make(chan string, 64)
		var calls atomic.Int64
		b.emit = func(_ context.Context, name string, _ []byte) {
			if calls.Add(1) == 1 {
				close(held)
				<-release
				return
			}
			select {
			case frames <- name:
			default:
			}
		}

		go b.streamToFrontend(context.Background())
		await.Recv(t, "the old stream to emit its first frame", held)

		// Resync returns at once: the frontend's call must not hang behind an
		// emit that cancellation cannot interrupt.
		resynced := make(chan struct{})
		go func() {
			b.Resync()
			close(resynced)
		}()
		await.Recv(t, "Resync to return while the old stream is held", resynced)

		// Every goroutine in the bubble is now blocked: the old stream in its
		// held emit, and the handoff waiting for it to exit. Nothing can emit
		// until the test releases it, so this check is conclusive.
		synctest.Wait()
		select {
		case name := <-frames:
			t.Fatalf("a %q frame was emitted while the old stream was held; nothing else may stream yet", name)
		default:
		}

		close(release)
		// The new stream begins with a whole snapshot. The old one may have
		// emitted a patch on its way out; it cannot emit a full frame.
		for name := await.Recv(t, "a frame from the new stream", frames); name != "full"; {
			name = await.Recv(t, "a full frame from the new stream", frames)
		}
		endStream(t, b)
	})
}

// TestResync_StartsAStreamWhenNoneIsRunning covers a resync before any stream
// has started: there is nothing to wait for, and it must still leave the window
// streaming.
func TestResync_StartsAStreamWhenNoneIsRunning(t *testing.T) {
	m := newTestMonitor(t, "resync-idle-host")
	b := NewMonitorBinding(m)
	frames := make(chan string, 64)
	b.emit = func(_ context.Context, name string, _ []byte) {
		select {
		case frames <- name:
		default:
		}
	}

	b.Resync()

	if name := await.Recv(t, "the stream Resync started to emit a frame", frames); name != "full" {
		t.Errorf("the first frame is %q, want full", name)
	}
	endStream(t, b)
}
