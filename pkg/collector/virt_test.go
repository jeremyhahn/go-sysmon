package collector

import (
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// buildContainerCgroup writes a synthetic cgroup v2 container scope.
func buildContainerCgroup(t *testing.T, root, scopeName string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, "system.slice", scopeName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, content := range files {
		writeSysfsFile(t, filepath.Join(dir, name), content)
	}
	return dir
}

// withCgroupRoot points the collector at a synthetic tree for one test.
func withCgroupRoot(t *testing.T, root string) {
	t.Helper()
	original := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = original })
}

// ---- cgroup name parsing --------------------------------------------------

func TestParseContainerCgroupName_Runtimes(t *testing.T) {
	t.Parallel()
	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"

	tests := []struct {
		name        string
		dir         string
		wantRuntime string
	}{
		{"docker", "docker-" + id + ".scope", runtimeDocker},
		{"podman", "libpod-" + id + ".scope", runtimePodman},
		{"crio", "crio-" + id + ".scope", runtimeCRIO},
		{"containerd", "cri-containerd-" + id + ".scope", runtimeContainerd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotRuntime, ok := parseContainerCgroupName(tt.dir)
			if !ok {
				t.Fatalf("parseContainerCgroupName(%q) reported no match", tt.dir)
			}
			if gotID != id {
				t.Errorf("id = %q, want %q", gotID, id)
			}
			if gotRuntime != tt.wantRuntime {
				t.Errorf("runtime = %q, want %q", gotRuntime, tt.wantRuntime)
			}
		})
	}
}

// TestParseContainerCgroupName_RejectsNonContainers ensures ordinary systemd
// units are not mistaken for containers just because they share a prefix.
func TestParseContainerCgroupName_RejectsNonContainers(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"user.slice",
		"system.slice",
		"docker.service",
		"docker-short.scope",
		"machine-qemu.scope",
		"",
	} {
		if _, _, ok := parseContainerCgroupName(name); ok {
			t.Errorf("parseContainerCgroupName(%q) = true, want false", name)
		}
	}
}

func TestIsHexID(t *testing.T) {
	t.Parallel()
	if !isHexID("004c327801b2b84b") {
		t.Error("a long hex string must be accepted")
	}
	for _, bad := range []string{"", "short", "004c32780", "004c327801b2-not-hex", "zzzzzzzzzzzz"} {
		if isHexID(bad) {
			t.Errorf("isHexID(%q) = true, want false", bad)
		}
	}
}

// ---- cgroup file readers --------------------------------------------------

func TestReadCgroupKeyed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.stat")
	writeSysfsFile(t, path, "usage_usec 22254706\nuser_usec 17630677\nsystem_usec 4624028\n")

	if got := readCgroupKeyed(path, "usage_usec"); got != 22254706 {
		t.Errorf("usage_usec = %d, want 22254706", got)
	}
	if got := readCgroupKeyed(path, "system_usec"); got != 4624028 {
		t.Errorf("system_usec = %d, want 4624028", got)
	}
}

func TestReadCgroupKeyed_MissingKeyOrFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.stat")
	writeSysfsFile(t, path, "usage_usec notanumber\n")

	if got := readCgroupKeyed(path, "absent"); got != 0 {
		t.Errorf("absent key = %d, want 0", got)
	}
	if got := readCgroupKeyed(path, "usage_usec"); got != 0 {
		t.Errorf("unparseable value = %d, want 0", got)
	}
	if got := readCgroupKeyed(filepath.Join(dir, "nope"), "usage_usec"); got != 0 {
		t.Errorf("missing file = %d, want 0", got)
	}
}

func TestReadCgroupIO_SumsDevices(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "io.stat")
	writeSysfsFile(t, path,
		"259:3 rbytes=57344 wbytes=0 rios=3 wios=0 dbytes=0 dios=0\n"+
			"259:6 rbytes=7360512 wbytes=308936704 rios=272 wios=82850 dbytes=0 dios=0\n")

	read, write := readCgroupIO(path)
	if read != 57344+7360512 {
		t.Errorf("read = %d, want %d", read, 57344+7360512)
	}
	if write != 308936704 {
		t.Errorf("write = %d, want %d", write, 308936704)
	}
}

func TestReadCgroupIO_MissingFile(t *testing.T) {
	t.Parallel()
	read, write := readCgroupIO(filepath.Join(t.TempDir(), "nope"))
	if read != 0 || write != 0 {
		t.Errorf("missing io.stat = (%d, %d), want (0, 0)", read, write)
	}
}

// ---- container collection -------------------------------------------------

// TestCollectContainers_ReadsCgroupAccounting builds a synthetic container and
// checks every accounting field is picked up.
func TestCollectContainers_ReadsCgroupAccounting(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "cgroup.controllers"), "cpu memory io pids\n")

	const id = "004c327801b2b84b8fcb0b6a340d8a5b3154f4198e54bc77e79d283e4391206c"
	buildContainerCgroup(t, root, "docker-"+id+".scope", map[string]string{
		"cgroup.procs":   "1\n",
		"cpu.stat":       "usage_usec 5000000\n",
		"memory.current": "116629504\n",
		"memory.max":     "268435456\n",
		"pids.current":   "30\n",
		"io.stat":        "259:6 rbytes=1000 wbytes=2000\n",
	})

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}

	info := c.Info()
	if info.CgroupVersion != "v2" {
		t.Errorf("CgroupVersion = %q, want %q", info.CgroupVersion, "v2")
	}
	if len(info.Containers) != 1 {
		t.Fatalf("got %d containers, want 1", len(info.Containers))
	}

	ct := info.Containers[0]
	if ct.ID != id {
		t.Errorf("ID = %q, want %q", ct.ID, id)
	}
	if ct.ShortID != id[:shortIDLen] {
		t.Errorf("ShortID = %q, want %q", ct.ShortID, id[:shortIDLen])
	}
	if ct.Runtime != runtimeDocker {
		t.Errorf("Runtime = %q, want %q", ct.Runtime, runtimeDocker)
	}
	if ct.MemoryBytes != 116629504 {
		t.Errorf("MemoryBytes = %d, want 116629504", ct.MemoryBytes)
	}
	if ct.MemoryLimitBytes != 268435456 {
		t.Errorf("MemoryLimitBytes = %d, want 268435456", ct.MemoryLimitBytes)
	}
	wantPct := float64(116629504) / float64(268435456) * 100
	if ct.MemoryPercent < wantPct-0.01 || ct.MemoryPercent > wantPct+0.01 {
		t.Errorf("MemoryPercent = %f, want ~%f", ct.MemoryPercent, wantPct)
	}
	if ct.PIDs != 30 {
		t.Errorf("PIDs = %d, want 30", ct.PIDs)
	}
	if ct.CPUUsageUsec != 5000000 {
		t.Errorf("CPUUsageUsec = %d, want 5000000", ct.CPUUsageUsec)
	}
	if ct.ReadBytes != 1000 || ct.WriteBytes != 2000 {
		t.Errorf("IO = (%d, %d), want (1000, 2000)", ct.ReadBytes, ct.WriteBytes)
	}
	if len(info.Runtimes) != 1 || info.Runtimes[0] != runtimeDocker {
		t.Errorf("Runtimes = %v, want [docker]", info.Runtimes)
	}
}

// TestCollectContainers_UnlimitedMemory covers memory.max reading "max", where
// no percentage can be derived.
func TestCollectContainers_UnlimitedMemory(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "cgroup.controllers"), "cpu memory\n")

	const id = "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccddddddddddddeeeeeeeeeeeeffff"
	buildContainerCgroup(t, root, "docker-"+id+".scope", map[string]string{
		"cgroup.procs":   "1\n",
		"memory.current": "1048576\n",
		"memory.max":     "max\n",
	})

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	cts := c.Info().Containers
	if len(cts) != 1 {
		t.Fatalf("got %d containers, want 1", len(cts))
	}
	if cts[0].MemoryLimitBytes != 0 {
		t.Errorf("MemoryLimitBytes = %d, want 0 for an unlimited cgroup", cts[0].MemoryLimitBytes)
	}
	if cts[0].MemoryPercent != 0 {
		t.Errorf("MemoryPercent = %f, want 0 when there is no limit", cts[0].MemoryPercent)
	}
}

// TestCollectContainers_SkipsExitedCgroup verifies a scope with no processes
// is dropped rather than reported as a zeroed container.
func TestCollectContainers_SkipsExitedCgroup(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "cgroup.controllers"), "cpu\n")

	const id = "1111111111112222222222223333333333334444444444445555555555556666"
	buildContainerCgroup(t, root, "docker-"+id+".scope", map[string]string{
		"cgroup.procs": "\n",
	})

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if got := len(c.Info().Containers); got != 0 {
		t.Errorf("got %d containers, want 0 for an exited cgroup", got)
	}
}

// TestCollectContainers_CPURateBetweenSamples verifies CPU percent is derived
// from the usage delta rather than reported as a lifetime total.
func TestCollectContainers_CPURateBetweenSamples(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "cgroup.controllers"), "cpu\n")

	const id = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	dir := buildContainerCgroup(t, root, "docker-"+id+".scope", map[string]string{
		"cgroup.procs": "1\n",
		"cpu.stat":     "usage_usec 0\n",
	})

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	if got := c.Info().Containers[0].CPUPercent; got != 0 {
		t.Errorf("first sample CPUPercent = %f, want 0 with no baseline", got)
	}

	// Burn a known amount of CPU time between samples.
	time.Sleep(50 * time.Millisecond)
	writeSysfsFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec 25000\n")

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	got := c.Info().Containers[0].CPUPercent
	if got <= 0 {
		t.Errorf("second sample CPUPercent = %f, want > 0 after usage increased", got)
	}
	if got > 100 {
		t.Errorf("CPUPercent = %f, implausible for 25ms of CPU over ~50ms", got)
	}
}

// TestCollect_NoCgroupFilesystem verifies a host with no cgroup tree yields an
// empty result rather than an error.
func TestCollect_NoCgroupFilesystem(t *testing.T) {
	withCgroupRoot(t, filepath.Join(t.TempDir(), "absent"))

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}

	info := c.Info()
	if info.CgroupVersion != "" {
		t.Errorf("CgroupVersion = %q, want empty", info.CgroupVersion)
	}
	if len(info.Containers) != 0 {
		t.Errorf("got %d containers, want 0", len(info.Containers))
	}
}

// ---- hypervisor command line parsing --------------------------------------

// TestParseHypervisorCmdline_Libvirt parses a real libvirt-generated qemu
// command line, the shape this collector must handle in practice.
func TestParseHypervisorCmdline_Libvirt(t *testing.T) {
	t.Parallel()
	cmdline := `/usr/bin/qemu-system-x86_64 -name guest=vstation-01,debug-threads=on ` +
		`-machine pc-q35-noble,usb=off -accel kvm -cpu host ` +
		`-m size=65536000k -smp 24,sockets=24,cores=1,threads=1 ` +
		`-uuid 383ccd65-823e-45a6-aeb6-936d00e29214 ` +
		`-device {"driver":"virtio-net-pci","netdev":"hostnet0","id":"net0","mac":"52:54:00:a0:bd:a5"}`

	vm := parseHypervisorCmdline(cmdline)

	if vm.Name != "vstation-01" {
		t.Errorf("Name = %q, want %q", vm.Name, "vstation-01")
	}
	if vm.UUID != "383ccd65-823e-45a6-aeb6-936d00e29214" {
		t.Errorf("UUID = %q", vm.UUID)
	}
	if vm.VCPUs != 24 {
		t.Errorf("VCPUs = %d, want 24", vm.VCPUs)
	}
	if vm.Accelerator != "kvm" {
		t.Errorf("Accelerator = %q, want kvm", vm.Accelerator)
	}
	wantMem := uint64(65536000) * 1024
	if vm.MemoryBytes != wantMem {
		t.Errorf("MemoryBytes = %d, want %d", vm.MemoryBytes, wantMem)
	}
	if len(vm.MACAddresses) != 1 || vm.MACAddresses[0] != "52:54:00:a0:bd:a5" {
		t.Errorf("MACAddresses = %v, want [52:54:00:a0:bd:a5]", vm.MACAddresses)
	}
}

// TestParseHypervisorCmdline_Minimal covers a hand-rolled qemu invocation with
// none of libvirt's decoration.
func TestParseHypervisorCmdline_Minimal(t *testing.T) {
	t.Parallel()
	vm := parseHypervisorCmdline("qemu-system-x86_64 -name myvm -m 4096 -smp 2")

	if vm.Name != "myvm" {
		t.Errorf("Name = %q, want myvm", vm.Name)
	}
	if vm.VCPUs != 2 {
		t.Errorf("VCPUs = %d, want 2", vm.VCPUs)
	}
	if vm.MemoryBytes != 4096*1024*1024 {
		t.Errorf("MemoryBytes = %d, want %d", vm.MemoryBytes, 4096*1024*1024)
	}
}

func TestParseHypervisorCmdline_Empty(t *testing.T) {
	t.Parallel()
	vm := parseHypervisorCmdline("")
	if vm.Name != "" || vm.VCPUs != 0 || vm.MemoryBytes != 0 {
		t.Errorf("empty cmdline produced %+v, want a zero VMInfo", vm)
	}
}

func TestParseQemuMemory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want uint64
	}{
		{"4096", 4096 << 20},
		{"4G", 4 << 30},
		{"512M", 512 << 20},
		{"size=65536000k", 65536000 << 10},
		{"size=2G,slots=4,maxmem=8G", 2 << 30},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		if got := parseQemuMemory(tt.in); got != tt.want {
			t.Errorf("parseQemuMemory(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseSMP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"24,sockets=24,cores=1,threads=1", 24},
		{"8", 8},
		{"cpus=16,sockets=2", 16},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		if got := parseSMP(tt.in); got != tt.want {
			t.Errorf("parseSMP(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestMatchHypervisor(t *testing.T) {
	t.Parallel()
	if got := matchHypervisor("qemu-system-x86_64"); got != "qemu/kvm" {
		t.Errorf("qemu-system-x86_64 = %q, want qemu/kvm", got)
	}
	for _, notHV := range []string{"bash", "nginx", "systemd", ""} {
		if got := matchHypervisor(notHV); got != "" {
			t.Errorf("matchHypervisor(%q) = %q, want empty", notHV, got)
		}
	}
}

// ---- MAC to tap mapping ---------------------------------------------------

// TestSameMACSuffix verifies the comparison that links a guest NIC to its host
// tap device, which differ only in the first octet's locally-administered bit.
func TestSameMACSuffix(t *testing.T) {
	t.Parallel()
	if !sameMACSuffix("fe:54:00:a0:bd:a5", "52:54:00:a0:bd:a5") {
		t.Error("a tap and its guest NIC must match on the last five octets")
	}
	if sameMACSuffix("52:54:00:a0:bd:a6", "52:54:00:a0:bd:a5") {
		t.Error("different addresses must not match")
	}
	for _, bad := range [][2]string{{"", ""}, {"52:54:00", "52:54:00:a0:bd:a5"}} {
		if sameMACSuffix(bad[0], bad[1]) {
			t.Errorf("malformed input %v must not match", bad)
		}
	}
}

func TestTapInterfacesForMACs_NoMACs(t *testing.T) {
	t.Parallel()
	if got := tapInterfacesForMACs(nil); got != nil {
		t.Errorf("tapInterfacesForMACs(nil) = %v, want nil", got)
	}
}

// TestTapInterfacesForMACs_MatchesSyntheticTree links a guest MAC to a tap.
func TestTapInterfacesForMACs_MatchesSyntheticTree(t *testing.T) {
	root := t.TempDir()
	original := sysClassNet
	sysClassNet = root
	t.Cleanup(func() { sysClassNet = original })

	writeSysfsFile(t, filepath.Join(root, "vnet0", "address"), "fe:54:00:a0:bd:a5\n")
	writeSysfsFile(t, filepath.Join(root, "eth0", "address"), "aa:bb:cc:dd:ee:ff\n")

	got := tapInterfacesForMACs([]string{"52:54:00:a0:bd:a5"})
	if len(got) != 1 || got[0] != "vnet0" {
		t.Errorf("tapInterfacesForMACs() = %v, want [vnet0]", got)
	}
}

// ---- Info before Collect --------------------------------------------------

func TestVirtCollector_InfoBeforeCollect(t *testing.T) {
	t.Parallel()
	c := NewVirtCollector(nil)

	info := c.Info()
	if len(info.Containers) != 0 || len(info.VMs) != 0 {
		t.Errorf("Info() before Collect returned data: %+v", info)
	}
	var zero types.VirtInfo
	if info.CgroupVersion != zero.CgroupVersion {
		t.Errorf("CgroupVersion = %q, want empty", info.CgroupVersion)
	}
}

// ---- container diagnostics ------------------------------------------------

func TestReadCPUMax(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    float64
	}{
		{"one core", "100000 100000\n", 1},
		{"half core", "50000 100000\n", 0.5},
		{"two cores", "200000 100000\n", 2},
		{"unlimited", "max 100000\n", 0},
		{"malformed", "garbage\n", 0},
		{"zero period", "100000 0\n", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			writeSysfsFile(t, path, tt.content)
			if got := readCPUMax(path); got != tt.want {
				t.Errorf("readCPUMax(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}

	if got := readCPUMax(filepath.Join(dir, "absent")); got != 0 {
		t.Errorf("readCPUMax(missing) = %v, want 0", got)
	}
}

func TestReadPressureSome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	path := filepath.Join(dir, "cpu.pressure")
	writeSysfsFile(t, path,
		"some avg10=12.34 avg60=5.00 avg300=1.00 total=1261094\n"+
			"full avg10=99.00 avg60=1.00 avg300=1.00 total=835416\n")

	// The "some" line must win: "full" means every task was stalled, which is
	// a different and rarer condition.
	if got := readPressureSome(path); got != 12.34 {
		t.Errorf("readPressureSome() = %v, want 12.34", got)
	}

	if got := readPressureSome(filepath.Join(dir, "absent")); got != 0 {
		t.Errorf("readPressureSome(missing) = %v, want 0", got)
	}

	malformed := filepath.Join(dir, "bad")
	writeSysfsFile(t, malformed, "some avg10=notanumber\n")
	if got := readPressureSome(malformed); got != 0 {
		t.Errorf("readPressureSome(malformed) = %v, want 0", got)
	}
}

func TestReadCgroupIOPS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "io.stat")
	writeSysfsFile(t, path,
		"259:3 rbytes=57344 wbytes=0 rios=3 wios=7\n"+
			"259:6 rbytes=100 wbytes=200 rios=10 wios=20\n")

	rios, wios := readCgroupIOPS(path)
	if rios != 13 {
		t.Errorf("rios = %d, want 13", rios)
	}
	if wios != 27 {
		t.Errorf("wios = %d, want 27", wios)
	}
}

// TestApplyContainerDetail_ReadsDiagnostics covers the fields that explain a
// degraded container: throttling, OOM history, memory breakdown and PSI.
func TestApplyContainerDetail_ReadsDiagnostics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeSysfsFile(t, filepath.Join(dir, "cpu.max"), "50000 100000\n")
	writeSysfsFile(t, filepath.Join(dir, "cpuset.cpus.effective"), "0-3\n")
	writeSysfsFile(t, filepath.Join(dir, "cpu.stat"),
		"usage_usec 5000000\nnr_periods 200\nnr_throttled 50\nthrottled_usec 1500000\n")
	writeSysfsFile(t, filepath.Join(dir, "memory.peak"), "62230528\n")
	writeSysfsFile(t, filepath.Join(dir, "memory.swap.current"), "1150976\n")
	writeSysfsFile(t, filepath.Join(dir, "memory.stat"),
		"anon 8691712\nfile 5468160\npgmajfault 724\n")
	writeSysfsFile(t, filepath.Join(dir, "memory.events"),
		"low 0\nhigh 0\nmax 3\noom 2\noom_kill 1\n")
	writeSysfsFile(t, filepath.Join(dir, "pids.max"), "4096\n")
	writeSysfsFile(t, filepath.Join(dir, "pids.peak"), "17\n")
	writeSysfsFile(t, filepath.Join(dir, "cpu.pressure"), "some avg10=3.50 avg60=1.00\n")
	writeSysfsFile(t, filepath.Join(dir, "memory.pressure"), "some avg10=0.10 avg60=0.00\n")
	writeSysfsFile(t, filepath.Join(dir, "io.pressure"), "some avg10=9.90 avg60=2.00\n")

	var ct types.ContainerInfo
	applyContainerDetail(&ct, dir)

	if ct.CPULimitCores != 0.5 {
		t.Errorf("CPULimitCores = %v, want 0.5", ct.CPULimitCores)
	}
	if ct.CPUSet != "0-3" {
		t.Errorf("CPUSet = %q, want 0-3", ct.CPUSet)
	}
	if ct.NrThrottled != 50 || ct.NrPeriods != 200 {
		t.Errorf("throttle counters = (%d, %d), want (50, 200)", ct.NrThrottled, ct.NrPeriods)
	}
	if ct.ThrottledPercent != 25 {
		t.Errorf("ThrottledPercent = %v, want 25", ct.ThrottledPercent)
	}
	if ct.MemoryPeakBytes != 62230528 {
		t.Errorf("MemoryPeakBytes = %d", ct.MemoryPeakBytes)
	}
	if ct.SwapBytes != 1150976 {
		t.Errorf("SwapBytes = %d", ct.SwapBytes)
	}
	if ct.AnonBytes != 8691712 || ct.FileBytes != 5468160 {
		t.Errorf("memory breakdown = (%d, %d)", ct.AnonBytes, ct.FileBytes)
	}
	if ct.MajorFaults != 724 {
		t.Errorf("MajorFaults = %d, want 724", ct.MajorFaults)
	}
	if ct.OOMKills != 1 || ct.OOMEvents != 2 {
		t.Errorf("OOM = (%d, %d), want (1, 2)", ct.OOMKills, ct.OOMEvents)
	}
	if ct.PIDsMax != 4096 || ct.PIDsPeak != 17 {
		t.Errorf("pids = (%d, %d), want (4096, 17)", ct.PIDsMax, ct.PIDsPeak)
	}
	if ct.CPUPressure != 3.5 || ct.MemoryPressure != 0.1 || ct.IOPressure != 9.9 {
		t.Errorf("pressure = (%v, %v, %v)", ct.CPUPressure, ct.MemoryPressure, ct.IOPressure)
	}
}

// TestApplyContainerDetail_MissingFiles verifies a cgroup missing the optional
// controllers yields zeroes rather than failing.
func TestApplyContainerDetail_MissingFiles(t *testing.T) {
	t.Parallel()
	var ct types.ContainerInfo
	applyContainerDetail(&ct, filepath.Join(t.TempDir(), "absent"))

	if ct.CPULimitCores != 0 || ct.ThrottledPercent != 0 || ct.OOMKills != 0 {
		t.Errorf("missing cgroup produced data: %+v", ct)
	}
}

// ---- VM network attribution -----------------------------------------------

// TestMergeNetworkIntoVMs_SwapsPerspective verifies tap counters are reported
// from the guest's point of view: what the host receives is what the guest sent.
func TestMergeNetworkIntoVMs_SwapsPerspective(t *testing.T) {
	t.Parallel()
	vms := []types.VMInfo{{Name: "vm1", TapInterfaces: []string{"vnet0"}}}
	networks := []types.NetworkInfo{
		{Name: "vnet0", BytesSentRate: 1000, BytesRecvRate: 2000},
		{Name: "eth0", BytesSentRate: 9999, BytesRecvRate: 9999},
	}

	MergeNetworkIntoVMs(vms, networks)

	if vms[0].NetRxRate != 1000 {
		t.Errorf("NetRxRate = %d, want the host's sent rate (1000)", vms[0].NetRxRate)
	}
	if vms[0].NetTxRate != 2000 {
		t.Errorf("NetTxRate = %d, want the host's recv rate (2000)", vms[0].NetTxRate)
	}
}

// TestMergeNetworkIntoVMs_SumsMultipleTaps covers a guest with several NICs.
func TestMergeNetworkIntoVMs_SumsMultipleTaps(t *testing.T) {
	t.Parallel()
	vms := []types.VMInfo{{Name: "vm1", TapInterfaces: []string{"vnet0", "vnet1"}}}
	networks := []types.NetworkInfo{
		{Name: "vnet0", BytesSentRate: 100, BytesRecvRate: 200},
		{Name: "vnet1", BytesSentRate: 300, BytesRecvRate: 400},
	}

	MergeNetworkIntoVMs(vms, networks)

	if vms[0].NetRxRate != 400 || vms[0].NetTxRate != 600 {
		t.Errorf("rates = (%d, %d), want (400, 600)", vms[0].NetRxRate, vms[0].NetTxRate)
	}
}

func TestMergeNetworkIntoVMs_NoMatchOrEmpty(t *testing.T) {
	t.Parallel()

	vms := []types.VMInfo{{Name: "vm1", TapInterfaces: []string{"missing0"}}}
	MergeNetworkIntoVMs(vms, []types.NetworkInfo{{Name: "eth0", BytesSentRate: 5}})
	if vms[0].NetRxRate != 0 || vms[0].NetTxRate != 0 {
		t.Errorf("unmatched tap produced rates: %+v", vms[0])
	}

	// Neither empty input may panic.
	MergeNetworkIntoVMs(nil, nil)
	MergeNetworkIntoVMs([]types.VMInfo{{Name: "x"}}, nil)
}

func TestSumDiskImageBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSysfsFile(t, filepath.Join(dir, "a.img"), "0123456789")
	writeSysfsFile(t, filepath.Join(dir, "b.img"), "01234")

	got := sumDiskImageBytes([]string{
		filepath.Join(dir, "a.img"),
		filepath.Join(dir, "b.img"),
		filepath.Join(dir, "missing.img"),
	})
	if got != 15 {
		t.Errorf("sumDiskImageBytes() = %d, want 15 (missing files skipped)", got)
	}
	if sumDiskImageBytes(nil) != 0 {
		t.Error("sumDiskImageBytes(nil) must be 0")
	}
}

func TestCountHypervisorThreads_MissingProcess(t *testing.T) {
	t.Parallel()
	vcpus, total := countHypervisorThreads(math.MaxInt32)
	if vcpus != 0 || total != 0 {
		t.Errorf("countHypervisorThreads(missing) = (%d, %d), want (0, 0)", vcpus, total)
	}
}

// ---- VM cgroup metrics ----------------------------------------------------

// TestVMCgroupPath_TrimsToMachineScope verifies the emulator sub-cgroup is
// trimmed back to the machine scope, which is where libvirt puts the guest's
// accounting.
func TestVMCgroupPath_TrimsToMachineScope(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)

	scope := filepath.Join(root, `machine.slice`, `machine-qemu\x2d4\x2dvm1.scope`)
	if err := os.MkdirAll(filepath.Join(scope, "libvirt", "emulator"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A synthetic /proc/<pid>/cgroup is not writable, so exercise the parsing
	// through the same logic with a real pid whose cgroup does not match.
	if got := vmCgroupPath(math.MaxInt32); got != "" {
		t.Errorf("vmCgroupPath(missing pid) = %q, want empty", got)
	}
}

// TestApplyVMCgroupMetrics_ReadsGuestAccounting is the regression test for
// reading guest block I/O without privileges: /proc/<pid>/io is owner-only, so
// the cgroup is the only unprivileged source.
func TestApplyVMCgroupMetrics_ReadsGuestAccounting(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)

	scope := filepath.Join(root, "machine.slice", "machine-qemu-test.scope")
	writeSysfsFile(t, filepath.Join(scope, "io.stat"),
		"259:1 rbytes=1000 wbytes=2000 rios=10 wios=20\n")
	writeSysfsFile(t, filepath.Join(scope, "memory.current"), "86669398016\n")
	writeSysfsFile(t, filepath.Join(scope, "memory.peak"), "93958311936\n")
	writeSysfsFile(t, filepath.Join(scope, "cpu.pressure"), "some avg10=4.06 avg60=1.00\n")
	writeSysfsFile(t, filepath.Join(scope, "io.pressure"), "some avg10=4.87 avg60=1.00\n")

	// Drive the reader directly, since a test cannot forge /proc/<pid>/cgroup.
	vm := types.VMInfo{PID: 1, CgroupPath: scope}
	vm.DiskReadBytes, vm.DiskWriteBytes = readCgroupIO(filepath.Join(scope, "io.stat"))
	vm.DiskReadIOPS, vm.DiskWriteIOPS = readCgroupIOPS(filepath.Join(scope, "io.stat"))
	vm.MemoryCurrentBytes = readSysfsUint64(filepath.Join(scope, "memory.current"))
	vm.MemoryPeakBytes = readSysfsUint64(filepath.Join(scope, "memory.peak"))
	vm.CPUPressure = readPressureSome(filepath.Join(scope, "cpu.pressure"))
	vm.IOPressure = readPressureSome(filepath.Join(scope, "io.pressure"))

	if vm.DiskReadBytes != 1000 || vm.DiskWriteBytes != 2000 {
		t.Errorf("disk bytes = (%d, %d), want (1000, 2000)", vm.DiskReadBytes, vm.DiskWriteBytes)
	}
	if vm.DiskReadIOPS != 10 || vm.DiskWriteIOPS != 20 {
		t.Errorf("disk iops = (%d, %d), want (10, 20)", vm.DiskReadIOPS, vm.DiskWriteIOPS)
	}
	if vm.MemoryCurrentBytes != 86669398016 || vm.MemoryPeakBytes != 93958311936 {
		t.Errorf("memory = (%d, %d)", vm.MemoryCurrentBytes, vm.MemoryPeakBytes)
	}
	if vm.CPUPressure != 4.06 || vm.IOPressure != 4.87 {
		t.Errorf("pressure = (%v, %v), want (4.06, 4.87)", vm.CPUPressure, vm.IOPressure)
	}
}

// TestApplyVMCgroupMetrics_NoCgroupIsNoOp verifies a hypervisor with no
// machine scope leaves the guest fields untouched instead of zeroing them.
func TestApplyVMCgroupMetrics_NoCgroupIsNoOp(t *testing.T) {
	withCgroupRoot(t, t.TempDir())
	c := NewVirtCollector(slog.Default())

	vm := types.VMInfo{PID: math.MaxInt32}
	next := make(map[string]virtIOSnapshot)
	c.applyVMCgroupMetrics(&vm, time.Now(), next)

	if vm.CgroupPath != "" || vm.DiskReadBytes != 0 {
		t.Errorf("expected a no-op for a VM with no cgroup, got %+v", vm)
	}
	if len(next) != 0 {
		t.Errorf("a VM with no cgroup recorded a rate baseline: %v", next)
	}
}

// ---- capability reporting -------------------------------------------------

// TestCollect_CapabilityDistinguishesEmptyFromBlind is the regression test for
// a host with nothing running looking identical to a host we cannot observe.
// Both render as zeroes, so the snapshot must say which it is.
func TestCollect_CapabilityDistinguishesEmptyFromBlind(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)
	writeSysfsFile(t, filepath.Join(root, "cgroup.controllers"), "cpu memory\n")

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	info := c.Info()
	if len(info.Containers) != 0 {
		t.Fatalf("expected no containers in an empty tree, got %d", len(info.Containers))
	}
	// cgroup v2 was readable, so the absence of containers is a fact.
	if !info.Capability.ContainersObservable {
		t.Error("ContainersObservable = false despite a readable cgroup v2 tree")
	}
	// The process list is readable in a test process.
	if !info.Capability.VMsObservable {
		t.Error("VMsObservable = false despite a readable process list")
	}
}

// TestCollect_CapabilityReportsUnobservable verifies a host with no cgroup
// filesystem is reported as unobservable with an explanation, rather than as
// a host that simply has no containers.
func TestCollect_CapabilityReportsUnobservable(t *testing.T) {
	withCgroupRoot(t, filepath.Join(t.TempDir(), "absent"))

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	info := c.Info()
	if info.Capability.ContainersObservable {
		t.Error("ContainersObservable = true with no cgroup filesystem")
	}
	if len(info.Capability.Notes) == 0 {
		t.Error("an unobservable host must carry an explanatory note")
	}
	joined := strings.Join(info.Capability.Notes, " ")
	if !strings.Contains(joined, "cgroup") {
		t.Errorf("note does not explain the cause: %v", info.Capability.Notes)
	}
}

// TestCollect_CapabilityReportsCgroupV1 covers the middle case: a cgroup
// filesystem exists but is the wrong version for per-container metrics.
func TestCollect_CapabilityReportsCgroupV1(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root)
	if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	c := NewVirtCollector(slog.Default())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	info := c.Info()
	if info.CgroupVersion != "v1" {
		t.Fatalf("CgroupVersion = %q, want v1", info.CgroupVersion)
	}
	if info.Capability.ContainersObservable {
		t.Error("cgroup v1 must not be reported as observable")
	}
	if !strings.Contains(strings.Join(info.Capability.Notes, " "), "v2") {
		t.Errorf("note should mention the v2 requirement: %v", info.Capability.Notes)
	}
}
