package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// errListenerClose is the fault closeFailingListener injects.
var errListenerClose = errors.New("injected listener close failure")

// closeFailingListener is a bound listener whose Close fails with closeErr.
type closeFailingListener struct {
	net.Listener
	closeErr error
}

func (l *closeFailingListener) Close() error {
	if err := l.Listener.Close(); err != nil {
		return err
	}
	return l.closeErr
}

// captureDefaultLog routes the default logger, at debug level, into a buffer
// for the rest of the test. The handlers log through slog's default because
// they have no logger of their own; the test is sequential, so the swap cannot
// reach another test.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// stopWithListener stops a never-started server holding l as its bound
// listener, and returns Stop's error.
func stopWithListener(t *testing.T, l net.Listener) error {
	t.Helper()
	m, err := monitor.New(idleSnapshotter{}, time.Hour)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	srv := New(m, "127.0.0.1:0", nil)
	srv.ln.Store(&l)
	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()
	return srv.Stop(ctx)
}

// TestStop_ReportsAListenerThatWouldNotClose verifies that a listener Stop
// could not release is returned as a typed error naming the address: the port
// may still be held, which is the defect #106 was about.
func TestStop_ReportsAListenerThatWouldNotClose(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := inner.Addr().String()

	err = stopWithListener(t, &closeFailingListener{Listener: inner, closeErr: errListenerClose})

	var closeErr *types.ListenerCloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("Stop() = %v, want a *types.ListenerCloseError", err)
	}
	if closeErr.Addr != addr {
		t.Errorf("ListenerCloseError.Addr = %q, want %q", closeErr.Addr, addr)
	}
	if !errors.Is(err, errListenerClose) {
		t.Errorf("Stop() = %v, want it to wrap the close failure", err)
	}
}

// TestStop_AlreadyClosedListenerIsNotAnError verifies the normal path stays
// quiet: http.Server has usually closed the listener already, and net.ErrClosed
// from the second close is expected, not a failure to stop.
func TestStop_AlreadyClosedListenerIsNotAnError(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := inner.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := stopWithListener(t, inner); err != nil {
		t.Errorf("Stop() = %v, want nil for an already-closed listener", err)
	}
}

// TestHandleStatus_LogsAFailedWrite verifies the status page's body write
// failure is logged against the client rather than dropped.
func TestHandleStatus_LogsAFailedWrite(t *testing.T) {
	logs := captureDefaultLog(t)
	w := &failingWriter{err: errors.New("broken pipe")}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.7:4242"

	(&Server{}).handleStatus(w, r)

	out := logs.String()
	if !strings.Contains(out, "status page write") || !strings.Contains(out, "broken pipe") || !strings.Contains(out, "192.0.2.7:4242") {
		t.Errorf("log %q does not report the failed write against the client", out)
	}
}

// TestHandleEvents_LogsAFailedStreamClose verifies that the gzip trailer a
// dead client could not receive is logged when the stream closes. The first
// write fails, so the handler returns at once and the deferred Close reports
// the same broken connection.
func TestHandleEvents_LogsAFailedStreamClose(t *testing.T) {
	logs := captureDefaultLog(t)
	m := startedMonitor(t, idleSnapshotter{}, time.Hour)
	srv := New(m, ":0", nil)

	w := &failingWriter{err: errors.New("connection reset by peer")}
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.RemoteAddr = "192.0.2.9:5151"

	srv.handleEvents(w, r)

	out := logs.String()
	if !strings.Contains(out, "event stream close") || !strings.Contains(out, "connection reset by peer") || !strings.Contains(out, "192.0.2.9:5151") {
		t.Errorf("log %q does not report the failed stream close against the client", out)
	}
}
