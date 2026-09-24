package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// These tests exercise the configured *http.Server rather than a handler.
// Every other test in this package reaches the mux through httptest.NewServer,
// which builds an http.Server of its own and therefore carries none of the
// timeouts set in NewWithConfig -- so a response deadline asserted through
// httptest would be asserting httptest's configuration, not ours.

// bigSnapshotter serves one large snapshot. The size is the point: a response
// that fits inside the kernel's socket buffers completes even when the peer
// never reads a byte, so a small fixture would pass with or without a write
// deadline.
type bigSnapshotter struct{ snap *types.Snapshot }

func (b *bigSnapshotter) Snapshot() (*types.Snapshot, error) { return b.snap, nil }

// bigSnapshot builds a snapshot whose JSON encoding is several megabytes, using
// the field that is genuinely large in production: the per-process list.
func bigSnapshot() *types.Snapshot {
	const processes = 30000
	list := make([]types.ProcessDetail, processes)
	for i := range list {
		list[i] = types.ProcessDetail{
			PID:            int32(i + 1),
			Name:           fmt.Sprintf("process-%05d", i),
			Username:       "sysmon",
			CPUPercent:     1.5,
			MemoryBytes:    1 << 20,
			ReadBytesRate:  4096,
			WriteBytesRate: 4096,
			Priority:       20,
			Status:         "sleeping",
		}
	}
	return &types.Snapshot{
		Host:      types.HostInfo{Hostname: "deadline-host"},
		Processes: types.ProcessSummary{Total: processes, Sleeping: processes, Processes: list},
	}
}

// serveOnEphemeralPort runs srv's configured http.Server on a real listener and
// returns its address. It uses Serve rather than Start so the port is chosen by
// the kernel and known to the test without a reserve-then-rebind race.
func serveOnEphemeralPort(t *testing.T, srv *Server) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveOnListener(t, srv, l)
	return l.Addr().String()
}

// waitForFirstSnapshot blocks until m has collected once, so a request cannot
// race the poll loop and get a 503 instead of the body under test. It
// subscribes before asking, so a collection landing in between is still seen.
func waitForFirstSnapshot(t *testing.T, m *monitor.Monitor) {
	t.Helper()

	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	if _, err := m.Snapshot(); err == nil {
		return
	}
	await.Recv(t, "the monitor's first snapshot", sub.Ch)
}

// closeSignallingListener hands out connections that report when the server
// closes them, so a test can wait for the server to abandon a response instead
// of guessing how long that takes.
type closeSignallingListener struct {
	net.Listener
	closed chan struct{} // one signal per server-side connection closed
}

func (l *closeSignallingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &closeSignallingConn{Conn: c, closed: l.closed}, nil
}

// closeSignallingConn signals once, on its first Close.
type closeSignallingConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *closeSignallingConn) Close() error {
	c.once.Do(func() { c.closed <- struct{}{} })
	return c.Conn.Close()
}

// serveOnListener runs srv's configured http.Server on l until the test ends.
func serveOnListener(t *testing.T, srv *Server, l net.Listener) {
	t.Helper()

	go func() {
		if err := srv.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
}

// A peer that stops reading is the failure this deadline exists for: the write
// parks in TCP retransmission and nothing ever releases the goroutine or the
// connection. Measured on vm-host-2 on 2026-09-03, 44 goroutines were held
// that way for seven minutes.
func TestResponseWriteDeadline_AbortsAWriteToAPeerThatNeverReads(t *testing.T) {
	original := responseWriteTimeout
	responseWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { responseWriteTimeout = original })

	snap := bigSnapshot()
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	m := startedMonitor(t, &bigSnapshotter{snap: snap}, 20*time.Millisecond)
	waitForFirstSnapshot(t, m)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l := &closeSignallingListener{Listener: inner, closed: make(chan struct{}, 1)}
	serveOnListener(t, New(m, "127.0.0.1:0", nil), l)

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close client connection: %v", err)
		}
	})

	if _, err := io.WriteString(conn, "GET /api/snapshot HTTP/1.1\r\nHost: sysmon\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read nothing at all until the server has given up on the connection.
	// This is the whole fixture: the peer is present at the TCP level and
	// consumes nothing. With the deadline the server abandons the response
	// and closes its end; without it the write is parked for good and the
	// close never comes, so the wait below fails at its bound.
	await.Recv(t, "the server to close a connection whose peer never reads", l.closed)

	// Now drain. The server has already abandoned the response, so this ends
	// short of the full body. Were the write unbounded, draining would let it
	// resume and deliver every byte.
	if err := conn.SetReadDeadline(time.Now().Add(await.Timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, err := io.Copy(io.Discard, conn)
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connection still open after the server closed it: read %d of %d body bytes",
			n, len(body))
	}
	if n >= int64(len(body)) {
		t.Fatalf("server delivered the whole %d-byte body to a peer that read nothing until the server closed; the write was never bounded",
			len(body))
	}
}

// stampingSnapshotter names every collection it makes and records, on the
// monotonic clock, when it made it. A streamed snapshot then says when it was
// collected, and so a lower bound on when the server wrote it.
type stampingSnapshotter struct {
	mu        sync.Mutex
	collected map[string]time.Time
}

func newStampingSnapshotter() *stampingSnapshotter {
	return &stampingSnapshotter{collected: make(map[string]time.Time)}
}

func (s *stampingSnapshotter) Snapshot() (*types.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := fmt.Sprintf("collection-%d", len(s.collected)+1)
	s.collected[name] = time.Now()
	return &types.Snapshot{Host: types.HostInfo{Hostname: name}}, nil
}

// collectedAt reports when the collection named name was made.
func (s *stampingSnapshotter) collectedAt(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.collected[name]
	return at, ok
}

// The stream is the response that must NOT be bounded in total. It resets its
// own deadline before every event, which replaces the server-wide one, so it
// outlives a deadline that would have killed an ordinary response.
//
// The proof is one event the server can only have written after the
// server-wide deadline would have fired: a snapshot collected later than that.
// The server arms the deadline when it reads the request, which is before the
// response headers reach the client, so a collection made more than
// responseWriteTimeout after the headers arrived was written past it. Were the
// stream bounded, the server would close it at the deadline and the stream
// would end without ever carrying such a snapshot.
func TestResponseWriteDeadline_EventStreamOutlivesIt(t *testing.T) {
	originalWrite, originalKeepalive := responseWriteTimeout, keepaliveInterval
	responseWriteTimeout = 150 * time.Millisecond
	keepaliveInterval = 25 * time.Millisecond
	t.Cleanup(func() {
		responseWriteTimeout, keepaliveInterval = originalWrite, originalKeepalive
	})

	stamps := newStampingSnapshotter()
	m := startedMonitor(t, stamps, 20*time.Millisecond)
	waitForFirstSnapshot(t, m)
	addr := serveOnEphemeralPort(t, New(m, "127.0.0.1:0", nil))

	ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	}()
	pastDeadline := time.Now().Add(responseWriteTimeout)

	var events int
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		events++
		var snap types.Snapshot
		if err := json.Unmarshal([]byte(payload), &snap); err != nil {
			t.Fatalf("decode event %d: %v", events, err)
		}
		at, ok := stamps.collectedAt(snap.Host.Hostname)
		if !ok {
			t.Fatalf("event %d carries %q, which was never collected", events, snap.Host.Hostname)
		}
		if at.After(pastDeadline) {
			return
		}
	}

	t.Fatalf("stream ended (%v) after %d events without delivering a snapshot collected past the %v response write deadline: the stream inherited it",
		scanner.Err(), events, responseWriteTimeout)
}
