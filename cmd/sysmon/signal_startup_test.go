package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
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
	listenHook = func(net.Addr) {
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

	// The process survived the signal and shut down on its own terms.
	if err := await.Recv(t, "runServeCmd to return after a SIGTERM sent between the bind and serving", done); err != nil {
		t.Errorf("runServeCmd() = %v, want nil after a stop during startup", err)
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

	srv := server.New(mon, "127.0.0.1:0", nil)
	bound, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if !listensOn(t, bound) {
		t.Fatalf("no listening socket on %v after Listen, so the check below proves nothing", bound)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal arrived while the collectors were starting

	done := make(chan error, 1)
	go func() { done <- serveUntilSignal(ctx, srv, mon, nil) }()

	if err := await.Recv(t, "serveUntilSignal to return for an already-cancelled context", done); err != nil {
		t.Errorf("serveUntilSignal() = %v, want nil", err)
	}

	// The socket is closed rather than left listening on a process that is on
	// its way out. This asks the kernel whether this process still holds a
	// listening socket on the port, rather than dialling it: once released,
	// the port may belong to another process at once, and a dial would find
	// that process's listener and blame this one (#229).
	if listensOn(t, bound) {
		t.Error("the listener is still open after a shutdown during startup")
	}
}

// listensOn reports whether this process holds a listening TCP socket on
// addr's port, from the kernel's socket table and this process's descriptors.
func listensOn(t *testing.T, addr net.Addr) bool {
	t.Helper()
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("%v is not a TCP address", addr)
	}
	table, err := os.ReadFile("/proc/self/net/tcp")
	if err != nil {
		t.Fatalf("read the socket table: %v", err)
	}
	port := fmt.Sprintf(":%04X", tcp.Port)
	const listen = "0A"
	inodes := map[string]bool{}
	for _, line := range strings.Split(string(table), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 9 && strings.HasSuffix(f[1], port) && f[3] == listen {
			inodes["socket:["+f[9]+"]"] = true
		}
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read descriptors: %v", err)
	}
	for _, fd := range fds {
		// A descriptor closed between the listing and the read is not a
		// listener this process holds.
		if link, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && inodes[link] {
			return true
		}
	}
	return false
}
