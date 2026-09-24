package monitor_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// countingDecorator names the node with whatever name holds and records how
// many times it has evaluated.
type countingDecorator struct {
	mu          sync.Mutex
	name        string
	evaluations int
}

func (d *countingDecorator) setName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.name = name
}

func (d *countingDecorator) evaluated() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evaluations
}

func (d *countingDecorator) Identify(snap *types.Snapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	snap.NodeName = d.name
}

func (d *countingDecorator) Evaluate(snap *types.Snapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evaluations++
	snap.Alerts = []types.Alert{{ID: "rule", Message: d.name}}
}

// awaitPublished waits until the published snapshot satisfies ok.
func awaitPublished(t *testing.T, m *monitor.Monitor, ok func(*types.Snapshot) bool) *types.Snapshot {
	t.Helper()
	// Subscribed before the first look, so a publication landing between the
	// look and the wait still wakes it: every publication is broadcast.
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	for {
		if snap, err := m.Snapshot(); err == nil && ok(snap) {
			return snap
		}
		await.Recv(t, "a publication reaching the expected state", sub.Ch)
	}
}

// runningMonitor starts a monitor over stub at interval and stops it at the
// end of the test.
func runningMonitor(t *testing.T, stub monitor.Snapshotter, interval time.Duration) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(stub, interval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return m
}

// TestDecorator_DecoratesEveryCollection: a decorator installed before the
// first collection sees every snapshot, identity and alerts both, and the
// Snapshotter's own value is never written to.
func TestDecorator_DecoratesEveryCollection(t *testing.T) {
	stub := newStub("host")
	d := &countingDecorator{name: "db-01"}
	m, err := monitor.New(stub, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, d)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	snap := awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })
	if len(snap.Alerts) != 1 {
		t.Errorf("alerts = %v, want the one Evaluate attached", snap.Alerts)
	}
	if stub.snapshot.NodeName != "" || stub.snapshot.Alerts != nil {
		t.Errorf("the Snapshotter's own snapshot was written: %+v", stub.snapshot)
	}
}

// TestDecorator_InstalledLateDecoratesThePublishedSnapshot: a server built
// after its monitor started must not leave the snapshot already published
// undecorated until the next tick -- an hour away here.
func TestDecorator_InstalledLateDecoratesThePublishedSnapshot(t *testing.T) {
	m := runningMonitor(t, newStub("host"), 5*time.Millisecond)
	awaitPublished(t, m, func(*types.Snapshot) bool { return true })
	if err := m.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	sub := m.Subscribe()
	t.Cleanup(func() { m.Unsubscribe(sub.ID) })

	d := &countingDecorator{name: "db-01"}
	mustDecorate(t, m, d)

	// Synchronous: a server built on this monitor serves nothing undecorated
	// from the moment its constructor returns.
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.NodeName != "db-01" {
		t.Fatalf("NodeName = %q straight after SetDecorator, want db-01", snap.NodeName)
	}
	if len(snap.Alerts) != 1 {
		t.Errorf("alerts = %v, want the published snapshot evaluated too", snap.Alerts)
	}
	// Subscribers are told, so a stream does not keep the undecorated one.
	for {
		if got := await.Recv(t, "a decorated snapshot to be broadcast", sub.Ch); got.NodeName == "db-01" {
			return
		}
	}
}

// TestRestamp_RenamesWithoutReevaluating: a configuration change reaches the
// published snapshot at once, and the evaluator -- which keeps state between
// readings -- is never handed a reading twice: it evaluates exactly once per
// collection, however many restamps happen in between.
func TestRestamp_RenamesWithoutReevaluating(t *testing.T) {
	stub := newStub("host")
	d := &countingDecorator{name: "db-01"}
	m, err := monitor.New(stub, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, d)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })
	if err := m.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}

	for _, name := range []string{"database", "db-02", "database"} {
		d.setName(name)
		m.Restamp()
		// Synchronous: the rename is published by the time Restamp returns.
		snap, err := m.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap.NodeName != name {
			t.Fatalf("NodeName = %q straight after Restamp, want %q", snap.NodeName, name)
		}
	}

	// Stopping waits for the loop, so the two counts below are final.
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if evals, calls := d.evaluated(), stub.callCount(); evals != calls {
		t.Errorf("evaluations = %d for %d collections; a restamp must not evaluate", evals, calls)
	}
}

// TestRestamp_OnAStoppedMonitorAppliesDirectly: a stopped monitor still serves
// its last reading, so a rename must reach that too, with no loop to do it.
func TestRestamp_OnAStoppedMonitorAppliesDirectly(t *testing.T) {
	d := &countingDecorator{name: "db-01"}
	m, err := monitor.New(newStub("host"), 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, d)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	d.setName("database")
	m.Restamp()

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.NodeName != "database" {
		t.Errorf("NodeName = %q, want database applied without a running loop", snap.NodeName)
	}
}

// TestRestamp_WithNothingPublishedDoesNothing is the bad-input case: before
// any collection there is no snapshot to restamp, and none is invented.
func TestRestamp_WithNothingPublishedDoesNothing(t *testing.T) {
	m, err := monitor.New(newStub("host"), time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, &countingDecorator{name: "db-01"})
	m.Restamp()
	if snap, err := m.Snapshot(); err == nil {
		t.Errorf("Snapshot() = %+v, want no snapshot", snap)
	}
}

// TestSetDecorator_NilRemovesIt: with the decorator removed, snapshots are
// published as collected.
func TestSetDecorator_NilRemovesIt(t *testing.T) {
	stub := snapshotterFunc(func() (*types.Snapshot, error) {
		return &types.Snapshot{Host: types.HostInfo{Hostname: "host"}}, nil
	})
	m, err := monitor.New(stub, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, &countingDecorator{name: "db-01"})
	mustDecorate(t, m, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	snap := awaitPublished(t, m, func(*types.Snapshot) bool { return true })
	if snap.NodeName != "" || len(snap.Alerts) != 0 {
		t.Errorf("snapshot decorated after the decorator was removed: %+v", snap)
	}
}

// TestRestamp_LandsWhileCollectionIsHung: a collector stuck in a syscall must
// not hold a rename back -- the published snapshot is restamped at once.
func TestRestamp_LandsWhileCollectionIsHung(t *testing.T) {
	m := hungMonitor(t)
	d := &countingDecorator{name: "db-01"}
	mustDecorate(t, m, d)
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })

	d.setName("database")
	m.Restamp()

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.NodeName != "database" {
		t.Errorf("NodeName = %q with collection hung, want database", snap.NodeName)
	}
}

// mustDecorate installs d, failing the test if the monitor refuses it.
func mustDecorate(t *testing.T, m *monitor.Monitor, d monitor.Decorator) {
	t.Helper()
	if err := m.SetDecorator(d); err != nil {
		t.Fatalf("SetDecorator: %v", err)
	}
}

// TestSetDecorator_ASecondDecoratorIsRefused: one monitor describes one node,
// so a different decorator does not replace the installed one -- it is
// refused, and the snapshot keeps the first's decoration. Installing the same
// one again, or removing it, is fine.
func TestSetDecorator_ASecondDecoratorIsRefused(t *testing.T) {
	m := runningMonitor(t, newStub("host"), 5*time.Millisecond)
	first := &countingDecorator{name: "db-01"}
	mustDecorate(t, m, first)
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })

	var installed *types.DecoratorInstalledError
	if err := m.SetDecorator(&countingDecorator{name: "other"}); !errors.As(err, &installed) {
		t.Fatalf("SetDecorator(second) = %v, want DecoratorInstalledError", err)
	}
	if got := m.Decorator(); got != monitor.Decorator(first) {
		t.Errorf("Decorator() = %v, want the first still installed", got)
	}
	if err := m.SetDecorator(first); err != nil {
		t.Errorf("SetDecorator(same) = %v, want nil", err)
	}
	mustDecorate(t, m, nil)
	if got := m.Decorator(); got != nil {
		t.Errorf("Decorator() = %v after removal, want nil", got)
	}
	mustDecorate(t, m, &countingDecorator{name: "other"})
}

// snapshotterFunc adapts a function to monitor.Snapshotter.
type snapshotterFunc func() (*types.Snapshot, error)

func (f snapshotterFunc) Snapshot() (*types.Snapshot, error) { return f() }

// TestRestamp_UnchangedIdentityPublishesNothing: most reloads change nothing
// Identify reads. Those must not republish -- a stream would be sent a frame
// that repeats the reading and says nothing new. The loop is paced by hand, so
// nothing else can publish while this looks.
//
// The subscriber is taken before anything decorated is published and waits
// for that publication to arrive, so it can have nothing left to be sent but
// what Restamp broadcasts. Taking it after the published snapshot is seen is
// not enough: a publication is visible to Snapshot before its broadcast has
// reached every subscriber, and a subscription taken in between is sent it --
// which this test then blamed on Restamp (#214). The publication is held in
// exactly that window, so the ordering is exercised on every run.
func TestRestamp_UnchangedIdentityPublishesNothing(t *testing.T) {
	m := newMonitor(t, newStub("host"), time.Hour)
	ticker := useManualTicker(m)
	hold := holdFanOut(m)
	startMonitor(t, m)
	sub := m.Subscribe()
	t.Cleanup(func() { m.Unsubscribe(sub.ID) })
	// Nothing is published yet, so installing it publishes nothing either,
	// and the hold catches the first collection.
	mustDecorate(t, m, &countingDecorator{name: "db-01"})
	hold.arm(t)
	ticker.tick(t)
	hold.reached(t)
	before, err := m.Snapshot()
	if err != nil || before.NodeName != "db-01" {
		t.Fatalf("Snapshot() = %+v, %v while its broadcast waits; want the decorated reading", before, err)
	}
	hold.release(t)
	if got := await.Recv(t, "the decorated reading to be broadcast", sub.Ch); got != before {
		t.Fatalf("the subscriber was sent %p, want the published %p", got, before)
	}

	m.Restamp()

	if after, err := m.Snapshot(); err != nil || after != before {
		t.Errorf("Snapshot() = %p, %v; want the same %p, not a republication", after, err, before)
	}
	select {
	case got := <-sub.Ch:
		t.Errorf("an unchanged identity was broadcast: %+v", got)
	default:
	}
}

// hungMonitor runs a monitor whose collector answers once and then blocks
// until the test ends.
func hungMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	release := make(chan struct{})
	var calls atomic.Int64
	stub := snapshotterFunc(func() (*types.Snapshot, error) {
		if calls.Add(1) > 1 {
			<-release
		}
		return &types.Snapshot{Host: types.HostInfo{Hostname: "host"}}, nil
	})
	m, err := monitor.New(stub, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		if err := m.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return m
}

// TestSetDecorator_RemovedWhileRunningStopsDecorating: removal takes effect for
// everything published after it returns.
func TestSetDecorator_RemovedWhileRunningStopsDecorating(t *testing.T) {
	var calls atomic.Int64
	stub := snapshotterFunc(func() (*types.Snapshot, error) {
		n := calls.Add(1)
		return &types.Snapshot{Host: types.HostInfo{Hostname: "host", Uptime: uint64(n)}}, nil
	})
	m := runningMonitor(t, stub, 2*time.Millisecond)
	mustDecorate(t, m, &countingDecorator{name: "db-01"})
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })

	mustDecorate(t, m, nil)
	removedAt := uint64(calls.Load())

	snap := awaitPublished(t, m, func(s *types.Snapshot) bool { return s.Host.Uptime > removedAt })
	if snap.NodeName != "" || len(snap.Alerts) != 0 {
		t.Errorf("a collection after removal was decorated: %+v", snap)
	}
}

// TestRestamp_NothingIsBroadcastAfterStopReturns: a restamp racing Stop either
// broadcasts before Stop has closed the subscribers or not at all -- never
// after Stop returns, which is the guarantee Stop gives its consumers.
func TestRestamp_NothingIsBroadcastAfterStopReturns(t *testing.T) {
	d := &countingDecorator{name: "a"}
	m, err := monitor.New(newStub("host"), time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, d)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SetInterval(time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "a" })
	sub := m.Subscribe()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			d.setName([]string{"a", "b"}[i%2])
			m.Restamp()
		}
	}()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Whatever landed before Stop returned is allowed; drain it.
	for drained := false; !drained; {
		select {
		case <-sub.Ch:
		default:
			drained = true
		}
	}
	// Restamps keep running against the stopped monitor for a while.
	for range 200 {
		m.Restamp()
	}
	close(stop)
	<-done

	select {
	case got := <-sub.Ch:
		t.Errorf("broadcast after Stop returned: %+v", got)
	default:
	}
}

// TestPublish_DatesAnUndatedReading: clients key history on a reading's
// timestamp, so a Snapshotter that leaves it unset must not publish every
// reading under the same zero time.
func TestPublish_DatesAnUndatedReading(t *testing.T) {
	m := runningMonitor(t, newStub("host"), 5*time.Millisecond)
	snap := awaitPublished(t, m, func(*types.Snapshot) bool { return true })
	if snap.Timestamp.IsZero() {
		t.Error("an undated reading was published with the zero time")
	}
}

// TestPublish_KeepsTheCollectorsDate: a reading the collector dated keeps its
// own time; the monitor only fills an absence.
func TestPublish_KeepsTheCollectorsDate(t *testing.T) {
	taken := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	stub := snapshotterFunc(func() (*types.Snapshot, error) {
		return &types.Snapshot{Timestamp: taken}, nil
	})
	m := runningMonitor(t, stub, 5*time.Millisecond)
	snap := awaitPublished(t, m, func(*types.Snapshot) bool { return true })
	if !snap.Timestamp.Equal(taken) {
		t.Errorf("Timestamp = %v, want the collector's %v", snap.Timestamp, taken)
	}
}

// TestRestamp_NeverCarriesAReadingIntoTheNextRun: a restamp racing a restart
// must not hand the previous run's last reading to the new run's subscribers.
// Readings are numbered, so one from before the restart is recognisable.
func TestRestamp_NeverCarriesAReadingIntoTheNextRun(t *testing.T) {
	var reading atomic.Uint64
	stub := snapshotterFunc(func() (*types.Snapshot, error) {
		return &types.Snapshot{Host: types.HostInfo{Uptime: reading.Add(1)}}, nil
	})
	d := &countingDecorator{name: "a"}
	m, err := monitor.New(stub, time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustDecorate(t, m, d)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			d.setName([]string{"a", "b"}[i%2])
			m.Restamp()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	for range 50 {
		if err := m.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sub := m.Subscribe()
		awaitPublished(t, m, func(*types.Snapshot) bool { return true })
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		lastOfRun := reading.Load()

		if err := m.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		next := m.Subscribe()
		awaitPublished(t, m, func(s *types.Snapshot) bool { return s.Host.Uptime > lastOfRun })
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		for drained := false; !drained; {
			select {
			case got := <-next.Ch:
				if got.Host.Uptime <= lastOfRun {
					t.Fatalf("reading %d from the previous run reached a subscriber of the next",
						got.Host.Uptime)
				}
			default:
				drained = true
			}
		}
		m.Unsubscribe(sub.ID)
		m.Unsubscribe(next.ID)
	}
}

// TestSetDecorator_ReinstallingDoesNotEvaluateAgain: a decorator removed and
// installed again is the same evaluator, and has already seen the published
// reading; evaluating it twice would feed it a zero-length interval.
func TestSetDecorator_ReinstallingDoesNotEvaluateAgain(t *testing.T) {
	m := hungMonitor(t)
	d := &countingDecorator{name: "db-01"}
	mustDecorate(t, m, d)
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })
	before := d.evaluated()

	mustDecorate(t, m, nil)
	mustDecorate(t, m, d)

	if got := d.evaluated(); got != before {
		t.Errorf("evaluations = %d after reinstalling, want %d", got, before)
	}
}

// TestRestamp_WithNoDecoratorKeepsTheEvaluationRecord: a restamp while no
// decorator is installed has nothing to apply, and must not forget which
// decorator evaluated the published reading -- or reinstalling that one would
// evaluate the reading a second time.
func TestRestamp_WithNoDecoratorKeepsTheEvaluationRecord(t *testing.T) {
	m := hungMonitor(t)
	d := &countingDecorator{name: "db-01"}
	mustDecorate(t, m, d)
	published := awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "db-01" })
	before := d.evaluated()

	mustDecorate(t, m, nil)
	m.Restamp()
	if got, err := m.Snapshot(); err != nil || got != published {
		t.Errorf("Snapshot() = %p, %v after a restamp with no decorator; want %p unchanged", got, err, published)
	}
	mustDecorate(t, m, d)

	if got := d.evaluated(); got != before {
		t.Errorf("evaluations = %d, want %d -- the reading was evaluated again", got, before)
	}
}

// TestSetDecorator_SwappingBackReusesTheFirstEvaluation: removing d1,
// installing d2, and swapping back to d1 before the next collection must not
// evaluate the reading with d1 a second time -- and the snapshot must carry
// d1's alerts again, not d2's.
func TestSetDecorator_SwappingBackReusesTheFirstEvaluation(t *testing.T) {
	m := hungMonitor(t)
	d1 := &countingDecorator{name: "one"}
	d2 := &countingDecorator{name: "two"}
	mustDecorate(t, m, d1)
	awaitPublished(t, m, func(s *types.Snapshot) bool { return s.NodeName == "one" })
	before := d1.evaluated()

	mustDecorate(t, m, nil)
	mustDecorate(t, m, d2)
	mustDecorate(t, m, nil)
	mustDecorate(t, m, d1)

	if got := d1.evaluated(); got != before {
		t.Errorf("d1 evaluations = %d, want %d -- it evaluated the same reading again", got, before)
	}
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.NodeName != "one" || len(snap.Alerts) != 1 || snap.Alerts[0].Message != "one" {
		t.Errorf("snapshot = name %q alerts %v; want d1's name and d1's alerts", snap.NodeName, snap.Alerts)
	}
}
