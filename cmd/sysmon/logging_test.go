//go:build !desktop

package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withLogFlags sets the logging flags for one test and restores them after,
// including the global slog default the setup replaces.
func withLogFlags(t *testing.T, file, level, format string) {
	t.Helper()
	origFile, origLevel, origFormat := logFile, logLevel, logFormat
	origLogger := slog.Default()

	logFile, logLevel, logFormat = file, level, format

	t.Cleanup(func() {
		closeLogFile()
		logFile, logLevel, logFormat = origFile, origLevel, origFormat
		slog.SetDefault(origLogger)
	})
}

// ---- parseLogLevel --------------------------------------------------------

func TestParseLogLevel_Accepted(t *testing.T) {
	t.Parallel()
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"WARNING": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for name, want := range tests {
		got, err := parseLogLevel(name)
		if err != nil {
			t.Errorf("parseLogLevel(%q) error = %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseLogLevel_Rejected(t *testing.T) {
	t.Parallel()
	_, err := parseLogLevel("verbose")
	if err == nil {
		t.Fatal("parseLogLevel(verbose) = nil error, want a rejection")
	}
	var invalid *types.InvalidLogLevelError
	if !errors.As(err, &invalid) {
		t.Errorf("error = %T, want *types.InvalidLogLevelError", err)
	}
	if !strings.Contains(err.Error(), "debug") {
		t.Errorf("error does not list the valid levels: %v", err)
	}
}

// ---- setupLogging ---------------------------------------------------------

// TestSetupLogging_WritesToFile verifies logs land in the requested file and
// that missing parent directories are created rather than failing.
func TestSetupLogging_WritesToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "sysmon.log")
	withLogFlags(t, path, "info", "text")

	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging() error = %v", err)
	}
	slog.Info("a message that must reach the file", "key", "value")
	closeLogFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
	if !strings.Contains(string(raw), "a message that must reach the file") {
		t.Errorf("log file does not contain the message; got:\n%s", raw)
	}
}

// TestSetupLogging_AppendsAcrossRuns is the regression test for a restart
// discarding the history that explains why the previous run stopped.
func TestSetupLogging_AppendsAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.log")

	withLogFlags(t, path, "info", "text")
	if err := setupLogging(); err != nil {
		t.Fatalf("first setupLogging: %v", err)
	}
	slog.Info("first run")
	closeLogFile()

	if err := setupLogging(); err != nil {
		t.Fatalf("second setupLogging: %v", err)
	}
	slog.Info("second run")
	closeLogFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, want := range []string{"first run", "second run"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("log was truncated; %q missing from:\n%s", want, raw)
		}
	}
}

// TestSetupLogging_JSONFormat verifies the machine-readable handler is used.
func TestSetupLogging_JSONFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.log")
	withLogFlags(t, path, "info", "json")

	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging() error = %v", err)
	}
	slog.Info("structured", "field", 42)
	closeLogFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(raw), `"msg":"structured"`) {
		t.Errorf("output is not JSON; got:\n%s", raw)
	}
}

// TestSetupLogging_LevelFiltering verifies a raised level suppresses lower
// messages, which is how an operator quiets a busy log.
func TestSetupLogging_LevelFiltering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.log")
	withLogFlags(t, path, "error", "text")

	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging() error = %v", err)
	}
	slog.Info("this must be filtered out")
	slog.Error("this must survive")
	closeLogFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), "filtered out") {
		t.Errorf("an INFO message survived a level=error filter:\n%s", raw)
	}
	if !strings.Contains(string(raw), "this must survive") {
		t.Errorf("the ERROR message was dropped:\n%s", raw)
	}
}

func TestSetupLogging_RejectsBadFormat(t *testing.T) {
	withLogFlags(t, "", "info", "yaml")

	err := setupLogging()
	if err == nil {
		t.Fatal("setupLogging() = nil, want a rejection for an unknown format")
	}
	var invalid *types.InvalidLogFormatError
	if !errors.As(err, &invalid) {
		t.Errorf("error = %T, want *types.InvalidLogFormatError", err)
	}
}

// TestSetupLogging_UnwritablePathReports verifies a bad path is reported
// rather than silently leaving logs on stderr.
func TestSetupLogging_UnwritablePathReports(t *testing.T) {
	// A path under a regular file cannot be created as a directory.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	withLogFlags(t, filepath.Join(blocker, "sysmon.log"), "info", "text")

	err := setupLogging()
	if err == nil {
		t.Fatal("setupLogging() = nil, want an error for an uncreatable path")
	}
	var logErr *types.LogFileError
	if !errors.As(err, &logErr) {
		t.Errorf("error = %T, want *types.LogFileError", err)
	}
}

// TestSetupLogging_DefaultsToStderr verifies the CLI keeps logs off stdout so
// that --json output stays machine-parseable.
func TestSetupLogging_DefaultsToStderr(t *testing.T) {
	withLogFlags(t, "", "info", "text")

	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging() error = %v", err)
	}
	if logFileHandle != nil {
		t.Error("no --log-file was given, but a file was opened")
	}
}

func TestCloseLogFile_Idempotent(t *testing.T) {
	withLogFlags(t, filepath.Join(t.TempDir(), "sysmon.log"), "info", "text")
	if err := setupLogging(); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}

	closeLogFile()
	// A second close must not panic or double-close a descriptor.
	closeLogFile()
}
