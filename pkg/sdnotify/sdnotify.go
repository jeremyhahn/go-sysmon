// Package sdnotify speaks the systemd notification protocol.
//
// It is the transport half of the watchdog in issue #66: vm-host-2 served
// nothing for two days while systemd reported the unit active, because
// Type=exec means "the binary is still running" and says nothing about whether
// a request can be answered. A watchdog turns that into a question the service
// has to answer repeatedly.
//
// The protocol is a datagram of newline-separated key=value pairs sent to the
// unix socket named in NOTIFY_SOCKET. It is implemented here rather than taken
// from a dependency because it is a dozen lines, and because linking
// libsystemd would put a cgo dependency in a binary that is otherwise
// cross-buildable.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Notifier sends messages to the service manager. A nil *Notifier is valid and
// discards everything, which is the state outside systemd: running under a
// shell, in a container without notification, or in a test.
type Notifier struct {
	conn     *net.UnixConn
	interval time.Duration
}

// New connects to the socket named by NOTIFY_SOCKET and captures the watchdog
// interval in the same breath.
//
// getenv resolves environment variables and pid is this process's; pass
// os.Getenv and os.Getpid(). A nil Notifier and a nil error mean the manager is
// not asking to be notified, which is a normal state and not a failure.
//
// The interval is read HERE, and not by a separate call later, because the
// caller is expected to clear these variables from the environment so nothing
// it starts can answer for its liveness -- and reading the interval after that
// clearing yields zero, which disables the watchdog silently. That is not a
// hypothetical: it is what the first version of this did, and the end-to-end
// test caught it as "Watchdog timeout (limit 10s)!" with no ping ever sent.
// Capturing both together makes the ordering impossible to get wrong.
func New(getenv func(string) string, pid int) (*Notifier, error) {
	addr := getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil, nil
	}

	// A leading '@' names an abstract socket, whose address begins with NUL.
	// Passing the '@' through unchanged addresses a filesystem socket of that
	// name instead, which does not exist -- so every notification would fail
	// with a misleading "no such file" on a host using abstract sockets.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return nil, &types.NotifySocketError{Addr: addr, Cause: err}
	}
	return &Notifier{conn: conn, interval: watchdogInterval(getenv, pid)}, nil
}

// WatchdogInterval is how often Watchdog must be called, or zero when no
// watchdog was requested. A nil Notifier reports zero.
func (n *Notifier) WatchdogInterval() time.Duration {
	if n == nil {
		return 0
	}
	return n.interval
}

// Ready tells the manager the service has finished starting.
func (n *Notifier) Ready() error { return n.send("READY=1") }

// Watchdog tells the manager the service is still doing its job. What that
// means is the caller's to decide and is the whole difficulty: a ping sent
// from a bare ticker proves only that a ticker fires.
func (n *Notifier) Watchdog() error { return n.send("WATCHDOG=1") }

// Status sets the one-line description shown by systemctl status.
func (n *Notifier) Status(text string) error { return n.send("STATUS=" + text) }

// Close releases the socket. A nil Notifier closes successfully.
func (n *Notifier) Close() error {
	if n == nil || n.conn == nil {
		return nil
	}
	return n.conn.Close()
}

// send writes one datagram. A nil Notifier discards it.
func (n *Notifier) send(msg string) error {
	if n == nil || n.conn == nil {
		return nil
	}
	if _, err := n.conn.Write([]byte(msg)); err != nil {
		return &types.NotifySendError{Message: msg, Cause: err}
	}
	return nil
}

// watchdogInterval derives the ping period from the WATCHDOG_USEC and
// WATCHDOG_PID variables the manager sets.
//
// The returned interval is HALF the timeout systemd will enforce, which is what
// systemd's own documentation asks for: pinging at exactly the deadline leaves
// no room for scheduling jitter, and on a loaded host that is a restart of a
// healthy service.
//
// A zero interval means no watchdog was requested. WATCHDOG_PID naming another
// process means the variables were inherited rather than addressed to us, and
// also yields zero -- honouring them would have this process answering for a
// parent's liveness.
func watchdogInterval(getenv func(string) string, pid int) time.Duration {
	if want := getenv("WATCHDOG_PID"); want != "" && want != strconv.Itoa(pid) {
		return 0
	}

	usec, err := strconv.ParseInt(getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	return time.Duration(usec) * time.Microsecond / 2
}

// Unsetenv removes the notification variables from the environment so they are
// not inherited by anything this process starts.
//
// systemd's own convention: the variables address one process, and a child that
// inherits them can satisfy a watchdog its parent has stopped satisfying.
func Unsetenv() error {
	for _, k := range []string{"NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID"} {
		if err := os.Unsetenv(k); err != nil {
			return err
		}
	}
	return nil
}
