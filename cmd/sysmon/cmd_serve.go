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
	serveDockerAPI  bool
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web server",
	Long:  "Start the HTTP/WebSocket server that streams live system metrics to connected browsers.",
	RunE:  runServeCmd,
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":8080", "HTTP/WebSocket listen address")
	serveCmd.Flags().IntVar(&serveIntervalMs, "interval", 1000, "Polling interval in milliseconds")
	serveCmd.Flags().BoolVar(&serveDockerAPI, "docker", false,
		"Query the container runtime API for image inventory and storage usage. "+
			"Off by default: the runtime socket grants control of the daemon.")
	rootCmd.AddCommand(serveCmd)
}

func runServeCmd(_ *cobra.Command, _ []string) error {
	if serveIntervalMs <= 0 {
		return &types.InvalidIntervalError{Message: "interval must be greater than zero"}
	}
	interval := time.Duration(serveIntervalMs) * time.Millisecond

	slog.Info("starting sysmon web server",
		"addr", serveAddr,
		"interval", interval,
		"version", version,
		"commit", gitCommit,
		"built", buildDate,
	)

	// Report environment problems once, up front. The dashboard cannot show
	// what the process is not permitted to read, and a silent gap looks like
	// an idle machine rather than a missing permission.
	runStartupChecks()

	sc := collector.NewSystemCollector(slog.Default())
	sc.EnableRuntimeAPI(serveDockerAPI)
	if serveDockerAPI {
		slog.Info("container runtime API enabled", "note", "image inventory will be collected")
	}
	snap := newMonitorSnapshotter(sc)

	mon, err := monitor.New(snap, interval)
	if err != nil {
		return err
	}

	if err := mon.Start(); err != nil {
		return err
	}

	webAssets, err := fs.Sub(frontendAssets, "frontend/dist")
	if err != nil {
		stopMonitor(mon)
		return err
	}

	srv := server.New(mon, serveAddr, webAssets)
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
	// WebSocket handlers send a close frame before the process exits. Hijacked
	// connections are not tracked by http.Server.Shutdown, so this is the only
	// thing that gives streaming clients a clean disconnect.
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
