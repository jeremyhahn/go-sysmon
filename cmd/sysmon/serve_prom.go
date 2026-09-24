//go:build prometheus

// The remote-write and history wiring. Everything here needs pkg/prom, and a
// build without the tag carries none of it (#100).
package main

import (
	"log/slog"
	"time"

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

// peerSamples offers each peer reading to the exporter as it arrives, under
// that peer's own instance label and dated by the time the peer collected it.
//
// Three properties are deliberate. The timestamp is the READING's, taken from
// the peer's clock, not this node's: a peer's history says when the peer
// measured rather than when this node heard about it, and a rate() taken over
// it divides by the peer's own collection intervals, which a receive time would
// jitter by up to a poll interval. The source is the peer's name, so the
// exporter orders each peer's readings against its own clock only -- skew
// between two machines never makes one of them look out of order. And there is
// no cadence here: every poll is offered, and the exporter writes the reading
// only if the peer has collected since the last one it wrote (#173). A peer
// polled faster than it collects therefore contributes each reading once,
// neither repeated under new timestamps nor thinned to the exporter's own
// interval.
func peerSamples(submit func(prom.Measurement) bool) fleet.Reading {
	return func(node string, snap *types.Snapshot) {
		if snap == nil || node == "" {
			return
		}
		submit(prom.Measurement{
			Source: node,
			TimeMs: snap.Timestamp.UnixMilli(),
			Write: func(b *prom.Builder, timeMs int64) {
				metrics.Write(metrics.SampleSink{Builder: b, TimeMs: timeMs, Instance: node}, snap)
			},
		})
	}
}

// snapshotSamples is the exporter's collect step: the monitor's latest
// snapshot, described exactly as GET /metrics renders it and dated by the
// time the collector took it.
//
// The date is what keeps the history honest when the dashboard collects more
// slowly than the exporter samples. The monitor caches one snapshot between
// collections; offered at every sample tick, it carries the same time each
// time, and the exporter writes it only once (#173).
func snapshotSamples(mon *monitor.Monitor) prom.Collect {
	return func() (prom.Measurement, error) {
		snap, err := mon.Snapshot()
		if err != nil {
			return prom.Measurement{}, err
		}
		return prom.Measurement{
			TimeMs: snap.Timestamp.UnixMilli(),
			Write: func(b *prom.Builder, timeMs int64) {
				metrics.Write(metrics.SampleSink{Builder: b, TimeMs: timeMs}, snap)
			},
		}, nil
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
//
// History is fetched by the poller's own per-peer workers at historyCadence,
// apart from the summary poll: with remote write off the fleet view needs
// nothing beyond the peers' summaries, and with it on a slow history endpoint
// must never hold back a card.
//
// Each reading passes the history gate first (peerHistoryGate): a peer that
// collects too slowly for its history to be charted is not written, and the
// fleet card says so.
func observePeers(mgr *config.Manager, poller *fleet.Poller, exporter server.MetricsExporter,
	history server.HistoryQuerier) {
	if poller == nil || exporter == nil {
		return
	}
	poller.GateHistory(peerHistoryGate(mgr, history))
	poller.Observe(peerSamples(exporter.Submit), historyFetchCadence(mgr))
}

// historyFetchCadence is how often the fleet poller fetches each peer's history
// payload: the exporter's sample interval while this node writes history, and
// zero -- record nothing, fetch nothing -- while it does not. It is read from
// the current configuration, so turning remote write on or off, or retiming
// it, is a reload. Fetching at the sample interval cannot double-write: the
// exporter writes a peer's reading only when the peer has collected since the
// last one it wrote (#173).
func historyFetchCadence(mgr *config.Manager) func() time.Duration {
	return func() time.Duration {
		p := mgr.Current().Prometheus
		if p.RemoteWriteURL == "" {
			return 0
		}
		return p.SampleInterval()
	}
}
