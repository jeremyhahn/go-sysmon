package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
)

// statusAnswers is how a backend answers the two status endpoints the
// lookback is read from.
type statusAnswers struct {
	flags     int // status code for /status/flags
	flagsBody string
	build     int
	buildBody string
}

// statusServer serves statusAnswers, which a test may change between
// requests, and counts the flags requests.
type statusServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	answers  statusAnswers
	requests atomic.Int64
}

func newStatusServer(t *testing.T, a statusAnswers) *statusServer {
	t.Helper()
	s := &statusServer{answers: a}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		a := s.answers
		s.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/status/flags":
			s.requests.Add(1)
			w.WriteHeader(a.flags)
			if _, err := w.Write([]byte(a.flagsBody)); err != nil {
				t.Errorf("write response: %v", err)
			}
		case "/api/v1/status/buildinfo":
			w.WriteHeader(a.build)
			if _, err := w.Write([]byte(a.buildBody)); err != nil {
				t.Errorf("write response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// set changes how the server answers from the next request on.
func (s *statusServer) set(a statusAnswers) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = a
}

// TestLookback_WhatEachBackendAnswers is each backend's actual answer, from
// its source and documentation (see lookback.go), and what is made of it.
func TestLookback_WhatEachBackendAnswers(t *testing.T) {
	const mimirBuild = `{"status":"success","data":{"application":"Grafana Mimir","version":"2.14.0"}}`
	cases := []struct {
		name string
		s    statusAnswers
		want time.Duration
		// unpublished, when set, is the expected refusal instead of a value.
		unpublished bool
	}{
		{name: "Prometheus at its default", s: statusAnswers{flags: 200,
			flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m","log.level":"info"}}`},
			want: 5 * time.Minute},
		{name: "Prometheus with the lookback raised", s: statusAnswers{flags: 200,
			flagsBody: `{"status":"success","data":{"query.lookback-delta":"1h30m"}}`},
			want: 90 * time.Minute},
		// Thanos reports its flag unset as zero; the engine then runs at the
		// PromQL default, which Thanos documents.
		{name: "Thanos with the lookback unset", s: statusAnswers{flags: 200,
			flagsBody: `{"status":"success","data":{"query.lookback-delta":"0s"}}`},
			want: 5 * time.Minute},
		// Mimir serves flags empty by design, and names itself in buildinfo:
		// its documented default applies.
		{name: "Mimir", s: statusAnswers{flags: 200, flagsBody: `{"status":"success","data":{}}`,
			build: 200, buildBody: mimirBuild},
			want: 5 * time.Minute},
		// VictoriaMetrics serves no flags endpoint and has no fixed lookback.
		{name: "VictoriaMetrics", s: statusAnswers{flags: 404, flagsBody: "404 page not found",
			build: 200, buildBody: `{"status":"success","data":{"version":"2.24.0"}}`},
			unpublished: true},
		// Empty flags from something that is not Mimir: nothing to go on.
		{name: "empty flags from an unknown backend", s: statusAnswers{flags: 200,
			flagsBody: `{"status":"success","data":{}}`, build: 404},
			unpublished: true},
		{name: "a lookback that is not a duration", s: statusAnswers{flags: 200,
			flagsBody: `{"status":"success","data":{"query.lookback-delta":"five minutes"}}`},
			unpublished: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStatusServer(t, c.s)
			got, err := querier(s.srv.URL, 5*time.Second).Lookback(context.Background())
			if c.unpublished {
				var unpublished *ErrLookbackUnpublished
				if !errors.As(err, &unpublished) {
					t.Fatalf("Lookback() = %v, %v; want *ErrLookbackUnpublished", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Lookback() = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// TestLookback_FailuresAreErrors. A database that cannot be reached, or that
// answers the flags request with an error, has not said it publishes nothing:
// that is a failure to ask, and is reported as one.
func TestLookback_FailuresAreErrors(t *testing.T) {
	s := newStatusServer(t, statusAnswers{flags: 500,
		flagsBody: `{"status":"error","errorType":"internal","error":"boom"}`})
	var failed *ErrQuery
	if _, err := querier(s.srv.URL, 5*time.Second).Lookback(context.Background()); !errors.As(err, &failed) {
		t.Errorf("Lookback() on a 500 = %v, want *ErrQuery", err)
	}

	var request *ErrRequest
	if _, err := querier(deadpeer.URL(t), time.Second).Lookback(context.Background()); !errors.As(err, &request) {
		t.Errorf("Lookback() on a refused connection = %v, want *ErrRequest", err)
	}

	var none *ErrNoQueryEndpoint
	if _, err := querier("", time.Second).Lookback(context.Background()); !errors.As(err, &none) {
		t.Errorf("Lookback() with no endpoint = %v, want *ErrNoQueryEndpoint", err)
	}
}

// TestLookback_IsCachedAndRefreshed. The flag changes only when the database
// restarts, so it is read once per lookbackTTL, not per chart -- and read
// again after it. A failure to reach the database is retried much sooner: it
// is not a property of the database.
func TestLookback_IsCachedAndRefreshed(t *testing.T) {
	s := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	q := querier(s.srv.URL, 5*time.Second)
	clock := time.Unix(1788300000, 0)
	q.now = func() time.Time { return clock }

	for range 3 {
		if _, err := q.Lookback(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("%d flags requests for three reads inside the TTL, want 1", n)
	}

	s.set(statusAnswers{flags: 200, flagsBody: `{"status":"success","data":{"query.lookback-delta":"15m"}}`})
	clock = clock.Add(lookbackTTL)
	got, err := q.Lookback(context.Background())
	if err != nil || got != 15*time.Minute {
		t.Fatalf("after the TTL Lookback() = %v, %v; want the new 15m", got, err)
	}

	// A transient failure is held only for lookbackRetry.
	s.set(statusAnswers{flags: 503, flagsBody: `{"status":"error","error":"down"}`})
	clock = clock.Add(lookbackTTL)
	if _, err := q.Lookback(context.Background()); err == nil {
		t.Fatal("Lookback() on a 503 succeeded")
	}
	s.set(statusAnswers{flags: 200, flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	clock = clock.Add(lookbackRetry)
	if got, err := q.Lookback(context.Background()); err != nil || got != 5*time.Minute {
		t.Fatalf("after lookbackRetry Lookback() = %v, %v; want it read again", got, err)
	}
}

// TestLookback_PerDatabase. The configuration check asks about a database
// the querier may not be reading from yet; its answer is cached under its own
// URL, and never answers for another.
func TestLookback_PerDatabase(t *testing.T) {
	five := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	hour := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"1h"}}`})
	q := querier(five.srv.URL, 5*time.Second)

	if got, err := q.Lookback(context.Background()); err != nil || got != 5*time.Minute {
		t.Fatalf("Lookback() = %v, %v", got, err)
	}
	if got, err := q.LookbackAt(context.Background(), hour.srv.URL+"/"); err != nil || got != time.Hour {
		t.Fatalf("LookbackAt(other) = %v, %v; want that database's 1h", got, err)
	}
}

func TestParsePromDuration(t *testing.T) {
	good := map[string]time.Duration{
		"0":          0,
		"0s":         0,
		"5m":         5 * time.Minute,
		"1h30m":      90 * time.Minute,
		"2d":         48 * time.Hour,
		"1w":         7 * 24 * time.Hour,
		"1y":         365 * 24 * time.Hour,
		"1m30s500ms": 90*time.Second + 500*time.Millisecond,
	}
	for in, want := range good {
		if got, err := parsePromDuration(in); err != nil || got != want {
			t.Errorf("parsePromDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "5", "5 m", "m", "30m1h", "-5m", "1.5h", "5M", "999999999999y"} {
		var bad *ErrDuration
		if got, err := parsePromDuration(in); !errors.As(err, &bad) {
			t.Errorf("parsePromDuration(%q) = %v, %v; want *ErrDuration", in, got, err)
		}
	}
}

// TestLookback_ACancelledRequestIsNotCached. The dashboard cancels a history
// request whenever the range or the node changes. That cancellation is the
// caller's, not the database's, and the request replacing it must not
// inherit it as a cached failure.
func TestLookback_ACancelledRequestIsNotCached(t *testing.T) {
	s := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	q := querier(s.srv.URL, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Lookback(ctx); err == nil {
		t.Fatal("Lookback() with a cancelled context succeeded")
	}
	if got, err := q.Lookback(context.Background()); err != nil || got != 5*time.Minute {
		t.Fatalf("the next Lookback() = %v, %v; want the database's 5m, not the cancelled failure", got, err)
	}
}

// TestQuerier_PinnedHoldsItsDatabase. A read pinned before a reload keeps
// reading the database it began on, while the querier itself follows the
// reload; the two share the lookback cache.
func TestQuerier_PinnedHoldsItsDatabase(t *testing.T) {
	first := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	second := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"1h"}}`})
	var base atomic.Value
	base.Store(first.srv.URL)
	q := NewQuerier(func() QueryOptions { return QueryOptions{BaseURL: base.Load().(string), Timeout: 5 * time.Second} })

	pinned := q.Pinned()
	base.Store(second.srv.URL) // a reload, mid-read

	if got, err := pinned.Lookback(context.Background()); err != nil || got != 5*time.Minute {
		t.Fatalf("pinned Lookback() = %v, %v; want the first database's 5m", got, err)
	}
	if got, err := q.Lookback(context.Background()); err != nil || got != time.Hour {
		t.Fatalf("querier Lookback() = %v, %v; want the reloaded database's 1h", got, err)
	}
	if n := first.requests.Load(); n != 1 {
		t.Fatalf("first database asked %d times", n)
	}
	if _, err := q.LookbackAt(context.Background(), first.srv.URL); err != nil || first.requests.Load() != 1 {
		t.Fatalf("the pinned read's answer was not shared with the querier's cache")
	}
}

// TestWithLookbackCache. The cache's lifetimes can be set as the querier is
// built; a failure is then retried after the configured interval.
func TestWithLookbackCache(t *testing.T) {
	s := newStatusServer(t, statusAnswers{flags: 503, flagsBody: `{"status":"error","error":"down"}`})
	q := NewQuerier(func() QueryOptions { return QueryOptions{BaseURL: s.srv.URL, Timeout: 5 * time.Second} },
		WithLookbackCache(time.Hour, time.Minute))
	clock := time.Unix(1788300000, 0)
	q.now = func() time.Time { return clock }

	if _, err := q.Lookback(context.Background()); err == nil {
		t.Fatal("Lookback() on a 503 succeeded")
	}
	s.set(statusAnswers{flags: 200, flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	clock = clock.Add(59 * time.Second)
	if _, err := q.Lookback(context.Background()); err == nil {
		t.Fatal("the failure was not held for the configured minute")
	}
	clock = clock.Add(time.Second)
	if got, err := q.Lookback(context.Background()); err != nil || got != 5*time.Minute {
		t.Fatalf("after the configured retry Lookback() = %v, %v; want 5m", got, err)
	}
	s.set(statusAnswers{flags: 200, flagsBody: `{"status":"success","data":{"query.lookback-delta":"9m"}}`})
	clock = clock.Add(59 * time.Minute)
	if got, err := q.Lookback(context.Background()); err != nil || got != 5*time.Minute {
		t.Fatalf("inside the configured hour Lookback() = %v, %v; want the cached 5m", got, err)
	}
	clock = clock.Add(time.Minute)
	if got, err := q.Lookback(context.Background()); err != nil || got != 9*time.Minute {
		t.Fatalf("after the configured hour Lookback() = %v, %v; want the new 9m", got, err)
	}
}

// TestCachedLookbackAt. The last lookback the database gave, without a
// request -- held through a failed refresh and past expiry until it gives
// another, so a check relying on it never lapses to "unknown" and fails open
// while the database is briefly down.
func TestCachedLookbackAt(t *testing.T) {
	s := newStatusServer(t, statusAnswers{flags: 200,
		flagsBody: `{"status":"success","data":{"query.lookback-delta":"5m"}}`})
	q := querier(s.srv.URL, 5*time.Second)
	clock := time.Unix(1788300000, 0)
	q.now = func() time.Time { return clock }

	if _, ok := q.CachedLookbackAt(s.srv.URL); ok || s.requests.Load() != 0 {
		t.Fatal("an empty cache answered, or asked the database")
	}
	if _, err := q.Lookback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := q.CachedLookbackAt(s.srv.URL + "/"); !ok || got != 5*time.Minute {
		t.Fatalf("CachedLookbackAt = %v, %v; want the cached 5m", got, ok)
	}

	// Expired, and the refresh fails: the last answer stands.
	clock = clock.Add(lookbackTTL)
	s.set(statusAnswers{flags: 503, flagsBody: `{"status":"error","error":"down"}`})
	if _, err := q.Lookback(context.Background()); err == nil {
		t.Fatal("a 503 succeeded")
	}
	if got, ok := q.CachedLookbackAt(s.srv.URL); !ok || got != 5*time.Minute {
		t.Fatalf("after a failed refresh CachedLookbackAt = %v, %v; want the last known 5m", got, ok)
	}

	// A newer answer replaces it.
	clock = clock.Add(lookbackRetry)
	s.set(statusAnswers{flags: 200, flagsBody: `{"status":"success","data":{"query.lookback-delta":"2m"}}`})
	if _, err := q.Lookback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.CachedLookbackAt(s.srv.URL); got != 2*time.Minute {
		t.Fatalf("CachedLookbackAt = %v, want the newer 2m", got)
	}

	// A database that has only ever failed has no answer to give.
	down := newStatusServer(t, statusAnswers{flags: 503, flagsBody: `{"status":"error","error":"down"}`})
	if _, err := q.LookbackAt(context.Background(), down.srv.URL); err == nil {
		t.Fatal("a 503 succeeded")
	}
	if _, ok := q.CachedLookbackAt(down.srv.URL); ok {
		t.Fatal("a database that never answered has a cached lookback")
	}
}

// TestLookback_ATimeoutIsCached. A database that does not answer in time has
// said something about itself: that failure is held for lookbackRetry, so a
// blackholed database is not asked again, for the whole timeout, by every
// request that follows.
func TestLookback_ATimeoutIsCached(t *testing.T) {
	var requests atomic.Int64
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-r.Context().Done()
	}))
	defer hung.Close()
	q := querier(hung.URL, 5*time.Second)
	// The cache is read against a clock that does not move, so the failure
	// cannot expire mid-test however slowly the calls run.
	clock := time.Unix(1788300000, 0)
	q.now = func() time.Time { return clock }

	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := q.Lookback(ctx)
		cancel()
		if err == nil {
			t.Fatal("Lookback() of a hung database succeeded")
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests to a database that timed out, want 1 until lookbackRetry", n)
	}
}

// TestLookback_ConcurrentCallersShareOneFetch. Many charts opened at once
// against an empty cache ask the database once between them -- a slow one is
// not asked once per chart -- and all get its answer.
func TestLookback_ConcurrentCallersShareOneFetch(t *testing.T) {
	release := make(chan struct{})
	var requests atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if _, err := w.Write([]byte(`{"status":"success","data":{"query.lookback-delta":"5m"}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer slow.Close()
	q := querier(slow.URL, 5*time.Second)

	const callers = 8
	results := make(chan time.Duration, callers)
	var started sync.WaitGroup
	for range callers {
		started.Add(1)
		go func() {
			started.Done()
			got, err := q.Lookback(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- got
		}()
	}
	started.Wait()
	await.Until(t, "the shared fetch to reach the database", func(context.Context) bool { return requests.Load() > 0 })
	close(release)
	for range callers {
		if got := await.Recv(t, "a caller's answer", results); got != 5*time.Minute {
			t.Errorf("a caller got %v, want 5m", got)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests for %d concurrent callers, want 1", n, callers)
	}
}
