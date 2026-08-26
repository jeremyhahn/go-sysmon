package main

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// shutdownTimeout bounds how long the server waits for in-flight HTTP requests
// to finish after a shutdown signal before closing them.
const shutdownTimeout = 10 * time.Second

var (
	serveAddr       string
	serveIntervalMs int
	serveTLSCert    string
	serveTLSKey     string
	serveConfigPath string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web server",
	Long: "Start the HTTP server that streams live system metrics to connected browsers " +
		"as server-sent events.",
	RunE: runServeCmd,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":8080", "HTTP listen address")
	serveCmd.Flags().IntVar(&serveIntervalMs, "interval", 1000, "Polling interval in milliseconds")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "", "Path to a PEM certificate; enables HTTPS")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "", "Path to the PEM private key matching --tls-cert")
	serveCmd.Flags().StringVar(&serveConfigPath, "config", config.DefaultPath,
		"Path to the YAML configuration file; missing is not an error")
	rootCmd.AddCommand(serveCmd)
}

// serveTLSConfig builds the server TLS settings from the command flags,
// returning nil when neither flag was supplied.
func serveTLSConfig(cfg *config.Config) *server.TLS {
	if cfg.Server.TLSCert == "" && cfg.Server.TLSKey == "" {
		return nil
	}
	return &server.TLS{CertFile: cfg.Server.TLSCert, KeyFile: cfg.Server.TLSKey}
}

// localNodeName is how this node identifies itself in the fleet view. The
// hostname is the name an operator already knows the machine by, but a
// configured name wins: a repurposed machine can carry a hostname that no
// longer describes its role, and relabelling a dashboard is far cheaper than
// renaming a running host.
func localNodeName(mgr *config.Manager) string {
	if mgr == nil {
		d := config.Defaults()
		return d.LocalName()
	}
	// Resolved by the config itself, so the name published to the fleet is the
	// same one peers are validated against at load.
	return mgr.Current().LocalName()
}

func runServeCmd(cmd *cobra.Command, _ []string) error {
	if serveIntervalMs <= 0 {
		return &types.InvalidIntervalError{Message: "interval must be greater than zero"}
	}

	cfgMgr, err := loadServeConfig(cmd)
	if err != nil {
		return err
	}
	cfg := cfgMgr.Current()
	interval := time.Duration(cfg.Server.IntervalMs) * time.Millisecond

	slog.Info("starting sysmon web server",
		"addr", cfg.Server.Addr,
		"interval", interval,
		"config", cfgMgr.Path(),
		"peers", len(cfg.Fleet.Peers),
		"version", version,
		"commit", gitCommit,
		"built", buildDate,
	)

	// Report environment problems once, up front. The dashboard cannot show
	// what the process is not permitted to read, and a silent gap looks like
	// an idle machine rather than a missing permission.
	runStartupChecks()

	sc := collector.NewSystemCollector(slog.Default())
	snap := newEnrichedSnapshotter(newMonitorSnapshotter(sc), cfgMgr, version)

	mon, err := monitor.New(snap, interval)
	if err != nil {
		return err
	}

	// The polling interval is consumed once, when the monitor is built. Without
	// this applier a reload would change what GET /api/config reports while the
	// collection loop kept running at its original cadence -- the settings view
	// would be describing a service that does not exist.
	cfgMgr.OnReload(func(c *config.Config) error {
		ms := c.Server.IntervalMs
		if ms <= 0 {
			return &types.InvalidIntervalError{Message: "interval_ms must be greater than zero"}
		}
		return mon.SetInterval(time.Duration(ms) * time.Millisecond)
	})

	// done bounds the reload watchers and the fleet poller to the lifetime of
	// this command.
	done := make(chan struct{})
	defer close(done)

	watchReloadSignals(cfgMgr, done, slog.Default())
	if cfg.Server.Watch {
		if err := cfgMgr.Watch(done, slog.Default(), func(deferred []string) {
			if len(deferred) > 0 {
				slog.Warn("config: these settings need a restart to take effect", "settings", deferred)
			}
		}); err != nil {
			slog.Warn("config: cannot watch the config file", "error", err)
		}
	}

	// The fleet poller reads its peer list from the config manager on every
	// cycle, so peers can be added or removed by reloading rather than
	// restarting -- which would reset Prometheus counters and drop every
	// connected browser.
	fleetPoller := fleet.New(
		func() config.Fleet { return cfgMgr.Current().Fleet },
		func() *types.Snapshot {
			s, err := mon.Snapshot()
			if err != nil {
				return nil
			}
			return s
		},
		func() string { return localNodeName(cfgMgr) },
		slog.Default(),
	)
	go fleetPoller.Run(done)

	if err := mon.Start(); err != nil {
		return err
	}

	webAssets, err := fs.Sub(frontendAssets, "frontend/dist")
	if err != nil {
		stopMonitor(mon)
		return err
	}

	srv, err := server.NewWithConfig(server.Config{
		Monitor:   mon,
		Addr:      cfg.Server.Addr,
		Assets:    webAssets,
		TLS:       serveTLSConfig(cfg),
		Fleet:     fleetPoller,
		Config:    cfgMgr,
		Version:   version,
		GitCommit: gitCommit,
		BuildDate: buildDate,
	})
	if err != nil {
		stopMonitor(mon)
		return err
	}

	return serveUntilSignal(srv, mon)
}

// serveUntilSignal runs srv until it fails or the process receives SIGINT or
// SIGTERM, then shuts both srv and mon down cleanly. It returns the error that
// ended the server, or nil when the shutdown was signal-initiated and clean.
func serveUntilSignal(srv *server.Server, mon *monitor.Monitor) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start blocks, so run it alongside the signal wait. The buffered channel
	// keeps the goroutine from leaking if a signal wins the race.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	select {
	case err := <-errCh:
		// The listener failed (for example, the address is already in use).
		// There is nothing to shut down beyond the monitor.
		stopMonitor(mon)
		return err

	case <-ctx.Done():
		slog.Info("shutdown signal received, stopping server")
	}

	// Stop the monitor first: closing each subscriber's Done channel lets the
	// event-stream handlers emit a final "bye" event and return. Without it,
	// Shutdown would block on those handlers until the timeout expires, since
	// a stream that is working as intended never finishes on its own.
	stopMonitor(mon)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Stop(shutdownCtx); err != nil {
		slog.Error("server shutdown", "err", err)
		return err
	}

	slog.Info("server stopped cleanly")
	return nil
}

// stopMonitor stops mon, logging rather than propagating a failure: it is only
// ever called on a path that is already terminating.
func stopMonitor(mon *monitor.Monitor) {
	if err := mon.Stop(); err != nil {
		slog.Warn("monitor stop", "err", err)
	}
}
