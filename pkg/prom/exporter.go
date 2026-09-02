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

// tickInterval is how often the exporter wakes to decide whether to sample or
// flush.
//
// It is a variable rather than a constant so tests can drive the loop without
// waiting on production cadences, the same way keepaliveInterval works in
// pkg/server. Waking on a fixed base tick rather than on tickers sized to the
// configured intervals is what bounds reload latency: with a ticker per
// interval, moving from a one-hour cadence to a one-minute one would not be
// noticed until the hour was up.
var tickInterval = time.Second

// Options is the exporter's runtime configuration. It is re-read on every
// cycle, so remote write can be turned on, retargeted or turned off by
// reloading the config file rather than restarting -- which would drop every
// connected browser and empty the history buffers.
type Options struct {
	// RemoteWriteURL is the receiver. Empty disables the exporter entirely:
	// nothing is collected, nothing is buffered, and no request is ever made.
	RemoteWriteURL string
	// SampleInterval is how often a set of samples is taken. It is deliberately
	// independent of the dashboard's stream rate: a browser watching at 250ms
	// must not write four samples a second into a time-series database.
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

// Collect adds one instant's samples to b, stamped timeMs. Returning an error
// means this tick produced nothing; the exporter logs it and carries on rather
// than stopping, since a collector that fails once usually succeeds next time.
type Collect func(b *Builder, timeMs int64) error

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
	return &Exporter{opts: opts, collect: collect, log: log}
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

// Run drives the exporter until done is closed. It blocks, so callers run it in
// a goroutine, exactly as the fleet poller is run.
//
// One base tick decides everything: the configured cadences are compared
// against the clock rather than turned into tickers of their own, so a reload
// takes effect within one tick however long the previous interval was.
func (e *Exporter) Run(done <-chan struct{}) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	var lastSample, lastFlush time.Time

	for {
		select {
		case <-done:
			// A last flush would need a deadline of its own and could block
			// shutdown on an unreachable receiver, so the buffer is abandoned
			// instead. At most one flush interval of samples is lost, and the
			// alternative is a service that will not stop.
			return

		case now := <-ticker.C:
			o := e.opts()
			if o.RemoteWriteURL == "" {
				e.disable()
				continue
			}
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

// sample collects one instant, and flushes early if the batch is full.
func (e *Exporter) sample(o Options) {
	if o.RemoteWriteURL == "" {
		e.disable()
		return
	}
	e.enabled.Store(true)
	e.ensureBuilder(o)

	if err := e.collect(e.builder, time.Now().UnixMilli()); err != nil {
		e.log.Debug("prom: nothing to sample this tick", "error", err)
		return
	}
	e.buffered.Store(int64(e.pendingSamples + e.builder.Len()))

	if o.BatchSize > 0 && e.builder.Len() >= o.BatchSize {
		e.flush(o)
	}
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
	}
	timeout := positive(o.Timeout, 10*time.Second)
	if e.writer == nil || e.writerURL != o.RemoteWriteURL || e.writerTimeout != timeout {
		if e.writer != nil && e.writerURL != o.RemoteWriteURL {
			e.discard("the remote-write endpoint changed")
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
