//go:build integration

package prom

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// cadenceRule is the alert raised for history written too slowly to chart.
const cadenceRule = "history_cadence_unchartable"

// awaitAlert waits for the snapshot's alerts to satisfy want.
func awaitAlert(t *testing.T, s *harness.Server, what string, want bool) {
	t.Helper()
	await.Until(t, what, func(context.Context) bool {
		_, ok := hasAlert(snapshotAlerts(t, s), cadenceRule)
		return ok == want
	})
}

// TestCadence_UnchartableHistoryIsRaisedAndRefused runs a node whose default
// cadence -- a 15s sample, the dashboard's 60s slowest collection and a 10s
// write timeout, 85s in all -- is past a database with a 70s lookback. The
// service says so everywhere an operator looks: an error in the log, a
// warning alert, and its systemd status; a reload that leaves it so is
// refused; and one that shortens the cadence clears all three.
func TestCadence_UnchartableHistoryIsRaisedAndRefused(t *testing.T) {
	db := newTSDB(t)
	db.set(func(db *tsdb) {
		db.flags = map[string]string{"query.lookback-delta": "70s"}
		db.lookback = 70 * time.Second
	})
	notify := newNotifySocket(t)
	slow := fmt.Sprintf("fleet:\n  local_name: rack-7\nprometheus:\n  remote_write_url: %q\n", db.writeURL())
	n := serveNode(t, slow, notify.env())

	rec := n.Logs.Await(t, "the violation to be logged", func(r harness.LogRecord) bool {
		return strings.HasPrefix(r.Msg(), "history: ") && strings.Contains(r.Msg(), "prometheus.sample_interval_ms")
	})
	if rec.Level() != "ERROR" {
		t.Errorf("the violation was logged at %s, want ERROR", rec.Level())
	}
	awaitAlert(t, n.Server, "the cadence alert to be raised", true)
	alert, _ := hasAlert(snapshotAlerts(t, n.Server), cadenceRule)
	if alert.Severity != "warning" || !strings.Contains(alert.Message, "Shorten those intervals") {
		t.Errorf("cadence alert = %+v, want a warning saying what to change", alert)
	}
	notify.awaitStatus(t, "the unit's status to say it is degraded", func(s string) bool {
		return strings.HasPrefix(s, "degraded: ")
	})
	exposition := scrape(t, n.Server)
	if v, ok := metricValue(exposition, "sysmon_alert", `rule="`+cadenceRule+`"`, `severity="warning"`,
		`resource="prometheus.sample_interval_ms + server.interval_ms + prometheus.timeout_ms"`, `category="host"`); !ok || v != 1 {
		t.Errorf("the scrape does not carry the cadence alert:\n%s", exposition)
	}

	if rec := n.reload(t, slow); rec.Msg() != "config: SIGHUP reload failed, keeping previous settings" {
		t.Errorf("a reload keeping the unchartable cadence was %q, want it refused", rec.Msg())
	}

	fast := slow + "  sample_interval_ms: 1000\n  timeout_ms: 2000\n"
	if rec := n.reload(t, fast); rec.Msg() != "config: reloaded on SIGHUP" {
		t.Fatalf("a reload fixing the cadence was %q, want it applied", rec.Msg())
	}
	n.Logs.AwaitMsg(t, "history: cadence check passed")
	awaitAlert(t, n.Server, "the cadence alert to clear", false)
	notify.awaitStatus(t, "the unit's status to clear", func(s string) bool { return s == "" })
}

// TestCadence_PendingWhenTheLookbackCannotBeRead verifies the check waits,
// saying why, rather than passing or failing when there is no lookback to
// check against: remote write to a receiver whose server cannot be derived,
// a database that cannot be reached, and one that does not publish it. An
// aggregator in that state writes its peers' history unverified, and says so.
func TestCadence_PendingWhenTheLookbackCannotBeRead(t *testing.T) {
	unpublished := newTSDB(t)
	unpublished.set(func(db *tsdb) { db.flagsStatus = 404 })
	receiver := newTSDB(t)
	for _, c := range []struct {
		name   string
		prom   string
		reason string
	}{
		{"a receiver that is not /api/v1/write",
			fmt.Sprintf("  remote_write_url: %q\n", receiver.srv.URL+"/receive"), "no query_url is set"},
		{"a database that cannot be reached",
			fmt.Sprintf("  remote_write_url: %q\n  query_url: %q\n  timeout_ms: 1000\n", receiver.writeURL(), deadpeer.URL(t)),
			"could not be reached"},
		{"a database that does not publish it",
			fmt.Sprintf("  remote_write_url: %q\n  query_url: %q\n", receiver.writeURL(), unpublished.srv.URL),
			"does not serve /api/v1/status/flags"},
	} {
		t.Run(c.name, func(t *testing.T) {
			notify := newNotifySocket(t)
			peer := newPeerNode(t, gpuBox("gpu-box", time.Now()), 1000)
			s := serveServer(t, fmt.Sprintf("fleet:\n  local_name: agg\n  poll_interval_ms: 1000\n  peers:\n"+
				"    - name: gpu-box\n      url: %s\nprometheus:\n%s", peer.srv.URL, c.prom), notify.env())

			rec := s.Logs.Await(t, "the pending check to be logged", func(r harness.LogRecord) bool {
				return strings.HasPrefix(r.Msg(), "history: cadence check pending: ")
			})
			if rec.Level() != "WARN" || !strings.Contains(rec.Msg(), c.reason) {
				t.Errorf("pending logged at %s as %q, want a warning naming %q", rec.Level(), rec.Msg(), c.reason)
			}
			notify.awaitStatus(t, "the unit's status to say the check is pending", func(s string) bool {
				return strings.HasPrefix(s, "history cadence check pending: ") && strings.Contains(s, c.reason)
			})
			await.Until(t, "gpu-box to be written unverified", func(context.Context) bool {
				state, reason := historyState(t, s, "gpu-box")
				return state == "unverified" && reason != ""
			})
		})
	}
}

// TestCadence_StopsPromptlyWhileTheDatabaseDoesNotAnswer verifies a service
// told to stop while its cadence check waits on a database that never answers
// stops at once and cleanly, rather than after the check's timeout.
func TestCadence_StopsPromptlyWhileTheDatabaseDoesNotAnswer(t *testing.T) {
	db := newTSDB(t)
	db.set(func(db *tsdb) {
		db.flagsHook = func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	})
	s := serveServer(t, fmt.Sprintf("fleet:\n  local_name: rack-7\nprometheus:\n  remote_write_url: %q\n"+
		"  timeout_ms: 600000\n", db.writeURL()))
	db.await(t, "the cadence check to ask for the lookback", func(db *tsdb) bool { return db.flagsAsked >= 1 })

	if err := s.Stop(t); err != nil {
		t.Errorf("stopping mid-check: %v, want a clean exit", err)
	}
}
