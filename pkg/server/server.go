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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
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

// responseWriteTimeout bounds a whole ordinary response: the handler's own work
// plus pushing the body onto the socket. It is deliberately generous. Its job is
// to release a connection whose peer has vanished, not to cap a slow client, and
// the longest legitimate response is a history query, which is already bounded
// by the Prometheus client's own timeout.
//
// A variable rather than a constant for the same reason as keepaliveInterval:
// so a test can drive the deadline without waiting half a minute.
var responseWriteTimeout = 30 * time.Second

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

	// Fleet is optional. When set, the fleet endpoints serve its aggregated
	// view; when nil the fleet reports itself disabled.
	Fleet *fleet.Poller

	// Config is optional. When set, the settings endpoint reports the
	// effective configuration. It is read-only: reloads happen through SIGHUP
	// or the file watcher, never over HTTP.
	Config *config.Manager

	// Exporter is optional. When set, its health is published on the scrape
	// endpoint, so a node that has quietly stopped pushing history says so.
	Exporter *prom.Exporter

	// History is optional. When set, the history endpoints answer date-range
	// queries from Prometheus; when nil they report history as unavailable and
	// the dashboard offers no range picker, which is exactly the standalone
	// node's behaviour.
	History *prom.Querier

	// Version, GitCommit and BuildDate are the build metadata reported at
	// GET /api/version. Empty values render as "dev"/"unknown" downstream.
	Version   string
	GitCommit string
	BuildDate string
}

// Server exposes system metrics over HTTP.
type Server struct {
	mon       *monitor.Monitor
	addr      string
	assets    fs.FS
	server    *http.Server
	tlsConfig *tls.Config
	// fleet and cfg are optional. When nil the corresponding endpoints report
	// the feature as disabled rather than 404ing, so a standalone node and an
	// old build are distinguishable by a client.
	fleet *fleet.Poller
	cfg   *config.Manager

	// exporter and history are optional in the same way, and for the same
	// reason: a node with neither is a standalone node, not a broken one.
	exporter *prom.Exporter
	history  *prom.Querier

	// deltas diffs once per tick for every client that asked for the delta
	// protocol. It runs only while at least one has.
	deltas *deltaHub

	// ln holds the bound listener once Listen has run. It is atomic because
	// the watchdog reads the address from another goroutine while Start is
	// binding, and a plain field would be a race the detector would find.
	ln atomic.Pointer[net.Listener]

	version   string
	gitCommit string
	buildDate string
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
		fleet:     cfg.Fleet,
		cfg:       cfg.Config,
		exporter:  cfg.Exporter,
		history:   cfg.History,
		version:   cfg.Version,
		gitCommit: cfg.GitCommit,
		buildDate: cfg.BuildDate,
	}
	s.deltas = newDeltaHub(cfg.Monitor, slog.Default())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/interval", s.handleGetInterval)
	mux.HandleFunc("POST /api/interval", s.handleSetInterval)
	mux.HandleFunc("GET /api/fleet", s.handleFleet)
	mux.HandleFunc("GET /api/fleet/{name}/snapshot", s.handleFleetSnapshot)
	mux.HandleFunc("GET /api/fleet/{name}/config", s.handleFleetConfig)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/history/metrics", s.handleHistoryMetrics)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

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

		// WriteTimeout bounds the response side, which for a long time nothing
		// did. A peer that goes away without an RST leaves a write parked in TCP
		// retransmission for as long as the kernel will retry, holding a
		// goroutine and a connection with nothing to release either.
		//
		// Measured on vm-host-2 on 2026-09-03: 53 goroutines in IO wait, every
		// one of them in net/http.checkConnErrorWriter.Write, 44 of them for the
		// same seven minutes, on a node whose accept queue had reached 2929.
		//
		// /api/events needs no exemption here, which is the point of the
		// symmetry with ReadTimeout above being only apparent. A ReadTimeout
		// would cap the stream and could not be undone from inside the handler;
		// this deadline is replaced by eventStream.deadline before every single
		// event it writes, so a stream is bounded per event rather than in
		// total -- which is the property a stream actually wants. Remove that
		// call and the stream inherits this timeout and dies at it.
		WriteTimeout: responseWriteTimeout,
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
	if _, err := s.Listen(); err != nil {
		return err
	}

	var err error
	if s.tlsConfig != nil {
		// Certificates already live in TLSConfig, so no paths are passed here.
		err = s.server.ServeTLS(s.listener(), "", "")
	} else {
		err = s.server.Serve(s.listener())
	}
	if err != nil && err != http.ErrServerClosed {
		return &types.ServerStartError{Cause: err}
	}
	return nil
}

// Listen binds the configured address and returns the address actually bound.
// It is idempotent: a second call returns the address from the first.
//
// It exists so the address is known before anything serves on it. Start blocks,
// so a caller that needs the address has to run Start in a goroutine and then
// discover it -- and with a port of 0, or an address that fails to bind, there
// is nothing to discover and no error until later. Binding first turns "the
// address is in use" into an error at the call site, and gives the watchdog a
// real address to dial rather than the configured string.
//
// The distinction matters: ":8081" is not dialable, and a specific bind address
// is not 127.0.0.1. The watchdog has to reach the socket that exists, not the
// one the configuration describes.
func (s *Server) Listen() (net.Addr, error) {
	if l := s.listener(); l != nil {
		return l.Addr(), nil
	}

	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, &types.ServerStartError{Cause: err}
	}
	s.ln.Store(&l)
	return l.Addr(), nil
}

// UsesTLS reports whether the server serves HTTPS. A caller dialling the
// server has to know the scheme, and the configured TLS block is not exported.
func (s *Server) UsesTLS() bool { return s.tlsConfig != nil }

// Addr returns the address the server is listening on, or "" before it binds.
func (s *Server) Addr() string {
	l := s.listener()
	if l == nil {
		return ""
	}
	return l.Addr().String()
}

// listener returns the bound listener, or nil.
func (s *Server) listener() net.Listener {
	if l := s.ln.Load(); l != nil {
		return *l
	}
	return nil
}

// Stop gracefully shuts down the HTTP server within the deadline given by ctx.
func (s *Server) Stop(ctx context.Context) error {
	err := s.server.Shutdown(ctx)

	// Shutdown closes the listeners http.Server is SERVING on, and a listener
	// that was bound and never served is not one of them. Listen exists so the
	// address can be bound before serving starts (#78), so Stop has to release
	// what Listen took -- otherwise a process told to stop during startup keeps
	// the port open, and the next start of the service finds its own address in
	// use (#106).
	//
	// Closing twice is not an error worth reporting: http.Server has already
	// closed it on the normal path, and "use of closed network connection" is
	// the expected answer there rather than a failure to stop.
	if l := s.listener(); l != nil {
		_ = l.Close()
	}
	return err
}

// handleVersion returns the build metadata embedded at compile time so the
// UI can display which release a node is running. The values are injected
// via -ldflags at build time and fall back to dev/unknown for local builds.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	v, c, d := s.version, s.gitCommit, s.buildDate
	if v == "" {
		v = "dev"
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version":    v,
		"git_commit": c,
		"build_date": d,
	})
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
	// Scope names which poller the value applies to. Empty or "local" is this
	// node's own monitor, which is what every existing client sends and means
	// exactly what it always did. "fleet" is the peer refresh cadence.
	//
	// The dashboard's control has to drive the poller that actually feeds the
	// view it is on (#97): while a peer is selected, that is the fleet poller,
	// and changing the local monitor instead would report a cadence nothing was
	// collecting at.
	Scope string `json:"scope,omitempty"`
	// FleetIntervalMS is reported, never accepted: it is the cadence peers are
	// actually refreshed at, after the floor and the configuration are
	// resolved. Zero means this node has no fleet.
	FleetIntervalMS int `json:"fleet_interval_ms,omitempty"`
}

// Interval scopes.
const (
	scopeLocal = "local"
	scopeFleet = "fleet"
)

// fleetIntervalMS reports the peer refresh cadence, or 0 when there is no
// fleet.
func (s *Server) fleetIntervalMS() int {
	if s.fleet == nil {
		return 0
	}
	return int(s.fleet.PollInterval().Milliseconds())
}

// handleGetInterval reports the monitor's current polling interval.
func (s *Server) handleGetInterval(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, intervalPayload{
		IntervalMS:      int(s.mon.Interval().Milliseconds()),
		FleetIntervalMS: s.fleetIntervalMS(),
	})
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

	switch req.Scope {
	case "", scopeLocal:
		if err := s.mon.SetInterval(time.Duration(req.IntervalMS) * time.Millisecond); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case scopeFleet:
		if s.fleet == nil {
			http.Error(w, (&types.InvalidIntervalError{
				Message: "this node has no fleet",
			}).Error(), http.StatusBadRequest)
			return
		}
		s.fleet.SetPollInterval(time.Duration(req.IntervalMS) * time.Millisecond)
	default:
		http.Error(w, (&types.InvalidIntervalError{
			Message: "unknown scope",
		}).Error(), http.StatusBadRequest)
		return
	}

	// The response reports what is in force, not what was asked for. A peer
	// refresh is floored (fleet.MinPollInterval), so a request for 250ms is
	// answered with the second it will actually run at -- the dashboard states
	// the cadence it is being served, and a clamped value must not be presented
	// back as though it had been honoured.
	writeJSON(w, intervalPayload{
		IntervalMS:      int(s.mon.Interval().Milliseconds()),
		FleetIntervalMS: s.fleetIntervalMS(),
	})
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

	// Two protocols on one endpoint. Without the parameter the stream is what
	// it has always been: one whole snapshot per tick, event "snapshot", the
	// payload the bare snapshot. Any client written against the documented API
	// keeps working unchanged.
	//
	// With ?deltas=1 the client declares it can apply patches, and gets "full"
	// and "delta" frames carrying sequence numbers. The names differ from
	// "snapshot" on purpose: a client that negotiated one protocol and is
	// reading the other fails at once rather than parsing an envelope as a
	// snapshot and rendering a page of undefined values.
	if wantsDeltas(r) {
		s.streamDeltas(w, r, stream)
		return
	}

	sub := s.mon.Subscribe()
	defer s.mon.Unsubscribe(sub.ID)

	// The same guard the delta path makes, on the protocol that has been here
	// longer -- and it was missing here for exactly as long.
	//
	// A stopped monitor still answers Snapshot with the last thing it collected
	// and still hands out subscriptions, but Stop has already walked the
	// subscriber map, so this one's Done will never close. A client connecting
	// after shutdown was therefore sent that pre-stop reading below and then
	// keepalives for as long as it stayed: a measurement from before the
	// service stopped, presented as live, for ever. Checked AFTER subscribing,
	// because Stop clears its flag before it walks the map -- a subscription
	// taken before the walk has its Done closed, and one taken after sees the
	// flag already false here.
	if !s.mon.Running() {
		if err := stream.send("bye", "monitor stopped"); err != nil {
			slog.Debug("event stream close", "err", err, "remote", r.RemoteAddr)
		}
		return
	}

	// Send the most recent snapshot immediately so a reconnecting client is
	// not blank until the next poll tick -- unless the subscription has closed
	// in the meantime. Running() was true a moment ago and Stop can have
	// completed since; re-asking here narrows that to the write itself, which
	// is as far as a check can go, and is the same thing the delta path does
	// before its initial frame.
	if snap, err := s.mon.Snapshot(); err == nil && !closed(sub.Done) {
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
			// A closed subscription wins over a queued snapshot. Both cases can
			// be ready at once and Go picks between them at random, so without
			// this a collection from before the stop could be sent after the
			// stream was already over -- rendered as current, then contradicted
			// by the bye that follows it.
			if closed(sub.Done) {
				if err := stream.send("bye", "monitor stopped"); err != nil {
					slog.Debug("event stream close", "err", err, "remote", r.RemoteAddr)
				}
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

// wantsDeltas reports whether the client asked for the delta protocol.
//
// Opt-in rather than negotiated by content type, because it is a change of
// payload shape rather than of encoding, and because an operator who suspects
// the delta path can drop the parameter and get the old stream back without
// deploying anything.
func wantsDeltas(r *http.Request) bool {
	switch r.URL.Query().Get("deltas") {
	case "1", "true", "yes":
		return true
	}
	return false
}

// streamDeltas serves the sequence-numbered protocol.
//
// The client is sent a whole snapshot whenever it is not exactly one frame
// behind: on connection, after a missed frame, and on the periodic resync. It
// is never sent a patch against a base it does not have, which is the one way
// this stream could leave a reading on screen that looks measured and is not.
func (s *Server) streamDeltas(w http.ResponseWriter, r *http.Request, stream *eventStream) {
	client := newClientStream(s.deltas)
	defer client.close()

	// The frame already published, so a client connecting between ticks is not
	// blank until the next one.
	//
	// Only while the monitor is actually collecting. The cached frame outlives
	// the run that produced it -- deliberately, so a client arriving between
	// ticks is not blank -- and after a stop that makes it a reading from
	// before the service stopped. The hub clears it when a run ends for that
	// reason, and clientStream.initial makes the same check on the way out.
	if name, payload, ok := client.initial(); ok {
		if err := stream.sendRaw(name, payload); err != nil {
			slog.Debug("event stream write", "err", err, "remote", r.RemoteAddr)
			return
		}
	}

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-client.done():
			if err := stream.send("bye", "monitor stopped"); err != nil {
				slog.Debug("event stream close", "err", err, "remote", r.RemoteAddr)
			}
			return

		case frame := <-client.frames():
			// A closed subscription wins over a queued frame. Both cases can be
			// ready at once and Go picks between them at random, so without
			// this a frame published before the stop could be written after the
			// stream was already over -- rendered and marked current, then
			// followed by the bye that says it is not.
			if client.ended() {
				if err := stream.send("bye", "monitor stopped"); err != nil {
					slog.Debug("event stream close", "err", err, "remote", r.RemoteAddr)
				}
				return
			}
			name, payload, skip := client.next(frame)
			if skip {
				continue // the frame already sent on connection
			}
			if err := stream.sendRaw(name, payload); err != nil {
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

// closed reports whether a done channel has been closed, without waiting.
//
// The cached frame is validated and then written, and a stop can complete
// between the two. Re-asking immediately before the write narrows that to the
// write itself, which is as far as a check can go: a frame that was current
// when it was handed to the socket is not the failure this stream guards
// against, while one from a stop that had already finished is.
func closed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
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

// sendRaw writes a named event whose payload is already encoded.
//
// It exists so a frame is encoded once for the whole fleet of connections
// rather than once per connection, which is the saving this protocol is for.
// The payload must be compact JSON: SSE terminates a data field at a newline,
// and encoding/json never emits a raw one.
func (s *eventStream) sendRaw(name string, payload []byte) error {
	if err := s.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, "event: "+name+"\ndata: "); err != nil {
		return err
	}
	if _, err := s.out.Write(payload); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, "\n\n"); err != nil {
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
