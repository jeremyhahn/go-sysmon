//go:build prometheus

// Tagged because the real history handlers exist only in the prometheus build (#100).
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/metrics"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// fakePrometheus answers range queries and records what was asked of it.
//
// With cadence set it also answers timestamp() the way Prometheus would for a
// series sampled every cadence since the epoch: at each evaluation point of
// the request, the time of the newest sample. That is what the counter path
// measures its sample gap from.
type fakePrometheus struct {
	srv     *httptest.Server
	seen    url.Values
	queries []url.Values
	status  int
	body    string
	cadence time.Duration
	// flagsStatus and flags answer /api/v1/status/flags, where the query
	// lookback is read from: Prometheus' default unless a test says otherwise.
	flagsStatus int
	flags       string
	// holdRate, when set, holds every rate query -- the last request of a
	// counter read -- until it is closed or the client gives up; rateArrived
	// is told each time one arrives. The lookback and gap requests are
	// answered at once, so a held rate query is a read that has done
	// everything else. No test waits on wall-clock latency.
	holdRate    chan struct{}
	rateArrived chan struct{}
}

// clientGone reports whether a failed write is the client having hung up --
// the expected end of a request the client's deadline cancelled -- rather
// than a fault in the fake.
func clientGone(r *http.Request, err error) bool {
	return r.Context().Err() != nil || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

func newFakePrometheus(t *testing.T) *fakePrometheus {
	t.Helper()
	f := &fakePrometheus{status: http.StatusOK, flagsStatus: http.StatusOK,
		flags: `{"status":"success","data":{"query.lookback-delta":"5m"}}`}
	f.body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"device":"sdb"},"values":[[100,"2"]]},
	  {"metric":{"device":"sda"},"values":[[100,"1"],[115,"1.5"]]}
	]}}`
	write := func(w http.ResponseWriter, r *http.Request, body string) {
		// A write can only fail because the client has gone -- a request its
		// deadline cancelled -- and that is tolerated as exactly that, never
		// ignored: any other failure fails the test.
		if _, err := w.Write([]byte(body)); err != nil && !clientGone(r, err) {
			t.Errorf("fake Prometheus: writing the response: %v", err)
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status/flags" {
			w.WriteHeader(f.flagsStatus)
			write(w, r, f.flags)
			return
		}
		q := r.URL.Query()
		f.seen = q
		f.queries = append(f.queries, q)
		timestamps := strings.HasPrefix(q.Get("query"), "timestamp(")
		if !timestamps && f.holdRate != nil {
			f.rateArrived <- struct{}{}
			select {
			case <-f.holdRate:
			case <-r.Context().Done():
				// The client gave up; there is no one to answer.
				return
			}
		}
		w.WriteHeader(f.status)
		body := f.body
		if timestamps {
			body = f.timestamps(t, q)
		}
		write(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// holdRates makes f hold every rate query until the test ends, releasing
// them before the server closes so no handler is left waiting.
func (f *fakePrometheus) holdRates(t *testing.T) {
	t.Helper()
	f.holdRate = make(chan struct{})
	f.rateArrived = make(chan struct{}, 16)
	t.Cleanup(func() { close(f.holdRate) })
}

// timestamps renders timestamp() over the request's range for a series
// sampled every f.cadence; with no cadence, a series with no samples.
func (f *fakePrometheus) timestamps(t *testing.T, q url.Values) string {
	t.Helper()
	if f.cadence == 0 {
		return `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	}
	parse := func(name string) float64 {
		v, err := strconv.ParseFloat(q.Get(name), 64)
		if err != nil {
			t.Fatalf("fake prometheus: %s = %q", name, q.Get(name))
		}
		return v
	}
	start, end, step := parse("start"), parse("end"), parse("step")
	cadence := f.cadence.Seconds()
	var values []string
	for at := start; at <= end+1e-9; at += step {
		newest := math.Floor(at/cadence) * cadence
		values = append(values, "["+strconv.FormatFloat(at, 'f', 3, 64)+`,"`+
			strconv.FormatFloat(newest, 'f', 3, 64)+`"]`)
	}
	return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[` +
		strings.Join(values, ",") + `]}]}}`
}

// historyConfig is a manager naming this node mon0 with one peer, fs. The
// history endpoint answers only for nodes it knows, so the tests need a
// configuration that says which those are.
func historyConfig(t *testing.T) *config.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	body := "fleet:\n  local_name: mon0\n  peers:\n    - name: fs\n      url: http://fs:8081\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	return mgr
}

// historyServer returns a Server wired to prom, or to nothing when url is "".
func historyServer(t *testing.T, url string) *Server {
	t.Helper()
	return newServer(Config{
		Config: historyConfig(t),
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: url, Timeout: 5 * time.Second}
		}),
	}, nil)
}

// TestHandleHistory_DisabledIsNotAnError. A node with no Prometheus is a
// standalone node, not a broken one: answering 404 would be indistinguishable
// from an old build without the endpoint, and the dashboard could not tell
// whether to hide the date picker or show a failure.
func TestHandleHistory_DisabledIsNotAnError(t *testing.T) {
	s := historyServer(t, "")

	for _, path := range []string{"/api/history/metrics", "/api/history?metric=cpu"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if body.Enabled {
			t.Errorf("GET %s reported enabled with no query URL", path)
		}
	}
}

// TestHandleHistoryMetrics_ListsTheCatalog.
func TestHandleHistoryMetrics_ListsTheCatalog(t *testing.T) {
	f := newFakePrometheus(t)
	rec := get(t, historyServer(t, f.srv.URL), "/api/history/metrics")

	var got types.HistoryCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || len(got.Metrics) != len(prom.Catalog()) {
		t.Fatalf("catalog = %+v, want every entry with enabled=true", got)
	}
	for _, m := range got.Metrics {
		if m.Key == "" || m.Label == "" || m.Unit == "" {
			t.Errorf("catalog entry %+v is missing a field the UI needs", m)
		}
	}
}

// TestHandleHistory_ServesARange checks the query built for Prometheus and the
// payload built for the browser, since a mistake in either is invisible in the
// other.
func TestHandleHistory_ServesARange(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	rec := get(t, s, "/api/history?metric=disk_used&node=mon0&start=1788300000000&end=1788300600000&step=15000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if got, want := f.seen.Get("query"), `sysmon_disk_used_percent{instance="mon0"}`; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}
	if got := f.seen.Get("step"); got != "15" {
		t.Errorf("upstream step = %q, want 15", got)
	}

	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Node != "mon0" || got.Metric != "disk_used" || got.Unit != types.HistoryUnitPercent {
		t.Fatalf("history envelope = %+v", got)
	}
	if got.StepMs != 15000 || got.StartMs != 1788300000000 || got.EndMs != 1788300600000 {
		t.Fatalf("window = %d..%d step %d, want it echoed back", got.StartMs, got.EndMs, got.StepMs)
	}

	// Prometheus returned sdb first. A legend that reshuffles between requests
	// is the same defect the fleet list had, so the order is imposed here.
	if len(got.Series) != 2 || got.Series[0].Name != "sda" || got.Series[1].Name != "sdb" {
		t.Fatalf("series = %+v, want sda then sdb", got.Series)
	}
	if len(got.Series[0].Points) != 2 || got.Series[0].Points[0].TimeMs != 100000 {
		t.Fatalf("sda points = %+v, want two points in milliseconds", got.Series[0].Points)
	}
}

// TestHandleHistory_CounterIsQueriedAsARate. A counter drawn raw is a line
// that only rises; the endpoint must measure how far apart its samples really
// are, ask Prometheus for its rate over a window sized from that gap and the
// step, scale the swap counters' pages to bytes by the node's page size, and
// report bytes per second.
func TestHandleHistory_CounterIsQueriedAsARate(t *testing.T) {
	f := newFakePrometheus(t)
	f.cadence = time.Minute
	s := historyServer(t, f.srv.URL)
	const scale = ` * sysmon_memory_page_size_bytes{instance="mon0"}`

	// A five-minute step over samples a minute apart: a step plus the gap.
	rec := get(t, s, "/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000&step=300000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.queries) != 2 {
		t.Fatalf("%d queries, want the measurement and the rate", len(f.queries))
	}
	measured := f.queries[0]
	if got, want := measured.Get("query"), `timestamp(sysmon_swap_out_pages_total{instance="mon0"})`; got != want {
		t.Errorf("measurement query = %q, want %q", got, want)
	}
	// It reaches back before the range, over the first windows: a step plus
	// two lookbacks.
	if got, want := measured.Get("start"), "1788299100.000"; got != want {
		t.Errorf("measurement start = %s, want %s: fifteen minutes before the range", got, want)
	}
	if got, want := f.seen.Get("query"), `rate(sysmon_swap_out_pages_total{instance="mon0"}[360s])`+scale+
		` and sysmon_swap_out_pages_total{instance="mon0"}`; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}
	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Unit != types.HistoryUnitBytesPerSecond {
		t.Errorf("unit = %q, want bytes_per_second", got.Unit)
	}

	// A one-second step is held at two gaps, or a node collecting once a
	// minute would have one sample per window and draw nothing.
	rec = get(t, s, "/api/history?metric=swap_in&node=mon0&start=1788300000000&end=1788300600000&step=1000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, want := f.seen.Get("query"), `rate(sysmon_swap_in_pages_total{instance="mon0"}[120s])`+scale+
		` and sysmon_swap_in_pages_total{instance="mon0"}`; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}

	// Samples every four minutes -- a peer configured far slower than any
	// dashboard interval, which this node has no way to know -- are measured,
	// not guessed at.
	f.cadence = 4 * time.Minute
	rec = get(t, s, "/api/history?metric=swap_in&node=fs&start=1788300000000&end=1788303600000&step=6000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, want := f.seen.Get("query"), `rate(sysmon_swap_in_pages_total{instance="fs"}[480s])`; !strings.HasPrefix(got, want) {
		t.Errorf("upstream query = %q, want it to start %q", got, want)
	}
}

// TestHandleHistory_CounterWithNothingToMeasureIsEmpty. A counter with no
// two samples close enough to measure has no rate to draw. That is an empty
// range -- the same answer as a node that recorded nothing -- and no rate
// query is sent with a window sized from nothing.
func TestHandleHistory_CounterWithNothingToMeasureIsEmpty(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	rec := get(t, s, "/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || len(got.Series) != 0 {
		t.Errorf("history = %+v, want an enabled, empty range", got)
	}
	if len(f.queries) != 1 {
		t.Errorf("%d queries, want only the measurement", len(f.queries))
	}
}

// TestHandleHistory_CounterRangeIsValidatedFirst. A range Prometheus would
// refuse -- more points than it returns -- is the same 400 for a counter as
// for a gauge, and is refused before any query is made for it: not measured
// first, and not answered as an empty 200 when the measurement finds nothing.
func TestHandleHistory_CounterRangeIsValidatedFirst(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	for _, metric := range []string{"swap", "swap_out"} {
		f.queries = nil
		rec := get(t, s, "/api/history?metric="+metric+"&node=mon0&start=0&end=20000000&step=1000")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", metric, rec.Code)
		}
		if len(f.queries) != 0 {
			t.Errorf("%s: %d queries made for a range that is refused", metric, len(f.queries))
		}
	}
}

// TestHandleHistory_CounterMeasurementAlignmentIsBudgeted. The measurement
// is aligned on the end, which moves its start back by up to a resolution.
// Near the 90-day bound that slack is what would carry it past, so it is
// counted: a range leaving room for the extent but not the slack is refused
// before anything is read.
func TestHandleHistory_CounterMeasurementAlignmentIsBudgeted(t *testing.T) {
	f := newFakePrometheus(t)
	f.cadence = time.Minute
	s := historyServer(t, f.srv.URL)

	// 90 days less 22 minutes, at a 12-minute step (inside the point limit):
	// the extent -- a step and two lookbacks, 22 minutes -- uses exactly what
	// is left, and aligning the resulting 90-day span on its 706.974s
	// resolution moves the start back a further 7.026s, past the bound.
	const end = int64(1788300000000)
	start := end - (90*24*time.Hour - 22*time.Minute).Milliseconds()
	rec := get(t, s, "/api/history?metric=swap_out&node=mon0&start="+strconv.FormatInt(start, 10)+
		"&end="+strconv.FormatInt(end, 10)+"&step=720000")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(f.queries) != 0 {
		t.Errorf("%d queries made past the budget", len(f.queries))
	}
}

// TestHandleHistory_CounterNeedsAPublishedLookback. A database that does
// not publish its query lookback -- VictoriaMetrics serves no status/flags --
// cannot have a counter's rate window measured against it. That is said as
// what it is, a property of the database, and no rate is guessed at; a gauge
// from the same database is unaffected.
func TestHandleHistory_CounterNeedsAPublishedLookback(t *testing.T) {
	f := newFakePrometheus(t)
	f.cadence = time.Minute
	f.flagsStatus, f.flags = http.StatusNotFound, "404 page not found"
	s := historyServer(t, f.srv.URL)

	rec := get(t, s, "/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "does not publish") || strings.Contains(body, f.srv.URL) {
		t.Errorf("body = %q, want the reason and not the database's address", body)
	}
	if len(f.queries) != 0 {
		t.Errorf("%d queries made for a counter whose window cannot be sized", len(f.queries))
	}

	if rec := get(t, s, "/api/history?metric=swap&node=mon0&start=1788300000000&end=1788303600000"); rec.Code != http.StatusOK {
		t.Errorf("gauge from the same database: status = %d, want 200", rec.Code)
	}

	// A request that is invalid on its own terms is the caller's 400, found
	// without asking the database anything -- not a 502 because the database
	// would not have answered.
	rec = get(t, s, "/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000&step=31536000000")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a one-year step: status = %d, want 400 before the database is asked", rec.Code)
	}
}

// TestHandleHistory_TheDatabaseLookbackIsBudgeted. An instant selector reads
// one database lookback before its first evaluation -- the gap measurement's
// timestamp() and the rate's presence gate both use one. Under a 10-day
// lookback a 65-day range leaves 25 days: the measurement's extent (a step and
// two lookbacks, ~20 days) fits, and the lookback its selector reads on top
// does not.
func TestHandleHistory_TheDatabaseLookbackIsBudgeted(t *testing.T) {
	f := newFakePrometheus(t)
	f.cadence = time.Minute
	f.flags = `{"status":"success","data":{"query.lookback-delta":"10d"}}`
	s := historyServer(t, f.srv.URL)

	const end = int64(1788300000000)
	start := end - (65 * 24 * time.Hour).Milliseconds()
	rec := get(t, s, "/api/history?metric=swap_out&node=mon0&start="+strconv.FormatInt(start, 10)+
		"&end="+strconv.FormatInt(end, 10))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(f.queries) != 0 {
		t.Errorf("%d queries made past the budget", len(f.queries))
	}
}

// TestHandleHistory_ACounterReadEndsInsideTheWriteBudget. A counter read is
// several requests -- the lookback, the gap, the rate -- under ONE deadline,
// held inside the server's own write budget: a configured timeout longer than
// that must not keep the read going past the point where even a 502 could be
// sent.
//
// Deterministic, with no latency in it: the lookback and gap are answered at
// once and the rate query is held until the test ends, under a Prometheus
// timeout of an hour. Only a single deadline spanning the whole read, capped
// at the write budget, can end it -- per-request timeouts would leave the
// handler waiting out the hour, and the test would fail at await's bound
// rather than pass.
func TestHandleHistory_ACounterReadEndsInsideTheWriteBudget(t *testing.T) {
	saved := responseWriteTimeout
	t.Cleanup(func() { responseWriteTimeout = saved })

	// The one deadline spans the lookback and gap requests as well as the
	// held rate query. On a loaded runner the first two can use up a short
	// budget, so the read ends -- correctly, with 502 -- before it asks for
	// the rate, and proves nothing about the held query. Such an attempt is
	// retried at twice the budget until the read reaches its rate query; the
	// budgets stay well inside await's bound, so a held read that ended at
	// the deadline is told apart from one the deadline did not end.
	for budget := 300 * time.Millisecond; budget <= 4800*time.Millisecond; budget *= 2 {
		responseWriteTimeout = budget
		if readReachedItsRateQuery(t) {
			return
		}
	}
	t.Fatal("no read reached its rate query inside a 4.8s write budget")
}

// readReachedItsRateQuery makes a counter read whose rate query is held, and
// reports whether the read reached that query. Either way the read must end
// at its deadline with 502.
func readReachedItsRateQuery(t *testing.T) bool {
	t.Helper()
	f := newFakePrometheus(t)
	f.cadence = time.Minute
	f.holdRates(t)
	s := newServer(Config{
		Config: historyConfig(t),
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: f.srv.URL, Timeout: time.Hour}
		}),
	}, nil)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- get(t, s, "/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000")
	}()
	rec := await.Recv(t, "the read to end at its deadline", done)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 once the write budget ran out", rec.Code)
	}
	select {
	case <-f.rateArrived:
		return true
	default:
		return false
	}
}

// TestHandleHistory_CounterMeasurementFailureIsABadGateway. The measurement
// is a query like any other: Prometheus failing it is a 502, with the detail
// in the log.
func TestHandleHistory_CounterMeasurementFailureIsABadGateway(t *testing.T) {
	f := newFakePrometheus(t)
	f.status = http.StatusInternalServerError
	s := historyServer(t, f.srv.URL)

	rec := get(t, s, "/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(f.queries) != 1 {
		t.Errorf("%d queries, want the rate never asked for after the measurement failed", len(f.queries))
	}
}

// TestHandleHistory_CounterLookbackIsBounded. A counter's rate window grows
// with the step, and the step is the caller's. The 90-day bound on how much
// one unauthenticated request may make Prometheus read has to count that
// window, or a short range at a huge step reads years before its start.
func TestHandleHistory_CounterLookbackIsBounded(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	cases := []string{
		// One hour at a one-year step: one point, a year of lookback.
		"/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000&step=31536000000",
		// A step whose lookback is within seconds of the largest duration:
		// range plus lookback would overflow negative and slip under a sum.
		"/api/history?metric=swap_out&node=mon0&start=1788300000000&end=1788303600000&step=9223372000000",
		// A full 90-day range plus any lookback at all.
		"/api/history?metric=swap_in&node=mon0&start=1780524000000&end=1788300000000",
	}
	for _, path := range cases {
		f.seen = nil
		rec := get(t, s, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("%s: Prometheus was queried with %q", path, f.seen.Get("query"))
		}
	}

	// The same huge step on a gauge reads nothing before start, so it is
	// served exactly as before.
	rec := get(t, s, "/api/history?metric=swap&node=mon0&start=1788300000000&end=1788303600000&step=31536000000")
	if rec.Code != http.StatusOK {
		t.Errorf("gauge at a huge step: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestHandleHistory_LabelsAreNotForwarded. Everything on a Prometheus label set
// other than the splitting label is either constant across the response or the
// operator's own labelling, and forwarding it would publish the fleet's
// Prometheus configuration to any browser that can reach the dashboard.
func TestHandleHistory_LabelsAreNotForwarded(t *testing.T) {
	f := newFakePrometheus(t)
	f.body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"device":"sda","instance":"mon0","job":"go-sysmon","datacenter":"rack-3"},
	   "values":[[100,"1"]]}]}}`

	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=disk_used&node=mon0")
	if body := rec.Body.String(); strings.Contains(body, "rack-3") || strings.Contains(body, "datacenter") {
		t.Fatalf("the response carried the operator's external labels: %s", body)
	}
}

// TestHandleHistory_CoresAreNamedByPackageAndCore: every package of a
// two-socket host has a Core 0, so a core's series is named "package:core",
// never by the core number alone that both sockets share.
func TestHandleHistory_CoresAreNamedByPackageAndCore(t *testing.T) {
	f := newFakePrometheus(t)
	f.body = `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"package":"0","core":"0","instance":"mon0"},"values":[[100,"50"]]},
	  {"metric":{"package":"1","core":"0","instance":"mon0"},"values":[[100,"60"]]}]}}`

	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=cpu_temperature&node=mon0")
	var resp types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	var names []string
	for _, s := range resp.Series {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "0:0,1:0" {
		t.Errorf("series names = %v, want 0:0 and 1:0", names)
	}
	query := f.seen.Get("query")
	if !strings.Contains(query, `package!=""`) {
		t.Errorf("query %q does not require the package label", query)
	}
}

// TestHandleHistory_UnknownMetricIsRejected is the allowlist. Anything that is
// not a catalog key must be a 400, never an expression forwarded upstream.
func TestHandleHistory_UnknownMetricIsRejected(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	for _, metric := range []string{
		"",
		"sysmon_cpu_usage_percent",
		"up",
		`up} or vector(1) #`,
		"nonsense",
	} {
		f.seen = nil
		// A valid node is supplied deliberately. Without one the request is
		// rejected for the node rather than the metric, and this would pass
		// against a handler with no allowlist at all.
		rec := get(t, s, "/api/history?node=mon0&metric="+url.QueryEscape(metric))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("metric=%q gave %d, want 400", metric, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("metric=%q reached Prometheus as %q; the allowlist did not hold",
				metric, f.seen.Get("query"))
		}
	}
}

// TestHandleHistory_InjectionInTheNodeIsRejected. The node name is the only
// caller-supplied part of the expression; if it can close the matcher, this
// endpoint becomes an open PromQL proxy on an unauthenticated port.
func TestHandleHistory_InjectionInTheNodeIsRejected(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	for _, node := range []string{
		`mon0"} or on() group_left() up{`,
		`mon0" or foo="`,
		"mon0\nup",
		"mon0 # x",
		`.*`,
	} {
		f.seen = nil
		rec := get(t, s, "/api/history?metric=cpu&node="+url.QueryEscape(node))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("node=%q gave %d, want 400", node, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("node=%q reached Prometheus as %q", node, f.seen.Get("query"))
		}
	}
}

// TestHandleHistory_BadWindowsAreRejected covers the window arithmetic, and
// separates the caller's mistake (400) from the receiver's failure (502).
func TestHandleHistory_BadWindowsAreRejected(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	tests := []struct {
		name  string
		query string
	}{
		{"end before start", "start=2000&end=1000"},
		{"empty window", "start=1000&end=1000"},
		{"non-numeric start", "start=yesterday&end=1000"},
		{"non-numeric step", "step=fast"},
		// A year, when the ceiling is 90 days.
		{"window past the ceiling", "start=0&end=31536000000"},
		// 20000 points at a one-second step, when Prometheus returns 11000.
		{"resolution past the ceiling", "start=0&end=20000000&step=1000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, s, "/api/history?metric=cpu&node=mon0&"+tt.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleHistory_UpstreamFailureIsABadGateway. The distinction matters to
// whoever is reading the error: 502 says the dashboard reached go-sysmon and
// go-sysmon could not reach Prometheus.
//
// What it must NOT say is where that Prometheus is, or what it replied. This
// endpoint has no authentication of its own, and the address of the fleet's
// time-series database is reconnaissance -- the same reason the settings view
// redacts it. The detail belongs in the server log.
func TestHandleHistory_UpstreamFailureIsABadGateway(t *testing.T) {
	f := newFakePrometheus(t)
	f.status = http.StatusInternalServerError
	f.body = `{"status":"error","errorType":"internal","error":"tsdb at /var/lib/prometheus is compacting"}`

	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=cpu&node=mon0")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{f.srv.URL, "/var/lib/prometheus", "tsdb"} {
		if strings.Contains(body, secret) {
			t.Errorf("the 502 body disclosed %q: %s", secret, body)
		}
	}
}

// TestHandleHistory_OnlyKnownNodes. Several nodes write into one Prometheus,
// and this endpoint is unauthenticated. A name that merely looks like a
// hostname is not enough: forwarding it would let anyone able to reach this
// dashboard read the history of every machine in the database.
func TestHandleHistory_OnlyKnownNodes(t *testing.T) {
	f := newFakePrometheus(t)
	s := historyServer(t, f.srv.URL)

	// This node and its configured peer are served.
	for _, node := range []string{"mon0", "fs"} {
		f.seen = nil
		if rec := get(t, s, "/api/history?metric=cpu&node="+node); rec.Code != http.StatusOK {
			t.Errorf("node=%s gave %d, want 200 (body %s)", node, rec.Code, rec.Body.String())
		}
		if f.seen == nil {
			t.Errorf("node=%s never reached Prometheus", node)
		}
	}

	// A well-formed name belonging to some other machine is not.
	for _, node := range []string{"someone-elses-database-host", "prometheus", "10.0.0.5"} {
		f.seen = nil
		rec := get(t, s, "/api/history?metric=cpu&node="+node)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("node=%s gave %d, want 400", node, rec.Code)
		}
		if f.seen != nil {
			t.Errorf("node=%s was queried upstream as %q", node, f.seen.Get("query"))
		}
	}
}

// TestHandleHistory_DefaultsToTheLastHourOfThisNode: the dashboard's first
// request carries a metric and nothing else.
func TestHandleHistory_DefaultsToTheLastHourOfThisNode(t *testing.T) {
	f := newFakePrometheus(t)
	before := time.Now()
	rec := get(t, historyServer(t, f.srv.URL), "/api/history?metric=cpu&node=mon0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if window := time.Duration(got.EndMs-got.StartMs) * time.Millisecond; window != time.Hour {
		t.Errorf("default window = %v, want 1h", window)
	}
	if got.EndMs < before.UnixMilli() {
		t.Errorf("default end = %d, want roughly now", got.EndMs)
	}
	if got.StepMs <= 0 {
		t.Errorf("step = %d; the server must choose one when the caller does not", got.StepMs)
	}
}

// TestHandleMetrics_ExporterHealthOnlyWhenPushing.
//
// A node with no prometheus section must produce exactly the exposition it
// always did -- that is the promise the whole feature rests on -- and a node
// that IS pushing must report whether it is succeeding, because history that
// silently stops being recorded looks the same as history that has not happened
// yet.
func TestHandleMetrics_ExporterHealthOnlyWhenPushing(t *testing.T) {
	snap := &types.Snapshot{Host: types.HostInfo{Uptime: 1}}
	mon := newStaticMonitor(t, snap)

	enabled := ""
	exporter := prom.NewExporter(func() prom.Options {
		return prom.Options{RemoteWriteURL: enabled, SampleInterval: time.Hour, FlushInterval: time.Hour}
	}, func() (prom.Measurement, error) { return prom.Measurement{}, nil }, quietServerLogger())

	s := newServer(Config{Monitor: mon, Exporter: exporter}, nil)

	body := get(t, s, "/metrics").Body.String()
	if strings.Contains(body, "sysmon_remote_write") {
		t.Fatalf("a node that is not pushing exported remote-write series:\n%s", body)
	}

	// Nothing here starts the exporter's loop; Stats() reports what it holds,
	// and what it holds when disabled is "disabled".
	plain := &metrics.TextWriter{}
	metrics.Write(plain, snap)
	if body != plain.String() {
		t.Fatal("the exposition of a standalone node is no longer what pkg/metrics alone produces")
	}
}

// staticSnapshotter always yields the same snapshot.
type staticSnapshotter struct {
	snap *types.Snapshot
}

func (s staticSnapshotter) Snapshot() (*types.Snapshot, error) { return s.snap, nil }

// newStaticMonitor returns a started monitor serving one fixed snapshot.
//
// The interval is short because the poll loop collects on its first tick
// rather than on Start, so a long interval would leave Snapshot answering "not
// running" for the whole test. Re-collecting is harmless: every tick stores the
// same value.
func newStaticMonitor(t *testing.T, snap *types.Snapshot) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(staticSnapshotter{snap: snap}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("monitor.Stop: %v", err)
		}
	})

	// Start returns before the first collection has landed, and a handler that
	// reads too early gets "no snapshot available" instead of the fixture.
	awaitPublications(t, m, 1)
	return m
}

func quietServerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestHandleHistory_ServesTheLocalNodeWithNoFleetOrConfig.
//
// server.Config makes History independent of Fleet and of the configuration
// manager, so a caller embedding this package can wire up history on its own.
// localName() returned "" when neither was supplied, and an empty name is not
// a node, so every history request for the local node was rejected as unknown
// -- the feature was unusable in exactly the configuration the API invites.
//
// The name falls back to the same resolution the configuration makes, which is
// the hostname, and never to an empty string.
func TestHandleHistory_ServesTheLocalNodeWithNoFleetOrConfig(t *testing.T) {
	f := newFakePrometheus(t)
	s := newServer(Config{
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: f.srv.URL, Timeout: 5 * time.Second}
		}),
	}, nil)

	// No node parameter: the request is for whatever this node calls itself.
	rec := get(t, s, "/api/history?metric=disk_used&start=1788300000000&end=1788300600000&step=15000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; a server with History and nothing else "+
			"cannot answer for its own node", rec.Code, rec.Body.String())
	}

	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this machine: %v", err)
	}
	if got, want := f.seen.Get("query"),
		`sysmon_disk_used_percent{instance="`+host+`"}`; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}

	var got types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Node != host {
		t.Errorf("history is labelled %q, want the local hostname %q", got.Node, host)
	}
}

// TestHandleHistory_KnowsTheFleetWithoutAConfigManager: a server embedded as
// a library can be given a fleet and no configuration manager. The fleet then
// says which nodes history may be asked about -- this node and its configured
// peers, polled or not -- and nothing else is forwarded to Prometheus.
func TestHandleHistory_KnowsTheFleetWithoutAConfigManager(t *testing.T) {
	prometheus := newFakePrometheus(t)
	cfg := config.Fleet{Peers: []config.Peer{{Name: "fs", URL: "https://10.0.0.1:8081"}}, PollIntervalMs: 3000, TimeoutMs: 3000}
	poller := fleet.New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "mon0" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv := newServer(Config{
		Fleet: poller,
		History: prom.NewQuerier(func() prom.QueryOptions {
			return prom.QueryOptions{BaseURL: prometheus.srv.URL, Timeout: 5 * time.Second}
		}),
	}, nil)

	for node, want := range map[string]int{
		"mon0":  http.StatusOK,
		"fs":    http.StatusOK,
		"other": http.StatusBadRequest,
	} {
		if rec := get(t, srv, "/api/history?metric=cpu&node="+node); rec.Code != want {
			t.Errorf("history for %q = %d, want %d: %s", node, rec.Code, want, rec.Body.String())
		}
	}
}
