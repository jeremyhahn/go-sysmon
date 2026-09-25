//go:build integration

package config

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestLogging_WritesToTheLogFile runs the service with --log-file: the log
// goes to the file, creating its directory, in JSON, at the level asked for,
// and nothing is written to stderr.
func TestLogging_WritesToTheLogFile(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "logs", "sysmon.log")
	s := harness.Start(t, harness.Bins.Sysmon, nil, "serve", "--addr", "127.0.0.1:0", "--interval", "200",
		"--config", filepath.Join(t.TempDir(), "absent.yaml"),
		"--log-file", logFile, "--log-format", "json", "--log-level", "info")

	// The log file belongs to another process, so reading it is the only way
	// to see the listener come up.
	await.Until(t, "the service to log its listener to "+logFile, func(context.Context) bool {
		return fileLogs(t, logFile, harness.ListeningMsg)
	})
	if err := s.Stop(t); err != nil {
		t.Fatalf("sysmon serve on SIGTERM: %v, want a clean exit", err)
	}
	if !fileLogs(t, logFile, msgStoppedCleanly) {
		t.Errorf("%s does not end with the clean stop", logFile)
	}
	if s.Logs.Has(harness.ListeningMsg) {
		t.Error("the log went to stderr as well as the file")
	}
}

// fileLogs reports whether the JSON log at path holds a record with message
// msg, failing the test on a line that is not JSON or is below INFO.
func fileLogs(t *testing.T, path, msg string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec harness.LogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// The writer may be mid-line; only a complete file is judged.
			continue
		}
		if rec.Level() == "DEBUG" {
			t.Errorf("--log-level info let through DEBUG %q", rec.Msg())
		}
		found = found || rec.Msg() == msg
	}
	return found
}

// TestLogging_LevelsFilter verifies each level lets through what is at or
// above it: debug shows the watcher's progress, warn and error do not show a
// startup INFO line.
func TestLogging_LevelsFilter(t *testing.T) {
	for _, level := range []string{"warn", "warning", "error"} {
		r := harness.Run(t, harness.Bins.Sysmon, nil, "--log-level", level, "--log-format", "json", "version")
		if r.Code != 0 {
			t.Fatalf("--log-level %s: exit %d: %s", level, r.Code, r.Stderr)
		}
		if strings.Contains(r.Stderr, `"level":"INFO"`) || strings.Contains(r.Stderr, `"level":"DEBUG"`) {
			t.Errorf("--log-level %s let through: %s", level, r.Stderr)
		}
	}
}

// TestRoot_WithADisplayButNoGUIShowsTheOverview runs the bare command with a
// display set on a build with no GUI compiled in: it prints the overview
// rather than failing to open a window.
func TestRoot_WithADisplayButNoGUIShowsTheOverview(t *testing.T) {
	r := harness.Run(t, harness.Bins.Sysmon, []string{"DISPLAY=:0"})
	if r.Code != 0 || !strings.Contains(r.Stdout, "System Monitor") {
		t.Errorf("sysmon with DISPLAY=:0: exit %d, stdout %q; want 0 and the overview", r.Code, r.Stdout)
	}
}

// TestLogging_TextFormat verifies the default text format is logfmt-style
// key=value, not JSON.
func TestLogging_TextFormat(t *testing.T) {
	r := harness.Run(t, harness.Bins.Sysmon, nil, "--log-format", "text", "--log-level", "debug", "version")
	if r.Code != 0 {
		t.Fatalf("sysmon version exited %d: %s", r.Code, r.Stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(r.Stderr), "\n") {
		if strings.HasPrefix(line, "{") {
			t.Errorf("--log-format text wrote JSON: %q", line)
		}
	}
}

// TestLogging_RefusesWhatItCannotDo verifies an unknown level or format, or a
// log file that cannot be created, stops the command with the reason rather
// than logging somewhere the operator did not ask for.
func TestLogging_RefusesWhatItCannotDo(t *testing.T) {
	blocker := harness.WriteFile(t, "not-a-dir", "x")
	tests := map[string]struct {
		args []string
		want string
	}{
		"unknown level":  {[]string{"--log-level", "loud"}, "loud"},
		"unknown format": {[]string{"--log-format", "yaml"}, "yaml"},
		"uncreatable log file": {[]string{"--log-file", filepath.Join(blocker, "sysmon.log")},
			filepath.Join(blocker, "sysmon.log")},
		"log file that is a directory": {[]string{"--log-file", filepath.Dir(blocker)}, filepath.Dir(blocker)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Sysmon, nil, append(tc.args, "version")...)
			if r.Code != 1 || !strings.Contains(r.Stderr, "Error:") || !strings.Contains(r.Stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want 1 and an error naming %q", r.Code, r.Stderr, tc.want)
			}
		})
	}
}

// TestShutdown_OnSIGINT verifies Ctrl-C in a terminal stops the service as
// cleanly as SIGTERM from a service manager.
func TestShutdown_OnSIGINT(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{})
	s.Signal(t, syscall.SIGINT)
	if err := s.Wait(t); err != nil {
		t.Fatalf("sysmon serve on SIGINT: %v, want a clean exit", err)
	}
	s.Logs.AwaitMsg(t, "shutdown signal received, stopping server")
	s.Logs.AwaitMsg(t, msgStoppedCleanly)
}

// TestShutdown_DuringStartup sends SIGTERM once the service has begun
// starting -- its handler is installed before it says so -- the way
// `systemctl stop` can land during a rolling deploy: however far startup got,
// the process exits cleanly and never dies on the signal.
func TestShutdown_DuringStartup(t *testing.T) {
	s := harness.Start(t, harness.Bins.Sysmon, nil, "serve", "--addr", "127.0.0.1:0", "--interval", "200",
		"--log-format", "json", "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	s.Logs.AwaitMsg(t, "starting sysmon web server")
	if err := s.Stop(t); err != nil {
		t.Fatalf("sysmon serve stopped during startup: %v, want a clean exit", err)
	}
}

// TestServe_AddressInUseIsAnError verifies a listen address another socket
// holds is reported by the command's exit, not only in a log.
func TestServe_AddressInUseIsAnError(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	harness.CloseOnCleanup(t, "the held listener", held)

	r := harness.Run(t, harness.Bins.Sysmon, nil, "serve", "--addr", held.Addr().String(),
		"--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if r.Code != 1 || !strings.Contains(r.Stderr, "address already in use") {
		t.Errorf("exit %d, stderr %q; want 1 and the address named in use", r.Code, r.Stderr)
	}
}

// TestMemoryLimit_ReportsItsSource verifies the service says at startup where
// its Go memory limit came from: the operator's GOMEMLIMIT outranks anything
// derived, and without it the container's own cgroup is read.
func TestMemoryLimit_ReportsItsSource(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{Env: []string{"GOMEMLIMIT=512MiB"}})
	r := s.Logs.AwaitMsg(t, "memory limit: set by the environment")
	if src, _ := r["source"].(string); src != "GOMEMLIMIT" {
		t.Errorf("source = %q, want GOMEMLIMIT", src)
	}

	s = harness.Serve(t, harness.ServeOptions{})
	want := containerMemoryOutcome(t)
	s.Logs.AwaitMsg(t, want)
}

// containerMemoryOutcome is the startup line this container's cgroups should
// produce: any memory.high or memory.max set on the path from this process's
// cgroup up to the root means a derived limit.
func containerMemoryOutcome(t *testing.T) string {
	t.Helper()
	const none = "memory limit: no cgroup ceiling found; the Go runtime default applies"
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return none
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		for dir := filepath.Join("/sys/fs/cgroup", rest); strings.HasPrefix(dir, "/sys/fs/cgroup"); dir = filepath.Dir(dir) {
			for _, name := range []string{"memory.high", "memory.max"} {
				v, err := os.ReadFile(filepath.Join(dir, name))
				if err == nil && strings.TrimSpace(string(v)) != "max" {
					return "memory limit: derived from the cgroup"
				}
			}
		}
	}
	return none
}

// TestPeerConfig_RelayedWithTheTopologyWithheld runs an aggregator and its
// peer: the peer's settings, relayed through the aggregator, name the peer's
// settings and their sources but not where it listens.
func TestPeerConfig_RelayedWithTheTopologyWithheld(t *testing.T) {
	peer := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: leaf\nalerts:\n  cpu_pct: 60\n"})
	agg := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: agg\n  poll_interval_ms: 1000\n" +
		"  peers:\n    - name: leaf\n      url: http://" + peer.Addr + "\n"})

	var view types.ConfigView
	harness.GetJSON(t, agg.URL("/api/fleet/leaf/config"), &view)
	got := settings{entries: map[string]types.ConfigEntry{}}
	for _, e := range view.Entries {
		got.entries[e.Key] = e
	}
	got.want(t, "alerts.cpu_pct", "60", types.ConfigSourceFile)
	got.want(t, "fleet.local_name", "leaf", types.ConfigSourceFile)
	got.want(t, "server.addr", "(configured)", types.ConfigSourceFlag)
	got.want(t, "prometheus.query_url", "(not set)", types.ConfigSourceDefault)

	// The aggregator's own name answers with its own settings.
	var own types.ConfigView
	harness.GetJSON(t, agg.URL("/api/fleet/agg/config"), &own)
	if len(own.Entries) == 0 || own.ConfigFile != "(configured)" {
		t.Errorf("GET /api/fleet/agg/config = %+v, want the aggregator's own settings", own)
	}
}
