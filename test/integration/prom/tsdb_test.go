//go:build integration

package prom

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// tsdb is an in-process stand-in for a Prometheus server.
//
// Its remote-write receiver decodes the snappy-compressed protobuf with its
// own decoder, not pkg/prom's encoder run backwards, and refuses a request the
// way Prometheus does: labels out of order or repeated, an empty label name or
// value, a series without a name, or a sample not newer than one already
// stored -- including a second sample of one series at the same time within
// the same request, whether in one series entry or two. A refused request
// stores nothing. Its query API answers the range queries go-sysmon sends --
// selectors, timestamp(), rate() scaled and gated, and sum() -- from exactly
// the samples it was sent, evaluated with a query lookback like Prometheus'.
type tsdb struct {
	srv *httptest.Server

	mu      sync.Mutex
	changed chan struct{}
	series  map[string]*storedSeries
	// writes are the remote-write requests received, accepted or not.
	writes []writeRequest
	// respond scripts the answer to the n-th write request (from 0). A zero
	// status accepts it.
	respond func(n int) writeAnswer
	// queries are the expressions of every range query received.
	queries []string
	// queryHook, when set, answers a range query instead of the engine.
	queryHook http.HandlerFunc
	// flags is served at /api/v1/status/flags; flagsStatus, when non-zero,
	// is answered instead with an error envelope (or a bare 404).
	flags       map[string]string
	flagsStatus int
	// flagsHook, when set, answers /api/v1/status/flags instead.
	flagsHook http.HandlerFunc
	// flagsAsked counts the requests for the flags.
	flagsAsked int
	// buildinfo is the application /api/v1/status/buildinfo reports; ""
	// answers 404, as VictoriaMetrics' bare version string is not asked for.
	// buildinfoStatus, when non-zero, is answered instead as a failure.
	buildinfo       string
	buildinfoStatus int
	// lookback is the query lookback the engine evaluates with.
	lookback time.Duration
}

// writeAnswer is how one write request is answered.
type writeAnswer struct {
	status   int
	body     string
	location string
}

// writeRequest is one remote-write request as received.
type writeRequest struct {
	header   http.Header
	series   []wireSeries
	status   int
	rejected string
}

// wireSeries is one TimeSeries of a WriteRequest.
type wireSeries struct {
	labels  []wireLabel
	samples []wireSample
}

type wireLabel struct{ name, value string }

type wireSample struct {
	value float64
	ms    int64
}

// storedSeries is one series in the database.
type storedSeries struct {
	labels  map[string]string
	samples []wireSample
}

// newTSDB starts the stand-in with a five-minute lookback published in its
// flags, as a default Prometheus has.
func newTSDB(t *testing.T) *tsdb {
	t.Helper()
	db := &tsdb{
		changed:  make(chan struct{}),
		series:   make(map[string]*storedSeries),
		flags:    map[string]string{"query.lookback-delta": "5m"},
		lookback: 5 * time.Minute,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/write", db.handleWrite)
	mux.HandleFunc("GET /api/v1/query_range", db.handleQueryRange)
	mux.HandleFunc("GET /api/v1/status/flags", db.handleFlags)
	mux.HandleFunc("GET /api/v1/status/buildinfo", db.handleBuildinfo)
	db.srv = httptest.NewServer(mux)
	t.Cleanup(db.srv.Close)
	return db
}

// writeURL is the receiver's remote-write endpoint.
func (db *tsdb) writeURL() string { return db.srv.URL + "/api/v1/write" }

// set changes the stand-in's behaviour under its lock.
func (db *tsdb) set(fn func(db *tsdb)) {
	db.mu.Lock()
	defer db.mu.Unlock()
	fn(db)
}

// notify wakes every waiter. The caller holds mu.
func (db *tsdb) notify() {
	close(db.changed)
	db.changed = make(chan struct{})
}

// await returns once cond holds, re-checking it on every change to the
// database, and fails the test if it has not within await.Timeout of being
// called -- one bound for the whole wait, however often the database changes
// meanwhile. The failure says what the database holds.
func (db *tsdb) await(t *testing.T, what string, cond func(db *tsdb) bool) {
	t.Helper()
	bound := time.NewTimer(await.Timeout)
	defer bound.Stop()
	for {
		db.mu.Lock()
		ok := cond(db)
		changed := db.changed
		db.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-changed:
		case <-bound.C:
			db.mu.Lock()
			defer db.mu.Unlock()
			t.Fatalf("await: %s did not happen within %v; the database holds %s", what, await.Timeout, db.describe())
		}
	}
}

// describe summarises what the database holds, for a failure message. The
// caller holds mu.
func (db *tsdb) describe() string {
	instances := make(map[string]int)
	for _, s := range db.series {
		instances[s.labels["instance"]] += len(s.samples)
	}
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(db.writes)) + " writes (" + strconv.Itoa(db.accepted()) + " accepted); samples by instance ")
	fmt.Fprint(&b, instances)
	for i, w := range db.writes {
		if w.rejected != "" {
			b.WriteString("; write " + strconv.Itoa(i) + " rejected: " + w.rejected)
		}
	}
	return b.String()
}

// matching returns the stored series whose labels include every pair in
// want. The caller holds mu.
func (db *tsdb) matching(want map[string]string) []*storedSeries {
	var out []*storedSeries
	for _, s := range db.series {
		ok := true
		for k, v := range want {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return seriesKey(out[i].labels) < seriesKey(out[j].labels) })
	return out
}

// sampleTimes returns the distinct sample times stored for series matching
// want. The caller holds mu.
func (db *tsdb) sampleTimes(want map[string]string) []int64 {
	seen := make(map[int64]bool)
	for _, s := range db.matching(want) {
		for _, p := range s.samples {
			seen[p.ms] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for ms := range seen {
		out = append(out, ms)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// requestTimes returns the sample times of series named name in write
// request n. The caller holds mu.
func (db *tsdb) requestTimes(n int, name string) []int64 {
	var out []int64
	for _, s := range db.writes[n].series {
		for _, l := range s.labels {
			if l.name == "__name__" && l.value == name {
				for _, p := range s.samples {
					out = append(out, p.ms)
				}
			}
		}
	}
	return out
}

// seed stores samples of one series directly, as another writer would have.
func (db *tsdb) seed(labels map[string]string, samples ...wireSample) {
	db.mu.Lock()
	defer db.mu.Unlock()
	key := seriesKey(labels)
	stored := db.series[key]
	if stored == nil {
		stored = &storedSeries{labels: labels}
		db.series[key] = stored
	}
	stored.samples = append(stored.samples, samples...)
	db.notify()
}

// accepted counts the write requests stored.
func (db *tsdb) accepted() int {
	n := 0
	for _, w := range db.writes {
		if w.status/100 == 2 {
			n++
		}
	}
	return n
}

// ── Remote write ─────────────────────────────────────────────────────────────

func (db *tsdb) handleWrite(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := writeRequest{header: r.Header.Clone()}
	db.mu.Lock()
	defer db.mu.Unlock()
	defer db.notify()

	n := len(db.writes)
	var answer writeAnswer
	if db.respond != nil {
		answer = db.respond(n)
	}
	if answer.status == 0 {
		answer = db.store(&req, raw)
	} else if body, err := snappy.Decode(nil, raw); err == nil {
		// Decoded all the same, so a test can see what a refused request
		// carried.
		if series, err := decodeWriteRequest(body); err == nil {
			req.series = series
		}
	}
	req.status = answer.status
	db.writes = append(db.writes, req)

	if answer.location != "" {
		w.Header().Set("Location", answer.location)
	}
	w.WriteHeader(answer.status)
	if answer.body != "" {
		if _, err := io.WriteString(w, answer.body); err != nil {
			return
		}
	}
}

// store decodes and checks a write the way Prometheus does, keeping its
// samples only when every series in it is acceptable. The caller holds mu.
func (db *tsdb) store(req *writeRequest, raw []byte) writeAnswer {
	reject := func(reason string) writeAnswer {
		req.rejected = reason
		return writeAnswer{status: http.StatusBadRequest, body: reason}
	}
	if req.header.Get("Content-Encoding") != "snappy" {
		return reject("unsupported content encoding")
	}
	body, err := snappy.Decode(nil, raw)
	if err != nil {
		return reject("snappy: " + err.Error())
	}
	series, err := decodeWriteRequest(body)
	if err != nil {
		return reject("protobuf: " + err.Error())
	}
	req.series = series

	newest := make(map[string]int64)
	for _, s := range series {
		labels := make(map[string]string, len(s.labels))
		for i, l := range s.labels {
			if l.name == "" || l.value == "" {
				return reject("empty label name or value")
			}
			if i > 0 && s.labels[i-1].name >= l.name {
				return reject("out-of-order or duplicate label " + strconv.Quote(l.name))
			}
			labels[l.name] = l.value
		}
		if labels["__name__"] == "" {
			return reject("series without a metric name")
		}
		key := seriesKey(labels)
		last, ok := newest[key]
		if !ok {
			if stored := db.series[key]; stored != nil && len(stored.samples) > 0 {
				last = stored.samples[len(stored.samples)-1].ms
			}
		}
		for _, p := range s.samples {
			if p.ms <= last {
				return reject("out of order sample for " + key)
			}
			last = p.ms
		}
		newest[key] = last
	}
	for _, s := range series {
		labels := make(map[string]string, len(s.labels))
		for _, l := range s.labels {
			labels[l.name] = l.value
		}
		key := seriesKey(labels)
		stored := db.series[key]
		if stored == nil {
			stored = &storedSeries{labels: labels}
			db.series[key] = stored
		}
		stored.samples = append(stored.samples, s.samples...)
	}
	return writeAnswer{status: http.StatusNoContent}
}

// seriesKey renders a label set canonically.
func seriesKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + "=" + strconv.Quote(labels[k]) + ",")
	}
	return b.String()
}

// errProtobuf is a malformed protobuf message.
var errProtobuf = errors.New("malformed message")

// protoField is one field of a protobuf message.
type protoField struct {
	num   uint64
	wire  uint64
	value uint64
	bytes []byte
}

// protoFields splits a message into its fields.
func protoFields(b []byte) ([]protoField, error) {
	var out []protoField
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errProtobuf
		}
		b = b[n:]
		f := protoField{num: key >> 3, wire: key & 7}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errProtobuf
			}
			f.value, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errProtobuf
			}
			f.value, b = binary.LittleEndian.Uint64(b), b[8:]
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < size {
				return nil, errProtobuf
			}
			f.bytes, b = b[n:n+int(size)], b[n+int(size):]
		default:
			return nil, errProtobuf
		}
		out = append(out, f)
	}
	return out, nil
}

// decodeWriteRequest reads a remote-write v1 WriteRequest.
func decodeWriteRequest(b []byte) ([]wireSeries, error) {
	fields, err := protoFields(b)
	if err != nil {
		return nil, err
	}
	var out []wireSeries
	for _, f := range fields {
		if f.num != 1 || f.wire != 2 {
			continue
		}
		s, err := decodeTimeSeries(f.bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func decodeTimeSeries(b []byte) (wireSeries, error) {
	fields, err := protoFields(b)
	if err != nil {
		return wireSeries{}, err
	}
	var s wireSeries
	for _, f := range fields {
		if f.wire != 2 {
			return wireSeries{}, errProtobuf
		}
		inner, err := protoFields(f.bytes)
		if err != nil {
			return wireSeries{}, err
		}
		switch f.num {
		case 1:
			var l wireLabel
			for _, g := range inner {
				switch g.num {
				case 1:
					l.name = string(g.bytes)
				case 2:
					l.value = string(g.bytes)
				}
			}
			s.labels = append(s.labels, l)
		case 2:
			var p wireSample
			for _, g := range inner {
				switch g.num {
				case 1:
					p.value = math.Float64frombits(g.value)
				case 2:
					p.ms = int64(g.value)
				}
			}
			s.samples = append(s.samples, p)
		}
	}
	return s, nil
}

// ── Status endpoints ─────────────────────────────────────────────────────────

func (db *tsdb) handleFlags(w http.ResponseWriter, r *http.Request) {
	db.mu.Lock()
	status, flags, hook := db.flagsStatus, db.flags, db.flagsHook
	db.flagsAsked++
	db.notify()
	db.mu.Unlock()
	switch {
	case hook != nil:
		hook(w, r)
	case status == http.StatusNotFound:
		http.NotFound(w, nil)
	case status != 0:
		writeEnvelope(w, status, map[string]any{"status": "error", "errorType": "internal", "error": "flags unavailable"})
	default:
		writeEnvelope(w, http.StatusOK, map[string]any{"status": "success", "data": flags})
	}
}

func (db *tsdb) handleBuildinfo(w http.ResponseWriter, _ *http.Request) {
	db.mu.Lock()
	app, status := db.buildinfo, db.buildinfoStatus
	db.mu.Unlock()
	if status != 0 {
		writeEnvelope(w, status, map[string]any{"status": "error", "errorType": "internal", "error": "buildinfo unavailable"})
		return
	}
	if app == "" {
		http.NotFound(w, nil)
		return
	}
	writeEnvelope(w, http.StatusOK, map[string]any{"status": "success",
		"data": map[string]string{"application": app, "version": "2.14.0"}})
}

// writeEnvelope writes v as JSON with status.
func writeEnvelope(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// ── Range queries ────────────────────────────────────────────────────────────

func (db *tsdb) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	expr := q.Get("query")
	db.mu.Lock()
	db.queries = append(db.queries, expr)
	hook := db.queryHook
	db.notify()
	db.mu.Unlock()
	if hook != nil {
		hook(w, r)
		return
	}

	start, errStart := strconv.ParseFloat(q.Get("start"), 64)
	end, errEnd := strconv.ParseFloat(q.Get("end"), 64)
	step, errStep := strconv.ParseFloat(q.Get("step"), 64)
	if errStart != nil || errEnd != nil || errStep != nil || step <= 0 {
		writeEnvelope(w, http.StatusBadRequest, map[string]any{"status": "error", "errorType": "bad_data",
			"error": "cannot parse start, end or step"})
		return
	}
	node, err := parseExpr(expr)
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, map[string]any{"status": "error", "errorType": "bad_data",
			"error": err.Error()})
		return
	}

	db.mu.Lock()
	lines := make(map[string]*matrixLine)
	var order []string
	stepMs := int64(math.Round(step * 1000))
	for t := int64(math.Round(start * 1000)); t <= int64(math.Round(end*1000)); t += stepMs {
		for _, e := range node.eval(db, t) {
			key := seriesKey(e.labels)
			line := lines[key]
			if line == nil {
				line = &matrixLine{Metric: e.labels}
				lines[key] = line
				order = append(order, key)
			}
			line.Values = append(line.Values, [2]any{float64(t) / 1000,
				strconv.FormatFloat(e.value, 'f', -1, 64)})
		}
	}
	db.mu.Unlock()

	result := make([]*matrixLine, 0, len(order))
	for _, key := range order {
		result = append(result, lines[key])
	}
	writeEnvelope(w, http.StatusOK, map[string]any{"status": "success",
		"data": map[string]any{"resultType": "matrix", "result": result}})
}

// matrixLine is one series of a matrix answer.
type matrixLine struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

// element is one series' value at one evaluation instant.
type element struct {
	labels map[string]string
	value  float64
}

// exprNode is a parsed expression.
type exprNode interface {
	eval(db *tsdb, tMs int64) []element
}

// matcher is one label matcher of a selector.
type matcher struct {
	name  string
	value string
	re    *regexp.Regexp
	// negate is a != matcher. A label a series does not carry has the value
	// "", as in Prometheus, so label!="" selects the series that carry it.
	negate bool
}

func (m matcher) matches(labels map[string]string) bool {
	if m.re != nil {
		return m.re.MatchString(labels[m.name])
	}
	return (labels[m.name] == m.value) != m.negate
}

// selector is an instant vector selector: name{matchers}.
type selector struct {
	name     string
	matchers []matcher
}

// seriesFor returns the stored series the selector matches. The caller holds
// mu.
func (s selector) seriesFor(db *tsdb) []*storedSeries {
	var out []*storedSeries
	for _, st := range db.series {
		if st.labels["__name__"] != s.name {
			continue
		}
		ok := true
		for _, m := range s.matchers {
			if !m.matches(st.labels) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, st)
		}
	}
	return out
}

// latest is the newest sample at or before tMs within the lookback.
func latest(st *storedSeries, tMs int64, lookback time.Duration) (wireSample, bool) {
	var found wireSample
	ok := false
	for _, p := range st.samples {
		if p.ms <= tMs && p.ms > tMs-lookback.Milliseconds() {
			found, ok = p, true
		}
	}
	return found, ok
}

func (s selector) eval(db *tsdb, tMs int64) []element {
	var out []element
	for _, st := range s.seriesFor(db) {
		if p, ok := latest(st, tMs, db.lookback); ok {
			out = append(out, element{labels: st.labels, value: p.value})
		}
	}
	return out
}

// withoutName is a label set with __name__ removed, as a function's result
// carries it.
func withoutName(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k != "__name__" {
			out[k] = v
		}
	}
	return out
}

// timestampFn is timestamp(selector).
type timestampFn struct{ sel selector }

func (f timestampFn) eval(db *tsdb, tMs int64) []element {
	var out []element
	for _, st := range f.sel.seriesFor(db) {
		if p, ok := latest(st, tMs, db.lookback); ok {
			out = append(out, element{labels: withoutName(st.labels), value: float64(p.ms) / 1000})
		}
	}
	return out
}

// rateFn is rate(selector[window]), optionally multiplied by an instant
// vector and gated by another, matched on every label but the name.
type rateFn struct {
	sel    selector
	window time.Duration
	scale  *selector
	and    *selector
}

func (f rateFn) eval(db *tsdb, tMs int64) []element {
	var out []element
	for _, st := range f.sel.seriesFor(db) {
		var in []wireSample
		for _, p := range st.samples {
			if p.ms <= tMs && p.ms > tMs-f.window.Milliseconds() {
				in = append(in, p)
			}
		}
		if len(in) < 2 {
			continue
		}
		increase := 0.0
		for i := 1; i < len(in); i++ {
			d := in[i].value - in[i-1].value
			if d < 0 {
				d = in[i].value // a counter reset
			}
			increase += d
		}
		secs := float64(in[len(in)-1].ms-in[0].ms) / 1000
		out = append(out, element{labels: withoutName(st.labels), value: increase / secs})
	}
	if f.scale != nil {
		out = joinOn(out, f.scale.eval(db, tMs), func(l, r float64) float64 { return l * r })
	}
	if f.and != nil {
		out = joinOn(out, f.and.eval(db, tMs), func(l, _ float64) float64 { return l })
	}
	return out
}

// joinOn keeps each left element with a right element of the same labels
// apart from the name, combining their values.
func joinOn(left, right []element, combine func(l, r float64) float64) []element {
	byKey := make(map[string]float64, len(right))
	for _, r := range right {
		byKey[seriesKey(withoutName(r.labels))] = r.value
	}
	var out []element
	for _, l := range left {
		if v, ok := byKey[seriesKey(withoutName(l.labels))]; ok {
			out = append(out, element{labels: l.labels, value: combine(l.value, v)})
		}
	}
	return out
}

// sumFn is sum(expr).
type sumFn struct{ inner exprNode }

func (f sumFn) eval(db *tsdb, tMs int64) []element {
	in := f.inner.eval(db, tMs)
	if len(in) == 0 {
		return nil
	}
	total := 0.0
	for _, e := range in {
		total += e.value
	}
	return []element{{labels: map[string]string{}, value: total}}
}

// The expression forms pkg/prom's catalog builds.
var (
	selectorPattern  = `([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}`
	selectorRE       = regexp.MustCompile(`^` + selectorPattern + `$`)
	timestampRE      = regexp.MustCompile(`^timestamp\(` + selectorPattern + `\)$`)
	sumRE            = regexp.MustCompile(`^sum\((.*)\)$`)
	rateRE           = regexp.MustCompile(`^rate\(` + selectorPattern + `\[(\d+)(ms|s)\]\)(?: \* ` + selectorPattern + `)?(?: and ` + selectorPattern + `)?$`)
	matcherRE        = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(=~|!=|=)"((?:[^"\\]|\\.)*)"$`)
	errUnsupportedQL = errors.New("parse error: unsupported expression")
)

// parseExpr parses the expressions the history endpoint sends.
func parseExpr(expr string) (exprNode, error) {
	if m := sumRE.FindStringSubmatch(expr); m != nil {
		inner, err := parseExpr(m[1])
		if err != nil {
			return nil, err
		}
		return sumFn{inner: inner}, nil
	}
	if m := timestampRE.FindStringSubmatch(expr); m != nil {
		sel, err := parseSelector(m[1], m[2])
		return timestampFn{sel: sel}, err
	}
	if m := rateRE.FindStringSubmatch(expr); m != nil {
		sel, err := parseSelector(m[1], m[2])
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil {
			return nil, err
		}
		unit := time.Second
		if m[4] == "ms" {
			unit = time.Millisecond
		}
		f := rateFn{sel: sel, window: time.Duration(n) * unit}
		if m[5] != "" {
			scale, err := parseSelector(m[5], m[6])
			if err != nil {
				return nil, err
			}
			f.scale = &scale
		}
		if m[7] != "" {
			and, err := parseSelector(m[7], m[8])
			if err != nil {
				return nil, err
			}
			f.and = &and
		}
		return f, nil
	}
	if m := selectorRE.FindStringSubmatch(expr); m != nil {
		return parseSelector(m[1], m[2])
	}
	return nil, errUnsupportedQL
}

// parseSelector reads a selector's name and matcher list.
func parseSelector(name, list string) (selector, error) {
	sel := selector{name: name}
	for _, part := range strings.Split(list, ",") {
		m := matcherRE.FindStringSubmatch(part)
		if m == nil {
			return selector{}, errUnsupportedQL
		}
		value, err := strconv.Unquote(`"` + m[3] + `"`)
		if err != nil {
			return selector{}, err
		}
		mt := matcher{name: m[1], value: value, negate: m[2] == "!="}
		if m[2] == "=~" {
			re, err := regexp.Compile("^(?:" + value + ")$")
			if err != nil {
				return selector{}, err
			}
			mt.re = re
		}
		sel.matchers = append(sel.matchers, mt)
	}
	return sel, nil
}
