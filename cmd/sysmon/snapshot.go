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

// collectSnapshot creates a SystemCollector with tiering disabled, samples it
// with sampleSnapshot, and returns the result. It is the shared entry point for
// all one-shot CLI commands.
//
// It is a variable rather than a plain function so that unit tests can supply
// a synthetic snapshot: every command funnels through here, and reading real
// hardware in a unit test would be both slow and machine-dependent.
var collectSnapshot = func() (*types.Snapshot, error) {
	sc := collector.NewSystemCollector(slog.Default())
	sc.SetTiering(false)
	return sampleSnapshot(cmdContext(), sc, sampleWait)
}

// sampleWait is the wait collectSnapshot uses between its readings. A test
// that runs the real collector replaces it, so it never sleeps on the clock.
var sampleWait = waitSample

// oneShotSampleInterval is how long a one-shot command waits between its two
// readings. CPU usage, and every other rate, is the difference of two readings,
// so a command that read once could never report one; this is what top -n1 and
// mpstat do too. Half a second is the dashboard's fastest collection interval,
// and at the kernel's usual 100 ticks a second it resolves a core's usage to 2%.
const oneShotSampleInterval = 500 * time.Millisecond

// systemSnapshotter is the part of collector.SystemCollector sampleSnapshot
// reads, so a test can hand it readings of its choosing.
type systemSnapshotter interface {
	Snapshot(ctx context.Context) (types.Snapshot, error)
	WaitDriveSlots(ctx context.Context) error
}

// sampleSnapshot takes a priming reading, waits oneShotSampleInterval through
// wait, and returns the second reading, whose rates are measured across the
// wait. A figure the second reading still could not measure -- a core whose
// /proc/stat line is malformed -- stays marked unknown in it. The priming
// reading starts the drive-slot poll in the background; the second reading
// waits for it to conclude, so it carries the bays rather than "not taken yet".
func sampleSnapshot(ctx context.Context, sc systemSnapshotter, wait func(context.Context, time.Duration) error) (*types.Snapshot, error) {
	if _, err := sc.Snapshot(ctx); err != nil {
		return nil, err
	}
	if err := wait(ctx, oneShotSampleInterval); err != nil {
		return nil, err
	}
	if err := sc.WaitDriveSlots(ctx); err != nil {
		return nil, err
	}
	snap, err := sc.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// waitSample blocks for d or until ctx is done, whichever is first. It is what
// sampleSnapshot waits with outside tests.
func waitSample(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return &types.CollectorError{Collector: "cli", Cause: ctx.Err()}
	case <-timer.C:
		return nil
	}
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
