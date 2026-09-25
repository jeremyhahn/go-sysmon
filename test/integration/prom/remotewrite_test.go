//go:build integration

package prom

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// writeConfig is a node named name pushing to url every second.
func writeConfig(name, url, extra string) string {
	return fmt.Sprintf("fleet:\n  local_name: %s\nprometheus:\n  remote_write_url: %q\n"+
		"  sample_interval_ms: 1000\n  flush_interval_ms: 1000\n%s", name, url, extra)
}

// readings is how many distinct readings of name's sysmon_up are stored.
func readings(db *tsdb, name string) int {
	return len(db.sampleTimes(labelsOf("__name__", "sysmon_up", "instance", name)))
}

// TestRemoteWrite_PushesTheNodesOwnSamples runs a node pushing to the
// stand-in and checks what arrived: remote write v1 on the wire, every series
// labelled with the node's fleet name, the job and the operator's external
// labels, and nothing Prometheus would refuse.
func TestRemoteWrite_PushesTheNodesOwnSamples(t *testing.T) {
	db := newTSDB(t)
	s := serveServer(t, writeConfig("rack-7", db.writeURL(), "  external_labels:\n    site: dc1\n"))

	db.await(t, "two readings of rack-7 to be stored", func(db *tsdb) bool { return readings(db, "rack-7") >= 2 })

	db.mu.Lock()
	for i, w := range db.writes {
		if w.status/100 != 2 {
			t.Errorf("write %d answered %d: %s", i, w.status, w.rejected)
		}
		for header, want := range map[string]string{
			"Content-Encoding": "snappy", "Content-Type": "application/x-protobuf",
			"X-Prometheus-Remote-Write-Version": "0.1.0", "User-Agent": "go-sysmon",
		} {
			if got := w.header.Get(header); got != want {
				t.Errorf("write %d %s = %q, want %q", i, header, got, want)
			}
		}
	}
	for _, st := range db.matching(nil) {
		for k, want := range map[string]string{"instance": "rack-7", "job": "go-sysmon", "site": "dc1"} {
			if got := st.labels[k]; got != want {
				t.Errorf("series %s: %s = %q, want %q", seriesKey(st.labels), k, got, want)
			}
		}
	}
	up := db.matching(labelsOf("__name__", "sysmon_up"))
	loadSeries := db.matching(labelsOf("__name__", "sysmon_load1"))
	db.mu.Unlock()
	if len(up) != 1 || up[0].samples[0].value != 1 {
		t.Errorf("sysmon_up = %+v, want one series of 1", up)
	}
	if len(loadSeries) != 1 {
		t.Errorf("sysmon_load1 series = %d, want the node's load average", len(loadSeries))
	}

	exposition := awaitScrape(t, s, "the scrape endpoint to report a successful push", func(e string) bool {
		written, _ := metricValue(e, "sysmon_remote_write_samples_written_total_current")
		last, _ := metricValue(e, "sysmon_remote_write_last_success_timestamp_seconds")
		return written > 0 && last > 0
	})
	for name, want := range map[string]float64{
		"sysmon_remote_write_enabled": 1, "sysmon_remote_write_consecutive_failures": 0,
		"sysmon_remote_write_samples_dropped_total_current": 0,
	} {
		if got, ok := metricValue(exposition, name); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", name, got, ok, want)
		}
	}
}

// TestRemoteWrite_BatchSizeFlushesEarly verifies a full batch is sent
// without waiting for the flush interval: with an hour between flushes, only
// the batch size can explain a second and a third push.
func TestRemoteWrite_BatchSizeFlushesEarly(t *testing.T) {
	db := newTSDB(t)
	serveServer(t, fmt.Sprintf("fleet:\n  local_name: rack-7\nprometheus:\n  remote_write_url: %q\n"+
		"  sample_interval_ms: 1000\n  flush_interval_ms: 3600000\n  batch_size: 1\n", db.writeURL()))

	db.await(t, "three pushes inside the hour-long flush interval", func(db *tsdb) bool { return db.accepted() >= 3 })
}

// TestRemoteWrite_ServerErrorKeepsTheBatch verifies a receiver answering 503
// or 429 is sent the same samples again: nothing is lost to a receiver that
// is overloaded for a moment.
func TestRemoteWrite_ServerErrorKeepsTheBatch(t *testing.T) {
	db := newTSDB(t)
	db.set(func(db *tsdb) {
		db.respond = func(n int) writeAnswer {
			switch n {
			case 0:
				return writeAnswer{status: http.StatusServiceUnavailable, body: "overloaded"}
			case 1:
				return writeAnswer{status: http.StatusTooManyRequests, body: "slow down"}
			}
			return writeAnswer{}
		}
	})
	s := serveServer(t, writeConfig("rack-7", db.writeURL(), ""))

	s.Logs.AwaitMsg(t, "prom: remote write failed, keeping the batch for the next flush")
	db.await(t, "a push to be accepted", func(db *tsdb) bool { return db.accepted() >= 1 })

	db.mu.Lock()
	refused := db.requestTimes(0, "sysmon_up")
	stored := db.sampleTimes(labelsOf("__name__", "sysmon_up"))
	db.mu.Unlock()
	if len(refused) == 0 {
		t.Fatal("the refused push carried no sysmon_up sample")
	}
	for _, ms := range refused {
		if !slices.Contains(stored, ms) {
			t.Errorf("the reading at %d, refused with 503, was never delivered: stored %v", ms, stored)
		}
	}
}

// TestRemoteWrite_ClientErrorDropsTheBatch verifies a batch the receiver
// refuses as malformed is dropped rather than retried forever, which would
// stop every later batch behind it, and that the loss is counted.
func TestRemoteWrite_ClientErrorDropsTheBatch(t *testing.T) {
	db := newTSDB(t)
	db.set(func(db *tsdb) {
		db.respond = func(n int) writeAnswer {
			if n == 0 {
				return writeAnswer{status: http.StatusBadRequest, body: "out of order sample"}
			}
			return writeAnswer{}
		}
	})
	s := serveServer(t, writeConfig("rack-7", db.writeURL(), ""))

	rec := s.Logs.AwaitMsg(t, "prom: remote write rejected a batch permanently, dropping it")
	if !strings.Contains(fmt.Sprint(rec["error"]), "out of order sample") {
		t.Errorf("the drop was logged with %v, want the receiver's reason", rec["error"])
	}
	db.await(t, "a later push to be accepted", func(db *tsdb) bool { return db.accepted() >= 1 })

	db.mu.Lock()
	dropped := db.requestTimes(0, "sysmon_up")
	stored := db.sampleTimes(labelsOf("__name__", "sysmon_up"))
	db.mu.Unlock()
	for _, ms := range dropped {
		if slices.Contains(stored, ms) {
			t.Errorf("the reading at %d, refused with 400, was sent again", ms)
		}
	}
	awaitScrape(t, s, "the drop to be counted", func(e string) bool {
		v, _ := metricValue(e, "sysmon_remote_write_samples_dropped_total_current")
		return v > 0
	})
}

// TestRemoteWrite_RedirectIsRefused verifies a receiver answering with a
// redirect cannot move the node's metrics to another host: the push fails,
// and the host redirected to hears nothing.
func TestRemoteWrite_RedirectIsRefused(t *testing.T) {
	elsewhere := newTSDB(t)
	db := newTSDB(t)
	db.set(func(db *tsdb) {
		db.respond = func(int) writeAnswer {
			return writeAnswer{status: http.StatusTemporaryRedirect, location: elsewhere.writeURL()}
		}
	})
	s := serveServer(t, writeConfig("rack-7", db.writeURL(), ""))

	rec := s.Logs.AwaitMsg(t, "prom: remote write failed, keeping the batch for the next flush")
	if !strings.Contains(fmt.Sprint(rec["error"]), "redirect") {
		t.Errorf("the failure was logged with %v, want the refused redirect named", rec["error"])
	}
	elsewhere.mu.Lock()
	defer elsewhere.mu.Unlock()
	if len(elsewhere.writes) != 0 {
		t.Errorf("the redirect target received %d pushes, want none", len(elsewhere.writes))
	}
}

// TestRemoteWrite_ReceiverDownBoundsTheBacklog verifies a receiver that
// cannot be reached leaves the failure visible on the scrape endpoint, and
// that the backlog is bounded by the batch size, oldest dropped first.
func TestRemoteWrite_ReceiverDownBoundsTheBacklog(t *testing.T) {
	s := serveServer(t, fmt.Sprintf("fleet:\n  local_name: rack-7\nprometheus:\n  remote_write_url: %q\n"+
		"  sample_interval_ms: 1000\n  flush_interval_ms: 1000\n  batch_size: 1\n",
		deadpeer.URL(t)+"/api/v1/write"))

	s.Logs.AwaitMsg(t, "prom: remote write backlog exceeded its bound, dropped the oldest batch")
	exposition := awaitScrape(t, s, "the failures to show", func(e string) bool {
		failures, _ := metricValue(e, "sysmon_remote_write_consecutive_failures")
		return failures >= 2
	})
	for name, check := range map[string]func(float64) bool{
		"sysmon_remote_write_buffered_samples":               func(v float64) bool { return v > 0 },
		"sysmon_remote_write_samples_dropped_total_current":  func(v float64) bool { return v > 0 },
		"sysmon_remote_write_last_success_timestamp_seconds": func(v float64) bool { return v == 0 },
		"sysmon_remote_write_samples_written_total_current":  func(v float64) bool { return v == 0 },
	} {
		if v, ok := metricValue(exposition, name); !ok || !check(v) {
			t.Errorf("%s = %v (present %v) with the receiver down", name, v, ok)
		}
	}
}

// TestRemoteWrite_ReloadTurnsItOffRetargetsAndRenames verifies remote write
// follows a reload: switched off, it stops and its health series leave the
// scrape; pointed at another receiver, that one is written to; with the fleet
// name changed, the series carry the new name.
func TestRemoteWrite_ReloadTurnsItOffRetargetsAndRenames(t *testing.T) {
	first, second := newTSDB(t), newTSDB(t)
	n := serveNode(t, writeConfig("rack-7", first.writeURL(), ""))
	first.await(t, "a reading at the first receiver", func(db *tsdb) bool { return readings(db, "rack-7") >= 1 })

	if rec := n.reload(t, "fleet:\n  local_name: rack-7\n"); rec.Msg() != "config: reloaded on SIGHUP" {
		t.Fatalf("switching remote write off: %s", rec.Msg())
	}
	awaitScrape(t, n.Server, "remote write's health series to leave the scrape", func(e string) bool {
		return !strings.Contains(e, "sysmon_remote_write_enabled")
	})

	if rec := n.reload(t, writeConfig("rack-7", second.writeURL(), "")); rec.Msg() != "config: reloaded on SIGHUP" {
		t.Fatalf("retargeting remote write: %s", rec.Msg())
	}
	second.await(t, "a reading at the second receiver", func(db *tsdb) bool { return readings(db, "rack-7") >= 1 })

	if rec := n.reload(t, writeConfig("rack-8", second.writeURL(), "")); rec.Msg() != "config: reloaded on SIGHUP" {
		t.Fatalf("renaming the node: %s", rec.Msg())
	}
	second.await(t, "a reading under the new name", func(db *tsdb) bool { return readings(db, "rack-8") >= 1 })
}

// fleetNode is one node of GET /api/fleet, with its history decision.
type fleetNode struct {
	Name    string `json:"name"`
	History *struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	} `json:"history"`
}

// historyState returns the aggregator's history decision for name.
func historyState(t *testing.T, s *harness.Server, name string) (string, string) {
	t.Helper()
	var view struct {
		Nodes []fleetNode `json:"nodes"`
	}
	harness.GetJSON(t, s.URL("/api/fleet"), &view)
	for _, n := range view.Nodes {
		if n.Name == name && n.History != nil {
			return n.History.State, n.History.Reason
		}
	}
	return "", ""
}

// TestRemoteWrite_RetargetDiscardsTheOldBacklog verifies samples held for a
// receiver that could not be reached are not delivered to the one the node is
// pointed at instead: they belong to the first deployment, and the discard is
// said and counted rather than silent.
func TestRemoteWrite_RetargetDiscardsTheOldBacklog(t *testing.T) {
	db := newTSDB(t)
	n := serveNode(t, writeConfig("rack-7", deadpeer.URL(t)+"/api/v1/write", ""))
	awaitScrape(t, n.Server, "a backlog to build up", func(e string) bool {
		v, _ := metricValue(e, "sysmon_remote_write_buffered_samples")
		return v > 0
	})

	if rec := n.reload(t, writeConfig("rack-7", db.writeURL(), "")); rec.Msg() != "config: reloaded on SIGHUP" {
		t.Fatalf("retargeting remote write: %s", rec.Msg())
	}
	rec := n.Logs.AwaitMsg(t, "prom: discarding buffered samples")
	if rec["reason"] != "the remote-write endpoint changed" {
		t.Errorf("the backlog was discarded because %v, want the endpoint change named", rec["reason"])
	}
	db.await(t, "a reading at the new receiver", func(db *tsdb) bool { return readings(db, "rack-7") >= 1 })
}

// TestRemoteWrite_AHostWideAlertDoesNotCostTheBatch raises an alert that names
// no resource -- memory above a 0.01% threshold, which every real host crosses
// -- and checks it is written with no resource label rather than resource="",
// and that no write is refused for it. An empty label value is refused by a
// receiver that validates label values, and the refusal takes every other
// sample in the batch with it: on a busy host, that was every batch that
// carried a CPU, memory, load or swap alert.
func TestRemoteWrite_AHostWideAlertDoesNotCostTheBatch(t *testing.T) {
	db := newTSDB(t)
	serveServer(t, writeConfig("rack-7", db.writeURL(), "")+"alerts:\n  memory_pct: 0.01\n")

	alert := labelsOf("__name__", "sysmon_alert", "rule", "memory_high")
	db.await(t, "the memory alert to be stored", func(db *tsdb) bool { return len(db.matching(alert)) > 0 })

	db.mu.Lock()
	defer db.mu.Unlock()
	for i, w := range db.writes {
		if w.status/100 != 2 {
			t.Errorf("write %d answered %d: %s", i, w.status, w.rejected)
		}
	}
	for _, st := range db.matching(alert) {
		if v, ok := st.labels["resource"]; ok {
			t.Errorf("series %s carries resource=%q; a host-wide alert has no resource label", seriesKey(st.labels), v)
		}
	}
}
