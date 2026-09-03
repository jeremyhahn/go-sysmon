package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/memlimit"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check runtime prerequisites and report what is degraded",
	Long: "Check what this build can and cannot do on this machine: whether the " +
		"desktop GUI libraries are present, whether privileged metrics such as " +
		"SMART are readable, and which optional data sources are available.\n\n" +
		"A check names a remedy when there is one to name, and says nothing " +
		"when it cannot establish one. Nothing is installed or changed.",
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
		checkResourceLimits("", os.Getenv),
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

// smartCheckName is the label this check reports under.
const smartCheckName = "SMART / disk health"

// smartCandidate reports whether a device could have produced a SMART reading
// at all, which is what belongs in the denominator.
//
// Counting every block device put things in it that can never answer.
// ci-node-1 read "4 of 8" partly because one of the eight is a device-mapper
// node, which has no medium, and one is a boot card that reports it implements
// no SMART. Neither will ever be readable however the host is configured, and
// a denominator containing them is a target nobody can hit.
//
// ONLY TWO STATUSES ARE EXCLUDED, and behind_controller is deliberately NOT
// one of them. That status means "served by a RAID controller and did not
// answer directly" -- it does NOT establish that the device is a volume rather
// than a passthrough disk, which is stated in its own definition and is not
// knowable from sysfs. Excluding it would let a JBOD disk that genuinely fails
// to answer disappear from the count, which is the reverse of the defect this
// check is being fixed for: a gap hidden by shrinking the denominator until it
// closes.
func smartCandidate(status types.SMARTStatus) bool {
	switch status {
	case types.SMARTNotApplicable, types.SMARTUnsupported:
		return false
	}
	// An unclassified device is counted, so an older or unexpected payload
	// errs toward reporting a gap rather than hiding one.
	return true
}

// smartTally is what one pass over the devices establishes.
type smartTally struct {
	candidates int
	readable   int
	excluded   int
	// members counts how many of the candidates are drives behind a RAID
	// controller, so the detail line can say where the extra devices came
	// from. Without it "10 of 11" on a host with 8 block devices reads like
	// an error rather than like six drives nobody could see before.
	members int
	// reasons counts how many devices gave each distinct explanation for
	// having no reading, and order preserves first appearance.
	reasons map[string]int
	order   []string
}

// addMember records a drive behind a RAID controller, which counts exactly as
// a block device does except that it is also tallied separately for the
// detail line.
func (t *smartTally) addMember(status types.SMARTStatus, enabled bool, reason string) {
	if smartCandidate(status) {
		t.members++
	}
	t.add(status, enabled, reason)
}

func (t *smartTally) add(status types.SMARTStatus, enabled bool, reason string) {
	if !smartCandidate(status) {
		t.excluded++
		return
	}
	t.candidates++
	if enabled {
		t.readable++
		return
	}
	if reason == "" {
		return
	}
	if t.reasons == nil {
		t.reasons = make(map[string]int)
	}
	if !slices.Contains(t.order, reason) {
		t.order = append(t.order, reason)
	}
	t.reasons[reason]++
}

// commonest returns the explanation the most devices gave, and how many other
// distinct explanations there were.
//
// One line, always. Reasons are prose from the collector and some of them
// embed a device path and an errno, so they do not deduplicate across devices
// and joining them all would put a paragraph on a terminal line and in a JSON
// field. The most frequent one is the systemic thing worth acting on; the rest
// are counted rather than printed.
func (t *smartTally) commonest() (string, int) {
	best, bestN := "", 0
	for _, r := range t.order {
		if n := t.reasons[r]; n > bestN {
			best, bestN = r, n
		}
	}
	return best, len(t.order) - 1
}

// checkSMART reports whether disk health data is readable.
func checkSMART() checkResult {
	// Opening the block device proves nothing: the device node is often
	// readable while the SMART ioctl still returns EACCES. Run the real
	// collector and report what it actually managed to read.
	disks, err := diskHealth()
	if err != nil {
		return checkResult{Name: smartCheckName, Status: "unavailable", Detail: "disk collection failed"}
	}
	if len(disks) == 0 {
		return checkResult{Name: smartCheckName, Status: "unavailable", Detail: "no block devices found"}
	}

	var tally smartTally
	for _, d := range disks {
		tally.add(d.SMARTStatus, d.SMARTEnabled, d.SMARTReason)
		// The drives behind a hardware RAID controller. They are not block
		// devices and are in no disk list, so a count summed over the list
		// alone reported ci-node-1 as reading four devices while ten
		// answered -- a denominator that does not know what it left out.
		//
		// The volume they hang off is counted too, and separately: it is a
		// device we could not read directly, which is a different fact from
		// its members being readable. Neither substitutes for the other.
		for i := range d.Members {
			m := &d.Members[i]
			tally.addMember(m.SMARTStatus, m.SMARTEnabled, m.SMARTReason)
		}
	}

	// Nothing on this host can report SMART. That is a fact about the
	// hardware, not a fault to be fixed, and offering a remedy for it sends
	// somebody to change a group membership that will make no difference.
	if tally.candidates == 0 {
		return checkResult{
			Name:   smartCheckName,
			Status: "unavailable",
			Detail: fmt.Sprintf("no device on this host reports SMART (%d device(s) cannot)", tally.excluded),
		}
	}

	detail := fmt.Sprintf("readable on %d of %d device(s) that can report it", tally.readable, tally.candidates)
	if tally.members > 0 {
		detail += fmt.Sprintf(" (%d behind a RAID controller)", tally.members)
	}
	if tally.readable == tally.candidates {
		detail = fmt.Sprintf("readable on all %d device(s) that can report it", tally.candidates)
		if tally.members > 0 {
			detail += fmt.Sprintf(" (%d behind a RAID controller)", tally.members)
		}
	}
	if tally.excluded > 0 {
		detail += fmt.Sprintf("; %d cannot", tally.excluded)
	}

	if tally.readable == tally.candidates {
		return checkResult{Name: smartCheckName, Status: "ok", Detail: detail}
	}

	// The remedy comes from the devices rather than from a constant. This
	// check used to offer "add the user to the disk group" for every failure,
	// which on ci-node-1 was advice to do something already done: the node
	// opens there and it is the passthrough ioctl that is refused, needing a
	// capability no group membership grants. The collector already tells those
	// two apart; the reasons it produced are carried through.
	return checkResult{
		Name:   smartCheckName,
		Status: "degraded",
		Detail: detail,
		Fix:    smartFix(&tally),
	}
}

// smartFix renders the remedy from what the devices said.
//
// There is NO FALLBACK to the old "add the user to the disk group" advice. A
// device that did not explain itself supports "there is a gap" and supports
// nothing at all about permissions, and guessing a permissions diagnosis is
// how this check came to tell ci-node-1's operator to join a group the
// service was already in. An empty Fix says we do not know, which is true.
func smartFix(t *smartTally) string {
	reason, others := t.commonest()
	if reason == "" {
		return ""
	}
	if others > 0 {
		return fmt.Sprintf("%s (and %d other reason(s) on other devices)", reason, others)
	}
	return reason
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

// resourceCheckName is the doctor entry for the limits this process runs under.
const resourceCheckName = "resource limits"

// checkResourceLimits reports the cgroup ceilings and what the Go runtime was
// told about them.
//
// It exists because both halves of that sentence have been wrong in production
// on the same day (#65). A CPU quota froze the process mid-collection until it
// stopped serving, and a memory watermark was raised above the hard limit that
// enforces first, which made the watermark unreachable and its counter read
// zero -- a configuration that looks more generous than the one it replaced and
// whose own metric agrees with it. Neither showed up anywhere a person would
// look, so this is where a person looks.
//
// GOMAXPROCS is reported alongside because it is derived from the CPU the
// runtime believes it has. On a 112-CPU host under a half-core quota it was
// 112, which provisions garbage collection for a machine the process is not
// allowed to use.
// root is the filesystem root to read cgroup files under and getenv resolves
// environment variables: "" and os.Getenv for the running system, a fixture
// directory and a stub in tests.
//
// getenv is a parameter and not os.Getenv directly because an inherited
// GOMEMLIMIT short-circuits the whole derivation. The first version of this
// took only root, and every case passed locally and failed on CI -- whose
// runner image sets GOMEMLIMIT, so the fixture root was never read and the
// check reported "set by the environment" for all five cases. A fixture that
// controls one input while another can divert the code path tests nothing.
func checkResourceLimits(root string, getenv func(string) string) checkResult {
	detail := "GOMAXPROCS=" + strconv.Itoa(runtime.GOMAXPROCS(0))

	l, err := memlimit.Detect(root, getenv)
	if err != nil {
		return checkResult{
			Name:   resourceCheckName,
			Status: "unavailable",
			Detail: detail + "; cgroup memory ceilings unreadable: " + err.Error(),
		}
	}

	switch {
	case l.Incoherent:
		return checkResult{
			Name:   resourceCheckName,
			Status: "degraded",
			Detail: detail + "; MemoryHigh (" + humanBytes(l.High) + ") is at or above MemoryMax (" +
				humanBytes(l.Max) + "), so the throttle watermark can never be reached and the service " +
				"is OOM-killed instead of reclaimed",
			Fix: "raise MemoryMax above MemoryHigh in the unit, or clear MemoryHigh",
		}
	case l.Source == "":
		detail += "; no cgroup memory ceiling, Go default heap growth"
	case l.Applied == 0:
		detail += "; GOMEMLIMIT set by the environment"
	default:
		detail += "; " + l.Source + "=" + humanBytes(l.Ceiling()) +
			", GOMEMLIMIT=" + humanBytes(l.Applied)
	}

	return checkResult{Name: resourceCheckName, Status: "ok", Detail: detail}
}

// humanBytes renders a byte count in the units the systemd directives use, so
// the report can be compared with the unit file without arithmetic.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return strconv.FormatInt(n/(1<<30), 10) + "G"
	case n >= 1<<20:
		return strconv.FormatInt(n/(1<<20), 10) + "M"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}
