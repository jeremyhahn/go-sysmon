package prom

import (
	"context"
	"encoding/json"
	"io"
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
}

// NewQuerier returns a Querier reading its settings from opts on every query.
func NewQuerier(opts func() QueryOptions) *Querier {
	// Same policy as every other outbound client: a query API that redirects
	// off the configured host would have this node drawing another
	// deployment's history and labelling it as its own.
	return &Querier{opts: opts, client: outbound.NewClient(0)}
}

// Enabled reports whether a Prometheus is currently configured to read from.
func (q *Querier) Enabled() bool { return q.opts().BaseURL != "" }

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
		return nil, &ErrNoQueryEndpoint{}
	}
	if !end.After(start) {
		return nil, &ErrInvalidRange{Message: "end must be after start"}
	}
	if step <= 0 {
		return nil, &ErrInvalidRange{Message: "step must be greater than zero"}
	}
	// Points, not intervals. A range query returns a sample at start, at every
	// step, AND at end -- so t=0 to t=10s at 1s is eleven points, not ten.
	// Counting intervals let a request one point over the ceiling through, and
	// Prometheus rejected it, which reached the dashboard as a 502 from a range
	// this server had already decided was fine.
	if points := int(end.Sub(start)/step) + 1; points > maxPoints {
		return nil, &ErrInvalidRange{
			Message: "the range needs " + strconv.Itoa(points) + " points at this step; Prometheus returns at most " +
				strconv.Itoa(maxPoints) + ", so widen the step or narrow the range",
		}
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
		return nil, &ErrRequest{URL: endpoint, Err: err}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, &ErrRequest{URL: endpoint, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// Prometheus reports a bad query as 400 with the same JSON envelope as a
	// success, so the body is decoded before the status is judged: the envelope
	// says what is wrong, and the status alone does not.
	var parsed queryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseBodyLimit)).Decode(&parsed); err != nil {
		return nil, &ErrDecode{URL: endpoint, Err: err}
	}
	if parsed.Status != "success" {
		return nil, &ErrQuery{Query: expr, Type: parsed.ErrorType, Detail: parsed.Error}
	}
	// The envelope is decoded first but it is not the last word. A body saying
	// "success" under a 5xx did not come from a Prometheus that answered the
	// query -- it came from something in front of it, or from a Prometheus
	// that failed after starting the response -- and drawing a chart from it
	// presents whatever it contained, or an empty range, as this node's past.
	// The envelope explains a 4xx; it cannot make a 5xx into an answer.
	if resp.StatusCode/100 != 2 {
		return nil, &ErrQuery{
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
		return nil, &ErrDecode{
			URL: endpoint,
			Err: &ErrUnexpectedResultType{Got: parsed.Data.ResultType},
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
