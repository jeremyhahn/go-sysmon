package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// runServeWithConfig runs the serve command against a config file holding
// body, and returns what it returned.
func runServeWithConfig(t *testing.T, body string) error {
	t.Helper()
	withServeConfig(t, body)
	done := make(chan error, 1)
	go func() { done <- runServeCmd(nil, nil) }()
	return await.Recv(t, "runServeCmd to return", done)
}

// stopAtBind makes the serve command stop itself the moment its listener is
// bound, the same instant TestServe_StopDuringStartupIsHandled occupies.
func stopAtBind(t *testing.T) {
	t.Helper()
	previous := listenHook
	t.Cleanup(func() { listenHook = previous })
	listenHook = func(net.Addr) {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("send SIGTERM: %v", err)
		}
	}
}

// TestRunServeCmd_UnreadableFleetCAIsAStartupError verifies a fleet CA that
// cannot be read stops the service at startup, before it binds, rather than
// surfacing later as every peer being unreachable.
func TestRunServeCmd_UnreadableFleetCAIsAStartupError(t *testing.T) {
	previous := listenHook
	t.Cleanup(func() { listenHook = previous })
	listenHook = func(net.Addr) { t.Error("the service bound its listener despite an unusable fleet CA") }

	ca := filepath.Join(t.TempDir(), "absent-ca.pem")
	err := runServeWithConfig(t, "server:\n  addr: \"127.0.0.1:0\"\nfleet:\n  tls:\n    ca_file: "+ca+"\n")

	if err == nil || !strings.Contains(err.Error(), "absent-ca.pem") {
		t.Fatalf("runServeCmd() = %v, want an error naming the unreadable CA", err)
	}
}

// TestRunServeCmd_BusyAddressIsAStartupError verifies an address another
// socket holds is reported by the command, not by a goroutine nobody reads.
func TestRunServeCmd_BusyAddressIsAStartupError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	t.Cleanup(func() {
		if err := held.Close(); err != nil {
			t.Errorf("release the port: %v", err)
		}
	})

	err = runServeWithConfig(t, "server:\n  addr: \""+held.Addr().String()+"\"\n")

	var startErr *types.ServerStartError
	if !errors.As(err, &startErr) || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("runServeCmd() = %v, want a *types.ServerStartError for the address in use", err)
	}
}

// sawHandler is a slog.Handler that reports, once, a record whose message
// contains want. It never blocks and never closes anything, since goroutines
// the service started may still log after the command has returned.
type sawHandler struct {
	want string
	saw  chan struct{}
}

func (h sawHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h sawHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.want) {
		select {
		case h.saw <- struct{}{}:
		default:
		}
	}
	return nil
}
func (h sawHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h sawHandler) WithGroup(string) slog.Handler      { return h }

// TestRunServeCmd_WarnsWhenFleetVerificationIsOff verifies a fleet running
// with peer certificate verification disabled says so at startup -- the only
// place it is visible -- and that a watched config starts cleanly.
func TestRunServeCmd_WarnsWhenFleetVerificationIsOff(t *testing.T) {
	h := sawHandler{want: "peer certificate verification is DISABLED", saw: make(chan struct{}, 1)}
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(slog.New(h))
	stopAtBind(t)

	err := runServeWithConfig(t, "server:\n  addr: \"127.0.0.1:0\"\n  watch: true\n"+
		"fleet:\n  tls:\n    insecure_skip_verify: true\n")
	if err != nil {
		t.Fatalf("runServeCmd() = %v, want a clean start and stop", err)
	}

	// Logged before the bind, so by the time the command returns it is here.
	select {
	case <-h.saw:
	default:
		t.Error("no startup warning that fleet peer verification is disabled")
	}
}

// datedSnapshotter answers every collection with a snapshot taken at the time
// age before now.
type datedSnapshotter struct{ age time.Duration }

func (d datedSnapshotter) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Timestamp: time.Now().Add(-d.age)}, nil
}

// runningMonitor starts a monitor over s, waits for its first snapshot, and
// then sets its interval to a minute -- so the freshness limit is three
// minutes, far from anything a loaded host could blur.
func runningMonitor(t *testing.T, s monitor.Snapshotter) *monitor.Monitor {
	t.Helper()
	mon, err := monitor.New(s, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { stopMonitor(mon) })
	await.Until(t, "the first collection", func(context.Context) bool {
		_, err := mon.Snapshot()
		return err == nil
	})
	if err := mon.SetInterval(time.Minute); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	return mon
}

// TestCollectionIsCurrent_JudgesTheLatestSnapshotsAge verifies the watchdog's
// freshness check passes a recent collection, fails one older than three
// polling intervals, and fails a monitor that has collected nothing.
func TestCollectionIsCurrent_JudgesTheLatestSnapshotsAge(t *testing.T) {
	if err := collectionIsCurrent(runningMonitor(t, datedSnapshotter{}))(); err != nil {
		t.Errorf("a fresh collection: %v, want nil", err)
	}

	err := collectionIsCurrent(runningMonitor(t, datedSnapshotter{age: time.Hour}))()
	var stale *types.StaleCollectionError
	if !errors.As(err, &stale) || stale.Limit != 3*time.Minute {
		t.Errorf("an hour-old collection: %v, want a *types.StaleCollectionError against 3m", err)
	}

	idle, err := monitor.New(datedSnapshotter{}, time.Minute)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	var notRunning *types.MonitorNotRunningError
	if err := collectionIsCurrent(idle)(); !errors.As(err, &notRunning) {
		t.Errorf("a monitor never started: %v, want a *types.MonitorNotRunningError", err)
	}
}
