//go:build integration

package prom

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// aggregatorConfig is an aggregator named agg polling peers every second and
// pushing to url. With no query_url, the database's lookback is asked of the
// server remote write goes to.
func aggregatorConfig(url string, peers map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fleet:\n  local_name: agg\n  poll_interval_ms: 1000\n  timeout_ms: 2000\n  peers:\n")
	for name, peer := range peers {
		fmt.Fprintf(&b, "    - name: %s\n      url: %s\n", name, peer)
	}
	fmt.Fprintf(&b, "prometheus:\n  remote_write_url: %q\n  sample_interval_ms: 1000\n  flush_interval_ms: 1000\n", url)
	return b.String()
}

// sampleAt returns the value of the one sample of the series matching want
// at ms, and whether there is one.
func sampleAt(db *tsdb, want map[string]string, ms int64) (float64, bool) {
	for _, s := range db.matching(want) {
		for _, p := range s.samples {
			if p.ms == ms {
				return p.value, true
			}
		}
	}
	return 0, false
}

// TestAggregator_WritesItsPeersHistory runs an aggregator over a peer whose
// hardware the devcontainer does not have, and checks the peer's readings
// arrive in the database under the peer's own name, dated by the peer's
// clock, with every kind of series the node describes itself with.
func TestAggregator_WritesItsPeersHistory(t *testing.T) {
	db := newTSDB(t)
	taken := time.Now().Add(-2 * time.Second).Truncate(time.Millisecond)
	peer := newPeerNode(t, gpuBox("gpu-box", taken), 1000)
	agg := serveServer(t, aggregatorConfig(db.writeURL(), map[string]string{"gpu-box": peer.srv.URL}))

	at := taken.UnixMilli()
	db.await(t, "the peer's reading to be stored", func(db *tsdb) bool {
		_, ok := sampleAt(db, labelsOf("__name__", "sysmon_up", "instance", "gpu-box"), at)
		return ok
	})

	db.mu.Lock()
	for _, c := range []struct {
		labels []string
		want   float64
	}{
		{[]string{"__name__", "sysmon_gpu_utilization_percent", "gpu", "0"}, 73},
		{[]string{"__name__", "sysmon_power_watts", "source", "cpu:package-0"}, 55.5},
		{[]string{"__name__", "sysmon_power_watts", "source", "gpu:0"}, 180},
		{[]string{"__name__", "sysmon_power_watts", "source", "npu:0"}, 4},
		{[]string{"__name__", "sysmon_npu_counters_stale", "npu", "0"}, 1},
		{[]string{"__name__", "sysmon_cpu_temperature_celsius", "core", "0"}, 61},
		{[]string{"__name__", "sysmon_disk_wear_percent", "device", "nvme0n1"}, 3},
		{[]string{"__name__", "sysmon_disk_temperature_celsius", "device", "nvme0n1"}, 41},
		{[]string{"__name__", "sysmon_disk_member_temperature_celsius", "serial", "WD-0001", "volume", "sda"}, 35},
		{[]string{"__name__", "sysmon_raid_degraded", "array", "md0", "level", "raid1"}, 1},
		{[]string{"__name__", "sysmon_raid_sync_percent", "array", "md0"}, 12.5},
		{[]string{"__name__", "sysmon_network_receive_bytes_rate", "interface", "eth0"}, 1000},
		{[]string{"__name__", "sysmon_network_up", "interface", "eth1"}, 0},
		{[]string{"__name__", "sysmon_alert", "rule", "raid_degraded", "severity", "critical"}, 1},
		{[]string{"__name__", "sysmon_alerts_firing", "severity", "critical"}, 1},
		{[]string{"__name__", "sysmon_swap_in_pages_total"}, 10},
	} {
		want := labelsOf(append(c.labels, "instance", "gpu-box", "job", "go-sysmon")...)
		if got, ok := sampleAt(db, want, at); !ok || got != c.want {
			t.Errorf("%v at the peer's reading = %v (stored %v), want %v", c.labels, got, ok, c.want)
		}
	}
	if lo := db.matching(labelsOf("instance", "gpu-box", "interface", "lo")); len(lo) != 0 {
		t.Errorf("the peer's loopback interface was written: %d series", len(lo))
	}
	// The volume's second drive reports no serial, and is not written: a
	// series keyed by an empty serial would merge every such drive into one.
	members := db.matching(labelsOf("__name__", "sysmon_disk_member_temperature_celsius", "instance", "gpu-box"))
	if len(members) != 1 {
		t.Errorf("member temperature series = %d, want only the drive with a serial", len(members))
	}
	db.mu.Unlock()

	await.Until(t, "the aggregator to say it records gpu-box", func(context.Context) bool {
		state, _ := historyState(t, agg, "gpu-box")
		return state == "recorded"
	})
}

// TestAggregator_RefusesReadingsItCannotDateTruthfully walks a peer's clock
// backwards, then forwards, then away altogether: readings older than one
// already written, and readings with no time, are dropped and said so once;
// the peer's history resumes by itself once its clock passes the newest.
func TestAggregator_RefusesReadingsItCannotDateTruthfully(t *testing.T) {
	db := newTSDB(t)
	taken := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	peer := newPeerNode(t, gpuBox("gpu-box", taken), 1000)
	agg := serveServer(t, aggregatorConfig(db.writeURL(), map[string]string{"gpu-box": peer.srv.URL}))
	up := labelsOf("__name__", "sysmon_up", "instance", "gpu-box")
	db.await(t, "the first reading", func(db *tsdb) bool { _, ok := sampleAt(db, up, taken.UnixMilli()); return ok })

	peer.setTimestamp(taken.Add(-time.Hour))
	rec := agg.Logs.AwaitMsg(t, "prom: a clock stepped backwards; its readings are dropped until it passes the newest one written")
	if rec["source"] != "gpu-box" {
		t.Errorf("the step was logged for %v, want gpu-box", rec["source"])
	}

	later := taken.Add(time.Second)
	peer.setTimestamp(later)
	agg.Logs.AwaitMsg(t, "prom: a source's readings are being written again")
	db.await(t, "the reading after the clock recovered", func(db *tsdb) bool {
		_, ok := sampleAt(db, up, later.UnixMilli())
		return ok
	})

	peer.setTimestamp(time.Time{})
	rec = agg.Logs.AwaitMsg(t, "prom: a source sends readings with no collection time; they are dropped")
	if rec["source"] != "gpu-box" {
		t.Errorf("the undated readings were logged for %v, want gpu-box", rec["source"])
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if times := db.sampleTimes(up); times[0] != taken.UnixMilli() || times[len(times)-1] != later.UnixMilli() {
		t.Errorf("stored readings %v, want only %d and %d: a stepped-back or undated one was written",
			times, taken.UnixMilli(), later.UnixMilli())
	}
}

// TestAggregator_GatesPeersItCannotChart verifies a peer collecting too
// slowly for its history to be charted is refused -- not written, and the
// card says why -- and that a peer too old to report its interval is written
// but marked unverified.
func TestAggregator_GatesPeersItCannotChart(t *testing.T) {
	db := newTSDB(t)
	now := time.Now().Truncate(time.Millisecond)
	slow := newPeerNode(t, gpuBox("slow-box", now), (10 * time.Minute).Milliseconds())
	old := newPeerNode(t, gpuBox("old-box", now), 0)
	// An interval no duration can hold: refused, not overflowed into a small
	// one that would pass.
	vast := newPeerNode(t, gpuBox("vast-box", now), math.MaxInt64/2)
	agg := serveServer(t, aggregatorConfig(db.writeURL(),
		map[string]string{"slow-box": slow.srv.URL, "old-box": old.srv.URL, "vast-box": vast.srv.URL}))

	for name, want := range map[string]struct{ state, reason string }{
		"slow-box": {"refused", "past the time-series database's 5m0s query lookback"},
		"vast-box": {"refused", "too long for any history to be charted"},
		"old-box":  {"unverified", "does not report its collection interval"},
	} {
		var reason string
		await.Until(t, "the aggregator's history decision for "+name, func(context.Context) bool {
			var state string
			state, reason = historyState(t, agg, name)
			return state == want.state
		})
		if !strings.Contains(reason, want.reason) {
			t.Errorf("%s is %s because %q, want %q", name, want.state, reason, want.reason)
		}
	}
	db.await(t, "old-box's reading to be written", func(db *tsdb) bool { return readings(db, "old-box") >= 1 })

	// Both peers take a new reading. old-box's is written; slow-box's, now
	// that it is refused, is not even fetched.
	db.mu.Lock()
	before := readings(db, "slow-box")
	db.mu.Unlock()
	next := now.Add(time.Second)
	slow.setTimestamp(next)
	old.setTimestamp(next)
	db.await(t, "old-box's next reading", func(db *tsdb) bool {
		_, ok := sampleAt(db, labelsOf("__name__", "sysmon_up", "instance", "old-box"), next.UnixMilli())
		return ok
	})
	db.mu.Lock()
	defer db.mu.Unlock()
	if after := readings(db, "slow-box"); after != before {
		t.Errorf("slow-box, refused, had %d readings written and now has %d", before, after)
	}
}

// historyQuery asks the aggregator for a range of one metric on one node.
func historyQuery(t *testing.T, s *harness.Server, metric, node string, start, end time.Time, extra string) (int, []byte) {
	t.Helper()
	url := fmt.Sprintf("/api/history?metric=%s&start=%d&end=%d%s", metric, start.UnixMilli(), end.UnixMilli(), extra)
	if node != "" {
		url += "&node=" + node
	}
	return harness.Get(t, s.URL(url))
}

// TestAggregator_ATwoSocketPeersCoresAreDistinctSeries relays a two-socket
// peer, whose packages both have a Core 0. Each is written as its own series,
// told apart by package, and no write is refused. With core alone labelling
// them, both sockets' Core 0 was one series given two samples at one time: a
// duplicate the receiver refused together with the whole batch, which is how
// a two-socket CI runner lost every write the suite made.
func TestAggregator_ATwoSocketPeersCoresAreDistinctSeries(t *testing.T) {
	db := newTSDB(t)
	taken := time.Now().Add(-2 * time.Second).Truncate(time.Millisecond)
	snap := gpuBox("dual", taken)
	snap.Sensors.CoreTemps = []types.CoreTemp{
		{PackageID: 0, CoreID: 0, Label: "Core 0", TempCelsius: 61},
		{PackageID: 1, CoreID: 0, Label: "Core 0", TempCelsius: 67},
	}
	peer := newPeerNode(t, snap, 1000)
	serveServer(t, aggregatorConfig(db.writeURL(), map[string]string{"dual": peer.srv.URL}))

	at := taken.UnixMilli()
	db.await(t, "the peer's reading to be stored", func(db *tsdb) bool {
		_, ok := sampleAt(db, labelsOf("__name__", "sysmon_up", "instance", "dual"), at)
		return ok
	})

	db.mu.Lock()
	defer db.mu.Unlock()
	for pkg, want := range map[string]float64{"0": 61, "1": 67} {
		labels := labelsOf("__name__", "sysmon_cpu_temperature_celsius", "instance", "dual", "package", pkg, "core", "0")
		if got, ok := sampleAt(db, labels, at); !ok || got != want {
			t.Errorf("package %s core 0 = %v (stored %v), want %v", pkg, got, ok, want)
		}
	}
	for i, w := range db.writes {
		if w.status/100 != 2 {
			t.Errorf("write %d answered %d: %s", i, w.status, w.rejected)
		}
	}
}
