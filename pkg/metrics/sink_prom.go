//go:build prometheus

// SampleSink is the remote-write half of this package, and it is the only part
// that needs pkg/prom. Behind the build tag so a binary built without
// prometheus support does not link the writer, the protobuf encoder or snappy
// (#100): the realtime desktop and systray builds carry none of it.
package metrics

import (
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
)

type SampleSink struct {
	Builder *prom.Builder
	TimeMs  int64
	// Instance names the node the samples describe, when that is not the node
	// doing the writing. The aggregator writes its peers' readings, and a
	// series with the aggregator's own instance label would merge ten machines
	// into one history that no later query could untangle. Empty means "this
	// node", and the Builder's common labels apply unchanged.
	Instance string
}

// Gauge implements Sink. The help text is dropped: remote write carries samples
// and labels, and Prometheus takes its HELP from the scrape endpoint.
func (s SampleSink) Gauge(name, _ string, value float64, labels ...string) {
	if s.Instance != "" {
		// Copied rather than appended in place: labels is the caller's backing
		// array, and appending into spare capacity would write a label into a
		// slice the caller still owns.
		//
		// Builder.Add resolves a repeated label in favour of the metric's own,
		// so this displaces the common instance rather than producing a series
		// Prometheus would reject.
		stamped := make([]string, 0, len(labels)+2)
		stamped = append(stamped, labels...)
		labels = append(stamped, "instance", s.Instance)
	}
	s.Builder.Add(name, value, s.TimeMs, labels...)
}
