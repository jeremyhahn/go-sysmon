package watchdog_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
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

// tick is the ping interval. It paces the loop; no assertion depends on how
// many ticks fit in any stretch of wall-clock time.
const tick = 10 * time.Millisecond

// ticks is how many complete ticks a test watches before judging the pings.
// Twenty, so that zero pings is not luck.
const ticks = 20

// countingProbe answers with the result of fn and counts its entries. The loop
// probes first on every tick, so entry n+1 proves ticks 1..n have finished --
// pings and all -- which makes the count of completed ticks exact.
type countingProbe struct {
	entered atomic.Int64
	fn      func(context.Context) error
}

func (p *countingProbe) probe(ctx context.Context) error {
	p.entered.Add(1)
	return p.fn(ctx)
}

// awaitTicks returns once n ticks of the loop have completed.
func (p *countingProbe) awaitTicks(t *testing.T, n int64) {
	t.Helper()
	await.Until(t, fmt.Sprintf("%d completed ticks", n), func(context.Context) bool { return p.entered.Load() > n })
}

// start runs w until the test ends, then cancels it and waits for Run to
// return.
func start(t *testing.T, w *watchdog.Watchdog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		await.Recv(t, "Run to return after its context was cancelled", done)
	})
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
	probe := &countingProbe{fn: func(context.Context) error { return probeErr }}

	start(t, watchdog.New(p, tick, probe.probe, nil, quiet()))
	probe.awaitTicks(t, ticks)

	if got := p.count(); got != 0 {
		t.Fatalf("sent %d pings over %d ticks while the probe was failing, want 0: a ping here tells systemd a dead service is healthy", got, ticks)
	}
}

// The same for the other signal. A server answering requests while collection
// is wedged is serving readings from before the stall and presenting them as
// current -- which the probe alone cannot see, because the probe gets a
// perfectly good response.
func TestRun_DoesNotPingWhileCollectionIsStale(t *testing.T) {
	p := &countingPinger{}
	staleErr := errors.New("last snapshot is 4m old")
	probe := &countingProbe{fn: ok}

	start(t, watchdog.New(p, tick, probe.probe, func() error { return staleErr }, quiet()))
	probe.awaitTicks(t, ticks)

	if got := p.count(); got != 0 {
		t.Fatalf("sent %d pings over %d ticks while collection was stale, want 0", got, ticks)
	}
}

// And the control: with both signals good it must actually ping, or the two
// tests above would pass against a watchdog that never pings at all. Every
// completed tick pings, so the count is exact.
func TestRun_PingsWhenBothSignalsHold(t *testing.T) {
	p := &countingPinger{}
	probe := &countingProbe{fn: ok}

	start(t, watchdog.New(p, tick, probe.probe, func() error { return nil }, quiet()))
	probe.awaitTicks(t, ticks)

	if got := p.count(); got < ticks {
		t.Fatalf("sent %d pings over %d ticks with a healthy probe and current collection, want one per tick", got, ticks)
	}
}

// A signal that recovers must be believed again: a watchdog that latched on
// first failure would restart a service that had already recovered, and the
// stall this exists for is transient before it is permanent.
func TestRun_ResumesPingingAfterTheProbeRecovers(t *testing.T) {
	p := &countingPinger{}
	var failing atomic.Bool
	failing.Store(true)
	probe := &countingProbe{fn: func(context.Context) error {
		if failing.Load() {
			return errors.New("still stalled")
		}
		return nil
	}}

	start(t, watchdog.New(p, tick, probe.probe, nil, quiet()))
	probe.awaitTicks(t, ticks/2)
	if during := p.count(); during != 0 {
		t.Fatalf("sent %d pings during the stall, want 0", during)
	}

	failing.Store(false)
	await.Until(t, "a ping after the probe recovered", func(context.Context) bool { return p.count() > 0 })
}

// A ping that fails is logged and the loop continues. Returning would leave the
// service unwatched for the rest of its life after one transient socket error,
// which is a worse outcome than a restart.
func TestRun_KeepsGoingWhenAPingFails(t *testing.T) {
	p := &failingPinger{}

	start(t, watchdog.New(p, tick, ok, nil, quiet()))
	await.Until(t, "a second ping attempt after the first failed", func(context.Context) bool { return p.n.Load() >= 2 })
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

			await.Recv(t, "Run to return with no watchdog configured", done)
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
// visible only in the loop continuing to try: the loop's context is not
// cancelled until the test ends, so an unbounded probe is entered once and
// never again, and the wait for later entries fails.
func TestRun_DoesNotPingWhenTheProbeHangs(t *testing.T) {
	p := &countingPinger{}
	probe := &countingProbe{fn: func(ctx context.Context) error {
		<-ctx.Done() // as long as it is allowed to
		return ctx.Err()
	}}

	start(t, watchdog.New(p, tick, probe.probe, nil, quiet()))
	probe.awaitTicks(t, 5)

	if got := p.count(); got != 0 {
		t.Fatalf("sent %d pings while every probe hung, want 0", got)
	}
}
