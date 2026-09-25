package prom

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
)

// maxPoints is the ceiling Prometheus itself puts on a range query's resolution
// (11000 points). Asking for more is rejected outright, so the step is checked
// here where the message can say what to do about it.
const maxPoints = 11000

// TargetPoints is the resolution a range is drawn at when the caller does not
// choose one. It is a chart width, not a data-fidelity figure: more points than
// a few hundred cost bytes and draw time without adding anything a reader can
// see.
const TargetPoints = 600

// responseBodyLimit bounds a query response. A range at maximum resolution is a
// few megabytes; anything far past that is not this API answering.
const responseBodyLimit = 32 << 20

// Point is one instant of a series.
type Point struct {
	TimeMs int64   `json:"t"`
	Value  float64 `json:"v"`
}

// RangeSeries is one series returned by a range query, with the labels that
// distinguish it from its siblings -- the device, the interface, the GPU index.
type RangeSeries struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// QueryOptions is where and how to read history. Like the exporter's Options
// it is re-read per request, so the address can be changed by reloading the
// config file rather than restarting.
type QueryOptions struct {
	// BaseURL is the address of a Prometheus server, e.g. http://mon0:9090 --
	// the server, not one of its endpoints; /api/v1/query_range is appended
	// here. Empty means history is not available from this node.
	BaseURL string
	// Timeout bounds one query.
	Timeout time.Duration
}

// Querier reads history back out of Prometheus.
//
// One http.Client is kept for the life of the Querier so connections are
// pooled across queries, and each request carries its own deadline rather than
// the client carrying a fixed one -- which is what lets the timeout be
// reconfigured without discarding the pool.
type Querier struct {
	opts   func() QueryOptions
	client *http.Client
	// lookbacks caches each database's query lookback; see lookback.go. A
	// pointer so a pinned querier shares it.
	lookbacks *lookbackCache
	// now is the clock the cache is read against, replaceable by a test.
	now func() time.Time
	// lookbackTTL and lookbackRetry are how long a lookback answer, and a
	// failure to get one, are cached; see WithLookbackCache.
	lookbackTTL, lookbackRetry time.Duration
	// log receives what cannot be returned: a response body that failed to
	// drain after the answer was already decoded. See WithLogger.
	log *slog.Logger
}

// QuerierOption adjusts a Querier as it is built.
type QuerierOption func(*Querier)

// WithLookbackCache sets how long a database's query lookback is cached once
// read (ttl), and how soon a failure to read it is retried (retry). The
// defaults are ten minutes and thirty seconds; see lookback.go.
func WithLookbackCache(ttl, retry time.Duration) QuerierOption {
	return func(q *Querier) {
		q.lookbackTTL, q.lookbackRetry = ttl, retry
	}
}

// WithLogger sets where the querier logs what it cannot return. The default is
// slog.Default() at the time the querier is built.
func WithLogger(log *slog.Logger) QuerierOption {
	return func(q *Querier) { q.log = log }
}

// NewQuerier returns a Querier reading its settings from opts on every query.
func NewQuerier(opts func() QueryOptions, options ...QuerierOption) *Querier {
	// Same policy as every other outbound client: a query API that redirects
	// off the configured host would have this node drawing another
	// deployment's history and labelling it as its own.
	q := &Querier{
		opts: opts, client: outbound.NewClient(0), lookbacks: &lookbackCache{}, now: time.Now,
		lookbackTTL: lookbackTTL, lookbackRetry: lookbackRetry, log: slog.Default(),
	}
	for _, option := range options {
		option(q)
	}
	return q
}

// Enabled reports whether a Prometheus is currently configured to read from.
func (q *Querier) Enabled() bool { return q.opts().BaseURL != "" }

// Pinned returns a querier bound to the options in force now, sharing this
// one's connections and lookback cache.
//
// A read that takes several requests -- a counter's lookback, gap and rate --
// must make them all against one database. The options are re-read per
// request so a reload retargets the querier; mid-read, that would let the gap
// measured on one database size a query sent to another.
func (q *Querier) Pinned() *Querier {
	o := q.opts()
	return &Querier{
		opts: func() QueryOptions { return o }, client: q.client, lookbacks: q.lookbacks, now: q.now,
		lookbackTTL: q.lookbackTTL, lookbackRetry: q.lookbackRetry, log: q.log,
	}
}

// Timeout is how long one read of history may take, as configured.
func (q *Querier) Timeout() time.Duration { return q.opts().Timeout }

// URL returns the Prometheus base URL this querier reads from.
func (q *Querier) URL() string { return strings.TrimRight(q.opts().BaseURL, "/") }

// queryResponse is the documented shape of a query_range answer. Values are
// [unixSeconds, "stringifiedFloat"] pairs, which is why the sample type is
// json.RawMessage rather than a struct: the pair is heterogeneous.
type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// QueryRange runs a range query and returns its series.
//
// A query that matched nothing is not an error: a node that was switched off
// for the window asked about genuinely has no data, and the caller has to be
// able to tell that apart from a failure. It comes back as an empty slice.
func (q *Querier) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]RangeSeries, error) {
	o := q.opts()
	if o.BaseURL == "" {
		return nil, &NoQueryEndpointError{}
	}
	if err := ValidateRange(start, end, step); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("query", expr)
	params.Set("start", formatTime(start))
	params.Set("end", formatTime(end))
	params.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	endpoint := strings.TrimRight(o.BaseURL, "/") + "/api/v1/query_range"

	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), http.NoBody)
	if err != nil {
		return nil, &RequestError{URL: endpoint, Err: err}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, &RequestError{URL: endpoint, Err: err}
	}
	defer releaseBody(resp.Body, endpoint, q.log)

	// Prometheus reports a bad query as 400 with the same JSON envelope as a
	// success, so the body is decoded before the status is judged: the envelope
	// says what is wrong, and the status alone does not.
	var parsed queryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseBodyLimit)).Decode(&parsed); err != nil {
		return nil, &DecodeError{URL: endpoint, Err: err}
	}
	if parsed.Status != "success" {
		return nil, &QueryError{Query: expr, Type: parsed.ErrorType, Detail: parsed.Error}
	}
	// The envelope is decoded first but it is not the last word. A body saying
	// "success" under a 5xx did not come from a Prometheus that answered the
	// query -- it came from something in front of it, or from a Prometheus
	// that failed after starting the response -- and drawing a chart from it
	// presents whatever it contained, or an empty range, as this node's past.
	// The envelope explains a 4xx; it cannot make a 5xx into an answer.
	if resp.StatusCode/100 != 2 {
		return nil, &QueryError{
			Query: expr,
			Type:  "http_" + strconv.Itoa(resp.StatusCode),
			Detail: "the endpoint answered " + strconv.Itoa(resp.StatusCode) +
				" with a body claiming success",
		}
	}
	// A range query returns a matrix. Anything else is a different question's
	// answer -- an instant query's vector, most likely a proxy rewriting the
	// path -- and its result decodes into this shape as an empty series list,
	// which draws as "no history" rather than as the error it is.
	if parsed.Data.ResultType != "matrix" {
		return nil, &DecodeError{
			URL: endpoint,
			Err: &UnexpectedResultTypeError{Got: parsed.Data.ResultType},
		}
	}

	out := make([]RangeSeries, 0, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		series := RangeSeries{Labels: r.Metric, Points: make([]Point, 0, len(r.Values))}
		for _, v := range r.Values {
			p, ok := decodePoint(v)
			if !ok {
				// A NaN or a malformed pair is a gap, not a failure: dropping
				// the point leaves a hole in the line, which is what actually
				// happened, whereas failing the request would blank a whole
				// chart because one scrape was missing.
				continue
			}
			series.Points = append(series.Points, p)
		}
		out = append(out, series)
	}
	return out, nil
}

// ValidateRange reports whether Prometheus will answer a range query over
// start to end at step, as an *InvalidRangeError saying what to change if not.
// QueryRange checks it; a caller that queries anything else first -- a
// counter's gap measurement -- checks it before, so a range that would be
// refused is refused before any work is done for it.
func ValidateRange(start, end time.Time, step time.Duration) error {
	if !end.After(start) {
		return &InvalidRangeError{Message: "end must be after start"}
	}
	if step <= 0 {
		return &InvalidRangeError{Message: "step must be greater than zero"}
	}
	// Points, not intervals. A range query returns a sample at start, at every
	// step, AND at end -- so t=0 to t=10s at 1s is eleven points, not ten.
	// Counting intervals let a request one point over the ceiling through, and
	// Prometheus rejected it, which reached the dashboard as a 502 from a range
	// this server had already decided was fine.
	if points := int(end.Sub(start)/step) + 1; points > maxPoints {
		return &InvalidRangeError{
			Message: "the range needs " + strconv.Itoa(points) + " points at this step; Prometheus returns at most " +
				strconv.Itoa(maxPoints) + ", so widen the step or narrow the range",
		}
	}
	return nil
}

// decodePoint reads one [timestamp, "value"] pair.
func decodePoint(v [2]json.RawMessage) (Point, bool) {
	var secs float64
	if err := json.Unmarshal(v[0], &secs); err != nil {
		return Point{}, false
	}
	var raw string
	if err := json.Unmarshal(v[1], &raw); err != nil {
		return Point{}, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return Point{}, false
	}
	// Prometheus renders a missing or undefined value as the strings "NaN",
	// "+Inf" and "-Inf", all of which ParseFloat accepts. They must not travel
	// any further: encoding/json refuses to marshal a non-finite float, so one
	// of them reaching the response would fail the whole request -- a chart
	// blanked by an error because one scrape was missing.
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Point{}, false
	}
	return Point{TimeMs: int64(secs * 1000), Value: value}, true
}

// formatTime renders an instant the way the query API expects: Unix seconds
// with sub-second precision.
func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/float64(time.Second), 'f', 3, 64)
}

// StepFor picks a resolution for a window, rounded up to whole seconds because
// Prometheus steps below a second are meaningless against data written every
// few seconds.
func StepFor(window time.Duration) time.Duration {
	if window <= 0 {
		return time.Second
	}
	step := window / TargetPoints
	if step < time.Second {
		return time.Second
	}
	return step.Round(time.Second)
}

// RateGap is the sample gap a counter's rate window at step is sized from,
// measured over enough of the series to cover every window of the range.
//
// A window at a chart point reads back RateWindow(step, gap) before it, so the
// point at start reads samples from before the range. Measuring over the range
// alone misses them -- a one-minute range over a once-a-minute series holds at
// most one gap, often only part of one -- and the first windows come out too
// narrow to hold two samples. So the measurement reaches back an extent E
// first: step plus two lookbacks, which covers any window whose gap is within
// the lookback, and a gap is (SampleGap treats a longer hole as an outage). The
// lookback is the database's own (Querier.Lookback), so a deployment that has
// raised it is measured over enough to see its slower cadences. Should the
// window come out wider than E all the same, the span is widened to the
// window and measured again, until it covers it.
//
// budget is how far before start, at most, the caller allows the measurement
// and the rate query to read; a span it would need beyond that is refused as
// an invalid range, the same bound the rate query itself is held to.
func (q *Querier) RateGap(ctx context.Context, selector string, start, end time.Time, step, budget time.Duration) (time.Duration, error) {
	// Decidable without the database: the measurement reaches back more
	// than a step, so a step the budget cannot hold is refused as the
	// caller's mistake before anything upstream is asked -- including
	// whether the database publishes a lookback at all.
	if step >= budget {
		return 0, &InvalidRangeError{Message: "the range and the rate window it needs exceed the " +
			"most that may be read at once; narrow the range or the step"}
	}
	lookback, err := q.Lookback(ctx)
	if err != nil {
		return 0, err
	}
	return rateGap(step, lookback, budget, func(extent time.Duration) (time.Duration, error) {
		// SampleGap moves the start back to align on end, by up to one
		// resolution, and timestamp() of an instant selector reads a further
		// lookback before its first evaluation. Both are read, so both are
		// held to the budget.
		from := start.Add(-extent)
		if start.Sub(alignedStart(from, end, gapResolution(end.Sub(from))))+lookback > budget {
			return 0, &InvalidRangeError{Message: "the range and the rate window it needs exceed the " +
				"most that may be read at once; narrow the range or the step"}
		}
		return q.SampleGap(ctx, selector, from, end)
	})
}

// rateGap is RateGap's search, over any measurement of the span reaching
// extent before the range's start.
func rateGap(step, lookback, budget time.Duration, measure func(extent time.Duration) (time.Duration, error)) (time.Duration, error) {
	if step <= 0 {
		return 0, &InvalidRangeError{Message: "step must be greater than zero"}
	}
	if lookback <= 0 || lookback > (math.MaxInt64-step)/2 {
		return 0, &InvalidRangeError{Message: "the step is too large to take a rate over"}
	}
	extent := step + 2*lookback
	for {
		if extent > budget {
			return 0, &InvalidRangeError{Message: "the range and the rate window it needs exceed the " +
				"most that may be read at once; narrow the range or the step"}
		}
		gap, err := measure(extent)
		if err != nil || gap == 0 {
			return gap, err
		}
		window, err := RateWindow(step, gap)
		if err != nil {
			return 0, err
		}
		if window <= extent {
			return gap, nil
		}
		extent = window
	}
}

// SampleGap measures the longest interval between two consecutive samples of
// the series selector matches, over start to end, from the samples actually
// stored. A counter's rate window is sized from it (RateWindow), so the window
// fits the cadence the data really has -- whoever wrote it, at whatever
// interval it was collected, under whatever settings were in force then --
// rather than a cadence this node would have to guess at. A peer's collection
// interval, a peer pushing its own history, and a node whose dashboard
// interval was changed mid-range are all just samples here.
//
// It reads timestamp(selector) over the range: at each evaluation point, the
// time of the newest sample at or before it. Two adjacent points see sample
// times s1 <= s2; if the series has a gap from a to b, the last point before b
// sees a sample no later than a and the first point at or after b sees one no
// earlier than b, so their difference is at least b - a. The largest
// difference between adjacent points is therefore never less than the largest
// real gap, and exceeds it by at most one resolution step -- which is why the
// finest resolution Prometheus will serve for the range is used, not the
// chart's.
//
// A point with no value means the series had no sample within the database's
// lookback before it: the node was down, or not recording. Two samples either
// side of such a hole are an outage, not a cadence, and are not measured
// across: sizing every window in the chart from an outage would smear the
// whole range flat.
//
// A missing point is the ONLY sure sign of one. Between two points that both
// have a value, the jump in sample time can never exceed lookback plus one
// resolution -- the newer sample is at or before the later point, the older
// within the lookback of the earlier -- and any jump up to that bound can be
// made by healthy samples the evaluations stepped over: samples at 0s, 299s
// and 301s under a 5m lookback show as a 301s jump, where the real gaps are
// 299s and 2s. So such a jump is measured as a gap. It can only overstate the
// real one -- a wider window, never an empty one -- and the most a hole it
// hides can be is lookback plus one resolution, which the window then spans as
// any rate() spans a hole shorter than itself: the samples either side are
// real, and the counter's increase between them is the average of what
// happened.
//
// Zero means the series was not seen at all over the span: there is nothing a
// rate could be taken over.
func (q *Querier) SampleGap(ctx context.Context, selector string, start, end time.Time) (time.Duration, error) {
	resolution := gapResolution(end.Sub(start))
	// The first evaluation is moved back, not the last one forward, so that
	// an evaluation lands exactly on end: whether the series is still there
	// at end is what decides if its open gap counts (see longestGap), and an
	// evaluation short of end cannot say. Whole resolutions only, so the
	// point count stays within what gapResolution allowed for.
	series, err := q.QueryRange(ctx, "timestamp("+selector+")", alignedStart(start, end, resolution), end, resolution)
	if err != nil {
		return 0, err
	}
	return longestGap(series, resolution, end.UnixMilli()), nil
}

// alignedStart moves start back to the nearest instant a whole number of
// resolutions before end, so a range query from it evaluates exactly at end.
func alignedStart(start, end time.Time, resolution time.Duration) time.Time {
	steps := (end.Sub(start) + resolution - 1) / resolution
	return end.Add(-steps * resolution)
}

// gapResolution is the finest step Prometheus serves for a window: one
// second, or coarser when the window would otherwise need more than maxPoints.
func gapResolution(window time.Duration) time.Duration {
	// Rounded UP, to whole milliseconds, so the point count never exceeds
	// maxPoints: QueryRange counts window/step + 1 points.
	intervals := time.Duration(maxPoints - 1)
	resolution := (window + intervals - 1) / intervals
	if r := resolution % time.Millisecond; r != 0 {
		resolution += time.Millisecond - r
	}
	return max(time.Second, resolution)
}

// longestGap is the largest difference between the sample times seen at two
// adjacent evaluation points, in any series. Points are adjacent when they are
// one resolution apart; anything further means a point between them had no
// value, which is an outage rather than a gap between samples.
//
// The gap still open at endMs counts too. At the live edge the newest sample
// is as old as it is, and the one that will follow it has not been written:
// that interval is a gap LONGER than endMs minus the newest sample's time --
// strictly, since no sample landed at endMs -- so it is counted as one
// millisecond, the resolution of a sample time, more. The strictness matters:
// a window of exactly twice the elapsed time is left-open, and would exclude
// the sample before the newest one when it lay exactly that far back. A
// window sized from the closed gaps alone could hold only the newest sample.
// It is counted only for a series with a value AT endMs, which SampleGap
// aligns an evaluation to: one with none there is past Prometheus' lookback
// -- it has stopped -- and has no rate at the edge to draw. Counting its age
// would size every window from an outage. And only once the series has two
// samples since its last hole, for the same reason.
func longestGap(series []RangeSeries, resolution time.Duration, endMs int64) time.Duration {
	stepMs := resolution.Milliseconds()
	var gapMs int64
	for _, s := range series {
		// Whether the run of adjacent points the series ends in has seen two
		// different samples. A run begins after every hole.
		pairedSinceHole := false
		for i := 1; i < len(s.Points); i++ {
			prev, cur := s.Points[i-1], s.Points[i]
			if d := cur.TimeMs - prev.TimeMs; d < stepMs-1 || d > stepMs+1 {
				pairedSinceHole = false
				continue
			}
			// Measured as a gap, however long: see the note on outages
			// above. It can only overstate the real gap -- a wider window,
			// never an empty one.
			if d := sampleMs(cur) - sampleMs(prev); d > 0 {
				gapMs = max(gapMs, d)
				pairedSinceHole = true
			}
		}
		// Only after a pair. A lone sample after an outage has no cadence to
		// speak of yet: counting its age would size a window that reaches
		// back across the outage to the sample before it, and draw a rate
		// over the time the node was gone.
		if n := len(s.Points); pairedSinceHole && abs(endMs-s.Points[n-1].TimeMs) <= 1 {
			gapMs = max(gapMs, endMs-sampleMs(s.Points[n-1])+1)
		}
		// Two samples inside one evaluation step show as one: the point after
		// them sees only the newer. They are less than a resolution apart, so
		// a series seen at all has gaps of at least that to allow for. Without
		// it, a counter that began in the last few minutes of a month-long
		// range -- one step at that resolution -- would measure as nothing,
		// though a rate window of that size holds both samples.
		if len(s.Points) > 0 {
			gapMs = max(gapMs, stepMs)
		}
	}
	return time.Duration(gapMs) * time.Millisecond
}

// sampleMs is the sample time a timestamp() point carries: seconds, as a
// float, of a time stored in whole milliseconds.
func sampleMs(p Point) int64 { return int64(math.Round(p.Value * 1000)) }

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
