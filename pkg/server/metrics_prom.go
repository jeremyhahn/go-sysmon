//go:build prometheus

package server

import "github.com/jeremyhahn/go-sysmon/pkg/metrics"

// writeExporterMetrics reports the remote-write exporter's own health.
//
// An exporter that has quietly stopped delivering is the failure this feature
// has to avoid: history that silently stops being recorded looks exactly like
// history that has not happened yet, and the dashboard would draw a flat gap
// with nothing to say why. These series give the gap a cause sitting next to
// it, on the endpoint an operator already scrapes.
//
// Nothing is emitted on a node with no exporter, so a standalone node's
// exposition is byte-for-byte what it always was.
func (s *Server) writeExporterMetrics(m *metrics.TextWriter) {
	if s.exporter == nil {
		return
	}
	st := s.exporter.Stats()
	// Nothing is emitted for an exporter that is not pushing, so a node with no
	// prometheus section produces exactly the exposition it always has -- which
	// is the promise this whole feature is built on. A disabled exporter has no
	// health to report anyway: it is not failing, it is not running.
	if !st.Enabled {
		return
	}
	m.Gauge("sysmon_remote_write_enabled",
		"1 when this node is configured to push its metrics to Prometheus.", metrics.Bool01(st.Enabled))
	m.Gauge("sysmon_remote_write_buffered_samples",
		"Samples held in memory awaiting a successful push.", float64(st.BufferedSamples))
	m.Gauge("sysmon_remote_write_consecutive_failures",
		"Pushes that have failed in a row since the last success.", float64(st.ConsecutiveFailures))
	m.Gauge("sysmon_remote_write_last_success_timestamp_seconds",
		"When the last push succeeded. 0 if none ever has.", float64(st.LastSuccessUnix))
	m.Gauge("sysmon_remote_write_samples_written_total_current",
		"Samples accepted by the remote-write endpoint since start.", float64(st.SamplesWritten))
	m.Gauge("sysmon_remote_write_samples_dropped_total_current",
		"Samples discarded: rejected permanently, or aged out of a full buffer.", float64(st.SamplesDropped))
}
