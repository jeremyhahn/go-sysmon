//go:build integration

package prom

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// historyConfig is an aggregator named agg over peers, writing to and
// reading history back from db.
func historyConfig(db *tsdb, peers map[string]string, extra string) string {
	return aggregatorConfig(db.writeURL(), peers) + fmt.Sprintf("  query_url: %q\n%s", db.srv.URL, extra)
}

// history asks for a range and decodes the answer, which must be 200.
func history(t *testing.T, s *harness.Server, metric, node string, start, end time.Time, extra string) types.History {
	t.Helper()
	status, body := historyQuery(t, s, metric, node, start, end, extra)
	if status != http.StatusOK {
		t.Fatalf("GET /api/history = %d: %s", status, body)
	}
	var h types.History
	decodeJSON(t, body, &h)
	return h
}

// setSwap advances the peer's swap-in counter and dates the reading.
func (p *peerNode) setSwap(ts time.Time, pages uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snap.Timestamp = ts
	p.snap.Memory.SwapInPages = pages
}

// TestHistory_ReadsBackWhatWasWritten runs an aggregator that writes a
// peer's readings to the stand-in and draws them back through the history
// API: gauges as they were written, a series per device for a split metric,
// a summed aggregate, and a counter's rate scaled from pages to bytes.
func TestHistory_ReadsBackWhatWasWritten(t *testing.T) {
	db := newTSDB(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)
	peer := newPeerNode(t, gpuBox("gpu-box", t0), 1000)
	agg := serveServer(t, historyConfig(db, map[string]string{"gpu-box": peer.srv.URL}, ""))

	// Three readings a second apart, 10 pages swapped in each second.
	up := labelsOf("__name__", "sysmon_up", "instance", "gpu-box")
	for i := range 3 {
		ts := t0.Add(time.Duration(i) * time.Second)
		peer.setSwap(ts, uint64(10*(i+1)))
		db.await(t, fmt.Sprintf("reading %d to be stored", i), func(db *tsdb) bool {
			_, ok := sampleAt(db, up, ts.UnixMilli())
			return ok
		})
	}
	start, end := t0, t0.Add(2*time.Second)

	var catalog types.HistoryCatalog
	harness.GetJSON(t, agg.URL("/api/history/metrics"), &catalog)
	keys := make([]string, 0, len(catalog.Metrics))
	for _, m := range catalog.Metrics {
		keys = append(keys, m.Key)
	}
	if !catalog.Enabled || !slices.Contains(keys, "gpu_util") || !slices.Contains(keys, "swap_in") {
		t.Errorf("GET /api/history/metrics = enabled %v, keys %v; want history offered", catalog.Enabled, keys)
	}

	h := history(t, agg, "gpu_util", "gpu-box", start, end, "&step=1000")
	if !h.Enabled || h.Node != "gpu-box" || h.Metric != "gpu_util" || h.StepMs != 1000 ||
		h.StartMs != start.UnixMilli() || h.EndMs != end.UnixMilli() || h.Unit != types.HistoryUnitPercent {
		t.Errorf("gpu_util history = %+v, want the range asked for", h)
	}
	if len(h.Series) != 1 || h.Series[0].Name != "0" || len(h.Series[0].Points) != 3 {
		t.Fatalf("gpu_util series = %+v, want GPU 0 at each of three steps", h.Series)
	}
	for _, p := range h.Series[0].Points {
		if p.Value != 73 {
			t.Errorf("gpu_util at %d = %v, want the 73 written", p.TimeMs, p.Value)
		}
	}

	h = history(t, agg, "cpu_power", "gpu-box", start, end, "&step=1000")
	if len(h.Series) != 1 || h.Series[0].Name != "" || h.Series[0].Points[0].Value != 55.5 {
		t.Errorf("cpu_power = %+v, want one summed line of 55.5 W", h.Series)
	}

	h = history(t, agg, "disk_used", "gpu-box", start, end, "&step=1000")
	var names []string
	for _, s := range h.Series {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"nvme0n1", "sda"}) {
		t.Errorf("disk_used lines = %v, want one per device with a filesystem, in order", names)
	}

	// 10 pages a second of 4096 bytes. The first step has no earlier sample
	// to take a rate from, and is a gap rather than a zero.
	h = history(t, agg, "swap_in", "gpu-box", start, end, "&step=1000")
	if h.Unit != types.HistoryUnitBytesPerSecond || len(h.Series) != 1 {
		t.Fatalf("swap_in = %+v, want one line in bytes per second", h)
	}
	points := h.Series[0].Points
	if len(points) != 2 || points[0].TimeMs != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("swap_in points = %+v, want the two steps with a sample before them", points)
	}
	for _, p := range points {
		if math.Abs(p.Value-40960) > 1e-9 {
			t.Errorf("swap_in at %d = %v B/s, want 40960", p.TimeMs, p.Value)
		}
	}
	db.mu.Lock()
	queries := slices.Clone(db.queries)
	db.mu.Unlock()
	if !slices.ContainsFunc(queries, func(q string) bool {
		return strings.HasPrefix(q, `timestamp(sysmon_swap_in_pages_total{instance="gpu-box"})`)
	}) {
		t.Errorf("queries %q: the counter's sample gap was not measured before its rate was taken", queries)
	}

	// A step that is not whole seconds: the window it needs, a step plus a
	// gap, is written in milliseconds.
	h = history(t, agg, "swap_in", "gpu-box", start, end, "&step=1500")
	if len(h.Series) != 1 || len(h.Series[0].Points) != 1 ||
		h.Series[0].Points[0] != (types.HistoryPoint{TimeMs: t0.Add(1500 * time.Millisecond).UnixMilli(), Value: 40960}) {
		t.Errorf("swap_in at a 1.5s step = %+v, want 40960 B/s at the one step with two samples in its window", h.Series)
	}
	db.mu.Lock()
	queries = slices.Clone(db.queries)
	db.mu.Unlock()
	if !slices.ContainsFunc(queries, func(q string) bool { return strings.Contains(q, "[2500ms])") }) {
		t.Errorf("queries %q: want a 2500ms rate window for a 1.5s step over 1s samples", queries)
	}

	// Without a step, one is chosen for the window.
	h = history(t, agg, "load1", "gpu-box", start, end, "")
	if h.StepMs != 1000 || len(h.Series) != 1 {
		t.Errorf("load1 with no step = step %d, %d series; want a 1s step and one line", h.StepMs, len(h.Series))
	}
	// The aggregator's own history, asked for without a node, over an hour
	// with no step: six-second steps draw it in 600 points.
	h = history(t, agg, "load1", "", t0.Add(-time.Hour), t0, "")
	if h.Node != "agg" || h.StepMs != 6000 {
		t.Errorf("the aggregator's own load1 over an hour = node %q, step %dms; want agg at 6000ms", h.Node, h.StepMs)
	}
}

// TestHistory_RefusesWhatItWillNotAsk verifies a request the server will not
// forward is refused as the caller's mistake, with a message saying what to
// ask for instead -- before the database is asked anything.
func TestHistory_RefusesWhatItWillNotAsk(t *testing.T) {
	db := newTSDB(t)
	agg := serveServer(t, historyConfig(db, map[string]string{"gpu-box": deadpeer.URL(t)}, ""))
	end := time.Now()
	start := end.Add(-time.Hour)
	base := fmt.Sprintf("/api/history?start=%d&end=%d", start.UnixMilli(), end.UnixMilli())

	for _, c := range []struct {
		name, path, want string
	}{
		{"unknown metric", base + "&metric=nope", `no history is published for metric "nope"`},
		{"unknown node", base + "&metric=load1&node=stranger", "stranger"},
		{"non-numeric start", "/api/history?metric=load1&start=yesterday", "start must be an integer"},
		{"non-numeric step", base + "&metric=load1&step=fast", "step must be an integer"},
		{"end before start", fmt.Sprintf("/api/history?metric=load1&start=%d&end=%d", end.UnixMilli(), start.UnixMilli()), "end must be after start"},
		{"a window past 90 days", fmt.Sprintf("/api/history?metric=load1&start=%d&end=%d", end.Add(-100*24*time.Hour).UnixMilli(), end.UnixMilli()), "at most"},
		{"more points than Prometheus returns", base + "&metric=load1&step=1", "points at this step"},
		{"a counter step past the read budget", base + "&metric=swap_in&step=" + fmt.Sprint((90 * 24 * time.Hour).Milliseconds()), "exceed"},
		// The range leaves an hour of the 90-day read budget; a 59-minute step
		// fits in it, but the rate window it needs, with two lookbacks, does not.
		// 25 minutes of budget left: a 12-minute step and two 5-minute
		// lookbacks fit, but measuring the gap also reads one lookback before
		// a start aligned back by up to a resolution, which does not.
		{"a counter whose gap measurement reaches past the budget", fmt.Sprintf("/api/history?metric=swap_in&start=%d&end=%d&step=%d", end.Add(-(90*24*time.Hour - 25*time.Minute)).UnixMilli(), end.UnixMilli(), (12 * time.Minute).Milliseconds()), "exceed"},
		{"a counter whose rate window reaches past the budget", fmt.Sprintf("/api/history?metric=swap_in&start=%d&end=%d&step=%d", end.Add(-(90*24-1)*time.Hour).UnixMilli(), end.UnixMilli(), (59 * time.Minute).Milliseconds()), "exceed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := harness.Get(t, agg.URL(c.path))
			if status != http.StatusBadRequest || !strings.Contains(string(body), c.want) {
				t.Errorf("GET %s = %d %q, want 400 naming %q", c.path, status, body, c.want)
			}
		})
	}
}

// TestHistory_DatabaseFailuresAreBadGateway verifies every way the database
// can fail to answer is a 502 that names no address -- the detail goes to the
// log -- and that a missing or non-finite point is a gap rather than a failure.
func TestHistory_DatabaseFailuresAreBadGateway(t *testing.T) {
	db := newTSDB(t)
	agg := serveServer(t, historyConfig(db, nil, "  timeout_ms: 1000\n"))
	end := time.Now()
	start := end.Add(-time.Minute)
	matrix := func(values string) string {
		return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"instance":"agg"},"values":` + values + `}]}}`
	}

	for _, c := range []struct {
		name     string
		status   int
		body     string
		hang     bool
		truncate bool
		metric   string
	}{
		{name: "a query error", status: http.StatusBadRequest, body: `{"status":"error","errorType":"bad_data","error":"parse error"}`},
		{name: "success claimed under a 500", status: http.StatusInternalServerError, body: matrix(`[[1,"1"]]`)},
		{name: "an instant vector", status: http.StatusOK, body: `{"status":"success","data":{"resultType":"vector","result":[]}}`},
		{name: "no result type", status: http.StatusOK, body: `{"status":"success","data":{"result":[]}}`},
		{name: "a body that is not JSON", status: http.StatusOK, body: `<html>proxy error</html>`},
		{name: "no answer before the timeout", hang: true},
		{name: "a body cut off mid-answer", truncate: true},
		{name: "a counter whose sample gap cannot be measured", status: http.StatusBadRequest,
			body: `{"status":"error","errorType":"execution","error":"query timed out"}`, metric: "swap_in"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db.set(func(db *tsdb) {
				db.queryHook = func(w http.ResponseWriter, r *http.Request) {
					if c.hang {
						<-r.Context().Done()
						return
					}
					if c.truncate {
						truncated(t, w, `{"status":"success","data":{"resultType":"matrix","result":[`)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					if _, err := w.Write([]byte(c.body)); err != nil {
						return
					}
				}
			})
			metric := c.metric
			if metric == "" {
				metric = "load1"
			}
			status, body := historyQuery(t, agg, metric, "", start, end, "")
			if status != http.StatusBadGateway || !strings.Contains(string(body), "cannot read history from prometheus") ||
				strings.Contains(string(body), db.srv.URL) {
				t.Errorf("history = %d %q, want 502 without the database's address", status, body)
			}
		})
	}
	agg.Logs.AwaitMsg(t, "server: history query failed")

	db.set(func(db *tsdb) {
		db.queryHook = func(w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{
				"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{},
					"values": []any{[]any{100, "1.5"}, []any{101, "NaN"}, []any{102, "+Inf"}, []any{103, 7}, []any{"x", "2"}, []any{103.5, "abc"}, []any{104, "2.5"}},
				}}}})
		}
	})
	h := history(t, agg, "load1", "", start, end, "")
	if len(h.Series) != 1 || len(h.Series[0].Points) != 2 ||
		h.Series[0].Points[0] != (types.HistoryPoint{TimeMs: 100000, Value: 1.5}) ||
		h.Series[0].Points[1] != (types.HistoryPoint{TimeMs: 104000, Value: 2.5}) {
		t.Errorf("series = %+v, want the two usable points and the rest left as gaps", h.Series)
	}
}

// TestHistory_UnreachableDatabase verifies a query_url nothing answers on is
// a 502 like any other database failure.
func TestHistory_UnreachableDatabase(t *testing.T) {
	agg := serveServer(t, fmt.Sprintf("fleet:\n  local_name: agg\nprometheus:\n  query_url: %q\n  timeout_ms: 1000\n", deadpeer.URL(t)))
	end := time.Now()
	status, body := historyQuery(t, agg, "load1", "", end.Add(-time.Minute), end, "")
	if status != http.StatusBadGateway {
		t.Errorf("history with the database down = %d %q, want 502", status, body)
	}
	status, body = historyQuery(t, agg, "swap_in", "", end.Add(-time.Minute), end, "")
	if status != http.StatusBadGateway {
		t.Errorf("counter history with the database down = %d %q, want 502", status, body)
	}
}

// TestHistory_CounterLookback reads a counter's history from databases that
// publish their query lookback in each of the ways they do -- or do not --
// and checks a counter is drawn exactly when the lookback is known.
func TestHistory_CounterLookback(t *testing.T) {
	raw := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if _, err := w.Write([]byte(body)); err != nil {
				return
			}
		}
	}
	for _, c := range []struct {
		name            string
		flags           map[string]string
		status          int
		hook            http.HandlerFunc
		buildinfo       string
		buildinfoStatus int
		want            int
	}{
		{name: "Prometheus", flags: map[string]string{"query.lookback-delta": "1h30m"}, want: http.StatusOK},
		{name: "a zero lookback, meaning the engine's default", flags: map[string]string{"query.lookback-delta": "0s"}, want: http.StatusOK},
		{name: "Mimir, which publishes an empty flag map", flags: map[string]string{}, buildinfo: "Grafana Mimir", want: http.StatusOK},
		{name: "an empty flag map from something else", flags: map[string]string{}, buildinfo: "VictoriaMetrics", want: http.StatusBadGateway},
		{name: "no flags endpoint", status: http.StatusNotFound, want: http.StatusBadGateway},
		{name: "a lookback that is not a duration", flags: map[string]string{"query.lookback-delta": "a while"}, want: http.StatusBadGateway},
		{name: "a failing flags endpoint", status: http.StatusInternalServerError, want: http.StatusBadGateway},
		{name: "a bare zero", flags: map[string]string{"query.lookback-delta": "0"}, want: http.StatusOK},
		{name: "a lookback too long to hold", flags: map[string]string{"query.lookback-delta": "999999999999y"}, want: http.StatusBadGateway},
		{name: "an empty flag map and a failing buildinfo", flags: map[string]string{}, buildinfoStatus: http.StatusInternalServerError, want: http.StatusBadGateway},
		{name: "flags that are not JSON", hook: raw(http.StatusOK, "<html>login</html>"), want: http.StatusBadGateway},
		{name: "flags that are not a map", hook: raw(http.StatusOK, `{"status":"success","data":"lookback"}`), want: http.StatusBadGateway},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := newTSDB(t)
			db.set(func(db *tsdb) {
				db.flags, db.flagsStatus, db.flagsHook = c.flags, c.status, c.hook
				db.buildinfo, db.buildinfoStatus = c.buildinfo, c.buildinfoStatus
			})
			agg := serveServer(t, fmt.Sprintf("fleet:\n  local_name: agg\nprometheus:\n  query_url: %q\n", db.srv.URL))
			end := time.Now()
			status, body := historyQuery(t, agg, "swap_in", "", end.Add(-time.Minute), end, "")
			if status != c.want {
				t.Fatalf("counter history = %d %q, want %d", status, body, c.want)
			}
			if c.want == http.StatusBadGateway && c.status == 0 && c.hook == nil && c.buildinfoStatus == 0 &&
				!strings.Contains(string(body), "does not publish") {
				t.Errorf("counter history = %q, want the unpublished lookback named", body)
			}
			// A gauge needs no lookback.
			if status, body := historyQuery(t, agg, "load1", "", end.Add(-time.Minute), end, ""); status != http.StatusOK {
				t.Errorf("gauge history = %d %q, want 200 whatever the lookback", status, body)
			}
		})
	}
}

// TestHistory_ACounterOutageIsAGap seeds a counter with two runs of samples
// either side of an outage longer than the database's lookback. The rate is
// drawn on both sides at what it was, and the outage is a gap: it is not a
// cadence to size the rate window from, which would smear the whole chart.
func TestHistory_ACounterOutageIsAGap(t *testing.T) {
	db := newTSDB(t)
	db.set(func(db *tsdb) {
		db.flags = map[string]string{"query.lookback-delta": "5s"}
		db.lookback = 5 * time.Second
	})
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(s int) int64 { return t0.Add(time.Duration(s) * time.Second).UnixMilli() }
	node := labelsOf("instance", "gpu-box", "job", "go-sysmon")
	with := func(name string) map[string]string {
		l := labelsOf("__name__", name)
		for k, v := range node {
			l[k] = v
		}
		return l
	}
	db.seed(with("sysmon_swap_in_pages_total"),
		wireSample{10, at(0)}, wireSample{20, at(1)}, wireSample{110, at(10)}, wireSample{120, at(11)})
	db.seed(with("sysmon_memory_page_size_bytes"),
		wireSample{4096, at(0)}, wireSample{4096, at(1)}, wireSample{4096, at(10)}, wireSample{4096, at(11)})
	agg := serveServer(t, fmt.Sprintf("fleet:\n  local_name: agg\n  peers:\n    - name: gpu-box\n      url: %s\n"+
		"prometheus:\n  query_url: %q\n", deadpeer.URL(t), db.srv.URL))

	h := history(t, agg, "swap_in", "gpu-box", t0, t0.Add(11*time.Second), "&step=1000")
	if len(h.Series) != 1 {
		t.Fatalf("swap_in = %+v, want one line", h.Series)
	}
	want := []types.HistoryPoint{{TimeMs: at(1), Value: 40960}, {TimeMs: at(11), Value: 40960}}
	if !slices.Equal(h.Series[0].Points, want) {
		t.Errorf("swap_in points = %+v, want %+v: the rate either side of the outage, and nothing across it",
			h.Series[0].Points, want)
	}
}

// TestHistory_AbandonedRequestLeavesTheLookbackUncached verifies a request
// the dashboard gives up on while the database's lookback is being fetched
// says nothing about the database: the next request asks again, rather than
// inheriting the abandoned one's failure.
func TestHistory_AbandonedRequestLeavesTheLookbackUncached(t *testing.T) {
	db := newTSDB(t)
	release := make(chan struct{})
	db.set(func(db *tsdb) {
		db.flagsHook = func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeEnvelope(w, http.StatusOK, map[string]any{"status": "success",
				"data": map[string]string{"query.lookback-delta": "5m"}})
		}
	})
	agg := serveServer(t, fmt.Sprintf("fleet:\n  local_name: agg\nprometheus:\n  query_url: %q\n", db.srv.URL))
	end := time.Now()
	url := agg.URL(fmt.Sprintf("/api/history?metric=swap_in&start=%d&end=%d", end.Add(-time.Minute).UnixMilli(), end.UnixMilli()))

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = resp.Body.Close()
		}
		abandoned <- err
	}()
	db.await(t, "the lookback to be asked for", func(db *tsdb) bool { return db.flagsAsked == 1 })
	cancel()
	if err := await.Recv(t, "the abandoned request to end", abandoned); !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned request ended with %v, want it cancelled", err)
	}

	close(release)
	status, body := harness.Get(t, url)
	if status != http.StatusOK {
		t.Fatalf("the next request = %d %q, want the lookback fetched afresh and the range drawn", status, body)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.flagsAsked != 2 {
		t.Errorf("the lookback was asked for %d times, want twice: the abandoned fetch was cached", db.flagsAsked)
	}
}

// truncated answers with a Content-Length longer than the body it sends, then
// drops the connection: a response cut off mid-answer, as from a proxy that
// failed while relaying it.
func truncated(t *testing.T, w http.ResponseWriter, partial string) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Errorf("the stand-in's response writer cannot be hijacked")
		return
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the hijacked connection: %v", err)
		}
	}()
	if _, err := fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		len(partial)+4096, partial); err != nil {
		t.Errorf("write the truncated answer: %v", err)
		return
	}
	if err := buf.Flush(); err != nil {
		t.Errorf("flush the truncated answer: %v", err)
	}
}

// TestHistory_ATwoSocketHostsCoresAreSeparateLines reads back a two-socket
// peer's core temperatures: one line per package and core, named
// "package:core", each with its own socket's readings -- never both Core 0
// merged into one line.
func TestHistory_ATwoSocketHostsCoresAreSeparateLines(t *testing.T) {
	db := newTSDB(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)
	snap := gpuBox("dual", t0)
	snap.Sensors.CoreTemps = []types.CoreTemp{
		{PackageID: 0, CoreID: 0, Label: "Core 0", TempCelsius: 50},
		{PackageID: 1, CoreID: 0, Label: "Core 0", TempCelsius: 60},
	}
	peer := newPeerNode(t, snap, 1000)
	agg := serveServer(t, historyConfig(db, map[string]string{"dual": peer.srv.URL}, ""))
	up := labelsOf("__name__", "sysmon_up", "instance", "dual")
	db.await(t, "the reading to be stored", func(db *tsdb) bool {
		_, ok := sampleAt(db, up, t0.UnixMilli())
		return ok
	})

	h := history(t, agg, "cpu_temperature", "dual", t0, t0.Add(time.Second), "&step=1000")
	got := make(map[string]float64)
	for _, s := range h.Series {
		if len(s.Points) == 0 {
			t.Fatalf("series %q has no points", s.Name)
		}
		got[s.Name] = s.Points[0].Value
	}
	if want := map[string]float64{"0:0": 50, "1:0": 60}; !reflect.DeepEqual(got, want) {
		t.Errorf("cpu_temperature series = %v, want %v", got, want)
	}
}
