package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Logging flags, applied to every subcommand.
var (
	logFile   string
	logLevel  string
	logFormat string
)

// logFileHandle is kept so the file can be closed on shutdown.
var logFileHandle *os.File

// setupLogging installs the process-wide slog handler.
//
// Without --log-file, diagnostics go to stderr as before. That matters for the
// CLI: --json output goes to stdout, so keeping logs on stderr is what lets
// `sysmon cpu --json | jq` work. With --log-file they are written to that file
// instead, which is what a long-running server wants.
func setupLogging() error {
	level, err := parseLogLevel(logLevel)
	if err != nil {
		return err
	}

	dest, err := logDestination()
	if err != nil {
		return err
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(logFormat) {
	case "json":
		handler = slog.NewJSONHandler(dest, opts)
	case "text", "":
		handler = slog.NewTextHandler(dest, opts)
	default:
		return &types.InvalidLogFormatError{Format: logFormat}
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

// logDestination opens the log file when one was requested, creating any
// missing parent directories, and otherwise returns stderr.
func logDestination() (io.Writer, error) {
	if logFile == "" {
		return os.Stderr, nil
	}

	// 0750, not 0755: the logs below record hostnames, process lists and
	// device serial numbers, so the directory should not be world-traversable.
	dir := filepath.Dir(logFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, &types.LogFileError{Path: logFile, Cause: err}
		}
	}

	// Append rather than truncate: a restart must not discard the history that
	// explains why the previous run stopped. 0600 because the contents are
	// host inventory, which no other local user needs to read.
	//
	// #nosec G304 -- logFile is the value of the operator's own --log-file
	// flag. There is no untrusted input on this path; a caller who can set the
	// flag can already run the binary.
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, &types.LogFileError{Path: logFile, Cause: err}
	}

	logFileHandle = f
	return f, nil
}

// closeLogFile releases the log file if one was opened.
func closeLogFile() {
	if logFileHandle == nil {
		return
	}
	if err := logFileHandle.Close(); err != nil {
		// The handler may already be writing here, so report to stderr.
		os.Stderr.WriteString("closing log file: " + err.Error() + "\n") //nolint:errcheck
	}
	logFileHandle = nil
}

// parseLogLevel converts a level name to a slog level.
func parseLogLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, &types.InvalidLogLevelError{Level: name}
	}
}
