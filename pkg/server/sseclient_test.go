package server_test

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// event is a single decoded server-sent event.
type event struct {
	// Name is the value of the event field, empty for a bare data event.
	Name string
	// Data is the concatenation of the event's data fields.
	Data string
	// Retry is the value of the retry field, or -1 when absent.
	Retry int
	// Comment is the text of a comment frame, empty otherwise.
	Comment string
}

// sseClient reads an event stream from the server, transparently decoding gzip
// when the server chose to compress.
type sseClient struct {
	resp   *http.Response
	body   io.ReadCloser
	reader *bufio.Reader
	cancel context.CancelFunc
	// t reports a failure to close the stream.
	t *testing.T
}

// dialEvents opens an event stream at url over the default transport.
func dialEvents(t *testing.T, url string, acceptGzip bool) *sseClient {
	t.Helper()
	return dialEventsVia(t, http.DefaultTransport, url, acceptGzip)
}

// dialEventsVia opens an event stream at url using rt. acceptGzip controls
// whether the request advertises gzip support, which is what makes the server
// compress.
//
// A RoundTripper is used rather than an http.Client because a client Timeout
// applies to the whole exchange including the body, which would cut a stream
// that is behaving exactly as intended.
func dialEventsVia(t *testing.T, rt http.RoundTripper, url string, acceptGzip bool) *sseClient {
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
		// An explicit identity keeps the transport from adding gzip itself.
		req.Header.Set("Accept-Encoding", "identity")
	}

	// Because the request carries its own Accept-Encoding, the transport does
	// not transparently decompress; this client handles it below.
	resp, err := rt.RoundTrip(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", url, err)
	}

	c := &sseClient{resp: resp, body: resp.Body, cancel: cancel, t: t}

	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			cancel()
			t.Fatalf("gzip reader: %v", err)
		}
		c.body = gz
	}

	c.reader = bufio.NewReader(c.body)
	t.Cleanup(c.Close)
	return c
}

// Close tears down the stream and the underlying request.
// It is safe to call more than once.
func (c *sseClient) Close() {
	c.cancel()
	if err := c.resp.Body.Close(); err != nil {
		c.t.Errorf("closing the event stream: %v", err)
	}
}

// next reads the next complete event, blocking until one arrives or the read
// fails. Comment frames are returned as events with Comment set.
func (c *sseClient) next() (event, error) {
	var ev event
	ev.Retry = -1

	var data []string

	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")

		// A blank line dispatches the accumulated event.
		if line == "" {
			if ev.Name == "" && len(data) == 0 && ev.Retry < 0 && ev.Comment == "" {
				continue // stray blank line
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}

		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "":
			// A line beginning with a colon is a comment.
			ev.Comment = value
		case "event":
			ev.Name = value
		case "data":
			data = append(data, value)
		case "retry":
			n := 0
			for _, r := range value {
				if r < '0' || r > '9' {
					n = -1
					break
				}
				n = n*10 + int(r-'0')
			}
			ev.Retry = n
		}
	}
}

// nextNamed reads events until one with the given name arrives, skipping
// comments and other event types. It fails the test if the stream ends first
// or no such event arrives within await.Timeout.
func (c *sseClient) nextNamed(t *testing.T, name string) event {
	t.Helper()
	return c.awaitEvent(t, "a "+strconv.Quote(name)+" event", func(ev event) bool { return ev.Name == name })
}

// nextEvent reads the next event of any kind, comments included. It fails the
// test if the stream ends or nothing arrives within await.Timeout.
func (c *sseClient) nextEvent(t *testing.T) event {
	t.Helper()
	return c.awaitEvent(t, "the next event", func(event) bool { return true })
}

// awaitEvent reads events until match accepts one and returns it. The read
// runs on its own goroutine because it blocks, so the wait can be bounded;
// what names the awaited event in a failure.
func (c *sseClient) awaitEvent(t *testing.T, what string, match func(event) bool) event {
	t.Helper()

	type result struct {
		ev  event
		err error
	}
	ch := make(chan result, 1)

	go func() {
		for {
			ev, err := c.next()
			if err != nil {
				ch <- result{err: err}
				return
			}
			if match(ev) {
				ch <- result{ev: ev}
				return
			}
		}
	}()

	r := await.Recv(t, what, ch)
	if r.err != nil {
		t.Fatalf("waiting for %s: %v", what, r.err)
	}
	return r.ev
}

// decodeSnapshot unmarshals an event's data payload into v.
func decodeSnapshot(t *testing.T, ev event, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(ev.Data), v); err != nil {
		t.Fatalf("unmarshal %q event data: %v (data=%q)", ev.Name, err, ev.Data)
	}
}
