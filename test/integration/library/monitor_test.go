//go:build integration

package library

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// epoch dates the readings the test snapshotter hands out.
var epoch = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// countingSnapshotter is a collector the tests control: it fails its first
// failures collections, then answers. With dated set, reading n is dated n
// seconds after epoch; otherwise it leaves the timestamp zero, as a minimal
// Snapshotter does.
type countingSnapshotter struct {
	failures int64
	dated    bool
	calls    atomic.Int64
}

func (s *countingSnapshotter) Snapshot() (*types.Snapshot, error) {
	n := s.calls.Add(1)
	if n <= s.failures {
		return nil, errors.New("the collector could not read /proc")
	}
	snap := &types.Snapshot{}
	if s.dated {
		snap.Timestamp = epoch.Add(time.Duration(n) * time.Second)
	}
	return snap, nil
}

// tagDecorator is a Decorator that marks the hostname, comparable as a pointer
// is.
type tagDecorator struct{ host string }

func (d *tagDecorator) Identify(snap *types.Snapshot) { snap.Host.Hostname = d.host }
func (d *tagDecorator) Evaluate(*types.Snapshot)      {}

// startMonitor starts a monitor over s, stopped when the test ends if it is
// still running.
func startMonitor(t *testing.T, s monitor.Snapshotter, interval time.Duration) *monitor.Monitor {
	t.Helper()
	mon, err := monitor.New(s, interval)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if mon.Running() {
			if err := mon.Stop(); err != nil {
				t.Errorf("monitor.Stop: %v", err)
			}
		}
	})
	return mon
}

// TestMonitor_RefusesAnInvalidInterval covers both doors an interval comes
// through: construction and a change of a running monitor.
func TestMonitor_RefusesAnInvalidInterval(t *testing.T) {
	var invalid *types.InvalidIntervalError
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := monitor.New(&countingSnapshotter{}, d); !errors.As(err, &invalid) {
			t.Errorf("New(%v) = %v, want an InvalidIntervalError", d, err)
		}
	}

	mon := startMonitor(t, &countingSnapshotter{}, time.Second)
	if err := mon.SetInterval(0); !errors.As(err, &invalid) {
		t.Errorf("SetInterval(0) = %v, want an InvalidIntervalError", err)
	}
	if got := mon.Interval(); got != time.Second {
		t.Errorf("Interval() = %v after a refused change, want 1s", got)
	}
	if err := mon.SetInterval(5 * time.Second); err != nil {
		t.Errorf("SetInterval(5s) = %v, want nil", err)
	}
	if got := mon.Interval(); got != 5*time.Second {
		t.Errorf("Interval() = %v after a change, want 5s", got)
	}
}

// TestMonitor_LifecycleMisuseIsReported starts a running monitor and stops a
// stopped one: both are errors a caller can tell apart, and neither disturbs
// the monitor.
func TestMonitor_LifecycleMisuseIsReported(t *testing.T) {
	mon := startMonitor(t, &countingSnapshotter{}, 10*time.Millisecond)

	var already *types.MonitorAlreadyRunningError
	if err := mon.Start(); !errors.As(err, &already) {
		t.Errorf("a second Start = %v, want a MonitorAlreadyRunningError", err)
	}
	if !mon.Running() {
		t.Error("a refused Start stopped the monitor")
	}

	if err := mon.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var notRunning *types.MonitorNotRunningError
	if err := mon.Stop(); !errors.As(err, &notRunning) {
		t.Errorf("a second Stop = %v, want a MonitorNotRunningError", err)
	}
}

// TestMonitor_UnsubscribingAnUnknownConsumerChangesNothing is the double
// unsubscribe an SSE handler makes when a client disconnects as the monitor
// stops.
func TestMonitor_UnsubscribingAnUnknownConsumerChangesNothing(t *testing.T) {
	mon := startMonitor(t, &countingSnapshotter{}, 10*time.Millisecond)
	sub := mon.Subscribe()

	mon.Unsubscribe(sub.ID + 1000)
	select {
	case <-sub.Done:
		t.Fatal("unsubscribing an unknown ID closed another consumer")
	default:
	}

	mon.Unsubscribe(sub.ID)
	mon.Unsubscribe(sub.ID)
	await.Recv(t, "the unsubscribed consumer's Done to close", sub.Done)
}

// TestMonitor_OneDecoratorAtATime installs, repeats, conflicts, removes and
// replaces a decorator, and checks what a reader is served at each step.
func TestMonitor_OneDecoratorAtATime(t *testing.T) {
	mon := startMonitor(t, &countingSnapshotter{}, 10*time.Millisecond)
	first, second := &tagDecorator{host: "first"}, &tagDecorator{host: "second"}

	hostIs := func(want string) {
		t.Helper()
		await.Until(t, "readers to be served host "+want, func(context.Context) bool {
			snap, err := mon.Snapshot()
			return err == nil && snap.Host.Hostname == want
		})
	}

	if err := mon.SetDecorator(first); err != nil {
		t.Fatalf("SetDecorator(first): %v", err)
	}
	if err := mon.SetDecorator(first); err != nil {
		t.Errorf("installing the installed decorator again = %v, want nil", err)
	}
	var installed *types.DecoratorInstalledError
	if err := mon.SetDecorator(second); !errors.As(err, &installed) {
		t.Errorf("installing a second decorator = %v, want a DecoratorInstalledError", err)
	}
	if mon.Decorator() != monitor.Decorator(first) {
		t.Error("a refused SetDecorator replaced the installed decorator")
	}
	hostIs("first")

	if err := mon.SetDecorator(nil); err != nil {
		t.Fatalf("SetDecorator(nil): %v", err)
	}
	if mon.Decorator() != nil {
		t.Error("SetDecorator(nil) left a decorator installed")
	}
	if err := mon.SetDecorator(second); err != nil {
		t.Fatalf("SetDecorator(second) after removal: %v", err)
	}
	hostIs("second")
}

// TestMonitor_ACollectionErrorIsSkippedAndAnUndatedReadingIsDated has the
// collector fail on its first ticks: the loop survives, nothing is published
// for the failures, and the first good reading -- which the collector left
// undated -- is dated as collected.
func TestMonitor_ACollectionErrorIsSkippedAndAnUndatedReadingIsDated(t *testing.T) {
	s := &countingSnapshotter{failures: 3}
	before := time.Now()
	mon := startMonitor(t, s, 5*time.Millisecond)

	var snap *types.Snapshot
	await.Until(t, "a reading after the failed collections", func(context.Context) bool {
		var err error
		snap, err = mon.Snapshot()
		return err == nil
	})
	if calls := s.calls.Load(); calls <= s.failures {
		t.Errorf("a reading was published after %d collections, but the first %d failed", calls, s.failures)
	}
	if snap.Timestamp.Before(before) {
		t.Errorf("an undated reading was stamped %v, before the monitor started at %v", snap.Timestamp, before)
	}
}

// TestMonitor_ASlowSubscriberLosesTheOldestReadingNotTheNewest never reads its
// channel while the monitor publishes far more than the channel holds: the
// monitor keeps going and the consumer is left the newest readings.
func TestMonitor_ASlowSubscriberLosesTheOldestReadingNotTheNewest(t *testing.T) {
	s := &countingSnapshotter{dated: true}
	mon := startMonitor(t, s, 2*time.Millisecond)
	sub := mon.Subscribe()

	const published = 20
	await.Until(t, "more readings than the subscriber's channel holds", func(context.Context) bool {
		return s.calls.Load() >= published
	})
	if err := mon.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	last, err := mon.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after Stop: %v", err)
	}

	if len(sub.Ch) == 0 {
		t.Fatal("the slow subscriber was sent nothing")
	}
	var newest *types.Snapshot
	for len(sub.Ch) > 0 {
		newest = <-sub.Ch
	}
	if !newest.Timestamp.Equal(last.Timestamp) {
		t.Errorf("the slow subscriber's newest reading is dated %v, want the monitor's last, %v", newest.Timestamp, last.Timestamp)
	}
}
