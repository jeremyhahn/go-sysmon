//go:build integration

package collector

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// vmInfo is one entry of `sysmon vms --json`.
type vmInfo struct {
	Name               string   `json:"name"`
	UUID               string   `json:"uuid"`
	Hypervisor         string   `json:"hypervisor"`
	Accelerator        string   `json:"accelerator"`
	PID                int      `json:"pid"`
	VCPUs              int      `json:"vcpus"`
	MemoryBytes        uint64   `json:"memory_bytes"`
	RSSBytes           uint64   `json:"rss_bytes"`
	MACAddresses       []string `json:"mac_addresses"`
	TapInterfaces      []string `json:"tap_interfaces"`
	DiskImages         []string `json:"disk_images"`
	DiskImageBytes     uint64   `json:"disk_image_bytes"`
	VCPUThreads        int      `json:"vcpu_threads"`
	ThreadCount        int      `json:"thread_count"`
	CgroupPath         string   `json:"cgroup_path"`
	MemoryCurrentBytes uint64   `json:"memory_current_bytes"`
}

// vmsPayload is `sysmon vms --json`.
type vmsPayload struct {
	VMs        []vmInfo `json:"vms"`
	Hypervisor struct {
		State    string `json:"state"`
		Evidence []struct {
			Stack string `json:"stack"`
			Kind  string `json:"kind"`
		} `json:"evidence"`
	} `json:"hypervisor"`
}

// vmByPID finds the guest with pid, failing the test if there is none.
func vmByPID(t *testing.T, vms []vmInfo, pid int) vmInfo {
	t.Helper()
	for _, vm := range vms {
		if vm.PID == pid {
			return vm
		}
	}
	t.Fatalf("no virtual machine with PID %d among %+v", pid, vms)
	return vmInfo{}
}

// awaitVMs polls sysmon vms --json, run as bin, until the virtual machine with
// the given PID is listed, and returns that listing.
func awaitVMs(t *testing.T, bin string, pid int) vmsPayload {
	t.Helper()
	return awaitListing(t, bin, "the virtual machine to be listed", func(p vmsPayload) bool {
		return slices.ContainsFunc(p.VMs, func(vm vmInfo) bool { return vm.PID == pid })
	}, "vms", "--json")
}

// sizedFile creates a file of size bytes and returns its path.
func sizedFile(t *testing.T, dir, name string, size int64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("size %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return path
}

// TestVMs_ALibvirtGuestIsDescribed runs a process named qemu-system-x86_64
// with the command line libvirt gives a guest, in the guest's machine scope,
// and checks sysmon describes the guest the way the host sees it: its
// configuration from the command line, its vCPU threads, its disks' sizes, the
// host tap device carrying its NIC, and its cgroup's accounting.
func TestVMs_ALibvirtGuestIsDescribed(t *testing.T) {
	requireDevcontainer(t)
	images := t.TempDir()
	qcow2 := sizedFile(t, images, "web01.qcow2", 3<<20)
	raw := sizedFile(t, images, "scratch.raw", 1<<20)

	scope := newCgroup(t, `machine.slice/machine-qemu\x2d1\x2dweb01.scope/libvirt/emulator`)
	qemu := startGuest(t, &scope, guestBinary(t, "qemu-system-x86_64"), 4,
		"-name", "guest=web01,debug-threads=on",
		"-uuid", "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
		"-machine", "pc-q35-8.2,usb=off,dump-guest-core=off",
		"-accel", "kvm",
		"-smp", "4,sockets=4,cores=1,threads=1",
		"-m", "size=2097152k",
		"-blockdev", `{"driver":"file","filename":"`+qcow2+`","node-name":"libvirt-2-storage"}`,
		"-blockdev", `{"node-name":"libvirt-2-format","driver":"qcow2","file":"libvirt-2-storage"}`,
		"-blockdev", "driver=file,filename="+raw+",node-name=f1",
		"-netdev", "tap,fd=30,id=hostnet0",
		"-device", `{"driver":"virtio-net-pci","netdev":"hostnet0","id":"net0","mac":"52:54:00:ab:cd:ef"}`,
		"-chardev", "socket,id=charmonitor,fd=28,server=on,wait=off",
	)

	// The host side of the guest's NIC: a tap whose MAC differs from the
	// guest's only in the first octet, as libvirt creates it.
	bin := sandbox{net: true, setup: []string{
		"ip tuntap add vnet0 mode tap",
		"ip link set vnet0 address fe:54:00:ab:cd:ef",
		"ip link set vnet0 up",
	}}.wrap(t, harness.Bins.Sysmon)

	p := awaitVMs(t, bin, qemu.pid())
	vm := vmByPID(t, p.VMs, qemu.pid())

	if vm.Name != "web01" || vm.UUID != "1b4e28ba-2fa1-11d2-883f-0016d3cca427" || vm.Hypervisor != "qemu/kvm" || vm.Accelerator != "kvm" {
		t.Errorf("guest = %q %q on %q/%q, want web01 1b4e28ba-... on qemu/kvm/kvm", vm.Name, vm.UUID, vm.Hypervisor, vm.Accelerator)
	}
	if vm.VCPUs != 4 || vm.MemoryBytes != 2<<30 {
		t.Errorf("configuration = %d vCPUs, %d bytes; want 4 and %d", vm.VCPUs, vm.MemoryBytes, 2<<30)
	}
	if vm.VCPUThreads != 4 || vm.ThreadCount < 5 {
		t.Errorf("threads = %d vCPU of %d, want 4 vCPU threads among more", vm.VCPUThreads, vm.ThreadCount)
	}
	if strings.Join(vm.DiskImages, ",") != qcow2+","+raw || vm.DiskImageBytes != 4<<20 {
		t.Errorf("disks = %q (%d bytes), want %s and %s (%d bytes)", vm.DiskImages, vm.DiskImageBytes, qcow2, raw, 4<<20)
	}
	if strings.Join(vm.MACAddresses, ",") != "52:54:00:ab:cd:ef" || strings.Join(vm.TapInterfaces, ",") != "vnet0" {
		t.Errorf("network = MACs %q on taps %q, want 52:54:00:ab:cd:ef on vnet0", vm.MACAddresses, vm.TapInterfaces)
	}
	wantScope := filepath.Join(cgroupRoot, `machine.slice/machine-qemu\x2d1\x2dweb01.scope`)
	if vm.CgroupPath != wantScope || vm.MemoryCurrentBytes == 0 || vm.RSSBytes == 0 {
		t.Errorf("accounting = cgroup %q, %d bytes charged, %d resident; want %s with memory charged", vm.CgroupPath, vm.MemoryCurrentBytes, vm.RSSBytes, wantScope)
	}
	if p.Hypervisor.State != "present" {
		t.Errorf("hypervisor stack = %+v, want present with the guest running", p.Hypervisor)
	}

	// The same guest in the text report, and alone by its index.
	text := harness.Run(t, bin, nil, "vms")
	if text.Code != 0 || !strings.Contains(text.Stdout, "web01") || !strings.Contains(text.Stdout, "vnet0") {
		t.Errorf("sysmon vms exited %d without the guest and its tap:\n%s", text.Code, text.Output())
	}
}

// TestVMs_OtherCommandLineForms verifies the other ways qemu is told its
// size and accelerator -- -smp cpus=N, -m with a unit, accel= inside
// -machine -- and that a whole host block device given to the guest counts
// its capacity, as sysfs reports it, toward the guest's disks.
func TestVMs_OtherCommandLineForms(t *testing.T) {
	requireDevcontainer(t)
	sectors, err := os.ReadFile("/sys/dev/block/7:0/size")
	if err != nil {
		t.Fatalf("read the size of loop0: %v", err)
	}
	loopBytes, err := strconv.ParseUint(strings.TrimSpace(string(sectors)), 10, 64)
	if err != nil {
		t.Fatalf("parse the size of loop0: %v", err)
	}
	image := sizedFile(t, t.TempDir(), "build.img", 2<<20)
	qemu := startGuest(t, nil, guestBinary(t, "qemu-kvm"), 0,
		"-machine", "q35,accel=tcg",
		"-smp", "cpus=2",
		"-m", "4G",
		"-blockdev", `{"driver":"host_device","filename":"/dev/loop0","node-name":"hd0"}`,
		"-drive", "file="+image+",format=raw",
		"-blockdev", "driver=file,filename="+image+",node-name=f0",
	)

	p := awaitVMs(t, harness.Bins.Sysmon, qemu.pid())
	vm := vmByPID(t, p.VMs, qemu.pid())

	if vm.Name != "qemu-kvm" || vm.VCPUs != 2 || vm.MemoryBytes != 4<<30 || vm.Accelerator != "tcg" {
		t.Errorf("guest = %q with %d vCPUs, %d bytes, accel %q; want qemu-kvm, 2, %d, tcg", vm.Name, vm.VCPUs, vm.MemoryBytes, vm.Accelerator, 4<<30)
	}
	if strings.Join(vm.DiskImages, ",") != "/dev/loop0,"+image || vm.DiskImageBytes != loopBytes*512+2<<20 {
		t.Errorf("disks = %q (%d bytes), want /dev/loop0 and %s (%d bytes)", vm.DiskImages, vm.DiskImageBytes, image, loopBytes*512+2<<20)
	}
}

// TestVMs_FirecrackerAndAMissingIndex verifies a Firecracker microVM is named
// by its --id, and that asking for a guest index that does not exist fails.
func TestVMs_FirecrackerAndAMissingIndex(t *testing.T) {
	requireDevcontainer(t)
	fc := startGuest(t, nil, guestBinary(t, "firecracker"), 0,
		"--id", "fc-build-7", "--api-sock", "/run/firecracker.socket")

	p := awaitVMs(t, harness.Bins.Sysmon, fc.pid())
	vm := vmByPID(t, p.VMs, fc.pid())
	if vm.Name != "fc-build-7" || vm.Hypervisor != "firecracker" {
		t.Errorf("microVM = %q on %q, want fc-build-7 on firecracker", vm.Name, vm.Hypervisor)
	}

	missing := harness.Run(t, harness.Bins.Sysmon, nil, "vms", "--index", strconv.Itoa(len(p.VMs)+5))
	if missing.Code == 0 || !strings.Contains(missing.Output(), "not found") {
		t.Errorf("sysmon vms --index past the end exited %d:\n%s\nwant a not-found failure", missing.Code, missing.Output())
	}
}

// containerID returns a fresh 64-digit container ID.
func containerID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("make a container ID: %v", err)
	}
	return hex.EncodeToString(b)
}

// containerInfo is one entry of `sysmon containers --json`.
type containerInfo struct {
	ID               string  `json:"id"`
	ShortID          string  `json:"short_id"`
	Name             string  `json:"name"`
	Runtime          string  `json:"runtime"`
	Command          string  `json:"command"`
	CgroupPath       string  `json:"cgroup_path"`
	ProcessCount     int     `json:"process_count"`
	MemoryBytes      uint64  `json:"memory_bytes"`
	MemoryLimitBytes uint64  `json:"memory_limit_bytes"`
	CPULimitCores    float64 `json:"cpu_limit_cores"`
	PIDsMax          uint64  `json:"pids_max"`
	PIDs             uint64  `json:"pids"`
}

// containersPayload is `sysmon containers --json`.
type containersPayload struct {
	CgroupVersion string          `json:"cgroup_version"`
	Containers    []containerInfo `json:"containers"`
}

// containerByID finds the container with id.
func containerByID(t *testing.T, cs []containerInfo, id string) containerInfo {
	t.Helper()
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no container %s among %+v", id, cs)
	return containerInfo{}
}

// awaitContainers polls sysmon containers --json until every container in ids
// is listed, and returns that listing.
func awaitContainers(t *testing.T, bin string, ids ...string) containersPayload {
	t.Helper()
	return awaitListing(t, bin, "every container to be listed", func(p containersPayload) bool {
		for _, id := range ids {
			if !slices.ContainsFunc(p.Containers, func(c containerInfo) bool { return c.ID == id }) {
				return false
			}
		}
		return true
	}, "containers", "--json")
}

// TestContainers_EveryRuntimesCgroupLayout creates the cgroups each runtime
// creates for a container -- Docker's systemd and cgroupfs drivers, Podman,
// containerd under Kubernetes, and a container that delegates its cgroup to
// systemd inside it -- with a real process in each, and checks sysmon finds
// every one, attributes it to its runtime, and reads its limits and usage.
func TestContainers_EveryRuntimesCgroupLayout(t *testing.T) {
	requireDevcontainer(t)
	nginx := guestBinary(t, "nginx")

	tests := []struct {
		name    string
		layout  func(id string) (scope, leaf string)
		runtime string
	}{
		{"docker systemd driver", func(id string) (string, string) {
			s := "system.slice/docker-" + id + ".scope"
			return s, s
		}, "docker"},
		{"docker cgroupfs driver", func(id string) (string, string) {
			s := "docker/" + id
			return s, s
		}, "docker"},
		{"podman", func(id string) (string, string) {
			s := "machine.slice/libpod-" + id + ".scope"
			return s, s
		}, "podman"},
		{"containerd under kubernetes", func(id string) (string, string) {
			s := "kubepods.slice/kubepods-burstable.slice/cri-containerd-" + id + ".scope"
			return s, s
		}, "containerd"},
		{"a container delegating its cgroup", func(id string) (string, string) {
			s := "system.slice/docker-" + id + ".scope"
			return s, s + "/init.scope"
		}, "docker"},
	}

	type made struct {
		id, scope, runtime string
		pid                int
	}
	var all []made
	for _, tt := range tests {
		id := containerID(t)
		scope, leaf := tt.layout(id)
		inner := newCgroup(t, leaf)
		limits := cgroup{path: filepath.Join(cgroupRoot, scope)}
		limits.set(t, "memory.max", strconv.Itoa(256<<20))
		limits.set(t, "cpu.max", "50000 100000")
		limits.set(t, "pids.max", "100")
		g := startGuest(t, &inner, nginx, 0, "-g", "daemon off;")
		all = append(all, made{id: id, scope: filepath.Join(cgroupRoot, scope), runtime: tt.runtime, pid: g.pid()})
	}

	// Decoys: a unit whose name shares a runtime's prefix but carries no
	// container ID, and a cgroupfs-style child of docker that is not one.
	decoy := newCgroup(t, "system.slice/docker-compose@web.service")
	notAnID := newCgroup(t, "docker/buildkit")
	startGuest(t, &decoy, nginx, 0)
	startGuest(t, &notAnID, nginx, 0)

	ids := make([]string, len(all))
	for i, m := range all {
		ids[i] = m.id
	}
	p := awaitContainers(t, harness.Bins.Sysmon, ids...)
	for _, c := range p.Containers {
		if strings.Contains(c.CgroupPath, "compose@") || strings.HasSuffix(c.CgroupPath, "/buildkit") {
			t.Errorf("%s was taken for a container", c.CgroupPath)
		}
	}
	if p.CgroupVersion != "v2" {
		t.Errorf("cgroup version = %q, want v2", p.CgroupVersion)
	}
	for i, m := range all {
		t.Run(tests[i].name, func(t *testing.T) {
			c := containerByID(t, p.Containers, m.id)
			if c.Runtime != m.runtime || c.ShortID != m.id[:12] || c.CgroupPath != m.scope {
				t.Errorf("container = %s %s at %s, want %s %s at %s", c.Runtime, c.ShortID, c.CgroupPath, m.runtime, m.id[:12], m.scope)
			}
			if c.Name != "nginx" || !strings.Contains(c.Command, "daemon off;") || c.ProcessCount != 1 {
				t.Errorf("identity = %q running %q with %d processes, want nginx running its command, 1 process", c.Name, c.Command, c.ProcessCount)
			}
			if c.MemoryLimitBytes != 256<<20 || c.CPULimitCores != 0.5 || c.PIDsMax != 100 {
				t.Errorf("limits = %d bytes, %v cores, %d pids; want %d, 0.5, 100", c.MemoryLimitBytes, c.CPULimitCores, c.PIDsMax, 256<<20)
			}
			if c.MemoryBytes == 0 || c.PIDs == 0 {
				t.Errorf("usage = %d bytes, %d pids; want the process's charge", c.MemoryBytes, c.PIDs)
			}
		})
	}

	// One container alone, by its index, in the text report.
	first := containerByID(t, p.Containers, all[0].id)
	var index int
	for i, c := range p.Containers {
		if c.ID == first.ID {
			index = i
		}
	}
	text := harness.Run(t, harness.Bins.Sysmon, nil, "containers", "--index", strconv.Itoa(index))
	if text.Code != 0 || !strings.Contains(text.Stdout, first.ShortID) {
		t.Errorf("sysmon containers --index %d exited %d without %s:\n%s", index, text.Code, first.ShortID, text.Output())
	}
}

// TestServe_ListsARunningContainer verifies the server, which rescans the
// cgroup tree on its collections, lists a container that started after it
// did, with the name of the process it runs.
func TestServe_ListsARunningContainer(t *testing.T) {
	requireDevcontainer(t)
	id := containerID(t)
	cg := newCgroup(t, "system.slice/docker-"+id+".scope")

	s := harness.Serve(t, harness.ServeOptions{})
	startGuest(t, &cg, guestBinary(t, "redis-server"), 0, "*:6379")

	awaitSnapshot(t, s, "the container to be listed", func(raw []byte) bool {
		var snap struct {
			Virt containersPayload `json:"virtualization"`
		}
		harness.MustParseJSON(t, string(raw), &snap)
		for _, c := range snap.Virt.Containers {
			if c.ID == id {
				return c.Name == "redis-server"
			}
		}
		return false
	})
}
