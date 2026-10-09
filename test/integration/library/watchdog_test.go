//go:build integration

// Package library drives the importable packages -- the watchdog, the monitor
// and the configuration loader -- the way an application that embeds go-sysmon
// does, against real sockets, files and goroutines, along the paths a failure
// takes.
package library

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/sdnotify"
	"github.com/jeremyhahn/go-sysmon/pkg/watchdog"
)

// pingEvery is the watchdog interval the tests use.
const pingEvery = 20 * time.Millisecond

// manager is a service manager's notification socket: a real unixgram socket
// the watchdog's pings arrive on.
type manager struct {
	conn     *net.UnixConn
	notifier *sdnotify.Notifier
}

// startManager listens on a notification socket and connects a Notifier to it.
func startManager(t *testing.T) *manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on the notification socket: %v", err)
	}
	t.Cleanup(func() {
		// A test may close the socket itself to simulate a lost manager.
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close the notification socket: %v", err)
		}
	})
	env := map[string]string{"NOTIFY_SOCKET": path}
	notifier, err := sdnotify.New(func(k string) string { return env[k] }, 1)
	if err != nil {
		t.Fatalf("sdnotify.New: %v", err)
	}
	t.Cleanup(func() {
		if err := notifier.Close(); err != nil {
			t.Errorf("close the notifier: %v", err)
		}
	})
	return &manager{conn: conn, notifier: notifier}
}

// pings counts the datagrams that arrive, in the background, until the test
// ends.
func (m *manager) pings(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	go func() {
		buf := make([]byte, 64)
		for {
			size, _, err := m.conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			if string(buf[:size]) == "WATCHDOG=1" {
				n.Add(1)
			}
		}
	}()
	return &n
}

// runWatchdog runs the watchdog until the test ends.
func runWatchdog(t *testing.T, w *watchdog.Watchdog) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		await.Recv(t, "the watchdog to return after cancellation", done)
	})
}

// TestWatchdog_APingReachesTheManagerWhileTheServerAnswers is the healthy
// case: a probe that succeeds and a monitor that is collecting yield pings, and
// a watchdog built without a logger falls back to the default one.
func TestWatchdog_APingReachesTheManagerWhileTheServerAnswers(t *testing.T) {
	m := startManager(t)
	got := m.pings(t)
	w := watchdog.New(m.notifier, pingEvery,
		func(context.Context) error { return nil },
		func() error { return nil },
		nil)
	runWatchdog(t, w)
	await.Until(t, "pings to reach the manager", func(context.Context) bool { return got.Load() >= 2 })
}

// TestWatchdog_NoPingWhenTheServerDoesNotAnswerItsOwnRequest is the failure
// the watchdog exists for: accept is wedged, the probe fails, and the manager
// must hear nothing so that it restarts the service.
func TestWatchdog_NoPingWhenTheServerDoesNotAnswerItsOwnRequest(t *testing.T) {
	m := startManager(t)
	got := m.pings(t)
	var probed atomic.Int64
	w := watchdog.New(m.notifier, pingEvery,
		func(context.Context) error {
			probed.Add(1)
			return errors.New("accept is wedged")
		},
		func() error { return nil },
		slog.New(slog.DiscardHandler))
	runWatchdog(t, w)

	await.Until(t, "the probe to fail on several ticks", func(context.Context) bool { return probed.Load() >= 3 })
	if n := got.Load(); n != 0 {
		t.Fatalf("the manager received %d pings although every probe failed", n)
	}
}

// TestWatchdog_NoPingWhileCollectionIsStale covers the other signal: the server
// answers, but the monitor's collection loop is wedged.
func TestWatchdog_NoPingWhileCollectionIsStale(t *testing.T) {
	m := startManager(t)
	got := m.pings(t)
	var checked atomic.Int64
	w := watchdog.New(m.notifier, pingEvery,
		func(context.Context) error { return nil },
		func() error {
			checked.Add(1)
			return errors.New("the last snapshot is stale")
		},
		slog.New(slog.DiscardHandler))
	runWatchdog(t, w)

	await.Until(t, "the freshness check to fail on several ticks", func(context.Context) bool { return checked.Load() >= 3 })
	if n := got.Load(); n != 0 {
		t.Fatalf("the manager received %d pings although collection was stale", n)
	}
}

// TestWatchdog_RunsNothingWhenNoWatchdogWasRequested is the service started
// outside systemd, or under a unit without WatchdogSec: Run returns at once and
// nothing is sent.
func TestWatchdog_RunsNothingWhenNoWatchdogWasRequested(t *testing.T) {
	m := startManager(t)
	got := m.pings(t)
	probe := func(context.Context) error { return nil }
	tests := map[string]*watchdog.Watchdog{
		"no notifier": watchdog.New(nil, pingEvery, probe, nil, slog.New(slog.DiscardHandler)),
		"no interval": watchdog.New(m.notifier, 0, probe, nil, slog.New(slog.DiscardHandler)),
		"no probe":    watchdog.New(m.notifier, pingEvery, nil, nil, slog.New(slog.DiscardHandler)),
	}
	for name, w := range tests {
		done := make(chan struct{})
		go func() {
			w.Run(context.Background())
			close(done)
		}()
		await.Recv(t, name+": Run to return without being cancelled", done)
	}
	if n := got.Load(); n != 0 {
		t.Fatalf("the manager received %d pings from an unconfigured watchdog", n)
	}
}

// TestWatchdog_ALostNotificationSocketDoesNotStopTheWatchdog closes the
// manager's socket under a healthy service: the ping fails, the watchdog keeps
// ticking, and it does not panic or exit.
func TestWatchdog_ALostNotificationSocketDoesNotStopTheWatchdog(t *testing.T) {
	m := startManager(t)
	if err := m.conn.Close(); err != nil {
		t.Fatalf("close the notification socket: %v", err)
	}
	var probed atomic.Int64
	w := watchdog.New(m.notifier, pingEvery,
		func(context.Context) error { probed.Add(1); return nil },
		func() error { return nil },
		slog.New(slog.DiscardHandler))
	runWatchdog(t, w)
	await.Until(t, "the watchdog to keep ticking after failed pings", func(context.Context) bool { return probed.Load() >= 3 })
}
