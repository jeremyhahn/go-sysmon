package prom

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The query lookback.
//
// Prometheus answers an evaluation at t with a series' newest sample in the
// lookback before t, and treats a series with none there as absent. Two things
// here depend on how long that is: a counter's rate window is measured against
// it (a hole longer than the lookback is an outage, not a cadence -- see
// SampleGap), and pkg/config refuses to write history more slowly than it,
// since no chart could draw what was written. So it is read from the database
// rather than assumed.
//
// What each backend answers, from its own source and documentation:
//
//   - Prometheus serves GET /api/v1/status/flags with "query.lookback-delta".
//   - Thanos serves the same endpoint on every component with an HTTP API
//     (v0.15.0 on), from the same flag map.
//   - Either may report "0s". The PromQL engine replaces a zero lookback with
//     its default, five minutes (promql.NewEngine, defaultLookbackDelta), and
//     Thanos documents the same: "If unset it will use the promql default of
//     5m". A zero is therefore read as five minutes, because that is what the
//     engine answering the queries uses -- not as a guess.
//   - Grafana Mimir serves /api/v1/status/flags with an EMPTY map, by design:
//     "it exists only to be compatible with the Prometheus API". Its lookback
//     is -querier.lookback-delta, documented default 5m, and is not published.
//     A database identifying itself as Mimir in /api/v1/status/buildinfo
//     ("application") is read as having that documented default. An operator
//     who has changed it on Mimir is not visible from here; that is Mimir's
//     omission, and it is stated where the value is used.
//   - VictoriaMetrics does not serve /api/v1/status/flags (404), and has no
//     fixed lookback to publish: its range queries follow the median interval
//     between samples. Its /api/v1/status/buildinfo is a bare Prometheus
//     version string, so it cannot be told apart from anything else that
//     answers 404 -- and none of them is given a number it did not publish.
//
// Anything that publishes no lookback is reported as *ErrLookbackUnpublished,
// and the callers say what they cannot do without one.

// promqlDefaultLookback is the lookback the PromQL engine uses when its own is
// zero, and Thanos' documented default: promql/engine.go defaultLookbackDelta.
const promqlDefaultLookback = 5 * time.Minute

// mimirDefaultLookback is Grafana Mimir's documented -querier.lookback-delta
// default. Mimir does not publish the value in effect; see above.
const mimirDefaultLookback = 5 * time.Minute

// lookbackTTL is how long a lookback read from the database is trusted. It is
// a server flag, changed only by restarting the database, so ten minutes of
// staleness costs nothing while sparing a flags request per chart.
const lookbackTTL = 10 * time.Minute

// lookbackRetry is how soon a failure to reach the database is retried. A
// refused connection is not a property of the database, so it is not held for
// long; a lookback the database does not publish is, and gets lookbackTTL.
const lookbackRetry = 30 * time.Second

// lookbackEntry is one cached answer for one database.
type lookbackEntry struct {
	value   time.Duration
	err     error
	expires time.Time
	// known is the last lookback the database actually gave, carried past
	// later failures and expiry until it gives another; zero before it ever
	// has. See CachedLookbackAt.
	known time.Duration
}

// lookbackCache holds the answers per database base URL. A sync.Map because
// entries are written rarely and read on every counter chart, from many
// requests at once, and the key set is one or two URLs.
type lookbackCache struct {
	entries sync.Map // base URL -> *lookbackEntry
	// fetching holds the refresh in flight per base URL, so concurrent
	// callers share one request to the database -- a slow one is asked once,
	// not once per chart -- and results are published in the order fetched.
	fetching sync.Map // base URL -> *lookbackFetch
}

// lookbackFetch is one refresh in flight: done is closed when entry is set.
type lookbackFetch struct {
	done  chan struct{}
	entry *lookbackEntry
	// cancelled is set when the fetch ended with its caller's cancellation,
	// which says nothing about the database; a waiter then fetches afresh.
	cancelled bool
}

// Lookback returns the query lookback of the database this querier reads from.
func (q *Querier) Lookback(ctx context.Context) (time.Duration, error) {
	return q.LookbackAt(ctx, q.URL())
}

// LookbackAt returns the query lookback of the database at base -- the server,
// not one of its endpoints, as for QueryOptions.BaseURL -- through this
// querier's client and cache. It is exported for the configuration check,
// which validates a configuration that is not live yet and so may name a
// database the querier is not reading from.
func (q *Querier) LookbackAt(ctx context.Context, base string) (time.Duration, error) {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return 0, &ErrNoQueryEndpoint{}
	}
	now := q.now()
	if cached, ok := q.lookbacks.entries.Load(base); ok {
		if e := cached.(*lookbackEntry); now.Before(e.expires) {
			return e.value, e.err
		}
	}

	// Join a refresh already in flight rather than start another.
	f := &lookbackFetch{done: make(chan struct{})}
	if running, loaded := q.lookbacks.fetching.LoadOrStore(base, f); loaded {
		other := running.(*lookbackFetch)
		select {
		case <-other.done:
		case <-ctx.Done():
			return 0, &ErrRequest{URL: base + "/api/v1/status/flags", Err: ctx.Err()}
		}
		if !other.cancelled {
			return other.entry.value, other.entry.err
		}
		return q.LookbackAt(ctx, base)
	}
	defer func() {
		q.lookbacks.fetching.Delete(base)
		close(f.done)
	}()

	value, err := q.fetchLookback(ctx, base)
	f.entry = &lookbackEntry{value: value, err: err}
	// A request its caller gave up on -- the dashboard cancels one whenever
	// the range or the node changes -- says nothing about the database, and
	// is not cached as though it did: the request replacing it would inherit
	// the failure. A deadline that ran out does say something -- the database
	// did not answer in time -- and is cached like any failure, so a database
	// that blackholes is not asked again by every request for the full
	// timeout.
	if errors.Is(ctx.Err(), context.Canceled) {
		f.cancelled = true
		return value, err
	}
	ttl := q.lookbackTTL
	var unpublished *ErrLookbackUnpublished
	if err != nil && !errors.As(err, &unpublished) {
		ttl = q.lookbackRetry
	}
	entry := &lookbackEntry{value: value, err: err, expires: now.Add(ttl), known: value}
	if err != nil {
		entry.known = 0
		if prev, ok := q.lookbacks.entries.Load(base); ok {
			entry.known = prev.(*lookbackEntry).known
		}
	}
	q.lookbacks.entries.Store(base, entry)
	f.entry = entry
	return value, err
}

// CachedLookbackAt returns the last lookback the database at base gave,
// without asking it anything: false only when it has never given one. It is
// for callers that must not wait on the database -- configuration
// validation, which startup and reloads never block on, and the fleet's
// history gate, which runs in the poll -- and leave the asking to something
// that can.
//
// The last value is held through a failed refresh and past its expiry, until
// the database gives another. A check that lapsed to "unknown" whenever the
// database was briefly unreachable would fail open: a peer refused for
// collecting too slowly would be relayed again for exactly as long as nobody
// could confirm the refusal.
func (q *Querier) CachedLookbackAt(base string) (time.Duration, bool) {
	cached, ok := q.lookbacks.entries.Load(strings.TrimRight(base, "/"))
	if !ok {
		return 0, false
	}
	known := cached.(*lookbackEntry).known
	return known, known > 0
}

// fetchLookback asks the database at base for its lookback.
func (q *Querier) fetchLookback(ctx context.Context, base string) (time.Duration, error) {
	var flags map[string]string
	found, err := q.statusGet(ctx, base+"/api/v1/status/flags", &flags)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, &ErrLookbackUnpublished{Reason: "it does not serve /api/v1/status/flags"}
	}
	if raw, ok := flags["query.lookback-delta"]; ok {
		d, err := parsePromDuration(raw)
		if err != nil {
			return 0, &ErrLookbackUnpublished{
				Reason: "its query.lookback-delta, " + strconv.Quote(raw) + ", is not a duration",
			}
		}
		if d == 0 {
			return promqlDefaultLookback, nil
		}
		return d, nil
	}

	var build struct {
		Application string `json:"application"`
	}
	found, err = q.statusGet(ctx, base+"/api/v1/status/buildinfo", &build)
	if err != nil {
		return 0, err
	}
	if found && strings.Contains(build.Application, "Mimir") {
		return mimirDefaultLookback, nil
	}
	return 0, &ErrLookbackUnpublished{Reason: "its /api/v1/status/flags does not list query.lookback-delta"}
}

// statusGet reads one status endpoint's data into out. found is false when the
// endpoint does not exist (404); any other failure is an error.
func (q *Querier) statusGet(ctx context.Context, endpoint string, out any) (found bool, err error) {
	// The caller's deadline wins when it has one. A reload validates a
	// configuration that is not live yet, under ITS timeout; the querier's
	// options still hold the old one, and a shorter old one would fail the
	// check the new configuration would have passed.
	if _, ok := ctx.Deadline(); !ok && q.opts().Timeout > 0 {
		timeout := q.opts().Timeout
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, &ErrRequest{URL: endpoint, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := q.client.Do(req)
	if err != nil {
		return false, &ErrRequest{URL: endpoint, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	var envelope struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		ErrorType string          `json:"errorType"`
		Error     string          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, statusBodyLimit)).Decode(&envelope); err != nil {
		return false, &ErrDecode{URL: endpoint, Err: err}
	}
	if resp.StatusCode/100 != 2 || envelope.Status != "success" {
		return false, &ErrQuery{
			Query:  endpoint,
			Type:   "http_" + strconv.Itoa(resp.StatusCode),
			Detail: envelope.Error,
		}
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return false, &ErrDecode{URL: endpoint, Err: err}
	}
	return true, nil
}

// statusBodyLimit bounds a status response. Prometheus' flag map is a few
// kilobytes; anything far past that is not a status endpoint answering.
const statusBodyLimit = 1 << 20

// promDurationPattern is Prometheus' duration syntax (model.ParseDuration):
// units from years down to milliseconds, each at most once, largest first.
var promDurationPattern = regexp.MustCompile(
	`^(?:(\d+)y)?(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?(?:(\d+)ms)?$`)

// promDurationUnits are the units of promDurationPattern's groups, in order.
// A Prometheus year is 365 days and a week 7, with no calendar in either.
var promDurationUnits = []time.Duration{
	365 * 24 * time.Hour, 7 * 24 * time.Hour, 24 * time.Hour,
	time.Hour, time.Minute, time.Second, time.Millisecond,
}

// parsePromDuration reads a duration in Prometheus' syntax -- "5m", "1h30m",
// "0s" -- which is what status/flags reports. "0" alone is accepted, as
// Prometheus accepts it.
func parsePromDuration(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	m := promDurationPattern.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, &ErrDuration{Value: s}
	}
	var d time.Duration
	for i, unit := range promDurationUnits {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || time.Duration(n) > (1<<63-1-d)/unit {
			return 0, &ErrDuration{Value: s}
		}
		d += time.Duration(n) * unit
	}
	return d, nil
}
