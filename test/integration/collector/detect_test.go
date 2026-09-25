//go:build integration

package collector

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// detection is a stack detection in sysmon's JSON.
type detection struct {
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Evidence []struct {
		Stack string `json:"stack"`
		Kind  string `json:"kind"`
		Path  string `json:"path"`
	} `json:"evidence"`
}

// evidence lists the detection's evidence as stack:kind:path.
func (d detection) evidence() []string {
	out := make([]string, 0, len(d.Evidence))
	for _, e := range d.Evidence {
		out = append(out, e.Stack+":"+e.Kind+":"+e.Path)
	}
	return out
}

// detect runs sysmon with its own root filesystem prepared by setup and
// returns what it concludes is installed: the container runtime and the
// hypervisor stack.
func detect(t *testing.T, setup ...string) (containers, hypervisor detection) {
	t.Helper()
	requireDevcontainer(t)
	bin := sandbox{root: true, setup: setup}.wrap(t, harness.Bins.Sysmon)

	var c struct {
		ContainerRuntime detection `json:"container_runtime"`
	}
	r := harness.Run(t, bin, nil, "containers", "--json")
	if r.Code != 0 {
		t.Fatalf("sysmon containers --json exited %d:\n%s", r.Code, r.Output())
	}
	harness.MustParseJSON(t, r.Stdout, &c)

	var v struct {
		Hypervisor detection `json:"hypervisor"`
	}
	r = harness.Run(t, bin, nil, "vms", "--json")
	if r.Code != 0 {
		t.Fatalf("sysmon vms --json exited %d:\n%s", r.Code, r.Output())
	}
	harness.MustParseJSON(t, r.Stdout, &v)
	return c.ContainerRuntime, v.Hypervisor
}

// listenUnix opens a unix socket for the test to show sysmon as a daemon's.
func listenUnix(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "daemon")
	if err != nil {
		t.Fatalf("create the socket directory: %v", err)
	}
	path := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	return path
}

// TestDetect_InstalledStacksOnAHost verifies that on a host -- no container
// marker, sysmon in no container's cgroup -- installed software is found by
// its executables and daemon sockets, each named as the evidence, and that a
// dangling link or a directory with an executable's name is not evidence.
func TestDetect_InstalledStacksOnAHost(t *testing.T) {
	containerd := listenUnix(t)
	libvirt := listenUnix(t)

	containers, hypervisor := detect(t,
		": >/usr/bin/dockerd", "chmod +x /usr/bin/dockerd",
		"ln -s dockerd /usr/bin/docker",
		"ln -s /opt/absent/podman /usr/bin/podman",
		"mkdir /usr/local/bin/crio",
		": >/usr/local/bin/qemu-system-x86_64",
		"mkdir -p /run/containerd /run/libvirt",
		": >/run/containerd/containerd.sock", "mount --bind "+containerd+" /run/containerd/containerd.sock",
		": >/run/libvirt/libvirt-sock", "mount --bind "+libvirt+" /run/libvirt/libvirt-sock",
	)

	if containers.State != "present" {
		t.Fatalf("container runtime = %+v, want present", containers)
	}
	got := strings.Join(containers.evidence(), " ")
	for _, want := range []string{"docker:binary:/usr/bin/docker", "docker:binary:/usr/bin/dockerd", "containerd:socket:/run/containerd/containerd.sock"} {
		if !strings.Contains(got, want) {
			t.Errorf("container evidence %s lacks %s", got, want)
		}
	}
	for _, not := range []string{"podman", "crio"} {
		if strings.Contains(got, not) {
			t.Errorf("container evidence %s names %s, which is a dangling link or a directory", got, not)
		}
	}
	hv := strings.Join(hypervisor.evidence(), " ")
	if hypervisor.State != "present" || !strings.Contains(hv, "qemu:binary:/usr/local/bin/qemu-system-x86_64") || !strings.Contains(hv, "libvirt:socket:/run/libvirt/libvirt-sock") {
		t.Errorf("hypervisor = %+v, want present from the qemu binary and the libvirt socket", hypervisor)
	}
}

// TestDetect_NothingInstalled verifies a host with no runtime proves its
// absence -- the cgroup tree can be read, so no container would go unseen --
// while the hypervisor stack stays unknown: in a private PID namespace a guest
// could run outside sysmon's view, and it says so.
func TestDetect_NothingInstalled(t *testing.T) {
	containers, hypervisor := detect(t)

	if containers.State != "absent" || len(containers.Evidence) != 0 {
		t.Errorf("container runtime = %+v, want absent with no evidence", containers)
	}
	if hypervisor.State != "unknown" || !strings.Contains(hypervisor.Reason, "private PID namespace") {
		t.Errorf("hypervisor = %+v, want unknown because of the private PID namespace", hypervisor)
	}
}

// TestDetect_InsideAContainerSaysSo verifies that where sysmon itself runs in
// a container -- here, in a Docker scope's cgroup -- it does not trust the
// binaries it can see, which are the container's and not the host's, and
// says why it cannot tell.
func TestDetect_InsideAContainerSaysSo(t *testing.T) {
	scope := newCgroup(t, "system.slice/docker-"+containerID(t)+".scope")

	containers, _ := detect(t,
		": >/usr/bin/dockerd",
		`echo $$ >"`+scope.path+`/cgroup.procs"`,
	)

	if containers.State == "absent" {
		t.Fatalf("container runtime = %+v, want not absent: sysmon cannot see the host", containers)
	}
	if strings.Contains(strings.Join(containers.evidence(), " "), "/usr/bin/dockerd") {
		t.Errorf("container evidence %v includes the container's own dockerd", containers.evidence())
	}
}

// TestDetect_ContainerMarkersAndKubernetes verifies the other ways sysmon
// learns it is in a container -- Podman's marker file, a Kubernetes pod's
// cgroup -- and that it then answers unknown, never absent.
func TestDetect_ContainerMarkersAndKubernetes(t *testing.T) {
	pod := newCgroup(t, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod7e3f.slice")
	tests := map[string][]string{
		"podman's marker":  {": >/run/.containerenv"},
		"a kubernetes pod": {`echo $$ >"` + pod.path + `/cgroup.procs"`},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			containers, _ := detect(t, setup...)
			if containers.State != "unknown" || !strings.Contains(containers.Reason, "running in a container") {
				t.Errorf("container runtime = %+v, want unknown because sysmon is in a container", containers)
			}
		})
	}
}

// TestDetect_AnUnreadableDirectoryIsNotAbsence runs sysmon as an ordinary
// user on a host where one of the directories it looks in is closed to it,
// and checks it answers unknown and names the directory, instead of proving an
// absence it did not see.
func TestDetect_AnUnreadableDirectoryIsNotAbsence(t *testing.T) {
	requireDevcontainer(t)
	// The binary is copied into the sandbox's own root where the ordinary
	// user can run it, and the coverage directory, when coverage is being
	// collected, is opened to that user's writes.
	asNobody := filepath.Join(t.TempDir(), "as-nobody")
	script := "#!/bin/sh\nexec setpriv --reuid=65534 --regid=65534 --clear-groups /usr/local/bin/sysmon-under-test \"$@\"\n"
	if err := os.WriteFile(asNobody, []byte(script), 0o700); err != nil {
		t.Fatalf("write the wrapper: %v", err)
	}
	bin := sandbox{root: true, setup: []string{
		"cp " + harness.Bins.Sysmon + " /usr/local/bin/sysmon-under-test",
		"mkdir -p /usr/libexec", "chmod 700 /usr/libexec",
		`if [ -n "${GOCOVERDIR:-}" ]; then chmod 1777 "$GOCOVERDIR"; fi`,
	}}.wrap(t, asNobody)

	r := harness.Run(t, bin, nil, "containers", "--json")
	if r.Code != 0 {
		t.Fatalf("sysmon containers --json as nobody exited %d:\n%s", r.Code, r.Output())
	}
	var c struct {
		ContainerRuntime detection `json:"container_runtime"`
	}
	harness.MustParseJSON(t, r.Stdout, &c)
	if c.ContainerRuntime.State != "unknown" || !strings.Contains(c.ContainerRuntime.Reason, "could not check /usr/libexec: permission denied") {
		t.Errorf("container runtime = %+v, want unknown naming /usr/libexec", c.ContainerRuntime)
	}
}
