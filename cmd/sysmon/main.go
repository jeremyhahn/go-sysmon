// Package main is the entry point for the sysmon application.
// It supports three operating modes:
//
//   - Desktop GUI mode: launched when the binary is built with the "desktop"
//     build tag, no command-line arguments are provided, and a display server
//     is detected (DISPLAY or WAYLAND_DISPLAY).
//   - Overview mode: prints a one-shot system overview when no display is
//     available and no subcommand is given.
//   - Subcommand mode: runs the specified CLI subcommand.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var (
	version   = "dev"
	gitCommit = "unknown"
	buildDate = "unknown"
)

// jsonOutput is set by the global --json flag.
var jsonOutput bool

// refreshInterval is set by the global --refresh flag (seconds, 0 = disabled).
var refreshInterval float64

var rootCmd = &cobra.Command{
	Use:           "sysmon",
	Short:         "Real-time system monitoring tool",
	Long:          "A desktop application and CLI for real-time system monitoring.",
	SilenceUsage:  true,
	SilenceErrors: true,
	// Logging is configured before any subcommand runs so that every command,
	// not just serve, honours --log-file and --log-level.
	PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
		return setupLogging()
	},
	RunE: runRoot,
}

// noTray disables the system tray icon when set via --no-tray.
var noTray bool

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output as JSON instead of formatted text")
	rootCmd.PersistentFlags().Float64Var(&refreshInterval, "refresh", 0, "Refresh interval in seconds (e.g. 1, 0.5); 0 = one-shot")
	rootCmd.PersistentFlags().BoolVar(&noTray, "no-tray", false, "Disable system tray icon")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "",
		"Write logs to this file instead of stderr; parent directories are created")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Log level: debug, info, warn or error")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text",
		"Log format: text or json")
}

func main() {
	defer closeLogFile()

	if err := rootCmd.Execute(); err != nil {
		reportError(os.Stderr, err)
		os.Exit(1)
	}
}

// reportError writes err to w. The root command sets SilenceErrors so that
// cobra does not print errors itself; without this the process would exit
// non-zero with no explanation (e.g. a "serve" address already in use).
func reportError(w io.Writer, err error) {
	fmt.Fprintln(w, "Error:", err)
}

// runRoot handles the case where sysmon is invoked with no subcommand.
// It launches the GUI when a display is available; otherwise it falls
// through to the overview command.
func runRoot(cmd *cobra.Command, args []string) error {
	if hasDisplay() {
		err := launchGUI()
		if err == nil {
			return nil
		}
		var guiErr *types.GUIUnavailableError
		if !errors.As(err, &guiErr) {
			// Unexpected GUI error: propagate it.
			return err
		}
		// GUI unavailable (stub build): fall through to overview.
	}
	return runOverviewCmd(cmd, args)
}

// hasDisplay reports whether a display server is available on the current
// platform by inspecting the DISPLAY (X11) and WAYLAND_DISPLAY environment
// variables.
func hasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
