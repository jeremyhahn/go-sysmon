//go:build !prometheus

package server

import "github.com/jeremyhahn/go-sysmon/pkg/metrics"

// writeExporterMetrics emits nothing in a build with no prometheus support.
//
// The exposition is byte-for-byte what a node with no remote_write_url
// produces, which is what it has always been: these series describe the
// exporter's health, and there is no exporter to have any (#100).
func (s *Server) writeExporterMetrics(_ *metrics.TextWriter) {}
