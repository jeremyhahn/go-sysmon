//go:build integration

package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// event is one server-sent event.
type event struct {
	name string
	data string
}

// stream is an open /api/events response.
type stream struct {
	header http.Header
	reader *bufio.Reader
	// retry is the reconnect delay the server advertised, as written.
	retry string
}

// streamOptions say how to open a stream.
type streamOptions struct {
	// gzip offers compression.
	gzip bool
	// client, when set, carries the request: the stream is over TLS, or
	// through a proxy.
	client *http.Client
	// rcvbuf, when set, shrinks the socket's receive buffer, so a client that
	// stops reading stalls the server's writes within a few frames.
	rcvbuf int
}

// openStream opens url as an event stream. The response is closed when the
// test ends.
//
// The request goes through a Transport rather than an http.Client with a
// timeout, because a client timeout covers the body too, and would cut a
// stream that is doing exactly what it should.
func openStream(t *testing.T, url string, o streamOptions) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if o.gzip {
		req.Header.Set("Accept-Encoding", "gzip")
	} else {
		req.Header.Set("Accept-Encoding", "identity")
	}

	rt := http.DefaultTransport
	switch {
	case o.client != nil:
		rt = o.client.Transport
	case o.rcvbuf > 0:
		rt = &http.Transport{DialContext: (&net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, o.rcvbuf)
			}); err != nil {
				return err
			}
			return sockErr
		}}).DialContext, DisableCompression: true}
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() {
		cancel()
		if err := resp.Body.Close(); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("close the %s stream: %v", url, err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}

	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatalf("gzip stream: %v", err)
		}
		body = gz
	}
	return &stream{header: resp.Header, reader: bufio.NewReaderSize(body, 1<<20)}
}

// read returns the next event, skipping comments. A retry field is recorded
// rather than returned. It returns io.EOF when the server ends the stream.
func (s *stream) read() (event, error) {
	var ev event
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if ev.name == "" && len(data) == 0 {
				continue
			}
			ev.data = strings.Join(data, "\n")
			return ev, nil
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.name = value
		case "data":
			data = append(data, value)
		case "retry":
			s.retry = value
		}
	}
}

// readResult is one read's outcome.
type readResult struct {
	ev  event
	err error
}

// next returns the next event, failing the test if the stream ends or none
// arrives within await's bound.
func (s *stream) next(t *testing.T) event {
	t.Helper()
	r := s.nextResult(t)
	if r.err != nil {
		t.Fatalf("reading the event stream: %v", r.err)
	}
	return r.ev
}

// nextResult is next, returning how the read ended instead of failing on it.
func (s *stream) nextResult(t *testing.T) readResult {
	t.Helper()
	ch := make(chan readResult, 1)
	go func() {
		ev, err := s.read()
		ch <- readResult{ev: ev, err: err}
	}()
	return await.Recv(t, "the next event", ch)
}
