package prom

import (
	"slices"
	"testing"
)

// These tests drive the exporter's steps directly -- sample, drain, flush --
// rather than through Run, so what reaches the receiver is decided by the
// sequence of readings and not by how ticks happened to land (#173).

// reading is one scripted collection: when it was taken, and the counter
// value it read.
type reading struct {
	timeMs int64
	value  float64
}

// measure turns a scripted reading into a Measurement for source, writing one
// counter sample labelled with the source's instance.
func measure(source string, r reading) Measurement {
	return Measurement{Source: source, TimeMs: r.timeMs, Write: func(b *Builder, timeMs int64) {
		if source == "" {
			b.Add("sysmon_swap_in_pages_total", r.value, timeMs)
			return
		}
		b.Add("sysmon_swap_in_pages_total", r.value, timeMs, "instance", source)
	}}
}

// scripted is a collector that returns readings[i] on its i-th call, and the
// last one for every call after that -- the monitor's cached snapshot, which
// does not change between collections.
func scripted(readings []reading) Collect {
	i := 0
	return func() (Measurement, error) {
		r := readings[min(i, len(readings)-1)]
		i++
		return measure("", r), nil
	}
}

// delivered returns, per instance, the timestamps and values the receiver
// accepted, decoded from the bytes on the wire.
func delivered(t *testing.T, rec *recorder) (times map[string][]int64, values map[string][]float64) {
	t.Helper()
	times, values = map[string][]int64{}, map[string][]float64{}
	for _, s := range rec.deliveredSeries(t) {
		times[s.instance] = append(times[s.instance], s.times...)
		values[s.instance] = append(values[s.instance], s.values...)
	}
	return times, values
}

// TestExporter_SlowCollectionWritesEachReadingOnce is the issue's own case: a
// dashboard collecting every 60s under an exporter sampling every 15s. Six
// sample ticks see three collections. Before #173 all six were written, each
// re-dated to its tick, so a counter's rate() over a short window read a flat
// run and then a jump. Now each collection is written once, at the time it was
// taken, and the rate between stored samples is the rate that happened.
func TestExporter_SlowCollectionWritesEachReadingOnce(t *testing.T) {
	rec := newRecorder(t)
	const minute = int64(60_000)
	collected := []reading{
		{baseMs, 100},
		{baseMs, 100},
		{baseMs, 100},
		{baseMs, 100},
		{baseMs + minute, 160},
		{baseMs + minute, 160},
		{baseMs + minute, 160},
		{baseMs + minute, 160},
		{baseMs + 2*minute, 220},
	}
	o := fixedOptions(rec.srv.URL)
	e := NewExporter(func() Options { return o }, scripted(collected), quietLogger())

	for range collected {
		e.sample(o)
	}
	e.flush(o)

	times, values := delivered(t, rec)
	if want := []int64{baseMs, baseMs + minute, baseMs + 2*minute}; !slices.Equal(times["mon0"], want) {
		t.Fatalf("delivered timestamps = %v, want each collection once at its own time %v", times["mon0"], want)
	}
	if want := []float64{100, 160, 220}; !slices.Equal(values["mon0"], want) {
		t.Fatalf("delivered values = %v, want %v", values["mon0"], want)
	}
	// 60 pages a minute is one a second, between every pair of stored
	// samples -- which is what rate() over any window holding two of them
	// computes.
	ts, vs := times["mon0"], values["mon0"]
	for i := 1; i < len(ts); i++ {
		if rate := (vs[i] - vs[i-1]) / (float64(ts[i]-ts[i-1]) / 1000); rate != 1 {
			t.Errorf("rate between samples %d and %d = %v/s, want 1/s", i-1, i, rate)
		}
	}
	if st := e.Stats(); st.SamplesWritten != 3 || st.SamplesDropped != 0 {
		t.Errorf("Stats() = %+v, want 3 written and none dropped; an unchanged reading is not a loss", st)
	}
}

// TestExporter_UnchangedReadingIsNotWrittenInALaterBatch: the watermark has to
// outlive a flush, or a reading cached across a flush boundary is written once
// per batch.
func TestExporter_UnchangedReadingIsNotWrittenInALaterBatch(t *testing.T) {
	rec := newRecorder(t)
	o := fixedOptions(rec.srv.URL)
	e := NewExporter(func() Options { return o }, scripted([]reading{{baseMs, 1}}), quietLogger())

	e.sample(o)
	e.flush(o)
	e.sample(o)
	e.flush(o)

	if times, _ := delivered(t, rec); !slices.Equal(times["mon0"], []int64{baseMs}) {
		t.Fatalf("delivered timestamps = %v, want the one reading once", times["mon0"])
	}
}

// TestExporter_PeerReadingPolledTwiceIsWrittenOnce is topology A: the
// aggregator polls a peer faster than the peer collects, so the same reading
// arrives more than once. It is written once, at the peer's collection time.
//
// The peer's clock is also set behind this node's here. Readings are ordered
// per source, so a peer whose timestamps are older than the local node's is
// not mistaken for a clock that stepped backwards.
func TestExporter_PeerReadingPolledTwiceIsWrittenOnce(t *testing.T) {
	rec := newRecorder(t)
	o := fixedOptions(rec.srv.URL)
	e := NewExporter(func() Options { return o }, scripted([]reading{{baseMs, 1}}), quietLogger())

	peer := baseMs - 3_600_000 // an hour behind this node
	polls := []reading{{peer, 5}, {peer, 5}, {peer + 1000, 6}, {peer + 1000, 6}, {peer + 2000, 7}}
	e.sample(o)
	for _, r := range polls {
		if !e.Submit(measure("omega", r)) {
			t.Fatal("Submit refused a reading with an empty queue")
		}
		e.drain(o)
	}
	e.flush(o)

	times, values := delivered(t, rec)
	if want := []int64{peer, peer + 1000, peer + 2000}; !slices.Equal(times["omega"], want) {
		t.Fatalf("peer timestamps = %v, want each peer reading once %v", times["omega"], want)
	}
	if want := []float64{5, 6, 7}; !slices.Equal(values["omega"], want) {
		t.Fatalf("peer values = %v, want %v", values["omega"], want)
	}
	if !slices.Equal(times["mon0"], []int64{baseMs}) {
		t.Fatalf("local timestamps = %v; a peer's readings must not affect this node's", times["mon0"])
	}
	if dropped := e.Stats().SamplesDropped; dropped != 0 {
		t.Errorf("SamplesDropped = %d; a peer behind this node's clock is skew, not a backwards step", dropped)
	}
}

// TestExporter_ClockSteppedBackwards: a source whose clock steps back offers
// readings older than ones already written. The receiver would refuse them as
// out of order, and would refuse the rest of their batch with them, so they
// are dropped and counted -- and the series resumes by itself once the clock
// passes the newest timestamp written, rather than staying stuck.
func TestExporter_ClockSteppedBackwards(t *testing.T) {
	stepped := []reading{
		{baseMs + 10_000, 1},
		{baseMs + 11_000, 2},
		{baseMs + 5_000, 3}, // the clock steps back six seconds
		{baseMs + 9_000, 4},
		{baseMs + 11_000, 5}, // a new collection level with the newest written: not newer
		{baseMs + 12_000, 6}, // past it: written again
		{baseMs + 13_000, 7},
	}
	wantTimes := []int64{baseMs + 10_000, baseMs + 11_000, baseMs + 12_000, baseMs + 13_000}
	wantValues := []float64{1, 2, 6, 7}

	check := func(t *testing.T, rec *recorder, e *Exporter, instance string) {
		t.Helper()
		times, values := delivered(t, rec)
		if !slices.Equal(times[instance], wantTimes) {
			t.Fatalf("delivered timestamps = %v, want %v", times[instance], wantTimes)
		}
		if !slices.Equal(values[instance], wantValues) {
			t.Fatalf("delivered values = %v, want %v", values[instance], wantValues)
		}
		if dropped := e.Stats().SamplesDropped; dropped != 3 {
			t.Errorf("SamplesDropped = %d, want the 3 readings not newer than the newest written", dropped)
		}
	}

	t.Run("this node", func(t *testing.T) {
		rec := newRecorder(t)
		o := fixedOptions(rec.srv.URL)
		e := NewExporter(func() Options { return o }, scripted(stepped), quietLogger())
		for range stepped {
			e.sample(o)
			// A flush between readings, so the older ones would have been
			// requests of their own had they been sent.
			e.flush(o)
		}
		check(t, rec, e, "mon0")
	})

	t.Run("a peer", func(t *testing.T) {
		rec := newRecorder(t)
		o := fixedOptions(rec.srv.URL)
		e := NewExporter(func() Options { return o }, scripted([]reading{{baseMs, 0}}), quietLogger())
		for _, r := range stepped {
			// Polled twice, as an aggregator polling faster than the peer
			// collects would: a dropped reading is still counted once.
			e.Submit(measure("omega", r))
			e.Submit(measure("omega", r))
			e.drain(o)
			e.flush(o)
		}
		check(t, rec, e, "omega")
	})
}

// TestExporter_UndatedReadingIsDropped: a reading with no collection time -- a
// peer that did not send one decodes as the zero time, far before 1970 -- has
// no truthful timestamp to be written at.
func TestExporter_UndatedReadingIsDropped(t *testing.T) {
	rec := newRecorder(t)
	o := fixedOptions(rec.srv.URL)
	e := NewExporter(func() Options { return o }, scripted([]reading{{baseMs, 1}}), quietLogger())

	e.Submit(measure("omega", reading{-62135596800000, 1}))
	e.Submit(measure("omega", reading{-62135596800000, 1}))
	e.Submit(measure("omega", reading{0, 1}))
	e.drain(o)
	e.flush(o)

	if times, _ := delivered(t, rec); len(times["omega"]) != 0 {
		t.Fatalf("undated readings were written at %v", times["omega"])
	}
	if dropped := e.Stats().SamplesDropped; dropped != 2 {
		t.Errorf("SamplesDropped = %d, want each run of undated readings counted once", dropped)
	}
}

// TestExporter_DroppedReadingIsCountedInSamples: SamplesDropped is published
// as a sample count, like SamplesWritten, so a dropped reading of three series
// is three, not one.
func TestExporter_DroppedReadingIsCountedInSamples(t *testing.T) {
	rec := newRecorder(t)
	o := fixedOptions(rec.srv.URL)
	three := func(timeMs int64) Measurement {
		return Measurement{TimeMs: timeMs, Write: func(b *Builder, timeMs int64) {
			for _, dev := range []string{"sda", "sdb", "sdc"} {
				b.Add("sysmon_disk_used_percent", 1, timeMs, "device", dev)
			}
		}}
	}
	readings := []Measurement{three(baseMs + 1000), three(baseMs)}
	i := 0
	e := NewExporter(func() Options { return o }, func() (Measurement, error) {
		m := readings[i]
		i++
		return m, nil
	}, quietLogger())

	e.sample(o)
	e.sample(o)
	e.flush(o)

	if st := e.Stats(); st.SamplesWritten != 3 || st.SamplesDropped != 3 {
		t.Fatalf("Stats() = %+v, want 3 written and the stepped-back reading's 3 samples dropped", st)
	}
}

// TestExporter_RetargetWritesTheCurrentReadingToTheNewReceiver: a new endpoint
// holds nothing of what the old one was sent, so the reading already written
// there is new to it and is written again rather than leaving it empty until
// the next collection.
func TestExporter_RetargetWritesTheCurrentReadingToTheNewReceiver(t *testing.T) {
	first, second := newRecorder(t), newRecorder(t)
	o := fixedOptions(first.srv.URL)
	e := NewExporter(func() Options { return o }, scripted([]reading{{baseMs, 1}}), quietLogger())

	e.sample(o)
	e.flush(o)
	o.RemoteWriteURL = second.srv.URL
	e.sample(o)
	e.flush(o)

	for name, rec := range map[string]*recorder{"first": first, "second": second} {
		if times, _ := delivered(t, rec); !slices.Equal(times["mon0"], []int64{baseMs}) {
			t.Errorf("%s receiver got timestamps %v, want the current reading once", name, times["mon0"])
		}
	}
}
