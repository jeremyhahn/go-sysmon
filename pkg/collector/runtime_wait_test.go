package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// gatedDaemon is a fake runtime whose /system/df handler holds each query open
// until the test chooses its outcome. That lets a test observe waiters while a
// query is provably still in flight, then conclude it on demand.
type gatedDaemon struct {
	entered  chan struct{}
	statuses chan int
}

// serveGatedDaemon starts a fake runtime whose /info always succeeds and
// whose /system/df answers with whatever status the test sends on statuses.
func serveGatedDaemon(t *testing.T) *gatedDaemon {
	t.Helper()
	d := &gatedDaemon{entered: make(chan struct{}), statuses: make(chan int)}
	stop := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(dockerInfo{ServerVersion: "29.1.3"}); err != nil {
			t.Errorf("encode /info: %v", err)
		}
	})
	mux.HandleFunc("/system/df", func(w http.ResponseWriter, r *http.Request) {
		select {
		case d.entered <- struct{}{}:
		case <-stop:
			return
		}
		var status int
		select {
		case status = <-d.statuses:
		case <-stop:
			return
		case <-r.Context().Done():
			return
		}
		if status != http.StatusOK {
			http.Error(w, "server error", status)
			return
		}
		if err := json.NewEncoder(w).Encode(diskUsageFixture()); err != nil {
			t.Errorf("encode /system/df: %v", err)
		}
	})
	serveFakeDaemon(t, mux)
	// Registered after the server's own cleanup, so it runs first and frees
	// any handler still parked before the server shuts down.
	t.Cleanup(func() { close(stop) })
	return d
}

// startQuery runs a collection, which starts the background /system/df query,
// and returns once the daemon has received it.
func (d *gatedDaemon) startQuery(t *testing.T, c *RuntimeCollector) {
	t.Helper()
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	await.Recv(t, "the /system/df query", d.entered)
}

// waitingFrame is WaitForDiskUsage's line in a goroutine dump.
const waitingFrame = "collector.(*RuntimeCollector).WaitForDiskUsage("

// startWaiters launches n concurrent WaitForDiskUsage calls and returns the
// channel each result arrives on, once every one of them is either blocked
// inside the wait or has returned.
func startWaiters(t *testing.T, ctx context.Context, c *RuntimeCollector, n int) chan error {
	t.Helper()
	results := make(chan error, n)
	for range n {
		go func() { results <- c.WaitForDiskUsage(ctx) }()
	}
	await.Until(t, "every waiter to block in the wait or return", func(context.Context) bool {
		return await.Parked(waitingFrame)+len(results) >= n
	})
	return results
}

// assertStillWaiting fails when any waiter has returned. startWaiters has seen
// each one either return or block inside the wait, so this is conclusive: a
// wait that returned early is in results now. It cannot fail spuriously:
// while the query is held open, a correct wait has nothing to return.
func assertStillWaiting(t *testing.T, results chan error) {
	t.Helper()
	select {
	case err := <-results:
		t.Fatalf("waiter returned %v while the query was still in flight", err)
	default:
	}
}

// collectResults gathers n waiter results, failing if any is missing.
func collectResults(t *testing.T, results chan error, n int) []error {
	t.Helper()
	errs := make([]error, 0, n)
	for range n {
		errs = append(errs, await.Recv(t, "every waiter to be released", results))
	}
	return errs
}

// TestWaitForDiskUsage_ReleasesAllWaitersWhenResultLands verifies every
// concurrent waiter blocks while the query runs and is released, with the
// result already visible, the moment it is stored.
func TestWaitForDiskUsage_ReleasesAllWaitersWhenResultLands(t *testing.T) {
	d := serveGatedDaemon(t)
	c := NewRuntimeCollector(quietLogger())
	d.startQuery(t, c)

	const waiters = 8
	results := startWaiters(t, context.Background(), c, waiters)
	assertStillWaiting(t, results)

	d.statuses <- http.StatusOK

	for i, err := range collectResults(t, results, waiters) {
		if err != nil {
			t.Errorf("waiter %d: WaitForDiskUsage() = %v, want nil", i, err)
		}
	}
	if c.diskUsage.Load() == nil {
		t.Error("waiters released before the disk usage result was stored")
	}
}

// TestWaitForDiskUsage_FailedQueryReleasesWaitersWithError verifies a failed
// query wakes every waiter with the query's own error, instead of leaving
// them to run out the caller's timeout.
func TestWaitForDiskUsage_FailedQueryReleasesWaitersWithError(t *testing.T) {
	d := serveGatedDaemon(t)
	c := NewRuntimeCollector(quietLogger())
	d.startQuery(t, c)

	const waiters = 4
	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()
	results := startWaiters(t, ctx, c, waiters)
	assertStillWaiting(t, results)

	d.statuses <- http.StatusInternalServerError

	for i, err := range collectResults(t, results, waiters) {
		var collErr *types.CollectorError
		if !errors.As(err, &collErr) || collErr.Collector != "runtime" {
			t.Errorf("waiter %d: error = %v, want a runtime CollectorError", i, err)
		}
		var statusErr *runtimeStatusError
		if !errors.As(err, &statusErr) || statusErr.Status != http.StatusInternalServerError {
			t.Errorf("waiter %d: error = %v, want the /system/df status error", i, err)
		}
	}
}

// TestWaitForDiskUsage_CancellationReturnsPromptly verifies a waiter leaves as
// soon as its context ends, with a typed error, while the query carries on.
func TestWaitForDiskUsage_CancellationReturnsPromptly(t *testing.T) {
	d := serveGatedDaemon(t)
	c := NewRuntimeCollector(quietLogger())
	d.startQuery(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	results := startWaiters(t, ctx, c, 1)
	assertStillWaiting(t, results)

	cancel()

	err := collectResults(t, results, 1)[0]
	var collErr *types.CollectorError
	if !errors.As(err, &collErr) || collErr.Collector != "runtime" {
		t.Errorf("error = %v, want a runtime CollectorError", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// TestWaitForDiskUsage_NewQueryRearms verifies a query started after an
// earlier one concluded gets its own completion signal: a waiter blocks on the
// new query rather than returning the previous query's error.
func TestWaitForDiskUsage_NewQueryRearms(t *testing.T) {
	d := serveGatedDaemon(t)
	c := NewRuntimeCollector(quietLogger())
	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()

	d.startQuery(t, c)
	d.statuses <- http.StatusInternalServerError
	if err := c.WaitForDiskUsage(ctx); err == nil {
		t.Fatal("first query failed but WaitForDiskUsage() returned nil")
	}

	// Let the refresh interval lapse so the next collection queries again.
	c.dfLastStart.Store(0)
	d.startQuery(t, c)

	results := startWaiters(t, ctx, c, 1)
	assertStillWaiting(t, results)

	d.statuses <- http.StatusOK

	if err := collectResults(t, results, 1)[0]; err != nil {
		t.Errorf("second query succeeded but WaitForDiskUsage() = %v", err)
	}
	if c.diskUsage.Load() == nil {
		t.Error("second query's result was not stored")
	}
}

// TestWaitForDiskUsage_ReturnsAtOnceWithStoredResult verifies a result from an
// earlier query satisfies the wait even while a refresh is in flight.
func TestWaitForDiskUsage_ReturnsAtOnceWithStoredResult(t *testing.T) {
	t.Parallel()
	c := NewRuntimeCollector(quietLogger())
	c.diskUsage.Store(&types.RuntimeInfo{VolumesBytes: 600})
	c.dfQuery.Store(&diskUsageQuery{done: make(chan struct{})})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.WaitForDiskUsage(ctx); err != nil {
		t.Errorf("WaitForDiskUsage() = %v, want nil: a result is already stored", err)
	}
}
