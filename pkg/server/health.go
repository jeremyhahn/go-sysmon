package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// readyIntervals is how many stream intervals the latest snapshot may age
// before /readyz fails. One missed tick is noise; three in a row is a stall.
const readyIntervals = 3

// readyFloor is the least the readiness bound ever is. At a 500ms interval
// three intervals is 1.5s, shorter than a slow collection (SMART, NVML) can
// take, and a probe that flaps on every slow read is worse than none.
const readyFloor = 10 * time.Second

// readyBound returns how old the latest snapshot may be at the given stream
// interval and still count as fresh.
func readyBound(interval time.Duration) time.Duration {
	return max(readyIntervals*interval, readyFloor)
}

// handleHealthz is liveness: it answers 200 for as long as the process can
// serve a request at all, and checks nothing else.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeHealth(w, r, http.StatusOK, "ok")
}

// handleReadyz is readiness: 200 only while the collector is publishing and
// the server is not draining, otherwise 503 with the reason in the body.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.ready(time.Now()); err != nil {
		writeHealth(w, r, http.StatusServiceUnavailable, "not ready: "+err.Error())
		return
	}
	writeHealth(w, r, http.StatusOK, "ready")
}

// ready reports why the server is not ready at now, or nil when it is.
func (s *Server) ready(now time.Time) error {
	if s.draining.Load() {
		return &types.ServerDrainingError{}
	}
	snap, err := s.mon.Snapshot()
	if err != nil {
		return err
	}
	bound := readyBound(s.mon.Interval())
	if age := now.Sub(snap.Timestamp); age > bound {
		return &types.SnapshotStaleError{Age: age, Bound: bound}
	}
	return nil
}

// writeHealth writes a one-line plain-text probe answer. Probes must never be
// served from a cache, or a stale 200 would outlive the stall it hides.
func writeHealth(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(msg + "\n")); err != nil {
		// The status is already sent; a failed body write means the prober
		// went away, and there is no one left to tell.
		slog.Debug("health probe write", "err", err, "remote", r.RemoteAddr)
	}
}
