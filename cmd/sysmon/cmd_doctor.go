package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check runtime prerequisites and report what is degraded",
	Long: "Check what this build can and cannot do on this machine: whether the " +
		"desktop GUI libraries are present, whether privileged metrics such as " +
		"SMART are readable, and which optional data sources are available.\n\n" +
		"Every check names the fix. Nothing is installed or changed.",
	RunE: runDoctorCmd,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// checkResult is one prerequisite and its outcome.
type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, degraded or unavailable
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func runDoctorCmd(cmd *cobra.Command, _ []string) error {
	results := []checkResult{
		checkBuildFlavour(),
		checkDesktopGUI(),
		checkSMART(),
		checkCgroups(),
		checkRuntimeSocket(),
	}

	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Checks []checkResult `json:"checks"`
		}{Checks: results})
	}

	w := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(w, "sysmon doctor"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "═══════════════════════════════════════════════════"); err != nil {
		return err
	}

	for _, r := range results {
		mark := "✓"
		switch r.Status {
		case "degraded":
			mark = "!"
		case "unavailable":
			mark = "✗"
		}
		if _, err := fmt.Fprintf(w, "%s %-22s %s\n", mark, r.Name, r.Detail); err != nil {
			return err
		}
		if r.Fix != "" {
			if _, err := fmt.Fprintf(w, "  %-22s %s\n", "", r.Fix); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkBuildFlavour reports which of the two builds is running, since that
// determines whether a GUI is even possible.
func checkBuildFlavour() checkResult {
	if hasDesktopGUI() {
		return checkResult{
			Name:   "build",
			Status: "ok",
			Detail: "desktop build (GUI window, CLI and web server)",
		}
	}
	return checkResult{
		Name:   "build",
		Status: "ok",
		Detail: "server build (CLI and web server; no GUI window)",
	}
}

// checkDesktopGUI reports whether the desktop libraries are installed. On a
// server build this is informational; on a desktop build the binary could not
// have started without them.
// missingDesktopLibs is indirected so tests can exercise the missing-library
// branch on a machine that has them installed.
var missingDesktopLibs = collector.MissingDesktopLibs

func checkDesktopGUI() checkResult {
	missing := missingDesktopLibs()

	if len(missing) == 0 {
		if hasDesktopGUI() {
			return checkResult{Name: "desktop GUI", Status: "ok", Detail: "libraries present; run with no arguments to open the window"}
		}
		return checkResult{
			Name:   "desktop GUI",
			Status: "ok",
			Detail: "libraries present, but this build has no GUI compiled in",
			Fix:    "use the desktop binary (make build-desktop) to get a window",
		}
	}

	names := make([]string, 0, len(missing))
	for _, lib := range missing {
		names = append(names, lib.SOName)
	}

	return checkResult{
		Name:   "desktop GUI",
		Status: "unavailable",
		Detail: fmt.Sprintf("missing %v", names),
		Fix:    collector.InstallHint(missing),
	}
}

// diskHealth reports what the disk collector managed to read. It is a variable
// so tests can present the outcomes a single build host cannot produce: no
// devices, an unreadable one, and a mix.
var diskHealth = func() ([]types.DiskInfo, error) {
	dc := collector.NewDiskCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := dc.Collect(); err != nil {
		return nil, err
	}
	return dc.Info(), nil
}

// checkSMART reports whether disk health data is readable, which needs either
// root or membership of the disk group.
func checkSMART() checkResult {
	// Opening the block device proves nothing: the device node is often
	// readable while the SMART ioctl still returns EACCES. Run the real
	// collector and report what it actually managed to read.
	disks, err := diskHealth()
	if err != nil {
		return checkResult{Name: "SMART / disk health", Status: "unavailable", Detail: "disk collection failed"}
	}

	if len(disks) == 0 {
		return checkResult{Name: "SMART / disk health", Status: "unavailable", Detail: "no block devices found"}
	}

	enabled := 0
	for _, d := range disks {
		if d.SMARTEnabled {
			enabled++
		}
	}

	if enabled == len(disks) {
		return checkResult{
			Name:   "SMART / disk health",
			Status: "ok",
			Detail: fmt.Sprintf("readable on all %d device(s)", len(disks)),
		}
	}
	if enabled > 0 {
		return checkResult{
			Name:   "SMART / disk health",
			Status: "degraded",
			Detail: fmt.Sprintf("readable on %d of %d device(s)", enabled, len(disks)),
			Fix:    "sudo usermod -aG disk $USER   (then log out and back in), or run as root",
		}
	}

	return checkResult{
		Name:   "SMART / disk health",
		Status: "degraded",
		Detail: "no SMART data; temperatures and wear level unavailable",
		Fix:    "sudo usermod -aG disk $USER   (then log out and back in), or run as root",
	}
}

// cgroupV2Marker and cgroupV1Marker are the files whose presence identifies
// the cgroup hierarchy in use. They are variables so tests can present a
// hierarchy the build host is not running.
var (
	cgroupV2Marker = "/sys/fs/cgroup/cgroup.controllers"
	cgroupV1Marker = "/sys/fs/cgroup/memory"
)

// checkCgroups reports whether container metrics are possible.
func checkCgroups() checkResult {
	switch {
	case pathReadable(cgroupV2Marker):
		return checkResult{Name: "container metrics", Status: "ok", Detail: "cgroup v2 available"}
	case pathReadable(cgroupV1Marker):
		return checkResult{
			Name:   "container metrics",
			Status: "degraded",
			Detail: "cgroup v1 detected; per-container metrics need v2",
			Fix:    "boot with systemd.unified_cgroup_hierarchy=1",
		}
	default:
		return checkResult{Name: "container metrics", Status: "unavailable", Detail: "no cgroup filesystem"}
	}
}

// doctorRuntimeSockets are the container runtime sockets probed for image
// inventory. It is a variable so tests can point at a socket they control.
var doctorRuntimeSockets = []string{
	"/var/run/docker.sock",
	"/run/docker.sock",
	"/run/podman/podman.sock",
}

// checkRuntimeSocket reports whether image inventory can be collected.
func checkRuntimeSocket() checkResult {
	for _, sock := range doctorRuntimeSockets {
		// A unix socket must be connected to; open() on one fails with ENXIO
		// even when the caller has full access.
		conn, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if err != nil {
			continue
		}
		if closeErr := conn.Close(); closeErr != nil {
			continue
		}
		return checkResult{
			Name:   "image inventory",
			Status: "ok",
			Detail: "runtime socket reachable at " + sock,
		}
	}

	return checkResult{
		Name:   "image inventory",
		Status: "unavailable",
		Detail: "no reachable container runtime socket",
		Fix:    "sudo usermod -aG docker $USER   (then log out and back in)",
	}
}

// pathReadable reports whether a path exists and can be opened.
//
// #nosec G304 -- every caller passes one of the package-level cgroup marker
// constants. Nothing reaches this from user input or over the network.
func pathReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

// runStartupChecks reports environment problems once at startup.
//
// A monitor that silently renders zeroes because it lacks a permission is
// worse than one that says so: the reader cannot tell an idle machine from a
// blind one. Anything less than "ok" is logged with the command that fixes it.
// Returns the number of problems found.
func runStartupChecks() int {
	checks := []checkResult{
		checkSMART(),
		checkCgroups(),
		checkRuntimeSocket(),
	}

	// The desktop GUI check is only actionable on a build that has a GUI to
	// launch. On the server build -- which is what runs in a container, and on
	// every headless host -- missing GTK is the expected state, not a problem,
	// and warning about it on every start is pure noise. `sysmon doctor` still
	// reports it, because there the question being asked is "what can this
	// build do here".
	if hasDesktopGUI() {
		checks = append([]checkResult{checkDesktopGUI()}, checks...)
	}

	problems := 0
	for _, r := range checks {
		if r.Status == "ok" {
			continue
		}
		problems++
		attrs := []any{"check", r.Name, "detail", r.Detail}
		if r.Fix != "" {
			attrs = append(attrs, "fix", r.Fix)
		}
		slog.Warn("environment check", attrs...)
	}

	if problems > 0 {
		slog.Warn("environment checks found problems; some metrics will be missing",
			"count", problems, "detail", "run 'sysmon doctor' for the full report")
	}
	return problems
}
