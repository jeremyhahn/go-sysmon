package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// stubSnap is a minimal monitor.Snapshotter for internal tests.
type stubSnap struct{}

func (stubSnap) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Host: types.HostInfo{Hostname: "internal-test"}}, nil
}

// testMonitor returns a started monitor with a fast poll interval, stopped
// automatically when the test finishes.
func testMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(stubSnap{}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		// Tests that exercise shutdown stop the monitor themselves, so a
		// "not running" error here is expected rather than a failure.
		var notRunning *types.MonitorNotRunningError
		if err := m.Stop(); err != nil && !errors.As(err, &notRunning) {
			t.Errorf("monitor.Stop: %v", err)
		}
	})
	return m
}

// serverConn spins up a websocket endpoint and returns the server-side
// connection, so tests can drive the unexported write helpers directly.
func serverConn(t *testing.T) *websocket.Conn {
	t.Helper()

	connCh := make(chan *websocket.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		connCh <- c
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/"
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() }) //nolint:errcheck

	select {
	case c := <-connCh:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("server connection never arrived")
		return nil
	}
}

// TestWriteJSON_Success is the control case: a healthy connection encodes and
// flushes without error.
func TestWriteJSON_Success(t *testing.T) {
	conn := serverConn(t)
	if err := writeJSON(conn, map[string]string{"hello": "world"}); err != nil {
		t.Errorf("writeJSON() = %v, want nil", err)
	}
}

// TestWriteJSON_EncodeError covers the branch where the value cannot be
// marshalled. The writer must still be closed and the encode error returned,
// not the close error.
func TestWriteJSON_EncodeError(t *testing.T) {
	conn := serverConn(t)

	// A channel is not representable in JSON.
	err := writeJSON(conn, make(chan int))
	if err == nil {
		t.Fatal("writeJSON() = nil, want an encoding error")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("writeJSON() error = %v, want an unsupported-type error", err)
	}
}

// TestWriteJSON_DeadConnection covers the failure path taken when the peer has
// gone away before the write begins.
func TestWriteJSON_DeadConnection(t *testing.T) {
	conn := serverConn(t)

	if err := conn.UnderlyingConn().Close(); err != nil {
		t.Fatalf("close underlying conn: %v", err)
	}

	if err := writeJSON(conn, map[string]string{"hello": "world"}); err == nil {
		t.Error("writeJSON() = nil, want an error writing to a closed connection")
	}
}

// TestWriteClose_DeadConnection verifies writeClose swallows the failure when
// the peer is already gone; it must never panic on a shutdown path.
func TestWriteClose_DeadConnection(t *testing.T) {
	conn := serverConn(t)

	if err := conn.UnderlyingConn().Close(); err != nil {
		t.Fatalf("close underlying conn: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("writeClose panicked on a dead connection: %v", r)
		}
	}()
	writeClose(conn)
}

// TestHandleWebSocket_MalformedCommand covers the read pump's parse-failure
// branch: a client that sends non-JSON must be ignored, not disconnected.
func TestHandleWebSocket_MalformedCommand(t *testing.T) {
	m := testMonitor(t)
	srv := New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
		t.Fatalf("write malformed message: %v", err)
	}

	// The connection must survive and keep streaming snapshots.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Errorf("connection died after a malformed command: %v", err)
	}
}

// TestHandleWebSocket_PongExtendsDeadline drives the pong handler installed on
// the server connection, which resets the read deadline for live clients.
func TestHandleWebSocket_PongExtendsDeadline(t *testing.T) {
	m := testMonitor(t)
	srv := New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// An unsolicited pong is legal and must be accepted by the server.
	if err := conn.WriteMessage(websocket.PongMessage, nil); err != nil {
		t.Fatalf("write pong: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Errorf("connection died after a pong: %v", err)
	}
}

// TestHandleWebSocket_MonitorStopClosesStream covers the branch where the
// subscriber channel closes while a client is connected.
func TestHandleWebSocket_MonitorStopClosesStream(t *testing.T) {
	m := testMonitor(t)
	srv := New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read first snapshot: %v", err)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			return
		}
		t.Fatalf("read error = %v, want a normal closure", err)
	}
}

// TestWriteJSON_AfterCloseSent covers the NextWriter failure branch: once a
// close frame has been sent, gorilla refuses to open a new writer.
func TestWriteJSON_AfterCloseSent(t *testing.T) {
	conn := serverConn(t)

	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done")
	if err := conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("write close control: %v", err)
	}

	err := writeJSON(conn, map[string]string{"hello": "world"})
	if err == nil {
		t.Fatal("writeJSON() = nil, want an error after a close frame was sent")
	}
	if !errors.Is(err, websocket.ErrCloseSent) {
		t.Errorf("writeJSON() error = %v, want websocket.ErrCloseSent", err)
	}
}

// TestHandleWebSocket_SendsPing covers the keepalive branch by shortening the
// ping interval for the duration of the test.
func TestHandleWebSocket_SendsPing(t *testing.T) {
	original := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = original })

	m := testMonitor(t)
	srv := New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	pinged := make(chan struct{}, 1)
	conn.SetPingHandler(func(string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return nil
	})

	// Reads drive the handlers, so keep reading until a ping arrives.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-pinged:
	case <-time.After(3 * time.Second):
		t.Error("server never sent a ping frame")
	}

	conn.Close() //nolint:errcheck
	<-done
}
