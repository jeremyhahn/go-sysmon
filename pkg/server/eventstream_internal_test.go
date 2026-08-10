package server

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- test doubles -------------------------------------------------------

// unflushableWriter is a ResponseWriter with no Flush method, which is what a
// middleware that wraps the writer without forwarding Flush looks like.
type unflushableWriter struct {
	header http.Header
	body   strings.Builder
	code   int
}

func (w *unflushableWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *unflushableWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *unflushableWriter) WriteHeader(code int)        { w.code = code }

// failingWriter flushes but fails every write, standing in for a peer that
// disappeared mid-stream.
type failingWriter struct {
	header http.Header
	err    error
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }
func (w *failingWriter) WriteHeader(int)           {}
func (w *failingWriter) Flush()                    {}

// ---- newEventStream -----------------------------------------------------

func TestNewEventStream_RejectsUnflushableWriter(t *testing.T) {
	w := &unflushableWriter{}
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	_, err := newEventStream(w, r)
	if err == nil {
		t.Fatal("newEventStream() = nil error, want a failure on a writer that cannot flush")
	}

	var streamErr *types.StreamUnsupportedError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %T (%v), want *types.StreamUnsupportedError", err, err)
	}
	if w.body.Len() != 0 {
		t.Errorf("body = %q, want nothing written before the error", w.body.String())
	}
}

func TestNewEventStream_IdentityWhenGzipNotOffered(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if s.gz != nil {
		t.Error("stream compressed without the client offering gzip")
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want it unset", got)
	}
}

func TestNewEventStream_GzipWhenOffered(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	r.Header.Set("Accept-Encoding", "gzip")

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}

	if s.gz == nil {
		t.Fatal("stream not compressed although the client offered gzip")
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}

	if err := s.send("snapshot", map[string]string{"host": "gz"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !strings.Contains(string(got), `"host":"gz"`) {
		t.Errorf("decompressed body = %q, want it to carry the payload", got)
	}
}

// ---- acceptsGzip --------------------------------------------------------

func TestAcceptsGzip(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate", true},
		{"deflate, gzip", true},
		{"gzip;q=1.0, identity;q=0.5", true},
		{" gzip ", true},
		{"GZIP", true},
		{"", false},
		{"identity", false},
		{"deflate", false},
		{"br", false},
		// A token that merely contains "gzip" is a different encoding.
		{"gzipper", false},
		{"x-gzip", false},
	}

	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
		if tc.header != "" {
			r.Header.Set("Accept-Encoding", tc.header)
		}
		if got := acceptsGzip(r); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// ---- framing ------------------------------------------------------------

func TestEventStream_SendFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.send("snapshot", map[string]any{"host": "framing", "n": 1}); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := rec.Body.String()
	if !strings.HasPrefix(got, "event: snapshot\ndata: ") {
		t.Errorf("frame = %q, want it to open with the event and data fields", got)
	}
	// A dispatched event ends with a blank line.
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("frame = %q, want it to end with a blank line", got)
	}
	// The payload must occupy exactly one data field, so it must not contain a
	// raw newline of its own.
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "event: snapshot\ndata: "), "\n\n")
	if strings.Contains(payload, "\n") {
		t.Errorf("payload = %q, want a single line", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if decoded["host"] != "framing" {
		t.Errorf("decoded host = %v, want %q", decoded["host"], "framing")
	}
}

func TestEventStream_RetryFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.retry(4200); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := rec.Body.String(); got != "retry: 4200\n\n" {
		t.Errorf("frame = %q, want %q", got, "retry: 4200\n\n")
	}
}

func TestEventStream_CommentFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.comment("keepalive"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if got := rec.Body.String(); got != ": keepalive\n\n" {
		t.Errorf("frame = %q, want %q", got, ": keepalive\n\n")
	}
}

// ---- error paths --------------------------------------------------------

// TestEventStream_SendUnencodableValue covers a payload the JSON encoder
// cannot represent. The stream must report the failure rather than emit a
// half-written frame and carry on.
func TestEventStream_SendUnencodableValue(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.send("snapshot", make(chan int)); err == nil {
		t.Fatal("send(chan) = nil error, want an encoding failure")
	}
}

// TestEventStream_SendToBrokenPeer is the disconnect path: once writes fail the
// stream must surface the error so the handler can unsubscribe and return.
func TestEventStream_SendToBrokenPeer(t *testing.T) {
	wantErr := errors.New("connection reset by peer")
	w := &failingWriter{err: wantErr}
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(w, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	defer s.Close() //nolint:errcheck

	err = s.send("snapshot", map[string]string{"host": "broken"})
	if err == nil {
		t.Fatal("send() = nil error, want the write failure to propagate")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestEventStream_RetryAndCommentToBrokenPeer(t *testing.T) {
	wantErr := errors.New("broken pipe")
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	for name, fn := range map[string]func(*eventStream) error{
		"retry":   func(s *eventStream) error { return s.retry(1000) },
		"comment": func(s *eventStream) error { return s.comment("keepalive") },
	} {
		w := &failingWriter{err: wantErr}
		s, err := newEventStream(w, r)
		if err != nil {
			t.Fatalf("%s: newEventStream: %v", name, err)
		}
		if err := fn(s); !errors.Is(err, wantErr) {
			t.Errorf("%s() error = %v, want it to wrap %v", name, err, wantErr)
		}
		s.Close() //nolint:errcheck
	}
}

// TestEventStream_CloseWithoutGzip verifies Close is safe on an uncompressed
// stream, which is the path a non-browser client takes.
func TestEventStream_CloseWithoutGzip(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)

	s, err := newEventStream(rec, r)
	if err != nil {
		t.Fatalf("newEventStream: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() = %v, want nil for an uncompressed stream", err)
	}
}
