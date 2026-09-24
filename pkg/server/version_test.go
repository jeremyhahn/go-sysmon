package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/server"
)

// TestHandleVersion_ReportsBuildMetadata verifies GET /api/version returns the
// version, git commit and build date supplied through the server Config -- the
// values the UI footer shows so an operator can tell which release a node runs.
func TestHandleVersion_ReportsBuildMetadata(t *testing.T) {
	m := newMonitor(t, "version-host")
	srv, err := server.NewWithConfig(server.Config{
		Monitor:   m,
		Addr:      ":0",
		Version:   "1.2.3-test",
		GitCommit: "abc1234",
		BuildDate: "2026-01-02T03:04:05Z",
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		Version   string `json:"version"`
		GitCommit string `json:"git_commit"`
		BuildDate string `json:"build_date"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != "1.2.3-test" || got.GitCommit != "abc1234" || got.BuildDate != "2026-01-02T03:04:05Z" {
		t.Errorf("version response = %+v, want the configured build metadata", got)
	}
}

// TestHandleVersion_DefaultsWhenUnset confirms an unconfigured build reports the
// dev/unknown fallbacks rather than empty strings, so the footer always renders.
func TestHandleVersion_DefaultsWhenUnset(t *testing.T) {
	m := newMonitor(t, "version-default-host")
	srv := server.New(m, ":0", nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	})

	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["version"] != "dev" || got["git_commit"] != "unknown" || got["build_date"] != "unknown" {
		t.Errorf("defaults = %+v, want dev/unknown/unknown", got)
	}
}
