package prom

import (
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/golang/snappy"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorder is a remote-write receiver that counts requests, keeps every batch
// it accepted, and can be told to reject.
type recorder struct {
	mu       sync.Mutex
	requests int
	bodies   [][]byte
	status   atomic.Int32
	srv      *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.status.Store(http.StatusNoContent)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := int(r.status.Load())
		r.mu.Lock()
		r.requests++
		if status/100 == 2 {
			r.bodies = append(r.bodies, body)
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) counts() (requests, accepted int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests, len(r.bodies)
}

// acceptedSamples counts the samples in every batch the receiver kept, by
// decompressing what actually arrived rather than trusting the exporter's own
// bookkeeping.
func (r *recorder) acceptedSamples(t *testing.T) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, body := range r.bodies {
		decoded, err := snappy.Decode(nil, body)
		if err != nil {
			t.Fatalf("a delivered body was not snappy: %v", err)
		}
		n += countSamplesInWireFormat(t, decoded)
	}
	return n
}

// deliveredValues returns the value of every sample the receiver accepted.
func (r *recorder) deliveredValues(t *testing.T) []float64 {
	t.Helper()
	var out []float64
	for _, series := range r.deliveredSeries(t) {
		out = append(out, series.values...)
	}
	return out
}

// deliveredInstances returns the instance label of every series accepted.
func (r *recorder) deliveredInstances(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, series := range r.deliveredSeries(t) {
		out = append(out, series.instance)
	}
	return out
}

type wireSeries struct {
	instance string
	values   []float64
	times    []int64
}

// deliveredSeries decodes what the receiver actually got.
//
// It walks the bytes on the wire rather than re-using this package's encoder,
// so a bug that mislabelled or dropped samples on the way out is visible here
// instead of being cancelled out by the same bug on the way back.
func (r *recorder) deliveredSeries(t *testing.T) []wireSeries {
	t.Helper()
	r.mu.Lock()
	bodies := append([][]byte(nil), r.bodies...)
	r.mu.Unlock()

	var out []wireSeries
	for _, body := range bodies {
		decoded, err := snappy.Decode(nil, body)
		if err != nil {
			t.Fatalf("a delivered body was not snappy: %v", err)
		}
		forEachField(t, decoded, func(field int, series []byte) {
			if field != fieldTimeseries {
				return
			}
			got := wireSeries{}
			forEachField(t, series, func(inner int, body []byte) {
				switch inner {
				case fieldLabels:
					name, value := decodeLabel(t, body)
					if name == "instance" {
						got.instance = value
					}
				case fieldSamples:
					got.values = append(got.values, decodeSampleValue(body))
					got.times = append(got.times, decodeSampleTime(body))
				}
			})
			out = append(out, got)
		})
	}
	return out
}

// decodeLabel reads a Label message.
func decodeLabel(t *testing.T, buf []byte) (name, value string) {
	t.Helper()
	forEachField(t, buf, func(field int, body []byte) {
		switch field {
		case fieldLabelName:
			name = string(body)
		case fieldLabelValue:
			value = string(body)
		}
	})
	return name, value
}

// decodeSampleValue reads a Sample's double, which is fixed64 rather than
// length-delimited and so is walked here rather than by forEachField. An
// omitted value is the proto3 default, zero.
func decodeSampleValue(buf []byte) float64 {
	for len(buf) > 0 {
		key, n := uvarint(buf)
		if n <= 0 {
			return 0
		}
		buf = buf[n:]
		field, wire := int(key>>3), int(key&7)
		switch {
		case field == fieldSampleVal && wire == wireFixed64 && len(buf) >= 8:
			return math.Float64frombits(binary.LittleEndian.Uint64(buf))
		case wire == wireVarint:
			_, n := uvarint(buf)
			if n <= 0 {
				return 0
			}
			buf = buf[n:]
		case wire == wireFixed64 && len(buf) >= 8:
			buf = buf[8:]
		default:
			return 0
		}
	}
	return 0
}

// decodeSampleTime reads a Sample's timestamp, a varint. An omitted timestamp
// is the proto3 default, zero.
func decodeSampleTime(buf []byte) int64 {
	for len(buf) > 0 {
		key, n := uvarint(buf)
		if n <= 0 {
			return 0
		}
		buf = buf[n:]
		field, wire := int(key>>3), int(key&7)
		switch {
		case wire == wireVarint:
			v, n := uvarint(buf)
			if n <= 0 {
				return 0
			}
			if field == fieldSampleTime {
				return int64(v)
			}
			buf = buf[n:]
		case wire == wireFixed64 && len(buf) >= 8:
			buf = buf[8:]
		default:
			return 0
		}
	}
	return 0
}

// countSamplesInWireFormat counts Sample submessages in an encoded
// WriteRequest by walking the wire format.
//
// It reads the bytes the receiver got rather than re-using this package's
// encoder, so a bug that dropped samples during encoding would be caught here
// instead of being cancelled out.
func countSamplesInWireFormat(t *testing.T, buf []byte) int {
	t.Helper()
	total := 0
	forEachField(t, buf, func(field int, body []byte) {
		if field != fieldTimeseries {
			return
		}
		forEachField(t, body, func(inner int, _ []byte) {
			if inner == fieldSamples {
				total++
			}
		})
	})
	return total
}

// forEachField walks length-delimited protobuf fields. Only the two message
// levels above are ever walked, so nothing else needs handling.
func forEachField(t *testing.T, buf []byte, fn func(field int, body []byte)) {
	t.Helper()
	for len(buf) > 0 {
		key, n := uvarint(buf)
		if n <= 0 {
			t.Fatalf("malformed protobuf: bad field key")
		}
		buf = buf[n:]
		field, wire := int(key>>3), int(key&7)
		if wire != wireBytes {
			t.Fatalf("unexpected wire type %d for field %d", wire, field)
		}
		length, n := uvarint(buf)
		if n <= 0 || int(length) > len(buf)-n {
			t.Fatalf("malformed protobuf: bad length")
		}
		buf = buf[n:]
		fn(field, buf[:length])
		buf = buf[length:]
	}
}

func uvarint(buf []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(buf) && i < 10; i++ {
		v |= uint64(buf[i]&0x7f) << (7 * i)
		if buf[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// fixedOptions returns settings that flush promptly, so tests do not wait on
// production cadences.
func fixedOptions(url string) Options {
	return Options{
		RemoteWriteURL: url,
		SampleInterval: 4 * time.Millisecond,
		FlushInterval:  8 * time.Millisecond,
		BatchSize:      1000,
		Timeout:        2 * time.Second,
		Labels:         []Label{{Name: "instance", Value: "mon0"}, {Name: "job", Value: "go-sysmon"}},
	}
}

// baseMs dates the readings the test collectors produce.
const baseMs int64 = 1788300000000

// countingCollector offers a new reading, one sample, on every call and
// reports how often it ran.
func countingCollector(calls *atomic.Int64) Collect {
	return func() (Measurement, error) {
		n := calls.Add(1)
		return Measurement{TimeMs: baseMs + n, Write: func(b *Builder, timeMs int64) {
			b.Add("sysmon_up", 1, timeMs)
		}}, nil
	}
}

// newReadingEveryCall is a collector whose every call is a fresh collection,
// for the tests about batching and delivery rather than about which readings
// are new.
func newReadingEveryCall(write func(b *Builder, timeMs int64)) Collect {
	var n atomic.Int64
	return func() (Measurement, error) {
		return Measurement{TimeMs: baseMs + n.Add(1), Write: write}, nil
	}
}

// maxTicks bounds every wait on the exporter's loop. It is counted in ticks
// the test delivered, not in time, so a slow machine cannot reach it: a
// condition still false after this many is one the loop is never going to
// satisfy.
const maxTicks = 1000

// loop drives an exporter's loop by hand. Every tick is delivered by the test
// at a time the test chose, so which ticks have happened, and what each was
// due for, is known exactly rather than estimated from elapsed time.
type loop struct {
	t     *testing.T
	ticks chan time.Time
	// read receives once per tick, as the loop reads its options. The loop
	// reads them once, first thing, so after it the tick's configuration is
	// fixed and a test changing it affects only later ticks.
	read chan struct{}
	now  time.Time
}

// runExporter starts e's loop on ticks delivered by the returned loop, and
// stops it when the test ends.
func runExporter(t *testing.T, e *Exporter) *loop {
	t.Helper()
	l := &loop{
		t:     t,
		ticks: make(chan time.Time),
		read:  make(chan struct{}),
		now:   time.UnixMilli(baseMs),
	}
	opts := e.opts
	e.opts = func() Options {
		o := opts()
		l.read <- struct{}{}
		return o
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.run(done, l.ticks)
	}()
	t.Cleanup(func() {
		close(done)
		await.Recv(t, "the loop to return after done was closed", stopped)
	})
	return l
}

// deliver hands the loop one tick at now and returns once the loop has read
// the options it will handle it under. The loop takes a tick only after
// finishing the one before, so that one is complete as well.
func (l *loop) deliver(now time.Time) {
	l.t.Helper()
	await.Send(l.t, "the exporter loop to take a tick", l.ticks, now)
	await.Recv(l.t, "the exporter loop to read its options", l.read)
}

// step advances the loop's clock by d and delivers one tick at the new time.
func (l *loop) step(d time.Duration) {
	l.t.Helper()
	l.now = l.now.Add(d)
	l.deliver(l.now)
}

// settle returns once everything the last tick triggered has finished.
//
// It does so by delivering one more tick, which the loop takes only after
// finishing the last one. That tick is still being handled as the test goes
// on, so it must do nothing the test could observe. It is stamped with the
// zero time, before every tick already handled, so no interval has elapsed
// since the last sample or flush and neither is due. (A loop that has never
// sampled finds everything due, so settle follows a tick handled under the
// same options: if that one was enabled it sampled and flushed, and if it
// was disabled so is this.) Its options were read before settle returned, so
// a change the test makes next cannot reach it.
func (l *loop) settle() {
	l.t.Helper()
	l.deliver(time.Time{})
}

// tick delivers one tick a production base tick after the last one and
// returns once all of its work is done.
func (l *loop) tick() {
	l.t.Helper()
	l.step(tickInterval)
	l.settle()
}

// tickN delivers n ticks, as tick does.
func (l *loop) tickN(n int) {
	l.t.Helper()
	for range n {
		l.tick()
	}
}

// until ticks the loop until cond holds, failing the test if it still does not
// after maxTicks. cond is checked before the first tick and after each one has
// finished.
func (l *loop) until(why string, cond func() bool) {
	l.t.Helper()
	for range maxTicks {
		if cond() {
			return
		}
		l.tick()
	}
	if !cond() {
		l.t.Fatalf("%s did not happen within %d ticks", why, maxTicks)
	}
}

// TestExporter_RunStopsWhenDoneIsClosed covers Run itself: it paces the loop
// with a real ticker and returns when done is closed.
func TestExporter_RunStopsWhenDoneIsClosed(t *testing.T) {
	e := NewExporter(func() Options { return Options{} }, countingCollector(new(atomic.Int64)), quietLogger())
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Run(done)
	}()
	close(done)
	await.Recv(t, "Run to return after done was closed", stopped)
}

// TestExporter_DisabledCollectsNothing is the load-bearing property of this
// whole feature: with no remote_write_url a node behaves exactly as it did
// before the section existed. Not "makes no request" -- makes no request AND
// never calls the collector, so it costs a node nothing to have the exporter
// wired in unconditionally.
func TestExporter_DisabledCollectsNothing(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		o.RemoteWriteURL = ""
		return o
	}, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	// Every tick is due for a sample and a flush, so each is a chance to
	// collect or send that a disabled exporter must pass up.
	l.tickN(10)

	if got := calls.Load(); got != 0 {
		t.Errorf("collector ran %d times with no endpoint configured, want 0", got)
	}
	if requests, _ := rec.counts(); requests != 0 {
		t.Errorf("made %d requests with no endpoint configured, want 0", requests)
	}
	if st := e.Stats(); st.Enabled || st.BufferedSamples != 0 {
		t.Errorf("Stats() = %+v, want disabled and empty", st)
	}
}

// TestExporter_PushesSamples checks the samples reach the receiver, counted
// from the bytes it received.
func TestExporter_PushesSamples(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	l.until("the first batch to be accepted", func() bool { return rec.acceptedSamples(t) > 0 })
	l.until("several samples to arrive", func() bool { return rec.acceptedSamples(t) >= 3 })

	st := e.Stats()
	if !st.Enabled {
		t.Error("Stats().Enabled = false while pushing")
	}
	if st.SamplesWritten <= 0 {
		t.Errorf("Stats().SamplesWritten = %d, want > 0", st.SamplesWritten)
	}
	if st.LastSuccessUnix == 0 {
		t.Error("Stats().LastSuccessUnix = 0 after a successful push")
	}
}

// TestExporter_BatchSizeFlushesEarly: the flush interval is 10s here and the
// loop's clock is never advanced that far after the first tick's flush, so
// the later samples got there because the batch filled, not because the timer
// fired.
func TestExporter_BatchSizeFlushesEarly(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		o.FlushInterval = 10 * time.Second
		o.BatchSize = 3
		return o
	}, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	// The first tick flushes at once -- a node's first flush does not wait out
	// the interval -- so one sample is delivered by the timer. The three after
	// it can only be delivered by the batch filling.
	l.tick()
	if got := rec.acceptedSamples(t); got != 1 {
		t.Fatalf("delivered %d samples after the first tick, want the 1 of its immediate flush", got)
	}
	l.tickN(3) // three seconds on the loop's clock: well inside the 10s interval
	if got := rec.acceptedSamples(t); got != 4 {
		t.Fatalf("delivered %d samples after three more ticks, want 4: a full batch of 3 must flush early", got)
	}
}

// TestExporter_RetryableFailureKeepsTheBatch. A receiver that is restarting
// must not cost the history it could not take: the samples stay buffered and go
// out when it comes back, rather than being counted as sent and discarded.
func TestExporter_RetryableFailureKeepsTheBatch(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	l.until("the exporter to record failures", func() bool { return e.Stats().ConsecutiveFailures > 0 })
	l.until("samples to accumulate in the buffer", func() bool { return e.Stats().BufferedSamples >= 2 })

	if st := e.Stats(); st.SamplesDropped != 0 {
		t.Errorf("Stats().SamplesDropped = %d during a retryable outage, want 0", st.SamplesDropped)
	}
	buffered := e.Stats().BufferedSamples

	rec.status.Store(http.StatusNoContent)
	l.until("the buffered samples to be delivered", func() bool {
		return rec.acceptedSamples(t) >= buffered
	})
	l.until("the failure counter to reset", func() bool { return e.Stats().ConsecutiveFailures == 0 })
}

// TestExporter_PermanentRejectionIsDropped. A batch Prometheus will never
// accept -- a sample outside the retention window, a label set that does not
// parse -- must not be replayed forever: holding it would block every later
// batch behind a request guaranteed to fail, so one bad sample set would stop
// history altogether.
func TestExporter_PermanentRejectionIsDropped(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusBadRequest)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	l.until("the rejected batch to be dropped", func() bool { return e.Stats().SamplesDropped > 0 })
	l.until("the buffer to stay bounded", func() bool { return e.Stats().BufferedSamples <= 1 })

	rec.status.Store(http.StatusNoContent)
	l.until("history to resume once the receiver recovers", func() bool {
		return rec.acceptedSamples(t) > 0
	})
}

// TestExporter_BacklogIsBounded. A receiver that is down for a long time must
// not grow the process without limit, and what survives has to be the recent
// past -- the part anyone looks at after an outage.
func TestExporter_BacklogIsBounded(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable)

	// Batch size 1 with ten buffered batches means the bound is ten samples,
	// reached within a few ticks instead of a few hours.
	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		o.BatchSize = 1
		return o
	}, newReadingEveryCall(func(b *Builder, timeMs int64) {
		b.Add("sysmon_up", 1, timeMs)
	}), quietLogger())
	l := runExporter(t, e)

	l.until("the backlog to reach its bound", func() bool { return e.Stats().SamplesDropped > 0 })

	// Keep collecting well past the bound: it has to hold on every tick, not
	// just the one that first reached it.
	for range 3 * bufferedBatches {
		l.tick()
		if buffered := e.Stats().BufferedSamples; buffered > 1*bufferedBatches+1 {
			t.Fatalf("BufferedSamples = %d, want at most %d; the bound is not holding",
				buffered, 1*bufferedBatches+1)
		}
	}
}

// TestExporter_ReloadDisables covers turning remote write off in the config
// file. The buffer goes with it: backfilling a gap on re-enable would deliver
// samples the operator had already decided not to keep.
func TestExporter_ReloadDisables(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable)

	var url atomic.Value
	url.Store(rec.srv.URL)
	var calls atomic.Int64

	e := NewExporter(func() Options {
		o := fixedOptions(url.Load().(string))
		o.RemoteWriteURL = url.Load().(string)
		return o
	}, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	l.until("samples to buffer while the receiver is down", func() bool {
		return e.Stats().BufferedSamples > 0
	})

	url.Store("")
	l.until("the exporter to go quiet", func() bool {
		st := e.Stats()
		return !st.Enabled && st.BufferedSamples == 0
	})

	before := calls.Load()
	l.tickN(10)
	if after := calls.Load(); after != before {
		t.Errorf("collector ran %d more times after remote write was turned off", after-before)
	}
}

// TestExporter_ReloadRetargets: pointing at a different receiver has to take
// effect without a restart, which would drop every connected browser.
//
// And the backlog does NOT follow. Samples buffered while the first endpoint
// was unreachable belong to that endpoint; delivering them to the second after
// a retarget would write one deployment's data into another's database, across
// whatever administrative or tenant boundary the operator just moved the node
// over. The first receiver is deliberately failing here, so there IS a backlog
// to carry -- with a healthy first receiver the test would pass whatever the
// code did.
func TestExporter_ReloadRetargets(t *testing.T) {
	first, second := newRecorder(t), newRecorder(t)
	first.status.Store(http.StatusServiceUnavailable)

	var url atomic.Value
	url.Store(first.srv.URL)

	// Each sample carries a value naming its intended endpoint, so a sample
	// delivered to the wrong one is identifiable in the bytes that arrived.
	var target atomic.Int64
	target.Store(1)
	e := NewExporter(func() Options {
		return fixedOptions(url.Load().(string))
	}, newReadingEveryCall(func(b *Builder, timeMs int64) {
		b.Add("sysmon_up", float64(target.Load()), timeMs)
	}), quietLogger())
	l := runExporter(t, e)

	l.until("a backlog to build against the first receiver", func() bool {
		return e.Stats().BufferedSamples >= 3
	})

	target.Store(2)
	url.Store(second.srv.URL)
	l.until("the second receiver to be used", func() bool { return second.acceptedSamples(t) > 0 })

	// Let the retargeted exporter flush anything it kept: each tick flushes.
	l.tickN(10)
	for _, value := range second.deliveredValues(t) {
		if value == 1 {
			t.Fatalf("a sample intended for the first endpoint was delivered to the second")
		}
	}
	if e.Stats().SamplesDropped == 0 {
		t.Error("the discarded backlog was not counted as dropped; a gap in the " +
			"recorded history would have no number beside it")
	}
}

// TestExporter_IdentityChangeDiscardsTheBuffer. Buffered samples describe a
// node under the name it had; sending them after a rename would attribute one
// machine's past to another's series.
func TestExporter_IdentityChangeDiscardsTheBuffer(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable)

	var instance atomic.Value
	instance.Store("mon0")
	var calls atomic.Int64

	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		o.Labels = []Label{{Name: "instance", Value: instance.Load().(string)}}
		return o
	}, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	l.until("a backlog to build", func() bool { return e.Stats().BufferedSamples >= 3 })

	instance.Store("mon1")
	l.until("the buffer to be discarded and counted", func() bool {
		return e.Stats().SamplesDropped > 0
	})

	rec.status.Store(http.StatusNoContent)
	l.until("the new identity to be delivered", func() bool { return rec.acceptedSamples(t) > 0 })
	for _, name := range rec.deliveredInstances(t) {
		if name != "mon1" {
			t.Fatalf("delivered a sample labelled %q after the node was renamed", name)
		}
	}
}

// TestExporter_NewestBatchSurvivesTheBound. The bound is "BatchSize x
// bufferedBatches, or one batch, whichever is larger": a batch is one or more
// whole collections and cannot be split, so a BatchSize smaller than a single
// collection must not mean discarding everything the moment it is collected --
// a configuration that records nothing rather than one that records a little.
func TestExporter_NewestBatchSurvivesTheBound(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable)

	// 50 samples per collection with a batch size of 1: every collection is
	// fifty times the configured batch and five times the whole bound.
	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		o.BatchSize = 1
		return o
	}, newReadingEveryCall(func(b *Builder, timeMs int64) {
		for i := 0; i < 50; i++ {
			b.Add("sysmon_disk_used_percent", float64(i), timeMs, "device", "sd"+string(rune('a'+i%26)))
		}
	}), quietLogger())
	l := runExporter(t, e)

	l.until("the bound to start dropping", func() bool { return e.Stats().SamplesDropped > 0 })

	// The newest batch has to survive every trim, not only the first.
	for range 10 {
		l.tick()
		if buffered := e.Stats().BufferedSamples; buffered == 0 {
			t.Fatal("everything was discarded; a batch larger than the bound must still be kept")
		}
	}

	rec.status.Store(http.StatusNoContent)
	l.until("the kept batch to be delivered once the receiver recovers", func() bool {
		return rec.acceptedSamples(t) > 0
	})
}

// TestExporter_ReloadShortensALongInterval. Sizing a ticker to the configured
// interval means a node moving from an hourly cadence to a fast one would not
// notice for an hour. The loop wakes on a fixed base tick instead, so a reload
// takes effect within one of those.
func TestExporter_ReloadShortensALongInterval(t *testing.T) {
	rec := newRecorder(t)

	var slow atomic.Bool
	slow.Store(true)
	var calls atomic.Int64

	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		if slow.Load() {
			o.SampleInterval = time.Hour
			o.FlushInterval = time.Hour
		}
		return o
	}, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	// The first tick fires immediately -- a node with an hourly cadence must
	// not record nothing for its first hour -- so exactly one sample is
	// expected, and no more for the rest of the hour.
	l.tick()
	if got := calls.Load(); got != 1 {
		t.Fatalf("collector ran %d times on the first tick, want 1", got)
	}
	for range 59 {
		l.step(time.Minute)
	}
	l.settle()
	if got := calls.Load(); got != 1 {
		t.Fatalf("collector ran %d times within an hourly interval, want 1", got)
	}

	// The reload takes effect on the very next tick, not when the hour is up.
	slow.Store(false)
	l.tick()
	if got := calls.Load(); got != 2 {
		t.Fatalf("collector ran %d times on the tick after the interval was shortened, want 2", got)
	}
	l.until("the shortened interval to keep sampling", func() bool { return calls.Load() >= 4 })
}

// TestExporter_CollectFailureIsNotFatal: a snapshot that is not ready yet is
// ordinary at startup, and the exporter has to keep running through it.
func TestExporter_CollectFailureIsNotFatal(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, func() (Measurement, error) {
		n := calls.Add(1)
		if n <= 3 {
			return Measurement{}, &ErrNoQueryEndpoint{} // any error will do
		}
		return Measurement{TimeMs: baseMs + n, Write: func(b *Builder, timeMs int64) {
			b.Add("sysmon_up", 1, timeMs)
		}}, nil
	}, quietLogger())
	l := runExporter(t, e)

	l.until("the exporter to recover and push", func() bool { return rec.acceptedSamples(t) > 0 })
}

// TestExporter_StatsAreRaceFree exercises the published statistics from another
// goroutine, which is how the scrape handler reads them.
func TestExporter_StatsAreRaceFree(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.Stats()
				}
			}
		}()
	}
	l.until("a push while stats are being read", func() bool { return rec.acceptedSamples(t) > 0 })
	close(stop)
	wg.Wait()
}

// TestExporter_RequestTimeoutIsNotRetried pins 408 to the protocol rather than
// to intuition, because intuition gets this one wrong and did during review.
//
// Read as English, 408 says the receiver stopped waiting and the batch would be
// accepted a second later. Read as Remote-Write 1.0, it is a 4xx, and the spec
// binds both ends: senders "MUST NOT retry write requests on HTTP 2xx and 4xx
// responses other than 429", and receivers send 4xx only for a request that
// "will never be able to succeed and should not be retried". A receiver that
// wants a retry has 503.
//
// So this asserts the batch is DROPPED, and it exists because the opposite
// change was suggested, made, and had to be undone: without a test naming the
// spec, the next reviewer to read 408 as a timeout will suggest it again.
func TestExporter_RequestTimeoutIsNotRetried(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusRequestTimeout)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	l.until("the 408-rejected batch to be dropped", func() bool { return e.Stats().SamplesDropped > 0 })
	l.until("the buffer not to fill with batches the receiver refused", func() bool {
		return e.Stats().BufferedSamples <= 1
	})

	// And the exporter is not wedged: a receiver that starts accepting is
	// written to immediately, rather than after working through a queue of
	// rejections it was holding.
	rec.status.Store(http.StatusNoContent)
	l.until("history to resume once the receiver recovers", func() bool {
		return rec.acceptedSamples(t) > 0
	})
}

// TestErrRemoteWrite_RetryableFollowsTheSpec enumerates the boundary, since the
// classification is one expression and every status either side of it means a
// different outcome for the samples.
func TestErrRemoteWrite_RetryableFollowsTheSpec(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{400, false, "malformed: the receiver will never accept it"},
		{401, false, "unauthorized: replaying does not acquire credentials"},
		{404, false, "wrong path: a configuration error, not a wobble"},
		{408, false, "a 4xx, and the spec permits retrying only 429 within that range"},
		{413, false, "too large: it will be too large next time as well"},
		{429, true, "backpressure, the one 4xx the spec allows"},
		{499, false, "still a 4xx"},
		{500, true, "the receiver's problem"},
		{503, true, "unavailable: what a receiver sends when it wants the retry"},
		{599, true, "still a 5xx"},
		{200, false, "not an error at all; Write never builds one of these"},
	}

	for _, tc := range cases {
		err := &ErrRemoteWrite{URL: "http://receiver:9090/api/v1/write", Status: tc.status}
		if got := err.Retryable(); got != tc.want {
			t.Errorf("Retryable() for %d = %v, want %v -- %s", tc.status, got, tc.want, tc.why)
		}
	}
}

// TestExporter_TheTimeoutIsPerRequestNotPerFlush.
//
// Options.Timeout is documented as bounding one remote-write request. It was
// built once per flush and shared by every batch in the backlog, so the first
// slow write ate the deadline and the rest were cancelled mid-flight --
// requests that would each have finished well inside the configured timeout.
//
// Two things had to be arranged for this to be able to fail, and both were
// missing from a first version that passed against the bug.
//
// A BACKLOG. A flush produces at most one batch, so with everything succeeding
// the loop runs once per pass and a shared context is indistinguishable from a
// per-request one. The receiver therefore refuses first, until several batches
// are held, and only then starts accepting.
//
// And the DEADLINE each request carried as the assertion, rather than whether
// a slow receiver happened to see a request abandoned -- which depends on how
// fast the machine is. Each request's deadline must lie a full timeout after
// the previous request was answered: a request given its own timeout when it
// is sent always satisfies that, and one sharing a deadline set before the
// first request never can.
func TestExporter_TheTimeoutIsPerRequestNotPerFlush(t *testing.T) {
	const timeout = 300 * time.Millisecond

	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable) // retryable, so the batches are kept

	o := fixedOptions(rec.srv.URL)
	o.Timeout = timeout
	// One sample per batch, so the backlog below is several batches rather
	// than one large one.
	o.BatchSize = 1
	// Driven directly on this goroutine rather than through the loop, so the
	// writer's transport can be replaced without racing it.
	e := NewExporter(func() Options { return o }, countingCollector(new(atomic.Int64)), quietLogger())

	// With a batch size of one every sample flushes at once, and every flush
	// is refused, so each leaves one more batch held.
	for range 3 {
		e.sample(o)
	}
	if got := e.Stats().BufferedSamples; got != 3 {
		t.Fatalf("BufferedSamples = %d after three refused flushes, want a backlog of 3", got)
	}

	tr := &deadlineTransport{next: rec.srv.Client().Transport}
	e.writer.client.Transport = tr
	rec.status.Store(http.StatusNoContent)
	e.flush(o)

	if got := rec.acceptedSamples(t); got != 3 {
		t.Fatalf("the receiver accepted %d samples, want the backlog of 3 in one flush", got)
	}
	if st := e.Stats(); st.SamplesDropped != 0 {
		t.Errorf("Stats().SamplesDropped = %d against a receiver that accepted every "+
			"request, want 0", st.SamplesDropped)
	}
	sent := tr.requests()
	if len(sent) != 3 {
		t.Fatalf("%d requests were sent, want 3", len(sent))
	}
	for i, r := range sent {
		if r.deadline.IsZero() {
			t.Fatalf("request %d was sent with no deadline; the timeout does not bound it", i)
		}
		if i == 0 {
			continue
		}
		if earliest := sent[i-1].answered.Add(timeout); r.deadline.Before(earliest) {
			t.Errorf("request %d had to finish by %v, %v short of a full %v after request %d "+
				"was answered: it was sharing an earlier request's deadline",
				i, r.deadline, earliest.Sub(r.deadline), timeout, i-1)
		}
	}
}

// sentRequest is one request a deadlineTransport passed on.
type sentRequest struct {
	sent     time.Time // when it reached the transport
	deadline time.Time // zero when the request carried none
	answered time.Time // when its response, or its failure, arrived
}

// deadlineTransport records the deadline each request is sent under and when
// its response arrived, then passes it on.
type deadlineTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	sent []sentRequest
}

func (d *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	sent := time.Now()
	deadline, _ := r.Context().Deadline()
	resp, err := d.next.RoundTrip(r)
	d.mu.Lock()
	d.sent = append(d.sent, sentRequest{sent: sent, deadline: deadline, answered: time.Now()})
	d.mu.Unlock()
	return resp, err
}

func (d *deadlineTransport) requests() []sentRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sentRequest(nil), d.sent...)
}

// TestExporter_SubmitWritesUnderTheSubmittedInstance is topology A's contract
// on the exporter side: the aggregator writes its peers' readings, and each has
// to arrive labelled as the peer rather than as the node that sent it. One
// history per machine is the whole point; merged series cannot be untangled by
// any later query.
func TestExporter_SubmitWritesUnderTheSubmittedInstance(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	l := runExporter(t, e)

	if !e.Submit(Measurement{Source: "omega", TimeMs: 1700000000000, Write: func(b *Builder, timeMs int64) {
		b.Add("sysmon_up", 1, timeMs, "instance", "omega")
	}}) {
		t.Fatal("Submit refused a sample with an empty queue")
	}

	l.until("the submitted sample to be delivered", func() bool {
		for _, got := range rec.deliveredInstances(t) {
			if got == "omega" {
				return true
			}
		}
		return false
	})

	// The local samples must still be the local node's: a submission changes
	// one batch entry, not the exporter's identity.
	l.until("a local sample to be delivered", func() bool {
		for _, got := range rec.deliveredInstances(t) {
			if got == "mon0" {
				return true
			}
		}
		return false
	})
}

// TestExporter_SubmitDropsRatherThanBlocks covers the failure path. The fleet
// poller calls Submit on its own goroutine; if the queue were unbounded or
// blocking, an unreachable time-series database would stall peer polling and
// the dashboard's freshness would depend on Prometheus being up.
//
// The bound below is deliberately NOT written as submitQueue. An earlier
// version asserted "accepted == submitQueue", which the queue depth satisfies
// whatever it is set to -- raising the constant to 100,000 kept the test green
// while destroying the property it was meant to protect. What is actually
// required is that a caller is refused promptly and within a sane amount of
// memory, so the test says that instead.
func TestExporter_SubmitDropsRatherThanBlocks(t *testing.T) {
	// Inside a synctest bubble the clock advances only while every goroutine
	// in it is blocked, so a Submit that waited in any way -- on the queue, or
	// on a timer however short -- shows up as time having passed, and one that
	// blocked for good is reported as a deadlock. Nothing here depends on how
	// fast the machine is.
	synctest.Test(t, func(t *testing.T) {
		// Deliberately NOT run: with no loop nothing drains the queue, so this
		// measures what a caller sees when the exporter cannot keep up. No
		// request is ever made, so the endpoint need not exist.
		e := NewExporter(func() Options { return fixedOptions("http://receiver.invalid/api/v1/write") },
			countingCollector(new(atomic.Int64)), quietLogger())

		const sane = 10000 // a queue deeper than this is a memory bug of its own
		accepted, refused := 0, 0
		for i := 0; i < sane && refused == 0; i++ {
			start := time.Now()
			ok := e.Submit(Measurement{Source: "omega", TimeMs: 1700000000000, Write: func(b *Builder, timeMs int64) {
				b.Add("sysmon_up", 1, timeMs)
			}})
			if waited := time.Since(start); waited != 0 {
				t.Fatalf("Submit waited %v on call %d; it must never wait on the exporter", waited, i)
			}
			if ok {
				accepted++
			} else {
				refused++
			}
		}

		if refused == 0 {
			t.Fatalf("Submit accepted %d samples with nothing draining them and never refused; the queue is unbounded in practice", accepted)
		}
		if dropped := e.Stats().SamplesDropped; dropped != int64(refused) {
			t.Errorf("SamplesDropped = %d after %d refusals, want them counted", dropped, refused)
		}
		if e.Submit(Measurement{Source: "omega", TimeMs: 1700000000000}) {
			t.Error("Submit accepted a measurement with nothing to write")
		}
	})
}

// TestExporter_SubmitDiscardedWhileDisabled keeps a disabled node costing
// nothing. A submission arriving with no endpoint configured is dropped as it
// is drained -- not held -- so turning remote write on later does not flush a
// burst of samples carrying timestamps from before it was enabled.
func TestExporter_SubmitDiscardedWhileDisabled(t *testing.T) {
	rec := newRecorder(t)
	var enabled atomic.Bool

	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		if !enabled.Load() {
			o.RemoteWriteURL = ""
		}
		return o
	}, countingCollector(new(atomic.Int64)), quietLogger())
	l := runExporter(t, e)

	if !e.Submit(Measurement{Source: "omega", TimeMs: 1600000000000, Write: func(b *Builder, timeMs int64) {
		b.Add("sysmon_stale", 1, timeMs, "instance", "omega")
	}}) {
		t.Fatal("Submit refused a sample with an empty queue")
	}
	// The next tick drains the queue, and a disabled exporter drops what it
	// drains.
	l.tick()
	if got := e.Stats().SamplesDropped; got != 1 {
		t.Fatalf("SamplesDropped = %d after the disabled exporter drained one submission, want 1", got)
	}

	enabled.Store(true)
	l.until("the enabled exporter to deliver its own samples", func() bool {
		return len(rec.deliveredInstances(t)) > 0
	})

	for _, got := range rec.deliveredInstances(t) {
		if got == "omega" {
			t.Error("a submission made while remote write was off was delivered after it was turned on")
		}
	}
}
