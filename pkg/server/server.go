// Package server provides an HTTP and WebSocket server for real-time system
// monitoring. Clients can poll the latest snapshot via REST or subscribe to a
// live stream over WebSocket.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const (
	// writeTimeout is the maximum time allowed to write a single WebSocket message.
	writeTimeout = 10 * time.Second

	// pongWait is the maximum time the server waits for a pong response.
	pongWait = 60 * time.Second
)

// pingInterval controls how often the server sends a WebSocket ping frame.
// It is a variable rather than a constant so tests can drive the keepalive
// path without waiting half a minute.
var pingInterval = 30 * time.Second

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

// wsCommand is a JSON message sent by the client to change server behavior.
// Currently only "set_interval" is supported.
type wsCommand struct {
	Type       string `json:"type"`
	IntervalMS int    `json:"interval_ms,omitempty"`
}

// upgrader configures the WebSocket handshake. CheckOrigin permits all origins
// so that the server is usable behind a reverse proxy or during local development.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Server exposes system metrics over HTTP and WebSocket.
type Server struct {
	mon    *monitor.Monitor
	addr   string
	assets fs.FS
	server *http.Server
}

// New creates a Server that delegates to mon for metric data.
// addr is the TCP address to listen on (e.g. ":8080").
// assets may be nil; when provided it is served at GET /.
func New(mon *monitor.Monitor, addr string, assets fs.FS) *Server {
	s := &Server{
		mon:    mon,
		addr:   addr,
		assets: assets,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /ws", s.handleWebSocket)

	if assets != nil {
		mux.Handle("GET /", cacheControl(http.FileServerFS(assets)))
	} else {
		mux.HandleFunc("GET /", s.handleStatus)
	}

	s.server = &http.Server{
		Addr:    addr,
		Handler: mux,

		// ReadHeaderTimeout bounds how long a client may take to send its
		// request headers. Without it, a handful of connections dribbling
		// headers one byte at a time can hold every accept slot open
		// indefinitely -- a slowloris, which matters here because the
		// dashboard is unauthenticated and may be reachable by anything that
		// can route to it.
		//
		// Only the header phase is bounded. A ReadTimeout would also cap the
		// body and would break /ws, whose whole purpose is a connection that
		// stays open for hours.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s
}

// ServeHTTP implements http.Handler, allowing the server's mux to be used
// directly with httptest.Server or any other http.Handler consumer.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.server.Handler.ServeHTTP(w, r)
}

// Start begins listening for incoming connections. It blocks until the server
// is stopped or encounters a fatal error. ErrServerClosed is not returned as
// an error – it signals a clean shutdown.
func (s *Server) Start() error {
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

// handleWebSocket upgrades the connection and streams snapshots in real time.
// Each connection subscribes to the monitor and receives every snapshot until
// the client disconnects or the monitor stops.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade", "err", err, "remote", r.RemoteAddr)
		// upgrader already wrote an HTTP error response.
		return
	}

	sub := s.mon.Subscribe()
	defer func() {
		s.mon.Unsubscribe(sub.ID)
		conn.Close()
	}()

	// Configure the pong handler to extend the read deadline on each response.
	conn.SetReadDeadline(time.Now().Add(pongWait)) //nolint:errcheck
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	// readPump drains incoming frames and processes client commands. It keeps
	// the connection healthy by consuming pong and close frames, and handles
	// set_interval commands to change the monitor's polling rate.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var cmd wsCommand
			if json.Unmarshal(msg, &cmd) != nil {
				continue
			}
			if cmd.Type == "set_interval" && allowedIntervals[cmd.IntervalMS] {
				_ = s.mon.SetInterval(time.Duration(cmd.IntervalMS) * time.Millisecond)
			}
		}
	}()

	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-readDone:
			// Client closed the connection.
			return

		case <-sub.Done:
			// Monitor stopped; send a close frame and exit.
			writeClose(conn)
			return

		case snap, ok := <-sub.Ch:
			if !ok {
				writeClose(conn)
				return
			}
			if err := writeJSON(conn, snap); err != nil {
				slog.Debug("websocket write", "err", err, "remote", r.RemoteAddr)
				return
			}

		case <-pingTicker.C:
			conn.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:errcheck
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				slog.Debug("websocket ping", "err", err, "remote", r.RemoteAddr)
				return
			}
		}
	}
}

// writeJSON encodes v as JSON and writes it as a text WebSocket message.
func writeJSON(conn *websocket.Conn, v any) error {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:errcheck
	w, err := conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The encode error is the more useful of the two; close the writer to
		// release the connection's write lock but report the original failure.
		if closeErr := w.Close(); closeErr != nil {
			slog.Debug("websocket writer close after encode error", "err", closeErr)
		}
		return err
	}
	return w.Close()
}

// writeClose sends a normal WebSocket close frame.
func writeClose(conn *websocket.Conn) {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "monitor stopped")
	conn.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:errcheck
	conn.WriteMessage(websocket.CloseMessage, msg)      //nolint:errcheck
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
