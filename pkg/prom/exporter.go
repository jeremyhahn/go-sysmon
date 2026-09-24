package prom

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// bufferedBatches is how many batches worth of samples may pile up while
// remote write is failing, before the oldest is dropped. It bounds memory
// against an outage that outlasts the process's patience, and it is expressed
// in batches rather than bytes because that is the unit the operator configured.
//
// The most recent batch is always kept, whatever its size. A batch is one or
// more whole collections and cannot be split usefully, so a BatchSize smaller
// than a single collection would otherwise mean discarding every batch the
// moment it was made -- a configuration that recorded nothing at all rather
// than one that recorded a little. The bound is therefore "BatchSize x
// bufferedBatches, or one batch, whichever is larger".
const bufferedBatches = 10

// submitQueue bounds how many out-of-band submissions wait for Run to apply
// them. A full queue drops the newest submission and counts it, because the
// alternative -- blocking the fleet poller until the exporter wakes -- would
// make the dashboard's freshness depend on a time-series database being
// reachable, which is exactly the coupling this design is avoiding.
const submitQueue = 256

// tickInterval is how often the exporter wakes to decide whether to sample or
// flush.
//
// Waking on a fixed base tick rather than on tickers sized to the configured
// intervals is what bounds reload latency: with a ticker per interval, moving
// from a one-hour cadence to a one-minute one would not be noticed until the
// hour was up.
const tickInterval = time.Second

// Options is the exporter's runtime configuration. It is re-read on every
// cycle, so remote write can be turned on, retargeted or turned off by
// reloading the config file rather than restarting -- which would drop every
// connected browser and empty the history buffers.
type Options struct {
	// RemoteWriteURL is the receiver. Empty disables the exporter entirely:
	// nothing is collected, nothing is buffered, and no request is ever made.
	RemoteWriteURL string
	// SampleInterval is how often the exporter looks for a new reading to
	// write. It is deliberately independent of the dashboard's stream rate: a
	// browser watching at 250ms must not write four samples a second into a
	// time-series database.
	//
	// It is a ceiling on the write rate, not the write rate. A reading is
	// written once, at the time it was taken, so when collection is slower
	// than this the series advance at the collection interval instead (#173).
	SampleInterval time.Duration
	// FlushInterval bounds how long a sample waits before being sent.
	FlushInterval time.Duration
	// BatchSize is how many samples force a flush early.
	BatchSize int
	// Timeout bounds one remote-write request.
	Timeout time.Duration
	// Labels identify this node on every series it writes. Prometheus attaches
	// `instance` at scrape time; nothing does that for a pushed sample, so it
	// has to travel with the data.
	Labels []Label
}

// Measurement is one reading, dated by when it was taken.
//
// A reading is written once, stamped with its own time, and never again (owner
// ruling on #173, 2026-09-22). Stamping the exporter's clock instead re-dated
// the latest cached reading at every sample tick whenever collection was
// slower than sampling: gauges drew a staircase of copies presented as fresh
// measurements, and a counter's rate() over a short window read a flat run and
// then a jump -- zero, then a spike -- instead of the rate that happened.
// Written once at its own time, Prometheus holds only what was measured.
type Measurement struct {
	// Source names whose clock TimeMs was read from and whose series Write
	// adds to: "" for this node, a peer's fleet name for a peer's reading.
	// Readings are ordered per source, never across sources, because two
	// machines' clocks agree only as well as their NTP does.
	Source string
	// TimeMs is when the reading was taken, in milliseconds since the Unix
	// epoch on Source's clock. It is also the reading's identity: a reading
	// offered again with the same TimeMs -- this node's cached snapshot at a
	// second sample tick, or a peer's polled twice between its collections --
	// is the same measurement and is not written again.
	TimeMs int64
	// Write adds the reading's samples to b, every one stamped timeMs. It is
	// called on the exporter's goroutine; it must not retain b or block.
	Write func(b *Builder, timeMs int64)
}

// Collect returns this node's latest reading. The exporter decides whether it
// is new; the collector only has to date it truthfully.
//
// Returning an error means there is nothing to offer this tick; the exporter
// logs it and carries on rather than stopping, since a collector that fails
// once usually succeeds next time.
type Collect func() (Measurement, error)

// watermark is what the exporter knows of one source's readings.
type watermark struct {
	// writtenMs is the time of the newest reading written; zero before the
	// first.
	writtenMs int64
	// offeredMs is the time of the newest reading offered, written or not. A
	// reading offered again with this time is the same reading, so it is
	// neither written nor counted a second time.
	offeredMs int64
	// refusing is set while the source's readings are being dropped -- its
	// clock stepped backwards, or it sends no time at all -- so the cause is
	// logged once rather than at every poll until it clears.
	refusing bool
}

// Stats is what the exporter reports about itself.
//
// An exporter that has quietly stopped delivering is the failure this whole
// feature has to avoid: history that silently stops being recorded looks
// exactly like history that has not happened yet. These are published as
// metrics on the scrape endpoint, so the absence of data has a cause visible
// next to it.
type Stats struct {
	Enabled             bool
	BufferedSamples     int
	ConsecutiveFailures int
	LastSuccessUnix     int64
	SamplesWritten      int64
	SamplesDropped      int64
}

// Exporter samples on a schedule and pushes to a remote-write endpoint.
//
// One goroutine owns every mutable field except the published statistics, so
// the batching has no locks in it; Stats is read from HTTP handlers and is
// therefore atomic.
type Exporter struct {
	opts    func() Options
	collect Collect
	log     *slog.Logger

	// Owned by Run.
	builder        *Builder
	commonKey      string
	pending        [][]Series
	pendingSamples int
	writer         *Writer
	writerURL      string
	writerTimeout  time.Duration
	// marks holds the newest reading written per source. Every series of a
	// reading shares its timestamp, so one mark per source keeps each series'
	// timestamps strictly increasing, which remote write requires: a receiver
	// rejects an out-of-order or duplicate sample with a 4xx for the WHOLE
	// request, so a single one would cost every other series in its batch.
	marks map[string]watermark
	// scratch sizes a reading that is dropped rather than written, so the
	// loss is counted in samples like every other drop.
	scratch *Builder

	// submitted carries samples produced outside the exporter's own cadence --
	// the aggregator writing a peer's reading as it arrives. A channel rather
	// than a lock because Run owns the Builder and must keep owning it: the
	// closure is applied on Run's goroutine, never on the caller's.
	submitted chan Measurement

	enabled  atomic.Bool
	buffered atomic.Int64
	failures atomic.Int64
	lastOK   atomic.Int64
	written  atomic.Int64
	dropped  atomic.Int64
}

// NewExporter returns an exporter reading its settings from opts on every
// cycle and its samples from collect.
func NewExporter(opts func() Options, collect Collect, log *slog.Logger) *Exporter {
	if log == nil {
		log = slog.Default()
	}
	return &Exporter{
		opts:      opts,
		collect:   collect,
		log:       log,
		marks:     make(map[string]watermark),
		submitted: make(chan Measurement, submitQueue),
	}
}

// Stats reports the exporter's current state. Safe for concurrent use.
func (e *Exporter) Stats() Stats {
	return Stats{
		Enabled:             e.enabled.Load(),
		BufferedSamples:     int(e.buffered.Load()),
		ConsecutiveFailures: int(e.failures.Load()),
		LastSuccessUnix:     e.lastOK.Load(),
		SamplesWritten:      e.written.Load(),
		SamplesDropped:      e.dropped.Load(),
	}
}

// Submit hands the exporter a reading taken on someone else's schedule, to be
// added to the current batch when Run next wakes -- if it is new. The same
// reading submitted twice is written once; see Measurement.
//
// It exists for fleet aggregation: a peer's reading arrives when the fleet
// poller fetches it, not when this exporter decides to sample, and writing it
// on the exporter's cadence instead would mean either repeating one reading
// several times or discarding readings that did happen. Both fabricate the
// record -- the first invents observations, the second hides them.
//
// Returns false when the queue is full, having counted the loss as a drop, and
// for a measurement with no Write. Never blocks.
func (e *Exporter) Submit(m Measurement) bool {
	if m.Write == nil {
		return false
	}
	select {
	case e.submitted <- m:
		return true
	default:
		e.dropped.Add(1)
		e.log.Debug("prom: submission queue full; sample dropped")
		return false
	}
}

// drain applies everything Submit has queued. Called only from Run, which is
// what makes the Builder's single ownership hold.
func (e *Exporter) drain(o Options) {
	for {
		select {
		case m := <-e.submitted:
			// A submission arriving while remote write is off is discarded
			// rather than buffered: the exporter holds nothing in that state,
			// and a queue that filled up during an outage would flush a burst
			// of stale timestamps the moment it was turned back on.
			if o.RemoteWriteURL == "" {
				e.dropped.Add(1)
				continue
			}
			e.ensureBuilder(o)
			e.record(m)
			e.buffered.Store(int64(e.pendingSamples + e.builder.Len()))
		default:
			return
		}
	}
}

// Run drives the exporter until done is closed. It blocks, so callers run it in
// a goroutine, exactly as the fleet poller is run.
//
// One base tick decides everything: the configured cadences are compared
// against the clock rather than turned into tickers of their own, so a reload
// takes effect within one tick however long the previous interval was.
func (e *Exporter) Run(done <-chan struct{}) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	e.run(done, ticker.C)
}

// run is Run's loop, woken by ticks and deciding from each tick's time what is
// due. It takes the ticks as a channel so a test can deliver them itself and
// choose their times: it then knows exactly which ticks the loop has handled,
// instead of estimating it from how long it waited.
func (e *Exporter) run(done <-chan struct{}, ticks <-chan time.Time) {
	var lastSample, lastFlush time.Time

	for {
		select {
		case <-done:
			// A last flush would need a deadline of its own and could block
			// shutdown on an unreachable receiver, so the buffer is abandoned
			// instead. At most one flush interval of samples is lost, and the
			// alternative is a service that will not stop.
			return

		case now := <-ticks:
			o := e.opts()
			if o.RemoteWriteURL == "" {
				e.disable()
				// Still drained, so a queue filled while remote write was off
				// does not survive to be written under a later timestamp.
				e.drain(o)
				continue
			}
			// Before sampling, so a peer reading and the local sample taken in
			// the same tick land in the same batch.
			e.drain(o)
			if due(lastSample, now, o.SampleInterval, 15*time.Second) {
				lastSample = now
				e.sample(o)
			}
			if due(lastFlush, now, o.FlushInterval, 30*time.Second) {
				lastFlush = now
				e.flush(o)
			}
		}
	}
}

// due reports whether an interval has elapsed since last. A zero last means the
// first tick after startup, which fires immediately rather than waiting out a
// full interval -- otherwise a node with an hour-long flush interval would
// record nothing for its first hour.
func due(last, now time.Time, interval, fallback time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(positive(interval, fallback)))
}

// sample takes this node's latest reading, writes it if it is new, and flushes
// early if the batch is full.
func (e *Exporter) sample(o Options) {
	if o.RemoteWriteURL == "" {
		e.disable()
		return
	}
	e.enabled.Store(true)
	e.ensureBuilder(o)

	m, err := e.collect()
	if err != nil {
		e.log.Debug("prom: nothing to sample this tick", "error", err)
		return
	}
	e.record(m)
	e.buffered.Store(int64(e.pendingSamples + e.builder.Len()))

	if o.BatchSize > 0 && e.builder.Len() >= o.BatchSize {
		e.flush(o)
	}
}

// record adds a reading to the batch, stamped with its own time, unless it has
// been written already or is older than one that has.
//
// Older happens when a source's clock steps backwards -- an NTP step, or a
// clock corrected after running ahead. Those readings are real, but the
// receiver already holds later timestamps for the same series and would
// refuse them as out of order, taking the rest of their batch with them. So
// they are dropped here, counted, and the source's history resumes by itself
// the moment its clock passes the newest timestamp written. The gap is exactly
// as long as the step; nothing is re-dated to fill it, because a re-dated
// reading is one the history would then misreport.
//
// A reading with no usable time -- a peer on a build that did not send one --
// is dropped for the same reason: there is no truthful time to write it at.
// Such readings cannot be told apart, so a source sending them is counted once
// per run of them, not once per poll.
//
// Every drop is counted in samples, the unit SamplesDropped reports in.
func (e *Exporter) record(m Measurement) {
	if m.Write == nil {
		return
	}
	mark, seen := e.marks[m.Source]
	if seen && m.TimeMs == mark.offeredMs {
		return // the same reading, offered again
	}
	mark.offeredMs = m.TimeMs

	undated := m.TimeMs <= 0
	if undated || m.TimeMs <= mark.writtenMs {
		e.dropped.Add(e.samplesIn(m))
		if !mark.refusing {
			mark.refusing = true
			if undated {
				e.log.Warn("prom: a source sends readings with no collection time; they are dropped",
					"source", m.Source)
			} else {
				e.log.Warn("prom: a clock stepped backwards; its readings are dropped until it passes the newest one written",
					"source", m.Source, "reading_ms", m.TimeMs, "newest_written_ms", mark.writtenMs,
					"step", time.Duration(mark.writtenMs-m.TimeMs)*time.Millisecond)
			}
		}
		e.marks[m.Source] = mark
		return
	}
	if mark.refusing {
		e.log.Info("prom: a source's readings are being written again", "source", m.Source)
	}
	e.marks[m.Source] = watermark{writtenMs: m.TimeMs, offeredMs: m.TimeMs}
	m.Write(e.builder, m.TimeMs)
}

// samplesIn counts the samples a reading would have written.
func (e *Exporter) samplesIn(m Measurement) int64 {
	if e.scratch == nil {
		e.scratch = NewBuilder(nil)
	}
	m.Write(e.scratch, m.TimeMs)
	n := e.scratch.Len()
	e.scratch.Drain()
	return int64(n)
}

// discard drops everything held and accounts for it, so a gap in the recorded
// history always has a number beside it rather than appearing from nowhere.
func (e *Exporter) discard(reason string) {
	held := e.pendingSamples
	if e.builder != nil {
		held += e.builder.Len()
		e.builder.Drain()
	}
	e.pending, e.pendingSamples = nil, 0
	if held > 0 {
		e.dropped.Add(int64(held))
		e.log.Warn("prom: discarding buffered samples", "reason", reason, "samples", held)
	}
	e.buffered.Store(0)
}

// disable drops everything held for a receiver that is no longer configured.
// Keeping the buffer would mean that turning remote write off and on again
// backfilled a gap with samples the operator had already decided not to keep.
func (e *Exporter) disable() {
	wasEnabled := e.enabled.Swap(false)
	if wasEnabled || e.pendingSamples > 0 {
		e.discard("remote write was turned off")
	}
}

// ensureBuilder rebuilds the accumulator when the node's labels change, and
// the client when the endpoint or timeout does.
//
// Both changes throw the buffer away, and that is the point rather than an
// oversight. A backlog accumulated while endpoint A was unreachable belongs to
// A: delivering it to B after a retarget would cross whatever administrative or
// tenant boundary the operator moved the node across, writing one deployment's
// data into another's database. A change of identity labels is the same
// argument in miniature -- the held samples describe a node that no longer
// exists under that name.
func (e *Exporter) ensureBuilder(o Options) {
	if key := labelKey(o.Labels); e.builder == nil || key != e.commonKey {
		if e.builder != nil {
			e.discard("the node's identity labels changed")
		}
		e.builder = NewBuilder(o.Labels)
		e.commonKey = key
		// The marks are kept. Not every series changes with these labels --
		// a peer's instance is its own -- and the receiver still holds the
		// newest timestamps of those that do not. Keeping them costs a
		// renamed series at most one collection interval before its first
		// sample; clearing them could write a lagging clock's reading out of
		// order into a series that was never renamed.
	}
	timeout := positive(o.Timeout, 10*time.Second)
	if e.writer == nil || e.writerURL != o.RemoteWriteURL || e.writerTimeout != timeout {
		if e.writer != nil && e.writerURL != o.RemoteWriteURL {
			e.discard("the remote-write endpoint changed")
			// The new receiver holds none of what the old one was sent, so
			// the current reading is new to it.
			clear(e.marks)
		}
		e.writer = NewWriter(o.RemoteWriteURL, timeout)
		e.writerURL, e.writerTimeout = o.RemoteWriteURL, timeout
	}
}

// flush moves the accumulated samples into the pending queue and sends as much
// of it as the receiver will take.
func (e *Exporter) flush(o Options) {
	if o.RemoteWriteURL == "" {
		e.disable()
		return
	}
	e.ensureBuilder(o)

	if n := e.builder.Len(); n > 0 {
		e.pending = append(e.pending, e.builder.Drain())
		e.pendingSamples += n
	}
	e.trim(o)

	// One timeout PER REQUEST, not one for the pass.
	//
	// A single context built here is shared by every batch in the backlog, so
	// the first slow write eats the deadline and the rest fail immediately
	// with "context deadline exceeded" -- requests that would each have
	// completed well inside the configured timeout. During exactly the
	// condition the buffer exists for, a receiver that is slow but working,
	// that turns a backlog into dropped samples.
	timeout := positive(o.Timeout, 10*time.Second)

	for len(e.pending) > 0 {
		batch := e.pending[0]
		err := e.writeOne(timeout, batch)
		if err == nil {
			e.pending = e.pending[1:]
			n := sampleCount(batch)
			e.pendingSamples -= n
			e.written.Add(int64(n))
			e.failures.Store(0)
			e.lastOK.Store(time.Now().Unix())
			continue
		}

		// A rejection that replaying cannot fix is dropped here rather than
		// held: keeping it would block every later batch behind a request that
		// is guaranteed to fail, so one bad sample set would stop history
		// altogether.
		var rw *ErrRemoteWrite
		if errors.As(err, &rw) && !rw.Retryable() {
			e.pending = e.pending[1:]
			n := sampleCount(batch)
			e.pendingSamples -= n
			e.dropped.Add(int64(n))
			e.log.Warn("prom: remote write rejected a batch permanently, dropping it",
				"error", err, "samples", n)
			continue
		}

		e.failures.Add(1)
		e.log.Warn("prom: remote write failed, keeping the batch for the next flush",
			"error", err, "buffered_samples", e.pendingSamples,
			"consecutive_failures", e.failures.Load())
		break
	}
	e.buffered.Store(int64(e.pendingSamples))
}

// writeOne sends one batch under its own deadline.
//
// A function rather than an inline context so the cancel is deferred to the
// end of the REQUEST rather than the end of the flush: deferring inside the
// loop would hold every context of the pass open until it finished, which is
// the leak vet warns about and, with a long backlog, a real one.
func (e *Exporter) writeOne(timeout time.Duration, batch []Series) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e.writer.Write(ctx, batch)
}

// trim drops the oldest batches once the backlog exceeds what the operator's
// batch size implies. Oldest first, because during a long outage the recent
// past is the part anyone will look at.
//
// The last batch is never dropped, so the bound is "BatchSize x
// bufferedBatches, or one batch, whichever is larger" -- see the constant.
func (e *Exporter) trim(o Options) {
	limit := positiveInt(o.BatchSize, 1000) * bufferedBatches
	for e.pendingSamples > limit && len(e.pending) > 1 {
		n := sampleCount(e.pending[0])
		e.pending = e.pending[1:]
		e.pendingSamples -= n
		e.dropped.Add(int64(n))
		e.log.Warn("prom: remote write backlog exceeded its bound, dropped the oldest batch",
			"samples", n, "limit", limit)
	}
}

func sampleCount(batch []Series) int {
	n := 0
	for i := range batch {
		n += len(batch[i].Samples)
	}
	return n
}

func positive(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

func positiveInt(n, fallback int) int {
	if n <= 0 {
		return fallback
	}
	return n
}
