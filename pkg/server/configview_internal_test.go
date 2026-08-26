package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// reloadManager returns a config manager over a temp file holding body, plus
// the file's path so a test can rewrite it and reload.
func reloadManager(t *testing.T, body string) (*config.Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	return mgr, path
}

// intervalServer builds a server whose monitor and config manager both start at
// bootMs, which is the state a freshly started daemon is in.
func intervalServer(t *testing.T, bootMs int) (*Server, *config.Manager, string) {
	t.Helper()
	mgr, path := reloadManager(t, "server:\n  interval_ms: "+strconv.Itoa(bootMs)+"\n")
	mon, err := monitor.New(idleSnapshotter{}, time.Duration(bootMs)*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New(): %v", err)
	}
	return &Server{mon: mon, cfg: mgr}, mgr, path
}

// configEntry returns the settings-view entry for key as GET /api/config
// serves it, so a test sees exactly what a client would.
func configEntry(t *testing.T, s *Server, key string) types.ConfigEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	var view types.ConfigView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	for _, e := range view.Entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("view has no entry for %q", key)
	return types.ConfigEntry{}
}

// setInterval drives POST /api/interval, the call the dashboard's refresh-rate
// selector makes.
func setInterval(t *testing.T, s *Server, ms int) {
	t.Helper()
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"interval_ms":` + strconv.Itoa(ms) + `}`)
	s.handleSetInterval(rec, httptest.NewRequest(http.MethodPost, "/api/interval", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/interval = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestConfigView_ReportsRuntimeInterval covers the contradiction a user creates
// just by using the dashboard: picking a refresh rate retunes the monitor
// without touching the file, so a settings view reading the file would report
// one number while GET /api/interval reports another, about one running
// service. The view reports what is running, and says so.
func TestConfigView_ReportsRuntimeInterval(t *testing.T) {
	s, _, _ := intervalServer(t, 1000)

	if entry := configEntry(t, s, "server.interval_ms"); entry.Value != "1000" {
		t.Fatalf("before any override the view says %q, want the configured 1000", entry.Value)
	}

	setInterval(t, s, 5000)

	entry := configEntry(t, s, "server.interval_ms")
	if entry.Value != "5000" {
		t.Errorf("view value = %q, want the running 5000", entry.Value)
	}
	if entry.Source != config.SourceRuntime {
		t.Errorf("view source = %q, want %q -- the number came from the dashboard, not the file",
			entry.Source, config.SourceRuntime)
	}
	if running := s.mon.Interval(); running != 5*time.Second {
		t.Errorf("monitor interval = %v, want 5s", running)
	}
}

// TestConfigView_ReloadReclaimsTheInterval pins the deliberate half of this
// behaviour: a reload means "make the file true", so it takes the interval back
// from a transient dashboard choice -- in the monitor, not just in the report.
func TestConfigView_ReloadReclaimsTheInterval(t *testing.T) {
	s, mgr, _ := intervalServer(t, 1000)
	mgr.OnReload(func(c *config.Config) error {
		return s.mon.SetInterval(time.Duration(c.Server.IntervalMs) * time.Millisecond)
	})

	setInterval(t, s, 5000)
	if entry := configEntry(t, s, "server.interval_ms"); entry.Source != config.SourceRuntime {
		t.Fatalf("override did not take: source = %q", entry.Source)
	}

	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}

	entry := configEntry(t, s, "server.interval_ms")
	if entry.Value != "1000" {
		t.Errorf("after reload view value = %q, want the configured 1000", entry.Value)
	}
	if entry.Source == config.SourceRuntime {
		t.Error("after reload source is still runtime; the configured value is in force and should say so")
	}
	if running := s.mon.Interval(); running != time.Second {
		t.Errorf("monitor interval = %v, want the reload to have actually retuned it to 1s", running)
	}
}

// TestConfigView_NoRuntimeOverrideKeepsConfiguredSource is the no-regression
// guard: "runtime" must never appear when nothing overrode anything, or the
// marker stops meaning what it says.
func TestConfigView_NoRuntimeOverrideKeepsConfiguredSource(t *testing.T) {
	s, _, _ := intervalServer(t, 1000)

	entry := configEntry(t, s, "server.interval_ms")
	if entry.Value != "1000" {
		t.Errorf("view value = %q, want 1000", entry.Value)
	}
	if entry.Source != config.SourceFile {
		t.Errorf("view source = %q, want %q -- the file set this value and nothing overrode it",
			entry.Source, config.SourceFile)
	}

	// Setting the same value the file already has is not an override either.
	setInterval(t, s, 1000)
	if got := configEntry(t, s, "server.interval_ms").Source; got != config.SourceFile {
		t.Errorf("source = %q after setting the configured value, want %q", got, config.SourceFile)
	}
}

// TestConfigView_WithoutAMonitorStillServes covers the nil-monitor path so the
// override cannot panic a server built without one.
func TestConfigView_WithoutAMonitorStillServes(t *testing.T) {
	mgr, _ := reloadManager(t, "server:\n  interval_ms: 1000\n")
	if got := configEntry(t, &Server{cfg: mgr}, "server.interval_ms").Value; got != "1000" {
		t.Errorf("value = %q, want the configured 1000", got)
	}
}
