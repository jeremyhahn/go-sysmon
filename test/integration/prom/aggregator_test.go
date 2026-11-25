//go:build integration

package prom

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
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

// droppedMetric is the scrape's count of samples remote write has discarded.
const droppedMetric = "sysmon_remote_write_samples_dropped_total_current"

// TestAggregator_ADriveListedTwiceCostsItsSecondSampleNotTheBatch relays a
// peer whose RAID volume lists one drive twice, so every reading gives each
// of that drive's two series a second sample at the same time. The first
// sample stands and the rest of the batch is delivered, each second sample is
// counted as dropped, and the defect is reported once per series however many
// readings repeat it.
func TestAggregator_ADriveListedTwiceCostsItsSecondSampleNotTheBatch(t *testing.T) {
	db := newTSDB(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)
	snap := gpuBox("gpu-box", t0)
	snap.Disks[1].Members = []types.DiskMember{
		{Serial: "WD-0001", Temperature: 35, SMARTEnabled: true, SMARTHealthy: true},
		{Serial: "WD-0001", Temperature: 44, SMARTEnabled: true, SMARTHealthy: false},
	}
	peer := newPeerNode(t, snap, 1000)
	config := aggregatorConfig(db.writeURL(), map[string]string{"gpu-box": peer.srv.URL})
	agg := serveNode(t, config)

	const taken = 3
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	up := labelsOf("__name__", "sysmon_up", "instance", "gpu-box")
	for i := range taken {
		peer.setTimestamp(at(i))
		db.await(t, fmt.Sprintf("reading %d to be stored", i), func(db *tsdb) bool {
			_, ok := sampleAt(db, up, at(i).UnixMilli())
			return ok
		})
	}

	db.mu.Lock()
	for i, w := range db.writes {
		if w.status/100 != 2 {
			t.Errorf("write %d answered %d: %s", i, w.status, w.rejected)
		}
	}
	for name, want := range map[string]float64{
		"sysmon_disk_member_temperature_celsius": 35, "sysmon_disk_member_smart_healthy": 1,
	} {
		drive := labelsOf("__name__", name, "instance", "gpu-box", "serial", "WD-0001", "volume", "sda")
		for i := range taken {
			if got, ok := sampleAt(db, drive, at(i).UnixMilli()); !ok || got != want {
				t.Errorf("%s at reading %d = %v (stored %v), want the %v of the drive's first listing", name, i, got, ok, want)
			}
		}
	}
	db.mu.Unlock()

	if dropped, ok := metricValue(scrape(t, agg.Server), droppedMetric); !ok || dropped != 2*taken {
		t.Errorf("%s = %v (present %v), want the second sample of two series in each of %d readings",
			droppedMetric, dropped, ok, taken)
	}
	var reported []string
	for _, rec := range agg.logged(t, config, "prom: a reading gave one series two samples at the same time; the second is dropped") {
		if rec["source"] != "gpu-box" {
			t.Errorf("the collision was reported for %v, want gpu-box", rec["source"])
		}
		reported = append(reported, fmt.Sprint(rec["series"]))
	}
	slices.Sort(reported)
	if len(reported) != 2 || !strings.Contains(reported[0], "sysmon_disk_member_smart_healthy") ||
		!strings.Contains(reported[1], "sysmon_disk_member_temperature_celsius") {
		t.Errorf("collisions reported over %d readings = %q, want each of the drive's two series once", taken, reported)
	}
}

// TestAggregator_AStalledReceiverDropsAndCountsWhatItCannotQueue stalls the
// receiver mid-write under an aggregator of many peers. Nothing is taken from
// the queue of peer readings while the write is out, so the queue fills; the
// readings past it are dropped and counted rather than held up, and history
// resumes once the receiver answers.
func TestAggregator_AStalledReceiverDropsAndCountsWhatItCannotQueue(t *testing.T) {
	db := newTSDB(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)
	const peerCount = 64
	peers := make(map[string]string, peerCount)
	var first *peerNode
	for i := range peerCount {
		name := fmt.Sprintf("node-%02d", i)
		peer := newPeerNode(t, gpuBox(name, t0), 1000)
		peers[name] = peer.srv.URL
		if i == 0 {
			first = peer
		}
	}
	agg := serveServer(t, aggregatorConfig(db.writeURL(), peers)+"  timeout_ms: 20000\n")
	db.await(t, "every peer's first reading to be stored", func(db *tsdb) bool {
		return len(db.matching(labelsOf("__name__", "sysmon_up"))) == peerCount+1
	})
	if dropped, ok := metricValue(scrape(t, agg), droppedMetric); !ok || dropped != 0 {
		t.Fatalf("%s = %v (present %v) before the receiver stalled, want 0", droppedMetric, dropped, ok)
	}

	held, release := make(chan struct{}, 1), make(chan struct{})
	db.set(func(db *tsdb) {
		db.writeHook = func(w http.ResponseWriter, r *http.Request) {
			select {
			case held <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	await.Recv(t, "a write to be held by the receiver", held)
	awaitScrape(t, agg, "the readings the queue has no room for to be counted as dropped", func(e string) bool {
		dropped, _ := metricValue(e, droppedMetric)
		return dropped > 0
	})

	db.set(func(db *tsdb) { db.writeHook = nil })
	close(release)
	later := t0.Add(time.Second)
	first.setTimestamp(later)
	db.await(t, "a reading taken after the stall to be stored", func(db *tsdb) bool {
		_, ok := sampleAt(db, labelsOf("__name__", "sysmon_up", "instance", "node-00"), later.UnixMilli())
		return ok
	})
}
