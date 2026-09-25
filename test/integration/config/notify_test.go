//go:build integration

package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// notifySocket is a stand-in for systemd's notification socket: a unixgram
// socket delivering every datagram the service sends.
type notifySocket struct {
	env  string
	msgs chan string
	conn *net.UnixConn
}

// listenNotify opens a notification socket at addr -- a path, or "@name" for
// an abstract socket -- and returns it with the NOTIFY_SOCKET value naming it.
func listenNotify(t *testing.T, addr string) *notifySocket {
	t.Helper()
	name := addr
	if addr[0] == '@' {
		name = "\x00" + addr[1:]
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	n := &notifySocket{env: "NOTIFY_SOCKET=" + addr, msgs: make(chan string, 1024), conn: conn}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			n.msgs <- string(buf[:k])
		}
	}()
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close the notification socket: %v", err)
		}
		await.Recv(t, "the notification reader to stop", done)
	})
	return n
}

// awaitMsg waits for the datagram want, discarding the others before it.
func (n *notifySocket) awaitMsg(t *testing.T, want string) {
	t.Helper()
	for {
		if got := await.Recv(t, "the service to send "+want, n.msgs); got == want {
			return
		}
	}
}

// TestNotify_ReadyPingsAndShutdown runs the service the way a Type=notify unit
// with WatchdogSec does: it says READY=1 once it serves, pings WATCHDOG=1 on
// evidence of health at half the watchdog interval, and says it is shutting
// down when stopped.
func TestNotify_ReadyPingsAndShutdown(t *testing.T) {
	n := listenNotify(t, filepath.Join(t.TempDir(), "notify.sock"))
	s := harness.Serve(t, harness.ServeOptions{Env: []string{n.env, "WATCHDOG_USEC=400000"}})

	n.awaitMsg(t, "READY=1")
	s.Logs.AwaitMsg(t, "watchdog: pinging on evidence")
	n.awaitMsg(t, "WATCHDOG=1")
	n.awaitMsg(t, "WATCHDOG=1")

	if err := s.Stop(t); err != nil {
		t.Fatalf("sysmon serve on SIGTERM: %v, want a clean exit", err)
	}
	n.awaitMsg(t, "STATUS=shutting down")
	if !s.Logs.Has(msgStoppedCleanly) {
		t.Error("the service did not log a clean stop")
	}
}

// TestNotify_AbstractSocket verifies an abstract socket, named with a leading
// '@' as systemd names them, is reached rather than looked for on disk.
func TestNotify_AbstractSocket(t *testing.T) {
	n := listenNotify(t, "@sysmon-integration-"+strconv.Itoa(os.Getpid())+"-"+filepath.Base(t.TempDir()))
	harness.Serve(t, harness.ServeOptions{Env: []string{n.env}})
	n.awaitMsg(t, "READY=1")
}

// TestNotify_WatchdogForAnotherProcess verifies WATCHDOG_PID naming another
// process means this one is not being watched: it says READY=1 and does not
// start pinging.
func TestNotify_WatchdogForAnotherProcess(t *testing.T) {
	n := listenNotify(t, filepath.Join(t.TempDir(), "notify.sock"))
	s := harness.Serve(t, harness.ServeOptions{
		Env:  []string{n.env, "WATCHDOG_USEC=400000", "WATCHDOG_PID=1"},
		Args: []string{"--log-level", "debug"},
	})
	n.awaitMsg(t, "READY=1")
	s.Logs.AwaitMsg(t, "watchdog: not configured, no pings will be sent")
}

// TestNotify_UnusableSocketStillServes verifies a notification socket the
// service cannot reach is reported, and the service serves regardless: an
// unwatched service is useful, and refusing would turn a notification problem
// into an outage.
func TestNotify_UnusableSocketStillServes(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{
		Env: []string{"NOTIFY_SOCKET=" + filepath.Join(t.TempDir(), "absent.sock")},
	})
	if r := s.Logs.AwaitMsg(t, "watchdog: notification socket unusable, running unwatched"); r.Level() != "WARN" {
		t.Errorf("an unusable socket is logged at %s, want WARN", r.Level())
	}
	if status, _ := harness.Get(t, s.URL("/api/version")); status != 200 {
		t.Errorf("GET /api/version = %d with no notification socket, want 200", status)
	}
}

// TestNotify_APingThatCannotBeDeliveredIsReported closes the manager's end
// while the service runs: the next ping fails, and the service says so
// rather than believing it is watched.
func TestNotify_APingThatCannotBeDeliveredIsReported(t *testing.T) {
	n := listenNotify(t, filepath.Join(t.TempDir(), "notify.sock"))
	s := harness.Serve(t, harness.ServeOptions{Env: []string{n.env, "WATCHDOG_USEC=400000"}})
	n.awaitMsg(t, "WATCHDOG=1")

	if err := n.conn.Close(); err != nil {
		t.Fatal(err)
	}
	r := s.Logs.AwaitMsg(t, "watchdog: ping failed")
	if r.Level() != "WARN" {
		t.Errorf("a failed ping is logged at %s, want WARN", r.Level())
	}
}

// TestNotify_WatchdogProbesOverTLS runs a watched HTTPS service: its liveness
// probe crosses the TLS listener it actually serves on, and the pings follow.
// Peer verification is off in its fleet settings, which it warns about once.
func TestNotify_WatchdogProbesOverTLS(t *testing.T) {
	p := newPKI(t)
	n := listenNotify(t, filepath.Join(t.TempDir(), "notify.sock"))
	s := harness.Serve(t, harness.ServeOptions{
		Config: "fleet:\n  tls:\n    insecure_skip_verify: true\n",
		Env:    []string{n.env, "WATCHDOG_USEC=400000"},
		Args:   []string{"--tls-cert", p.certFile, "--tls-key", p.keyFile},
		Client: p.client(),
	})
	n.awaitMsg(t, "WATCHDOG=1")
	if r := s.Logs.AwaitMsg(t, "fleet: peer certificate verification is DISABLED "+
		"(fleet.tls.insecure_skip_verify); https peers are encrypted but not authenticated"); r.Level() != "WARN" {
		t.Errorf("disabled verification is logged at %s, want WARN", r.Level())
	}
}
