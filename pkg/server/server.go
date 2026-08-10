// Package server provides an HTTP server for real-time system monitoring.
// Clients can poll the latest snapshot via REST or subscribe to a live stream
// of server-sent events (SSE) at /api/events.
//
// SSE is used rather than WebSocket because the data flow is one-way: the
// server pushes snapshots and the client never needs a duplex channel. As an
// ordinary HTTP response the stream stays inside the standard handler chain,
// so it is subject to the same-origin policy, ordinary middleware, and normal
// response compression. Rate control, the one client-to-server operation, is a
// plain REST call on /api/interval.
package server

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const (
	// writeTimeout is the maximum time allowed to write a single event.
	writeTimeout = 10 * time.Second

	// reconnectDelayMS is advertised to EventSource clients in the stream's
	// retry field, replacing the browser default of about three seconds.
	reconnectDelayMS = 3000
)

// keepaliveInterval controls how often a comment frame is written to an
// otherwise idle stream. It keeps intermediaries from reaping the connection
// and surfaces a dead peer as a write error. It is a variable rather than a
// constant so tests can drive the keepalive path without waiting half a
// minute.
var keepaliveInterval = 30 * time.Second

// allowedIntervals is the set of polling intervals (in milliseconds) that
// clients are permitted to request. Values match the UI dropdown options.
var allowedIntervals = map[int]bool{
	250:   true,
	500:   true,
	1000:  true,
	5000:  true,
	10000: true,
	15000: true,
	30000: true,
	60000: true,
}

// TLS describes the server's transport security. It supports both the classic
// static case, where certificate and key live on disk, and the dynamic case,
// where an embedding application resolves the configuration itself.
//
// Note that TLS is negotiated once per connection, before any HTTP request
// exists, so the dynamic hook fires per handshake rather than per request.
// GetConfigForClient receives the ClientHelloInfo, which carries the SNI
// server name, offered ALPN protocols, cipher suites and the client address.
type TLS struct {
	// CertFile and KeyFile are paths to a PEM certificate and private key.
	// Both must be set together.
	CertFile string
	KeyFile  string

	// Config is a caller-supplied base configuration. It is cloned before use,
	// so the caller's value is never mutated. Certificates loaded from
	// CertFile and KeyFile are appended to the clone.
	Config *tls.Config

	// GetConfigForClient resolves the configuration for each incoming
	// handshake. A non-nil return value replaces the base configuration for
	// that connection entirely, so it must carry its own certificates. To keep
	// HTTP/2 available, the returned config's NextProtos should include "h2"
	// and "http/1.1".
	GetConfigForClient func(*tls.ClientHelloInfo) (*tls.Config, error)
}

// buildConfig resolves t into a *tls.Config ready to hand to http.Server.
// It returns a TLSConfigError when no certificate source is configured or a
// key pair fails to load.
func (t *TLS) buildConfig() (*tls.Config, error) {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return nil, &types.TLSConfigError{
			Message: "CertFile and KeyFile must be set together",
		}
	}

	var cfg *tls.Config
	if t.Config != nil {
		cfg = t.Config.Clone()
	} else {
		cfg = &tls.Config{}
	}

	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, &types.TLSConfigError{
				Message: "load key pair " + t.CertFile,
				Cause:   err,
			}
		}
		cfg.Certificates = append(cfg.Certificates, cert)
	}

	if t.GetConfigForClient != nil {
		cfg.GetConfigForClient = t.GetConfigForClient
	}

	// A config with no way to produce a certificate fails at handshake time
	// with an opaque error. Catching it at construction points at the real
	// mistake instead.
	if len(cfg.Certificates) == 0 && cfg.GetCertificate == nil && cfg.GetConfigForClient == nil {
		return nil, &types.TLSConfigError{
			Message: "no certificate source: set CertFile and KeyFile, or supply Config or GetConfigForClient",
		}
	}

	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}

	return cfg, nil
}

// Config carries everything needed to construct a Server. It is the entry
// point for applications that embed go-sysmon as a library.
type Config struct {
	// Monitor supplies snapshot data. Required.
	Monitor *monitor.Monitor

	// Addr is the TCP address to listen on, for example ":8080".
	Addr string

	// Assets is an optional filesystem served at GET /. When nil, a plain
	// status page is served instead.
	Assets fs.FS

	// TLS is optional. When nil the server speaks plain HTTP.
	TLS *TLS
}

// Server exposes system metrics over HTTP.
type Server struct {
	mon       *monitor.Monitor
	addr      string
	assets    fs.FS
	server    *http.Server
	tlsConfig *tls.Config
}

// New creates a Server that delegates to mon for metric data.
// addr is the TCP address to listen on (e.g. ":8080").
// assets may be nil; when provided it is served at GET /.
//
// New serves plain HTTP. Use NewWithConfig to enable TLS.
func New(mon *monitor.Monitor, addr string, assets fs.FS) *Server {
	return newServer(Config{Monitor: mon, Addr: addr, Assets: assets}, nil)
}

// NewWithConfig creates a Server from cfg. It returns a TLSConfigError when
// cfg.TLS is present but cannot be resolved into a usable configuration.
func NewWithConfig(cfg Config) (*Server, error) {
	var tlsConfig *tls.Config
	if cfg.TLS != nil {
		var err error
		tlsConfig, err = cfg.TLS.buildConfig()
		if err != nil {
			return nil, err
		}
	}
	return newServer(cfg, tlsConfig), nil
}

// newServer wires the routes and http.Server. It cannot fail: every input has
// already been validated by the exported constructors.
func newServer(cfg Config, tlsConfig *tls.Config) *Server {
	s := &Server{
		mon:       cfg.Monitor,
		addr:      cfg.Addr,
		assets:    cfg.Assets,
		tlsConfig: tlsConfig,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/interval", s.handleGetInterval)
	mux.HandleFunc("POST /api/interval", s.handleSetInterval)

	if cfg.Assets != nil {
		mux.Handle("GET /", cacheControl(http.FileServerFS(cfg.Assets)))
	} else {
		mux.HandleFunc("GET /", s.handleStatus)
	}

	s.server = &http.Server{
		Addr:      cfg.Addr,
		Handler:   mux,
		TLSConfig: s.tlsConfig,

		// ReadHeaderTimeout bounds how long a client may take to send its
		// request headers. Without it, a handful of connections dribbling
		// headers one byte at a time can hold every accept slot open
		// indefinitely -- a slowloris, which matters here because the
		// dashboard is unauthenticated and may be reachable by anything that
		// can route to it.
		//
		// Only the header phase is bounded. A ReadTimeout would also cap the
		// body and would break /api/events, whose whole purpose is a response
		// that stays open for hours.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s
}

// ServeHTTP implements http.Handler, allowing the server's mux to be used
// directly with httptest.Server or mounted on an embedding application's own
// router. Applications that terminate TLS themselves can use this and ignore
// Start entirely.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.server.Handler.ServeHTTP(w, r)
}

// Start begins listening for incoming connections, over TLS when the server
// was configured with it. It blocks until the server is stopped or encounters
// a fatal error. ErrServerClosed is not returned as an error – it signals a
// clean shutdown.
func (s *Server) Start() error {
	var err error
	if s.tlsConfig != nil {
		// Certificates already live in TLSConfig, so no paths are passed here.
		err = s.server.ListenAndServeTLS("", "")
	} else {
		err = s.server.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		return &types.ServerStartError{Cause: err}
	}
	return nil
}

// Stop gracefully shuts down the HTTP server within the deadline given by ctx.
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// handleStatus serves a minimal status page when no frontend assets are embedded.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("go-sysmon is running\n"))
}

// handleSnapshot returns the most recent system snapshot as JSON.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.mon.Snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		slog.Error("snapshot encode", "err", err)
	}
}

// intervalPayload is the JSON body accepted by POST /api/interval and returned
// by both interval handlers.
type intervalPayload struct {
	IntervalMS int `json:"interval_ms"`
}

// handleGetInterval reports the monitor's current polling interval.
func (s *Server) handleGetInterval(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, intervalPayload{IntervalMS: int(s.mon.Interval().Milliseconds())})
}

// handleSetInterval changes the monitor's polling interval.
//
// The interval is a property of the monitor, which every subscriber shares, so
// a change here affects every connected client. That is the pre-existing
// behaviour of the single shared poll loop; the REST endpoint makes it visible
// rather than burying it in a stream command.
func (s *Server) handleSetInterval(w http.ResponseWriter, r *http.Request) {
	var req intervalPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	if !allowedIntervals[req.IntervalMS] {
		http.Error(w, (&types.InvalidIntervalError{
			Message: "not an allowed value",
		}).Error(), http.StatusBadRequest)
		return
	}

	if err := s.mon.SetInterval(time.Duration(req.IntervalMS) * time.Millisecond); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, intervalPayload{IntervalMS: req.IntervalMS})
}

// writeJSON encodes v as a JSON response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("response encode", "err", err)
	}
}

// handleEvents streams snapshots to the client as server-sent events. Each
// connection subscribes to the monitor and receives every snapshot until the
// client disconnects or the monitor stops.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Connection", "keep-alive")
	// Reverse proxies buffer response bodies by default, which defeats
	// streaming entirely. This asks nginx and its imitators not to.
	h.Set("X-Accel-Buffering", "no")

	stream, err := newEventStream(w, r)
	if err != nil {
		// Nothing has been written yet, so a normal error response is still
		// possible. http.Error resets the content type set above.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	if err := stream.retry(reconnectDelayMS); err != nil {
		slog.Debug("event stream retry", "err", err, "remote", r.RemoteAddr)
		return
	}

	sub := s.mon.Subscribe()
	defer s.mon.Unsubscribe(sub.ID)

	// Send the most recent snapshot immediately so a reconnecting client is
	// not blank until the next poll tick.
	if snap, err := s.mon.Snapshot(); err == nil {
		if err := stream.send("snapshot", snap); err != nil {
			slog.Debug("event stream write", "err", err, "remote", r.RemoteAddr)
			return
		}
	}

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected.
			return

		case <-sub.Done:
			// Monitor stopped; tell the client not to reconnect.
			if err := stream.send("bye", "monitor stopped"); err != nil {
				slog.Debug("event stream close", "err", err, "remote", r.RemoteAddr)
			}
			return

		case snap, ok := <-sub.Ch:
			if !ok {
				return
			}
			if err := stream.send("snapshot", snap); err != nil {
				slog.Debug("event stream write", "err", err, "remote", r.RemoteAddr)
				return
			}

		case <-keepalive.C:
			if err := stream.comment("keepalive"); err != nil {
				slog.Debug("event stream keepalive", "err", err, "remote", r.RemoteAddr)
				return
			}
		}
	}
}

// eventStream writes server-sent events to a single client.
//
// When the client offers gzip the stream is compressed. That matters more here
// than for a normal response: consecutive snapshots are nearly identical, and
// because the deflate dictionary carries across flushes, every snapshot after
// the first compresses against the one before it.
type eventStream struct {
	rc  *http.ResponseController
	out io.Writer
	enc *json.Encoder
	gz  *gzip.Writer
}

// newEventStream negotiates content encoding and prepares w for streaming.
// It returns a StreamUnsupportedError when w cannot be flushed.
func newEventStream(w http.ResponseWriter, r *http.Request) (*eventStream, error) {
	// Content negotiation has to happen before the probe flush below, because
	// that flush commits the header block. Setting Content-Encoding afterwards
	// would compress a body the client was never told to inflate.
	gzipped := acceptsGzip(r)
	if gzipped {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
	}

	// Probe for flush support. On failure nothing has been committed, so the
	// caller can still send an error response — but the negotiated headers must
	// come back off first.
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		w.Header().Del("Content-Encoding")
		w.Header().Del("Vary")
		return nil, &types.StreamUnsupportedError{}
	}

	s := &eventStream{rc: rc, out: w}
	if gzipped {
		s.gz = gzip.NewWriter(w)
		s.out = s.gz
	}

	s.enc = json.NewEncoder(s.out)
	return s, nil
}

// acceptsGzip reports whether the client advertised gzip support.
func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(enc, ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return true
		}
	}
	return false
}

// send writes a named event carrying v as JSON.
func (s *eventStream) send(name string, v any) error {
	if err := s.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, "event: "+name+"\ndata: "); err != nil {
		return err
	}
	// Encode writes compact JSON followed by a newline. Compact JSON never
	// contains a raw newline, so the payload always occupies a single data
	// field; the extra newline below terminates the event.
	if err := s.enc.Encode(v); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, "\n"); err != nil {
		return err
	}
	return s.flush()
}

// retry writes the stream's reconnect hint, in milliseconds.
func (s *eventStream) retry(ms int) error {
	if err := s.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, "retry: "+strconv.Itoa(ms)+"\n\n"); err != nil {
		return err
	}
	return s.flush()
}

// comment writes an SSE comment frame, which clients ignore. It exists to move
// bytes over an idle connection.
func (s *eventStream) comment(text string) error {
	if err := s.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, ": "+text+"\n\n"); err != nil {
		return err
	}
	return s.flush()
}

// deadline bounds the write that follows. A writer that cannot carry a
// deadline is not an error: the deadline is hardening against a stalled peer,
// not a correctness requirement, and the stream must still work behind
// middleware or a test recorder that does not implement it.
func (s *eventStream) deadline() error {
	err := s.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// flush pushes buffered bytes all the way to the socket. The gzip writer is
// flushed first so its output reaches the ResponseWriter before that in turn
// is flushed to the client.
func (s *eventStream) flush() error {
	if s.gz != nil {
		if err := s.gz.Flush(); err != nil {
			return err
		}
	}
	return s.rc.Flush()
}

// Close releases the gzip writer, if one was used.
func (s *eventStream) Close() error {
	if s.gz != nil {
		return s.gz.Close()
	}
	return nil
}

// cacheControl sets caching headers appropriate to each kind of asset.
//
// The bundle filenames are content-hashed, so they can be cached forever and a
// new build produces new names. index.html references those names and must
// therefore never be served from cache without revalidation: a stale copy
// pins the browser to an old bundle permanently, so a redeployed server keeps
// showing the previous UI. The embedded filesystem also reports a zero
// modification time, which leaves the response with no validator at all and
// lets browsers fall back to heuristic caching.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			// Content-addressed: safe to keep indefinitely.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Entry point and anything else: always revalidate.
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}
