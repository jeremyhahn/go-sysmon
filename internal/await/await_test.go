package await

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recorder is a TB that records a failure instead of ending the goroutine, so
// the failure paths can be asserted on.
type recorder struct {
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, format)
}

// expired returns a bound that has already fired, so a failure path is taken
// without any real time passing.
func expired() <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

// ended returns a context that has already ended, as Until's does at the
// bound.
func ended() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestUntil_GivesTheConditionTheBoundingContext(t *testing.T) {
	Until(t, "a condition that checks its context", func(ctx context.Context) bool {
		deadline, ok := ctx.Deadline()
		return ok && ctx.Err() == nil && !deadline.IsZero()
	})
}

func TestRecv_ReturnsTheValueSent(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 42
	if got := Recv(t, "the value", ch); got != 42 {
		t.Fatalf("Recv = %d, want 42", got)
	}
}

func TestRecv_WaitsForAClosedChannel(t *testing.T) {
	done := make(chan struct{})
	go close(done)
	Recv(t, "done to close", done)
}

func TestRecv_FailsWhenNothingArrives(t *testing.T) {
	r := &recorder{}
	got := recv(r, "a value", make(chan int), expired())
	if got != 0 {
		t.Errorf("recv on a timeout = %d, want the zero value", got)
	}
	if len(r.failures) != 1 || !strings.Contains(r.failures[0], "did not happen") {
		t.Fatalf("failures = %q, want one timeout", r.failures)
	}
}

func TestSend_IsAcceptedByAReceiver(t *testing.T) {
	ch := make(chan int)
	got := make(chan int, 1)
	go func() { got <- <-ch }()
	Send(t, "the value", ch, 7)
	if v := Recv(t, "the relayed value", got); v != 7 {
		t.Fatalf("receiver got %d, want 7", v)
	}
}

func TestSend_FailsWhenNobodyReceives(t *testing.T) {
	r := &recorder{}
	send(r, "a value", make(chan int), 1, expired())
	if len(r.failures) != 1 || !strings.Contains(r.failures[0], "not accepted") {
		t.Fatalf("failures = %q, want one timeout", r.failures)
	}
}

func TestUntil_ReturnsOnceTheConditionHolds(t *testing.T) {
	calls := 0
	Until(t, "the third check", func(context.Context) bool {
		calls++
		return calls == 3
	})
	if calls != 3 {
		t.Fatalf("condition evaluated %d times, want 3", calls)
	}
}

func TestUntil_DoesNotWaitForAConditionAlreadyTrue(t *testing.T) {
	r := &recorder{}
	// Neither channel ever fires: a condition true on entry must not wait.
	until(context.Background(), r, "an immediate condition", func(context.Context) bool { return true }, nil)
	if len(r.failures) != 0 {
		t.Fatalf("failures = %q, want none", r.failures)
	}
}

func TestUntil_FailsWhenTheConditionNeverHolds(t *testing.T) {
	r := &recorder{}
	until(ended(), r, "an impossible condition", func(context.Context) bool { return false }, nil)
	if len(r.failures) != 1 || !strings.Contains(r.failures[0], "did not become true") {
		t.Fatalf("failures = %q, want one timeout", r.failures)
	}
}

func TestParked_CountsAGoroutineBlockedInAFrame(t *testing.T) {
	release := make(chan struct{})
	parked := make(chan struct{})
	go func() {
		close(parked)
		blockUntil(release)
	}()
	Recv(t, "the goroutine to start", parked)
	Until(t, "the goroutine to block in blockUntil", func(context.Context) bool {
		return Parked("await.blockUntil(") == 1
	})
	close(release)
	Until(t, "the goroutine to leave blockUntil", func(context.Context) bool {
		return Parked("await.blockUntil(") == 0
	})
}

// blockUntil blocks until release is closed; its name is what the test above
// looks for in the goroutine dump.
func blockUntil(release <-chan struct{}) {
	<-release
}

func TestCountParked_IgnoresRunningGoroutinesAndPartialMatches(t *testing.T) {
	dump := "goroutine 1 [running]:\nmain.a(...)\nmain.b(...)\n\n" +
		"goroutine 2 [chan receive]:\nmain.a(...)\nmain.b(...)\n\n" +
		"goroutine 3 [select]:\nmain.a(...)\n\n" +
		"goroutine 4 [runnable]:\nmain.a(...)\nmain.b(...)"
	if got := countParked(dump, []string{"main.a(", "main.b("}); got != 1 {
		t.Fatalf("countParked = %d, want only the blocked goroutine with both frames", got)
	}
	if got := countParked(dump, []string{"main.missing("}); got != 0 {
		t.Fatalf("countParked = %d for a frame on no stack, want 0", got)
	}
}

func TestGoroutineDump_GrowsUntilTheDumpFits(t *testing.T) {
	dump := goroutineDump(1)
	if !strings.Contains(dump, "await.TestGoroutineDump_GrowsUntilTheDumpFits(") {
		t.Fatalf("a dump grown from one byte lacks this test's own goroutine:\n%s", dump)
	}
}
