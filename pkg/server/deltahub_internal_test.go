package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestFrameFor is the rule the whole protocol rests on: a patch is sent only to
// a client that has the frame it was computed against.
//
// Every other case has to fall back to the whole snapshot. Getting any of them
// wrong is silent -- the client applies a patch to the wrong base and renders
// numbers that look measured and are not, which is the same failure class as
// the stale peer this dashboard already guards against.
func TestFrameFor(t *testing.T) {
	full := []byte(`{"seq":7,"snapshot":{}}`)
	patch := []byte(`{"seq":7,"patch":{}}`)

	tests := []struct {
		name  string
		last  uint64
		frame *deltaFrame
		want  string
	}{
		{
			name:  "exactly one behind takes the patch",
			last:  6,
			frame: &deltaFrame{seq: 7, full: full, patch: patch},
			want:  "delta",
		},
		{
			name:  "a new client has nothing to patch",
			last:  0,
			frame: &deltaFrame{seq: 7, full: full, patch: patch},
			want:  "full",
		},
		{
			name:  "a missed frame leaves a gap",
			last:  5,
			frame: &deltaFrame{seq: 7, full: full, patch: patch},
			want:  "full",
		},
		{
			name:  "several missed frames",
			last:  1,
			frame: &deltaFrame{seq: 7, full: full, patch: patch},
			want:  "full",
		},
		{
			name:  "a resync frame carries no patch",
			last:  6,
			frame: &deltaFrame{seq: 7, full: full},
			want:  "full",
		},
		{
			name: "a client somehow ahead of the server",
			// Cannot happen through the handler, which never advances past what
			// it was sent. If it ever did, sending a patch would be the worst
			// possible answer.
			last:  9,
			frame: &deltaFrame{seq: 7, full: full, patch: patch},
			want:  "full",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, payload := frameFor(tt.last, tt.frame)
			if name != tt.want {
				t.Fatalf("frameFor(%d) = %q, want %q", tt.last, name, tt.want)
			}
			want := tt.frame.full
			if tt.want == "delta" {
				want = tt.frame.patch
			}
			if !bytes.Equal(payload, want) {
				t.Fatalf("frameFor(%d) returned the wrong payload", tt.last)
			}
		})
	}
}

// tickingSnapshotter yields a snapshot whose uptime advances every call, so
// consecutive snapshots always differ by exactly one field.
type tickingSnapshotter struct {
	uptime uint64
}

func (s *tickingSnapshotter) Snapshot() (*types.Snapshot, error) {
	s.uptime++
	return &types.Snapshot{
		Host:  types.HostInfo{Hostname: "mon0", Uptime: s.uptime},
		CPUs:  []types.CPUInfo{{Index: 0, UsagePercent: float64(s.uptime), UsageAvailable: true}},
		Disks: []types.DiskInfo{{Name: "sda", UsedPercent: 40}},
	}, nil
}

func hubMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(&tickingSnapshotter{}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { stopHubMonitor(t, m) })
	return m
}

// stopHubMonitor stops m at the end of a test, tolerating a test that has
// already stopped it.
func stopHubMonitor(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	var notRunning *types.MonitorNotRunningError
	if err := m.Stop(); err != nil && !errors.As(err, &notRunning) {
		t.Errorf("monitor.Stop: %v", err)
	}
}

// onceSnapshotter collects once and then holds every later collection until
// the test ends. A hub over it publishes exactly one frame of its own, so any
// other frame a test sees is one it published by hand, and nothing the hub
// does can interleave with what the test is arranging.
type onceSnapshotter struct {
	calls   atomic.Int64
	parked  chan struct{} // closed when the second collection begins
	release chan struct{} // closed at the end of the test
}

func (s *onceSnapshotter) Snapshot() (*types.Snapshot, error) {
	n := s.calls.Add(1)
	if n == 2 {
		close(s.parked)
	}
	if n > 1 {
		<-s.release
	}
	return &types.Snapshot{Host: types.HostInfo{Hostname: "mon0", Uptime: 1}}, nil
}

// singleCollectionMonitor returns a running monitor that has published exactly
// one collection and is parked inside the next. The poll loop publishes before
// it collects again, so once the second collection has begun the first has
// been published and broadcast, and no further broadcast can happen.
func singleCollectionMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	s := &onceSnapshotter{parked: make(chan struct{}), release: make(chan struct{})}
	m, err := monitor.New(s, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		// Released first: Stop waits for the poll loop, which is parked.
		close(s.release)
		stopHubMonitor(t, m)
	})
	await.Recv(t, "the monitor to publish once and park in its next collection", s.parked)
	return m
}

// permitSnapshotter completes a collection only against a permit, so a test
// states exactly how many readings its monitor publishes and knows the
// published reading cannot change under it. openAll lets every collection
// through, which is how a test stops the monitor: Stop waits for a collection
// in progress.
type permitSnapshotter struct {
	mu      sync.Mutex
	changed *sync.Cond
	permits int
	open    bool
	uptime  uint64
}

func newPermitSnapshotter(permits int) *permitSnapshotter {
	s := &permitSnapshotter{permits: permits}
	s.changed = sync.NewCond(&s.mu)
	return s
}

func (s *permitSnapshotter) Snapshot() (*types.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.open && s.permits == 0 {
		s.changed.Wait()
	}
	if !s.open {
		s.permits--
	}
	s.uptime++
	return &types.Snapshot{Host: types.HostInfo{Hostname: "mon0", Uptime: s.uptime}}, nil
}

// openAll lets every collection through, now and from then on.
func (s *permitSnapshotter) openAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = true
	s.changed.Broadcast()
}

// gate closes the snapshotter again, allowing exactly permits more collections.
func (s *permitSnapshotter) gate(permits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open, s.permits = false, permits
	s.changed.Broadcast()
}

// permitMonitor starts a monitor over s and returns it once its first reading
// is published. s must allow at least one collection.
func permitMonitor(t *testing.T, s *permitSnapshotter) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(s, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		// Opened first: Stop waits for the poll loop, which may be held.
		s.openAll()
		stopHubMonitor(t, m)
	})
	await.Recv(t, "the monitor's first reading", sub.Ch)
	return m
}

// stopPermitMonitor stops m mid-test, letting through the collection the poll
// loop may be held in.
func stopPermitMonitor(t *testing.T, s *permitSnapshotter, m *monitor.Monitor) {
	t.Helper()
	s.openAll()
	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
}

// currentReading is the reading m publishes now.
func currentReading(t *testing.T, m *monitor.Monitor) *types.Snapshot {
	t.Helper()
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("monitor.Snapshot: %v", err)
	}
	return snap
}

func quietHubLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDeltaHub_RunsOnlyWhileWatched. The hub subscribes to the monitor and
// diffs every tick; a dashboard nobody has open must not pay for that, and a
// server built for a test that never streams must not leak a goroutine.
func TestDeltaHub_RunsOnlyWhileWatched(t *testing.T) {
	h := newDeltaHub(hubMonitor(t), quietHubLogger())

	if h.running {
		t.Fatal("the hub is running before anyone subscribed")
	}

	first := h.subscribe()
	waitForFrame(t, first)
	if !h.running {
		t.Fatal("the hub is not running with a subscriber attached")
	}

	second := h.subscribe()
	h.unsubscribe(first)
	if !h.running {
		t.Fatal("the hub stopped while a second client was still attached")
	}

	h.unsubscribe(second)
	if h.running {
		t.Fatal("the hub is still running with nobody attached")
	}
	if h.latest.Load() != nil {
		t.Error("the hub kept its last frame after stopping; a client " +
			"reconnecting later would be handed a snapshot from another era")
	}

	// And it comes back for the next client, rather than staying stopped.
	third := h.subscribe()
	defer h.unsubscribe(third)
	waitForFrame(t, third)
}

// TestDeltaHub_SameBytesForEveryClient is the structural saving.
//
// The stream this replaces encoded a whole snapshot separately for every
// connection on every tick. Diffing once and handing the same buffer to
// everyone is what makes the cost O(metrics) plus O(clients) writes; two
// clients receiving pointer-identical payloads is the observable form of that.
func TestDeltaHub_SameBytesForEveryClient(t *testing.T) {
	h := newDeltaHub(hubMonitor(t), quietHubLogger())

	a, b := h.subscribe(), h.subscribe()
	defer h.unsubscribe(a)
	defer h.unsubscribe(b)

	fa, fb := waitForFrame(t, a), waitForFrame(t, b)
	for fa.seq != fb.seq {
		if fa.seq < fb.seq {
			fa = waitForFrame(t, a)
		} else {
			fb = waitForFrame(t, b)
		}
	}
	if fa != fb {
		t.Fatal("two clients were given different frame objects for the same tick; " +
			"the snapshot was encoded more than once")
	}
}

// TestDeltaHub_ResyncsOnSchedule. A client that has applied every patch is in
// principle exactly in step, so nothing would ever correct a copy that had
// drifted for a reason nobody predicted. The periodic full snapshot bounds how
// long such a disagreement can last.
func TestDeltaHub_ResyncsOnSchedule(t *testing.T) {
	previous := resyncEvery
	resyncEvery = 3
	t.Cleanup(func() { resyncEvery = previous })

	h := newDeltaHub(hubMonitor(t), quietHubLogger())
	sub := h.subscribe()
	defer h.unsubscribe(sub)

	seen := make(map[uint64]bool)
	for i := 0; i < 20 && len(seen) < 8; i++ {
		frame := waitForFrame(t, sub)
		if seen[frame.seq] {
			continue
		}
		seen[frame.seq] = true
		if frame.seq%resyncEvery == 0 && frame.patch != nil {
			t.Fatalf("frame %d is a resync frame and still carries a patch", frame.seq)
		}
		if frame.seq%resyncEvery != 0 && frame.seq > 1 && frame.patch == nil {
			t.Fatalf("frame %d carries no patch and is not a resync frame", frame.seq)
		}
	}
	if len(seen) < 4 {
		t.Fatalf("only saw %d frames; the hub is not publishing", len(seen))
	}
}

// TestDeltaHub_PatchReconstructsTheSnapshot closes the loop on the server side:
// the patch a client is sent, applied to the snapshot it was sent before it,
// must give the snapshot the server actually has.
func TestDeltaHub_PatchReconstructsTheSnapshot(t *testing.T) {
	h := newDeltaHub(hubMonitor(t), quietHubLogger())
	sub := h.subscribe()
	defer h.unsubscribe(sub)

	base := waitForFrame(t, sub)
	var next *deltaFrame
	for i := 0; i < 40; i++ {
		frame := waitForFrame(t, sub)
		if frame.seq == base.seq+1 && frame.patch != nil {
			next = frame
			break
		}
		base = frame
	}
	if next == nil {
		t.Fatal("no consecutive pair of frames carried a patch")
	}

	var fullEnv struct {
		Seq      uint64          `json:"seq"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(base.full, &fullEnv); err != nil {
		t.Fatalf("decoding the full envelope: %v", err)
	}
	var nextEnv struct {
		Seq      uint64          `json:"seq"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(next.full, &nextEnv); err != nil {
		t.Fatalf("decoding the next envelope: %v", err)
	}

	got := applyEnvelope(t, fullEnv.Snapshot, next.patch)
	want, err := deltaDecode(nextEnv.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, got, want) {
		t.Fatal("applying the patch did not reproduce the snapshot the server holds")
	}
}

func waitForFrame(t *testing.T, sub *deltaSubscriber) *deltaFrame {
	t.Helper()
	bound := time.NewTimer(await.Timeout)
	defer bound.Stop()
	select {
	case frame := <-sub.frames:
		return frame
	case <-sub.done:
		t.Fatal("the hub stopped while waiting for a frame")
	case <-bound.C:
		t.Fatalf("no frame arrived within %v", await.Timeout)
	}
	return nil
}

// deltaDecode reads JSON the way the differ does.
func deltaDecode(raw []byte) (any, error) {
	return delta.Decode(raw)
}

// applyEnvelope applies a delta frame's patch to a snapshot.
func applyEnvelope(t *testing.T, snapshot, frame []byte) any {
	t.Helper()
	var env struct {
		Seq   uint64       `json:"seq"`
		Patch *delta.Patch `json:"patch"`
	}
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("decoding the delta envelope: %v", err)
	}
	base, err := delta.Decode(snapshot)
	if err != nil {
		t.Fatalf("decoding the base snapshot: %v", err)
	}
	got, err := delta.Apply(base, env.Patch)
	if err != nil {
		t.Fatalf("applying the patch: %v", err)
	}
	return got
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ra, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ra, rb)
}

// TestDeltaHub_SequenceSurvivesARestart.
//
// A run ends when the last client leaves and starts again when another
// arrives. If the sequence restarted with it, a client attached across that
// boundary would hold a number from the old run and see every frame of the new
// one as older than what it already had -- its handler would discard all of
// them and the snapshot on screen would freeze, indefinitely, while the server
// went on measuring.
func TestDeltaHub_SequenceSurvivesARestart(t *testing.T) {
	h := newDeltaHub(hubMonitor(t), quietHubLogger())

	first := h.subscribe()
	before := waitForFrame(t, first).seq
	h.unsubscribe(first)

	second := h.subscribe()
	defer h.unsubscribe(second)
	after := waitForFrame(t, second).seq

	if after <= before {
		t.Fatalf("the sequence went from %d to %d across a restart; it must not go backwards",
			before, after)
	}
}

// TestDeltaHub_ConcurrentChurn is the interleaving a single-threaded lifecycle
// test cannot reach: clients arriving while the last one is leaving.
//
// Two publishing loops fanning out through the same subscriber map would hand a
// new client a frame from the outgoing run, after which every frame of the new
// run looks older and is dropped -- a frozen dashboard. The invariant that
// catches it is that no subscriber ever sees the sequence go backwards.
func TestDeltaHub_ConcurrentChurn(t *testing.T) {
	h := newDeltaHub(hubMonitor(t), quietHubLogger())

	// Dense, deliberately. The window in which a second loop could start is the
	// few microseconds an outgoing one takes to return, so the transitions have
	// to be packed tightly enough to land in it: a subscriber that reads
	// nothing and leaves immediately makes almost every iteration a start or a
	// stop.
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				sub := h.subscribe()
				select {
				case frame := <-sub.frames:
					if frame.seq == 0 {
						t.Error("a frame carried sequence zero")
					}
				default:
				}
				h.unsubscribe(sub)
			}
		}()
	}
	wg.Wait()

	// A subscriber that stays for a while must never see the sequence go
	// backwards, whatever churned underneath it.
	watcher := h.subscribe()
	var last uint64
	for i := 0; i < 3; i++ {
		frame := waitForFrame(t, watcher)
		if frame.seq < last {
			t.Errorf("a subscriber saw the sequence go from %d back to %d", last, frame.seq)
		}
		last = frame.seq
	}
	h.unsubscribe(watcher)

	// The invariant the sequence check above can only catch by luck: exactly
	// one publishing loop, always. Two would fan out through the same
	// subscriber map, and the window in which they overlap is narrow enough
	// that no timing-based assertion finds it reliably.
	if overlaps := h.overlaps.Load(); overlaps != 0 {
		t.Errorf("%d publishing loops started while another was still running", overlaps)
	}

	// And it is left in a usable state rather than wedged.
	final := h.subscribe()
	defer h.unsubscribe(final)
	waitForFrame(t, final)
	if overlaps := h.overlaps.Load(); overlaps != 0 {
		t.Errorf("%d overlapping publishing loops", overlaps)
	}
}

// TestDeltaHub_TellsAClientArrivingAfterTheMonitorStopped.
//
// A run ends on its own when the monitor stops, without anyone unsubscribing.
// A client arriving in that window is not the first subscriber by count, so a
// hub that only started on the first would leave it waiting on a loop that no
// longer exists -- alive on keepalives, showing nothing, for as long as the tab
// stayed open. It therefore gets a fresh run.
//
// That run must not publish. A stopped monitor still answers Snapshot with the
// last thing it collected and still hands out subscriptions that nothing will
// ever write to or close, so the fresh run used to send that cached reading as
// an ordinary full frame, marked current, and then keepalives for ever: a
// measurement from before the service stopped, presented as live, with no way
// for the client to know. Being TOLD is the correct answer -- the stream ends,
// the dashboard reconnects on its backoff, and it shows the last reading
// labelled disconnected.
//
// This test asserted the opposite until the behaviour was fixed: "the next
// client still gets a run, and the snapshot the monitor last took." The
// intent in the comment was right and the assertion contradicted it.
func TestDeltaHub_TellsAClientArrivingAfterTheMonitorStopped(t *testing.T) {
	m := hubMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	// A subscriber that stays attached, so the count never returns to zero and
	// nothing calls shutdown.
	held := h.subscribe()
	defer h.unsubscribe(held)
	waitForFrame(t, held)

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
	await.Recv(t, "the held subscriber to be told the monitor stopped", held.done)

	next := h.subscribe()
	defer h.unsubscribe(next)

	bound := time.NewTimer(await.Timeout)
	defer bound.Stop()
	select {
	case <-next.done:
	case frame := <-next.frames:
		t.Fatalf("a client arriving after the monitor stopped was sent frame %d; "+
			"that is the last reading before shutdown, presented as current", frame.seq)
	case <-bound.C:
		t.Fatal("a client arriving after the monitor stopped was neither told nor served; " +
			"it would sit on keepalives for as long as the tab stayed open")
	}

	// And nothing arrives afterwards either: the run returned rather than
	// blocking on a subscription the monitor will never write to. Once the
	// loop has exited nothing is left that could publish, so the queue is
	// final and a non-blocking look at it is the whole answer.
	waitForHubLoop(t, h)
	select {
	case frame := <-next.frames:
		t.Fatalf("frame %d arrived after the stream was closed", frame.seq)
	default:
	}
}

// TestDeltaHub_DropsTheCachedFrameWhenTheMonitorStops.
//
// latest is what a connecting client is served before anything else arrives --
// deliberately, so a client arriving between ticks is not blank. It used to be
// cleared only by shutdown(), which runs when the LAST subscriber leaves. So
// while one client stayed attached after a stop, a second connecting alongside
// it was sent that pre-stop frame as a full frame, marked current, a moment
// before its bye: a measurement from before the service stopped, presented as
// live, on a stream built to make exactly that impossible.
func TestDeltaHub_DropsTheCachedFrameWhenTheMonitorStops(t *testing.T) {
	m := hubMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	held := h.subscribe()
	defer h.unsubscribe(held)
	waitForFrame(t, held)

	if h.latest.Load() == nil {
		t.Fatal("nothing was cached to begin with, so this proves nothing")
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
	await.Recv(t, "the held subscriber to be told the monitor stopped", held.done)

	// Checked without unsubscribing: the subscriber count is still one, so
	// shutdown() has not run and this is the hub's own doing.
	if frame := h.latest.Load(); frame != nil {
		t.Fatalf("frame %d is still cached after the monitor stopped; a client "+
			"connecting now would be served a reading from before the stop", frame.seq)
	}
}

// TestDeltaHub_CachedFrameIsNotServedByAStoppedMonitor is the other half, and
// it is not redundant: the two close the same hole from opposite ends. Clearing
// latest is a store the handler races; refusing to serve it while the monitor
// is not collecting needs no ordering at all.
func TestDeltaHub_CachedFrameIsNotServedByAStoppedMonitor(t *testing.T) {
	// One reading, so the cache cannot be superseded while this looks at it.
	s := newPermitSnapshotter(1)
	m := permitMonitor(t, s)
	h := newDeltaHub(m, quietHubLogger())

	held := h.subscribe()
	defer h.unsubscribe(held)
	waitForFrame(t, held)

	cached := h.latest.Load()
	if cached == nil {
		t.Fatal("nothing was cached to begin with, so this proves nothing")
	}
	if got := h.cachedFrame(); got != cached {
		t.Fatalf("cachedFrame() = %v while the monitor is collecting, want the cached frame", got)
	}

	stopPermitMonitor(t, s, m)

	// Put it back, standing in for the window between a run ending and the
	// store landing. Even then it must not be served.
	h.latest.Store(cached)
	if got := h.cachedFrame(); got != nil {
		t.Fatalf("cachedFrame() = frame %d from a monitor that is not collecting", got.seq)
	}
}

// TestDeltaHub_CachedFrameIsRefusedAfterTheMonitorRestarts.
//
// Running() is not enough on its own. A stop followed by a start makes it true
// again immediately, while the hub's publishing loop may not yet have noticed
// its subscription closed and cleared the cache -- so a client connecting in
// that window was served a frame from before the gap. For a service restarted
// after an hour that is an hour-old reading, rendered as current.
//
// Stamping the frame with the hub's own run counter did NOT fix this, which is
// worth recording: in that window the hub's run has not turned over either, so
// its counter still matched. The generation has to be the MONITOR's, which
// advances the instant the restart completes and needs no notification to
// reach anybody.
//
// The window is simulated rather than raced, because it is exactly the thing
// that cannot be scheduled: the frame is put back by hand, which is what the
// losing interleaving leaves behind.
func TestDeltaHub_CachedFrameIsRefusedAfterTheMonitorRestarts(t *testing.T) {
	s := newPermitSnapshotter(1)
	m := permitMonitor(t, s)
	h := newDeltaHub(m, quietHubLogger())

	sub := h.subscribe()
	defer h.unsubscribe(sub)
	waitForFrame(t, sub)
	stale := h.latest.Load()
	if stale == nil {
		t.Fatal("nothing was cached to begin with, so this proves nothing")
	}

	stopPermitMonitor(t, s, m)

	// Wait for the hub's own loop to finish reacting to that stop before
	// touching h.latest below.
	//
	// This is a fixture that raced the code it was testing, at about one run in
	// forty (#75). The loop notices its subscription closed and calls
	// endBecauseTheMonitorStopped, which does latest.Store(nil) -- so a frame
	// this test stored a moment earlier could be cleared between the Store and
	// the read, and cachedFrame returned nil for a reason that had nothing to
	// do with generations. Nothing in the product does this: only the hub's own
	// goroutine writes latest, and a client never stores a frame at all.
	waitForHubLoop(t, h)

	// The restarted run publishes exactly one reading, so the one it has is
	// the one it will keep while this looks.
	s.gate(1)
	restarted := m.Subscribe()
	defer m.Unsubscribe(restarted.ID)
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	await.Recv(t, "the restarted run's reading", restarted.Ch)
	if !m.Running() {
		t.Fatal("the monitor is not running after the restart, so this tests nothing")
	}

	// The window: the monitor is collecting again and the old frame is what a
	// handler would find if the hub's loop had not cleared it yet.
	h.latest.Store(stale)
	if got := h.cachedFrame(); got != nil {
		t.Fatalf("cachedFrame() = frame %d from generation %d while the monitor is on "+
			"generation %d; a client connecting now would render a reading from before "+
			"the restart", got.seq, got.generation, m.Generation())
	}

	// And a frame from the current generation is still served, so this refuses
	// the stale one rather than everything.
	fresh := &deltaFrame{seq: 99, generation: m.Generation(), source: currentReading(t, m), full: []byte("{}")}
	h.latest.Store(fresh)
	if got := h.cachedFrame(); got != fresh {
		t.Fatalf("cachedFrame() = %v for a frame from the current generation, want it served", got)
	}
}

// TestDeltaHub_CachedFrameIsRefusedOnceTheMonitorHasMovedOn. The hub learns of
// a publication only when its loop takes it off the subscription, so the
// monitor can be a reading ahead of the cache. A restamp makes that visible: a
// rename lands in the monitor before Reload returns, and a client connecting
// straight afterwards was sent the cached frame with the old name while GET
// /api/snapshot already served the new one. Such a frame is superseded, and a
// connecting client must wait for the one replacing it rather than be sent it.
//
// Constructed rather than raced: the window is the hub loop's reaction time,
// so the superseded cache is put there by hand.
func TestDeltaHub_CachedFrameIsRefusedOnceTheMonitorHasMovedOn(t *testing.T) {
	m := singleCollectionMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	current := &deltaFrame{seq: 7, generation: m.Generation(), source: currentReading(t, m), full: []byte("{}")}
	h.latest.Store(current)
	if got := h.cachedFrame(); got != current {
		t.Fatalf("cachedFrame() = %v for the frame of the monitor's current reading, want it served", got)
	}

	// Same run, but encoding a reading the monitor no longer publishes.
	superseded := &deltaFrame{seq: 6, generation: m.Generation(), source: &types.Snapshot{}, full: []byte("{}")}
	h.latest.Store(superseded)
	if got := h.cachedFrame(); got != nil {
		t.Fatalf("cachedFrame() = frame %d, which the monitor has already superseded; a client "+
			"connecting now would be sent an older reading than GET /api/snapshot serves", got.seq)
	}
}

// TestDeltaHub_DropsACollectionWhoseRunHasEnded.
//
// The last of this class, and the narrowest: the hub dequeues a collection,
// the monitor stops and starts, and only then is the frame published. Stamping
// at publish time would label a measurement from the previous run with the
// current generation -- which is exactly the mark that says it can be trusted.
//
// The generation is therefore captured with the collection and carried in, and
// publish drops a frame whose run has ended. A gap is always preferable to a
// reading from before a restart: the next frame reaches every client as a
// whole snapshot.
func TestDeltaHub_DropsACollectionWhoseRunHasEnded(t *testing.T) {
	m := hubMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	sub := h.subscribe()
	defer h.unsubscribe(sub)
	waitForFrame(t, sub)

	if h.latest.Load() == nil {
		t.Fatal("nothing was published to begin with, so this proves nothing")
	}

	// A collection held from the run that is about to end.
	held, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	stale := m.Generation()

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	if m.Generation() == stale {
		t.Fatal("the generation did not advance, so this tests nothing")
	}

	if got := h.publish(h.nextSeq(), held, nil, stale); got != nil {
		t.Error("publish returned a tree for a dropped frame; the next diff would " +
			"be against a snapshot nobody was sent")
	}
	// Asked the way a connecting client asks. Reading the field directly is a
	// stricter check than the property that matters and a racier one: the hub
	// is live, so whether its own loop has noticed the stop yet is a matter of
	// scheduling, while what it will SERVE is not.
	if now := h.cachedFrame(); now != nil && now.generation == stale {
		t.Fatalf("a connecting client would be served frame %d from generation %d, "+
			"which is a measurement from before the restart", now.seq, now.generation)
	}

	// A collection from the run that IS current still publishes, so this drops
	// the stale one rather than everything.
	fresh := m.Generation()
	if got := h.publish(h.nextSeq(), held, nil, fresh); got == nil {
		t.Fatal("publish dropped a frame from the current run")
	}
}

// TestNextSeq_RestartsBeforeItStopsBeingExact.
//
// A sequence past 2^53-1 is not an exact integer in a browser: two consecutive
// frames can arrive carrying the same number, and a client comparing seq
// against last+1 resyncs on every frame for ever. The counter restarts instead,
// and sequence 1 is published as a whole snapshot because nothing a client
// holds is a base for it.
func TestNextSeq_RestartsBeforeItStopsBeingExact(t *testing.T) {
	h := newDeltaHub(nil, quietHubLogger())

	if got := h.nextSeq(); got != 1 {
		t.Fatalf("the first sequence is %d, want 1", got)
	}

	h.seq.Store(maxSeq - 1)
	if got := h.nextSeq(); got != maxSeq {
		t.Fatalf("nextSeq() = %d, want %d", got, maxSeq)
	}
	if got := h.nextSeq(); got != 1 {
		t.Fatalf("nextSeq() after the ceiling = %d, want it to restart at 1", got)
	}

	// Every value it can produce is exact in a browser, which is the whole
	// point of the ceiling.
	if float64(maxSeq) != float64(maxSeq)+0 || uint64(float64(maxSeq)) != maxSeq {
		t.Fatalf("%d does not survive a float64", maxSeq)
	}
}

// TestStreamDeltas_ServesNoCachedFrameAfterTheMonitorStopped is the end-to-end
// shape of the two tests above: not "does the hub clear a field" but what a
// browser connecting after shutdown actually receives.
//
// It is an INTERNAL test because it has to hold a subscriber. The hub clears
// its cached frame in shutdown() too, which runs when the last subscriber
// leaves -- so an end-to-end test that lets the previous client go proves
// nothing: shutdown tidies up before the late client connects and the test
// passes against the bug. Measured, not assumed: written first in
// deltastream_test.go, it survived removing both halves of the fix.
//
// Holding a subscriber is the real situation as well. The bug needed one
// client still attached after the stop, which is what happens whenever more
// than one dashboard is open.
func TestStreamDeltas_ServesNoCachedFrameAfterTheMonitorStopped(t *testing.T) {
	m := hubMonitor(t)
	s := New(m, "", nil)
	srv := httptest.NewServer(s)
	defer srv.Close()

	// Attached for the whole test, so the count never reaches zero and
	// shutdown() never runs.
	held := s.deltas.subscribe()
	defer s.deltas.unsubscribe(held)
	waitForFrame(t, held)
	if s.deltas.latest.Load() == nil {
		t.Fatal("nothing was cached to begin with, so this proves nothing")
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
	await.Recv(t, "the held subscriber to be told the monitor stopped", held.done)

	// A browser opening the dashboard now.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/events?deltas=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dialling the late stream: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	}()

	var names []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		name, ok := strings.CutPrefix(line, "event: ")
		if !ok {
			continue
		}
		names = append(names, name)
		if name == "bye" {
			break
		}
	}
	if len(names) != 1 || names[0] != "bye" {
		t.Fatalf("the late stream sent %v; want [bye] and nothing before it -- a full "+
			"frame here is the snapshot from before the monitor stopped, rendered as current",
			names)
	}
}

// TestDeltaHub_ReplacesALoopThatIsOnItsWayOut.
//
// The window a subscriber can be lost in. subscribe() registers the client and
// then calls ensureRunning; a loop ending because the monitor stopped closes
// the subscribers it can see with sync.Map.Range, which explicitly does not
// snapshot and is allowed to miss an entry added while it runs. If the miss
// happens while the loop is still marked running -- it has not closed its
// stopped channel yet -- ensureRunning used to see a live hub and return, and
// that client was left registered with nobody publishing and nobody to close
// it: keepalives for as long as the tab stayed open, showing whatever was last
// on screen.
//
// The state is set up by hand rather than raced into, because the window is
// between two statements of a goroutine that cannot be paused from here. What
// is asserted is the decision ensureRunning makes in it.
func TestDeltaHub_ReplacesALoopThatIsOnItsWayOut(t *testing.T) {
	m := hubMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	// A loop that has announced it is ending and has not finished.
	h.running = true
	h.stop = make(chan struct{})
	stopped := make(chan struct{})
	h.stopped = stopped
	h.ending.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ensureRunning()
	}()

	// ensureRunning holds the lifecycle lock for its whole decision, so once
	// it has the lock it is past any check of stopped it could make before
	// the loop finishes -- and with stopped still open it cannot return.
	await.Until(t, "ensureRunning to take the lifecycle lock", func(context.Context) bool {
		if h.lifecycle.TryLock() {
			h.lifecycle.Unlock()
			return false
		}
		return true
	})
	select {
	case <-done:
		t.Fatal("ensureRunning returned while the ending loop had not finished; " +
			"it should wait it out")
	default:
	}

	// The loop finishes, as the real one does -- it has only a return left.
	close(stopped)
	await.Recv(t, "ensureRunning to return once the ending loop has finished", done)

	h.lifecycle.Lock()
	replaced := h.stopped != stopped
	running := h.running
	h.lifecycle.Unlock()

	if !running || !replaced {
		t.Fatal("ensureRunning left the hub attached to a loop that was on its way out; " +
			"a client registering in that window has nobody publishing and nobody to close it")
	}
	if h.ending.Load() {
		t.Error("the ending flag was not cleared by the replacement loop")
	}

	// And the replacement is a real one: it publishes.
	sub := h.subscribe()
	defer h.unsubscribe(sub)
	waitForFrame(t, sub)
}

// TestDeltaHub_InitialFrameLeavesNothingOlderQueued.
//
// subscribe() starts queueing immediately, so a handler can accumulate frames
// between registering and reaching its select loop -- and the cache holds the
// NEWEST of them. Sending the cache and then draining normally delivered seq 12
// and then seq 11, and a client renders a full frame whatever sequence it
// carries, because it has to: a periodic resync arrives at an arbitrary one.
// So the dashboard stepped BACKWARDS to an older reading and presented it as
// current, noticing only at the next delta.
//
// Constructed rather than raced: the window is between two statements of a
// request handler, so the queue is filled by hand with exactly what that window
// leaves behind.
func TestDeltaHub_InitialFrameLeavesNothingOlderQueued(t *testing.T) {
	m := singleCollectionMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	sub := h.subscribe()
	defer h.unsubscribe(sub)
	waitForFrame(t, sub)

	// Three ticks arrive while the handler is still getting started. fanOut
	// stores the cache before it queues, so the newest of them is the cache.
	var newest *deltaFrame
	for i := 0; i < 3; i++ {
		newest = &deltaFrame{seq: h.nextSeq(), generation: m.Generation(), source: currentReading(t, m), full: []byte("{}")}
		h.fanOut(newest)
	}

	got := h.initialFrame(sub)
	if got != newest {
		t.Fatalf("initialFrame() = %v, want the newest published frame (seq %d)", got, newest.seq)
	}

	// Nothing older may be left for the handler's loop to send afterwards. A
	// frame here would be delivered as a full and render as current.
	select {
	case queued := <-sub.frames:
		t.Fatalf("frame %d is still queued behind the initial frame %d; it would be "+
			"sent next and render an older reading as current", queued.seq, got.seq)
	default:
	}
}

// TestDeltaHub_InitialFrameKeepsWhatArrivesAfterIt. The drain must not swallow
// the stream: a frame published after the initial one is the next thing the
// client needs, and dropping it would cost a resync on every connection.
func TestDeltaHub_InitialFrameKeepsWhatArrivesAfterIt(t *testing.T) {
	m := singleCollectionMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	sub := h.subscribe()
	defer h.unsubscribe(sub)
	waitForFrame(t, sub)

	if got := h.initialFrame(sub); got == nil {
		t.Fatal("initialFrame() = nil while the hub is publishing")
	}

	next := &deltaFrame{seq: h.nextSeq(), generation: m.Generation(), source: currentReading(t, m), full: []byte("{}")}
	h.fanOut(next)

	// fanOut queues synchronously and nothing else publishes on this hub, so
	// the frame is either queued now or was swallowed.
	select {
	case queued := <-sub.frames:
		if queued != next {
			t.Fatalf("received frame %d, want the one published after the initial frame", queued.seq)
		}
	default:
		t.Fatal("the frame published after the initial one was not queued")
	}
}

// TestEvents_LegacyStreamAlsoSaysByeAfterTheMonitorStopped.
//
// The delta path was hardened against a stopped monitor and the legacy one was
// not, although both are branches of the same handler and both take a
// subscription that Stop has already walked past. A client on plain
// /api/events connecting after shutdown was sent the last pre-stop reading as
// an ordinary "snapshot" event and then keepalives for as long as it stayed.
//
// Internal for the same reason as its delta sibling: it has to hold a
// subscriber so the hub's own shutdown does not tidy up first. This one uses a
// monitor subscription rather than a hub one, since the legacy path does not
// go through the hub at all -- which is precisely why the hub's guard did not
// cover it.
func TestEvents_LegacyStreamAlsoSaysByeAfterTheMonitorStopped(t *testing.T) {
	m := hubMonitor(t)
	s := New(m, "", nil)
	srv := httptest.NewServer(s)
	defer srv.Close()

	// Wait for a collection, so there is something stale to serve.
	waitForFirstSnapshot(t, m)

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	// No ?deltas=1: the protocol that has been here longest.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dialling the legacy stream: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	}()

	// Read the first event rather than until "bye" arrives. The bug is that
	// "bye" never does -- the client sits on keepalives -- so waiting for it
	// makes the failure a timeout that names nothing. The first event is there
	// at once either way, and the assertion says what is wrong.
	first := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
				first <- name
				return
			}
		}
		first <- ""
	}()

	name := await.Recv(t, "the legacy stream's first event after the monitor stopped "+
		"(it should be told rather than left on keepalives)", first)
	if name != "bye" {
		t.Fatalf("the legacy stream's first event after the monitor stopped is %q; "+
			"want bye -- a snapshot here is the last reading before shutdown, "+
			"rendered as live", name)
	}
}

// waitForHubLoop blocks until the hub's publishing goroutine has exited, so a
// test can write h.latest without racing the loop's own cleanup.
func waitForHubLoop(t *testing.T, h *deltaHub) {
	t.Helper()

	h.lifecycle.Lock()
	stopped := h.stopped
	h.lifecycle.Unlock()
	if stopped == nil {
		return
	}

	await.Recv(t, "the hub's publishing loop to exit", stopped)
}

// TestDeltaHub_EndingClearsTheCache demonstrates, deterministically, the
// mechanism behind the flake in #75 -- so that the synchronisation in the test
// above is not removed later as ceremony.
//
// The flake was one run in forty and would not reproduce under a thousand
// iterations at every GOMAXPROCS once the machine was quiet, which is exactly
// why it is pinned here as an ordinary call rather than chased as a race: the
// window is real whether or not the scheduler opens it today.
//
// Note what this is NOT evidence of. Only the hub's own goroutine writes
// latest, and no client ever stores a frame, so nothing in the product can lose
// a frame this way. The race was between a fixture and the code it was testing.
func TestDeltaHub_EndingClearsTheCache(t *testing.T) {
	m := singleCollectionMonitor(t)
	h := newDeltaHub(m, quietHubLogger())

	sub := h.subscribe()
	defer h.unsubscribe(sub)
	waitForFrame(t, sub)

	frame := &deltaFrame{seq: 1, generation: m.Generation(), full: []byte("{}")}
	h.latest.Store(frame)
	if h.latest.Load() != frame {
		t.Fatal("the frame was not stored, so this proves nothing")
	}

	// What the publishing loop does when it notices the monitor has stopped.
	h.endBecauseTheMonitorStopped()

	if got := h.latest.Load(); got != nil {
		t.Errorf("latest = %v after the loop ended, want nil", got)
	}
}
