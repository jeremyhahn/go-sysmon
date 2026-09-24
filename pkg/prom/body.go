package prom

import (
	"io"
	"log/slog"
)

// drainLimit bounds how much of a response body is read and thrown away before
// it is closed. Draining to EOF is what returns the connection to the pool; a
// body with more left than this is not worth reading to save one connection,
// and closing it unread only costs that connection.
const drainLimit = 64 << 10

// releaseBody drains and closes a response body once its caller is done with
// it.
//
// Neither failure changes the answer the caller has already decoded, or the
// error it is already returning: by the time a body is released, whatever it
// said has been read. What a failure does mean is that the connection could not
// go back to the pool, so it is logged at debug level, against the URL, for
// anyone chasing connection churn.
func releaseBody(body io.ReadCloser, url string, log *slog.Logger) {
	if _, err := io.Copy(io.Discard, io.LimitReader(body, drainLimit)); err != nil {
		log.Debug("prom: draining response body", "url", url, "error", err)
	}
	if err := body.Close(); err != nil {
		log.Debug("prom: closing response body", "url", url, "error", err)
	}
}
