package prom

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// discardLog is the logger for tests that do not look at what is logged.
var discardLog = slog.New(slog.DiscardHandler)

// errBodyRead and errBodyClose are the faults failingBody injects.
var (
	errBodyRead  = errors.New("injected read failure")
	errBodyClose = errors.New("injected close failure")
)

// failingBody is a response body whose read and close both fail.
type failingBody struct{ closed bool }

func (b *failingBody) Read([]byte) (int, error) { return 0, errBodyRead }

func (b *failingBody) Close() error {
	b.closed = true
	return errBodyClose
}

// countingBody records how much of it was read and whether it was closed.
type countingBody struct {
	r      io.Reader
	read   int64
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *countingBody) Close() error {
	b.closed = true
	return nil
}

// TestReleaseBody_LogsBothFailures verifies that a body which fails to drain
// and to close is still closed, and that both failures reach the log against
// the URL instead of vanishing.
func TestReleaseBody_LogsBothFailures(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := &failingBody{}

	releaseBody(body, "http://prom:9090/api/v1/query_range", log)

	if !body.closed {
		t.Error("releaseBody did not close a body whose drain failed")
	}
	out := logs.String()
	for _, want := range []string{"draining response body", "injected read failure", "closing response body", "injected close failure", "http://prom:9090/api/v1/query_range"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}

// TestReleaseBody_BoundsTheDrain verifies that an oversized body is drained
// only up to drainLimit before it is closed, and that a clean release logs
// nothing.
func TestReleaseBody_BoundsTheDrain(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := &countingBody{r: bytes.NewReader(make([]byte, 4*drainLimit))}

	releaseBody(body, "http://prom:9090", log)

	if body.read != drainLimit {
		t.Errorf("drained %d bytes, want exactly drainLimit (%d)", body.read, drainLimit)
	}
	if !body.closed {
		t.Error("releaseBody did not close the body")
	}
	if logs.Len() != 0 {
		t.Errorf("a clean release logged %q, want nothing", logs.String())
	}
}

// TestWrite_RejectionWithUnreadableBody verifies that a rejected write whose
// body breaks off partway is still reported as the rejection, with its status,
// and carries the read failure rather than presenting a truncated body as
// complete.
func TestWrite_RejectionWithUnreadableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise more than is sent: the server then closes the connection,
		// and the client's read ends in io.ErrUnexpectedEOF.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte("out of order")); err != nil {
			t.Errorf("fake receiver: write: %v", err)
		}
	}))
	defer srv.Close()

	w := NewWriter(srv.URL, 5*time.Second, discardLog)
	err := w.Write(context.Background(), []Series{{Labels: []Label{{Name: "__name__", Value: "up"}}, Samples: []Sample{{Value: 1, TimeMs: 1}}}})

	var rw *ErrRemoteWrite
	if !errors.As(err, &rw) {
		t.Fatalf("Write() = %v, want *ErrRemoteWrite", err)
	}
	if rw.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400", rw.Status)
	}
	if rw.Body != "out of order" {
		t.Errorf("Body = %q, want what was read before the failure", rw.Body)
	}
	if !errors.Is(rw.BodyErr, io.ErrUnexpectedEOF) {
		t.Errorf("BodyErr = %v, want io.ErrUnexpectedEOF", rw.BodyErr)
	}
	if !strings.Contains(rw.Error(), "response body unreadable") {
		t.Errorf("Error() = %q does not mention the unreadable body", rw.Error())
	}
}
