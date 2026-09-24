// Package deadpeer gives tests a peer address that refuses every connection
// and that no other socket can take while the test runs.
//
// The obvious way to find an address nobody answers on -- listen on port 0,
// read the port, close the listener -- leaves the port free for the rest of
// the test, and any other process on the host may bind it in that time: the
// next package of a parallel `go test ./...`, or another copy of the same
// test. A test server that lands there answers as the "dead" peer, and the
// test fails for a reason that has nothing to do with the code under test
// (#222). Closing a live test server to make it dead has the same gap.
//
// The helpers here hold the port instead. A TCP socket that is bound but has
// never called listen(2) owns its port -- a bind(2) to the same port fails
// with EADDRINUSE -- but it is not in the kernel's listening table, so a
// connect(2) to it is answered with a reset: ECONNREFUSED, at once, exactly as
// for a port nothing holds. The socket is closed when the test ends.
//
// This is Linux behaviour, which is all this module supports; the package's
// tests assert both halves of it.
package deadpeer

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// loopback is the address every reservation is made on: tests reach their
// peers over IPv4 loopback.
var loopback = netip.MustParseAddr("127.0.0.1")

// ReserveError reports a failed step of reserving or joining a port. Op names
// the step (socket, setsockopt, bind, getsockname, close, listen) and Err is
// the underlying error.
type ReserveError struct {
	Op  string
	Err error
}

func (e *ReserveError) Error() string {
	return "deadpeer: " + e.Op + ": " + e.Err.Error()
}

func (e *ReserveError) Unwrap() error { return e.Err }

// reservation is a TCP socket bound to a port and never listening.
type reservation struct {
	fd   int
	addr netip.AddrPort
}

// reserve binds a new TCP socket to ip on a port the kernel chooses and
// returns it without listening. shared sets SO_REUSEPORT, which lets a
// listener of the same user join the port while the reservation holds it;
// without it no other socket can bind the port at all. SO_REUSEADDR is never
// set: with it, any SO_REUSEADDR listener -- which is every Go listener --
// could bind a port whose only holder is not listening.
func reserve(ip netip.Addr, shared bool) (*reservation, error) {
	if !ip.Is4() {
		return nil, &ReserveError{Op: "bind", Err: &net.AddrError{Err: "not an IPv4 address", Addr: ip.String()}}
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, &ReserveError{Op: "socket", Err: os.NewSyscallError("socket", err)}
	}
	r := &reservation{fd: fd}
	if shared {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			return nil, r.abandon("setsockopt", err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: ip.As4()}); err != nil {
		return nil, r.abandon("bind", err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return nil, r.abandon("getsockname", err)
	}
	// An AF_INET socket always names itself with a SockaddrInet4.
	bound := sa.(*unix.SockaddrInet4)
	r.addr = netip.AddrPortFrom(netip.AddrFrom4(bound.Addr), uint16(bound.Port))
	return r, nil
}

// abandon closes a reservation whose op step failed with err, and returns the
// failure -- with the close's own, if closing failed too.
func (r *reservation) abandon(op string, err error) error {
	err = os.NewSyscallError(op, err)
	if closeErr := r.close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return &ReserveError{Op: op, Err: err}
}

// close releases the port.
func (r *reservation) close() error {
	if err := unix.Close(r.fd); err != nil {
		return &ReserveError{Op: "close", Err: os.NewSyscallError("close", err)}
	}
	return nil
}

// hold reserves a port on ip for the rest of t, failing t if it cannot.
func hold(t testing.TB, ip netip.Addr, shared bool) *reservation {
	t.Helper()
	r, err := reserve(ip, shared)
	if err != nil {
		t.Fatalf("reserve a dead peer port: %v", err)
		return nil
	}
	t.Cleanup(func() {
		if err := r.close(); err != nil {
			t.Errorf("release the dead peer port: %v", err)
		}
	})
	return r
}

// Addr returns a loopback host:port that refuses every connection and that no
// other socket can bind until t ends.
func Addr(t testing.TB) string {
	t.Helper()
	return hold(t, loopback, false).addr.String()
}

// URL returns Addr as an http:// URL: a peer that is down.
func URL(t testing.TB) string {
	t.Helper()
	return "http://" + Addr(t)
}

// Server starts an httptest.Server for h whose port stays held after the
// server is closed, so a test can take a live peer down with ts.Close() and
// have its address refuse connections -- never be answered by whatever binds
// the port next -- for the rest of t. The server is closed when t ends, if
// the test has not closed it already.
func Server(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	r := hold(t, loopback, true)
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return reusePort(c)
	}}
	l, err := lc.Listen(t.Context(), "tcp4", r.addr.String())
	if err != nil {
		t.Fatalf("listen on the held port: %v", &ReserveError{Op: "listen", Err: err})
	}
	ts := httptest.NewUnstartedServer(h)
	if err := ts.Listener.Close(); err != nil {
		t.Fatalf("close httptest's own listener: %v", err)
	}
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// reusePort sets SO_REUSEPORT on c, which a listener needs to share a port
// with the reservation holding it.
func reusePort(c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return &ReserveError{Op: "setsockopt", Err: err}
	}
	if sockErr != nil {
		return &ReserveError{Op: "setsockopt", Err: os.NewSyscallError("setsockopt", sockErr)}
	}
	return nil
}
