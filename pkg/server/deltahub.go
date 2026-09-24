package server

import (
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// resyncEvery is how many frames may pass before every client is sent a whole
// snapshot regardless of whether it needed one.
//
// A client that has applied every delta is, in principle, exactly in step with
// the server. Sending it a full snapshot anyway bounds how long any
// disagreement could last: a receiver whose copy has drifted for a reason
// nobody predicted is corrected within a minute at the default poll rate,
// rather than continuing to render numbers that look measured for as long as
// the tab is open. It is a variable so tests can drive the path without waiting
// for sixty ticks.
var resyncEvery uint64 = 60

// maxSeq is the largest sequence number sent, chosen so it is still an exact
// integer in a browser.
//
// JSON has one number type and JavaScript parses it as a float64, so past
// 2^53-1 a sequence stops being exact: two consecutive frames could arrive
// carrying the same number, and a client comparing seq against last+1 would
// resync on every frame for ever. The counter therefore restarts, and the
// restart is sent as a whole snapshot -- a client that sees a sequence go
// backwards has no base for a patch. It cannot be reached in practice (a
// quarter-second tick would take seventy million years), which is precisely why
// it has to be handled here rather than discovered.
const maxSeq uint64 = 1<<53 - 1

// deltaFrame is one tick, encoded once for everyone.
//
// Both encodings are built here, not per client. That is the whole point of the
// hub: the old stream encoded the entire snapshot separately for every
// connection on every tick, which is O(clients x metrics) before the per-client
// gzip stream on top.
type deltaFrame struct {
	seq uint64
	// generation is the monitor run that produced this frame.
	//
	// The cache is served to a client that connects between ticks, so it
	// outlives the collection that filled it -- and a stop and a start make it
	// a reading from before the gap. Stamping it and comparing on the way out
	// ties the cache to the run rather than to the monitor's running flag,
	// which is true again the instant a restart completes.
	generation uint64
	// source is the monitor's published reading this frame encodes, compared
	// by identity with what the monitor publishes now.
	//
	// The hub learns of a publication only when its loop takes it off the
	// subscription, so for a moment the monitor can be ahead of the cache. A
	// restamp makes that visible: a rename lands in the monitor before Reload
	// returns, and a client connecting straight afterwards was handed the
	// cached frame with the old name while GET /api/snapshot already served
	// the new one. A cache whose source is not the current reading is
	// superseded, and a connecting client waits for the frame that replaces
	// it rather than being sent it.
	source *types.Snapshot
	// full is the complete snapshot envelope. Always present: any client can
	// need it at any tick.
	full []byte
	// patch is the change from seq-1, or nil when there was none to compute --
	// the first frame, a resync frame, or a tick whose diff failed.
	patch []byte
}

// frameFor decides what a client whose last applied frame was last should be
// sent next.
//
// A patch applies only to the frame immediately before it. Everything else -- a
// new client, a client that missed a frame, a resync frame that carries no
// patch -- gets the whole snapshot, so a client's copy is never built on a base
// the server did not send it. That is the single rule that keeps this stream
// from ever leaving a reading on screen that looks measured and is not, and it
// is separated out here so it can be checked on its own rather than only
// through a socket.
func frameFor(last uint64, frame *deltaFrame) (name string, payload []byte) {
	if frame.patch != nil && frame.seq == last+1 {
		return "delta", frame.patch
	}
	return "full", frame.full
}

// clientStream is one client's position in the stream: which frame it last
// applied, and which frame it was handed on connection.
//
// It exists so the rule "what does this client get next" lives in one place.
// Two transports carry this stream -- server-sent events to a browser and the
// Wails event bridge to the desktop window -- and a second copy of the
// sequencing would be a second set of decisions about when a client is sent a
// patch it cannot apply. The transports differ only in how bytes are written.
type clientStream struct {
	hub *deltaHub
	sub *deltaSubscriber
	// last is the sequence number this client has applied.
	last uint64
	// sent is the frame delivered on connection, kept so the copy that arrives
	// on the channel a moment later can be recognised and dropped. Comparing
	// sequence numbers instead would also drop a frame that legitimately
	// carries an older number -- which happens when the counter restarts -- and
	// the client would then be frozen on that snapshot indefinitely.
	sent *deltaFrame
}

// newClientStream subscribes a client and returns its position.
func newClientStream(hub *deltaHub) *clientStream {
	return &clientStream{hub: hub, sub: hub.subscribe()}
}

// close unsubscribes the client.
func (c *clientStream) close() { c.hub.unsubscribe(c.sub) }

// frames is the channel new frames arrive on.
func (c *clientStream) frames() <-chan *deltaFrame { return c.sub.frames }

// done closes when the monitor stops.
func (c *clientStream) done() <-chan struct{} { return c.sub.done }

// ended reports whether the subscription has closed.
func (c *clientStream) ended() bool { return closed(c.sub.done) }

// initial returns the frame a client is given on connection, so it is not
// blank until the next tick, or ok=false when there is none to give.
func (c *clientStream) initial() (name string, payload []byte, ok bool) {
	frame := c.hub.initialFrame(c.sub)
	if frame == nil || c.ended() {
		return "", nil, false
	}
	c.last, c.sent = frame.seq, frame
	return "full", frame.full, true
}

// next returns what to send for a frame that has just arrived, or skip=true
// when the client already has it.
func (c *clientStream) next(frame *deltaFrame) (name string, payload []byte, skip bool) {
	if frame == c.sent {
		return "", nil, true
	}
	name, payload = frameFor(c.last, frame)
	c.last = frame.seq
	return name, payload, false
}

// deltaSubscriber is one connected client's view of the hub.
type deltaSubscriber struct {
	frames chan *deltaFrame
	// done closes when the hub stops, which is how a client learns the monitor
	// has gone rather than waiting on a channel that will never deliver.
	done chan struct{}
	id   uint64
}

// deltaHub turns the monitor's snapshots into a sequence of frames and fans
// them out.
//
// It runs only while someone is streaming. A dashboard nobody has open costs
// nothing, and a server built for a test that never connects starts no
// goroutine -- which also means no lifecycle to remember to shut down.
type deltaHub struct {
	mon *monitor.Monitor
	log *slog.Logger

	// lifecycle guards the transition between running and not. Everything
	// inside the publish loop is owned by that one goroutine.
	lifecycle sync.Mutex
	running   bool
	stop      chan struct{}
	stopped   chan struct{}

	subscribers sync.Map // id -> *deltaSubscriber
	nextID      atomic.Uint64
	count       atomic.Int64

	// liveRuns counts publishing loops, and overlaps counts the times a second
	// one started while the first was still going.
	//
	// One loop at a time is the invariant the lifecycle lock exists for. Two
	// fanning out through the same subscriber map would hand a client a frame
	// from the outgoing one, after which every frame of the new one looks older
	// and is dropped -- a snapshot frozen on screen while the server goes on
	// measuring. It is counted rather than only asserted in a test because if
	// it ever happens in production that is exactly what an operator would be
	// looking at, with nothing else to explain it.
	liveRuns atomic.Int64
	overlaps atomic.Int64

	// seq belongs to the hub rather than to a run of it.
	//
	// A run that ends and starts again -- the last client left and another
	// arrived -- must not restart the count. A client attached across that
	// boundary would be holding a sequence from the old run, and every frame of
	// the new one would look older than what it already has.
	seq atomic.Uint64

	// ending is set by a publishing loop that has decided to stop, before it
	// closes anyone, and cleared when a replacement starts. It is what lets a
	// client arriving in that window be given a new loop rather than being
	// left attached to one that is on its way out; see ensureRunning.
	ending atomic.Bool

	latest atomic.Pointer[deltaFrame]
}

func newDeltaHub(mon *monitor.Monitor, log *slog.Logger) *deltaHub {
	if log == nil {
		log = slog.Default()
	}
	return &deltaHub{mon: mon, log: log}
}

// subscribe registers a client and makes sure a run is publishing.
//
// The check is made on every subscribe rather than only on the first, because a
// run can end without anyone unsubscribing: the monitor stopping ends it. A
// client arriving in that window would otherwise be counted as the second
// subscriber, skip the start, and wait on a hub that is not running.
func (h *deltaHub) subscribe() *deltaSubscriber {
	sub := &deltaSubscriber{
		// Buffered, so one slow client cannot stall the fan-out. A client that
		// overflows it misses a frame and is behind by sequence number, which
		// the protocol already handles by sending it a whole snapshot -- the
		// same path a new connection takes.
		frames: make(chan *deltaFrame, 4),
		done:   make(chan struct{}),
		id:     h.nextID.Add(1),
	}
	h.subscribers.Store(sub.id, sub)
	h.count.Add(1)
	h.ensureRunning()
	return sub
}

// unsubscribe removes a client, stopping the hub when it was the last.
func (h *deltaHub) unsubscribe(sub *deltaSubscriber) {
	if _, loaded := h.subscribers.LoadAndDelete(sub.id); !loaded {
		return
	}
	if h.count.Add(-1) == 0 {
		h.shutdown()
	}
}

// ensureRunning starts a publishing loop unless one is already live.
func (h *deltaHub) ensureRunning() {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()
	if h.running {
		select {
		case <-h.stopped:
			// The loop ended on its own, which happens when the monitor stops.
			// A client arriving afterwards needs a fresh one rather than a hub
			// that is marked running and is not.
			h.running = false
		default:
			if !h.ending.Load() {
				return
			}
			// A loop that has decided to end but has not finished saying so.
			//
			// This is the window a subscriber can be lost in. subscribe()
			// registers before calling this, and the ending loop closes the
			// subscribers it can see with sync.Map.Range -- which explicitly
			// does not snapshot, and is allowed to miss an entry added while
			// it runs. Without this branch such a client is left registered
			// with nobody publishing and nobody to close it: keepalives for as
			// long as the tab stays open, showing whatever was last on screen.
			//
			// Waited out rather than raced past. The loop has only a return
			// left to do, and starting a second one alongside it is the
			// overlap shutdown() exists to prevent.
			<-h.stopped
			h.running = false
		}
	}
	h.ending.Store(false)
	h.running = true
	h.stop = make(chan struct{})
	h.stopped = make(chan struct{})
	// The previous tree starts empty, so the first frame of a new run carries
	// no patch and every client takes the whole snapshot. The sequence does NOT
	// start again: it belongs to the hub, so a client attached across a restart
	// never sees it go backwards.
	go h.run(h.stop, h.stopped)
}

// shutdown stops the publishing loop and waits for it to finish.
//
// The lock is held across the wait, not released before it. Releasing early
// would let a client arriving at that moment start a second run while the first
// was still publishing: both would fan out through the same subscriber map, and
// the new client would take a frame from the outgoing run and then discard
// every frame of the new one as older than what it already had -- a snapshot
// frozen on screen for as long as the two overlapped.
func (h *deltaHub) shutdown() {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()
	if !h.running {
		return
	}
	h.running = false

	// Reached once per run: the flag above is cleared under the lock before
	// anything is closed. Closing a stop channel whose loop has already
	// returned is harmless.
	close(h.stop)
	<-h.stopped
	h.latest.Store(nil)
}

// run consumes snapshots and publishes frames until the hub is stopped or the
// monitor is.
func (h *deltaHub) run(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)

	if live := h.liveRuns.Add(1); live > 1 {
		h.overlaps.Add(1)
		h.log.Error("server: two delta publishing loops are running at once; "+
			"clients may see the sequence go backwards", "loops", live)
	}
	defer h.liveRuns.Add(-1)

	sub := h.mon.Subscribe()
	defer h.mon.Unsubscribe(sub.ID)

	// Checked AFTER subscribing, and that order is the whole point.
	//
	// A stopped monitor still answers Snapshot with the last thing it
	// collected, and still hands out subscriptions -- but Stop has already
	// walked the subscriber map, so this one's Done will never close and its
	// channel will never carry anything. Without this check a client
	// connecting after shutdown was sent that cached snapshot as an ordinary
	// full frame, marked current, and then received keepalives for as long as
	// it stayed: a reading from before the service stopped, presented as live,
	// forever. The hub restarts on demand, so "the monitor is gone" is a state
	// a fresh run has to handle rather than one it can assume away.
	//
	// After, because Stop clears its running flag before it walks the map: a
	// subscription taken before the walk gets its Done closed, and one taken
	// after sees running already false here. Checking first would leave the
	// window between the two.
	if !h.mon.Running() {
		h.endBecauseTheMonitorStopped()
		return
	}

	var previous delta.Tree

	// The snapshot already collected is published first, so a client that
	// connects between ticks is not blank until the next one. The generation
	// is read BEFORE the snapshot, so a restart in between can only make the
	// two disagree and cause the frame to be dropped -- never make a reading
	// from the old run carry the new run's number.
	gen := h.generation()
	if snap, err := h.mon.Snapshot(); err == nil && snap != nil {
		previous = h.publish(h.nextSeq(), snap, nil, gen)
	}

	for {
		// Read BEFORE waiting for a collection, not after receiving one.
		//
		// Reading it afterwards leaves a window: the receive completes, a stop
		// and a start finish, and the generation read then is the NEW run's --
		// which would stamp a measurement from the old one as current. Read
		// first, the comparison can only be conservative: a restart while this
		// is blocked makes the number stale, publish drops the frame, and
		// every client takes a whole snapshot on the next tick. A gap costs a
		// tick; the alternative costs the truth.
		gen := h.generation()

		select {
		case <-stop:
			return

		case <-sub.Done:
			// The monitor stopped. Closing every subscriber's done channel is
			// what turns that into a "bye" on each stream rather than a set of
			// connections hanging on a channel nothing will ever write to.
			h.endBecauseTheMonitorStopped()
			return

		case snap, ok := <-sub.Ch:
			if !ok {
				h.endBecauseTheMonitorStopped()
				return
			}
			// A closed subscription wins over a snapshot already queued on the
			// channel. Both cases can be ready at once and Go picks between
			// them at random, so without this a collection from before the
			// stop could be published -- and if the monitor has restarted by
			// then, stamped with the new generation and served as current.
			select {
			case <-sub.Done:
				h.endBecauseTheMonitorStopped()
				return
			default:
			}
			seq := h.nextSeq()
			base := previous
			if seq == 1 || seq%resyncEvery == 0 {
				// A resync frame carries no patch, so every client takes the
				// full path however up to date it is. Sequence 1 is the same
				// case: the counter has just wrapped, and nothing a client
				// holds is a base for it.
				base = nil
			}
			previous = h.publish(seq, snap, base, gen)
		}
	}
}

// endBecauseTheMonitorStopped ends a run that the monitor ended, dropping the
// cached frame before telling anyone.
//
// The order is the point. latest is what a connecting client is served before
// anything else arrives, and it was only ever cleared by shutdown() -- which
// runs when the LAST subscriber leaves. So while one client stayed attached
// after a stop, a second connecting alongside it read that pre-stop frame and
// was sent it as a full frame, marked current, a moment before its bye. A
// measurement from before the service stopped, presented as live, on a stream
// built to make exactly that impossible.
//
// Clearing before closing gives the handler something to rely on: the store
// happens before the close, so a handler that has seen its subscription closed
// cannot then load a frame from before the stop.
func (h *deltaHub) endBecauseTheMonitorStopped() {
	// Announced BEFORE the sweep, so a client registering alongside it is
	// either closed by the sweep or finds a hub that says a replacement is
	// needed. Announcing afterwards would leave the gap that loses it.
	h.ending.Store(true)
	h.latest.Store(nil)
	h.closeSubscribers()
}

// initialFrame returns the frame a just-connected subscriber should be sent
// first, and makes sure nothing older is left queued behind it.
//
// subscribe() starts queueing immediately, so between registering and reaching
// its select loop a handler can accumulate frames -- and the cache holds the
// NEWEST of them. Sending the cache and then draining the queue normally
// therefore delivered seq 12 and then seq 11, and the client renders a full
// frame whatever sequence it carries (it has to: a periodic resync arrives at
// an arbitrary one). So the dashboard stepped backwards to an older reading and
// presented it as current, noticing only at the next delta.
//
// The order below is the whole fix, and it works because fanOut stores latest
// BEFORE it queues: anything already in the channel is therefore at most as new
// as what the cache will return, so draining first can never discard something
// newer than the frame that replaces it. A frame published between the drain
// and the read is both queued and returned, which the caller's own
// already-sent check handles.
func (h *deltaHub) initialFrame(sub *deltaSubscriber) *deltaFrame {
	for {
		select {
		case <-sub.frames:
		default:
			return h.cachedFrame()
		}
	}
}

// cachedFrame returns the last published frame, or nil when there is nothing a
// connecting client should be given.
//
// "Nothing" includes a frame from before the monitor stopped, which is why this
// asks rather than reading the field: a snapshot that was current is not a
// snapshot that IS current, and the difference is invisible in the frame.
func (h *deltaHub) cachedFrame() *deltaFrame {
	if h.mon == nil || !h.mon.Running() {
		return nil
	}
	frame := h.latest.Load()
	// From an earlier collection. Running() alone is not enough: a Stop
	// followed by a Start makes it true again immediately, while the hub's own
	// publishing loop may not yet have noticed its subscription closed and
	// cleared the cache. The frame sitting there is then from before the gap --
	// for a service restarted after an hour, an hour old. Asking the monitor
	// which run it is on needs no such notification.
	if frame == nil || frame.generation != h.generation() {
		return nil
	}
	// Superseded: the monitor has published a newer reading -- a collection,
	// or a restamp -- that the loop has not turned into a frame yet. The
	// client is already subscribed, so that frame reaches it next, whole.
	if current, err := h.mon.Snapshot(); err != nil || frame.source != current {
		return nil
	}
	return frame
}

// generation is the monitor run frames are stamped with. A hub with no monitor
// publishes nothing, so the value is never compared against anything.
func (h *deltaHub) generation() uint64 {
	if h.mon == nil {
		return 0
	}
	return h.mon.Generation()
}

// publish encodes one frame and hands it to every subscriber, returning the
// tree to diff the next snapshot against.
//
// gen is the monitor run the snapshot was TAKEN FROM, captured by the caller
// before it read the snapshot rather than stamped here. The two are not the
// same number: between dequeuing a collection and publishing it, a stop and a
// start can complete, and stamping at this point would label a measurement
// from the previous run with the current generation -- which is precisely the
// mark that says it can be trusted. Carrying it in means a frame can only ever
// claim the run it came from, and one whose run has ended is dropped rather
// than sent.
func (h *deltaHub) publish(seq uint64, snapshot *types.Snapshot, base delta.Tree, gen uint64) delta.Tree {
	if gen != h.generation() {
		// The monitor restarted while this collection was in hand. The
		// sequence has already advanced, so every client is now behind and the
		// next frame reaches them as a whole snapshot -- a gap, which is always
		// preferable to a reading from before the restart.
		h.log.Warn("server: dropping a snapshot from a monitor run that has ended",
			"seq", seq, "collected_in", gen, "current", h.generation())
		return nil
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		// Nothing can be sent this tick. The sequence has already advanced, so
		// every client is now behind and the next frame reaches them as a whole
		// snapshot -- a gap, never a stale value presented as current.
		h.log.Warn("server: cannot encode a snapshot for the event stream", "error", err)
		return base
	}

	frame := &deltaFrame{seq: seq, generation: gen, source: snapshot, full: fullEnvelope(seq, raw)}

	current, err := delta.Decode(raw)
	if err != nil {
		h.log.Warn("server: cannot read back an encoded snapshot", "error", err)
		h.fanOut(frame)
		return nil
	}

	if base != nil {
		if frame.patch, err = patchEnvelope(seq, base, current); err != nil {
			// A diff that failed is not a reason to stop streaming, and it is
			// not a reason to send something approximate either: the frame goes
			// out with no patch, so everyone takes the full path.
			h.log.Warn("server: cannot diff a snapshot", "error", err)
			frame.patch = nil
		}
	}

	h.fanOut(frame)
	return current
}

// fanOut publishes a frame. The same bytes reach every client; a client whose
// buffer is full misses this frame and receives the next one as a whole
// snapshot, because its sequence will no longer be contiguous.
func (h *deltaHub) fanOut(frame *deltaFrame) {
	h.latest.Store(frame)
	h.subscribers.Range(func(_, value any) bool {
		sub := value.(*deltaSubscriber)
		select {
		case sub.frames <- frame:
		default:
		}
		return true
	})
}

// nextSeq advances the hub's sequence, restarting it before it stops being an
// exact integer in a browser. See maxSeq.
func (h *deltaHub) nextSeq() uint64 {
	for {
		current := h.seq.Load()
		next := current + 1
		if current >= maxSeq {
			next = 1
		}
		if h.seq.CompareAndSwap(current, next) {
			return next
		}
	}
}

func (h *deltaHub) closeSubscribers() {
	h.subscribers.Range(func(_, value any) bool {
		sub := value.(*deltaSubscriber)
		select {
		case <-sub.done:
		default:
			close(sub.done)
		}
		return true
	})
}

// fullEnvelope wraps an encoded snapshot with its sequence number.
//
// Assembled by hand from the already-encoded snapshot, so the snapshot is
// marshalled once per tick rather than once per client. Only the sequence
// number is formatted here, and it is an integer.
func fullEnvelope(seq uint64, snapshot []byte) []byte {
	out := make([]byte, 0, len(snapshot)+32)
	out = append(out, `{"seq":`...)
	out = appendUint(out, seq)
	out = append(out, `,"snapshot":`...)
	out = append(out, snapshot...)
	return append(out, '}')
}

// patchEnvelope diffs two trees and wraps the result.
func patchEnvelope(seq uint64, base, current delta.Tree) ([]byte, error) {
	patch, err := delta.Diff(base, current)
	if err != nil {
		return nil, err
	}
	// Nothing moved. The frame still goes out, with no patch in it: the client
	// needs the sequence number to stay in step, and it needs to see the server
	// is alive and measuring. An "empty patch" cannot be spelled -- a Patch with
	// no operation set encodes as {} and is rejected on arrival, correctly, as
	// a patch that names nothing.
	if patch == nil {
		out := append([]byte(`{"seq":`), nil...)
		out = appendUint(out, seq)
		return append(out, '}'), nil
	}

	raw, err := delta.Encode(patch)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, len(raw)+32)
	out = append(out, `{"seq":`...)
	out = appendUint(out, seq)
	out = append(out, `,"patch":`...)
	out = append(out, raw...)
	return append(out, '}'), nil
}

// appendUint writes a decimal integer without allocating.
func appendUint(dst []byte, v uint64) []byte {
	if v == 0 {
		return append(dst, '0')
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return append(dst, digits[i:]...)
}
