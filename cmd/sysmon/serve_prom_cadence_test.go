//go:build prometheus && !desktop

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
)

// flagsServer answers /api/v1/status/flags as a database with the given
// query lookback does -- or with status when it is not 200 -- and counts the
// requests.
func flagsServer(t *testing.T, status int, lookback string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/v1/status/flags" || status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			if _, err := w.Write([]byte(`{"status":"error","error":"down"}`)); err != nil {
				t.Errorf("write response: %v", err)
			}
			return
		}
		if _, err := w.Write([]byte(`{"status":"success","data":{"query.lookback-delta":"` + lookback + `"}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// writing is a configuration that pushes history to write and reads it back
// from query, at the given sample interval.
func writing(write, query string, sampleMs int) *config.Config {
	c := config.Defaults()
	c.Prometheus.RemoteWriteURL = write
	c.Prometheus.QueryURL = query
	c.Prometheus.SampleIntervalMs = sampleMs
	c.Prometheus.TimeoutMs = 2000
	return &c
}

func capturedLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func newCadenceQuerier() *prom.Querier {
	return prom.NewQuerier(func() prom.QueryOptions { return prom.QueryOptions{Timeout: 2 * time.Second} })
}

// TestHistoryCadence_UsesTheDatabasesLookback. The limit is the lookback the
// database reports, not a constant: the same four-minute tick is a violation
// against a one-minute lookback and passes against an hour.
func TestHistoryCadence_UsesTheDatabasesLookback(t *testing.T) {
	short, _ := flagsServer(t, http.StatusOK, "1m")
	long, _ := flagsServer(t, http.StatusOK, "1h")
	q := newCadenceQuerier()

	r := historyCadence(context.Background(), q, writing(short.URL+"/api/v1/write", short.URL, 240000))
	if r.state != cadenceViolated || r.violation.LookbackMs != 60000 {
		t.Fatalf("against a 1m lookback: %+v, want violated at 60000ms", r)
	}
	if r := historyCadence(context.Background(), q, writing(long.URL+"/api/v1/write", long.URL, 240000)); r.state != cadenceOK {
		t.Fatalf("against a 1h lookback: %+v, want ok", r)
	}
}

// TestHistoryCadence_AWriteOnlyNodeAsksItsReceiver. A node that pushes to a
// Prometheus and reads nothing back still writes history someone will chart:
// its receiver's server is asked.
func TestHistoryCadence_AWriteOnlyNodeAsksItsReceiver(t *testing.T) {
	short, _ := flagsServer(t, http.StatusOK, "1m")
	r := historyCadence(context.Background(), newCadenceQuerier(), writing(short.URL+"/api/v1/write", "", 240000))
	if r.state != cadenceViolated {
		t.Fatalf("write-only node: %+v, want violated against its receiver's lookback", r)
	}
}

// TestHistoryCadence_UncheckableIsPendingNotGuessed. When the lookback cannot
// be learned, no lookback is made up: the check is pending, and says why.
func TestHistoryCadence_UncheckableIsPendingNotGuessed(t *testing.T) {
	unpublished, _ := flagsServer(t, http.StatusNotFound, "")
	down, _ := flagsServer(t, http.StatusServiceUnavailable, "")
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"a database that publishes no lookback", writing(unpublished.URL+"/api/v1/write", unpublished.URL, 600000),
			"does not publish its query lookback"},
		{"a database that is down", writing(down.URL+"/api/v1/write", down.URL, 600000),
			"could not be reached"},
		{"a receiver that is not Prometheus, and no query_url", writing("http://mimir:8080/api/v1/push", "", 600000),
			"not a Prometheus receiver"},
	}
	for _, c := range cases {
		r := historyCadence(context.Background(), newCadenceQuerier(), c.cfg)
		if r.state != cadencePending || !strings.Contains(r.reason, c.want) {
			t.Errorf("%s: %+v, want pending saying %q", c.name, r, c.want)
		}
	}
}

// TestHistoryCadence_NotWritingIsNotChecked. A node that writes no history
// asks no database anything.
func TestHistoryCadence_NotWritingIsNotChecked(t *testing.T) {
	c := config.Defaults()
	c.Server.IntervalMs = 600000
	if r := historyCadence(context.Background(), newCadenceQuerier(), &c); r.state != cadenceOK {
		t.Fatalf("not writing: %+v, want ok", r)
	}
}

// TestHistoryCadence_UsesTheConfigurationsTimeout. The configuration being
// checked may not be live yet, so the querier can still hold an older, shorter
// timeout. The configuration's own governs.
//
// Deterministic by construction rather than by timing: the querier's stale
// timeout is one nanosecond, so a request made under it expires before it can
// reach the database, while the configuration allows an hour. The database
// says when a request arrives and answers only when the test lets it -- so a
// request arriving at all proves the configuration's deadline was the one in
// force, and no assertion depends on how long anything took.
func TestHistoryCadence_UsesTheConfigurationsTimeout(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if _, err := w.Write([]byte(`{"status":"success","data":{"query.lookback-delta":"1m"}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(gated.Close)

	stale := prom.NewQuerier(func() prom.QueryOptions { return prom.QueryOptions{Timeout: time.Nanosecond} })
	cfg := writing(gated.URL+"/api/v1/write", gated.URL, 240000)
	cfg.Prometheus.TimeoutMs = int(time.Hour.Milliseconds())

	result := make(chan cadenceResult, 1)
	go func() { result <- historyCadence(context.Background(), stale, cfg) }()
	await.Recv(t, "the lookback request to reach the database under the configuration's deadline", arrived)
	close(release)
	if r := await.Recv(t, "the check to finish", result); r.state != cadenceViolated {
		t.Fatalf("%+v, want violated, read under the configuration's hour", r)
	}
}

// TestHistoryCadence_StopsWithTheServer. A check waiting on a database that
// is not answering ends when the server stops, rather than waiting out the
// timeout.
func TestHistoryCadence_StopsWithTheServer(t *testing.T) {
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hung.Close)

	ctx, stop := context.WithCancel(context.Background())
	stop() // the server is already stopping
	cfg := writing(hung.URL+"/api/v1/write", hung.URL, 15000)
	cfg.Prometheus.TimeoutMs = int(time.Hour.Milliseconds())

	done := make(chan cadenceResult, 1)
	go func() { done <- historyCadence(ctx, newCadenceQuerier(), cfg) }()
	if r := await.Recv(t, "the check to end once the server stopped", done); r.state != cadencePending {
		t.Fatalf("%+v, want pending: the check was not made", r)
	}
}

// TestHistoryCadenceValidator_NeverAsksTheDatabase. Startup and reloads
// never wait on the database: the validator checks against the lookback
// already cached and makes no request of its own.
func TestHistoryCadenceValidator_NeverAsksTheDatabase(t *testing.T) {
	db, requests := flagsServer(t, http.StatusOK, "1m")
	q := newCadenceQuerier()
	log, buf := capturedLogger()
	validate := historyCadenceValidator(q, log)

	// Nothing cached: accepted, pending, and said.
	if err := validate(writing(db.URL+"/api/v1/write", db.URL, 240000)); err != nil {
		t.Fatalf("with nothing cached: %v, want accepted", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("the validator made %d requests", n)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "pending") {
		t.Errorf("log = %q, want a warning that the check is pending", buf.String())
	}

	// Once the watch has read the lookback, a violation is visible without a
	// request, and refused like any bad configuration.
	if _, err := q.LookbackAt(context.Background(), db.URL); err != nil {
		t.Fatal(err)
	}
	var refused *config.ErrHistoryCadence
	if err := validate(writing(db.URL+"/api/v1/write", db.URL, 240000)); !errors.As(err, &refused) {
		t.Fatalf("with the lookback cached: %v, want *ErrHistoryCadence", err)
	}
	passing := writing(db.URL+"/api/v1/write", db.URL, 1000)
	passing.Server.IntervalMs = 1000
	if err := validate(passing); err == nil {
		t.Fatal("a 61s cadence passed a 1m lookback; the dashboard's 60s collection counts")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d requests, want only the one the test made", n)
	}
}

func TestLookbackSource(t *testing.T) {
	cases := []struct {
		name     string
		p        config.Prometheus
		want     string
		wantOkay bool
	}{
		{"query_url is where history is read", config.Prometheus{
			RemoteWriteURL: "http://a:9090/api/v1/write", QueryURL: "http://b:9090"}, "http://b:9090", true},
		{"a Prometheus receiver's own server", config.Prometheus{
			RemoteWriteURL: "http://a:9090/api/v1/write"}, "http://a:9090", true},
		{"behind a path prefix", config.Prometheus{
			RemoteWriteURL: "https://a/prom/api/v1/write/"}, "https://a/prom", true},
		{"another product's receiver", config.Prometheus{
			RemoteWriteURL: "http://mimir:8080/api/v1/push"}, "", false},
		{"a receiver path with no server", config.Prometheus{RemoteWriteURL: "/api/v1/write"}, "", false},
		// The POST is routed by its path; a query or fragment rides along
		// and must not stop the receiver being recognised, nor be carried
		// onto the status endpoints.
		{"a receiver with a query and a fragment", config.Prometheus{
			RemoteWriteURL: "http://prom:9090/api/v1/write?tenant=a#x"}, "http://prom:9090", true},
	}
	for _, c := range cases {
		got, ok := lookbackSource(c.p)
		if got != c.want || ok != c.wantOkay {
			t.Errorf("%s: lookbackSource = %q, %v; want %q, %v", c.name, got, ok, c.want, c.wantOkay)
		}
	}
}
