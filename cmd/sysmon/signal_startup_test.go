package main

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/server"
)

// A stop signal arriving between the bind and the first request must be
// handled, not fatal.
//
// `runServe` binds the socket before anything serves. Until #106 the signal
// handler was installed after that, inside serveUntilSignal, so in the window
// between the two the process was reachable on its port with SIGTERM still at
// its default disposition: `systemctl stop` during startup killed it outright,
// with no clean shutdown, no monitor stop and no drained connections.
//
// The window is a few microseconds wide, which is why it survived: a test that
// waits for the port and then signals wins the race nearly every time, and the
// one CI failure that caught it (#102, job 45534) reported only
// "signal: terminated" with no test name, because the test binary itself was
// killed. The hook below occupies that instant deterministically instead.
func TestServe_StopDuringStartupIsHandled(t *testing.T) {
	previous := listenHook
	t.Cleanup(func() { listenHook = previous })

	// Sent from inside the window: the socket is bound, nothing is serving.
	// Before the fix this killed the test binary.
	listenHook = func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("send SIGTERM: %v", err)
		}
	}

	// The test needs a bound listener to race the signal against, not a
	// particular one. Taking the default :8080 made it fail on any host already
	// serving that port -- with the same non-nil error a regression here would
	// produce (#152). Port 0 lets the kernel choose; loopback keeps the suite
	// from opening a port to the network mid-run.
	previousAddr, previousInterval := serveAddr, serveIntervalMs
	serveAddr, serveIntervalMs = "127.0.0.1:0", 1000
	t.Cleanup(func() { serveAddr, serveIntervalMs = previousAddr, previousInterval })

	done := make(chan error, 1)
	go func() { done <- runServeCmd(nil, nil) }()

	select {
	case err := <-done:
		// The process survived the signal and shut down on its own terms.
		if err != nil {
			t.Errorf("runServeCmd() = %v, want nil after a stop during startup", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runServeCmd did not return after a SIGTERM sent between the bind and serving")
	}
}

// TestServeUntilSignal_AlreadyCancelledNeverServes covers the other half: a
// signal that arrived during startup means the server is torn down without
// ever having served a request.
//
// Without the check, the select below would still shut down cleanly -- but only
// after Start() had opened for business, so a client could connect, be accepted
// and then be dropped mid-request by a process that had already been told to
// stop. Answering one request and abandoning it is worse than never answering.
func TestServeUntilSignal_AlreadyCancelledNeverServes(t *testing.T) {
	mon := startedMonitor(t)

	// Reserve then release a port, so the address is free and known.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	srv := server.New(mon, addr, nil)
	if _, err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal arrived while the collectors were starting

	done := make(chan error, 1)
	go func() { done <- serveUntilSignal(ctx, srv, mon) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilSignal() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveUntilSignal did not return for an already-cancelled context")
	}

	// The socket is closed rather than left listening on a process that is on
	// its way out.
	conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if dialErr == nil {
		conn.Close() //nolint:errcheck
		t.Error("the listener is still accepting after a shutdown during startup")
	}
}
