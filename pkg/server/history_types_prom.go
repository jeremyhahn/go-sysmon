//go:build prometheus

package server

import "github.com/jeremyhahn/go-sysmon/pkg/prom"

// HistoryQuerier reads date ranges back out of Prometheus.
//
// An alias rather than an interface: with the tag set this IS the querier, and
// naming it here is what lets Config carry the same field in both builds
// without pkg/server's public shape changing with a build flag (#100).
type HistoryQuerier = *prom.Querier

// MetricsExporter pushes samples to Prometheus.
type MetricsExporter = *prom.Exporter
