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
	"testing"
	"time"

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
	go func() {
		if err := srv.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return l.Addr().String()
}

// waitForFirstSnapshot blocks until m has collected once, so a request cannot
// race the poll loop and get a 503 instead of the body under test.
func waitForFirstSnapshot(t *testing.T, m *monitor.Monitor) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Snapshot(); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("monitor never produced a snapshot")
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
	addr := serveOnEphemeralPort(t, New(m, "127.0.0.1:0", nil))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if _, err := io.WriteString(conn, "GET /api/snapshot HTTP/1.1\r\nHost: sysmon\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read nothing at all until well past the deadline. This is the whole
	// fixture: the peer is present at the TCP level and consumes nothing.
	time.Sleep(4 * responseWriteTimeout)

	// Now drain. With the deadline in place the server has already abandoned
	// the response and closed, so this ends promptly and short of the full
	// body. Without it the server is merely blocked, resumes the moment we
	// start reading, and delivers every byte.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, err := io.Copy(io.Discard, conn)
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connection still open %v after the write deadline: read %d of %d body bytes",
			4*responseWriteTimeout, n, len(body))
	}
	if n >= int64(len(body)) {
		t.Fatalf("server delivered the whole %d-byte body to a peer that read nothing until %v had passed; the write was never bounded",
			len(body), 4*responseWriteTimeout)
	}
}

// The stream is the response that must NOT be bounded in total. It resets its
// own deadline before every event, which replaces the server-wide one, so it
// outlives a deadline that would have killed an ordinary response several times
// over. A fixture shorter than the deadline would pass either way, so this one
// runs to four times it.
func TestResponseWriteDeadline_EventStreamOutlivesIt(t *testing.T) {
	originalWrite, originalKeepalive := responseWriteTimeout, keepaliveInterval
	responseWriteTimeout = 150 * time.Millisecond
	keepaliveInterval = 25 * time.Millisecond
	t.Cleanup(func() {
		responseWriteTimeout, keepaliveInterval = originalWrite, originalKeepalive
	})

	m := startedMonitor(t, &bigSnapshotter{snap: &types.Snapshot{
		Host: types.HostInfo{Hostname: "stream-host"},
	}}, 20*time.Millisecond)
	waitForFirstSnapshot(t, m)
	addr := serveOnEphemeralPort(t, New(m, "127.0.0.1:0", nil))

	watch := 4 * responseWriteTimeout
	ctx, cancel := context.WithTimeout(context.Background(), watch)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	start := time.Now()
	var last time.Duration
	var events int
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			events++
			last = time.Since(start)
		}
	}

	if events == 0 {
		t.Fatal("stream delivered no events at all")
	}
	// The last event has to land after the server-wide deadline would have
	// fired, or the test proves nothing about the exemption.
	if last <= responseWriteTimeout {
		t.Fatalf("stream stopped delivering after %v, at or before the %v response write deadline: the stream inherited it (%d events)",
			last, responseWriteTimeout, events)
	}
}
