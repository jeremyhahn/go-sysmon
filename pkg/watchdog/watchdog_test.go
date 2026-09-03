package watchdog_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/watchdog"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// countingPinger records pings instead of sending them.
type countingPinger struct{ n atomic.Int64 }

func (p *countingPinger) Watchdog() error { p.n.Add(1); return nil }
func (p *countingPinger) count() int64    { return p.n.Load() }

// failingPinger stands in for a notification socket that has gone away.
type failingPinger struct{ n atomic.Int64 }

func (p *failingPinger) Watchdog() error { p.n.Add(1); return errors.New("socket closed") }

const (
	tick  = 10 * time.Millisecond
	watch = 200 * time.Millisecond // twenty ticks: enough that zero is not luck
)

// run drives a watchdog for watch and returns.
func run(t *testing.T, w *watchdog.Watchdog) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), watch)
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(watch + 2*time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func ok(context.Context) error { return nil }

// THE test. A watchdog that pings while the serving path is broken is worse
// than no watchdog: it converts a dead service into a dead service systemd has
// confirmed healthy. This asserts the ABSENCE of pings, which is the only
// assertion a bare ticker cannot satisfy -- remove the probe gate from tick and
// this fails while every other test here still passes.
func TestRun_DoesNotPingWhileTheServerCannotAnswer(t *testing.T) {
	p := &countingPinger{}
	probeErr := errors.New("connection refused")

	w := watchdog.New(p, tick, func(context.Context) error { return probeErr }, nil, quiet())
	run(t, w)

	if got := p.count(); got != 0 {
		t.Fatalf("sent %d pings while the probe was failing, want 0: a ping here tells systemd a dead service is healthy", got)
	}
}

// The same for the other signal. A server answering requests while collection
// is wedged is serving readings from before the stall and presenting them as
// current -- which the probe alone cannot see, because the probe gets a
// perfectly good response.
func TestRun_DoesNotPingWhileCollectionIsStale(t *testing.T) {
	p := &countingPinger{}
	staleErr := errors.New("last snapshot is 4m old")

	w := watchdog.New(p, tick, ok, func() error { return staleErr }, quiet())
	run(t, w)

	if got := p.count(); got != 0 {
		t.Fatalf("sent %d pings while collection was stale, want 0", got)
	}
}

// And the control: with both signals good it must actually ping, or the two
// tests above would pass against a watchdog that never pings at all.
func TestRun_PingsWhenBothSignalsHold(t *testing.T) {
	p := &countingPinger{}

	w := watchdog.New(p, tick, ok, func() error { return nil }, quiet())
	run(t, w)

	if got := p.count(); got == 0 {
		t.Fatal("sent no pings with a healthy probe and current collection")
	}
}

// A signal that recovers must be believed again: a watchdog that latched on
// first failure would restart a service that had already recovered, and the
// stall this exists for is transient before it is permanent.
func TestRun_ResumesPingingAfterTheProbeRecovers(t *testing.T) {
	p := &countingPinger{}
	var failing atomic.Bool
	failing.Store(true)

	probe := func(context.Context) error {
		if failing.Load() {
			return errors.New("still stalled")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), watch)
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); watchdog.New(p, tick, probe, nil, quiet()).Run(ctx) }()

	time.Sleep(watch / 2)
	during := p.count()
	failing.Store(false)
	<-done

	if during != 0 {
		t.Fatalf("sent %d pings during the stall, want 0", during)
	}
	if after := p.count(); after == 0 {
		t.Fatal("sent no pings after the probe recovered")
	}
}

// A ping that fails is logged and the loop continues. Returning would leave the
// service unwatched for the rest of its life after one transient socket error,
// which is a worse outcome than a restart.
func TestRun_KeepsGoingWhenAPingFails(t *testing.T) {
	p := &failingPinger{}

	w := watchdog.New(p, tick, ok, nil, quiet())
	run(t, w)

	if got := p.n.Load(); got < 2 {
		t.Fatalf("attempted %d pings, want the loop to continue past a failure", got)
	}
}

// Outside systemd there is nothing to notify, and that has to be silent rather
// than an error: the same binary runs from a shell and in a container.
func TestRun_ReturnsImmediatelyWhenNotConfigured(t *testing.T) {
	tests := []struct {
		name string
		w    *watchdog.Watchdog
	}{
		{"no pinger", watchdog.New(nil, tick, ok, nil, quiet())},
		{"no interval", watchdog.New(&countingPinger{}, 0, ok, nil, quiet())},
		{"no probe", watchdog.New(&countingPinger{}, tick, nil, nil, quiet())},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan struct{})
			// A context that is never cancelled: Run has to return on its own,
			// and a Run that waited for cancellation would pass a test that
			// cancelled for it.
			go func() { defer close(done); tt.w.Run(context.Background()) }()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return with no watchdog configured")
			}
		})
	}
}

// A probe that hangs must not hang the loop, and on a stalled server hanging is
// the normal case rather than the exception: the request is accepted into a
// queue nothing is draining and never answered.
//
// Counting ENTRIES to the probe, not pings, is what makes this test mean
// something. Asserting only "no pings" passes whether the probe is bounded or
// blocks the loop forever on its first call -- both send zero. The bound is
// visible only in the loop continuing to try.
//
// The threshold is 5 and not 2, which was measured rather than chosen.
// Unbounded, the probe is entered TWICE, not once: cancelling the outer context
// releases the blocked probe, and the loop then finds ctx.Done and a pending
// tick both ready and picks between them at random. Twenty ticks give 19
// entries when the probe is bounded and 2 when it is not, so a threshold of 2
// passes against the defect -- which is what it did on the first attempt here.
func TestRun_DoesNotPingWhenTheProbeHangs(t *testing.T) {
	p := &countingPinger{}
	var entered atomic.Int64

	probe := func(ctx context.Context) error {
		entered.Add(1)
		<-ctx.Done() // as long as it is allowed to
		return ctx.Err()
	}

	w := watchdog.New(p, tick, probe, nil, quiet())
	run(t, w)

	if got := p.count(); got != 0 {
		t.Fatalf("sent %d pings while every probe hung, want 0", got)
	}
	if got := entered.Load(); got < 5 {
		t.Fatalf("the probe was entered %d time(s) over %d ticks: an unbounded probe wedged the watchdog itself", got, watch/tick)
	}
}
