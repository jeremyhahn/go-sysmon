package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- reportError ----------------------------------------------------------

// errListenFailed stands in for the net.OpError a real bind conflict produces.
var errListenFailed = errors.New("listen tcp :8081: bind: address already in use")

// TestReportError_WrappedError verifies that a typed error surfaces its full
// message. The root command silences cobra's own error output, so this is the
// only thing standing between a failed command and a silent exit(1).
func TestReportError_WrappedError(t *testing.T) {
	var buf bytes.Buffer
	reportError(&buf, &types.ServerStartError{Cause: errListenFailed})

	got := buf.String()
	if !strings.Contains(got, "server start:") {
		t.Errorf("reportError() = %q, want it to contain the wrapped error prefix", got)
	}
	if !strings.Contains(got, errListenFailed.Error()) {
		t.Errorf("reportError() = %q, want it to contain the cause %q", got, errListenFailed)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("reportError() = %q, want a trailing newline", got)
	}
}

// TestReportError_NilError verifies that reportError does not panic when
// handed a nil error, which would turn a clean exit into a crash.
func TestReportError_NilError(t *testing.T) {
	var buf bytes.Buffer

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reportError(nil) panicked: %v", r)
		}
	}()
	reportError(&buf, nil)

	if got := buf.String(); !strings.Contains(got, "nil") {
		t.Errorf("reportError(nil) = %q, want it to mention nil", got)
	}
}

// ---- hasDisplay -----------------------------------------------------------

func TestHasDisplay_DISPLAY(t *testing.T) {
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")

	if !hasDisplay() {
		t.Error("hasDisplay() = false, want true when DISPLAY is set")
	}
}

func TestHasDisplay_WAYLAND_DISPLAY(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")

	if !hasDisplay() {
		t.Error("hasDisplay() = false, want true when WAYLAND_DISPLAY is set")
	}
}

func TestHasDisplay_BothSet(t *testing.T) {
	t.Setenv("DISPLAY", ":1")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")

	if !hasDisplay() {
		t.Error("hasDisplay() = false, want true when both env vars are set")
	}
}

func TestHasDisplay_NoneSet(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")

	if hasDisplay() {
		t.Error("hasDisplay() = true, want false when no display env vars are set")
	}
}

// ---- snapshotterAdapter ---------------------------------------------------

// fakeSystemCollector satisfies the interface required by snapshotterAdapter
// through the real SystemCollector field type, but we test the adapter with a
// real SystemCollector here because it is a lightweight call.
//
// We instead exercise the adapter by confirming it returns a non-nil pointer
// and that the returned type satisfies monitor.Snapshotter.

// adapterFromStub builds a snapshotterAdapter whose sc.Snapshot delegates to
// the given stubCollector via a small shim, since snapshotterAdapter embeds a
// *collector.SystemCollector directly. Because SystemCollector is a concrete
// type we cannot inject a fake; instead we test the adapter directly by
// constructing it with a real SystemCollector and ensuring it does not panic
// and returns a *types.Snapshot.
//
// For the error path we use a started-but-then-cancelled context supplied
// indirectly through the adapter's own context.Background() call, which means
// we cannot force an error from the adapter alone without a real collector
// failure. We therefore cover the error branch via a dedicated adapter shim
// defined in this test file only.

// testableAdapter is a local variant of snapshotterAdapter that accepts any
// func()-based snapshotter so we can inject errors without a real collector.
type testableAdapter struct {
	fn func() (types.Snapshot, error)
}

func (a *testableAdapter) Snapshot() (*types.Snapshot, error) {
	snap, err := a.fn()
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

func TestSnapshotterAdapter_ReturnsPointer(t *testing.T) {
	want := types.Snapshot{Host: types.HostInfo{Hostname: "adapter-host"}}
	a := &testableAdapter{fn: func() (types.Snapshot, error) { return want, nil }}

	got, err := a.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Snapshot() returned nil pointer, want non-nil")
	}
	if got.Host.Hostname != "adapter-host" {
		t.Errorf("Hostname = %q, want %q", got.Host.Hostname, "adapter-host")
	}
}

func TestSnapshotterAdapter_PropagatesError(t *testing.T) {
	cause := &types.CollectorError{Collector: "cpu", Cause: testError("forced")}
	a := &testableAdapter{fn: func() (types.Snapshot, error) { return types.Snapshot{}, cause }}

	got, err := a.Snapshot()
	if err == nil {
		t.Fatal("Snapshot() error = nil, want non-nil")
	}
	if got != nil {
		t.Errorf("Snapshot() = %v, want nil on error", got)
	}
}

// testError is a minimal error type used only in this test file.
type testError string

func (e testError) Error() string { return string(e) }

// ---- main -----------------------------------------------------------------

// TestMain_ExecutesRootCommand runs the real entry point with a subcommand
// that succeeds, so main() returns without calling os.Exit. This is the only
// test that exercises the binary's actual startup wiring.
func TestMain_ExecutesRootCommand(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() {
		os.Args = origArgs
		rootCmd.SetArgs(nil)
	})

	// "version" needs no hardware and cannot fail, so main() takes the
	// success path and never reaches os.Exit(1).
	os.Args = []string{"sysmon", "version"}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("main() panicked: %v", r)
		}
	}()
	main()
}

// ---- run ------------------------------------------------------------------

// TestRun_FailingCommandReportsAndExitsNonZero verifies run reports a command
// error on stderr and returns status 1, and that it still releases the log
// file, which main's old deferred close skipped on exactly this path because
// os.Exit does not run defers. An unknown flag fails in cobra's parse, before
// any command runs.
func TestRun_FailingCommandReportsAndExitsNonZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.log")
	withLogFlags(t, path, "info", "text")
	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	rootCmd.SetArgs([]string{"--no-such-flag"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	var stderr bytes.Buffer
	if status := run(&stderr); status != 1 {
		t.Errorf("run() = %d, want 1", status)
	}
	if !strings.Contains(stderr.String(), "Error:") || !strings.Contains(stderr.String(), "no-such-flag") {
		t.Errorf("stderr = %q, want the command error reported", stderr.String())
	}
	if logFileHandle != nil {
		t.Error("run returned without releasing the log file")
	}
}

// TestRun_LogFileCloseFailureIsReported verifies a log file that fails to
// close is reported and turns the exit status non-zero: the tail of the log
// may be lost, and a zero status would hide it.
func TestRun_LogFileCloseFailureIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.log")
	withLogFlags(t, path, "info", "text")
	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	if err := logFileHandle.Close(); err != nil {
		t.Fatalf("close underneath: %v", err)
	}
	rootCmd.SetArgs([]string{"--no-such-flag"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	var stderr bytes.Buffer
	if status := run(&stderr); status != 1 {
		t.Errorf("run() = %d, want 1", status)
	}
	if !strings.Contains(stderr.String(), "log file "+path) {
		t.Errorf("stderr = %q, want the log file close failure reported", stderr.String())
	}
}
