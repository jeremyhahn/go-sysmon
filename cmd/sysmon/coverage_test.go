package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- doctor ---------------------------------------------------------------

// TestCheckCgroups_ReportsHierarchy verifies each cgroup arrangement is
// classified correctly. Container metrics come from cgroup v2 only, so a v1
// host has to be told that rather than shown empty tables.
func TestCheckCgroups_ReportsHierarchy(t *testing.T) {
	dir := t.TempDir()
	v2 := filepath.Join(dir, "cgroup.controllers")
	v1 := filepath.Join(dir, "memory")
	for _, p := range []string{v2, v1} {
		if err := os.WriteFile(p, []byte("cpu memory\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	absent := filepath.Join(dir, "absent")

	origV2, origV1 := cgroupV2Marker, cgroupV1Marker
	t.Cleanup(func() { cgroupV2Marker, cgroupV1Marker = origV2, origV1 })

	tests := []struct {
		name       string
		v2, v1     string
		wantStatus string
		wantDetail string
	}{
		{"cgroup v2", v2, v1, "ok", "v2"},
		{"cgroup v1 only", absent, v1, "degraded", "v1"},
		{"no cgroups at all", absent, absent, "unavailable", "no cgroup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cgroupV2Marker, cgroupV1Marker = tt.v2, tt.v1

			got := checkCgroups()
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to mention %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

// TestCheckRuntimeSocket_ReachableSocket verifies a listening runtime socket is
// reported as usable, and names the socket so an operator can tell docker from
// podman.
func TestCheckRuntimeSocket_ReachableSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	orig := doctorRuntimeSockets
	doctorRuntimeSockets = []string{filepath.Join(t.TempDir(), "absent.sock"), sock}
	t.Cleanup(func() { doctorRuntimeSockets = orig })

	got := checkRuntimeSocket()
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if !strings.Contains(got.Detail, sock) {
		t.Errorf("Detail = %q, want it to name %q", got.Detail, sock)
	}
}

// TestCheckRuntimeSocket_NoRuntime verifies the absence of a runtime is
// reported with the command that grants access, since a permission problem
// looks identical to "not installed" from here.
func TestCheckRuntimeSocket_NoRuntime(t *testing.T) {
	orig := doctorRuntimeSockets
	doctorRuntimeSockets = []string{filepath.Join(t.TempDir(), "absent.sock")}
	t.Cleanup(func() { doctorRuntimeSockets = orig })

	got := checkRuntimeSocket()
	if got.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", got.Status)
	}
	if got.Fix == "" {
		t.Error("Fix is empty; an unavailable check must say how to fix it")
	}
}

// TestPathReadable verifies the helper distinguishes a readable file from one
// that is absent.
func TestPathReadable(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !pathReadable(present) {
		t.Error("pathReadable() = false for a readable file")
	}
	if pathReadable(filepath.Join(dir, "absent")) {
		t.Error("pathReadable() = true for a file that does not exist")
	}
	// A directory opens successfully; the check is "can this be opened", which
	// is what the callers rely on for the cgroup markers.
	if !pathReadable(dir) {
		t.Error("pathReadable() = false for a readable directory")
	}
}

// TestCheckBuildFlavour_NamesTheBuild verifies the build is identified, since
// which binary is running determines whether a GUI is possible at all.
func TestCheckBuildFlavour_NamesTheBuild(t *testing.T) {
	got := checkBuildFlavour()
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if !strings.Contains(got.Detail, "build") {
		t.Errorf("Detail = %q, want it to name the build flavour", got.Detail)
	}
}

// TestCheckSMART_ReportsReadability verifies the SMART check produces a
// classified result on whatever hardware the test runs on. The status depends
// on privileges, so only the shape is asserted.
func TestCheckSMART_ReportsReadability(t *testing.T) {
	got := checkSMART()

	switch got.Status {
	case "ok", "degraded", "unavailable":
	default:
		t.Errorf("Status = %q, want ok, degraded or unavailable", got.Status)
	}
	if got.Name != "SMART / disk health" {
		t.Errorf("Name = %q, want %q", got.Name, "SMART / disk health")
	}
	if got.Detail == "" {
		t.Error("Detail is empty")
	}
	if got.Status == "degraded" && got.Fix == "" {
		t.Error("a degraded SMART check must say how to fix it")
	}
}

// ---- vms ------------------------------------------------------------------

// TestFilterVMByIndex_NotFound verifies asking for a guest that is not running
// is a typed error naming the index, not an empty table.
func TestFilterVMByIndex_NotFound(t *testing.T) {
	vms := []types.VMInfo{{Index: 0, Name: "win11"}, {Index: 1, Name: "ubuntu"}}

	got, err := filterVMByIndex(vms, 1)
	if err != nil {
		t.Fatalf("filterVMByIndex(1) error = %v", err)
	}
	if got.Name != "ubuntu" {
		t.Errorf("filterVMByIndex(1) = %q, want ubuntu", got.Name)
	}

	if _, err := filterVMByIndex(vms, 7); err == nil {
		t.Fatal("filterVMByIndex(7) = nil error, want a not-found error")
	} else if !strings.Contains(err.Error(), "7") {
		t.Errorf("error = %q, want it to name the index", err)
	}
}

// ---- snapshot plumbing ----------------------------------------------------

// TestCmdContext_IsUsable verifies one-shot commands get a live context.
func TestCmdContext_IsUsable(t *testing.T) {
	ctx := cmdContext()
	if ctx == nil {
		t.Fatal("cmdContext() = nil")
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("cmdContext() is already done: %v", err)
	}
}

// TestSnapshotterAdapter_ReturnsSnapshot verifies the monitor adapter forwards
// a real collection rather than a zero value.
func TestSnapshotterAdapter_ReturnsSnapshot(t *testing.T) {
	sc := collector.NewSystemCollector(nil)
	sc.SetTiering(false)
	adapter := &snapshotterAdapter{sc: sc}

	snap, err := adapter.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snap == nil || snap.Host.Hostname == "" {
		t.Fatalf("Snapshot() returned no host detail: %+v", snap)
	}
}

// TestCheckSMART_EveryOutcome verifies each level of disk-health readability
// is classified and, where something can be done about it, says so. A single
// build host can only produce one of these, so the source is substituted.
func TestCheckSMART_EveryOutcome(t *testing.T) {
	orig := diskHealth
	t.Cleanup(func() { diskHealth = orig })

	tests := []struct {
		name       string
		disks      []types.DiskInfo
		err        error
		wantStatus string
		wantFix    bool
	}{
		{
			name:       "collection failed",
			err:        errors.New("collect: permission denied"),
			wantStatus: "unavailable",
		},
		{
			name:       "no block devices",
			disks:      nil,
			wantStatus: "unavailable",
		},
		{
			name:       "readable everywhere",
			disks:      []types.DiskInfo{{SMARTEnabled: true}, {SMARTEnabled: true}},
			wantStatus: "ok",
		},
		{
			name:       "readable on some devices",
			disks:      []types.DiskInfo{{SMARTEnabled: true}, {SMARTEnabled: false}},
			wantStatus: "degraded",
			wantFix:    true,
		},
		{
			name:       "readable nowhere",
			disks:      []types.DiskInfo{{SMARTEnabled: false}},
			wantStatus: "degraded",
			wantFix:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diskHealth = func() ([]types.DiskInfo, error) { return tt.disks, tt.err }

			got := checkSMART()
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q (detail: %s)", got.Status, tt.wantStatus, got.Detail)
			}
			if tt.wantFix && got.Fix == "" {
				t.Error("Fix is empty; a degraded check must say how to fix it")
			}
			if got.Detail == "" {
				t.Error("Detail is empty")
			}
		})
	}
}

// failAfterWriter fails once a given number of writes have succeeded, so each
// write in a rendering loop can be made the failing one.
type failAfterWriter struct {
	ok   int
	seen int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.seen++
	if w.seen > w.ok {
		return 0, errors.New("output stream closed")
	}
	return len(p), nil
}

// TestRunDoctorCmd_ReportsWriteFailures verifies a dead output stream stops the
// report and is returned, rather than being swallowed at whichever line it
// happened on. Piping into a closed `head` is the everyday way this happens.
func TestRunDoctorCmd_ReportsWriteFailures(t *testing.T) {
	resetGlobals(t)

	// A degraded check so the report also renders a fix line.
	dir := t.TempDir()
	v1 := filepath.Join(dir, "memory")
	if err := os.WriteFile(v1, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	origV2, origV1 := cgroupV2Marker, cgroupV1Marker
	cgroupV2Marker, cgroupV1Marker = filepath.Join(dir, "absent"), v1
	t.Cleanup(func() { cgroupV2Marker, cgroupV1Marker = origV2, origV1 })

	origHealth := diskHealth
	diskHealth = func() ([]types.DiskInfo, error) {
		return []types.DiskInfo{{SMARTEnabled: false}}, nil
	}
	t.Cleanup(func() { diskHealth = origHealth })

	// Every prefix length up to the full report: each one makes a different
	// write the failing one.
	for ok := range 8 {
		cmd := newTestCmd(nil)
		cmd.SetOut(&failAfterWriter{ok: ok})

		if err := runDoctorCmd(cmd, nil); err == nil {
			t.Errorf("runDoctorCmd() = nil after %d successful writes, want an error", ok)
		}
	}
}

// TestRunDoctorCmd_RendersDegradedChecks verifies a degraded result is marked
// differently from a healthy one and carries its fix, so a scan of the output
// shows what needs attention.
func TestRunDoctorCmd_RendersDegradedChecks(t *testing.T) {
	resetGlobals(t)

	dir := t.TempDir()
	v1 := filepath.Join(dir, "memory")
	if err := os.WriteFile(v1, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	origV2, origV1 := cgroupV2Marker, cgroupV1Marker
	cgroupV2Marker, cgroupV1Marker = filepath.Join(dir, "absent"), v1
	t.Cleanup(func() { cgroupV2Marker, cgroupV1Marker = origV2, origV1 })

	var buf bytes.Buffer
	if err := runDoctorCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runDoctorCmd() error = %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "cgroup v1") {
		t.Errorf("the degraded cgroup check is missing from the report:\n%s", out)
	}
	if !strings.Contains(out, "systemd.unified_cgroup_hierarchy=1") {
		t.Errorf("the degraded check's fix is missing from the report:\n%s", out)
	}
	if !strings.Contains(out, "!") {
		t.Errorf("degraded checks are not marked distinctly:\n%s", out)
	}
}

// TestRunVMsCmd_SelectsSingleGuest verifies --index narrows the output to the
// matching guest. The out-of-range case is covered elsewhere; this is the
// branch that has to keep the right one.
func TestRunVMsCmd_SelectsSingleGuest(t *testing.T) {
	resetGlobals(t)
	snap := fixtureSnapshot()
	snap.Virt.VMs = []types.VMInfo{
		{Index: 0, Name: "win11", VCPUs: 8},
		{Index: 1, Name: "ubuntu", VCPUs: 4},
	}
	withFixtureSnapshot(t, snap, nil)

	vmIndex = 1
	jsonOutput = true

	var buf bytes.Buffer
	if err := runVMsCmd(newTestCmd(&buf), nil); err != nil {
		t.Fatalf("runVMsCmd() error = %v", err)
	}

	var out struct {
		VMs []types.VMInfo `json:"vms"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode output: %v\n%s", err, buf.String())
	}
	if len(out.VMs) != 1 || out.VMs[0].Name != "ubuntu" {
		t.Errorf("got %+v, want only the guest at index 1", out.VMs)
	}
}

// TestRunVirtCmds_ReportRenderFailure verifies a dead output stream is
// reported by the virtualization commands rather than being discarded, which
// would make `sysmon vms | head` exit successfully having printed nothing.
func TestRunVirtCmds_ReportRenderFailure(t *testing.T) {
	commands := map[string]func(*cobra.Command, []string) error{
		"containers": runContainersCmd,
		"vms":        runVMsCmd,
	}

	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			resetGlobals(t)
			withFixtureSnapshot(t, virtFixture(), nil)

			cmd := newTestCmd(nil)
			cmd.SetOut(&failAfterWriter{ok: 0})

			if err := run(cmd, nil); err == nil {
				t.Errorf("%s = nil on a dead output stream, want an error", name)
			}
		})
	}
}

// TestRunStartupChecks_SkipsGUIOnServerBuild verifies the desktop GUI check is
// not counted as an environment problem on a build with no GUI. Missing GTK is
// the normal state on a headless host and inside a container, and warning
// about it on every start buries the checks that are actionable.
func TestRunStartupChecks_SkipsGUIOnServerBuild(t *testing.T) {
	resetGlobals(t)

	// Report every library missing: on a server build this must still not
	// register as a problem.
	origLibs := missingDesktopLibs
	missingDesktopLibs = func() []collector.DesktopLib {
		return []collector.DesktopLib{{SOName: "libgtk-3.so.0", Debian: "libgtk-3-0"}}
	}
	t.Cleanup(func() { missingDesktopLibs = origLibs })

	// Everything else healthy, so the GUI check is the only candidate.
	dir := t.TempDir()
	marker := filepath.Join(dir, "cgroup.controllers")
	if err := os.WriteFile(marker, []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	origV2, origV1 := cgroupV2Marker, cgroupV1Marker
	cgroupV2Marker, cgroupV1Marker = marker, marker
	t.Cleanup(func() { cgroupV2Marker, cgroupV1Marker = origV2, origV1 })

	origHealth := diskHealth
	diskHealth = func() ([]types.DiskInfo, error) {
		return []types.DiskInfo{{SMARTEnabled: true}}, nil
	}
	t.Cleanup(func() { diskHealth = origHealth })

	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	origSocks := doctorRuntimeSockets
	doctorRuntimeSockets = []string{sock}
	t.Cleanup(func() { doctorRuntimeSockets = origSocks })

	problems := runStartupChecks()

	if hasDesktopGUI() {
		// Desktop build: the GUI check applies and must be counted.
		if problems != 1 {
			t.Errorf("problems = %d on a desktop build with missing libraries, want 1", problems)
		}
		return
	}
	if problems != 0 {
		t.Errorf("problems = %d, want 0: a server build must not report missing GUI libraries", problems)
	}
}

// TestCheckDesktopGUI_StillReportsForDoctor verifies the check itself keeps
// reporting missing libraries, since `sysmon doctor` asks a different question
// than startup does: what can this build do here, rather than what is broken.
func TestCheckDesktopGUI_StillReportsForDoctor(t *testing.T) {
	orig := missingDesktopLibs
	missingDesktopLibs = func() []collector.DesktopLib {
		return []collector.DesktopLib{{SOName: "libgtk-3.so.0", Debian: "libgtk-3-0"}}
	}
	t.Cleanup(func() { missingDesktopLibs = orig })

	got := checkDesktopGUI()
	if got.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", got.Status)
	}
	if got.Fix == "" {
		t.Error("doctor must still print the install command")
	}
}
