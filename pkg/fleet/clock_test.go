package fleet

import (
	"sync/atomic"
	"time"
)

// manualClock is a Poller clock that moves only when a test advances it, so a
// duration or an age is stated exactly rather than waited out in real time.
type manualClock struct {
	ns atomic.Int64
}

// clockEpoch is where every manualClock starts.
var clockEpoch = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// newManualClock returns a clock standing at clockEpoch.
func newManualClock() *manualClock {
	c := &manualClock{}
	c.ns.Store(clockEpoch.UnixNano())
	return c
}

// Now returns the clock's current time.
func (c *manualClock) Now() time.Time {
	return time.Unix(0, c.ns.Load()).UTC()
}

// Advance moves the clock forward by d.
func (c *manualClock) Advance(d time.Duration) {
	c.ns.Add(int64(d))
}

// manualTimer is a worker timer that fires only when a test fires it, and
// reports every Reset, so a test can state exactly when a worker turns and
// what interval it armed for the next turn.
type manualTimer struct {
	c      chan time.Time
	resets chan time.Duration
}

// newManualTimers returns a timer factory for recorder.newTimer and a channel
// delivering each timer it makes, in the order the workers start.
func newManualTimers() (func(time.Duration) timer, <-chan *manualTimer) {
	made := make(chan *manualTimer, 16)
	return func(time.Duration) timer {
		mt := &manualTimer{c: make(chan time.Time), resets: make(chan time.Duration, 16)}
		made <- mt
		return mt
	}, made
}

// Chan returns the channel the test fires on.
func (m *manualTimer) Chan() <-chan time.Time { return m.c }

// Reset records d.
func (m *manualTimer) Reset(d time.Duration) bool {
	m.resets <- d
	return false
}

// Stop does nothing: the timer never fires on its own.
func (m *manualTimer) Stop() bool { return false }
