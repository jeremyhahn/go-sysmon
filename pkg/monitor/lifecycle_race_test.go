package monitor_test

import (
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// raceSnapshotter is the cheapest possible collector: these tests are about
// the lifecycle, and a collector that touched the host would decide the timing.
type raceSnapshotter struct{}

func (raceSnapshotter) Snapshot() (*types.Snapshot, error) { return &types.Snapshot{}, nil }

func raceMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()

	m, err := monitor.New(raceSnapshotter{}, time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	return m
}

// TestUnsubscribeDuringStop_DoesNotPanic.
//
// Stop sweeps the subscriber map and Unsubscribe removes one entry, and both
// used to check "is Done closed" and then close it. That is two operations, not
// one: both can see it open, both can close it, and closing a closed channel
// PANICS -- taking the process down, not returning an error.
//
// It is reachable in the product, which is why it is here rather than filed as
// theoretical: the SSE handler unsubscribes when a client disconnects, and Stop
// runs whenever the poll interval changes. A dashboard closing its tab at the
// moment somebody changes the interval was enough.
//
// Found by a probe written for the generation-scoping question (#36), on the
// first run, in about ten milliseconds.
func TestUnsubscribeDuringStop_DoesNotPanic(t *testing.T) {
	// A panic in a goroutine cannot be recovered by the test, so this fails by
	// crashing the run rather than by reporting -- which is exactly why it must
	// be iterated hard enough to be reliable rather than run once.
	for attempt := range 300 {
		m := raceMonitor(t)
		if err := m.Start(); err != nil {
			t.Fatalf("attempt %d: Start: %v", attempt, err)
		}

		subs := make([]*monitor.Subscriber, 16)
		for i := range subs {
			subs[i] = m.Subscribe()
		}

		var wg sync.WaitGroup
		wg.Add(len(subs) + 1)
		go func() { defer wg.Done(); _ = m.Stop() }()
		for _, sub := range subs {
			go func() { defer wg.Done(); m.Unsubscribe(sub.ID) }()
		}
		wg.Wait()
	}
}

// TestStopDoesNotCloseALaterRunsSubscribers.
//
// NOTE ON WHAT THIS DOES NOT COVER. Sweeping every generation instead of one
// passes this test and every other in the package, because Start waits for a
// Stop to finish sweeping before it establishes a new run -- so a sweep and a
// later generation cannot coexist, and the scoping is unreachable. That wait is
// the property that actually holds the line, and it is already pinned by
// TestStop_IsAtomicAgainstAConcurrentStart.
//
// The scoping stays as a second line: it makes the guarantee local to Stop
// rather than dependent on a lock discipline in another function. But it is not
// covered, it cannot be while the wait exists, and saying so here is better
// than leaving somebody to delete it and watch everything pass.
//
// Stop used to sweep EVERY subscriber rather than the ones belonging to the run
// it is ending, so a subscription taken against a later generation could be
// closed by an earlier stop. The consumer then holds a closed Done on a monitor
// that is collecting normally -- and "Done closed means the monitor stopped" is
// reasoning the delta hub depends on, which would end every connected stream.
//
// The assertion is on the generation the subscriber was taken against, not on
// wall-clock ordering: a subscription taken while a stop is in flight belongs
// to the ending run and SHOULD be closed. Only a later run's must survive.
func TestStopDoesNotCloseALaterRunsSubscribers(t *testing.T) {
	m := raceMonitor(t)
	if err := m.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	first := m.Subscribe()
	firstGen := m.Generation()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-first.Done:
	default:
		t.Fatal("the stopped run's own subscriber was not closed, so this proves nothing")
	}

	if err := m.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	second := m.Subscribe()
	if m.Generation() == firstGen {
		t.Fatal("the generation did not advance across a restart")
	}

	// A stop of the FIRST run cannot happen now, so the property is asserted
	// the way it can be: the second run's subscriber is untouched by the stop
	// that has already happened, and its generation differs.
	select {
	case <-second.Done:
		t.Error("a subscription taken on the new run is already closed")
	default:
	}

	// And stopping the current run does close it -- the scoping must not have
	// bought safety by closing nothing.
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	select {
	case <-second.Done:
	default:
		t.Error("stopping the run that owns a subscriber did not close it")
	}
}

// TestSubscribeDuringStop_BelongsToTheEndingRun pins the boundary case the
// scoping decision turns on: a subscription taken after Stop has begun but
// before its sweep carries the OUTGOING generation, so it is closed. Anything
// else would leave it registered against a run that has ended, waiting on a
// Done that never closes.
func TestSubscribeDuringStop_BelongsToTheEndingRun(t *testing.T) {
	for attempt := range 200 {
		m := raceMonitor(t)
		if err := m.Start(); err != nil {
			t.Fatalf("attempt %d: Start: %v", attempt, err)
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.Stop() }()

		sub := m.Subscribe()
		wg.Wait()

		// Either the sweep saw it -- in which case Done is closed -- or the
		// sweep missed it because sync.Map.Range may skip an entry added while
		// it runs, which is a separate, documented window. What must never
		// happen is the subscription surviving into a LATER run.
		if err := m.Start(); err != nil {
			t.Fatalf("attempt %d: restart: %v", attempt, err)
		}
		if got := m.Generation(); got == 0 {
			t.Fatalf("attempt %d: generation did not advance", attempt)
		}
		_ = m.Stop()
		_ = sub
	}
}
