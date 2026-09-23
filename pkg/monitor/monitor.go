// Package monitor provides the core aggregation service that polls a Snapshotter
// and broadcasts system snapshots to registered subscribers.
package monitor

import (
	"context"
	"reflect"
	"slices"
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

// Decorator attaches to each snapshot what the service knows rather than the
// hardware: which node and build is serving it, and the alerts its thresholds
// produce.
//
// It is applied inside the monitor because the monitor is the one door every
// served snapshot passes through -- the event stream in both protocols, GET
// /api/snapshot, the fleet's view of this node, /metrics and the exporter all
// read what it publishes. Decorating anywhere downstream of it meant each
// path had to remember to, and an embedded server's stream carried none of
// these fields while the CLI's did.
//
// A Decorator must be a comparable type, as a pointer is: the monitor compares
// it with the one installed.
type Decorator interface {
	// Identify sets the fields naming the serving node and its build. It must
	// depend only on the current configuration and never on the snapshot's
	// history, because the monitor re-applies it to the published snapshot
	// when that configuration changes (see Restamp).
	Identify(snap *types.Snapshot)
	// Evaluate sets the snapshot's alerts. It is called exactly once for each
	// snapshot, in collection order, and may keep state across calls: how
	// long a condition has lasted, and rates between two readings.
	Evaluate(snap *types.Snapshot)
}

// Subscriber holds the channel and lifecycle state for a single consumer.
type Subscriber struct {
	// ID uniquely identifies this subscriber.
	ID uint64
	// Ch delivers snapshots to the consumer. It is buffered; see subscriberBufSize.
	Ch chan *types.Snapshot
	// Done is closed when the subscriber is removed via Unsubscribe.
	Done chan struct{}

	// generation is the run this subscription was taken against, so a Stop
	// closes the subscribers of the run it is ending and no others.
	generation uint64

	// closeOnce guards Done. Two callers can reach a subscriber at once --
	// Stop sweeping the map and a consumer unsubscribing -- and both used to
	// check "is it closed" and then close it, which is not one operation. Both
	// could see it open and both could close it, which PANICS and takes the
	// process with it. Reachable in production: the SSE handler unsubscribes
	// when a client disconnects, and Stop runs whenever the poll interval
	// changes.
	closeOnce sync.Once
}

// close closes Done exactly once, however many callers reach it.
func (s *Subscriber) close() {
	s.closeOnce.Do(func() { close(s.Done) })
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
	// stopping is true from the moment a Stop begins until it has finished
	// closing subscribers. Start waits it out on settled rather than
	// proceeding, so a new generation cannot exist while the outgoing sweep is
	// still running and cannot have its subscribers closed by it.
	//
	// A flag and a condition rather than simply holding mu across the whole of
	// Stop: the wait is for a collection to finish, which is a syscall into
	// /proc, SMART or NVML and can hang. Holding the lock through it would
	// make one wedged collector block every Start, Stop and Running in the
	// process -- including the delta hub's check, which would stall the stream
	// rather than end it.
	stopping bool
	settled  *sync.Cond

	// generation advances on every Start. Read without the lock, so it is
	// atomic rather than guarded: a consumer comparing it against a value it
	// stamped earlier only needs to see a change, not to see it at any
	// particular instant.
	generation atomic.Uint64

	// subscribers holds *Subscriber values keyed by uint64 ID.
	subscribers sync.Map

	// nextID generates monotonically increasing subscriber IDs.
	nextID atomic.Uint64

	// latest holds the most recent reading: the snapshot last published and
	// the one this run published before it. One atomic pointer to the pair,
	// rather than one per snapshot, so a reader can never see a current
	// snapshot matched with a previous one from a different tick.
	latest atomic.Pointer[reading]

	// intervalCh delivers a new interval to the running poll loop.
	// Buffered with capacity 1 so SetInterval never blocks the caller.
	intervalCh chan time.Duration

	// pubMu serialises publishing: the poll loop's publication of a new
	// collection and a caller's re-decoration of the published one. Without
	// it a re-decoration could overwrite a newer collection, or a collection
	// identified under the previous configuration could overwrite the
	// re-decoration. Collection itself happens outside it, so a hung collector
	// never blocks a re-decoration. A mutex rather than a channel into the
	// loop for that reason: the loop may be inside a collection that does not
	// return, and a rename must still land.
	pubMu sync.Mutex
	// decorator is applied to every snapshot before it is published; nil
	// publishes snapshots as collected. Guarded by pubMu, so installing or
	// removing one is ordered against every publication.
	decorator Decorator
	// live is whether a run is publishing, as seen by whoever holds pubMu.
	// Running answers the same question under mu, but a re-decoration holds
	// pubMu and must not take mu: Start takes pubMu while holding mu, to make
	// its reset of the published snapshot one step for anyone publishing.
	// Start sets it and Stop clears it, both under pubMu.
	live bool
	// evaluations records, for the published reading, every decorator whose
	// Evaluate has run on it and the alerts it produced. Decorators are
	// compared by identity, so one removed and installed again is recognised
	// however many others were installed in between: a re-decoration reuses
	// its recorded alerts rather than evaluating the reading a second time,
	// which would feed a stateful evaluator a zero-length interval. A fresh
	// collection resets it. Guarded by pubMu; in practice it holds one entry.
	evaluations []evaluation
}

// evaluation is one decorator's Evaluate of the published reading.
type evaluation struct {
	by     Decorator
	alerts []types.Alert
}

// reading is one published snapshot and the baseline of the one the same run
// published immediately before it; prev is nil for a run's first collection.
type reading struct {
	prev *types.Snapshot
	cur  *types.Snapshot
}

// baseline is what the monitor keeps of a snapshot once a newer one replaces
// it: its timestamp, its boot time, and its memory section, which hold the
// cumulative counters a caller differences across one interval (the swap
// paging counters) and what says whether they share an origin. The rest
// -- the process list and per-CPU flags above all -- is what makes a busy
// host's snapshot large, and keeping all of it for every node, standalone
// ones included, would double the memory the monitor holds for two numbers.
func baseline(s *types.Snapshot) *types.Snapshot {
	return &types.Snapshot{
		Timestamp: s.Timestamp,
		Host:      types.HostInfo{BootTime: s.Host.BootTime},
		Memory:    s.Memory,
	}
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
	m.settled = sync.NewCond(&m.mu)
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
	// A stop in progress is finished first. Wait releases mu, so this blocks
	// this caller and nothing else.
	for m.stopping {
		m.settled.Wait()
	}
	if m.running {
		m.mu.Unlock()
		return &types.MonitorAlreadyRunningError{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	// The new generation, the cleared snapshot and the live flag change
	// together under pubMu, so a re-decoration sees either the previous run's
	// state or this one's -- never this run's number beside the last run's
	// reading, which it would then broadcast to this run's subscribers.
	//
	// All of it before running is set: pubMu can be held by a re-decoration,
	// and a subscription taken while this waits must not see the monitor
	// running under the previous generation, or no Stop would ever close it.
	m.pubMu.Lock()
	m.live = true
	m.generation.Add(1)
	// A new run has not collected anything yet, so it has no latest snapshot.
	//
	// The reading from the previous run is deliberately left in place by Stop,
	// so a caller can read the final state of a monitor that has finished. It
	// must not survive into the next one: Snapshot() means "the most recent
	// collection", and after a restart the most recent collection of THIS run
	// has not happened. Leaving it made the delta hub publish a pre-restart
	// reading as the first frame of the new stream, stamped with the new
	// generation, and marked current.
	m.latest.Store(nil)
	m.pubMu.Unlock()
	m.running = true
	m.cancel = cancel
	m.stopped = stopped
	m.mu.Unlock()

	go m.run(ctx, stopped)
	return nil
}

// Stop halts the poll loop and closes all subscriber Done channels.
// Returns MonitorNotRunningError if not started.
//
// A stop is one step as far as any other caller is concerned: no new
// generation can exist until this one has finished closing its subscribers.
//
// It used to release the lock and then wait, which let a concurrent Start
// establish a new running monitor in the middle. A subscriber taken against
// that new generation -- correctly, with Running() true -- was then closed by
// the outgoing sweep, which does not know one generation from another. The
// delta hub reads a closed subscription as "the monitor stopped", so it ended
// the stream and dropped every dashboard on a monitor that was running.
//
// The exclusion is a flag rather than the lock held throughout, because the
// wait is for a collection to finish and a collection is a syscall into /proc,
// SMART or NVML that can hang. Holding mu across it would make one wedged
// collector block every Start, Stop and Running in the process -- including
// the delta hub's check, which would stall the stream instead of ending it.
// With the flag, a hung collector blocks only this Stop and any Start behind
// it; Running answers false throughout, which is the truth.
func (m *Monitor) Stop() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return &types.MonitorNotRunningError{}
	}
	m.running = false
	m.stopping = true
	cancel, stopped := m.cancel, m.stopped
	m.cancel, m.stopped = nil, nil
	m.mu.Unlock()

	// Cleared however this returns, so a hung collector cannot leave every
	// later Start waiting on a stop that will never announce itself finished.
	defer func() {
		m.mu.Lock()
		m.stopping = false
		m.settled.Broadcast()
		m.mu.Unlock()
	}()

	cancel()

	// Wait for the poll loop to exit so that no broadcast can land after Stop
	// returns; subscribers would otherwise see a snapshot after their close.
	<-stopped

	// Signal the subscribers of THIS run that the monitor has stopped.
	//
	// Scoped by generation rather than sweeping everything: a subscription
	// belonging to a later run must not be closed by an earlier run's stop,
	// which would hand its consumer a closed Done on a monitor that is
	// collecting normally -- and "Done closed means the monitor stopped" is
	// reasoning the delta hub depends on.
	//
	// A subscriber taken after this Stop began still carries the outgoing
	// generation and is still closed here, which is right: it belongs to the
	// run that is ending.
	// Under pubMu, so a restamp that saw the run still going has finished
	// broadcasting before any subscriber is closed; see redecorateLocked.
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	m.live = false
	ending := m.generation.Load()
	m.subscribers.Range(func(_, value any) bool {
		if sub := value.(*Subscriber); sub.generation == ending {
			sub.close()
		}
		return true
	})

	return nil
}

// Generation counts the runs of the poll loop: it advances on every Start, so
// two readings taken across a stop and a start carry different numbers.
//
// It exists because "the monitor is running" is not the same question as "the
// monitor is running the same collection it was". A consumer that caches
// anything derived from a snapshot -- the delta hub caches a published frame,
// to serve a client that connects between ticks -- needs the second one:
// Running() is true again the instant a restart completes, while the cached
// value is from before the stop and as old as the gap between them.
func (m *Monitor) Generation() uint64 {
	return m.generation.Load()
}

// Running reports whether the poll loop is collecting.
//
// A stopped monitor still answers Snapshot with the last thing it collected --
// that is deliberate, so a caller can read the final state -- and still hands
// out subscriptions that nothing will ever write to or close, because Stop has
// already walked the subscriber map. Both are traps for a consumer that
// restarts on demand, so it needs to be able to ask.
func (m *Monitor) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Subscribe registers a new consumer and returns its Subscriber.
// The caller receives snapshots on Subscriber.Ch until Unsubscribe is called
// or the monitor is stopped (Subscriber.Done is closed in both cases).
func (m *Monitor) Subscribe() *Subscriber {
	sub := &Subscriber{
		ID:   m.nextID.Add(1),
		Ch:   make(chan *types.Snapshot, subscriberBufSize),
		Done: make(chan struct{}),
		// Stamped before registering, so a Stop that sweeps immediately after
		// the Store sees the generation this subscription was taken against
		// rather than a zero value that matches nothing.
		generation: m.generation.Load(),
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
	value.(*Subscriber).close()
}

// Snapshot returns the most recently collected snapshot without waiting for
// the next poll tick.
//
// There are three answers, not two, and the error says which:
//
//   - a snapshot and no error -- this run, or the run before a Stop, collected
//     one. A stopped monitor still answers with its last reading, deliberately,
//     so a caller can read the final state.
//   - NoSnapshotYetError -- the loop is running and has not finished its first
//     collection. Normal, transient, and the state every healthy start passes
//     through; the caller should retry rather than investigate.
//   - MonitorNotRunningError -- nothing is collecting and nothing has been
//     collected. Start was never called, or it was called and Stop cleared the
//     way for a run that has not begun.
//
// Both used to be MonitorNotRunningError, which named the rarer cause of the
// two and pointed every reader of a warm-up at a lifecycle bug (#33).
//
// Running is consulted only when there is no snapshot to return, so the path
// that has one stays lock-free.
func (m *Monitor) Snapshot() (*types.Snapshot, error) {
	_, cur, err := m.Consecutive()
	return cur, err
}

// Consecutive returns the latest snapshot together with a baseline of the one
// collected immediately before it in the same run, so a caller can difference
// cumulative counters over exactly one collection interval -- which is how a
// node reports its swap paging rate in its fleet summary (#134).
//
// prev is not a whole snapshot. It carries the earlier reading's Timestamp,
// Host.BootTime and Memory only; every other field is zero. See baseline.
//
// prev is nil when the run has published only one snapshot so far: after a
// start, and after an interval change restarts the loop. A rate then cannot be
// measured, and the caller must say so rather than report zero. The errors are
// Snapshot's.
func (m *Monitor) Consecutive() (prev, cur *types.Snapshot, err error) {
	r := m.latest.Load()
	if r == nil {
		if m.Running() {
			return nil, nil, &types.NoSnapshotYetError{}
		}
		return nil, nil, &types.MonitorNotRunningError{}
	}
	return r.prev, r.cur, nil
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

// SetDecorator installs d, applied to every snapshot from now on, and applies
// it to the snapshot already published before returning, so that no reader is
// served an undecorated one afterwards. Safe to call at any time.
//
// A monitor carries one decorator. Installing one while a different one is
// installed returns DecoratorInstalledError rather than replacing it; installing
// the one already there changes nothing. Passing nil removes the decorator, and
// nothing published after the call returns is decorated.
func (m *Monitor) SetDecorator(d Decorator) error {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	if d == nil {
		m.decorator = nil
		return nil
	}
	if m.decorator != nil {
		if m.decorator == d {
			return nil
		}
		return &types.DecoratorInstalledError{}
	}
	m.decorator = d
	m.redecorateLocked()
	return nil
}

// Decorator returns the installed decorator, or nil.
func (m *Monitor) Decorator() Decorator {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	return m.decorator
}

// Restamp re-applies the decorator's Identify to the published snapshot before
// returning and, while running, broadcasts the result. Call it when the
// configuration Identify reads has changed -- a renamed node -- so every reader
// sees the new identity now rather than at the next collection, which may be a
// minute away or, while collection is hung, never. Alerts are left as
// evaluated, and the reading itself, timestamp included, is unchanged: a
// consumer keeping history keys it by timestamp and adds no point.
func (m *Monitor) Restamp() {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	m.redecorateLocked()
}

// redecorateLocked republishes a decorated copy of the published snapshot. The
// published one is shared with every reader and is never written to. The
// caller holds pubMu.
//
// The copy replaces the published snapshot only if that is still the one it
// was made from. Start clears the published snapshot without taking pubMu,
// and a copy of the previous run's last reading must not be put back after it.
//
// Whether a run is live is read under pubMu, which Stop holds while it closes
// subscribers and Start holds while it resets for a new run: a broadcast here
// either lands before that sweep or sees the run over, and never carries one
// run's reading to the next run's subscribers.
func (m *Monitor) redecorateLocked() {
	old := m.latest.Load()
	if old == nil || m.decorator == nil {
		// Nothing to re-apply. The published snapshot, and the record of
		// which decorator evaluated it, stay as they are.
		return
	}
	snap := *old.cur
	evaluated := m.decorate(&snap, false)
	if !evaluated && reflect.DeepEqual(*old.cur, snap) {
		// Nothing Identify reads has changed -- most reloads touch something
		// else -- so there is no new state to publish, and a stream is not
		// sent a frame that says nothing.
		return
	}
	// The same reading, re-identified: its baseline is unchanged.
	if !m.latest.CompareAndSwap(old, &reading{prev: old.prev, cur: &snap}) {
		return
	}
	if m.live {
		m.broadcast(&snap)
	}
}

// decorate applies the installed decorator to snap and normalises it. fresh
// says snap is a new collection rather than the published reading again. The
// decorator evaluates a reading once: republished, it gets back the alerts it
// produced, and it evaluates only if it never has -- which it reports. The
// caller holds pubMu.
func (m *Monitor) decorate(snap *types.Snapshot, fresh bool) (evaluated bool) {
	if fresh {
		m.evaluations = m.evaluations[:0]
	}
	d := m.decorator
	if d != nil {
		d.Identify(snap)
		i := slices.IndexFunc(m.evaluations, func(e evaluation) bool { return e.by == d })
		if i >= 0 {
			snap.Alerts = m.evaluations[i].alerts
		} else {
			d.Evaluate(snap)
			m.evaluations = append(m.evaluations, evaluation{by: d, alerts: snap.Alerts})
			evaluated = true
		}
	}
	// Normalised here, before it is published, because this is the one door
	// every served snapshot passes through: whatever the Snapshotter is, and
	// whatever the decorator attached (the alerts are). Once stored it is
	// shared, and every later pass over it only reads (#151).
	types.ReplaceNilSlices(snap)
	return evaluated
}

// publish decorates a fresh collection, makes it the published snapshot --
// paired with prev, the baseline of the run's previous one -- and broadcasts
// it.
func (m *Monitor) publish(prev, snap *types.Snapshot) {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	m.decorate(snap, true)
	m.latest.Store(&reading{prev: prev, cur: snap})
	m.broadcast(snap)
}

// run is the internal poll loop executed in a dedicated goroutine.
func (m *Monitor) run(ctx context.Context, stopped chan struct{}) {
	defer close(stopped)

	ticker := time.NewTicker(m.Interval())
	defer ticker.Stop()

	// prev is this run's previous reading, as a baseline. Held here rather
	// than read back from latest so it can only ever come from this run:
	// Start clears latest, but a local cannot outlive the goroutine that owns
	// it.
	var prev *types.Snapshot

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
			// Decorated as a copy: the Snapshotter's result is its own, and
			// one that hands back a value it keeps must not find it written.
			published := *snap
			// A reading is identified by when it was taken -- clients key
			// their history on it, and a republished reading keeps it -- so
			// one a Snapshotter left undated is dated here, as collected.
			if published.Timestamp.IsZero() {
				published.Timestamp = time.Now()
			}
			m.publish(prev, &published)
			prev = baseline(&published)
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
