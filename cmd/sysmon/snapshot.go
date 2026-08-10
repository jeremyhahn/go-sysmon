package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"

	"github.com/spf13/cobra"
)

// snapshotterAdapter adapts collector.SystemCollector to the
// monitor.Snapshotter interface. SystemCollector.Snapshot accepts a context
// and returns a value; the monitor interface expects a pointer and no context.
type snapshotterAdapter struct {
	sc *collector.SystemCollector
}

// Snapshot implements monitor.Snapshotter.
func (a *snapshotterAdapter) Snapshot() (*types.Snapshot, error) {
	snap, err := a.sc.Snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// collectSnapshot creates a SystemCollector with tiering disabled, takes a
// single snapshot, and returns it. It is the shared entry point for all
// one-shot CLI commands.
//
// It is a variable rather than a plain function so that unit tests can supply
// a synthetic snapshot: every command funnels through here, and reading real
// hardware in a unit test would be both slow and machine-dependent.
var collectSnapshot = func() (*types.Snapshot, error) {
	sc := collector.NewSystemCollector(slog.Default())
	sc.SetTiering(false)
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// newMonitorSnapshotter wraps a SystemCollector in a snapshotterAdapter
// suitable for passing to monitor.New.
func newMonitorSnapshotter(sc *collector.SystemCollector) monitor.Snapshotter {
	return &snapshotterAdapter{sc: sc}
}

// runCmdFunc is the signature for a command's rendering logic. It receives a
// snapshot and writes output to the command's writer.
type runCmdFunc func(cmd *cobra.Command, snap *types.Snapshot) error

// runWithRefresh wraps a command's render function. When --refresh is set it
// reuses a single SystemCollector and loops at the requested interval, clearing
// the terminal between frames. When --refresh is 0 it behaves as a one-shot.
func runWithRefresh(cmd *cobra.Command, render runCmdFunc) error {
	if refreshInterval <= 0 {
		snap, err := collectSnapshot()
		if err != nil {
			return err
		}
		return render(cmd, snap)
	}

	sc := collector.NewSystemCollector(slog.Default())
	sc.SetTiering(false) // CLI refresh mode: poll everything at the requested rate
	interval := time.Duration(refreshInterval * float64(time.Second))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	for {
		start := time.Now()

		snap, err := sc.Snapshot(context.Background())
		if err != nil {
			return err
		}
		snapPtr := &snap

		// Clear terminal: move cursor to top-left and clear screen. A failure
		// here means the output stream is gone, so stop rather than loop on a
		// dead pipe.
		if _, err := cmd.OutOrStdout().Write([]byte("\033[H\033[2J")); err != nil {
			return err
		}

		if err := render(cmd, snapPtr); err != nil {
			return err
		}

		// Sleep for the remaining interval after subtracting collection + render time.
		elapsed := time.Since(start)
		remaining := interval - elapsed
		if remaining <= 0 {
			remaining = time.Millisecond
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(remaining):
		}
	}
}

// cmdContext returns the context one-shot commands collect under.
func cmdContext() context.Context {
	return context.Background()
}
