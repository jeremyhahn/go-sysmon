package main

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// enrichedSnapshotter decorates each snapshot with the data that belongs to the
// service rather than the hardware: the build serving it, and the alerts that
// its thresholds produce.
//
// Alerts are attached here, before the monitor broadcasts, so that the SSE
// stream, GET /api/snapshot and GET /metrics all describe the same evaluation.
// Thresholds are read from the config manager on every snapshot, which is what
// makes a reload take effect without restarting.
type enrichedSnapshotter struct {
	inner   monitor.Snapshotter
	cfg     *config.Manager
	eval    *alert.Evaluator
	version string
}

// Snapshot collects, stamps and evaluates.
func (e *enrichedSnapshotter) Snapshot() (*types.Snapshot, error) {
	snap, err := e.inner.Snapshot()
	if err != nil {
		return nil, err
	}
	snap.Version = e.version
	snap.Alerts = e.eval.Evaluate(e.cfg.Current().Alerts, snap, time.Now())
	return snap, nil
}

// newEnrichedSnapshotter wraps inner so its snapshots carry version and alerts.
func newEnrichedSnapshotter(inner monitor.Snapshotter, mgr *config.Manager, version string) monitor.Snapshotter {
	return &enrichedSnapshotter{
		inner:   inner,
		cfg:     mgr,
		eval:    alert.New(),
		version: version,
	}
}

// loadServeConfig builds the configuration manager for the serve command.
//
// Flags outrank every other source, but only when actually supplied: a flag
// left at its default must not silently override a value from the config file,
// so only flags the user changed are passed through.
func loadServeConfig(cmd *cobra.Command) (*config.Manager, error) {
	loader := config.NewLoader(serveConfigPath)

	// With no cobra command there is no flag state to consult, which is the
	// case when the serve loop is driven directly rather than from the CLI. The
	// caller set these variables deliberately, so they are treated as supplied.
	set := func(name string) bool {
		if cmd == nil {
			return true
		}
		return cmd.Flags().Changed(name)
	}

	if set("addr") && serveAddr != "" {
		loader.SetFlag("server.addr", serveAddr)
	}
	if set("interval") && serveIntervalMs > 0 {
		loader.SetFlag("server.interval_ms", strconv.Itoa(serveIntervalMs))
	}
	if set("tls-cert") && serveTLSCert != "" {
		loader.SetFlag("server.tls_cert", serveTLSCert)
	}
	if set("tls-key") && serveTLSKey != "" {
		loader.SetFlag("server.tls_key", serveTLSKey)
	}

	mgr, err := config.NewManager(loader)
	if err != nil {
		return nil, err
	}

	// The local-name collision is checked here rather than during parsing,
	// because the name depends on this machine (it falls back to the hostname)
	// and configuration loading must not -- the same file has to load the same
	// way wherever it runs. Registered as a validator so a reload that
	// introduces a collision is refused before it is published, leaving the
	// running configuration in place rather than going live and being reported
	// afterwards.
	//
	// The name is taken from the configuration being validated, not from the
	// manager: fleet.local_name is itself reloadable, and during a reload the
	// manager still holds the previous generation, so consulting it would check
	// the incoming peers against the outgoing name.
	mgr.OnValidate(func(c *config.Config) error {
		return c.ValidateAgainstLocalName(c.LocalName())
	})
	if err := mgr.Validate(mgr.Current()); err != nil {
		return nil, err
	}

	return mgr, nil
}

// watchReloadSignals reloads the configuration on SIGHUP, the conventional way
// to tell a daemon to re-read its settings. It is the primary reload path
// precisely because it adds no HTTP surface to a service that has no
// authentication of its own.
//
// Settings consumed when the listener bound cannot change in place; they are
// logged as needing a restart rather than being applied silently.
func watchReloadSignals(mgr *config.Manager, done <-chan struct{}, logger *slog.Logger) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)

	go func() {
		defer signal.Stop(sigs)
		for {
			select {
			case <-done:
				return
			case <-sigs:
				deferred, err := mgr.Reload()
				// A rejected setting is not a rejected reload: the new
				// configuration is already live for every component that
				// accepted it. Reporting "kept the previous settings" here
				// would send an operator hunting for a change that did happen.
				var applyErr *config.ErrApply
				if err != nil && !errors.As(err, &applyErr) {
					logger.Warn("config: SIGHUP reload failed, keeping previous settings", "error", err)
					continue
				}
				logger.Info("config: reloaded on SIGHUP", "path", mgr.Path())
				if applyErr != nil {
					logger.Warn("config: reload applied, but some settings were rejected",
						"error", applyErr)
				}
				if len(deferred) > 0 {
					logger.Warn("config: these settings need a restart to take effect",
						"settings", deferred)
				}
			}
		}
	}()
}
