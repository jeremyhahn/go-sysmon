//go:build integration

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// request sends method to url with body and returns the status, headers and
// body.
func request(t *testing.T, client *http.Client, method, url, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	status, header, respBody := harness.Do(t, client, req)
	return status, header, string(respBody)
}

// TestAPI_SnapshotDescribesThisNode verifies the snapshot is the real host's
// reading, identified with the node's configured name and this build, with
// every list present as a list.
func TestAPI_SnapshotDescribesThisNode(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: rack-7\n"})

	var snap types.Snapshot
	harness.GetJSON(t, s.URL("/api/snapshot"), &snap)
	if snap.NodeName != "rack-7" {
		t.Errorf("node_name = %q, want the configured rack-7", snap.NodeName)
	}
	if snap.Host.Hostname == "" || len(snap.CPUs) == 0 || snap.Memory.TotalBytes == 0 {
		t.Errorf("snapshot is missing the host's reading: host %q, %d CPUs, %d memory bytes",
			snap.Host.Hostname, len(snap.CPUs), snap.Memory.TotalBytes)
	}
	if snap.Timestamp.IsZero() {
		t.Error("the snapshot is undated")
	}
	_, body := harness.Get(t, s.URL("/api/snapshot"))
	if strings.Contains(string(body), `":null`) {
		t.Errorf("the snapshot carries a null where a list or object belongs: %.300s", body)
	}
}

// TestAPI_SummaryAndHistorySnapshotAreWhatAPeerPolls verifies the two views
// an aggregator polls: the summary, and the history snapshot without the
// process list.
func TestAPI_SummaryAndHistorySnapshotAreWhatAPeerPolls(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: rack-7\n"})
	// The summary's paging rate needs two collections.
	await.Until(t, "a second collection", func(context.Context) bool {
		status, _ := harness.Get(t, s.URL("/api/summary"))
		return status == http.StatusOK
	})

	var summary types.NodeSummary
	harness.GetJSON(t, s.URL("/api/summary"), &summary)
	if summary.Hostname == "" || summary.NodeName != "rack-7" || summary.CollectionIntervalMs != 200 || summary.MemoryPercent <= 0 {
		t.Errorf("summary = %+v, want the host's name, the fleet name, the 200ms cadence and its memory use", summary)
	}

	var hist types.Snapshot
	harness.GetJSON(t, s.URL("/api/snapshot/history"), &hist)
	if hist.Host.Hostname == "" {
		t.Error("the history snapshot has no host")
	}
	if len(hist.Processes.Processes) != 0 {
		t.Errorf("the history snapshot carries %d processes, want none: history does not record them",
			len(hist.Processes.Processes))
	}
	for _, cpu := range hist.CPUs {
		if len(cpu.Flags) != 0 {
			t.Fatalf("the history snapshot carries CPU flags, want none: %v", cpu.Flags)
		}
	}
}

// TestAPI_IntervalIsSetAndRefused covers /api/interval: an allowed value is
// applied and reported back, and everything else is refused without
// changing the cadence.
func TestAPI_IntervalIsSetAndRefused(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	url := s.URL("/api/interval")

	status, _, body := request(t, http.DefaultClient, http.MethodPost, url, `{"interval_ms":5000}`)
	if status != http.StatusOK || !strings.Contains(body, `"interval_ms":5000`) {
		t.Fatalf("POST 5000 = %d %s, want 200 reporting 5000", status, body)
	}

	refusals := map[string]string{
		"a value not on the menu":    `{"interval_ms":1234}`,
		"a negative value":           `{"interval_ms":-1000}`,
		"an unknown scope":           `{"interval_ms":1000,"scope":"galaxy"}`,
		"a body that is not JSON":    `interval=1000`,
		"a body over the size limit": `{"interval_ms":1000,"pad":"` + strings.Repeat("x", 2048) + `"}`,
	}
	for name, body := range refusals {
		t.Run(name, func(t *testing.T) {
			if status, _, resp := request(t, http.DefaultClient, http.MethodPost, url, body); status != http.StatusBadRequest {
				t.Errorf("POST %.40s = %d %s, want 400", body, status, resp)
			}
		})
	}

	// Peers are refreshed no faster than the fleet's floor, and the answer
	// says what is in force rather than what was asked for.
	status, _, body = request(t, http.DefaultClient, http.MethodPost, url, `{"interval_ms":250,"scope":"fleet"}`)
	if status != http.StatusOK || !strings.Contains(body, `"fleet_interval_ms":1000`) {
		t.Errorf("POST 250 for the fleet = %d %s, want the 1000ms floor reported", status, body)
	}

	status, _, body = request(t, http.DefaultClient, http.MethodGet, url, "")
	if status != http.StatusOK || !strings.Contains(body, `"interval_ms":5000`) {
		t.Errorf("GET after the refusals = %d %s, want the 5000 that was set", status, body)
	}
	if status, _, _ := request(t, http.DefaultClient, http.MethodPut, url, `{"interval_ms":1000}`); status != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/interval = %d, want 405", status)
	}
}

// entry returns the settings-view entry for key.
func entry(t *testing.T, view types.ConfigView, key string) types.ConfigEntry {
	t.Helper()
	for _, e := range view.Entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("/api/config has no %s", key)
	return types.ConfigEntry{}
}

// TestAPI_VersionConfigAndHistoryOnADefaultBuild covers the endpoints every
// build serves: the build's version; the settings view, which says where each
// value came from and reports the cadence actually running; and history, which
// a build without Prometheus support reports as not kept.
func TestAPI_VersionConfigAndHistoryOnADefaultBuild(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{Config: "alerts:\n  cpu_pct: 97\n"})

	var version map[string]string
	harness.GetJSON(t, s.URL("/api/version"), &version)
	for _, key := range []string{"version", "git_commit", "build_date"} {
		if version[key] == "" {
			t.Errorf("/api/version has no %s: %v", key, version)
		}
	}

	var cfg types.ConfigView
	harness.GetJSON(t, s.URL("/api/config"), &cfg)
	if e := entry(t, cfg, "alerts.cpu_pct"); e.Value != "97" || e.Source != types.ConfigSourceFile {
		t.Errorf("alerts.cpu_pct = %+v, want 97 from the file", e)
	}
	if status, _ := harness.Post(t, s.URL("/api/interval"), `{"interval_ms":10000}`); status != http.StatusOK {
		t.Fatalf("POST /api/interval = %d", status)
	}
	harness.GetJSON(t, s.URL("/api/config"), &cfg)
	if e := entry(t, cfg, "server.interval_ms"); e.Value != "10000" || e.Source != types.ConfigSourceRuntime {
		t.Errorf("server.interval_ms after a runtime change = %+v, want 10000 from the runtime", e)
	}

	var hist types.History
	harness.GetJSON(t, s.URL("/api/history?metric=cpu&range=1h"), &hist)
	if hist.Enabled {
		t.Error("/api/history on a build without Prometheus says history is kept")
	}
	var catalog types.HistoryCatalog
	harness.GetJSON(t, s.URL("/api/history/metrics"), &catalog)
	if catalog.Enabled {
		t.Error("/api/history/metrics on a build without Prometheus says history is kept")
	}
}

// TestAPI_UnknownPathsAndMethods verifies a path nothing serves is a 404 and
// a method a route does not take is a 405 naming what it does take.
func TestAPI_UnknownPathsAndMethods(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	if status, _, _ := request(t, http.DefaultClient, http.MethodGet, s.URL("/api/no-such-thing"), ""); status != http.StatusNotFound {
		t.Errorf("GET /api/no-such-thing = %d, want 404", status)
	}
	status, header, _ := request(t, http.DefaultClient, http.MethodPost, s.URL("/api/snapshot"), "{}")
	if status != http.StatusMethodNotAllowed || !strings.Contains(header.Get("Allow"), http.MethodGet) {
		t.Errorf("POST /api/snapshot = %d Allow %q, want 405 allowing GET", status, header.Get("Allow"))
	}
}
