package monitor_test

import (
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
)

// manualTicker paces a monitor's poll loop by hand. Its channel is
// unbuffered, so tick returns only once the loop has taken the tick -- which
// it can do only after finishing whatever the previous tick started. A test
// therefore knows exactly how many collections have happened and that each
// one's broadcast has completed, instead of estimating it from elapsed time.
type manualTicker struct {
	ticks    chan time.Time
	resets   chan time.Duration
	stopped  chan struct{}
	stopOnce sync.Once
}

// useManualTicker makes m's poll loop run on a manualTicker and returns it.
func useManualTicker(m *monitor.Monitor) *manualTicker {
	mt := &manualTicker{
		ticks:   make(chan time.Time),
		resets:  make(chan time.Duration, 16),
		stopped: make(chan struct{}),
	}
	monitor.SetTicker(m, func(time.Duration) monitor.Ticker { return mt })
	return mt
}

func (mt *manualTicker) Chan() <-chan time.Time { return mt.ticks }

func (mt *manualTicker) Reset(d time.Duration) { mt.resets <- d }

func (mt *manualTicker) Stop() { mt.stopOnce.Do(func() { close(mt.stopped) }) }

// tick delivers one tick and returns once the poll loop has taken it.
func (mt *manualTicker) tick(t *testing.T) {
	t.Helper()
	await.Send(t, "the poll loop to take a tick", mt.ticks, time.Now())
}

// isStopped reports whether the poll loop has stopped the ticker, which it
// does only on its way out.
func (mt *manualTicker) isStopped() bool {
	select {
	case <-mt.stopped:
		return true
	default:
		return false
	}
}
