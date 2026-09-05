//go:build !prometheus

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// A build with no prometheus support must answer the history endpoints with the
// same envelope a node with no query URL does -- Enabled=false, HTTP 200 --
// and never a 404.
//
// Owner ruling, 2026-09-04: prometheus is optional and behind a build tag. The
// dashboard hides its date picker when the catalog says history is unavailable;
// on a 404 it would be looking at a failure, and the picker's absence would
// stop being a decision and become a symptom (#100).
//
// The two builds are deliberately indistinguishable to a client that has no
// history: "this node keeps no history" and "this build cannot" are the same
// answer to the only question the dashboard asks.

func noPromServer(t *testing.T) *Server {
	t.Helper()
	mon, err := monitor.New(idleSnapshotter{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	srv, err := NewWithConfig(Config{
		Monitor: mon,
		Addr:    "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	return srv
}

func TestHistoryMetrics_WithoutPrometheusSaysUnavailable(t *testing.T) {
	srv := noPromServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 -- a build without history must answer, not fail", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" || ct[:16] != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}

	var catalog types.HistoryCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	if catalog.Enabled {
		t.Error("catalog reports enabled in a build with no prometheus support")
	}
	if len(catalog.Metrics) != 0 {
		t.Errorf("catalog advertises %d metrics that cannot be queried", len(catalog.Metrics))
	}
}

func TestHistory_WithoutPrometheusSaysUnavailable(t *testing.T) {
	srv := noPromServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history?metric=cpu&start=1&end=2", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}

	var history types.History
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	if history.Enabled {
		t.Error("history reports enabled in a build with no prometheus support")
	}
	if len(history.Series) != 0 {
		t.Errorf("history returned %d series from a build that cannot query", len(history.Series))
	}
}

// TestMetricsExposition_WithoutPrometheusHasNoExporterSeries is the other half:
// the exposition is byte-for-byte what a node with no remote_write_url
// produces, because there is no exporter to have any health.
func TestMetricsExposition_WithoutPrometheusHasNoExporterSeries(t *testing.T) {
	srv := noPromServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 200 or 503", rec.Code)
	}
	if body := rec.Body.String(); len(body) > 0 {
		for _, name := range []string{
			"sysmon_remote_write_enabled",
			"sysmon_remote_write_buffered_samples",
			"sysmon_remote_write_consecutive_failures",
		} {
			if strings.Contains(body, name) {
				t.Errorf("%s is exported by a build with no exporter", name)
			}
		}
	}
}
