// Package monitor provides the core aggregation service that polls a Snapshotter
// and broadcasts system snapshots to registered subscribers.
package monitor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const (
	// DefaultInterval is the default polling interval.
	DefaultInterval = time.Second

	// subscriberBufSize is the capacity of each subscriber's channel.
	// Slow consumers drop old snapshots rather than blocking the broadcast loop.
	subscriberBufSize = 4
)

// Snapshotter is implemented by any type that can produce a system snapshot.
type Snapshotter interface {
	Snapshot() (*types.Snapshot, error)
}

// Subscriber holds the channel and lifecycle state for a single consumer.
type Subscriber struct {
	// ID uniquely identifies this subscriber.
	ID uint64
	// Ch delivers snapshots to the consumer. It is buffered; see subscriberBufSize.
	Ch chan *types.Snapshot
	// Done is closed when the subscriber is removed via Unsubscribe.
	Done chan struct{}
}

// Monitor polls a Snapshotter on a fixed interval and broadcasts every
// resulting snapshot to all registered subscribers.
type Monitor struct {
	snapshotter Snapshotter

	// intervalNs is the polling interval in nanoseconds. It is read by the
	// poll loop and written by SetInterval from arbitrary goroutines, so it is
	// atomic rather than a plain time.Duration field.
	intervalNs atomic.Int64

	// mu guards the start/stop lifecycle: running, cancel and stopped. These
	// three must change together, and a plain atomic on each would still allow
	// Stop to observe running == true before Start has assigned cancel.
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	// stopped is closed by the poll goroutine as it exits, letting Stop wait
	// for the loop to finish instead of returning while it is mid-broadcast.
	stopped chan struct{}

	// subscribers holds *Subscriber values keyed by uint64 ID.
	subscribers sync.Map

	// nextID generates monotonically increasing subscriber IDs.
	nextID atomic.Uint64

	// latest holds the most recent *types.Snapshot (atomic pointer).
	latest atomic.Pointer[types.Snapshot]

	// intervalCh delivers a new interval to the running poll loop.
	// Buffered with capacity 1 so SetInterval never blocks the caller.
	intervalCh chan time.Duration
}

// New creates a Monitor with the given Snapshotter and polling interval.
// Returns an InvalidIntervalError when interval is less than or equal to zero.
func New(s Snapshotter, interval time.Duration) (*Monitor, error) {
	if interval <= 0 {
		return nil, &types.InvalidIntervalError{Message: "must be greater than zero"}
	}
	m := &Monitor{
		snapshotter: s,
		intervalCh:  make(chan time.Duration, 1),
	}
	m.intervalNs.Store(int64(interval))
	return m, nil
}

// Interval returns the current polling interval.
func (m *Monitor) Interval() time.Duration {
	return time.Duration(m.intervalNs.Load())
}

// Start begins the poll loop. Returns MonitorAlreadyRunningError if already started.
func (m *Monitor) Start() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return &types.MonitorAlreadyRunningError{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	m.running = true
	m.cancel = cancel
	m.stopped = stopped
	m.mu.Unlock()

	go m.run(ctx, stopped)
	return nil
}

// Stop halts the poll loop and closes all subscriber Done channels.
// Returns MonitorNotRunningError if not started.
func (m *Monitor) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return &types.MonitorNotRunningError{}
	}
	m.running = false
	cancel, stopped := m.cancel, m.stopped
	m.cancel, m.stopped = nil, nil
	m.mu.Unlock()

	cancel()

	// Wait for the poll loop to exit so that no broadcast can land after Stop
	// returns; subscribers would otherwise see a snapshot after their close.
	<-stopped

	// Signal all subscribers that the monitor has stopped.
	m.subscribers.Range(func(_, value any) bool {
		sub := value.(*Subscriber)
		select {
		case <-sub.Done:
			// already closed
		default:
			close(sub.Done)
		}
		return true
	})

	return nil
}

// Subscribe registers a new consumer and returns its Subscriber.
// The caller receives snapshots on Subscriber.Ch until Unsubscribe is called
// or the monitor is stopped (Subscriber.Done is closed in both cases).
func (m *Monitor) Subscribe() *Subscriber {
	sub := &Subscriber{
		ID:   m.nextID.Add(1),
		Ch:   make(chan *types.Snapshot, subscriberBufSize),
		Done: make(chan struct{}),
	}
	m.subscribers.Store(sub.ID, sub)
	return sub
}

// Unsubscribe removes the subscriber with the given ID and closes its Done channel.
// It is safe to call Unsubscribe more than once for the same ID.
func (m *Monitor) Unsubscribe(id uint64) {
	value, loaded := m.subscribers.LoadAndDelete(id)
	if !loaded {
		return
	}
	sub := value.(*Subscriber)
	select {
	case <-sub.Done:
		// already closed
	default:
		close(sub.Done)
	}
}

// Snapshot returns the most recently collected snapshot without waiting for
// the next poll tick. Returns MonitorNotRunningError when the monitor has
// never collected a snapshot yet (latest is nil).
func (m *Monitor) Snapshot() (*types.Snapshot, error) {
	snap := m.latest.Load()
	if snap == nil {
		return nil, &types.MonitorNotRunningError{}
	}
	return snap, nil
}

// SetInterval changes the polling interval. The change takes effect on the
// next tick of the running loop. Returns InvalidIntervalError when d is <= 0.
// It is safe to call concurrently with any other method.
func (m *Monitor) SetInterval(d time.Duration) error {
	if d <= 0 {
		return &types.InvalidIntervalError{Message: "must be greater than zero"}
	}
	m.intervalNs.Store(int64(d))
	// Non-blocking send; if a previous pending change exists, replace it so
	// the latest value always wins.
	select {
	case m.intervalCh <- d:
	default:
		select {
		case <-m.intervalCh:
		default:
		}
		m.intervalCh <- d
	}
	return nil
}

// run is the internal poll loop executed in a dedicated goroutine.
func (m *Monitor) run(ctx context.Context, stopped chan struct{}) {
	defer close(stopped)

	ticker := time.NewTicker(m.Interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case d := <-m.intervalCh:
			ticker.Reset(d)
		case <-ticker.C:
			snap, err := m.snapshotter.Snapshot()
			if err != nil {
				// A collection error is non-fatal; the loop continues on the next tick.
				continue
			}
			m.latest.Store(snap)
			m.broadcast(snap)
		}
	}
}

// broadcast delivers snap to every registered subscriber.
// If a subscriber's channel is full the snapshot is dropped for that subscriber
// so that one slow consumer cannot stall the broadcast loop.
func (m *Monitor) broadcast(snap *types.Snapshot) {
	m.subscribers.Range(func(_, value any) bool {
		sub := value.(*Subscriber)
		select {
		case sub.Ch <- snap:
		default:
			// channel full – discard oldest by draining one slot then re-sending.
			select {
			case <-sub.Ch:
			default:
			}
			select {
			case sub.Ch <- snap:
			default:
			}
		}
		return true
	})
}
