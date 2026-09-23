package collector

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	psprocess "github.com/shirou/gopsutil/v4/process"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withHostRoot points install detection at a synthetic filesystem for one test.
func withHostRoot(t *testing.T, root string) {
	t.Helper()
	original := hostRoot
	hostRoot = root
	t.Cleanup(func() { hostRoot = original })
}

// fakeHost builds a synthetic root holding the given host paths. A path ending
// in "/" is created as a directory; anything else as a file with no content.
//
// It starts as an ordinary host's view of itself: a named service cgroup, a
// /proc without hidepid, and the initial PID and user namespaces. A test about
// any of those overwrites the file concerned. Without them every detection
// would rightly be unknown -- procfs always has these files -- and the tests
// would depend on nothing they meant to test.
func fakeHost(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	writeSysfsFile(t, filepath.Join(root, "proc", "self", "cgroup"), "0::/system.slice/go-sysmon.service\n")
	writeSysfsFile(t, filepath.Join(root, "proc", "self", "mountinfo"),
		"23 22 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n")
	writeSysfsFile(t, filepath.Join(root, "proc", "self", "status"), "Name:\tsysmon\nCapEff:\t0000000000000000\n")
	for kind, target := range map[string]string{"pid": "pid:[4026531836]", "user": "user:[4026531837]"} {
		link := filepath.Join(root, "proc", "self", "ns", kind)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	for _, p := range paths {
		full := filepath.Join(root, p)
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			continue
		}
		writeSysfsFile(t, full, "")
	}
	return root
}

// symlinkLoop makes host path p a symlink to itself. Resolving it fails with
// ELOOP whoever runs the test, root included, which is what makes it a
// dependable stand-in for a path that exists but cannot be checked.
func symlinkLoop(t *testing.T, root, p string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Replace anything fakeHost put there.
	if err := os.RemoveAll(full); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(filepath.Base(full), full); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// detectContainers runs a fresh scan of root and decides the Containers tab
// for a node whose cgroup tree is readable and shows nothing running.
func detectContainers(t *testing.T, root string) types.StackDetection {
	t.Helper()
	withHostRoot(t, root)
	scan := scanInstalled()
	return detectStack(scan.sections[sectionContainers], scan.containerized, nil, true, reasonContainersUnobservable)
}

// detectHypervisor is detectContainers for the Virtualization tab.
func detectHypervisor(t *testing.T, root string) types.StackDetection {
	t.Helper()
	withHostRoot(t, root)
	scan := scanInstalled()
	return detectStack(scan.sections[sectionHypervisor], scan.containerized, nil, true, reasonVMsUnobservable)
}

func TestDetectContainers_Installed(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  []types.StackEvidence
	}{
		{
			name:  "binary only",
			paths: []string{"/usr/bin/docker"},
			want:  []types.StackEvidence{{Stack: "docker", Kind: types.EvidenceBinary, Path: "/usr/bin/docker"}},
		},
		{
			name:  "socket only",
			paths: []string{"/run/docker.sock"},
			want:  []types.StackEvidence{{Stack: "docker", Kind: types.EvidenceSocket, Path: "/run/docker.sock"}},
		},
		{
			name:  "binary and socket",
			paths: []string{"/usr/bin/dockerd", "/run/docker.sock"},
			want: []types.StackEvidence{
				{Stack: "docker", Kind: types.EvidenceBinary, Path: "/usr/bin/dockerd"},
				{Stack: "docker", Kind: types.EvidenceSocket, Path: "/run/docker.sock"},
			},
		},
		{
			// Daemonless podman has no socket: the binary is the only trace.
			name:  "podman without a socket",
			paths: []string{"/usr/bin/podman"},
			want:  []types.StackEvidence{{Stack: "podman", Kind: types.EvidenceBinary, Path: "/usr/bin/podman"}},
		},
		{
			name:  "containerd and nerdctl",
			paths: []string{"/usr/local/bin/nerdctl", "/run/containerd/containerd.sock"},
			want: []types.StackEvidence{
				{Stack: "containerd", Kind: types.EvidenceSocket, Path: "/run/containerd/containerd.sock"},
				{Stack: "nerdctl", Kind: types.EvidenceBinary, Path: "/usr/local/bin/nerdctl"},
			},
		},
		{
			name:  "cri-o",
			paths: []string{"/run/crio/crio.sock"},
			want:  []types.StackEvidence{{Stack: "crio", Kind: types.EvidenceSocket, Path: "/run/crio/crio.sock"}},
		},
		{
			name:  "docker snap",
			paths: []string{"/snap/bin/docker"},
			want:  []types.StackEvidence{{Stack: "docker", Kind: types.EvidenceBinary, Path: "/snap/bin/docker"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectContainers(t, fakeHost(t, tt.paths...))

			if got.State != types.PresencePresent {
				t.Fatalf("state = %q, want present", got.State)
			}
			if !slices.Equal(got.Evidence, tt.want) {
				t.Errorf("evidence = %+v, want %+v", got.Evidence, tt.want)
			}
			if got.Reason != "" {
				t.Errorf("reason = %q, want none for a settled answer", got.Reason)
			}
		})
	}
}

func TestDetectContainers_NothingInstalledIsAbsent(t *testing.T) {
	// Directories that exist and hold unrelated software are the realistic
	// "none": an empty root would pass even if listing a directory broke.
	got := detectContainers(t, fakeHost(t, "/usr/bin/ls", "/usr/sbin/sshd", "/run/dbus/system_bus_socket"))

	if got.State != types.PresenceAbsent {
		t.Fatalf("state = %q (%s), want absent", got.State, got.Reason)
	}
	if len(got.Evidence) != 0 {
		t.Errorf("evidence = %+v, want none", got.Evidence)
	}
}

func TestDetectContainers_UnreadableIsUnknown(t *testing.T) {
	tests := []struct {
		name string
		loop string
	}{
		{"binary directory", "/usr/bin"},
		{"socket", "/run/docker.sock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fakeHost(t, "/usr/sbin/")
			symlinkLoop(t, root, tt.loop)

			got := detectContainers(t, root)

			if got.State != types.PresenceUnknown {
				t.Fatalf("state = %q, want unknown: a probe that failed proves nothing", got.State)
			}
			if !strings.Contains(got.Reason, tt.loop) {
				t.Errorf("reason = %q, want it to name %s", got.Reason, tt.loop)
			}
		})
	}
}

func TestDetectContainers_UnreadableDoesNotHideEvidence(t *testing.T) {
	root := fakeHost(t, "/usr/bin/podman")
	symlinkLoop(t, root, "/run/docker.sock")

	got := detectContainers(t, root)

	if got.State != types.PresencePresent {
		t.Fatalf("state = %q, want present: podman was found whatever docker's socket says", got.State)
	}
}

func TestDetectHypervisor_EachStack(t *testing.T) {
	tests := []struct {
		name string
		path string
		want types.StackEvidence
	}{
		{"qemu system emulator", "/usr/bin/qemu-system-x86_64",
			types.StackEvidence{Stack: "qemu", Kind: types.EvidenceBinary, Path: "/usr/bin/qemu-system-x86_64"}},
		{"qemu-kvm on RHEL", "/usr/libexec/qemu-kvm",
			types.StackEvidence{Stack: "qemu", Kind: types.EvidenceBinary, Path: "/usr/libexec/qemu-kvm"}},
		{"libvirtd", "/usr/sbin/libvirtd",
			types.StackEvidence{Stack: "libvirt", Kind: types.EvidenceBinary, Path: "/usr/sbin/libvirtd"}},
		{"modular libvirt", "/usr/sbin/virtqemud",
			types.StackEvidence{Stack: "libvirt", Kind: types.EvidenceBinary, Path: "/usr/sbin/virtqemud"}},
		{"libvirt socket", "/run/libvirt/libvirt-sock",
			types.StackEvidence{Stack: "libvirt", Kind: types.EvidenceSocket, Path: "/run/libvirt/libvirt-sock"}},
		{"virtqemud socket", "/run/libvirt/virtqemud-sock",
			types.StackEvidence{Stack: "libvirt", Kind: types.EvidenceSocket, Path: "/run/libvirt/virtqemud-sock"}},
		{"virtualbox module", "/sys/module/vboxdrv/",
			types.StackEvidence{Stack: "virtualbox", Kind: types.EvidenceModule, Path: "/sys/module/vboxdrv"}},
		{"VBoxManage", "/usr/bin/VBoxManage",
			types.StackEvidence{Stack: "virtualbox", Kind: types.EvidenceBinary, Path: "/usr/bin/VBoxManage"}},
		{"xen toolstack", "/usr/sbin/xl",
			types.StackEvidence{Stack: "xen", Kind: types.EvidenceBinary, Path: "/usr/sbin/xl"}},
		{"cloud-hypervisor", "/usr/local/bin/cloud-hypervisor",
			types.StackEvidence{Stack: "cloud-hypervisor", Kind: types.EvidenceBinary, Path: "/usr/local/bin/cloud-hypervisor"}},
		{"firecracker", "/usr/local/bin/firecracker",
			types.StackEvidence{Stack: "firecracker", Kind: types.EvidenceBinary, Path: "/usr/local/bin/firecracker"}},
		{"vmware module", "/sys/module/vmmon/",
			types.StackEvidence{Stack: "vmware", Kind: types.EvidenceModule, Path: "/sys/module/vmmon"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectHypervisor(t, fakeHost(t, tt.path))

			if got.State != types.PresencePresent {
				t.Fatalf("state = %q, want present", got.State)
			}
			if !slices.Equal(got.Evidence, []types.StackEvidence{tt.want}) {
				t.Errorf("evidence = %+v, want [%+v]", got.Evidence, tt.want)
			}
		})
	}
}

func TestDetectHypervisor_XenControlDomain(t *testing.T) {
	root := fakeHost(t)
	writeSysfsFile(t, filepath.Join(root, "proc", "xen", "capabilities"), "control_d\n")

	got := detectHypervisor(t, root)

	want := []types.StackEvidence{{Stack: "xen", Kind: types.EvidenceControlDomain, Path: "/proc/xen/capabilities"}}
	if got.State != types.PresencePresent || !slices.Equal(got.Evidence, want) {
		t.Errorf("got %q %+v, want present %+v", got.State, got.Evidence, want)
	}
}

func TestDetectHypervisor_XenGuestIsAbsent(t *testing.T) {
	// A domU has /proc/xen as well, with no control_d: it runs under a
	// hypervisor, it does not host one.
	root := fakeHost(t)
	writeSysfsFile(t, filepath.Join(root, "proc", "xen", "capabilities"), "\n")

	if got := detectHypervisor(t, root); got.State != types.PresenceAbsent {
		t.Errorf("state = %q, want absent for a Xen guest", got.State)
	}
}

func TestDetectHypervisor_KVMDeviceAloneIsAbsent(t *testing.T) {
	// Owner ruling on #187: /dev/kvm is on nearly every x86 machine, so it
	// says nothing about whether anyone runs VMs here. The kvm modules are
	// loaded for the same reason and must not count either.
	root := fakeHost(t, "/dev/kvm", "/sys/module/kvm/", "/sys/module/kvm_intel/")

	if got := detectHypervisor(t, root); got.State != types.PresenceAbsent {
		t.Errorf("state = %q %+v, want absent", got.State, got.Evidence)
	}
}

func TestDetectHypervisor_UnreadableIsUnknown(t *testing.T) {
	root := fakeHost(t)
	symlinkLoop(t, root, "/proc/xen/capabilities")

	got := detectHypervisor(t, root)

	if got.State != types.PresenceUnknown || !strings.Contains(got.Reason, "/proc/xen/capabilities") {
		t.Errorf("got %q (%s), want unknown naming the capabilities file", got.State, got.Reason)
	}
}

func TestDetect_SectionsAreIndependent(t *testing.T) {
	root := fakeHost(t, "/usr/bin/docker")

	if got := detectHypervisor(t, root); got.State != types.PresenceAbsent {
		t.Errorf("hypervisor = %q, want absent: docker is not a VM stack", got.State)
	}
}

func TestDetect_MergedUsrReportsEachBinaryOnce(t *testing.T) {
	root := fakeHost(t, "/usr/bin/docker", "/usr/sbin/")
	for _, link := range []string{"bin", "sbin"} {
		if err := os.Symlink(filepath.Join("usr", link), filepath.Join(root, link)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	got := detectContainers(t, root)

	want := []types.StackEvidence{{Stack: "docker", Kind: types.EvidenceBinary, Path: "/usr/bin/docker"}}
	if !slices.Equal(got.Evidence, want) {
		t.Errorf("evidence = %+v, want %+v", got.Evidence, want)
	}
}

func TestDetect_DirectoriesAreNotBinaries(t *testing.T) {
	// /usr/libexec/docker is a directory of docker's helpers on some
	// distributions; only an executable named docker is the runtime.
	if got := detectContainers(t, fakeHost(t, "/usr/libexec/docker/")); got.State != types.PresenceAbsent {
		t.Errorf("state = %q %+v, want absent", got.State, got.Evidence)
	}
}

func TestDetect_InsideAContainerNothingFoundIsUnknown(t *testing.T) {
	for _, marker := range containerMarkers {
		t.Run(marker, func(t *testing.T) {
			root := fakeHost(t, marker)

			for name, got := range map[string]types.StackDetection{
				"containers": detectContainers(t, root),
				"hypervisor": detectHypervisor(t, root),
			} {
				if got.State != types.PresenceUnknown {
					t.Errorf("%s = %q, want unknown: a container cannot see the host's binaries", name, got.State)
				}
				if !strings.Contains(got.Reason, marker) {
					t.Errorf("%s reason = %q, want it to name %s", name, got.Reason, marker)
				}
			}
		})
	}
}

func TestDetect_InsideAContainerEvidenceStillCounts(t *testing.T) {
	// The documented deployment mounts the docker socket into the container,
	// and that socket is the host's.
	root := fakeHost(t, "/.dockerenv", "/run/docker.sock")

	if got := detectContainers(t, root); got.State != types.PresencePresent {
		t.Errorf("state = %q, want present", got.State)
	}
}

func TestDetect_UncheckableMarkerIsUnknown(t *testing.T) {
	// A marker that could not be checked leaves "is this the host's
	// filesystem?" open. Lstat does not follow the marker itself, so the loop
	// sits in its parent; nothing else is probed under /var.
	root := fakeHost(t)
	symlinkLoop(t, root, "/var/run/secrets")

	for name, got := range map[string]types.StackDetection{
		"containers": detectContainers(t, root),
		"hypervisor": detectHypervisor(t, root),
	} {
		if got.State != types.PresenceUnknown || !strings.Contains(got.Reason, "/var/run/secrets") {
			t.Errorf("%s = %q (%s), want unknown naming the marker", name, got.State, got.Reason)
		}
	}
}

func TestDetectStack_RunningWorkloads(t *testing.T) {
	running := runningEvidence([]string{"podman", "docker", "podman"})

	got := detectStack(sectionScan{}, "", running, true, reasonContainersUnobservable)

	want := []types.StackEvidence{
		{Stack: "docker", Kind: types.EvidenceRunning},
		{Stack: "podman", Kind: types.EvidenceRunning},
	}
	if got.State != types.PresencePresent || !slices.Equal(got.Evidence, want) {
		t.Errorf("got %q %+v, want present %+v", got.State, got.Evidence, want)
	}
}

func TestDetectStack_RunningWorkloadsWinInsideAContainer(t *testing.T) {
	got := detectStack(sectionScan{}, "/.dockerenv", runningEvidence([]string{"qemu/kvm"}), true, reasonVMsUnobservable)

	if got.State != types.PresencePresent {
		t.Errorf("state = %q, want present", got.State)
	}
}

func TestDetectStack_UnobservableWorkloadsAreUnknown(t *testing.T) {
	got := detectStack(sectionScan{}, "", nil, false, reasonVMsUnobservable)

	if got.State != types.PresenceUnknown || got.Reason != reasonVMsUnobservable {
		t.Errorf("got %q (%s), want unknown: %s", got.State, got.Reason, reasonVMsUnobservable)
	}
}

func TestDetectStack_DoesNotSortTheCachedScan(t *testing.T) {
	cached := []types.StackEvidence{
		{Stack: "podman", Kind: types.EvidenceBinary, Path: "/usr/bin/podman"},
		{Stack: "docker", Kind: types.EvidenceBinary, Path: "/usr/bin/docker"},
	}
	before := slices.Clone(cached)

	detectStack(sectionScan{evidence: cached}, "", nil, true, reasonContainersUnobservable)

	if !slices.Equal(cached, before) {
		t.Errorf("the cached scan was reordered to %+v; it is shared with the published snapshot", cached)
	}
}

func TestInstallDetector_RechecksAfterTheInterval(t *testing.T) {
	root := fakeHost(t, "/usr/bin/")
	withHostRoot(t, root)
	var d installDetector
	start := time.Unix(1_700_000_000, 0)

	if got := d.current(start).sections[sectionContainers].evidence; len(got) != 0 {
		t.Fatalf("first scan found %+v, want nothing", got)
	}

	// Installed after the first scan.
	writeSysfsFile(t, filepath.Join(root, "usr", "bin", "docker"), "")

	if got := d.current(start.Add(installRecheckInterval - time.Second)).sections[sectionContainers].evidence; len(got) != 0 {
		t.Errorf("a scan inside the interval found %+v: it should have been reused", got)
	}
	if got := d.current(start.Add(installRecheckInterval)).sections[sectionContainers].evidence; len(got) != 1 {
		t.Errorf("the scan after the interval found %+v, want the new docker binary", got)
	}
}

func TestInstallDetector_RechecksWhenTheClockGoesBack(t *testing.T) {
	root := fakeHost(t, "/usr/bin/")
	withHostRoot(t, root)
	var d installDetector
	start := time.Unix(1_700_000_000, 0)
	d.current(start)

	writeSysfsFile(t, filepath.Join(root, "usr", "bin", "podman"), "")

	// A clock stepped backwards would otherwise pin the old scan until the
	// wall clock caught up again.
	if got := d.current(start.Add(-time.Hour)).sections[sectionContainers].evidence; len(got) != 1 {
		t.Errorf("found %+v, want a rescan", got)
	}
}

func TestVirtCollector_CollectReportsStackDetection(t *testing.T) {
	cg := t.TempDir()
	writeSysfsFile(t, filepath.Join(cg, "cgroup.controllers"), "cpu memory io pids")
	withCgroupRoot(t, cg)
	withHostRoot(t, fakeHost(t, "/usr/bin/podman"))

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	info := c.Info()

	if info.ContainerRuntime.State != types.PresencePresent {
		t.Errorf("container runtime = %q, want present from /usr/bin/podman", info.ContainerRuntime.State)
	}
	// The process list is the real one, so VMs this machine happens to run may
	// appear; installed-software evidence can only come from the fake root,
	// which has none.
	for _, e := range info.Hypervisor.Evidence {
		if e.Kind != types.EvidenceRunning {
			t.Errorf("hypervisor evidence %+v did not come from the fake root", e)
		}
	}
	if len(info.Hypervisor.Evidence) == 0 && info.Hypervisor.State != types.PresenceAbsent {
		t.Errorf("hypervisor = %q (%s), want absent with nothing installed or running",
			info.Hypervisor.State, info.Hypervisor.Reason)
	}
}

func TestVirtCollector_CollectWithoutCgroupsIsUnknown(t *testing.T) {
	withCgroupRoot(t, t.TempDir())
	withHostRoot(t, fakeHost(t, "/usr/bin/"))

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := c.Info().ContainerRuntime
	if got.State != types.PresenceUnknown || got.Reason != reasonContainersUnobservable {
		t.Errorf("got %q (%s), want unknown: %s", got.State, got.Reason, reasonContainersUnobservable)
	}
}

func TestDetect_BinaryDirectoryThatIsAFileProvesNothingThere(t *testing.T) {
	// A path component that is a file cannot hold executables, which is as
	// settled an answer as the directory not existing.
	if got := detectContainers(t, fakeHost(t, "/usr/bin")); got.State != types.PresenceAbsent {
		t.Errorf("state = %q (%s), want absent", got.State, got.Reason)
	}
}

func TestDetect_InsideAContainerItsOwnBinariesAreNotEvidence(t *testing.T) {
	// The monitoring image may well ship a docker CLI or qemu; that says what
	// the image contains, not what the host has installed.
	root := fakeHost(t, "/.dockerenv", "/usr/bin/docker", "/usr/bin/qemu-system-x86_64")

	for name, got := range map[string]types.StackDetection{
		"containers": detectContainers(t, root),
		"hypervisor": detectHypervisor(t, root),
	} {
		if got.State != types.PresenceUnknown {
			t.Errorf("%s = %q %+v, want unknown", name, got.State, got.Evidence)
		}
	}
}

func TestDetect_ContainerWithoutAMarkerIsFoundFromItsCgroup(t *testing.T) {
	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"
	tests := []struct {
		name   string
		cgroup string
	}{
		// A private cgroup namespace: the pod's own cgroup is its root.
		{"private cgroup namespace", "0::/\n"},
		// The host's namespace, from inside a containerd pod.
		{"containerd pod", "0::/kubepods.slice/kubepods-besteffort.slice/cri-containerd-" + id + ".scope\n"},
		{"docker container", "0::/system.slice/docker-" + id + ".scope\n"},
		// Docker's cgroupfs driver: the ID is its own directory.
		{"docker cgroupfs driver", "0::/docker/" + id + "\n"},
		// Pure cgroup v1 with private namespaces: no unified line, every
		// hierarchy at its root.
		{"private cgroup v1 namespaces", "12:memory:/\n4:cpuset:/\n1:name=systemd:/\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fakeHost(t, "/usr/sbin/")
			writeSysfsFile(t, filepath.Join(root, "proc", "self", "cgroup"), tt.cgroup)

			got := detectHypervisor(t, root)

			if got.State != types.PresenceUnknown || !strings.Contains(got.Reason, "/proc/self/cgroup") {
				t.Errorf("got %q (%s), want unknown naming the cgroup file", got.State, got.Reason)
			}
		})
	}
}

func TestDetect_HostCgroupIsNotAContainer(t *testing.T) {
	tests := []struct {
		name   string
		cgroup string
	}{
		{"systemd service", "0::/system.slice/go-sysmon.service\n"},
		// cgroup v1 puts every process at the root of cpuset; only the unified
		// hierarchy's root means a namespace.
		{"pure cgroup v1 systemd host", "4:cpuset:/\n1:name=systemd:/system.slice/go-sysmon.service\n"},
		{"cgroup v1 hybrid", "4:cpuset:/\n1:name=systemd:/user.slice/user-1000.slice/session-2.scope\n0::/user.slice/user-1000.slice/session-2.scope\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fakeHost(t, "/usr/sbin/")
			writeSysfsFile(t, filepath.Join(root, "proc", "self", "cgroup"), tt.cgroup)

			if got := detectHypervisor(t, root); got.State != types.PresenceAbsent {
				t.Errorf("got %q (%s), want absent", got.State, got.Reason)
			}
		})
	}
}

func TestDetect_BinarySymlinks(t *testing.T) {
	tests := []struct {
		name   string
		target string
		setup  []string
		want   types.Presence
	}{
		{"to a binary", "../lib/docker/docker", []string{"/usr/lib/docker/docker"}, types.PresencePresent},
		// A snap alias left behind by a removed snap.
		{"dangling", "../lib/docker/docker", nil, types.PresenceAbsent},
		{"to a directory", "../lib/docker", []string{"/usr/lib/docker/"}, types.PresenceAbsent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fakeHost(t, append([]string{"/usr/bin/"}, tt.setup...)...)
			if err := os.Symlink(tt.target, filepath.Join(root, "usr", "bin", "docker")); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			if got := detectContainers(t, root); got.State != tt.want {
				t.Errorf("state = %q %+v, want %q", got.State, got.Evidence, tt.want)
			}
		})
	}
}

func TestDetect_BinarySymlinkThatCannotBeResolvedIsUnknown(t *testing.T) {
	root := fakeHost(t)
	symlinkLoop(t, root, "/usr/bin/podman")

	got := detectContainers(t, root)

	if got.State != types.PresenceUnknown || !strings.Contains(got.Reason, "/usr/bin/podman") {
		t.Errorf("got %q (%s), want unknown naming the link", got.State, got.Reason)
	}
}

// procMount builds a fake root whose /proc is mounted with opts and whose
// effective capability mask is capEff.
func procMount(t *testing.T, opts, capEff string) string {
	t.Helper()
	root := fakeHost(t)
	writeSysfsFile(t, filepath.Join(root, "proc", "self", "mountinfo"),
		"22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"+
			"23 22 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc "+opts+"\n")
	writeSysfsFile(t, filepath.Join(root, "proc", "self", "status"),
		"Name:\tsysmon\nCapInh:\t0000000000000000\nCapEff:\t"+capEff+"\n")
	namespaceLink(t, root, "pid", "pid:[4026531836]")
	namespaceLink(t, root, "user", "user:[4026531837]")
	return root
}

func TestProcVisibility(t *testing.T) {
	const noCaps, ptraceOnly = "0000000000000000", "0000000000080000"
	tests := []struct {
		name       string
		opts       string
		capEff     string
		restricted bool
	}{
		{"no hidepid", "rw", noCaps, false},
		{"hidepid off", "rw,hidepid=0", noCaps, false},
		{"hidepid=2", "rw,hidepid=2", noCaps, true},
		{"hidepid=invisible", "rw,hidepid=invisible", noCaps, true},
		// hidepid=1 leaves the directories but refuses their contents, so a
		// guest's name and command line are unreadable all the same.
		{"hidepid=1", "rw,hidepid=1", noCaps, true},
		{"hidepid with CAP_SYS_PTRACE", "rw,hidepid=2", ptraceOnly, false},
		{"hidepid with sysmon in the exempt group", "rw,hidepid=2,gid=" + strconv.Itoa(os.Getegid()), noCaps, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withHostRoot(t, procMount(t, tt.opts, tt.capEff))

			restricted := procVisibility()

			if got := restricted != ""; got != tt.restricted {
				t.Errorf("restricted = %v (%q), want %v", got, restricted, tt.restricted)
			}
		})
	}
}

func TestProcVisibility_UnreadableStatusIsUnknown(t *testing.T) {
	root := procMount(t, "rw,hidepid=2", "0")
	if err := os.Remove(filepath.Join(root, "proc", "self", "status")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	symlinkLoop(t, root, "/proc/self/status")
	withHostRoot(t, root)

	restricted := procVisibility()

	// Not an install-probe failure, which installed evidence would override:
	// the process list itself cannot vouch for being complete.
	if !strings.Contains(restricted, "/proc/self/status") {
		t.Errorf("procRestricted = %q, want it to name /proc/self/status", restricted)
	}
	if observable, _ := vmObservability(true, nil, restricted); observable {
		t.Error("VMs observable although hidepid could not be ruled out")
	}
}

func TestVirtCollector_HiddenProcessesMakeVMsUnobservable(t *testing.T) {
	withCgroupRoot(t, t.TempDir())
	withHostRoot(t, procMount(t, "rw,hidepid=2", "0000000000000000"))

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	info := c.Info()

	if info.Capability.VMsObservable {
		t.Error("VMsObservable = true, but hidepid hides other users' guests")
	}
	// Any guest this process can still see proves a stack present; without
	// one, absence cannot be claimed.
	if len(info.Hypervisor.Evidence) == 0 && info.Hypervisor.State != types.PresenceUnknown {
		t.Errorf("hypervisor = %q, want unknown", info.Hypervisor.State)
	}
}

func TestApplyStackDetection_UndescribedGuestsStillCount(t *testing.T) {
	// A qemu process whose command line could not be read produces no VM row,
	// but it was seen, and seeing it is the proof.
	info := types.VirtInfo{Capability: types.VirtCapability{ContainersObservable: true, VMsObservable: true}}

	applyStackDetection(&info, installScan{}, []string{"qemu/kvm"}, nil, "")

	// Reported as "qemu", the name its install evidence uses, so a node with
	// both shows one qemu row rather than two.
	want := []types.StackEvidence{{Stack: "qemu", Kind: types.EvidenceRunning}}
	if info.Hypervisor.State != types.PresencePresent || !slices.Equal(info.Hypervisor.Evidence, want) {
		t.Errorf("hypervisor = %q %+v, want present %+v", info.Hypervisor.State, info.Hypervisor.Evidence, want)
	}
	if info.ContainerRuntime.State != types.PresenceAbsent {
		t.Errorf("container runtime = %q, want absent", info.ContainerRuntime.State)
	}
}

func TestFindContainerCgroups_ReportsAWalkItCouldNotFinish(t *testing.T) {
	root := t.TempDir()
	symlinkLoop(t, root, "/cgroup")

	_, err := findContainerCgroups(filepath.Join(root, "cgroup"))

	if err == nil {
		t.Error("err = nil; a tree that could not be listed cannot vouch for having no containers")
	}
}

func TestFindContainerCgroups_MissingTreeIsNotAFailure(t *testing.T) {
	if _, err := findContainerCgroups(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("err = %v; no cgroup filesystem is reported by the cgroup version, not as a walk failure", err)
	}
}

func TestApplyStackDetection_ExplainsWhyWorkloadsWereUnobservable(t *testing.T) {
	walkErr := &os.PathError{Op: "open", Path: "/sys/fs/cgroup/machine.slice", Err: os.ErrPermission}
	nameErr := &os.PathError{Op: "open", Path: "/proc/4242/status", Err: os.ErrPermission}
	vmCauses := []struct {
		name       string
		scanErr    error
		restricted string
		wantReason string
	}{
		{"hidepid", nil, "/proc is mounted hidepid=2, which hides other users' processes from sysmon", "hidepid=2"},
		// Not "the process list cannot be read": it could, one entry could not.
		{"one process unidentified", nameErr, "", "some processes could not be identified"},
	}
	for _, tt := range vmCauses {
		t.Run(tt.name, func(t *testing.T) {
			info := types.VirtInfo{}
			observable, note := vmObservability(true, tt.scanErr, tt.restricted)
			info.Capability.VMsObservable = observable

			applyStackDetection(&info, installScan{}, nil, walkErr, note)

			if info.Hypervisor.State != types.PresenceUnknown || !strings.Contains(info.Hypervisor.Reason, tt.wantReason) {
				t.Errorf("hypervisor = %q (%s), want unknown naming %q", info.Hypervisor.State, info.Hypervisor.Reason, tt.wantReason)
			}
		})
	}

	info := types.VirtInfo{}
	applyStackDetection(&info, installScan{}, nil, walkErr, "")
	if info.ContainerRuntime.State != types.PresenceUnknown || !strings.Contains(info.ContainerRuntime.Reason, "permission denied") {
		t.Errorf("container runtime = %q (%s), want unknown naming the walk failure",
			info.ContainerRuntime.State, info.ContainerRuntime.Reason)
	}
}

func TestVirtCollector_ContainerThatCannotBeInspectedIsUnknown(t *testing.T) {
	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"
	tests := []struct {
		name           string
		procs          func(t *testing.T, scope string)
		wantObservable bool
		wantState      types.Presence
	}{
		{
			// The scope was found, but what runs in it could not be read.
			name:           "unreadable cgroup.procs",
			procs:          func(t *testing.T, scope string) { symlinkLoop(t, scope, "/cgroup.procs") },
			wantObservable: false,
			wantState:      types.PresenceUnknown,
		},
		{
			// No cgroup.procs is a cgroup that exited mid-scan: nothing runs.
			name:           "exited scope",
			procs:          func(t *testing.T, scope string) {},
			wantObservable: true,
			wantState:      types.PresenceAbsent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cg := t.TempDir()
			writeSysfsFile(t, filepath.Join(cg, "cgroup.controllers"), "cpu memory io pids")
			scope := filepath.Join(cg, "system.slice", "docker-"+id+".scope")
			if err := os.MkdirAll(scope, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tt.procs(t, scope)
			withCgroupRoot(t, cg)
			withHostRoot(t, fakeHost(t, "/usr/bin/"))

			c := NewVirtCollector(slog.Default())
			if err := c.Collect(); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			info := c.Info()

			if info.Capability.ContainersObservable != tt.wantObservable {
				t.Errorf("containers observable = %v, want %v", info.Capability.ContainersObservable, tt.wantObservable)
			}
			if info.ContainerRuntime.State != tt.wantState {
				t.Errorf("container runtime = %q (%s), want %q",
					info.ContainerRuntime.State, info.ContainerRuntime.Reason, tt.wantState)
			}
		})
	}
}

func TestVirtCollector_FindsContainersTheBoundedWalkMissed(t *testing.T) {
	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"
	tests := []struct {
		name      string
		scope     string
		procsFile string
	}{
		{
			// A container running systemd delegates its scope: its processes
			// live in a child cgroup and the scope's own list is empty.
			name:      "delegated scope",
			scope:     "system.slice/docker-" + id + ".scope",
			procsFile: "init.scope/cgroup.procs",
		},
		{
			// Rootless podman under a user manager, seven levels down.
			name:      "deeply nested rootless container",
			scope:     "user.slice/user-1000.slice/user@1000.service/user.slice/podman.slice/nested.slice/libpod-" + id + ".scope",
			procsFile: "cgroup.procs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cg := t.TempDir()
			writeSysfsFile(t, filepath.Join(cg, "cgroup.controllers"), "cpu memory io pids")
			scope := filepath.Join(cg, tt.scope)
			writeSysfsFile(t, filepath.Join(scope, "cgroup.procs"), "")
			writeSysfsFile(t, filepath.Join(scope, tt.procsFile), "4242\n")
			withCgroupRoot(t, cg)
			withHostRoot(t, fakeHost(t, "/usr/bin/"))

			c := NewVirtCollector(slog.Default())
			if err := c.Collect(); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			info := c.Info()

			if len(info.Containers) != 1 || info.Containers[0].ProcessCount != 1 {
				t.Fatalf("containers = %+v, want the one container with its one process", info.Containers)
			}
			if info.ContainerRuntime.State != types.PresencePresent {
				t.Errorf("container runtime = %q (%s), want present from the running container",
					info.ContainerRuntime.State, info.ContainerRuntime.Reason)
			}
		})
	}
}

func TestReadSubtreePIDs(t *testing.T) {
	t.Run("parent first, then children", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, filepath.Join(dir, "cgroup.procs"), "10\n")
		writeSysfsFile(t, filepath.Join(dir, "a", "cgroup.procs"), "20\n21\n")

		pids, err := readSubtreePIDs(dir)

		if err != nil || !slices.Equal(pids, []int32{10, 20, 21}) {
			t.Errorf("got %v, %v; want [10 20 21], nil", pids, err)
		}
	})

	t.Run("an exited cgroup is not an error", func(t *testing.T) {
		pids, err := readSubtreePIDs(filepath.Join(t.TempDir(), "gone"))

		if err != nil || len(pids) != 0 {
			t.Errorf("got %v, %v; want nothing and no error", pids, err)
		}
	})

	t.Run("an unreadable child is reported", func(t *testing.T) {
		dir := t.TempDir()
		writeSysfsFile(t, filepath.Join(dir, "cgroup.procs"), "10\n")
		symlinkLoop(t, dir, "/child/cgroup.procs")

		pids, err := readSubtreePIDs(dir)

		if err == nil {
			t.Error("err = nil, want the child's read failure")
		}
		if !slices.Equal(pids, []int32{10}) {
			t.Errorf("pids = %v, want what could be read: [10]", pids)
		}
	})
}

func TestVMObservability(t *testing.T) {
	denied := &os.PathError{Op: "open", Path: "/proc/4242/status", Err: os.ErrPermission}
	tests := []struct {
		name       string
		readable   bool
		scanErr    error
		restricted string
		want       bool
		noteHas    string
	}{
		{"everything seen", true, nil, "", true, ""},
		{"list unreadable", false, nil, "", false, "process list unreadable"},
		// The list was taken, but a process in it could not be named: it may
		// have been a hypervisor.
		{"a process could not be identified", true, denied, "", false, "permission denied"},
		{"hidepid", true, nil, "/proc is mounted hidepid=2", false, "hidepid=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, note := vmObservability(tt.readable, tt.scanErr, tt.restricted)
			if got != tt.want || !strings.Contains(note, tt.noteHas) || (tt.want && note != "") {
				t.Errorf("got %v %q, want %v with a note containing %q", got, note, tt.want, tt.noteHas)
			}
		})
	}
}

func TestProcessVanished(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "open", Path: "/proc/4242/status", Err: syscall.ENOENT},
		syscall.ESRCH,
		psprocess.ErrorProcessNotRunning,
	} {
		if !processVanished(err) {
			t.Errorf("processVanished(%v) = false, want true", err)
		}
	}
	if processVanished(&os.PathError{Op: "open", Path: "/proc/4242/status", Err: syscall.EACCES}) {
		t.Error("processVanished(EACCES) = true; a process that could not be read is not gone")
	}
}

func TestProcVisibility_PtraceInAChildUserNamespaceDoesNotCount(t *testing.T) {
	tests := []struct {
		name       string
		userNS     string
		restricted bool
	}{
		{"initial namespace", "user:[4026531837]", false},
		// unshare -Ur: root and CAP_SYS_PTRACE inside, an ordinary user
		// outside, where the hidden processes live. Judged by the namespace
		// itself, so a child namespace given the identity UID map is no
		// different.
		{"child namespace", "user:[4026532750]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := procMount(t, "rw,hidepid=2", "0000000000080000")
			namespaceLink(t, root, "user", tt.userNS)
			withHostRoot(t, root)

			restricted := procVisibility()

			if got := restricted != ""; got != tt.restricted {
				t.Errorf("restricted = %v (%q), want %v", got, restricted, tt.restricted)
			}
		})
	}
}

func TestVirtCollector_RefreshDetectionNoticesAnInstallBetweenCollects(t *testing.T) {
	cg := t.TempDir()
	writeSysfsFile(t, filepath.Join(cg, "cgroup.controllers"), "cpu memory io pids")
	withCgroupRoot(t, cg)
	root := fakeHost(t, "/usr/bin/")
	withHostRoot(t, root)

	c := NewVirtCollector(slog.Default())
	if err := c.RefreshDetection(); err != nil {
		t.Fatalf("RefreshDetection before any Collect: %v", err)
	}
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := c.Info().ContainerRuntime.State; got != types.PresenceAbsent {
		t.Fatalf("container runtime = %q, want absent before the install", got)
	}
	notesBefore := len(c.Info().Capability.Notes)

	// Installed after Collect, which will not run again for five ticks. The
	// scan is due, as it would be 30 s later.
	writeSysfsFile(t, filepath.Join(root, "usr", "bin", "podman"), "")
	c.install.scannedAt = time.Time{}

	if err := c.RefreshDetection(); err != nil {
		t.Fatalf("RefreshDetection: %v", err)
	}
	info := c.Info()
	if info.ContainerRuntime.State != types.PresencePresent {
		t.Errorf("container runtime = %q, want present from the new podman binary", info.ContainerRuntime.State)
	}
	// Refreshing re-derives the notes from the workload scan rather than
	// adding to the published ones.
	if len(info.Capability.Notes) != notesBefore {
		t.Errorf("notes grew from %d to %d on refresh: %v", notesBefore, len(info.Capability.Notes), info.Capability.Notes)
	}
}

func TestProcVisibility_UnreadableUserNamespaceIsUnverified(t *testing.T) {
	root := procMount(t, "rw,hidepid=2", "0000000000080000")
	if err := os.Remove(filepath.Join(root, "proc", "self", "ns", "user")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	withHostRoot(t, root)

	restricted := procVisibility()

	if !strings.Contains(restricted, "/proc/self/ns/user") {
		t.Errorf("procRestricted = %q, want it to name /proc/self/ns/user", restricted)
	}
}

func TestDetect_UnsettledHostViewIgnoresBinaries(t *testing.T) {
	// Whether this is a container could not be checked, so a docker binary
	// might be the image's. It must not turn unknown into present.
	for _, loop := range []string{"/var/run/secrets", "/proc/self/cgroup"} {
		t.Run(loop, func(t *testing.T) {
			root := fakeHost(t, "/usr/bin/docker")
			symlinkLoop(t, root, loop)

			if got := detectContainers(t, root); got.State != types.PresenceUnknown {
				t.Errorf("state = %q %+v, want unknown", got.State, got.Evidence)
			}
		})
	}
}

func TestVirtCollector_ContainerScanFailureDoesNotOutliveItsScan(t *testing.T) {
	// A failure belongs to the scan that met it. Once the tree reads cleanly
	// again, the next scan must be free to prove absence (PR #188 review).
	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"
	cg := t.TempDir()
	writeSysfsFile(t, filepath.Join(cg, "cgroup.controllers"), "cpu memory io pids")
	scope := filepath.Join(cg, "system.slice", "docker-"+id+".scope")
	symlinkLoop(t, scope, "/cgroup.procs")
	withCgroupRoot(t, cg)
	withHostRoot(t, fakeHost(t, "/usr/bin/"))

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := c.Info().ContainerRuntime.State; got != types.PresenceUnknown {
		t.Fatalf("first scan = %q, want unknown while cgroup.procs is unreadable", got)
	}

	// The container exits and its scope goes with it.
	if err := os.RemoveAll(scope); err != nil {
		t.Fatalf("remove scope: %v", err)
	}
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	info := c.Info()

	if !info.Capability.ContainersObservable {
		t.Errorf("containers observable = false after a clean scan; notes: %v", info.Capability.Notes)
	}
	if info.ContainerRuntime.State != types.PresenceAbsent {
		t.Errorf("second scan = %q (%s), want absent", info.ContainerRuntime.State, info.ContainerRuntime.Reason)
	}
}

// namespaceLink makes /proc/self/ns/<kind> under root read as target.
func namespaceLink(t *testing.T, root, kind, target string) {
	t.Helper()
	link := filepath.Join(root, "proc", "self", "ns", kind)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func TestProcVisibility_PIDNamespace(t *testing.T) {
	tests := []struct {
		name       string
		link       string
		restricted string
	}{
		{"initial namespace", "pid:[4026531836]", ""},
		// Any other namespace, however its own /proc numbers its processes.
		{"private namespace", "pid:[4026532601]", "private PID namespace"},
		{"unparseable link", "net:[4026531840]", "pid:[inode]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No hidepid: the namespace alone must decide.
			root := procMount(t, "rw", "0000000000000000")
			namespaceLink(t, root, "pid", tt.link)
			withHostRoot(t, root)

			restricted := procVisibility()

			if tt.restricted == "" && restricted != "" {
				t.Errorf("procRestricted = %q, want none", restricted)
			}
			if tt.restricted != "" && !strings.Contains(restricted, tt.restricted) {
				t.Errorf("procRestricted = %q, want it to mention %q", restricted, tt.restricted)
			}
		})
	}
}

func TestProcVisibility_UnreadablePIDNamespaceIsUnverified(t *testing.T) {
	root := procMount(t, "rw", "0000000000000000")
	if err := os.Remove(filepath.Join(root, "proc", "self", "ns", "pid")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	withHostRoot(t, root)

	restricted := procVisibility()

	if !strings.Contains(restricted, "/proc/self/ns/pid") {
		t.Errorf("procRestricted = %q, want it to name /proc/self/ns/pid", restricted)
	}
}

func TestVirtCollector_RefreshKeepsTheProcessViewOfItsScan(t *testing.T) {
	// hidepid is lifted after the VM scan ran under it. That scan still
	// missed other users' guests, so refreshing detection before the next
	// scan must not certify it.
	withCgroupRoot(t, t.TempDir())
	root := procMount(t, "rw,hidepid=2", "0000000000000000")
	withHostRoot(t, root)

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	writeSysfsFile(t, filepath.Join(root, "proc", "self", "mountinfo"),
		"23 22 0:22 / /proc rw,relatime shared:12 - proc proc rw\n")
	c.install.scannedAt = time.Time{}
	if err := c.RefreshDetection(); err != nil {
		t.Fatalf("RefreshDetection: %v", err)
	}

	if c.Info().Capability.VMsObservable {
		t.Error("VMsObservable = true for a VM scan taken under hidepid")
	}
}

func TestDetect_MissingSelfCgroupLeavesTheHostViewUnsettled(t *testing.T) {
	// procfs always has it; missing means a masked /proc, possibly in a
	// container whose image ships docker.
	root := fakeHost(t, "/usr/bin/docker")
	if err := os.Remove(filepath.Join(root, "proc", "self", "cgroup")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got := detectContainers(t, root); got.State != types.PresenceUnknown {
		t.Errorf("state = %q %+v, want unknown", got.State, got.Evidence)
	}
}

func TestProcVisibility_MissingMountInfoIsUnverified(t *testing.T) {
	root := fakeHost(t)
	if err := os.Remove(filepath.Join(root, "proc", "self", "mountinfo")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	withHostRoot(t, root)

	if got := procVisibility(); !strings.Contains(got, "/proc/self/mountinfo") {
		t.Errorf("procVisibility() = %q, want it to name /proc/self/mountinfo", got)
	}
}

func TestDetect_NixOSSystemProfile(t *testing.T) {
	// NixOS exposes installed packages only through the system profile, as
	// symlinks into the store.
	root := fakeHost(t, "/nix/store/abc-podman-5.2/bin/podman", "/run/current-system/sw/bin/")
	if err := os.Symlink(filepath.Join(root, "nix/store/abc-podman-5.2/bin/podman"),
		filepath.Join(root, "run/current-system/sw/bin/podman")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := detectContainers(t, root)

	want := []types.StackEvidence{{Stack: "podman", Kind: types.EvidenceBinary, Path: "/run/current-system/sw/bin/podman"}}
	if got.State != types.PresencePresent || !slices.Equal(got.Evidence, want) {
		t.Errorf("got %q %+v, want present %+v", got.State, got.Evidence, want)
	}
}

func TestProcVisibility_MountTableWithoutProcIsUnverified(t *testing.T) {
	for name, content := range map[string]string{
		"empty":    "",
		"filtered": "22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := fakeHost(t)
			writeSysfsFile(t, filepath.Join(root, "proc", "self", "mountinfo"), content)
			withHostRoot(t, root)

			if got := procVisibility(); !strings.Contains(got, "procfs mount on /proc") {
				t.Errorf("procVisibility() = %q, want it unverified for want of a /proc mount", got)
			}
		})
	}
}

func TestDetect_SelfCgroupWithoutMembershipIsUnsettled(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"sanitised": "garbage\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := fakeHost(t, "/usr/bin/docker")
			writeSysfsFile(t, filepath.Join(root, "proc", "self", "cgroup"), content)

			got := detectContainers(t, root)

			if got.State != types.PresenceUnknown || !strings.Contains(got.Reason, "cgroup membership") {
				t.Errorf("got %q (%s) %+v, want unknown for want of a membership", got.State, got.Reason, got.Evidence)
			}
		})
	}
}

func TestFindContainerCgroups_CgroupfsDriver(t *testing.T) {
	// Docker without systemd managing cgroups puts each container at
	// /docker/<id>. The walk used to look for a "docker/" name prefix, which
	// no single directory name can have, and descended into the container.
	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"
	root := t.TempDir()
	writeSysfsFile(t, filepath.Join(root, "docker", id, "cgroup.procs"), "4242\n")
	// A bare hex directory anywhere else is not a container.
	writeSysfsFile(t, filepath.Join(root, "system.slice", id, "cgroup.procs"), "4343\n")

	scopes, err := findContainerCgroups(root)

	if err != nil {
		t.Fatalf("findContainerCgroups: %v", err)
	}
	if len(scopes) != 1 || scopes[0].id != id || scopes[0].runtime != runtimeDocker {
		t.Errorf("scopes = %+v, want the one docker container under /docker", scopes)
	}
}
