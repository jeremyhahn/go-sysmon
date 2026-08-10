//go:build !desktop

package main

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

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
	defer l.Close() //nolint:errcheck

	mon := startedMonitor(t)
	srv := server.New(mon, l.Addr().String(), nil)

	err = serveUntilSignal(srv, mon)
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
	go func() { done <- serveUntilSignal(srv, mon) }()

	// Wait for the listener to come up before signalling.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close() //nolint:errcheck
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// serveUntilSignal installs a handler, so this does not terminate the test
	// binary.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilSignal() = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serveUntilSignal did not return after SIGTERM")
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
	deadline := time.Now().Add(10 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close() //nolint:errcheck
			up = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !up {
		t.Fatal("runServeCmd never started listening")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServeCmd() = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServeCmd did not return after SIGTERM")
	}
}
