package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- newMonitorSnapshotter ------------------------------------------------

// TestNewMonitorSnapshotter_ReturnsAdapter verifies the constructor wires the
// collector into something that satisfies monitor.Snapshotter.
func TestNewMonitorSnapshotter_ReturnsAdapter(t *testing.T) {
	sc := collector.NewSystemCollector(nil)
	got := newMonitorSnapshotter(sc)
	if got == nil {
		t.Fatal("newMonitorSnapshotter() = nil, want a Snapshotter")
	}
	adapter, ok := got.(*snapshotterAdapter)
	if !ok {
		t.Fatalf("newMonitorSnapshotter() = %T, want *snapshotterAdapter", got)
	}
	if adapter.sc != sc {
		t.Error("adapter does not hold the collector it was constructed with")
	}
}

// TestNewMonitorSnapshotter_NilCollector verifies the constructor does not
// panic on a nil collector; the failure surfaces later, on use.
func TestNewMonitorSnapshotter_NilCollector(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("newMonitorSnapshotter(nil) panicked: %v", r)
		}
	}()
	if got := newMonitorSnapshotter(nil); got == nil {
		t.Error("newMonitorSnapshotter(nil) = nil, want a non-nil Snapshotter")
	}
}

// ---- MonitorBinding lifecycle ---------------------------------------------

// bindStub is a Snapshotter for binding tests.
type bindStub struct{}

func (bindStub) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Host: types.HostInfo{Hostname: "binding-test"}}, nil
}

// TestMonitorBinding_StartupShutdown drives the Wails lifecycle hooks: startup
// must start the monitor and shutdown must stop it.
func TestMonitorBinding_StartupShutdown(t *testing.T) {
	mon, err := monitor.New(bindStub{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	b := NewMonitorBinding(mon)
	// startup also starts the frontend stream. This test is about the
	// monitor's lifecycle, and the desktop build's emitter is the Wails
	// runtime, which exits the process when handed a context the Wails
	// lifecycle did not create -- so frames go nowhere here, in every shape.
	b.emit = func(context.Context, string, []byte) {}

	b.startup(context.Background())

	// A started monitor serves a snapshot; a stopped one never would.
	waitSnapshot(t, mon)

	b.shutdown(context.Background())

	var notRunning *types.MonitorNotRunningError
	if stopErr := mon.Stop(); stopErr == nil || !errors.As(stopErr, &notRunning) {
		t.Errorf("shutdown() left the monitor running; Stop() = %v", stopErr)
	}
}

// TestMonitorBinding_ShutdownWithoutStartup verifies shutdown logs rather than
// panics when the monitor was never started.
func TestMonitorBinding_ShutdownWithoutStartup(t *testing.T) {
	mon, err := monitor.New(bindStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	b := NewMonitorBinding(mon)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("shutdown() panicked on a monitor that never started: %v", r)
		}
	}()
	b.shutdown(context.Background())
}

// ---- runRoot --------------------------------------------------------------

// TestRunRoot_NoDisplayFallsBackToOverview verifies that with no display
// server the root command renders the CLI overview instead of failing.
func TestRunRoot_NoDisplayFallsBackToOverview(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	resetGlobals(t)
	withFixtureSnapshot(t, fixtureSnapshot(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runRoot(cmd, nil); err != nil {
		t.Fatalf("runRoot() error = %v, want nil", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("fixture-host")) {
		t.Errorf("runRoot() did not render the overview; got:\n%s", buf.String())
	}
}

// ---- runWithRefresh loop --------------------------------------------------

// TestRunWithRefresh_LoopsUntilInterrupt drives the --refresh branch: it must
// render repeatedly and exit cleanly when interrupted.
func TestRunWithRefresh_LoopsUntilInterrupt(t *testing.T) {
	resetGlobals(t)
	refreshInterval = 0.02

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	renders := make(chan struct{}, 64)
	done := make(chan error, 1)
	go func() {
		done <- runWithRefresh(cmd, func(*cobra.Command, *types.Snapshot) error {
			select {
			case renders <- struct{}{}:
			default:
			}
			return nil
		})
	}()

	// Wait for at least two frames so the loop, not just the first pass, ran.
	for range 2 {
		await.Recv(t, "the refresh loop to render", renders)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	if err := await.Recv(t, "the refresh loop to exit after SIGINT", done); err != nil {
		t.Errorf("runWithRefresh() = %v, want nil after an interrupt", err)
	}
}

// TestMonitorBinding_StartupWhenAlreadyRunning covers the error branch in
// startup: a monitor that is already running must be reported and must not
// spawn a second streaming goroutine.
func TestMonitorBinding_StartupWhenAlreadyRunning(t *testing.T) {
	mon, err := monitor.New(bindStub{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		var notRunning *types.MonitorNotRunningError
		if stopErr := mon.Stop(); stopErr != nil && !errors.As(stopErr, &notRunning) {
			t.Errorf("monitor.Stop: %v", stopErr)
		}
	})

	b := NewMonitorBinding(mon)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("startup panicked on an already-running monitor: %v", r)
		}
	}()
	b.startup(context.Background())

	if b.running.Load() {
		t.Error("startup() started a streaming goroutine despite the start failure")
	}
}

// TestMonitorBinding_StreamToFrontendIsSingleFlight covers the guard that
// prevents a second streaming goroutine from running concurrently.
func TestMonitorBinding_StreamToFrontendIsSingleFlight(t *testing.T) {
	mon, err := monitor.New(bindStub{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}

	b := NewMonitorBinding(mon)
	b.ctx = context.Background()
	// A stream emits only after claiming the flag, so its first frame is the
	// proof that it is running.
	framed := make(chan struct{}, 1)
	b.emit = func(context.Context, string, []byte) {
		select {
		case framed <- struct{}{}:
		default:
		}
	}

	go b.streamToFrontend(context.Background())
	await.Recv(t, "the first stream to emit a frame", framed)
	if !b.running.Load() {
		t.Fatal("a stream is emitting without having marked itself running")
	}

	// A second call must return rather than double-subscribing. Nothing
	// would end a second stream -- its context is never cancelled and the
	// monitor is stopped only below -- so returning at all is the guard.
	returned := make(chan struct{})
	go func() {
		b.streamToFrontend(context.Background())
		close(returned)
	}()
	await.Recv(t, "a second streamToFrontend to return", returned)

	if err := mon.Stop(); err != nil {
		t.Errorf("monitor.Stop: %v", err)
	}
}

// TestMonitorBinding_SetIntervalRejectsNonPositive covers the validation guard.
func TestMonitorBinding_SetIntervalRejectsNonPositive(t *testing.T) {
	mon, err := monitor.New(bindStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	b := NewMonitorBinding(mon)

	for _, ms := range []int{0, -1} {
		err := b.SetInterval(ms)
		if err == nil {
			t.Fatalf("SetInterval(%d) = nil, want an error", ms)
		}
		var invalid *types.InvalidIntervalError
		if !errors.As(err, &invalid) {
			t.Errorf("SetInterval(%d) error = %T, want *types.InvalidIntervalError", ms, err)
		}
	}
}

// TestMonitorBinding_SetIntervalDoesNotStartCollection covers the success path
// on a monitor that is NOT running.
//
// It used to assert the opposite, because SetInterval stopped the monitor and
// built a replacement that it started -- so changing the polling rate could
// also begin collecting on a binding whose monitor was deliberately stopped.
// The cadence is now adjusted in place (#97): the interval changes, and
// starting collection stays the caller's decision. A control named "polling
// interval" that also starts the poller is two actions under one name.
func TestMonitorBinding_SetIntervalDoesNotStartCollection(t *testing.T) {
	mon, err := monitor.New(bindStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	b := NewMonitorBinding(mon)
	b.ctx = context.Background()

	if err := b.SetInterval(50); err != nil {
		t.Fatalf("SetInterval(50) = %v, want nil", err)
	}

	if got := b.mon; got != mon {
		t.Error("SetInterval replaced the monitor rather than retuning it")
	}
	if got := mon.Interval(); got != 50*time.Millisecond {
		t.Errorf("Interval() = %v, want 50ms", got)
	}
	if mon.Running() {
		t.Error("SetInterval started a monitor that was not running")
	}

	// And nothing collected: a stopped monitor has no snapshot, before or
	// after a rate change.
	var notRunning *types.MonitorNotRunningError
	if _, snapErr := mon.Snapshot(); !errors.As(snapErr, &notRunning) {
		t.Errorf("Snapshot() error = %v, want MonitorNotRunningError", snapErr)
	}
}
