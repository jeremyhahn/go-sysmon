// Package await synchronises tests on observable state instead of on the
// passage of wall-clock time.
//
// A test that sleeps and then counts is measuring the scheduler: it passes on
// an idle machine and fails on a loaded CI runner although the code under test
// is correct. Every helper here waits for a named event -- a value on a
// channel, a send accepted, a condition becoming true -- and fails the test
// only if that event has not happened within Timeout. The timeout is a failure
// bound, never the thing measured: a correct build meets the event long before
// it, however busy the host, and only a broken one ever reaches it.
package await

import (
	"context"
	"runtime"
	"strings"
	"time"
)

// Timeout bounds every wait. It is deliberately generous: it is reached only
// when the awaited event is never going to happen, so its length costs nothing
// on a passing run and keeps a starved runner from failing a correct build.
const Timeout = 30 * time.Second

// pollEvery is how often Until re-checks its condition.
const pollEvery = time.Millisecond

// TB is the part of testing.TB the helpers use. *testing.T and *testing.B
// satisfy it.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Recv returns the next value received from ch, failing the test if none
// arrives within Timeout. A closed channel yields its zero value at once, so
// Recv also waits for a done channel to be closed. what names the awaited
// event in the failure message.
func Recv[E any](t TB, what string, ch <-chan E) E {
	t.Helper()
	bound := time.NewTimer(Timeout)
	defer bound.Stop()
	return recv(t, what, ch, bound.C)
}

// Send delivers v on ch, failing the test if no receiver accepts it within
// Timeout. On an unbuffered channel a completed Send therefore proves the
// receiver reached its receive.
func Send[E any](t TB, what string, ch chan<- E, v E) {
	t.Helper()
	bound := time.NewTimer(Timeout)
	defer bound.Stop()
	send(t, what, ch, v, bound.C)
}

// Until returns once cond reports true, failing the test if it has not within
// Timeout. cond is re-evaluated every pollEvery, so it must read the real
// state each time. The context it is given ends at the bound: a condition that
// does I/O -- dialling a listener, making a request -- must use it, so a peer
// that accepts and then never answers fails the test at the bound instead of
// hanging it. Prefer Recv where the code under test signals the event: Until
// is for state with no notification to wait on, such as a listener becoming
// reachable.
func Until(t TB, what string, cond func(ctx context.Context) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	until(ctx, t, what, cond, poll.C)
}

// Parked reports how many goroutines are blocked -- neither running nor
// runnable -- with every one of frames on their stack. A frame is matched as a
// substring of its function's line in the goroutine dump, such as
// "monitor.(*Monitor).Start(".
//
// It is for asserting that something is still waiting: a check that a result
// has not arrived proves nothing until the goroutine that would deliver it has
// reached its wait, and the goroutine dump is the only place that is
// observable. Pair it with Until, and include the "it returned" case in the
// condition so a goroutine that never parks fails the test rather than waiting
// out the bound.
func Parked(frames ...string) int {
	return countParked(goroutineDump(1<<20), frames)
}

// goroutineDump returns the stacks of every goroutine, starting from a buffer
// of size bytes and doubling it until the dump fits.
func goroutineDump(size int) string {
	buf := make([]byte, size)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// countParked counts the goroutines in dump that are blocked with every one of
// frames on their stack.
func countParked(dump string, frames []string) int {
	count := 0
	for _, g := range strings.Split(dump, "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[running]") || strings.Contains(header, "[runnable]") {
			continue
		}
		all := true
		for _, f := range frames {
			if !strings.Contains(g, f) {
				all = false
				break
			}
		}
		if all {
			count++
		}
	}
	return count
}

// recv is Recv with the bound supplied, so the failure path is testable
// without waiting for real time to pass.
func recv[E any](t TB, what string, ch <-chan E, bound <-chan time.Time) E {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-bound:
		t.Fatalf("await: %s did not happen within %v", what, Timeout)
		var zero E
		return zero
	}
}

// send is Send with the bound supplied.
func send[E any](t TB, what string, ch chan<- E, v E, bound <-chan time.Time) {
	t.Helper()
	select {
	case ch <- v:
	case <-bound:
		t.Fatalf("await: %s was not accepted within %v", what, Timeout)
	}
}

// until is Until with the bounding context and the poll signal supplied.
func until(ctx context.Context, t TB, what string, cond func(ctx context.Context) bool, poll <-chan time.Time) {
	t.Helper()
	for !cond(ctx) {
		select {
		case <-poll:
		case <-ctx.Done():
			t.Fatalf("await: %s did not become true within %v", what, Timeout)
			return
		}
	}
}
