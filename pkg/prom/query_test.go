package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// promServer serves a canned body and records the query it was asked for.
func promServer(t *testing.T, status int, body string) (*httptest.Server, *url.Values) {
	t.Helper()
	seen := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func querier(url string, timeout time.Duration) *Querier {
	return NewQuerier(func() QueryOptions { return QueryOptions{BaseURL: url, Timeout: timeout} })
}

const rangeBody = `{
  "status":"success",
  "data":{"resultType":"matrix","result":[
    {"metric":{"__name__":"sysmon_disk_used_percent","device":"sda","instance":"mon0"},
     "values":[[1788300000,"12.5"],[1788300015,"13"]]},
    {"metric":{"__name__":"sysmon_disk_used_percent","device":"sdb","instance":"mon0"},
     "values":[[1788300000,"0"]]}
  ]}
}`

func TestQuerier_QueryRange_ParsesMatrix(t *testing.T) {
	srv, seen := promServer(t, http.StatusOK, rangeBody)
	q := querier(srv.URL, 5*time.Second)

	start := time.UnixMilli(1788300000000)
	end := start.Add(time.Minute)
	series, err := q.QueryRange(context.Background(), `sysmon_disk_used_percent{instance="mon0"}`, start, end, 15*time.Second)
	if err != nil {
		t.Fatalf("QueryRange() = %v", err)
	}

	if got := seen.Get("query"); got != `sysmon_disk_used_percent{instance="mon0"}` {
		t.Errorf("query = %q", got)
	}
	if got := seen.Get("step"); got != "15" {
		t.Errorf("step = %q, want 15 (seconds)", got)
	}
	if got := seen.Get("start"); got != "1788300000.000" {
		t.Errorf("start = %q, want unix seconds", got)
	}

	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}
	if series[0].Labels["device"] != "sda" {
		t.Errorf("first series device = %q", series[0].Labels["device"])
	}
	want := []Point{{TimeMs: 1788300000000, Value: 12.5}, {TimeMs: 1788300015000, Value: 13}}
	if len(series[0].Points) != len(want) {
		t.Fatalf("points = %v, want %v", series[0].Points, want)
	}
	for i := range want {
		if series[0].Points[i] != want[i] {
			t.Errorf("point %d = %+v, want %+v", i, series[0].Points[i], want[i])
		}
	}
	// A zero is a reading, not an absence: dropping it would draw a gap where
	// the machine reported "nothing is using this".
	if len(series[1].Points) != 1 || series[1].Points[0].Value != 0 {
		t.Errorf("second series = %+v, want one point valued 0", series[1].Points)
	}
}

// TestQuerier_QueryRange_EmptyMatchIsNotAnError: a node that was switched off
// for the window asked about genuinely has no data. Failing here would put an
// error banner over what is simply an empty chart.
func TestQuerier_QueryRange_EmptyMatchIsNotAnError(t *testing.T) {
	srv, _ := promServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	q := querier(srv.URL, time.Second)

	series, err := q.QueryRange(context.Background(), "up", time.UnixMilli(0), time.UnixMilli(60000), time.Second)
	if err != nil {
		t.Fatalf("QueryRange() = %v, want nil", err)
	}
	if len(series) != 0 {
		t.Fatalf("got %d series, want 0", len(series))
	}
}

// TestQuerier_QueryRange_UnparseablePointsAreGaps. Prometheus renders a NaN as
// the string "NaN", and a chart with a hole in it is what actually happened.
// Failing the request would blank the whole range because one scrape was
// missing.
func TestQuerier_QueryRange_UnparseablePointsAreGaps(t *testing.T) {
	// "NaN", "+Inf" and "-Inf" are how Prometheus renders an undefined value,
	// and strconv.ParseFloat accepts all three. They matter more than the
	// unparseable "x": encoding/json refuses to marshal a non-finite float, so
	// letting one through would fail the whole response rather than leave a
	// gap in one line.
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"device":"sda"},"values":[[1,"1"],[2,"NaN"],[3,"3"],[4,"x"],[5,"+Inf"],[6,"-Inf"]]}]}}`
	srv, _ := promServer(t, http.StatusOK, body)
	q := querier(srv.URL, time.Second)

	series, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second)
	if err != nil {
		t.Fatalf("QueryRange() = %v, want nil", err)
	}
	if len(series) != 1 || len(series[0].Points) != 2 {
		t.Fatalf("points = %+v, want the two parseable ones", series[0].Points)
	}
}

// TestQuerier_QueryRange_PrometheusError: Prometheus answers a bad query with
// 400 and the same JSON envelope as a success. The envelope says what is wrong;
// the status alone does not, so the body has to be read either way.
func TestQuerier_QueryRange_PrometheusError(t *testing.T) {
	body := `{"status":"error","errorType":"bad_data","error":"invalid parameter \"step\""}`
	srv, _ := promServer(t, http.StatusBadRequest, body)
	q := querier(srv.URL, time.Second)

	_, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second)
	var qerr *ErrQuery
	if !errors.As(err, &qerr) {
		t.Fatalf("QueryRange() = %v, want *ErrQuery", err)
	}
	if qerr.Type != "bad_data" || qerr.Detail == "" {
		t.Fatalf("ErrQuery = %+v, want the errorType and error carried through", qerr)
	}
}

// TestQuerier_QueryRange_NotPrometheus is the misconfiguration this endpoint
// invites: query_url pointing at a proxy, a dashboard, or the sysmon port
// itself. It must not look like a query that failed.
func TestQuerier_QueryRange_NotPrometheus(t *testing.T) {
	srv, _ := promServer(t, http.StatusOK, "<html>hello</html>")
	q := querier(srv.URL, time.Second)

	_, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second)
	var derr *ErrDecode
	if !errors.As(err, &derr) {
		t.Fatalf("QueryRange() = %v, want *ErrDecode", err)
	}
}

func TestQuerier_QueryRange_Rejections(t *testing.T) {
	srv, _ := promServer(t, http.StatusOK, rangeBody)
	q := querier(srv.URL, time.Second)
	start := time.UnixMilli(0)

	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		step  time.Duration
	}{
		{"end before start", start.Add(time.Hour), start, time.Second},
		{"end equal to start", start, start, time.Second},
		{"zero step", start, start.Add(time.Hour), 0},
		{"negative step", start, start.Add(time.Hour), -time.Second},
		// One point past what Prometheus will return. Counted by enumeration
		// rather than by repeating the formula: a sample at t=0, one at every
		// second, and one at t=11000 is 11,001 points. The check and its test
		// used to share the same off-by-one, so the test certified the bug.
		{"one point over the ceiling", start, start.Add(11000 * time.Second), time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := q.QueryRange(context.Background(), "m", tt.start, tt.end, tt.step)
			var rangeErr *ErrInvalidRange
			if !errors.As(err, &rangeErr) {
				t.Fatalf("QueryRange() = %v, want *ErrInvalidRange", err)
			}
		})
	}

	// And exactly at the ceiling is allowed. t=0, every second, through
	// t=10999 is 11,000 samples -- the most Prometheus will return. The two
	// durations here are written out rather than derived from maxPoints, so
	// this cannot agree with the code by sharing its arithmetic.
	if _, err := q.QueryRange(context.Background(), "m",
		start, start.Add(10999*time.Second), time.Second); err != nil {
		t.Fatalf("a range of exactly 11,000 points = %v, want it allowed", err)
	}
}

// TestQuerier_Disabled: with no query_url nothing is attempted, and the caller
// is told which of "no history here" and "history failed" happened.
func TestQuerier_Disabled(t *testing.T) {
	q := querier("", time.Second)
	if q.Enabled() {
		t.Fatal("Enabled() = true with no base URL")
	}

	_, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second)
	var none *ErrNoQueryEndpoint
	if !errors.As(err, &none) {
		t.Fatalf("QueryRange() = %v, want *ErrNoQueryEndpoint", err)
	}
}

// TestQuerier_ReadsOptionsPerQuery is what makes prometheus.query_url
// reloadable: turning history on must not need a restart, which would drop
// every connected browser to gain a setting.
func TestQuerier_ReadsOptionsPerQuery(t *testing.T) {
	srv, _ := promServer(t, http.StatusOK, rangeBody)

	base := ""
	q := NewQuerier(func() QueryOptions { return QueryOptions{BaseURL: base, Timeout: time.Second} })
	if q.Enabled() {
		t.Fatal("Enabled() = true before configuration")
	}

	base = srv.URL
	if !q.Enabled() {
		t.Fatal("Enabled() = false after the URL was configured")
	}
	if _, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second); err != nil {
		t.Fatalf("QueryRange() after reconfiguration = %v", err)
	}
}

// TestQuerier_TrailingSlashInBaseURL: "http://mon0:9090/" is what an operator
// copies out of a browser, and a doubled slash is a 404 from Prometheus.
func TestQuerier_TrailingSlashInBaseURL(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if _, err := w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	q := querier(srv.URL+"/", time.Second)
	if _, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second); err != nil {
		t.Fatalf("QueryRange() = %v", err)
	}
	if path != "/api/v1/query_range" {
		t.Fatalf("path = %q, want /api/v1/query_range", path)
	}
}

// TestQuerier_TimeoutBoundsTheQuery: the timeout is per request rather than on
// the client, so it can be reconfigured without discarding the connection pool.
// It still has to actually bound the request.
//
// The receiver never answers until the test ends, so the query returning at
// all proves something other than the receiver ended it; the deadline it was
// sent under proves that something was the configured timeout, measured from
// when the query was made.
func TestQuerier_TimeoutBoundsTheQuery(t *testing.T) {
	const timeout = 50 * time.Millisecond
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		if _, err := w.Write([]byte(`{"status":"success","data":{"result":[]}}`)); err != nil {
			t.Errorf("writing the late answer: %v", err)
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	q := querier(srv.URL, timeout)
	tr := &deadlineTransport{next: srv.Client().Transport}
	q.client.Transport = tr

	queried := time.Now()
	failed := make(chan error, 1)
	go func() {
		_, err := q.QueryRange(context.Background(), "m", time.UnixMilli(0), time.UnixMilli(60000), time.Second)
		failed <- err
	}()
	err := await.Recv(t, "QueryRange against a hung receiver to return", failed)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("QueryRange() against a hung receiver = %v, want it to end on its deadline", err)
	}

	sent := tr.requests()
	if len(sent) != 1 {
		t.Fatalf("%d requests were sent, want 1", len(sent))
	}
	if earliest, latest := queried.Add(timeout), sent[0].sent.Add(timeout); sent[0].deadline.Before(earliest) || sent[0].deadline.After(latest) {
		t.Fatalf("the query was sent with deadline %v, want the configured %v from when it was made: between %v and %v",
			sent[0].deadline, timeout, earliest, latest)
	}
}

func TestStepFor(t *testing.T) {
	tests := []struct {
		window time.Duration
		want   time.Duration
	}{
		{0, time.Second},
		{-time.Hour, time.Second},
		{time.Minute, time.Second},                // below the floor
		{TargetPoints * time.Second, time.Second}, // exactly the floor
		{time.Hour, 6 * time.Second},
		{24 * time.Hour, 144 * time.Second},
	}
	for _, tt := range tests {
		if got := StepFor(tt.window); got != tt.want {
			t.Errorf("StepFor(%v) = %v, want %v", tt.window, got, tt.want)
		}
	}

	// Whatever the window, the resulting step must keep the request under the
	// ceiling -- that is the whole reason the server picks one.
	for _, window := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour, 90 * 24 * time.Hour} {
		if points := window/StepFor(window) + 1; points > maxPoints {
			t.Errorf("StepFor(%v) yields %d points, over the %d ceiling", window, points, maxPoints)
		}
	}
}

// TestQuerier_QueryRange_SuccessUnderAFailureStatus.
//
// The envelope is decoded before the status is judged, because Prometheus
// answers a bad query with 400 and a body that explains it. That is not the
// same as letting the body overrule the status: a 5xx carrying
// {"status":"success"} did not come from a Prometheus that answered the query.
// It came from something in front of it, or from one that failed after
// starting the response -- and a chart drawn from it presents whatever it
// contained, or an empty range, as this node's past.
func TestQuerier_QueryRange_SuccessUnderAFailureStatus(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		srv, _ := promServer(t, status, rangeBody)
		q := querier(srv.URL, 5*time.Second)

		got, err := q.QueryRange(context.Background(), "up",
			time.UnixMilli(1788300000000), time.UnixMilli(1788300600000), 15*time.Second)
		if err == nil {
			t.Fatalf("QueryRange() = %d series, nil error for an HTTP %d carrying "+
				"a success envelope", len(got), status)
		}
		var qe *ErrQuery
		if !errors.As(err, &qe) {
			t.Fatalf("QueryRange() = %v for HTTP %d, want an *ErrQuery", err, status)
		}
	}

	// And a 2xx with the same body is still served, so this rejects the status
	// rather than the body.
	srv, _ := promServer(t, 200, rangeBody)
	q := querier(srv.URL, 5*time.Second)
	if _, err := q.QueryRange(context.Background(), "up",
		time.UnixMilli(1788300000000), time.UnixMilli(1788300600000), 15*time.Second); err != nil {
		t.Fatalf("QueryRange() = %v for a 200 carrying the same body", err)
	}
}

// TestQuerier_QueryRange_WrongResultType.
//
// A range query is answered with a matrix. A vector is an instant query's
// answer -- most likely a proxy rewriting the path -- and it decodes into this
// shape as an empty result, so without the check it draws as "no history for
// this window" rather than as the misconfiguration it is. That is the quiet
// failure this endpoint keeps producing in different forms: a wrong answer
// that looks like an ordinary empty one.
func TestQuerier_QueryRange_WrongResultType(t *testing.T) {
	bodies := map[string]string{
		"a vector":       `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		"a scalar":       `{"status":"success","data":{"resultType":"scalar","result":[]}}`,
		"no result type": `{"status":"success","data":{"result":[]}}`,
	}

	for name, body := range bodies {
		srv, _ := promServer(t, 200, body)
		q := querier(srv.URL, 5*time.Second)

		got, err := q.QueryRange(context.Background(), "up",
			time.UnixMilli(1788300000000), time.UnixMilli(1788300600000), 15*time.Second)
		if err == nil {
			t.Fatalf("%s: QueryRange() = %d series, nil error; an empty chart and a "+
				"wrong answer are indistinguishable to whoever is reading it", name, len(got))
		}
		var wrong *ErrUnexpectedResultType
		if !errors.As(err, &wrong) {
			t.Fatalf("%s: QueryRange() = %v, want an *ErrUnexpectedResultType", name, err)
		}
	}

	// An EMPTY matrix is still a legitimate answer: the window has no data.
	srv, _ := promServer(t, 200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	q := querier(srv.URL, 5*time.Second)
	got, err := q.QueryRange(context.Background(), "up",
		time.UnixMilli(1788300000000), time.UnixMilli(1788300600000), 15*time.Second)
	if err != nil || len(got) != 0 {
		t.Fatalf("QueryRange() = %v, %v for an empty matrix; a window with no data is "+
			"not an error", got, err)
	}
}
