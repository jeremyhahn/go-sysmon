// bindings.go wires the system monitor to the Wails frontend. It is compiled
// for all build targets so that the type is always available; the desktop
// build uses it directly while the stub build never instantiates it.
package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
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

	// streamCancel ends the current frame stream. Held so Resync can restart
	// it: a restarted stream begins with a whole snapshot, which is the only
	// way back for a frontend that lost its place.
	streamCancel context.CancelFunc
	// streamDone is closed when the stream streamCancel belongs to has exited
	// and released running, so Resync can wait for it instead of guessing how
	// long it takes. Guarded by mu, and set together with streamCancel.
	streamDone chan struct{}

	running atomic.Bool

	// emit is where frames are written. Per binding rather than a package
	// variable so a test can observe one binding's stream without seeing every
	// other binding's: the build-tagged emitFrame talks to the Wails runtime,
	// which is absent in every build a test can run (issue #34 one layer down).
	emit func(ctx context.Context, name string, payload []byte)
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

// NewMonitorBinding constructs a MonitorBinding backed by the given monitor.
//
// It used to take the *collector.SystemCollector as well, because SetInterval
// stopped the monitor and rebuilt it on that collector -- which is what made
// a test of SetInterval wait on a genuine full-machine collection and pass or
// fail on how busy the host was (#34). SetInterval retunes the running
// monitor instead (#97), so the rebuild is gone and nothing here has read the
// collector since.
//
// The parameter outlived its only reader. Keeping it meant a caller still had
// to hand over the real machine to fill a field nobody looked at, and left
// the rebuild looking like something this type was still built for.
func NewMonitorBinding(mon *monitor.Monitor) *MonitorBinding {
	return &MonitorBinding{
		mon: mon,
		// A binding always has a context, so nothing downstream has to invent
		// one: startup replaces this with the application's, and until then
		// there is no lifetime to inherit. Without it the stream would need a
		// context.Background() of its own inside a goroutine that DOES have a
		// lifetime available once startup has run -- which is both the gosec
		// G118 finding and the real hazard behind it.
		ctx:  context.Background(),
		emit: emitFrame,
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
	// The app's context is the stream's parent, so the stream ends when the
	// window does rather than outliving it on a background context of its own
	// (gosec G118). Wails cancels this when the application closes, and
	// shutdown below cancels it explicitly as well -- a goroutine whose only
	// stop condition is a runtime being polite is not stopped.
	go b.streamToFrontend(ctx)
}

// shutdown is called by the Wails runtime when the application is closing.
// It stops the monitor, which also terminates the streaming goroutine by
// closing the subscriber's Done channel.
//
// Called only from the desktop build, which the unused linter does not compile.
func (b *MonitorBinding) shutdown(_ context.Context) { //nolint:unused
	// Stop the stream before the monitor, and explicitly: the subscriber's Done
	// channel would end it too, but only after the monitor stops, and a stream
	// whose only exit is another component's teardown order is one refactor
	// away from outliving the window.
	b.mu.RLock()
	cancel := b.streamCancel
	b.mu.RUnlock()
	if cancel != nil {
		cancel()
	}

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

// SetInterval updates the polling interval on the running monitor. ms must be
// positive.
//
// Adjusted in place rather than by rebuilding. This used to stop the monitor,
// construct a new one and start it, which made the same control mean two
// different things depending on which build the user was running: the server
// changes the cadence and keeps the readings, the desktop discarded everything
// the monitor had accumulated and restarted collection. A control named
// "polling interval" that also clears the charts is a second, unnamed action
// (#97).
func (b *MonitorBinding) SetInterval(ms int) error {
	if ms <= 0 {
		return &types.InvalidIntervalError{Message: "interval must be greater than zero"}
	}
	return b.monitor().SetInterval(time.Duration(ms) * time.Millisecond)
}

// streamToFrontend delivers the delta protocol to the Wails frontend: one
// whole snapshot, then patches against it, each carrying a sequence number.
//
// It used to emit the entire snapshot on every tick. Measured on a ten-node
// fleet host, that is 367,126 bytes per frame against a delta averaging 7,842
// -- 47 times the bytes for identical information, on a machine that already
// had a correct delta hub it never called (#98). The browser has been getting
// patches since #29; the desktop window is the same Svelte application and was
// getting whole snapshots.
//
// The stream is the same code the socket uses, not a second implementation:
// pkg/server owns the rule that a patch is only ever sent to a client holding
// the frame before it, and two copies of that rule would be two places for it
// to be wrong.
func (b *MonitorBinding) streamToFrontend(parent context.Context) {
	if !b.running.CompareAndSwap(false, true) {
		return
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	b.mu.Lock()
	b.streamCancel, b.streamDone = cancel, done
	b.mu.Unlock()
	// running is released before done is closed, so whoever waited on done
	// finds the flag free and can start the next stream.
	defer func() {
		b.running.Store(false)
		close(done)
	}()
	defer cancel()

	stream := server.NewDeltaStream(b.monitor(), slog.Default())
	defer stream.Shutdown()

	stream.Run(ctx, func(name string, payload []byte) {
		b.emit(b.emitContext(), name, payload)
	})
}

// Resync restarts the stream, so the next frame the frontend receives is a
// whole snapshot rather than a patch.
//
// The frontend calls this when it cannot apply what it was sent -- a gap in the
// sequence, a patch that does not fit, a frame that is not a frame. The browser
// recovers from that by reopening the socket, which begins with a full frame;
// the desktop has no socket to reopen, and without this a window that lost its
// place would apply patches to a snapshot the server never sent it, drifting
// silently rather than failing. A patch is only meaningful against the frame it
// was computed from.
func (b *MonitorBinding) Resync() {
	b.mu.RLock()
	cancel, done := b.streamCancel, b.streamDone
	b.mu.RUnlock()
	ctx := b.emitContext()
	// The new stream starts only once the previous one has exited: started
	// while the old still held the flag, it would find it taken and return,
	// leaving the window with no stream at all. The wait happens off the
	// caller's goroutine, because the old stream may be inside an emit that
	// cancellation cannot interrupt, and the frontend's call must not hang
	// behind it.
	go func() {
		if cancel != nil {
			cancel()
			<-done
		}
		b.streamToFrontend(ctx)
	}()
}
