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
	"time"

	"github.com/golang/snappy"
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
		body, _ := io.ReadAll(req.Body)
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

// fastTicks shrinks the exporter's base tick for the duration of a test, so the
// loop runs at test speed rather than the one-second production cadence.
func fastTicks(t *testing.T) {
	t.Helper()
	previous := tickInterval
	tickInterval = 2 * time.Millisecond
	t.Cleanup(func() { tickInterval = previous })
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

// runExporter starts an exporter and stops it when the test ends.
func runExporter(t *testing.T, e *Exporter) {
	t.Helper()
	fastTicks(t)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Run(done)
	}()
	t.Cleanup(func() {
		close(done)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after done was closed")
		}
	})
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
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
	runExporter(t, e)

	time.Sleep(150 * time.Millisecond)

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
	runExporter(t, e)

	waitFor(t, "the first batch to be accepted", func() bool { return rec.acceptedSamples(t) > 0 })
	waitFor(t, "several samples to arrive", func() bool { return rec.acceptedSamples(t) >= 3 })

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

// TestExporter_BatchSizeFlushesEarly: the flush interval is 10s here, so
// anything that arrives inside the test window got there because the batch
// filled, not because the timer fired.
func TestExporter_BatchSizeFlushesEarly(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options {
		o := fixedOptions(rec.srv.URL)
		o.FlushInterval = 10 * time.Second
		o.BatchSize = 3
		return o
	}, countingCollector(&calls), quietLogger())
	runExporter(t, e)

	waitFor(t, "a batch-triggered flush", func() bool { return rec.acceptedSamples(t) >= 3 })
}

// TestExporter_RetryableFailureKeepsTheBatch. A receiver that is restarting
// must not cost the history it could not take: the samples stay buffered and go
// out when it comes back, rather than being counted as sent and discarded.
func TestExporter_RetryableFailureKeepsTheBatch(t *testing.T) {
	rec := newRecorder(t)
	rec.status.Store(http.StatusServiceUnavailable)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	runExporter(t, e)

	waitFor(t, "the exporter to record failures", func() bool { return e.Stats().ConsecutiveFailures > 0 })
	waitFor(t, "samples to accumulate in the buffer", func() bool { return e.Stats().BufferedSamples >= 2 })

	if st := e.Stats(); st.SamplesDropped != 0 {
		t.Errorf("Stats().SamplesDropped = %d during a retryable outage, want 0", st.SamplesDropped)
	}
	buffered := e.Stats().BufferedSamples

	rec.status.Store(http.StatusNoContent)
	waitFor(t, "the buffered samples to be delivered", func() bool {
		return rec.acceptedSamples(t) >= buffered
	})
	waitFor(t, "the failure counter to reset", func() bool { return e.Stats().ConsecutiveFailures == 0 })
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
	runExporter(t, e)

	waitFor(t, "the rejected batch to be dropped", func() bool { return e.Stats().SamplesDropped > 0 })
	waitFor(t, "the buffer to stay bounded", func() bool { return e.Stats().BufferedSamples <= 1 })

	rec.status.Store(http.StatusNoContent)
	waitFor(t, "history to resume once the receiver recovers", func() bool {
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
	runExporter(t, e)

	waitFor(t, "the backlog to reach its bound", func() bool { return e.Stats().SamplesDropped > 0 })
	time.Sleep(200 * time.Millisecond)

	if buffered := e.Stats().BufferedSamples; buffered > 1*bufferedBatches+1 {
		t.Fatalf("BufferedSamples = %d, want at most %d; the bound is not holding",
			buffered, 1*bufferedBatches+1)
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
	runExporter(t, e)

	waitFor(t, "samples to buffer while the receiver is down", func() bool {
		return e.Stats().BufferedSamples > 0
	})

	url.Store("")
	waitFor(t, "the exporter to go quiet", func() bool {
		st := e.Stats()
		return !st.Enabled && st.BufferedSamples == 0
	})

	before := calls.Load()
	time.Sleep(100 * time.Millisecond)
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
	runExporter(t, e)

	waitFor(t, "a backlog to build against the first receiver", func() bool {
		return e.Stats().BufferedSamples >= 3
	})

	target.Store(2)
	url.Store(second.srv.URL)
	waitFor(t, "the second receiver to be used", func() bool { return second.acceptedSamples(t) > 0 })

	// Give the retargeted exporter time to drain anything it kept.
	time.Sleep(100 * time.Millisecond)
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
	runExporter(t, e)

	waitFor(t, "a backlog to build", func() bool { return e.Stats().BufferedSamples >= 3 })

	instance.Store("mon1")
	waitFor(t, "the buffer to be discarded and counted", func() bool {
		return e.Stats().SamplesDropped > 0
	})

	rec.status.Store(http.StatusNoContent)
	waitFor(t, "the new identity to be delivered", func() bool { return rec.acceptedSamples(t) > 0 })
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
	runExporter(t, e)

	waitFor(t, "the bound to start dropping", func() bool { return e.Stats().SamplesDropped > 0 })
	time.Sleep(150 * time.Millisecond)

	if buffered := e.Stats().BufferedSamples; buffered == 0 {
		t.Fatal("everything was discarded; a batch larger than the bound must still be kept")
	}

	rec.status.Store(http.StatusNoContent)
	waitFor(t, "the kept batch to be delivered once the receiver recovers", func() bool {
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
	runExporter(t, e)

	// The first tick fires immediately -- a node with an hourly cadence must
	// not record nothing for its first hour -- so exactly one sample is
	// expected, and no more.
	waitFor(t, "the first sample", func() bool { return calls.Load() >= 1 })
	time.Sleep(80 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("collector ran %d times on an hourly interval, want 1", got)
	}

	slow.Store(false)
	waitFor(t, "the shortened interval to take effect", func() bool { return calls.Load() >= 4 })
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
	runExporter(t, e)

	waitFor(t, "the exporter to recover and push", func() bool { return rec.acceptedSamples(t) > 0 })
}

// TestExporter_StatsAreRaceFree exercises the published statistics from another
// goroutine, which is how the scrape handler reads them.
func TestExporter_StatsAreRaceFree(t *testing.T) {
	rec := newRecorder(t)
	var calls atomic.Int64

	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(&calls), quietLogger())
	runExporter(t, e)

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
	waitFor(t, "a push while stats are being read", func() bool { return rec.acceptedSamples(t) > 0 })
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
	runExporter(t, e)

	waitFor(t, "the 408-rejected batch to be dropped", func() bool { return e.Stats().SamplesDropped > 0 })
	waitFor(t, "the buffer not to fill with batches the receiver refused", func() bool {
		return e.Stats().BufferedSamples <= 1
	})

	// And the exporter is not wedged: a receiver that starts accepting is
	// written to immediately, rather than after working through a queue of
	// rejections it was holding.
	rec.status.Store(http.StatusNoContent)
	waitFor(t, "history to resume once the receiver recovers", func() bool {
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
// And the CANCELLATION as the assertion, seen from the receiver, rather than
// throughput -- which barely separates the two, since the shared deadline
// still lets one request through per pass. A request the exporter abandoned
// while the receiver was still holding it is unambiguous, and correct
// behaviour produces exactly none.
func TestExporter_TheTimeoutIsPerRequestNotPerFlush(t *testing.T) {
	const timeout = 300 * time.Millisecond

	var refusing atomic.Bool
	refusing.Store(true)
	var accepted, aborted atomic.Int64

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if refusing.Load() {
			// Retryable, so the batches are kept rather than dropped.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Long enough that two cannot share one deadline, short enough that
		// each fits comfortably in its own.
		select {
		case <-time.After(timeout * 2 / 3):
		case <-r.Context().Done():
			aborted.Add(1)
			return
		}
		accepted.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer slow.Close()

	var calls atomic.Int64
	opts := func() Options {
		o := fixedOptions(slow.URL)
		o.Timeout = timeout
		// One sample per batch, so the backlog below is several batches rather
		// than one large one.
		o.BatchSize = 1
		return o
	}

	e := NewExporter(opts, countingCollector(&calls), quietLogger())
	runExporter(t, e)

	waitFor(t, "a backlog of batches to build up", func() bool {
		return e.Stats().BufferedSamples >= 3
	})
	refusing.Store(false)

	waitFor(t, "the backlog to be delivered", func() bool { return accepted.Load() >= 3 })

	if n := aborted.Load(); n != 0 {
		t.Errorf("the receiver saw %d request(s) abandoned mid-flight; every one of "+
			"them was inside the configured %v and was cut short by an earlier "+
			"batch's share of it", n, timeout)
	}
	if st := e.Stats(); st.SamplesDropped != 0 {
		t.Errorf("Stats().SamplesDropped = %d against a receiver that accepted every "+
			"request, want 0", st.SamplesDropped)
	}
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
	runExporter(t, e)

	if !e.Submit(Measurement{Source: "omega", TimeMs: 1700000000000, Write: func(b *Builder, timeMs int64) {
		b.Add("sysmon_up", 1, timeMs, "instance", "omega")
	}}) {
		t.Fatal("Submit refused a sample with an empty queue")
	}

	waitFor(t, "the submitted sample to be delivered", func() bool {
		for _, got := range rec.deliveredInstances(t) {
			if got == "omega" {
				return true
			}
		}
		return false
	})

	// The local samples must still be the local node's: a submission changes
	// one batch entry, not the exporter's identity.
	waitFor(t, "a local sample to be delivered", func() bool {
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
	rec := newRecorder(t)
	// Deliberately NOT run: with no Run goroutine nothing drains the queue, so
	// this measures what a caller sees when the exporter cannot keep up.
	e := NewExporter(func() Options { return fixedOptions(rec.srv.URL) }, countingCollector(new(atomic.Int64)), quietLogger())

	const sane = 10000 // a queue deeper than this is a memory bug of its own
	accepted, refused := 0, 0
	for i := 0; i < sane && refused == 0; i++ {
		start := time.Now()
		ok := e.Submit(Measurement{Source: "omega", TimeMs: 1700000000000, Write: func(b *Builder, timeMs int64) {
			b.Add("sysmon_up", 1, timeMs)
		}})
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Fatalf("Submit blocked for %v on call %d; it must never wait on the exporter", elapsed, i)
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
	runExporter(t, e)

	e.Submit(Measurement{Source: "omega", TimeMs: 1600000000000, Write: func(b *Builder, timeMs int64) {
		b.Add("sysmon_stale", 1, timeMs, "instance", "omega")
	}})
	waitFor(t, "the disabled exporter to discard the submission", func() bool {
		return e.Stats().SamplesDropped >= 1
	})

	enabled.Store(true)
	waitFor(t, "the enabled exporter to deliver its own samples", func() bool {
		return len(rec.deliveredInstances(t)) > 0
	})

	for _, got := range rec.deliveredInstances(t) {
		if got == "omega" {
			t.Error("a submission made while remote write was off was delivered after it was turned on")
		}
	}
}
