//go:build !prometheus

package main

import (
	"context"

	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
)

// startExporter and newHistoryQuerier are the seam for a build with no
// prometheus support: the server is constructed the same way, with nothing
// behind the two optional fields (#100).
//
// Returning nil rather than a no-op implementation is deliberate. The server
// already treats a nil exporter and a nil querier as "this node keeps no
// history" -- that is what a node with no prometheus section has always been --
// so this build takes a path the code has been exercising since the feature
// shipped, rather than a second one written for it.
func startExporter(_ *config.Manager, _ *monitor.Monitor, _ <-chan struct{}) server.MetricsExporter {
	return nil
}

// newHistoryQuerier returns nothing to query.
func newHistoryQuerier(_ *config.Manager) server.HistoryQuerier {
	return nil
}

// observePeers does nothing here: with no exporter there is nowhere for a
// peer's reading to be written, and observing them would cost a callback per
// peer per poll to discard the result.
func observePeers(_ *config.Manager, _ *fleet.Poller, _ server.MetricsExporter, _ server.HistoryQuerier) {
}

// startHistoryCadenceWatch has nothing to watch here: this build writes no
// history.
func startHistoryCadenceWatch(_ context.Context, _ *config.Manager, _ server.HistoryQuerier,
	_ *alert.Evaluator, _ *serviceStatus) error {
	return nil
}
