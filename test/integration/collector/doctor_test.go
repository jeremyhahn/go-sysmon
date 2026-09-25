//go:build integration

package collector

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// check is one line of `sysmon doctor --json`.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// doctor runs `sysmon doctor --json` with bin and returns its checks by name.
func doctor(t *testing.T, bin string, env ...string) map[string]check {
	t.Helper()
	r := harness.Run(t, bin, env, "doctor", "--json")
	if r.Code != 0 {
		t.Fatalf("sysmon doctor --json exited %d:\n%s", r.Code, r.Output())
	}
	var p struct {
		Checks []check `json:"checks"`
	}
	harness.MustParseJSON(t, r.Stdout, &p)
	byName := make(map[string]check, len(p.Checks))
	for _, c := range p.Checks {
		byName[c.Name] = c
	}
	return byName
}

// TestDoctor_ReportsWhatThisHostCanDo verifies the report on the
// devcontainer as it is: a server build, no GUI libraries with the Debian
// packages that would provide them, cgroup v2, no runtime socket, and no
// memory ceiling on the cgroup it runs in.
func TestDoctor_ReportsWhatThisHostCanDo(t *testing.T) {
	requireDevcontainer(t)
	bin := sandbox{setup: []string{"mount -t tmpfs tmpfs /run"}}.wrap(t, harness.Bins.Sysmon)
	checks := doctor(t, bin)

	want := map[string]check{
		"build":             {Status: "ok", Detail: "server build (CLI and web server; no GUI window)"},
		"container metrics": {Status: "ok", Detail: "cgroup v2 available"},
		"image inventory":   {Status: "unavailable", Detail: "no reachable container runtime socket"},
	}
	for name, w := range want {
		if c := checks[name]; c.Status != w.Status || c.Detail != w.Detail {
			t.Errorf("%s = %s %q, want %s %q", name, c.Status, c.Detail, w.Status, w.Detail)
		}
	}
	gui := checks["desktop GUI"]
	if gui.Status != "unavailable" || !strings.Contains(gui.Detail, "libwebkit2gtk-4.1.so.0") || !strings.HasPrefix(gui.Fix, "sudo apt install ") {
		t.Errorf("desktop GUI = %+v, want unavailable, naming the missing WebKit library, fixed with apt", gui)
	}
	limits := checks["resource limits"]
	if limits.Status != "ok" || !strings.Contains(limits.Detail, "no cgroup memory ceiling") {
		t.Errorf("resource limits = %+v, want ok with no ceiling", limits)
	}
	if _, ok := checks["SMART / disk health"]; !ok {
		t.Error("the report has no SMART check")
	}
}

// TestDoctor_TextReport verifies the human-readable report marks each
// check's state and prints a fix under a check that has one.
func TestDoctor_TextReport(t *testing.T) {
	requireDevcontainer(t)
	bin := sandbox{setup: []string{"mount -t tmpfs tmpfs /run"}}.wrap(t, harness.Bins.Sysmon)

	r := harness.Run(t, bin, nil, "doctor")

	if r.Code != 0 {
		t.Fatalf("sysmon doctor exited %d:\n%s", r.Code, r.Output())
	}
	for _, want := range []string{"sysmon doctor", "✓ build", "✗ image inventory", "sudo usermod -aG docker $USER"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("sysmon doctor does not show %q:\n%s", want, r.Stdout)
		}
	}
}

// TestDoctor_FindsTheRuntimeSocket verifies the image inventory check passes
// once a runtime answers on its socket.
func TestDoctor_FindsTheRuntimeSocket(t *testing.T) {
	e := startEngine(t)

	checks := doctor(t, sysmonWithEngine(t, harness.Bins.Sysmon, e))

	if c := checks["image inventory"]; c.Status != "ok" || c.Detail != "runtime socket reachable at /var/run/docker.sock" {
		t.Errorf("image inventory = %+v, want ok at /var/run/docker.sock", c)
	}
}

// TestDoctor_MemoryCeilings runs the doctor inside a cgroup with the
// ceilings a systemd unit sets, and checks it reports the limit Go's runtime
// will be given, an incoherent pair as the degradation it is, and an
// operator's own GOMEMLIMIT as theirs.
func TestDoctor_MemoryCeilings(t *testing.T) {
	requireDevcontainer(t)
	tests := []struct {
		name       string
		high, max  string
		env        []string
		wantStatus string
		wantDetail string
		wantFix    bool
	}{
		{"a watermark below the limit", strconv.Itoa(512 << 20), strconv.Itoa(1 << 30), nil, "ok", "memory.high=512M, GOMEMLIMIT=", false},
		{"only a hard limit", "max", strconv.Itoa(2 << 30), nil, "ok", "memory.max=2G, GOMEMLIMIT=", false},
		{"a watermark at the limit", strconv.Itoa(1 << 30), strconv.Itoa(1 << 30), nil, "degraded", "MemoryHigh (1G) is at or above MemoryMax (1G)", true},
		{"an operator's GOMEMLIMIT", strconv.Itoa(512 << 20), "max", []string{"GOMEMLIMIT=256MiB"}, "ok", "GOMEMLIMIT set by the environment", false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unit := newCgroup(t, "system.slice/sysmon-doctor-"+strconv.Itoa(i)+".service")
			unit.set(t, "memory.max", tt.max)
			unit.set(t, "memory.high", tt.high)
			bin := sandbox{setup: []string{`echo $$ >"` + unit.path + `/cgroup.procs"`}}.wrap(t, harness.Bins.Sysmon)

			c := doctor(t, bin, tt.env...)["resource limits"]

			if c.Status != tt.wantStatus || !strings.Contains(c.Detail, tt.wantDetail) || (c.Fix != "") != tt.wantFix {
				t.Errorf("resource limits = %+v, want %s containing %q (fix %v)", c, tt.wantStatus, tt.wantDetail, tt.wantFix)
			}
		})
	}
}

// TestDoctor_DesktopLibrariesByDistribution verifies the missing GUI
// libraries are named with the install command of the distribution the host
// runs, and that a host with them all installed is told this build has no GUI
// rather than that something is missing.
func TestDoctor_DesktopLibrariesByDistribution(t *testing.T) {
	requireDevcontainer(t)
	libs := []string{"libwebkit2gtk-4.1.so.0", "libgtk-3.so.0", "libjavascriptcoregtk-4.1.so.0", "libsoup-3.0.so.0", "libglib-2.0.so.0"}

	tests := []struct {
		name    string
		setup   []string
		status  string
		fixHead string
		fixHas  string
	}{
		{"fedora", []string{`printf 'NAME="Fedora Linux"\nID=fedora\n' >/etc/os-release`}, "unavailable", "sudo dnf install ", "webkit2gtk4.1"},
		{"arch", []string{`printf 'NAME="Arch Linux"\nID=arch\n' >/etc/os-release`}, "unavailable", "sudo pacman -S ", "webkit2gtk-4.1"},
		{"every library installed", func() []string {
			// Where ld.so.conf.d says, as a package installing a library
			// outside the default directories would.
			s := []string{"mkdir -p /opt/gui/lib", "echo /opt/gui/lib >/etc/ld.so.conf.d/gui.conf"}
			for _, l := range libs {
				s = append(s, ": >/opt/gui/lib/"+l)
			}
			return s
		}(), "ok", "use the desktop binary", "make build-desktop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := sandbox{root: true, setup: tt.setup}.wrap(t, harness.Bins.Sysmon)

			c := doctor(t, bin)["desktop GUI"]

			if c.Status != tt.status || !strings.HasPrefix(c.Fix, tt.fixHead) || !strings.Contains(c.Fix, tt.fixHas) {
				t.Errorf("desktop GUI = %+v, want %s with a fix starting %q naming %q", c, tt.status, tt.fixHead, tt.fixHas)
			}
		})
	}
}

// TestDoctor_CgroupHierarchies verifies the container-metrics check, and the
// cgroup version the containers report names, on hosts whose cgroup
// filesystem is the legacy v1 hierarchy, or absent.
func TestDoctor_CgroupHierarchies(t *testing.T) {
	requireDevcontainer(t)
	tests := []struct {
		name    string
		setup   []string
		status  string
		version string
	}{
		{"cgroup v1", []string{"mount -t tmpfs cgroup /sys/fs/cgroup", "mkdir /sys/fs/cgroup/memory"}, "degraded", "v1"},
		{"no cgroup filesystem", []string{"mount -t tmpfs cgroup /sys/fs/cgroup"}, "unavailable", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := sandbox{setup: tt.setup}.wrap(t, harness.Bins.Sysmon)

			if c := doctor(t, bin)["container metrics"]; c.Status != tt.status {
				t.Errorf("container metrics = %+v, want %s", c, tt.status)
			}
			r := harness.Run(t, bin, nil, "containers", "--json")
			var p containersPayload
			harness.MustParseJSON(t, r.Stdout, &p)
			if r.Code != 0 || p.CgroupVersion != tt.version || len(p.Containers) != 0 {
				t.Errorf("containers = version %q with %d (exit %d), want version %q with none", p.CgroupVersion, len(p.Containers), r.Code, tt.version)
			}
		})
	}
}

// TestDoctor_MemoryModulesSaysWhetherTheInventoryCanBeRead runs the doctor
// over an SMBIOS table that is there and one that is not -- a firmware that
// publishes none, as a virtual machine's often does. Neither has a remedy: the
// one case with a remedy, a table the service account may not read, is the
// grant only an administrator makes, and the unit tests cover its advice.
func TestDoctor_MemoryModulesSaysWhetherTheInventoryCanBeRead(t *testing.T) {
	requireDevcontainer(t)
	tests := map[string]struct {
		setup  []string
		status string
	}{
		"readable": {[]string{"mount -t tmpfs tmpfs /sys/firmware/dmi/tables", ": >/sys/firmware/dmi/tables/DMI"}, "ok"},
		"absent":   {[]string{"mount -t tmpfs tmpfs /sys/firmware/dmi/tables"}, "unavailable"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := doctor(t, sandbox{setup: tt.setup}.wrap(t, harness.Bins.Sysmon))["memory modules"]
			if c.Status != tt.status || c.Fix != "" {
				t.Errorf("memory modules = %+v, want %s with no fix", c, tt.status)
			}
		})
	}
}
