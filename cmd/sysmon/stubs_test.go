//go:build !desktop

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

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- non-desktop stubs ----------------------------------------------------

// TestLaunchGUI_ReportsUnavailable verifies the non-desktop build reports the
// typed error that runRoot uses to fall through to the CLI.
func TestLaunchGUI_ReportsUnavailable(t *testing.T) {
	err := launchGUI()
	if err == nil {
		t.Fatal("launchGUI() = nil, want a GUIUnavailableError in a non-desktop build")
	}
	var guiErr *types.GUIUnavailableError
	if !errors.As(err, &guiErr) {
		t.Errorf("launchGUI() error = %T (%v), want *types.GUIUnavailableError", err, err)
	}
}

// TestEmitSnapshot_NoOp verifies the stub emitter accepts any input without
// panicking, including a nil snapshot.
func TestEmitSnapshot_NoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitSnapshot panicked: %v", r)
		}
	}()
	emitSnapshot(context.Background(), &types.Snapshot{})
	emitSnapshot(context.Background(), nil)
	emitSnapshot(nil, nil) //nolint:staticcheck // a nil context must not panic
}

// TestSystrayStubs_NoOp verifies the tray stubs are safe no-ops, including
// with nil callbacks.
func TestSystrayStubs_NoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("systray stub panicked: %v", r)
		}
	}()
	startSystray(nil, nil, nil)
	stopSystray()
}

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
	b := NewMonitorBinding(mon, collector.NewSystemCollector(nil))

	b.startup(context.Background())

	// A started monitor eventually serves a snapshot.
	deadline := time.Now().Add(3 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if _, snapErr := mon.Snapshot(); snapErr == nil {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("startup() did not start the monitor")
	}

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
	b := NewMonitorBinding(mon, collector.NewSystemCollector(nil))

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

// TestRunRoot_WithDisplayFallsBackWhenGUIUnavailable verifies that in a
// non-desktop build a detected display still falls through to the overview,
// because launchGUI reports GUIUnavailableError.
func TestRunRoot_WithDisplayFallsBackWhenGUIUnavailable(t *testing.T) {
	t.Setenv("DISPLAY", ":0")
	resetGlobals(t)
	withFixtureSnapshot(t, fixtureSnapshot(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runRoot(cmd, nil); err != nil {
		t.Fatalf("runRoot() error = %v, want nil", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("fixture-host")) {
		t.Errorf("runRoot() did not fall back to the overview; got:\n%s", buf.String())
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
	for i := 0; i < 2; i++ {
		select {
		case <-renders:
		case <-time.After(10 * time.Second):
			t.Fatal("refresh loop did not render repeatedly")
		}
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runWithRefresh() = %v, want nil after an interrupt", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("refresh loop did not exit after SIGINT")
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

	b := NewMonitorBinding(mon, collector.NewSystemCollector(nil))

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

	b := NewMonitorBinding(mon, collector.NewSystemCollector(nil))
	b.ctx = context.Background()

	go b.streamToFrontend()

	// Wait for the first goroutine to claim the flag.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !b.running.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !b.running.Load() {
		t.Fatal("streamToFrontend never marked itself running")
	}

	// A second call must return immediately rather than double-subscribing.
	returned := make(chan struct{})
	go func() {
		b.streamToFrontend()
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Error("a second streamToFrontend did not return immediately")
	}

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
	b := NewMonitorBinding(mon, collector.NewSystemCollector(nil))

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

// TestMonitorBinding_SetIntervalRestartsMonitor covers the success path,
// including the stop-then-recreate sequence on a monitor that is not running.
func TestMonitorBinding_SetIntervalRestartsMonitor(t *testing.T) {
	mon, err := monitor.New(bindStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	b := NewMonitorBinding(mon, collector.NewSystemCollector(nil))
	b.ctx = context.Background()

	if err := b.SetInterval(50); err != nil {
		t.Fatalf("SetInterval(50) = %v, want nil", err)
	}
	t.Cleanup(func() {
		var notRunning *types.MonitorNotRunningError
		if stopErr := b.mon.Stop(); stopErr != nil && !errors.As(stopErr, &notRunning) {
			t.Errorf("monitor.Stop: %v", stopErr)
		}
	})

	// The replacement monitor must be running and serving snapshots.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, snapErr := b.mon.Snapshot(); snapErr == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("SetInterval did not leave a running monitor behind")
}
