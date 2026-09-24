//go:build prometheus && !desktop

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// collectedAt is when the test snapshots say they were taken.
var collectedAt = time.UnixMilli(1788300000123)

// datedStub is a Snapshotter whose every snapshot carries the same collection
// time, as the collector would if it were asked for one twice between
// collections.
type datedStub struct{}

func (datedStub) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{Timestamp: collectedAt, Host: types.HostInfo{Hostname: "mon0"}}, nil
}

// written runs a measurement's Write into a fresh builder and returns what it
// produced, so a test sees the samples the exporter would batch.
func written(t *testing.T, m prom.Measurement) []prom.Series {
	t.Helper()
	if m.Write == nil {
		t.Fatal("the measurement has nothing to write")
	}
	b := prom.NewBuilder([]prom.Label{{Name: "instance", Value: "aggregator"}})
	m.Write(b, m.TimeMs)
	series := b.Drain()
	if len(series) == 0 {
		t.Fatal("the measurement wrote no samples")
	}
	return series
}

// instanceOf returns a series' instance label.
func instanceOf(s prom.Series) string {
	for _, l := range s.Labels {
		if l.Name == "instance" {
			return l.Value
		}
	}
	return ""
}

// TestSnapshotSamples_DatesTheReadingByItsCollection: the local node's reading
// carries the time the collector took it, not the time the exporter asked, and
// asking twice between collections offers the same reading -- which is how the
// exporter knows not to write it again (#173).
func TestSnapshotSamples_DatesTheReadingByItsCollection(t *testing.T) {
	mon, err := monitor.New(datedStub{}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := mon.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	waitSnapshot(t, mon)

	collect := snapshotSamples(mon)
	first, err := collect()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	second, err := collect()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if first.TimeMs != collectedAt.UnixMilli() || second.TimeMs != first.TimeMs {
		t.Fatalf("readings dated %d and %d, want both %d, the collection time",
			first.TimeMs, second.TimeMs, collectedAt.UnixMilli())
	}
	if first.Source != "" {
		t.Errorf("Source = %q, want \"\" for this node", first.Source)
	}
	for _, s := range written(t, first) {
		if got := instanceOf(s); got != "aggregator" {
			t.Errorf("a local series was labelled instance=%q, want the node's own", got)
		}
		for _, sample := range s.Samples {
			if sample.TimeMs != collectedAt.UnixMilli() {
				t.Fatalf("a sample was stamped %d, want the collection time %d", sample.TimeMs, collectedAt.UnixMilli())
			}
		}
	}
}

// TestSnapshotSamples_NoReadingYet: a monitor with nothing collected offers
// nothing, and says why.
func TestSnapshotSamples_NoReadingYet(t *testing.T) {
	mon, err := monitor.New(datedStub{}, time.Hour)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	var notRunning *types.MonitorNotRunningError
	if _, err := snapshotSamples(mon)(); !errors.As(err, &notRunning) {
		t.Fatalf("collect on a stopped monitor = %v, want MonitorNotRunningError", err)
	}
}

// TestPeerSamples_OffersEachPollDatedByThePeer: every poll is offered, under
// the peer's name and dated by the peer's collection time, so a peer polled
// twice without collecting offers the same reading twice and the exporter
// writes it once. The aggregator's clock appears nowhere.
func TestPeerSamples_OffersEachPollDatedByThePeer(t *testing.T) {
	var offered []prom.Measurement
	observe := peerSamples(func(m prom.Measurement) bool {
		offered = append(offered, m)
		return true
	})

	snap := &types.Snapshot{Timestamp: collectedAt}
	observe("omega", snap)
	observe("omega", snap)

	if len(offered) != 2 {
		t.Fatalf("offered %d readings for 2 polls, want 2", len(offered))
	}
	for _, m := range offered {
		if m.Source != "omega" || m.TimeMs != collectedAt.UnixMilli() {
			t.Fatalf("offered Source=%q TimeMs=%d, want omega at %d", m.Source, m.TimeMs, collectedAt.UnixMilli())
		}
		for _, s := range written(t, m) {
			if got := instanceOf(s); got != "omega" {
				t.Errorf("a peer series was labelled instance=%q, want omega", got)
			}
		}
	}
}

// TestPeerSamples_IgnoresNothingToRecord: no snapshot, or no name to file it
// under, offers nothing.
func TestPeerSamples_IgnoresNothingToRecord(t *testing.T) {
	calls := 0
	observe := peerSamples(func(prom.Measurement) bool {
		calls++
		return true
	})
	observe("omega", nil)
	observe("", &types.Snapshot{Timestamp: collectedAt})
	if calls != 0 {
		t.Fatalf("offered %d readings, want none", calls)
	}
}

// TestHistoryCadence_FollowsRemoteWrite: the fleet poller fetches each peer's
// history payload at this cadence, and not at all at zero. The question must
// be answered from remote_write_url -- the same setting the exporter keeps or
// discards a submission by -- and the cadence is the exporter's sample
// interval, which its per-source de-duplication makes safe to fetch at (#173).
func TestHistoryCadence_FollowsRemoteWrite(t *testing.T) {
	cases := map[string]struct {
		body string
		want time.Duration
	}{
		"remote write configured":        {"prometheus:\n  remote_write_url: \"http://prom.example:9090/api/v1/write\"\n  sample_interval_ms: 5000\n", 5 * time.Second},
		"remote write, default interval": {"prometheus:\n  remote_write_url: \"http://prom.example:9090/api/v1/write\"\n", 15 * time.Second},
		"no prometheus section":          {"server:\n  addr: \":8081\"\n", 0},
		"query only":                     {"prometheus:\n  query_url: \"http://prom.example:9090\"\n", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			withServeConfig(t, tc.body)
			mgr, err := loadServeConfig(nil)
			if err != nil {
				t.Fatalf("loadServeConfig(): %v", err)
			}
			if got := historyCadence(mgr)(); got != tc.want {
				t.Errorf("historyCadence() = %v, want %v", got, tc.want)
			}
		})
	}
}
