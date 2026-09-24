//go:build prometheus

package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The history-cadence check.
//
// A node that writes history more slowly than the time-series database's query
// lookback writes series that are stale between their own samples: nothing
// charts them (config.CheckHistoryCadence). The lookback is the database's, so
// the check needs the database -- and the owner's ruling (2026-09-23) is that
// the service never waits on it: START, THEN RE-CHECK WHEN REACHABLE.
//
//   - At startup and on every reload the check runs as a configuration
//     validator, against the lookback already cached -- it asks the database
//     nothing, so a database that blackholes cannot hold the service up. A
//     violation it can see is refused like any other bad configuration.
//     Otherwise the configuration is accepted and the check is PENDING --
//     said in the log and in the unit's status -- not skipped.
//   - A background watch re-checks: every retry interval while pending, and
//     once the lookback is known, every time the cached lookback refreshes
//     (it may have changed) and on every reload.
//   - A violation the watch finds surfaces loudly: the unit's status says the
//     service is degraded and why, and an alert fires through the ordinary
//     pipeline (alert.RuleHistoryCadence), naming the settings and the bound.
//     Both clear when a reload -- or a raised lookback -- fixes it.

// cadenceRetry is how often a pending check is retried: the lookback cache's
// own retry after a failure, so each attempt asks the database afresh.
const cadenceRetry = 30 * time.Second

// cadenceRecheck is how often a settled check is repeated: the lookback
// cache's lifetime, so each repeat reads a freshly fetched lookback and a
// database restarted with a different one is noticed.
const cadenceRecheck = 10 * time.Minute

// cadenceState is where the check stands.
type cadenceState int

const (
	// cadenceOK: the check was made and passed, or this node writes no
	// history.
	cadenceOK cadenceState = iota
	// cadencePending: the lookback could not be learned yet.
	cadencePending
	// cadenceViolated: the check was made and failed.
	cadenceViolated
)

// cadenceResult is one outcome of the check.
type cadenceResult struct {
	state cadenceState
	// reason says why the check is pending, or what it found violated.
	reason string
	// violation is the refusal when state is cadenceViolated.
	violation *config.ErrHistoryCadence
}

// historyCadence checks c against the lookback of the database its history is
// read back from, asking the database when it must.
func historyCadence(ctx context.Context, history *prom.Querier, c *config.Config) cadenceResult {
	return cadenceCheck(c, func(base string) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(ctx, c.Prometheus.Timeout())
		defer cancel()
		return history.LookbackAt(ctx, base)
	})
}

// cachedHistoryCadence checks c against a lookback already known, asking the
// database nothing: pending when none is.
func cachedHistoryCadence(history *prom.Querier, c *config.Config) cadenceResult {
	return cadenceCheck(c, func(base string) (time.Duration, error) {
		if lookback, ok := history.CachedLookbackAt(base); ok {
			return lookback, nil
		}
		return 0, errLookbackNotYetRead
	})
}

// errLookbackNotYetRead is the pending reason for a check made before the
// watch has read the database's lookback.
var errLookbackNotYetRead = &prom.ErrLookbackUnpublished{Reason: "its lookback has not been read yet"}

// cadenceCheck is the check against whatever lookback source it is given.
func cadenceCheck(c *config.Config, lookbackAt func(base string) (time.Duration, error)) cadenceResult {
	if !c.Prometheus.WriteEnabled() {
		return cadenceResult{state: cadenceOK}
	}
	base, ok := lookbackSource(c.Prometheus)
	if !ok {
		return cadenceResult{state: cadencePending, reason: "no query_url is set, and remote_write_url is not " +
			"a Prometheus receiver (/api/v1/write) whose server could be asked for its lookback"}
	}
	lookback, err := lookbackAt(base)
	if err != nil {
		reason := "the time-series database could not be reached"
		var unpublished *prom.ErrLookbackUnpublished
		switch {
		case errors.Is(err, errLookbackNotYetRead):
			reason = "the time-series database's lookback has not been read yet"
		case errors.As(err, &unpublished):
			reason = unpublished.Error()
		}
		return cadenceResult{state: cadencePending, reason: reason}
	}
	var violation *config.ErrHistoryCadence
	if err := config.CheckHistoryCadence(c, lookback); errors.As(err, &violation) {
		return cadenceResult{state: cadenceViolated, reason: violation.Error(), violation: violation}
	}
	return cadenceResult{state: cadenceOK}
}

// historyCadenceValidator is the load-time half. It asks the database nothing
// -- startup and reloads never wait on it (owner ruling, 2026-09-23) -- and
// checks against the lookback already cached: a violation it can see is
// refused like any other bad configuration, and one it cannot check yet is
// accepted with the check pending, for the watch to make.
func historyCadenceValidator(history *prom.Querier, log *slog.Logger) config.Validator {
	return func(c *config.Config) error {
		r := cachedHistoryCadence(history, c)
		switch r.state {
		case cadenceViolated:
			return r.violation
		case cadencePending:
			log.Warn("history: cadence check pending; it runs in the background until the database's " +
				"lookback can be read: " + r.reason)
		}
		return nil
	}
}

// cadenceWatch is the running half: it re-checks until the lookback is known,
// keeps re-checking as it may change, and raises or clears the condition.
type cadenceWatch struct {
	mgr     *config.Manager
	history *prom.Querier
	alerts  *alert.Evaluator
	status  *serviceStatus
	log     *slog.Logger
	retry   time.Duration
	recheck time.Duration
	// kick asks for a check now: a reload happened. One slot, because two
	// reloads before a check are one check's worth of work.
	kick chan struct{}
	// last is the state last applied, so a transition is logged once.
	last cadenceState
	// violatedIn is the configuration a standing violation was found in. A
	// check that cannot be made later does not clear it while that is still
	// the configuration: nothing that decides the cadence has changed, and a
	// database that stopped answering has not fixed anything.
	violatedIn *config.Config
	// violation is what that check found.
	violation string
	// observe, when set, is told each result as it is applied. Tests wait on
	// it instead of on the clock.
	observe func(cadenceResult)
	// stopped is closed when Run returns.
	stopped chan struct{}
}

// Kick asks for a check as soon as the watch is free. It never blocks.
func (w *cadenceWatch) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run checks at once, then on its schedule and on every kick, until ctx is
// done.
func (w *cadenceWatch) Run(ctx context.Context) {
	defer close(w.stopped)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		case <-timer.C:
		}
		cfg := w.mgr.Current()
		r := historyCadence(ctx, w.history, cfg)
		if ctx.Err() != nil {
			return
		}
		w.apply(cfg, r)
		next := w.recheck
		if r.state == cadencePending {
			next = w.retry
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// apply makes a result visible: the alert, the unit's status, and a log line
// on each change of state.
func (w *cadenceWatch) apply(cfg *config.Config, r cadenceResult) {
	if r.state == cadencePending && w.last == cadenceViolated && cfg == w.violatedIn {
		// Still degraded: the violation stands until a check says otherwise.
		w.status.Set("degraded: " + w.violation + "; re-check pending: " + r.reason)
		if w.observe != nil {
			w.observe(r)
		}
		return
	}
	switch r.state {
	case cadenceViolated:
		w.violatedIn, w.violation = cfg, r.reason
		v := r.violation
		w.alerts.Raise(alert.RuleHistoryCadence, types.SeverityWarning, alert.CategoryHost, v.Key,
			"History is written up to "+durationMs(v.IntervalMs)+" apart ("+v.Key+"), past the "+
				"time-series database's "+durationMs(v.LookbackMs)+" query lookback: it is stale between "+
				"samples, and no chart can draw it. Shorten those intervals.",
			float64(v.IntervalMs)/1000, float64(v.LookbackMs)/1000)
		w.status.Set("degraded: " + r.reason)
		if w.last != cadenceViolated {
			w.log.Error("history: " + r.reason)
		}
	case cadencePending:
		w.alerts.Clear(alert.RuleHistoryCadence)
		w.status.Set("history cadence check pending: " + r.reason)
		if w.last != cadencePending {
			w.log.Warn("history: cadence check pending: " + r.reason)
		}
	default:
		w.alerts.Clear(alert.RuleHistoryCadence)
		w.status.Set("")
		if w.last != cadenceOK {
			w.log.Info("history: cadence check passed")
		}
	}
	if r.state != cadenceViolated {
		w.violatedIn = nil
	}
	w.last = r.state
	if w.observe != nil {
		w.observe(r)
	}
}

// durationMs renders milliseconds for a reader.
func durationMs(ms int) string {
	return (time.Duration(ms) * time.Millisecond).String()
}

// startHistoryCadenceWatch registers the load-time check, applies it to the
// configuration already loaded, and starts the watch, which runs until ctx --
// the serve command's -- is done.
func startHistoryCadenceWatch(ctx context.Context, mgr *config.Manager, history server.HistoryQuerier,
	alerts *alert.Evaluator, status *serviceStatus) error {
	return newCadenceWatch(mgr, history, alerts, status, slog.Default()).Start(ctx)
}

// Start registers the load-time check and the reload trigger, checks the
// configuration already loaded, and runs the watch until ctx is done.
func (w *cadenceWatch) Start(ctx context.Context) error {
	w.mgr.OnValidate(historyCadenceValidator(w.history, w.log))
	if err := w.mgr.Validate(w.mgr.Current()); err != nil {
		return err
	}
	w.mgr.OnReload(func(*config.Config) error {
		w.Kick()
		return nil
	})
	go w.Run(ctx)
	return nil
}

// newCadenceWatch builds a watch on the production schedule.
func newCadenceWatch(mgr *config.Manager, history *prom.Querier, alerts *alert.Evaluator,
	status *serviceStatus, log *slog.Logger) *cadenceWatch {
	return &cadenceWatch{
		mgr: mgr, history: history, alerts: alerts, status: status, log: log,
		retry: cadenceRetry, recheck: cadenceRecheck,
		kick:    make(chan struct{}, 1),
		stopped: make(chan struct{}),
	}
}

// lookbackSource is the database whose lookback a configuration's history is
// drawn with: the one query_url reads from, or -- on a node that only writes --
// the Prometheus server a remote_write_url of /api/v1/write belongs to, which
// serves its status endpoints from the same address. Any other write path is a
// different product's receiver, whose query API lives elsewhere.
func lookbackSource(p config.Prometheus) (string, bool) {
	if p.QueryURL != "" {
		return p.QueryURL, true
	}
	// Parsed, not matched as a string: a receiver URL may carry a query or a
	// fragment (?tenant=a), which the POST ignores for routing and the
	// status endpoints must not inherit.
	u, err := url.Parse(p.RemoteWriteURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	prefix, ok := strings.CutSuffix(strings.TrimRight(u.Path, "/"), "/api/v1/write")
	if !ok {
		return "", false
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: prefix}).String(), true
}

// cadenceStateNames names states for tests and logs.
var cadenceStateNames = map[cadenceState]string{
	cadenceOK: "ok", cadencePending: "pending", cadenceViolated: "violated",
}

func (s cadenceState) String() string {
	if name, ok := cadenceStateNames[s]; ok {
		return name
	}
	return "cadenceState(" + strconv.Itoa(int(s)) + ")"
}

// peerHistoryGate decides, per relayed reading, whether a peer's history is
// written -- the per-peer half of the cadence rule, which the configuration
// cannot check because the cadence is the PEER'S.
//
// A relayed reading is stored at the peer's collection time, once (#173), so
// two of one peer's samples can be a relay tick plus the peer's collection
// interval apart:
//
//   - a peer read by its summary has its history fetched by its own worker at
//     the exporter's sample interval (floored at fleet.MinPollInterval), and a
//     fetch may run to twice the fleet timeout (the history request and its
//     fallback to the whole snapshot) -- after waiting for one of the
//     fleet.MaxHistoryFetches slots, each held by a fetch as long;
//   - a peer too old to serve a summary is relayed by the snapshot poll, at
//     the poll interval -- the larger of the configured one and the 60s the
//     dashboard can override it to -- plus four timeouts: the summary it is
//     asked for first, then the snapshot, in each of two refreshes that can
//     separate its readings.
//
// If that exceeds the database's query lookback the peer's history could not
// be charted, and it is refused rather than written and silently useless.
// When either side is unknown -- the peer reports no collection interval (an
// older build; any peer read by snapshot), or the database's lookback has not
// been read yet -- the reading is written and the card says it is unverified:
// nothing is assumed in place of the missing figure.
//
// It asks the database nothing: it is called from the poll and the history
// workers, which must not wait on it. The lookback is the one the cadence
// watch keeps cached.
func peerHistoryGate(mgr *config.Manager, history *prom.Querier) fleet.HistoryGate {
	return func(peer string, summary *types.NodeSummary, source types.SummarySource) (bool, types.PeerHistory) {
		c := mgr.Current()
		base, ok := lookbackSource(c.Prometheus)
		if !ok {
			return true, types.PeerHistory{State: types.PeerHistoryUnverified,
				Reason: "the time-series database's lookback cannot be read from this configuration"}
		}
		lookback, ok := history.CachedLookbackAt(base)
		if !ok {
			return true, types.PeerHistory{State: types.PeerHistoryUnverified,
				Reason: "the time-series database's lookback has not been read yet"}
		}
		if summary == nil || summary.CollectionIntervalMs <= 0 {
			return true, types.PeerHistory{State: types.PeerHistoryUnverified,
				Reason: "the node does not report its collection interval (a build older than the fleet summary)"}
		}
		// Checked before any conversion: an interval too large to convert
		// and add a relay tick to would wrap round to a small duration and
		// pass. No real node collects that rarely, and none can be charted.
		tick := relayTick(c, source)
		if summary.CollectionIntervalMs > (math.MaxInt64-int64(tick))/int64(time.Millisecond) {
			return false, types.PeerHistory{State: types.PeerHistoryRefused,
				Reason: "the node reports a collection interval of " +
					strconv.FormatInt(summary.CollectionIntervalMs, 10) + "ms, too long for any history to be " +
					"charted against the time-series database's " + lookback.String() + " query lookback"}
		}
		collection := time.Duration(summary.CollectionIntervalMs) * time.Millisecond
		gap := tick + collection
		if gap > lookback {
			return false, types.PeerHistory{State: types.PeerHistoryRefused,
				Reason: "the node collects every " + collection.String() + ", so its relayed samples can be " +
					gap.String() + " apart -- past the time-series database's " + lookback.String() +
					" query lookback; its history would be stale between samples and is not recorded"}
		}
		return true, types.PeerHistory{State: types.PeerHistoryRecorded}
	}
}

// relayTick is the longest interval between two relays of one peer's
// readings, by how the peer is read.
//
// Every fetch is counted as two requests, each with its own timeout, because
// it can be: a history fetch the peer does not serve falls back to its whole
// snapshot, and the poll of a peer too old for a summary asks for the summary
// before the snapshot.
//
// And a relayed reading reaches the database through the exporter, whose loop
// stops for each write, up to prometheus.timeout_ms: that wait is added once.
func relayTick(c *config.Config, source types.SummarySource) time.Duration {
	timeout := 2 * c.Fleet.Timeout()
	write := c.Prometheus.Timeout()
	if source == types.SummarySourceSnapshot {
		// A refresh waits for the slowest peer, so this peer's reading can
		// open one refresh held for two requests by another peer, and its
		// next close the refresh after the sleep, itself slow: two refreshes'
		// worth of requests between them.
		dashboardMax := time.Duration(config.SlowestDashboardIntervalMs) * time.Millisecond
		return max(c.Fleet.PollInterval(), dashboardMax) + 2*timeout + write
	}
	// A worker queues behind at most every other peer's fetch, those run
	// MaxHistoryFetches at a time: ceil(peers / slots) timeouts, its own
	// included.
	rounds := (len(c.Fleet.Peers) + fleet.MaxHistoryFetches - 1) / fleet.MaxHistoryFetches
	return max(c.Prometheus.SampleInterval(), fleet.MinPollInterval) + time.Duration(max(rounds, 1))*timeout + write
}
