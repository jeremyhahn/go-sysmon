//go:build !desktop

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// serveStub is a Snapshotter that never touches real hardware.
type serveStub struct{}

func (serveStub) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Host: types.HostInfo{Hostname: "serve-test"}}, nil
}

// startedMonitor returns a running monitor for serve tests.
func startedMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(serveStub{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	return m
}

// awaitListening returns once addr accepts a connection. A listener coming up
// is not announced to anything outside the code under test, so this polls it,
// dialling afresh on every pass.
func awaitListening(t *testing.T, addr string) {
	t.Helper()
	var dialer net.Dialer
	await.Until(t, "the server to listen on "+addr, func(ctx context.Context) bool {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		if err := conn.Close(); err != nil {
			t.Errorf("closing the probe connection: %v", err)
		}
		return true
	})
}

// ---- runServeCmd validation -----------------------------------------------

// TestRunServeCmd_RejectsNonPositiveInterval verifies the guard returns the
// typed error rather than starting a server that polls in a tight loop.
func TestRunServeCmd_RejectsNonPositiveInterval(t *testing.T) {
	original := serveIntervalMs
	t.Cleanup(func() { serveIntervalMs = original })

	for _, v := range []int{0, -1, -1000} {
		serveIntervalMs = v
		err := runServeCmd(nil, nil)
		if err == nil {
			t.Fatalf("runServeCmd() with interval %d = nil, want an error", v)
		}
		var invalid *types.InvalidIntervalError
		if !errors.As(err, &invalid) {
			t.Errorf("interval %d: error = %T (%v), want *types.InvalidIntervalError", v, err, err)
		}
	}
}

// ---- serveUntilSignal -----------------------------------------------------

// TestServeUntilSignal_ReturnsListenError covers the branch where the listener
// fails immediately: the error must be returned and the monitor stopped, not
// left running in the background.
func TestServeUntilSignal_ReturnsListenError(t *testing.T) {
	// Occupy a port so the server cannot bind it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Errorf("closing the occupying listener: %v", err)
		}
	}()

	mon := startedMonitor(t)
	srv := server.New(mon, l.Addr().String(), nil)

	err = serveUntilSignal(context.Background(), srv, mon)
	if err == nil {
		t.Fatal("serveUntilSignal() = nil, want the bind error")
	}
	var startErr *types.ServerStartError
	if !errors.As(err, &startErr) {
		t.Errorf("error = %T (%v), want *types.ServerStartError", err, err)
	}

	// The monitor must already be stopped, so a second Stop reports that.
	var notRunning *types.MonitorNotRunningError
	if stopErr := mon.Stop(); stopErr == nil || !errors.As(stopErr, &notRunning) {
		t.Errorf("monitor still running after a failed serve; Stop() = %v", stopErr)
	}
}

// TestServeUntilSignal_ShutsDownOnSignal covers the signal path end to end:
// SIGTERM must stop the server cleanly and return nil.
func TestServeUntilSignal_ShutsDownOnSignal(t *testing.T) {
	// Reserve then release a port so the server can bind it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	mon := startedMonitor(t)
	srv := server.New(mon, addr, nil)

	done := make(chan error, 1)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { done <- serveUntilSignal(ctx, srv, mon) }()

	// Wait for the listener to come up before signalling.
	awaitListening(t, addr)

	// serveUntilSignal installs a handler, so this does not terminate the test
	// binary.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	if err := await.Recv(t, "serveUntilSignal to return after SIGTERM", done); err != nil {
		t.Errorf("serveUntilSignal() = %v, want nil after a clean shutdown", err)
	}
}

// ---- stopMonitor ----------------------------------------------------------

// TestStopMonitor_Running stops a live monitor.
func TestStopMonitor_Running(t *testing.T) {
	mon := startedMonitor(t)
	stopMonitor(mon)

	var notRunning *types.MonitorNotRunningError
	if err := mon.Stop(); err == nil || !errors.As(err, &notRunning) {
		t.Errorf("monitor still running after stopMonitor; Stop() = %v", err)
	}
}

// TestStopMonitor_AlreadyStopped verifies the logged-and-ignored path does not
// panic when the monitor was never started.
func TestStopMonitor_AlreadyStopped(t *testing.T) {
	m, err := monitor.New(serveStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stopMonitor panicked on a stopped monitor: %v", r)
		}
	}()
	stopMonitor(m)
}

// TestRunServeCmd_ServesAndShutsDown drives runServeCmd end to end: it builds
// the real collector, monitor and server, serves on an ephemeral port, and
// exits cleanly on SIGTERM.
func TestRunServeCmd_ServesAndShutsDown(t *testing.T) {
	// Reserve then release a port so the command can bind it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	origAddr, origInterval := serveAddr, serveIntervalMs
	t.Cleanup(func() { serveAddr, serveIntervalMs = origAddr, origInterval })
	serveAddr = addr
	serveIntervalMs = 1000

	done := make(chan error, 1)
	go func() { done <- runServeCmd(nil, nil) }()

	// Wait for the listener before signalling.
	awaitListening(t, addr)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	if err := await.Recv(t, "runServeCmd to return after SIGTERM", done); err != nil {
		t.Errorf("runServeCmd() = %v, want nil after a clean shutdown", err)
	}
}
