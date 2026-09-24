//go:build integration

package integration

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// sseEvent is a single decoded server-sent event.
type sseEvent struct {
	name string
	data string
}

// eventStream reads server-sent events from the running sysmon binary,
// transparently decoding gzip when the server chose to compress.
type eventStream struct {
	contentType string
	encoding    string

	resp   *http.Response
	reader *bufio.Reader
	cancel context.CancelFunc
}

// dialEventStream opens GET url as an event stream. acceptGzip controls
// whether the request advertises gzip support.
//
// The request goes through a RoundTripper rather than an http.Client because a
// client timeout covers the response body too, which would cut a stream that
// is behaving exactly as intended.
func dialEventStream(t *testing.T, url string, acceptGzip bool) *eventStream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if acceptGzip {
		req.Header.Set("Accept-Encoding", "gzip")
	} else {
		req.Header.Set("Accept-Encoding", "identity")
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the refused %s response: %v", url, err)
		}
		cancel()
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}

	s := &eventStream{
		contentType: resp.Header.Get("Content-Type"),
		encoding:    resp.Header.Get("Content-Encoding"),
		resp:        resp,
		cancel:      cancel,
	}

	var body io.Reader = resp.Body
	if s.encoding == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Errorf("closing the %s response: %v", url, closeErr)
			}
			cancel()
			t.Fatalf("gzip reader: %v", err)
		}
		body = gz
	}

	s.reader = bufio.NewReader(body)

	t.Cleanup(func() {
		cancel()
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the %s event stream: %v", url, err)
		}
	})

	return s
}

// next reads the next complete event, skipping comment frames.
func (s *eventStream) next() (sseEvent, error) {
	var ev sseEvent
	var data []string

	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			if ev.name == "" && len(data) == 0 {
				continue // comment or stray blank line
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
		}
	}
}

// nextNamed reads events until one with the given name arrives, failing the
// test if the stream ends first or none arrives within await.Timeout.
func (s *eventStream) nextNamed(t *testing.T, name string) sseEvent {
	t.Helper()

	type result struct {
		ev  sseEvent
		err error
	}
	ch := make(chan result, 1)

	go func() {
		for {
			ev, err := s.next()
			if err != nil {
				ch <- result{err: err}
				return
			}
			if ev.name == name {
				ch <- result{ev: ev}
				return
			}
		}
	}()

	r := await.Recv(t, fmt.Sprintf("a %q event", name), ch)
	if r.err != nil {
		t.Fatalf("waiting for %q event: %v", name, r.err)
	}
	return r.ev
}
