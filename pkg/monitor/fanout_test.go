package monitor_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
)

// fanOutHold stops one publication of a monitor after it is stored and
// before any subscriber is sent it -- the window in which Snapshot already
// answers with it -- and keeps it there until released. A test can then act
// inside that window every run, instead of landing in it one run in thousands
// (#214).
type fanOutHold struct {
	armed    atomic.Bool
	entered  chan struct{}
	released chan struct{}
	// over is closed when the test ends, so a publication still held -- the
	// test failed before releasing it -- lets the poll loop go and the
	// monitor's Stop return.
	over     chan struct{}
	overOnce sync.Once
}

// holdFanOut installs a fanOutHold on m. It must be called before Start.
func holdFanOut(m *monitor.Monitor) *fanOutHold {
	h := &fanOutHold{
		entered:  make(chan struct{}),
		released: make(chan struct{}),
		over:     make(chan struct{}),
	}
	monitor.SetFanningOut(m, func() {
		// Only the publication the hold was armed for waits; every other
		// broadcast goes straight through, so an unexpected one reaches the
		// subscribers and fails the test rather than hanging it.
		if !h.armed.CompareAndSwap(true, false) {
			return
		}
		select {
		case h.entered <- struct{}{}:
		case <-h.over:
			return
		}
		select {
		case <-h.released:
		case <-h.over:
		}
	})
	return h
}

// arm makes the next publication to reach its broadcast wait in the window.
// The caller must know no earlier publication is still on its way to one.
//
// The monitor is running by now, so the cleanup registered here runs before
// the one that stops it.
func (h *fanOutHold) arm(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { h.overOnce.Do(func() { close(h.over) }) })
	h.armed.Store(true)
}

// reached returns once the armed publication has been stored and is waiting
// to be broadcast.
func (h *fanOutHold) reached(t *testing.T) {
	t.Helper()
	await.Recv(t, "a publication to be stored and wait for its broadcast", h.entered)
}

// release lets the waiting publication be broadcast.
func (h *fanOutHold) release(t *testing.T) {
	t.Helper()
	await.Send(t, "the waiting publication to take its release", h.released, struct{}{})
}
