package sdnotify_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/sdnotify"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// listener stands in for systemd: a datagram socket that records what arrives.
type listener struct {
	conn *net.UnixConn
	addr string
}

// newListener binds a unix datagram socket in a temp directory.
//
// A filesystem socket rather than an abstract one, because the path length
// limit for unix sockets is about 100 bytes and a t.TempDir() under a long
// scratch path can exceed it -- the socket is created in a short directory and
// the test fails clearly if even that is too long.
func newListener(t *testing.T) *listener {
	t.Helper()

	dir, err := os.MkdirTemp("", "sdn")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	addr := filepath.Join(dir, "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &listener{conn: conn, addr: addr}
}

// recv reads one datagram, or fails the test if none arrives.
func (l *listener) recv(t *testing.T) string {
	t.Helper()

	if err := l.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	n, err := l.conn.Read(buf)
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	return string(buf[:n])
}

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestNotifier_SendsEachMessage(t *testing.T) {
	l := newListener(t)

	n, err := sdnotify.New(env(map[string]string{"NOTIFY_SOCKET": l.addr}), os.Getpid())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })

	tests := []struct {
		name string
		send func() error
		want string
	}{
		{"ready", n.Ready, "READY=1"},
		{"watchdog", n.Watchdog, "WATCHDOG=1"},
		{"status", func() error { return n.Status("serving") }, "STATUS=serving"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.send(); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got := l.recv(t); got != tt.want {
				t.Errorf("received %q, want %q", got, tt.want)
			}
		})
	}
}

// Outside systemd there is no socket, and that is a normal state rather than a
// failure: the same binary runs from a shell, in a container and under a test.
func TestNew_NoSocketIsNotAnError(t *testing.T) {
	n, err := sdnotify.New(env(nil), os.Getpid())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n != nil {
		t.Fatalf("New returned %v, want nil when NOTIFY_SOCKET is unset", n)
	}
	if got := n.WatchdogInterval(); got != 0 {
		t.Errorf("WatchdogInterval on a nil Notifier = %v, want 0", got)
	}

	// And the nil notifier has to be usable, or every caller needs a guard.
	if err := n.Ready(); err != nil {
		t.Errorf("Ready on a nil Notifier: %v", err)
	}
	if err := n.Watchdog(); err != nil {
		t.Errorf("Watchdog on a nil Notifier: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Errorf("Close on a nil Notifier: %v", err)
	}
}

func TestNew_UnreachableSocketIsReported(t *testing.T) {
	_, err := sdnotify.New(env(map[string]string{"NOTIFY_SOCKET": "/nonexistent/sdnotify.sock"}), os.Getpid())

	var sockErr *types.NotifySocketError
	if !errors.As(err, &sockErr) {
		t.Fatalf("New error = %v, want *types.NotifySocketError", err)
	}
	if sockErr.Addr != "/nonexistent/sdnotify.sock" {
		t.Errorf("Addr = %q, want the address it tried", sockErr.Addr)
	}
}

// An address beginning with '@' names an abstract socket, whose real name
// begins with NUL. Passing the '@' through addresses a filesystem socket of
// that name, which does not exist -- so this failure looks like a missing file
// rather than a translation that was not done.
func TestNew_AbstractSocketNameIsTranslated(t *testing.T) {
	name := "\x00sysmon-test-" + strconv.Itoa(os.Getpid())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on abstract socket: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	n, err := sdnotify.New(env(map[string]string{"NOTIFY_SOCKET": "@" + name[1:]}), os.Getpid())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer n.Close() //nolint:errcheck

	if err := n.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64)
	got, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from abstract socket: %v", err)
	}
	if string(buf[:got]) != "READY=1" {
		t.Errorf("received %q, want READY=1", string(buf[:got]))
	}
}

func TestWatchdogInterval(t *testing.T) {
	const pid = 4242
	l := newListener(t)

	tests := []struct {
		name string
		env  map[string]string
		want time.Duration
	}{
		{
			// systemd asks for a ping at half the timeout. Pinging at the
			// deadline leaves nothing for scheduling jitter, and on a loaded
			// host that restarts a healthy service.
			name: "half the timeout",
			env:  map[string]string{"WATCHDOG_USEC": "30000000"},
			want: 15 * time.Second,
		},
		{
			name: "addressed to this process",
			env:  map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": strconv.Itoa(pid)},
			want: 15 * time.Second,
		},
		{
			// Inherited from a parent rather than addressed here. Honouring it
			// would have this process answering for another's liveness.
			name: "addressed to another process",
			env:  map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": "1"},
			want: 0,
		},
		{"no watchdog requested", map[string]string{}, 0},
		{"unparseable", map[string]string{"WATCHDOG_USEC": "soon"}, 0},
		{"zero", map[string]string{"WATCHDOG_USEC": "0"}, 0},
		{"negative", map[string]string{"WATCHDOG_USEC": "-1"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := map[string]string{"NOTIFY_SOCKET": l.addr}
			for k, v := range tt.env {
				e[k] = v
			}
			n, err := sdnotify.New(env(e), pid)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = n.Close() })

			if got := n.WatchdogInterval(); got != tt.want {
				t.Errorf("WatchdogInterval = %v, want %v", got, tt.want)
			}
		})
	}
}

// The notification variables address one process. A child that inherits them
// can satisfy a watchdog its parent has stopped satisfying, which is the
// failure the watchdog exists to catch, reintroduced by the watchdog itself.
func TestUnsetenv_RemovesEveryVariable(t *testing.T) {
	vars := []string{"NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID"}
	for _, k := range vars {
		t.Setenv(k, "set")
	}

	if err := sdnotify.Unsetenv(); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	for _, k := range vars {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is still set to %q", k, v)
		}
	}
}

// The interval has to survive the caller clearing the environment, because
// clearing it is the documented next step. Reading it afterwards yields zero
// and disables the watchdog in silence -- which is what the first version did,
// and what the end-to-end test caught as a watchdog timeout with no ping ever
// sent.
func TestNew_IntervalSurvivesUnsetenv(t *testing.T) {
	l := newListener(t)
	t.Setenv("NOTIFY_SOCKET", l.addr)
	t.Setenv("WATCHDOG_USEC", "30000000")

	n, err := sdnotify.New(os.Getenv, os.Getpid())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })

	if err := sdnotify.Unsetenv(); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	if got := n.WatchdogInterval(); got != 15*time.Second {
		t.Fatalf("WatchdogInterval after Unsetenv = %v, want 15s", got)
	}
	// And the socket still works, because it was dialled rather than named.
	if err := n.Watchdog(); err != nil {
		t.Fatalf("Watchdog after Unsetenv: %v", err)
	}
	if got := l.recv(t); got != "WATCHDOG=1" {
		t.Errorf("received %q, want WATCHDOG=1", got)
	}
}
