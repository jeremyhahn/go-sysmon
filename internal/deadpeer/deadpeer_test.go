package deadpeer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// assertRefused dials addr and requires the connection to be refused -- not
// accepted, and not left to time out.
func assertRefused(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err == nil {
		t.Errorf("dial %s connected; want the connection refused", addr)
		if err := conn.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		return
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("dial %s = %v, want ECONNREFUSED", addr, err)
	}
}

// assertInUse requires a Go listener -- which sets SO_REUSEADDR, as every
// test server here does -- to be unable to bind addr.
func assertInUse(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err == nil {
		t.Errorf("listen on %s succeeded; want the port held", addr)
		if err := l.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		return
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("listen on %s = %v, want EADDRINUSE", addr, err)
	}
}

// bindRaw binds a new TCP socket with the given SO_REUSEADDR and SO_REUSEPORT
// settings to addr and returns the bind error, closing the socket either way.
func bindRaw(t *testing.T, addr netip.AddrPort, reuseAddr, reusePort bool) error {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer func() {
		if err := unix.Close(fd); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	for opt, on := range map[int]bool{unix.SO_REUSEADDR: reuseAddr, unix.SO_REUSEPORT: reusePort} {
		if on {
			if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt, 1); err != nil {
				t.Fatalf("setsockopt %d: %v", opt, err)
			}
		}
	}
	return unix.Bind(fd, &unix.SockaddrInet4{Addr: addr.Addr().As4(), Port: int(addr.Port())})
}

func TestAddr_RefusesConnections(t *testing.T) {
	assertRefused(t, Addr(t))
}

// TestAddr_NoOtherSocketCanBindIt is the half the open-then-close helpers it
// replaces lacked: the port is not free for anyone else while the test runs,
// on the same address, on the wildcard address, or with either reuse option.
func TestAddr_NoOtherSocketCanBindIt(t *testing.T) {
	addr := netip.MustParseAddrPort(Addr(t))

	assertInUse(t, addr.String())
	assertInUse(t, netip.AddrPortFrom(netip.IPv4Unspecified(), addr.Port()).String())
	for _, c := range []struct {
		name                 string
		reuseAddr, reusePort bool
	}{
		{"plain", false, false},
		{"SO_REUSEADDR", true, false},
		{"SO_REUSEPORT", false, true},
		{"both", true, true},
	} {
		if err := bindRaw(t, addr, c.reuseAddr, c.reusePort); !errors.Is(err, unix.EADDRINUSE) {
			t.Errorf("bind %s with %s = %v, want EADDRINUSE", addr, c.name, err)
		}
	}
}

// TestHold_ReleasedWhenTheTestEnds: the port is held for the test that asked
// for it and no longer. It checks that the holding socket is gone from this
// process, by its inode, rather than binding the port again: under a parallel
// run another process may take a released port at once, which is the point.
func TestHold_ReleasedWhenTheTestEnds(t *testing.T) {
	var socket string
	t.Run("holder", func(t *testing.T) {
		r := hold(t, loopback, false)
		var st unix.Stat_t
		if err := unix.Fstat(r.fd, &st); err != nil {
			t.Fatalf("fstat: %v", err)
		}
		socket = "socket:[" + strconv.FormatUint(st.Ino, 10) + "]"
		if !holdsSocket(t, socket) {
			t.Fatalf("%s is not among this process's descriptors while held", socket)
		}
	})
	if holdsSocket(t, socket) {
		t.Errorf("%s is still open after its test ended", socket)
	}
}

// holdsSocket reports whether any of this process's descriptors is socket.
func holdsSocket(t *testing.T, socket string) bool {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read descriptors: %v", err)
	}
	for _, fd := range fds {
		// A descriptor closed between the listing and the read is not ours.
		if link, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && link == socket {
			return true
		}
	}
	return false
}

func TestURL_IsAnHTTPURLForARefusingAddress(t *testing.T) {
	u := URL(t)
	addr, ok := strings.CutPrefix(u, "http://")
	if !ok {
		t.Fatalf("URL() = %q, want an http:// URL", u)
	}
	assertRefused(t, addr)
}

// TestServer_RefusesOnceClosed: the server answers while it is up, and once
// it is closed its address refuses connections and cannot be bound.
func TestServer_RefusesOnceClosed(t *testing.T) {
	ts := Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "up"); err != nil {
			t.Errorf("write: %v", err)
		}
	}))

	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatalf("GET while up: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err := errors.Join(err, resp.Body.Close()); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "up" {
		t.Fatalf("GET while up = %q, want %q", body, "up")
	}

	ts.Close()
	addr := ts.Listener.Addr().String()
	assertRefused(t, addr)
	assertInUse(t, addr)
}

// TestServer_NoOtherListenerJoinsWhileUp: SO_REUSEPORT lets the server share
// the port with its reservation, but a listener without it -- every other Go
// listener -- still cannot bind the port.
func TestServer_NoOtherListenerJoinsWhileUp(t *testing.T) {
	ts := Server(t, http.NotFoundHandler())
	assertInUse(t, ts.Listener.Addr().String())
}

func TestReserve_RejectsAnAddressThatIsNotIPv4(t *testing.T) {
	_, err := reserve(netip.IPv6Loopback(), false)
	var re *ReserveError
	if !errors.As(err, &re) || re.Op != "bind" {
		t.Fatalf("reserve(::1) = %v, want a *ReserveError for bind", err)
	}
	var addrErr *net.AddrError
	if !errors.As(err, &addrErr) {
		t.Errorf("reserve(::1) = %v, want it to wrap a *net.AddrError", err)
	}
}

// TestReserve_FailsOnAnAddressNotOnThisHost drives a real bind failure: an
// address from TEST-NET-1 (RFC 5737) is never assigned to this host.
func TestReserve_FailsOnAnAddressNotOnThisHost(t *testing.T) {
	_, err := reserve(netip.MustParseAddr("192.0.2.1"), false)
	var re *ReserveError
	if !errors.As(err, &re) || re.Op != "bind" {
		t.Fatalf("reserve(192.0.2.1) = %v, want a *ReserveError for bind", err)
	}
	if !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Errorf("reserve(192.0.2.1) = %v, want it to wrap EADDRNOTAVAIL", err)
	}
	if !strings.HasPrefix(err.Error(), "deadpeer: bind: ") {
		t.Errorf("error = %q, want it to name the package and the step", err)
	}
}

// TestClose_ReportsAFailedClose: releasing a socket that is not open is an
// error, typed and naming the step.
func TestClose_ReportsAFailedClose(t *testing.T) {
	err := (&reservation{fd: -1}).close()
	var re *ReserveError
	if !errors.As(err, &re) || re.Op != "close" || !errors.Is(err, unix.EBADF) {
		t.Errorf("close of fd -1 = %v, want a *ReserveError for close wrapping EBADF", err)
	}
}

// TestAbandon_KeepsBothFailures: a reservation abandoned part-way whose close
// also fails reports the step that failed and the close, not just one.
func TestAbandon_KeepsBothFailures(t *testing.T) {
	err := (&reservation{fd: -1}).abandon("bind", unix.EADDRINUSE)
	var re *ReserveError
	if !errors.As(err, &re) || re.Op != "bind" {
		t.Fatalf("abandon = %v, want a *ReserveError for bind", err)
	}
	if !errors.Is(err, unix.EADDRINUSE) || !errors.Is(err, unix.EBADF) {
		t.Errorf("abandon = %v, want both the bind's EADDRINUSE and the close's EBADF", err)
	}
}

// rawConn is a syscall.RawConn whose Control either fails or hands f a
// descriptor that is not open.
type rawConn struct {
	syscall.RawConn
	controlErr error
}

func (c rawConn) Control(f func(fd uintptr)) error {
	if c.controlErr != nil {
		return c.controlErr
	}
	f(^uintptr(0)) // fd -1
	return nil
}

func TestReusePort_ReportsEachFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		conn rawConn
		want error
	}{
		{"control fails", rawConn{controlErr: unix.EINVAL}, unix.EINVAL},
		{"setsockopt fails", rawConn{}, unix.EBADF},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := reusePort(c.conn)
			var re *ReserveError
			if !errors.As(err, &re) || re.Op != "setsockopt" || !errors.Is(err, c.want) {
				t.Errorf("reusePort = %v, want a *ReserveError for setsockopt wrapping %v", err, c.want)
			}
		})
	}
}

// fatalRecorder is a testing.TB that records a Fatalf instead of ending the
// test, so the failure path can be asserted on.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

// TestHold_FailsTheTestWhenNoPortCanBeReserved: a reservation that cannot be
// made fails the test, saying which step failed, and holds nothing.
func TestHold_FailsTheTestWhenNoPortCanBeReserved(t *testing.T) {
	rec := &fatalRecorder{TB: t}
	if r := hold(rec, netip.MustParseAddr("192.0.2.1"), false); r != nil {
		t.Errorf("hold on an address not on this host = %+v, want nothing held", r)
	}
	if !strings.Contains(rec.fatal, "deadpeer: bind: ") {
		t.Errorf("failure = %q, want it to name the step that failed", rec.fatal)
	}
}
