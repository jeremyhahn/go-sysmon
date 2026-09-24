package prom

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"
)

// simLookback is the lookback of the simulated Prometheus: an evaluation sees a
// series only if it has a sample this long or less before the instant. Five
// minutes is Prometheus' default; the tests that need another say so.
const simLookback = 5 * time.Minute

// lookbackMs is simLookback in milliseconds.
const lookbackMs = int64(simLookback / time.Millisecond)

// writer is one exporter writing one series the way #173 made it: every tick
// it looks at the newest reading, and writes it -- at the reading's own
// collection time -- only if it has not written that reading already.
type writer struct {
	tickMs, collectMs int64
	// jitterMs is how late, at most, a collection lands after its nominal
	// time. Collections are real work and never land exactly on schedule.
	jitterMs int64
	// outage, when set, is a span with no collections at all: the node was
	// down.
	outageFrom, outageTo int64
}

// collectedAt is the time of collection k, deterministically jittered.
func (w writer) collectedAt(k int64) int64 {
	return k*w.collectMs + (k*7919)%(w.jitterMs+1)
}

// newest is the time of the newest collection at or before t, or -1.
func (w writer) newest(t int64) int64 {
	for k := t / w.collectMs; k >= 0; k-- {
		at := w.collectedAt(k)
		if at > t {
			continue
		}
		if w.outageTo > w.outageFrom && at >= w.outageFrom && at < w.outageTo {
			// Nothing was collected while the node was down; the newest
			// reading is the last one before the outage.
			k = w.outageFrom / w.collectMs
			continue
		}
		return at
	}
	return -1
}

// write returns the sample times the writer stores between 0 and endMs.
func (w writer) write(endMs int64) []int64 {
	var out []int64
	last := int64(-1)
	for at := int64(0); at <= endMs; at += w.tickMs {
		reading := w.newest(at)
		if reading > last {
			out = append(out, reading)
			last = reading
		}
	}
	return out
}

// evalTimestamp is timestamp(series) evaluated by a range query, the way
// Prometheus does it: at each point, the newest sample at or before it, if
// that sample is inside the lookback.
func evalTimestamp(samples []int64, startMs, endMs, stepMs int64) []RangeSeries {
	points := make([]Point, 0, (endMs-startMs)/stepMs+1)
	for t := startMs; t <= endMs; t += stepMs {
		i := sort.Search(len(samples), func(i int) bool { return samples[i] > t }) - 1
		if i < 0 || t-samples[i] >= lookbackMs {
			continue
		}
		points = append(points, Point{TimeMs: t, Value: float64(samples[i]) / 1000})
	}
	return []RangeSeries{{Points: points}}
}

// measureGap is SampleGap against simulated samples: timestamp() evaluated
// from fromMs to endMs at the finest resolution the span allows, aligned on
// the end exactly as SampleGap aligns it.
func measureGap(samples []int64, fromMs, endMs int64) time.Duration {
	resolution := gapResolution(time.Duration(endMs-fromMs) * time.Millisecond)
	from := alignedStart(time.UnixMilli(fromMs), time.UnixMilli(endMs), resolution).UnixMilli()
	return longestGap(evalTimestamp(samples, from, endMs, resolution.Milliseconds()), resolution, endMs)
}

// countIn counts the sorted samples in (from, to]: the samples a Prometheus
// range selector ending at `to` would read.
func countIn(samples []int64, from, to int64) int {
	lo := sort.Search(len(samples), func(i int) bool { return samples[i] > from })
	hi := sort.Search(len(samples), func(i int) bool { return samples[i] > to })
	return hi - lo
}

// realGap is the largest interval between consecutive samples in [from, to],
// ignoring intervals longer than the lookback: those are outages.
func realGap(samples []int64, from, to int64) int64 {
	var gap int64
	for i := 1; i < len(samples); i++ {
		if samples[i-1] < from || samples[i] > to {
			continue
		}
		if d := samples[i] - samples[i-1]; d < lookbackMs {
			gap = max(gap, d)
		}
	}
	return gap
}

// TestRateWindow_EverySampleWindowHoldsTwoSamples is the guarantee the rate
// window exists for, checked end to end against the samples a writer actually
// stores: at every point of every range the dashboard offers, the window holds
// the two samples rate() needs.
//
// The writers are the ones #173 left: this node's exporter ticking at its
// sample interval, a peer relayed by the fleet poller at its poll interval, and
// a slow exporter -- each over every collection interval the dashboard offers,
// up to 60s, with collections landing late by a jittered amount. The steps are
// the ones the server chooses for each preset range, and one second, the
// tightest it ever chooses. Nothing about the cadence is told to the code under
// test: it measures the gap from the evaluated timestamps, exactly as it does
// against a real Prometheus.
func TestRateWindow_EverySampleWindowHoldsTwoSamples(t *testing.T) {
	ms := func(d time.Duration) int64 { return int64(d / time.Millisecond) }

	ticks := map[string]time.Duration{
		"local exporter at the default 15s": 15 * time.Second,
		"peer relayed by a 10s fleet poll":  10 * time.Second,
		"a slow 90s exporter":               90 * time.Second,
	}
	// The dashboard's intervals, pkg/server's allowedIntervals.
	collections := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second,
		5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}
	// The UI's presets (RANGE_PRESETS), each at the step the server picks,
	// and a one-minute custom range at the one-second floor.
	ranges := []time.Duration{time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour,
		24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

	for name, tick := range ticks {
		for _, collection := range collections {
			w := writer{tickMs: ms(tick), collectMs: ms(collection), jitterMs: ms(collection) / 10}
			for _, window := range ranges {
				step := StepFor(window)
				// The series has been running for an hour before the range,
				// so the first window has history behind it, as it would.
				startMs := ms(time.Hour)
				endMs := startMs + ms(window)
				samples := w.write(endMs)

				// Exactly what RateGap does against a real Prometheus: measure
				// timestamp() over the range reaching back the extent it asks
				// for, at the finest resolution that span allows.
				gap, err := rateGap(step, simLookback, 24*time.Hour, func(extent time.Duration) (time.Duration, error) {
					return measureGap(samples, startMs-ms(extent), endMs), nil
				})
				if err != nil {
					t.Fatalf("%s, collection %v, range %v: rateGap = %v", name, collection, window, err)
				}

				if real := realGap(samples, startMs, endMs); ms(gap) < real {
					t.Fatalf("%s, collection %v, range %v: measured gap %v under the real %dms",
						name, collection, window, gap, real)
				}
				rw, err := RateWindow(step, gap)
				if err != nil {
					t.Fatalf("%s, collection %v, range %v: RateWindow(%v, %v) = %v",
						name, collection, window, step, gap, err)
				}
				for at := startMs; at <= endMs; at += ms(step) {
					if in := countIn(samples, at-ms(rw), at); in < 2 {
						t.Fatalf("%s, collection %v, range %v at step %v: window %v at %dms holds %d sample(s)",
							name, collection, window, step, rw, at, in)
					}
				}
			}
		}
	}
}

// TestLongestGap_AnOutageIsNotACadence. A node that was down for two hours
// left a two-hour hole. Measuring across it would size every window in the
// chart at four hours and smear the whole range flat; it is an outage, and the
// window stays sized for the cadence either side of it.
func TestLongestGap_AnOutageIsNotACadence(t *testing.T) {
	ms := func(d time.Duration) int64 { return int64(d / time.Millisecond) }
	w := writer{tickMs: ms(15 * time.Second), collectMs: ms(time.Minute), jitterMs: 500,
		outageFrom: ms(3 * time.Hour), outageTo: ms(5 * time.Hour)}
	startMs, endMs := ms(time.Hour), ms(7*time.Hour)
	samples := w.write(endMs)

	gap := measureGap(samples, startMs, endMs)
	if gap < time.Minute || gap > time.Minute+2*gapResolution(6*time.Hour) {
		t.Fatalf("gap across a two-hour outage = %v, want about the one-minute cadence", gap)
	}
}

func TestLongestGap(t *testing.T) {
	pts := func(pairs ...[2]float64) RangeSeries {
		s := RangeSeries{}
		for _, p := range pairs {
			s.Points = append(s.Points, Point{TimeMs: int64(p[0]), Value: p[1]})
		}
		return s
	}
	cases := []struct {
		name   string
		series []RangeSeries
		want   time.Duration
	}{
		{"nothing to measure", nil, 0},
		// A series seen at all may hide two samples inside one evaluation
		// step, so it has at least a resolution of gap to allow for.
		{"one point", []RangeSeries{pts([2]float64{0, 10})}, time.Second},
		{"the same sample seen twice", []RangeSeries{pts([2]float64{0, 10}, [2]float64{1000, 10})}, time.Second},
		{"adjacent points see the next sample",
			[]RangeSeries{pts([2]float64{0, 10}, [2]float64{1000, 10}, [2]float64{2000, 70})}, time.Minute},
		{"millisecond sample times survive the seconds encoding",
			[]RangeSeries{pts([2]float64{0, 10.25}, [2]float64{1000, 25.5})}, 15250 * time.Millisecond},
		{"points either side of a missing one are an outage, not a gap",
			[]RangeSeries{pts([2]float64{0, 10}, [2]float64{3000, 7210})}, time.Second},
		{"the largest across every series",
			[]RangeSeries{
				pts([2]float64{0, 10}, [2]float64{1000, 25}),
				pts([2]float64{0, 10}, [2]float64{1000, 40}),
			}, 30 * time.Second},
		{"a point one millisecond off the step is still adjacent",
			[]RangeSeries{pts([2]float64{0, 10}, [2]float64{1001, 20})}, 10 * time.Second},
	}
	for _, c := range cases {
		if got := longestGap(c.series, time.Second, 0); got != c.want {
			t.Errorf("%s: longestGap = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGapResolution(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		0:                             time.Second,
		15 * time.Minute:              time.Second,
		time.Hour:                     time.Second,
		24 * time.Hour:                7856 * time.Millisecond,
		30 * 24 * time.Hour:           235658 * time.Millisecond,
		(maxPoints - 1) * time.Second: time.Second,
		(maxPoints-1)*time.Second + 1: time.Second + time.Millisecond,
		90 * 24 * time.Hour:           706974 * time.Millisecond,
		time.Duration(maxPoints):      time.Second,
	}
	for window, want := range cases {
		got := gapResolution(window)
		if got != want {
			t.Errorf("gapResolution(%v) = %v, want %v", window, got, want)
		}
		// The whole point: QueryRange must accept it.
		if window > 0 {
			if points := int(window/got) + 1; points > maxPoints {
				t.Errorf("gapResolution(%v) = %v needs %d points, over %d", window, got, points, maxPoints)
			}
		}
	}
}

// TestQuerier_SampleGap reads the timestamps back through a real HTTP
// round trip, and asks for them at the finest resolution the range allows.
func TestQuerier_SampleGap(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"instance":"mon0"},"values":[[1788300000,"1788299990"],[1788300001,"1788299990"],[1788300002,"1788300050"]]}
	]}}`
	srv, seen := promServer(t, http.StatusOK, body)
	q := querier(srv.URL, 5*time.Second)

	start := time.UnixMilli(1788300000000)
	gap, err := q.SampleGap(context.Background(), `x_total{instance="mon0"}`, start, start.Add(15*time.Minute))
	if err != nil {
		t.Fatalf("SampleGap() = %v", err)
	}
	if gap != time.Minute {
		t.Errorf("SampleGap() = %v, want 1m", gap)
	}
	if got := seen.Get("query"); got != `timestamp(x_total{instance="mon0"})` {
		t.Errorf("query = %q", got)
	}
	if got := seen.Get("step"); got != strconv.Itoa(1) {
		t.Errorf("step = %q, want the one-second floor", got)
	}
}

func TestQuerier_SampleGap_PassesFailuresThrough(t *testing.T) {
	srv, _ := promServer(t, http.StatusBadRequest,
		`{"status":"error","errorType":"bad_data","error":"parse error"}`)
	q := querier(srv.URL, 5*time.Second)
	start := time.UnixMilli(1788300000000)

	var failed *ErrQuery
	if _, err := q.SampleGap(context.Background(), `x{instance="mon0"}`, start, start.Add(time.Hour)); !errors.As(err, &failed) {
		t.Fatalf("SampleGap() = %v, want *ErrQuery", err)
	}
	var none *ErrNoQueryEndpoint
	if _, err := querier("", time.Second).SampleGap(context.Background(), "x", start, start.Add(time.Hour)); !errors.As(err, &none) {
		t.Fatalf("SampleGap() with no endpoint = %v, want *ErrNoQueryEndpoint", err)
	}
}

// TestRateGap_ReachesBackOverTheFirstWindow. The point at a range's start
// reads its window from before the range, so the measurement has to cover
// that too -- step plus two lookbacks -- and widen further, and measure again,
// when the gap it finds needs a wider window than that.
func TestRateGap_ReachesBackOverTheFirstWindow(t *testing.T) {
	var asked []time.Duration
	gap, err := rateGap(time.Minute, simLookback, 90*24*time.Hour, func(extent time.Duration) (time.Duration, error) {
		asked = append(asked, extent)
		return time.Minute, nil
	})
	if err != nil || gap != time.Minute {
		t.Fatalf("rateGap = %v, %v; want 1m", gap, err)
	}
	if len(asked) != 1 || asked[0] != time.Minute+2*simLookback {
		t.Fatalf("measured over %v, want one pass reaching back step + two lookbacks", asked)
	}

	// A deployment with a longer lookback lets a longer gap be measured,
	// whose window is wider than the first extent: measure again over it.
	asked = nil
	gap, err = rateGap(time.Second, simLookback, 90*24*time.Hour, func(extent time.Duration) (time.Duration, error) {
		asked = append(asked, extent)
		return 20 * time.Minute, nil
	})
	if err != nil || gap != 20*time.Minute {
		t.Fatalf("rateGap = %v, %v; want 20m", gap, err)
	}
	if len(asked) != 2 || asked[1] != 40*time.Minute {
		t.Fatalf("measured over %v, want a second pass over the 40m window", asked)
	}
}

func TestRateGap_RejectsWhatItCannotMeasure(t *testing.T) {
	never := func(time.Duration) (time.Duration, error) {
		t.Fatal("measured a span that should have been refused")
		return 0, nil
	}
	var bad *ErrInvalidRange
	for _, c := range []struct {
		name         string
		step, budget time.Duration
	}{
		{"zero step", 0, time.Hour},
		{"a step that overflows the extent", math.MaxInt64, time.Hour},
		{"a step that overflows only with the lookback", math.MaxInt64 - time.Minute, time.Hour},
		{"an extent past the budget", time.Hour, time.Hour},
	} {
		if _, err := rateGap(c.step, simLookback, c.budget, never); !errors.As(err, &bad) {
			t.Errorf("%s: rateGap = %v, want *ErrInvalidRange", c.name, err)
		}
	}

	// Nothing measurable is an answer, not an error: there is no rate to
	// draw, and the caller says so with an empty range.
	gap, err := rateGap(time.Second, simLookback, time.Hour, func(time.Duration) (time.Duration, error) { return 0, nil })
	if err != nil || gap != 0 {
		t.Errorf("rateGap over nothing = %v, %v; want 0, nil", gap, err)
	}
	// A failed measurement is passed through untouched.
	failure := &ErrQuery{Query: "q", Type: "bad_data"}
	if _, err := rateGap(time.Second, simLookback, time.Hour, func(time.Duration) (time.Duration, error) { return 0, failure }); !errors.Is(err, failure) {
		t.Errorf("rateGap failure = %v, want the measurement's error", err)
	}
}

// TestLongestGap_CountsTheOpenGapAtTheEdge. At the end of a range reaching
// now, the newest sample is 100s old and the closed gaps are a minute; a
// window of two minutes would hold only that sample. The open gap is counted,
// one millisecond longer than it has lasted: no sample landed at the edge, so
// it is strictly longer, and a left-open window of exactly twice it would
// leave out the sample before the newest.
func TestLongestGap_CountsTheOpenGapAtTheEdge(t *testing.T) {
	// Evaluated every 100s.
	series := []RangeSeries{{Points: []Point{
		{TimeMs: 860_000, Value: 900},   // sample at 900s
		{TimeMs: 960_000, Value: 960},   // the next, a minute later
		{TimeMs: 1_060_000, Value: 960}, // the last point, at the edge: nothing newer
	}}}
	if got := longestGap(series, 100*time.Second, 1_060_000); got != 100*time.Second+time.Millisecond {
		t.Errorf("longestGap at an edge 100s past the newest sample = %v, want 100.001s", got)
	}
	// A series with no value at the last point has stopped; it has no rate
	// at the edge, so its age is not a gap.
	if got := longestGap(series, 100*time.Second, 1_070_000); got != 100*time.Second {
		t.Errorf("longestGap for a series that stopped = %v, want the 100s resolution, not its 110s age", got)
	}
}

// TestRateWindow_ACadenceChangeJustBeforeTheRange. Samples arrived once a
// minute until one landed a second before the range began, and every 15s
// after it -- the dashboard moved from 60s collection to 1s. The window at
// the range's start holds that last slow sample and must reach the one a
// minute before it; but that minute is entirely before the range, and every
// gap inside the range is 15s. Measured over the range alone the window is
// 30s and holds one sample. Measuring back over the first window is what
// finds the minute.
func TestRateWindow_ACadenceChangeJustBeforeTheRange(t *testing.T) {
	ms := func(d time.Duration) int64 { return int64(d / time.Millisecond) }
	startMs := ms(time.Hour)
	last := startMs - 1000

	var samples []int64
	for at := last - 30*60_000; at <= last; at += 60_000 {
		samples = append(samples, at)
	}
	for _, window := range []time.Duration{time.Minute, 15 * time.Minute} {
		endMs := startMs + ms(window)
		all := append([]int64(nil), samples...)
		for at := last + 15_000; at <= endMs; at += 15_000 {
			all = append(all, at)
		}

		step := StepFor(window)
		gap, err := rateGap(step, simLookback, 24*time.Hour, func(extent time.Duration) (time.Duration, error) {
			return measureGap(all, startMs-ms(extent), endMs), nil
		})
		if err != nil {
			t.Fatalf("range %v: rateGap = %v", window, err)
		}
		rw, err := RateWindow(step, gap)
		if err != nil {
			t.Fatal(err)
		}
		for at := startMs; at <= endMs; at += ms(step) {
			if in := countIn(all, at-ms(rw), at); in < 2 {
				t.Fatalf("range %v: window %v at %dms into the range holds %d sample(s)",
					window, rw, at-startMs, in)
			}
		}
	}
}

// TestRateWindow_TheEdgeSampleExactlyOneGapOld. The live edge is one closed
// gap past the newest sample, and the one before it one gap further back:
// samples at t-gap and t-2*gap. A window of exactly 2*gap is (t-2*gap, t] and
// holds only the newer. The open gap counted one millisecond long keeps both.
func TestRateWindow_TheEdgeSampleExactlyOneGapOld(t *testing.T) {
	const gapMs = int64(60_000)
	endMs := int64(10 * gapMs)
	samples := []int64{endMs - 3*gapMs, endMs - 2*gapMs, endMs - gapMs}

	gap := measureGap(samples, endMs-5*gapMs, endMs)
	rw, err := RateWindow(time.Second, gap)
	if err != nil {
		t.Fatal(err)
	}
	if in := countIn(samples, endMs-int64(rw/time.Millisecond), endMs); in < 2 {
		t.Fatalf("window %v at the edge holds %d sample(s), want 2", rw, in)
	}
}

// TestLongestGap_AStoppedSeriesIsNotAnOpenGap. The last evaluation had a
// value, but not one AT the end: the series was already past the lookback
// there. Its age is an outage, not a cadence, and must not size the windows.
func TestLongestGap_AStoppedSeriesIsNotAnOpenGap(t *testing.T) {
	series := []RangeSeries{{Points: []Point{
		{TimeMs: 999_000, Value: 900},
		{TimeMs: 1_000_000, Value: 960},
	}}}
	// One resolution short of the end, as an unaligned range would leave it.
	if got := longestGap(series, time.Second, 1_001_000); got != time.Minute {
		t.Errorf("longestGap = %v, want the closed 1m gap: a series absent at the end has no open gap", got)
	}
}

// TestQuerier_SampleGap_AlignsAnEvaluationOnTheEnd. Whether a series is still
// there at the end decides whether its open gap counts, so the measurement's
// last evaluation must land on the end, whatever the span.
func TestQuerier_SampleGap_AlignsAnEvaluationOnTheEnd(t *testing.T) {
	srv, seen := promServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	q := querier(srv.URL, 5*time.Second)

	end := time.UnixMilli(1788300000000)
	// 24h and 500ms: not a whole number of the 7.856s resolution.
	start := end.Add(-(24*time.Hour + 500*time.Millisecond))
	if _, err := q.SampleGap(context.Background(), `x{instance="mon0"}`, start, end); err != nil {
		t.Fatal(err)
	}
	from, err := strconv.ParseFloat(seen.Get("start"), 64)
	if err != nil {
		t.Fatal(err)
	}
	step, err := strconv.ParseFloat(seen.Get("step"), 64)
	if err != nil {
		t.Fatal(err)
	}
	fromMs, stepMs := int64(from*1000+0.5), int64(step*1000+0.5)
	if (end.UnixMilli()-fromMs)%stepMs != 0 {
		t.Errorf("measured from %d at %dms steps: no evaluation lands on the end %d", fromMs, stepMs, end.UnixMilli())
	}
	if fromMs > start.UnixMilli() {
		t.Errorf("measured from %d, after the requested %d: the start was moved forward", fromMs, start.UnixMilli())
	}
	if points := (end.UnixMilli()-fromMs)/stepMs + 1; points > maxPoints {
		t.Errorf("alignment made %d points, over %d", points, maxPoints)
	}
}

// TestLongestGap_ALoneSampleAfterAnOutageIsNoCadence. Samples at 0 and at
// 6m -- an outage longer than the lookback between them -- and the range ends
// at 10m. The one sample since the outage says nothing about cadence yet;
// counting its four-minute age would size an eight-minute window, and the
// point at 6m would take a rate across the outage.
func TestLongestGap_ALoneSampleAfterAnOutageIsNoCadence(t *testing.T) {
	samples := []int64{0, 360_000}
	endMs := int64(600_000)
	if got, resolution := measureGap(samples, 0, endMs), gapResolution(10*time.Minute); got != resolution {
		t.Errorf("gap = %v, want only the %v resolution: the lone sample's age is no cadence", got, resolution)
	}

	// A second sample after the outage makes it a cadence again, and the
	// open gap at the edge counts from then on.
	samples = append(samples, 420_000)
	if got := measureGap(samples, 0, endMs); got != 180*time.Second+time.Millisecond {
		t.Errorf("gap = %v, want the 180.001s open gap after a 60s pair", got)
	}
}

// TestRateGap_MeasuresBackOverTheDatabasesLookback. A database with its
// lookback raised to 30 minutes keeps a series sampled every 20 minutes
// present, so it is a cadence rather than an outage there -- and a one-minute
// range between two of its samples sees only one. The first measurement has to
// reach back far enough to see the one before: two of THIS database's
// lookbacks, not two of the default's.
func TestRateGap_MeasuresBackOverTheDatabasesLookback(t *testing.T) {
	var asked []time.Duration
	_, err := rateGap(time.Second, 30*time.Minute, 90*24*time.Hour, func(extent time.Duration) (time.Duration, error) {
		asked = append(asked, extent)
		return 20 * time.Minute, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked[0] != time.Second+time.Hour {
		t.Fatalf("first measurement reached back %v, want a step and two 30m lookbacks", asked[0])
	}
	if len(asked) != 1 {
		t.Fatalf("measured %d times; a window of 40m fits inside the first extent", len(asked))
	}
}

// TestLongestGap_AJumpBetweenPresentPointsIsAGap. Between two evaluations that
// both have a value, a jump in sample time cannot tell an outage from healthy
// samples stepped over: samples at 0s, 299s and 301s, evaluated at 296s and
// 304s, show as a 301s jump -- past a 5m lookback -- where the real gaps are
// 299s and 2s. It is measured as a gap, which can only overstate the real
// one: a window wide enough, never one too narrow to hold two samples.
func TestLongestGap_AJumpBetweenPresentPointsIsAGap(t *testing.T) {
	series := []RangeSeries{{Points: []Point{
		{TimeMs: 288_000, Value: 0},
		{TimeMs: 296_000, Value: 0},
		{TimeMs: 304_000, Value: 301},
	}}}
	got := longestGap(series, 8*time.Second, 0)
	if got < 299*time.Second {
		t.Fatalf("longestGap = %v, under the real 299s gap: a window sized from it would drop samples", got)
	}
}

// TestLongestGap_SkippedSamplesAtACoarseResolutionAreNotAnOutage. A 60-day
// range is measured at about 471s, past the five-minute lookback. Adjacent
// evaluations of a healthy 15s series see sample times ~471s apart: the
// samples between were skipped, not missing, and the range must still be
// measured rather than written off as one long outage.
func TestLongestGap_SkippedSamplesAtACoarseResolutionAreNotAnOutage(t *testing.T) {
	ms := func(d time.Duration) int64 { return int64(d / time.Millisecond) }
	var samples []int64
	endMs := ms(61 * 24 * time.Hour)
	for at := int64(0); at <= endMs; at += 15_000 {
		samples = append(samples, at)
	}
	gap := measureGap(samples, ms(24*time.Hour), endMs)
	resolution := gapResolution(60 * 24 * time.Hour)
	// At most one resolution plus the real 15s gap: adjacent evaluations see
	// samples no further apart than that.
	if gap == 0 || gap > resolution+15*time.Second {
		t.Fatalf("60-day gap = %v, want at most the %v resolution plus 15s: measurable, not an outage", gap, resolution)
	}
}

// TestRateWindow_ACounterBornInTheLastStepOfAMonth. Measured at the ~236s a
// 30-day range allows, a counter that began 100s before the end shows as one
// point: the evaluation after its samples sees only the newest. It still has
// a rate -- the 72-minute window the chart's step asks for holds every sample
// -- so it must not be measured as nothing.
func TestRateWindow_ACounterBornInTheLastStepOfAMonth(t *testing.T) {
	ms := func(d time.Duration) int64 { return int64(d / time.Millisecond) }
	startMs := ms(time.Hour)
	window := 30 * 24 * time.Hour
	endMs := startMs + ms(window)
	var samples []int64
	for at := endMs - 100_000; at <= endMs; at += 15_000 {
		samples = append(samples, at)
	}

	step := StepFor(window)
	gap, err := rateGap(step, simLookback, 24*time.Hour, func(extent time.Duration) (time.Duration, error) {
		return measureGap(samples, startMs-ms(extent), endMs), nil
	})
	if err != nil || gap == 0 {
		t.Fatalf("rateGap = %v, %v; want a gap to size a window from", gap, err)
	}
	rw, err := RateWindow(step, gap)
	if err != nil {
		t.Fatal(err)
	}
	if in := countIn(samples, endMs-ms(rw), endMs); in < 2 {
		t.Fatalf("window %v at the end holds %d samples, want them all", rw, in)
	}
}
