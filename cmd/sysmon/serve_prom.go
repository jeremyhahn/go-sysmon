//go:build prometheus

// The remote-write and history wiring. Everything here needs pkg/prom, and a
// build without the tag carries none of it (#100).
package main

import (
	"log/slog"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/metrics"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// jobLabel is the Prometheus job every pushed series carries. A scrape gets its
// job from the scrape config; a pushed sample has to bring one, and giving them
// all the same value is what lets one query cover the whole fleet.
const jobLabel = "go-sysmon"

// promOptions reads the exporter's settings from the config manager on every
// cycle, so remote write can be enabled, retargeted or switched off by
// reloading rather than restarting.
//
// The instance label is the node's fleet name, not its hostname: it is the name
// the fleet view already shows and the name the history API selects by, so a
// node relabelled in one place cannot become two identities in the other.
func promOptions(mgr *config.Manager) func() prom.Options {
	return func() prom.Options {
		p := mgr.Current().Prometheus
		labels := []prom.Label{
			{Name: "instance", Value: localNodeName(mgr)},
			{Name: "job", Value: jobLabel},
		}
		// Operator labels cannot displace the two above. Letting an
		// external_labels entry override instance would silently merge this
		// node's history with another machine's, which no later query could
		// untangle.
		for name, value := range p.ExternalLabels {
			if name == "instance" || name == "job" || name == "__name__" {
				continue
			}
			labels = append(labels, prom.Label{Name: name, Value: value})
		}
		return prom.Options{
			RemoteWriteURL: p.RemoteWriteURL,
			SampleInterval: p.SampleInterval(),
			FlushInterval:  p.FlushInterval(),
			BatchSize:      p.BatchSize,
			Timeout:        p.Timeout(),
			Labels:         labels,
		}
	}
}

// promQueryOptions reads the history endpoint's settings the same way.
func promQueryOptions(mgr *config.Manager) func() prom.QueryOptions {
	return func() prom.QueryOptions {
		p := mgr.Current().Prometheus
		return prom.QueryOptions{BaseURL: p.QueryURL, Timeout: p.Timeout()}
	}
}

// snapshotSamples is the exporter's collect step: the same description of a
// snapshot that GET /metrics renders, stamped with one instant and handed to
// the remote-write builder.
// peerSamples writes a peer's reading into the exporter's current batch, under
// that peer's own instance label and stamped with the time the peer collected
// it.
//
// Two properties are deliberate. The timestamp is the READING's, not the
// aggregator's clock, so a peer's history says when the peer measured rather
// than when this node heard about it. And there is no cadence here at all: one
// reading in, one set of samples out, at whatever rate the fleet poller runs.
// Resampling to the exporter's own interval would either repeat a reading that
// happened once or drop readings that happened -- inventing observations in the
// first case, hiding them in the second.
func peerSamples(exporter *prom.Exporter) fleet.Reading {
	return func(node string, snap *types.Snapshot) {
		if snap == nil || node == "" {
			return
		}
		timeMs := snap.Timestamp.UnixMilli()
		exporter.Submit(func(b *prom.Builder) {
			metrics.Write(metrics.SampleSink{Builder: b, TimeMs: timeMs, Instance: node}, snap)
		})
	}
}

func snapshotSamples(mon *monitor.Monitor) prom.Collect {
	return func(b *prom.Builder, timeMs int64) error {
		snap, err := mon.Snapshot()
		if err != nil {
			return err
		}
		metrics.Write(metrics.SampleSink{Builder: b, TimeMs: timeMs}, snap)
		return nil
	}
}

// startExporter builds the remote-write exporter and runs it.
//
// The exporter runs whether or not remote_write_url is configured, exactly as
// the fleet poller runs whether or not there are peers: with no endpoint it
// collects nothing, buffers nothing and makes no request. What that buys is
// that turning history on is a config reload rather than a restart, which would
// drop every connected browser.
func startExporter(mgr *config.Manager, mon *monitor.Monitor, done <-chan struct{}) server.MetricsExporter {
	exporter := prom.NewExporter(promOptions(mgr), snapshotSamples(mon), slog.Default())
	go exporter.Run(done)
	return exporter
}

// newHistoryQuerier builds the reader for date-range queries.
func newHistoryQuerier(mgr *config.Manager) server.HistoryQuerier {
	return prom.NewQuerier(promQueryOptions(mgr))
}

// observePeers wires the fleet poller's readings into the exporter, so the
// aggregator persists its peers' history (topology A, #96).
func observePeers(poller *fleet.Poller, exporter server.MetricsExporter) {
	if poller == nil || exporter == nil {
		return
	}
	poller.Observe(peerSamples(exporter))
}
