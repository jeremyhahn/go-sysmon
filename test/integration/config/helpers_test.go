//go:build integration

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// Log messages the serve command writes about its configuration.
const (
	msgSIGHUPReloaded = "config: reloaded on SIGHUP"
	msgSIGHUPFailed   = "config: SIGHUP reload failed, keeping previous settings"
	msgNeedsRestart   = "config: these settings need a restart to take effect"
	msgFileReloaded   = "config: reloaded from disk"
	msgFileFailed     = "config: reload failed, keeping previous settings"
	msgDirGone        = "config: the config directory is gone, changes are no longer watched"
	msgStoppedCleanly = "server stopped cleanly"
)

// settings is GET /api/config, keyed by setting.
type settings struct {
	file    string
	entries map[string]types.ConfigEntry
}

// viewOf fetches the server's settings view.
func viewOf(t *testing.T, s *harness.Server) settings {
	t.Helper()
	var view types.ConfigView
	harness.GetJSON(t, s.URL("/api/config"), &view)
	out := settings{file: view.ConfigFile, entries: make(map[string]types.ConfigEntry, len(view.Entries))}
	for _, e := range view.Entries {
		out.entries[e.Key] = e
	}
	return out
}

// want fails the test unless key has value, supplied by source.
func (s settings) want(t *testing.T, key, value string, source types.ConfigSource) {
	t.Helper()
	e, ok := s.entries[key]
	if !ok {
		t.Errorf("GET /api/config has no %s", key)
		return
	}
	if e.Value != value || e.Source != source {
		t.Errorf("%s = %q from %q, want %q from %q", key, e.Value, e.Source, value, source)
	}
}

// rewrite replaces the file at path with body in place, the way an editor
// that truncates and writes does: one open for writing, closed when done.
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// replace swaps a complete new file into path by rename, the way most
// configuration management writes a file atomically.
func replace(t *testing.T, path, body string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename %s into place: %v", tmp, err)
	}
}

// configDir writes body as sysmon.yaml in a fresh directory and returns its
// path.
func configDir(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	rewrite(t, path, body)
	return path
}

// settingsOf returns the "settings" list a restart-needed log line names.
func settingsOf(r harness.LogRecord) []string {
	raw, _ := r["settings"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// failsToStart runs `sysmon serve` with the config file body and returns
// what it printed, failing the test unless it refused to start with exit 1.
func failsToStart(t *testing.T, env []string, args ...string) string {
	t.Helper()
	full := append([]string{"serve", "--addr", "127.0.0.1:0", "--log-format", "json"}, args...)
	r := harness.Run(t, harness.Bins.Sysmon, env, full...)
	if r.Code != 1 {
		t.Fatalf("sysmon serve %v exited %d, want 1\nstderr:\n%s", args, r.Code, r.Stderr)
	}
	if strings.Contains(r.Stderr, harness.ListeningMsg) {
		t.Fatalf("sysmon serve %v bound a listener before refusing:\n%s", args, r.Stderr)
	}
	return r.Stderr
}
