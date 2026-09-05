package server

import (
	"log/slog"
	"net/http"

	"github.com/jeremyhahn/go-sysmon/pkg/metrics"
)

// metricsContentType is the Prometheus text exposition format version.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// handleMetrics serves the latest snapshot in Prometheus text format.
//
// What is exported lives in pkg/metrics, shared with the remote-write exporter,
// so a node that is scraped and a node that pushes describe themselves
// identically. Whatever else changes about the transport, those two cannot
// drift apart.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snap, err := s.mon.Snapshot()
	if err != nil || snap == nil {
		http.Error(w, "no snapshot available", http.StatusServiceUnavailable)
		return
	}

	m := &metrics.TextWriter{}
	metrics.Write(m, snap)
	s.writeExporterMetrics(m)

	w.Header().Set("Content-Type", metricsContentType)
	if _, err := w.Write([]byte(m.String())); err != nil {
		slog.Debug("server: writing metrics", "error", err)
	}
}
