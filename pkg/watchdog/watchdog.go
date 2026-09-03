// Package watchdog answers the service manager's liveness question with
// evidence rather than with a timer.
//
// systemd's Type=exec reports a unit active while the binary exists, which says
// nothing about whether a request can be answered. vm-host-2 served nothing
// for two days in exactly that state: NRestarts=0, unit active, 2929
// connections queued on a listener nothing was accepting from (issue #65).
//
// The trap this package exists to avoid is subtle and would pass every test
// written the obvious way: a goroutine that pings on a ticker allocates
// nothing and touches nothing, so it keeps pinging cheerfully through a stall
// that has parked every other goroutine in the process. A watchdog like that
// converts "the service is dead" into "the service is dead and systemd has
// confirmed it is fine". Every ping here is therefore conditional on evidence
// gathered in the same interval.
package watchdog

import (
	"context"
	"log/slog"
	"time"
)

// Pinger is the half of the systemd protocol this package needs.
type Pinger interface {
	// Watchdog reports that the service is still doing its job.
	Watchdog() error
}

// Probe exercises the serving path and returns an error when it could not be
// completed. It is expected to make a real request against the address the
// server actually bound: accept is what failed on vm-host-2, and a check that
// does not cross the listener cannot see that failure.
type Probe func(context.Context) error

// Collecting reports whether the monitor has produced a snapshot recently
// enough. A server that answers requests while its collection loop is wedged
// is serving stale readings and describing them as current, which is the
// failure the probe alone cannot see.
type Collecting func() error

// Watchdog pings the service manager while, and only while, both signals hold.
type Watchdog struct {
	pinger   Pinger
	interval time.Duration
	probe    Probe
	fresh    Collecting
	logger   *slog.Logger
}

// New builds a Watchdog. A nil pinger, a zero interval, or a nil probe all
// yield a Watchdog whose Run returns immediately -- the states outside systemd,
// with no watchdog configured, are normal rather than errors.
func New(p Pinger, interval time.Duration, probe Probe, fresh Collecting, logger *slog.Logger) *Watchdog {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watchdog{pinger: p, interval: interval, probe: probe, fresh: fresh, logger: logger}
}

// Run pings until ctx is cancelled. It returns immediately when no watchdog was
// requested.
//
// A failed check is logged and skipped rather than retried: the manager's
// timeout is the retry, and a watchdog that worked harder to produce a ping
// would be arguing with the mechanism that exists to restart it.
func (w *Watchdog) Run(ctx context.Context) {
	if w.pinger == nil || w.interval <= 0 || w.probe == nil {
		w.logger.Debug("watchdog: not configured, no pings will be sent")
		return
	}

	w.logger.Info("watchdog: pinging on evidence", "interval", w.interval)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick gathers evidence and pings only if all of it holds.
func (w *Watchdog) tick(ctx context.Context) {
	// Bounded by the ping interval: a probe still running when the next ping is
	// due has already failed to demonstrate anything in time, and letting it
	// run on would pile up requests against a server that is not answering.
	probeCtx, cancel := context.WithTimeout(ctx, w.interval)
	defer cancel()

	if err := w.probe(probeCtx); err != nil {
		w.logger.Warn("watchdog: not pinging, the server did not answer its own request", "err", err)
		return
	}
	if w.fresh != nil {
		if err := w.fresh(); err != nil {
			w.logger.Warn("watchdog: not pinging, collection is not current", "err", err)
			return
		}
	}

	if err := w.pinger.Watchdog(); err != nil {
		w.logger.Warn("watchdog: ping failed", "err", err)
	}
}
