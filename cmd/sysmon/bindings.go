// bindings.go wires the system monitor to the Wails frontend. It is compiled
// for all build targets so that the type is always available; the desktop
// build uses it directly while the stub build never instantiates it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// defaultInterval is the polling interval used when the GUI is launched
// without an explicit --interval flag.
//
// Used only by the desktop build (see gui_desktop.go); the unused linter
// does not compile that file, hence the directive.
const defaultInterval = time.Second //nolint:unused

// MonitorBinding exposes the system monitor to the Wails frontend via
// exported method bindings and server-sent events.
type MonitorBinding struct {
	// mu guards mon and ctx. SetInterval swaps the monitor while a streaming
	// goroutine is still reading it, so neither field can be a plain read.
	mu  sync.RWMutex
	mon *monitor.Monitor
	ctx context.Context

	sc      *collector.SystemCollector
	running atomic.Bool
}

// monitor returns the current monitor.
func (b *MonitorBinding) monitor() *monitor.Monitor {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mon
}

// emitContext returns the Wails context to emit events on, which is nil until
// the runtime has started up.
func (b *MonitorBinding) emitContext() context.Context {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ctx
}

// NewMonitorBinding constructs a MonitorBinding backed by the given monitor
// and system collector. The collector is retained so that SetInterval can
// create a replacement monitor when the interval changes.
func NewMonitorBinding(mon *monitor.Monitor, sc *collector.SystemCollector) *MonitorBinding {
	return &MonitorBinding{
		mon: mon,
		sc:  sc,
	}
}

// startup is called by the Wails runtime after the application window is
// ready. It stores the Wails context (required for EventsEmit) and starts
// the monitor and the streaming goroutine.
//
// Called only from the desktop build, which the unused linter does not compile.
func (b *MonitorBinding) startup(ctx context.Context) { //nolint:unused
	b.mu.Lock()
	b.ctx = ctx
	b.mu.Unlock()

	if err := b.monitor().Start(); err != nil {
		slog.Error("monitor start", "error", err)
		return
	}
	go b.streamToFrontend()
}

// shutdown is called by the Wails runtime when the application is closing.
// It stops the monitor, which also terminates the streaming goroutine by
// closing the subscriber's Done channel.
//
// Called only from the desktop build, which the unused linter does not compile.
func (b *MonitorBinding) shutdown(_ context.Context) { //nolint:unused
	if err := b.monitor().Stop(); err != nil {
		slog.Warn("monitor stop", "error", err)
	}
}

// GetSnapshot returns the most recent system snapshot. This method is
// exported and callable from the Wails frontend as a one-shot request.
func (b *MonitorBinding) GetSnapshot() (*types.Snapshot, error) {
	return b.monitor().Snapshot()
}

// GetVersion returns the build metadata embedded at compile time.
func (b *MonitorBinding) GetVersion() map[string]string {
	return map[string]string{
		"version":   version,
		"gitCommit": gitCommit,
		"buildDate": buildDate,
	}
}

// SetInterval updates the polling interval. ms must be positive. The running
// monitor is stopped, a new one is created with the updated interval, and
// streaming resumes automatically.
func (b *MonitorBinding) SetInterval(ms int) error {
	if ms <= 0 {
		return &types.InvalidIntervalError{Message: "interval must be greater than zero"}
	}

	interval := time.Duration(ms) * time.Millisecond

	// Stop the current monitor. A "not running" error is expected when it was
	// already stopped and is not a failure; anything else is worth recording.
	if err := b.monitor().Stop(); err != nil {
		var notRunning *types.MonitorNotRunningError
		if !errors.As(err, &notRunning) {
			slog.Warn("monitor stop during interval change", "err", err)
		}
	}

	snap := &snapshotterAdapter{sc: b.sc}
	newMon, err := monitor.New(snap, interval)
	if err != nil {
		return err
	}

	b.mu.Lock()
	b.mon = newMon
	b.mu.Unlock()

	if err := newMon.Start(); err != nil {
		return err
	}
	go b.streamToFrontend()
	return nil
}

// streamToFrontend subscribes to the monitor's snapshot channel and emits
// each snapshot as a "sysmon:snapshot" event to the Wails frontend. It exits
// when the subscriber's Done channel is closed (i.e. the monitor stops or
// Unsubscribe is called).
func (b *MonitorBinding) streamToFrontend() {
	if !b.running.CompareAndSwap(false, true) {
		return
	}
	defer b.running.Store(false)

	mon := b.monitor()
	sub := mon.Subscribe()
	defer mon.Unsubscribe(sub.ID)

	for {
		select {
		case <-sub.Done:
			return
		case snap, ok := <-sub.Ch:
			if !ok {
				return
			}
			emitSnapshot(b.emitContext(), snap)
		}
	}
}
