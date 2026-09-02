// Package prom connects go-sysmon to Prometheus.
//
// It provides the two halves of a time-series integration that the scrape
// endpoint in pkg/server cannot: a remote-write client, so a node pushes its
// own history into a Prometheus that need not be able to reach it, and a range
// query client, so the dashboard can draw a date range instead of the last
// sixty seconds held in the browser.
//
// The whole package is optional. With no remote_write_url configured nothing
// here runs, and the node behaves exactly as it always has: real time only,
// with no dependency on a Prometheus existing anywhere.
//
// # Why each node pushes its own metrics
//
// The obvious alternative -- have the fleet aggregator store what it already
// polls -- makes one machine a time-series database whose cost grows with
// nodes x metrics x retention, and adds that load to a host chosen for being
// small. Pushing from each node is constant cost per node and adds nothing to
// the existing ones as the fleet grows. Fleet polling stays what it is: an
// answer to "is this healthy right now", never to "what happened on Tuesday".
//
// # Why the protobuf is written by hand
//
// Remote write v1 is snappy-compressed protobuf, and the canonical schema
// lives in github.com/prometheus/prometheus/prompb. Importing it brings the
// whole Prometheus server module: measured at 257 modules in the build graph
// against this repository's 137. pkg/server/metrics.go already declined the
// Prometheus client library on exactly those grounds, hand-writing the four-
// line text exposition format instead, and pulling the far larger dependency
// in one file over would contradict that decision rather than revisit it.
//
// The schema being encoded is four messages and six fields, fixed by the
// remote-write specification rather than by any library version:
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; }
//	message TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label        { string name = 1; string value = 2; }
//	message Sample       { double value = 1; int64 timestamp = 2; }
//
// encode_test.go pins the output against golden bytes produced by the real
// prompb encoder, so the oracle is the library rather than a second copy of
// this code.
package prom

// Label is one name/value pair on a series. The metric name travels as the
// label __name__, which is what remote write expects; there is no separate
// name field.
type Label struct {
	Name  string
	Value string
}

// Sample is one value at one instant, the instant being Unix milliseconds as
// remote write specifies.
type Sample struct {
	Value  float64
	TimeMs int64
}

// Series is a label set and every sample recorded against it.
//
// Labels must be sorted by name and must not repeat: Prometheus identifies a
// series by its label set, and an unsorted or duplicated set is rejected at
// ingest -- rejecting the whole request, so one bad series loses every other
// sample sent with it. Builder sorts and de-duplicates; a caller constructing a
// Series directly is responsible for both.
type Series struct {
	Labels  []Label
	Samples []Sample
}
