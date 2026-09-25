package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
)

// recordHandler is a slog.Handler that delivers each record's message on a
// channel, so a test can wait for a log line instead of for time to pass.
type recordHandler struct{ msgs chan string }

func (h recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.msgs <- r.Message
	return nil
}
func (h recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordHandler) WithGroup(string) slog.Handler      { return h }

// reloadOnSIGHUP starts watchReloadSignals over the config at path, sends
// this process a SIGHUP, and returns the log lines the reload writes, in
// order, up to and including the one that ends it.
func reloadOnSIGHUP(t *testing.T, mgr *config.Manager, lines int) []string {
	t.Helper()
	msgs := make(chan string, 8)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	// Notify is installed before watchReloadSignals returns, so the signal
	// below is caught and never reaches SIGHUP's default action.
	watchReloadSignals(mgr, done, slog.New(recordHandler{msgs: msgs}))
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	got := make([]string, 0, lines)
	for range lines {
		got = append(got, await.Recv(t, "a reload log line", msgs))
	}
	return got
}

func TestWatchReloadSignals_ReloadsAndNamesWhatNeedsARestart(t *testing.T) {
	path := withServeConfig(t, "server:\n  addr: \":8081\"\n")
	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	// The listen address is consumed at bind: changing it needs a restart.
	if err := os.WriteFile(path, []byte("server:\n  addr: \":8082\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := reloadOnSIGHUP(t, mgr, 2)

	want := []string{"config: reloaded on SIGHUP", "config: these settings need a restart to take effect"}
	if got[0] != want[0] || got[1] != want[1] {
		t.Errorf("log = %q, want %q", got, want)
	}
	// Deferred, not applied: the running listener is still on :8081, and the
	// configuration says what is actually in effect.
	if addr := mgr.Current().Server.Addr; addr != ":8081" {
		t.Errorf("server.addr after SIGHUP = %q, want :8081 until a restart", addr)
	}
}

func TestWatchReloadSignals_KeepsTheSettingsWhenTheFileIsBroken(t *testing.T) {
	path := withServeConfig(t, "server:\n  addr: \":8081\"\n")
	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	if err := os.WriteFile(path, []byte("server: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := reloadOnSIGHUP(t, mgr, 1)

	if got[0] != "config: SIGHUP reload failed, keeping previous settings" {
		t.Errorf("log = %q, want the failed reload reported", got)
	}
	if addr := mgr.Current().Server.Addr; addr != ":8081" {
		t.Errorf("server.addr = %q, want the previous :8081 kept", addr)
	}
}

// TestWatchReloadSignals_ReportsARejectedSettingAsApplied: a setting one
// component refused does not undo the reload -- the rest is live -- so it is
// logged as applied with a rejection, not as a failed reload.
func TestWatchReloadSignals_ReportsARejectedSettingAsApplied(t *testing.T) {
	path := withServeConfig(t, "server:\n  interval_ms: 1000\n")
	mgr, err := loadServeConfig(nil)
	if err != nil {
		t.Fatalf("loadServeConfig(): %v", err)
	}
	mgr.OnReload(func(*config.Config) error { return errors.New("the monitor refused the interval") })
	if err := os.WriteFile(path, []byte("server:\n  interval_ms: 2000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := reloadOnSIGHUP(t, mgr, 2)

	want := []string{"config: reloaded on SIGHUP", "config: reload applied, but some settings were rejected"}
	if got[0] != want[0] || got[1] != want[1] {
		t.Errorf("log = %q, want %q", got, want)
	}
}
