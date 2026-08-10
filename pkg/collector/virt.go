package collector

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	psprocess "github.com/shirou/gopsutil/v4/process"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Container runtime names reported in ContainerInfo.Runtime.
const (
	runtimeDocker     = "docker"
	runtimePodman     = "podman"
	runtimeContainerd = "containerd"
	runtimeCRIO       = "crio"
	runtimeLXC        = "lxc"
)

// shortIDLen matches the ID length container tooling displays.
const shortIDLen = 12

// containerScopePrefixes maps a cgroup directory prefix to the runtime that
// creates it. Every mainstream runtime encodes its container ID in the cgroup
// name, which is why container accounting needs no daemon socket.
var containerScopePrefixes = map[string]string{
	"docker-":          runtimeDocker,
	"libpod-":          runtimePodman,
	"crio-":            runtimeCRIO,
	"cri-containerd-":  runtimeContainerd,
	"containerd-":      runtimeContainerd,
	"lxc-":             runtimeLXC,
	"lxc.payload.":     runtimeLXC,
	"docker/":          runtimeDocker,
	"kubepods-besteff": runtimeContainerd,
}

// cgroupRoot is the cgroup filesystem mount point. It is a variable so tests
// can point the collector at a synthetic tree.
var cgroupRoot = "/sys/fs/cgroup"

// procRoot is where per-process kernel state lives. It is a variable so tests
// can supply a synthetic process tree rather than depending on whatever the
// build host happens to be running.
var procRoot = "/proc"

// hypervisorProcesses maps a process name prefix to the hypervisor it denotes.
var hypervisorProcesses = map[string]string{
	"qemu-system":  "qemu/kvm",
	"qemu-kvm":     "qemu/kvm",
	"cloud-hyper":  "cloud-hypervisor",
	"VBoxHeadless": "virtualbox",
	"vmware-vmx":   "vmware",
}

// qemuGuestName extracts the guest name from a "-name guest=NAME,..." argument.
var qemuGuestName = regexp.MustCompile(`guest=([^,\s]+)`)

// macAddress matches the MAC in a qemu -device netdev argument.
var macAddress = regexp.MustCompile(`"?mac"?[=:]"?([0-9a-fA-F:]{17})"?`)

// virtIOSnapshot records a container's cumulative counters for rate maths.
type virtIOSnapshot struct {
	CPUUsageUsec uint64
	ReadBytes    uint64
	WriteBytes   uint64
	At           time.Time
}

// VirtCollector reports containers and virtual machines running on the host.
//
// Containers are read from cgroup accounting files and VMs from the hypervisor
// process command line. Both are read-only and unprivileged: no runtime daemon
// socket is opened and no guest agent is contacted.
type VirtCollector struct {
	info   atomic.Pointer[types.VirtInfo]
	logger *slog.Logger

	// prev and prevVM are only touched from Collect, which runs on a single
	// goroutine. VM rates are kept separately so container churn cannot evict
	// a guest's baseline.
	prev   map[string]virtIOSnapshot
	prevVM map[string]virtIOSnapshot

	// processesReadable records whether the last VM scan could read the
	// process list, which distinguishes "no guests" from "cannot tell".
	processesReadable bool
}

// NewVirtCollector returns a VirtCollector with default paths.
func NewVirtCollector(logger *slog.Logger) *VirtCollector {
	if logger == nil {
		logger = slog.Default()
	}
	c := &VirtCollector{
		logger: logger,
		prev:   make(map[string]virtIOSnapshot),
		prevVM: make(map[string]virtIOSnapshot),
	}
	c.info.Store(&types.VirtInfo{})
	return c
}

// Info returns the most recently collected virtualization data.
func (c *VirtCollector) Info() types.VirtInfo {
	return *c.info.Load()
}

// Collect refreshes container and VM state.
func (c *VirtCollector) Collect() error {
	now := time.Now()

	info := types.VirtInfo{
		CgroupVersion: detectCgroupVersion(),
	}

	containers := c.collectContainers(now)
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Name != containers[j].Name {
			return containers[i].Name < containers[j].Name
		}
		return containers[i].ID < containers[j].ID
	})
	for i := range containers {
		containers[i].Index = i
	}
	info.Containers = containers

	seen := make(map[string]struct{}, len(containers))
	for _, ct := range containers {
		if _, ok := seen[ct.Runtime]; !ok {
			seen[ct.Runtime] = struct{}{}
			info.Runtimes = append(info.Runtimes, ct.Runtime)
		}
	}
	sort.Strings(info.Runtimes)

	info.Capability.ContainersObservable = info.CgroupVersion == "v2"
	if !info.Capability.ContainersObservable {
		if info.CgroupVersion == "v1" {
			info.Capability.Notes = append(info.Capability.Notes,
				"cgroup v1 detected; per-container metrics require cgroup v2")
		} else {
			info.Capability.Notes = append(info.Capability.Notes,
				"no cgroup filesystem found; container metrics are unavailable")
		}
	}

	vms := c.collectVMs(now)
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	for i := range vms {
		vms[i].Index = i
	}
	info.VMs = vms

	// The process list is what VM detection scans; if it could not be read the
	// absence of guests is unknown rather than confirmed.
	info.Capability.VMsObservable = c.processesReadable
	if !info.Capability.VMsObservable {
		info.Capability.Notes = append(info.Capability.Notes,
			"process list unreadable; virtual machines cannot be detected")
	}

	c.info.Store(&info)
	return nil
}

// detectCgroupVersion reports which cgroup hierarchy is mounted. cgroup v2 is
// identified by the unified controller file at the root.
func detectCgroupVersion() string {
	if pathExists(filepath.Join(cgroupRoot, "cgroup.controllers")) {
		return "v2"
	}
	if dirExists(filepath.Join(cgroupRoot, "memory")) {
		return "v1"
	}
	return ""
}

// collectContainers walks the cgroup tree and builds one entry per container.
func (c *VirtCollector) collectContainers(now time.Time) []types.ContainerInfo {
	// Keep a zero-valued VirtCollector usable rather than panicking on the
	// first rate calculation.
	if c.prev == nil {
		c.prev = make(map[string]virtIOSnapshot)
	}

	scopes := findContainerCgroups(cgroupRoot)
	if len(scopes) == 0 {
		return nil
	}

	next := make(map[string]virtIOSnapshot, len(scopes))
	result := make([]types.ContainerInfo, 0, len(scopes))

	for _, s := range scopes {
		ct := c.buildContainer(s, now, next)
		if ct == nil {
			continue
		}
		result = append(result, *ct)
	}

	c.prev = next
	return result
}

// containerScope is a candidate container cgroup directory.
type containerScope struct {
	path    string
	id      string
	runtime string
}

// findContainerCgroups walks root looking for directories whose names encode a
// container ID. The walk is bounded to a few levels because runtimes nest
// containers no deeper than slice/scope or the kubepods QoS tiers.
func findContainerCgroups(root string) []containerScope {
	var found []containerScope

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 4 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			path := filepath.Join(dir, name)

			if id, rt, ok := parseContainerCgroupName(name); ok {
				found = append(found, containerScope{path: path, id: id, runtime: rt})
				continue
			}
			walk(path, depth+1)
		}
	}
	walk(root, 0)

	return found
}

// parseContainerCgroupName extracts the container ID and runtime from a cgroup
// directory name, reporting false when the name is not a container.
func parseContainerCgroupName(name string) (string, string, bool) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(name, ".scope"), ".slice")

	for prefix, rt := range containerScopePrefixes {
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		id := strings.TrimPrefix(trimmed, prefix)
		// A container ID is a long hex string. Anything shorter is a slice or
		// a systemd unit that merely shares the prefix.
		if !isHexID(id) {
			continue
		}
		return id, rt, true
	}
	return "", "", false
}

// isHexID reports whether s looks like a container ID: at least 12 hex digits.
func isHexID(s string) bool {
	if len(s) < shortIDLen {
		return false
	}
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// buildContainer reads one container's accounting files. It returns nil for a
// cgroup that has already exited, which is common in a busy tree.
func (c *VirtCollector) buildContainer(
	s containerScope,
	now time.Time,
	next map[string]virtIOSnapshot,
) *types.ContainerInfo {
	pids := readCgroupPIDs(filepath.Join(s.path, "cgroup.procs"))
	if len(pids) == 0 {
		return nil
	}

	short := s.id
	if len(short) > shortIDLen {
		short = short[:shortIDLen]
	}

	ct := types.ContainerInfo{
		ID:           s.id,
		ShortID:      short,
		Runtime:      s.runtime,
		CgroupPath:   s.path,
		ProcessCount: len(pids),
		PIDs:         readSysfsUint64(filepath.Join(s.path, "pids.current")),
		MemoryBytes:  readSysfsUint64(filepath.Join(s.path, "memory.current")),
		CPUUsageUsec: readCgroupKeyed(filepath.Join(s.path, "cpu.stat"), "usage_usec"),
	}

	// memory.max reads "max" when unlimited, which parses to 0.
	ct.MemoryLimitBytes = readSysfsUint64(filepath.Join(s.path, "memory.max"))
	if ct.MemoryLimitBytes > 0 {
		ct.MemoryPercent = float64(ct.MemoryBytes) / float64(ct.MemoryLimitBytes) * 100
	}

	ct.ReadBytes, ct.WriteBytes = readCgroupIO(filepath.Join(s.path, "io.stat"))
	ct.ReadIOPS, ct.WriteIOPS = readCgroupIOPS(filepath.Join(s.path, "io.stat"))

	// Throttling, memory breakdown, OOM history and PSI.
	applyContainerDetail(&ct, s.path)

	if p, procErr := psprocess.NewProcess(pids[0]); procErr == nil {
		if created, timeErr := p.CreateTime(); timeErr == nil && created > 0 {
			ct.UptimeSeconds = uint64(now.Sub(time.UnixMilli(created)).Seconds())
		}
	}

	// Identity comes from the main process: the runtime's own name lives in a
	// daemon database that would require a socket to query.
	ct.Name, ct.Command = processIdentity(pids[0])
	if ct.Name == "" {
		ct.Name = short
	}

	snap := virtIOSnapshot{
		CPUUsageUsec: ct.CPUUsageUsec,
		ReadBytes:    ct.ReadBytes,
		WriteBytes:   ct.WriteBytes,
		At:           now,
	}
	next[s.id] = snap

	if prev, ok := c.prev[s.id]; ok {
		elapsed := now.Sub(prev.At).Seconds()
		if elapsed > 0 {
			if ct.CPUUsageUsec >= prev.CPUUsageUsec {
				deltaSec := float64(ct.CPUUsageUsec-prev.CPUUsageUsec) / 1e6
				ct.CPUPercent = deltaSec / elapsed * 100
			}
			if ct.ReadBytes >= prev.ReadBytes {
				ct.ReadBytesRate = uint64(float64(ct.ReadBytes-prev.ReadBytes) / elapsed)
			}
			if ct.WriteBytes >= prev.WriteBytes {
				ct.WriteBytesRate = uint64(float64(ct.WriteBytes-prev.WriteBytes) / elapsed)
			}
		}
	}

	return &ct
}

// readCgroupPIDs reads a cgroup.procs file into a PID slice.
func readCgroupPIDs(path string) []int32 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int32
	for _, line := range strings.Fields(string(raw)) {
		v, convErr := strconv.ParseInt(line, 10, 32)
		if convErr != nil {
			continue
		}
		pids = append(pids, int32(v))
	}
	return pids
}

// readCgroupKeyed reads a "key value" cgroup file and returns one key's value.
func readCgroupKeyed(path, key string) uint64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != key {
			continue
		}
		v, convErr := strconv.ParseUint(fields[1], 10, 64)
		if convErr != nil {
			return 0
		}
		return v
	}
	return 0
}

// readCgroupIO sums the per-device counters in an io.stat file. Lines look
// like "259:6 rbytes=7360512 wbytes=308936704 rios=272 ...".
func readCgroupIO(path string) (uint64, uint64) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}

	var read, write uint64
	for _, line := range strings.Split(string(raw), "\n") {
		for _, field := range strings.Fields(line) {
			key, value, found := strings.Cut(field, "=")
			if !found {
				continue
			}
			v, convErr := strconv.ParseUint(value, 10, 64)
			if convErr != nil {
				continue
			}
			switch key {
			case "rbytes":
				read += v
			case "wbytes":
				write += v
			}
		}
	}
	return read, write
}

// processIdentity returns a process's name and full command line. Both come
// from procfs entries that are world-readable, so this works for containers
// owned by other users without elevated privileges.
func processIdentity(pid int32) (string, string) {
	name := strings.TrimSpace(readSysfsString(filepath.Join(procRoot, strconv.Itoa(int(pid)), "comm")))

	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(int(pid)), "cmdline"))
	if err != nil {
		return name, ""
	}
	cmd := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
	return name, cmd
}

// collectVMs finds hypervisor processes and describes the guest each one runs.
func (c *VirtCollector) collectVMs(now time.Time) []types.VMInfo {
	if c.prevVM == nil {
		c.prevVM = make(map[string]virtIOSnapshot)
	}
	next := make(map[string]virtIOSnapshot)

	procs, err := psprocess.Processes()
	if err != nil {
		c.logger.Debug("virt: cannot list processes", "error", err)
		c.processesReadable = false
		return nil
	}
	c.processesReadable = true

	var result []types.VMInfo
	for _, p := range procs {
		name, nameErr := p.Name()
		if nameErr != nil {
			continue
		}

		hypervisor := matchHypervisor(name)
		if hypervisor == "" {
			continue
		}

		cmdline, cmdErr := p.Cmdline()
		if cmdErr != nil {
			continue
		}

		vm := parseHypervisorCmdline(cmdline)
		vm.PID = p.Pid
		vm.Hypervisor = hypervisor
		if vm.Name == "" {
			vm.Name = name
		}

		if mi, memErr := p.MemoryInfo(); memErr == nil && mi != nil {
			vm.RSSBytes = mi.RSS
		}
		// Deliberately not p.CPUPercent(): that returns the average since the
		// process started, which for a guest running for days badly understates
		// what it is doing now. Derive a rate from the CPU-time delta instead,
		// the same way containers and processes are measured.
		if times, timesErr := p.Times(); timesErr == nil {
			vm.CPUPercent = c.vmCPUPercent(p.Pid, times.User+times.System, now, next)
		}

		vm.TapInterfaces = tapInterfacesForMACs(vm.MACAddresses)
		vm.VCPUThreads, vm.ThreadCount = countHypervisorThreads(p.Pid)
		vm.DiskImageBytes = sumDiskImageBytes(vm.DiskImages)

		if created, timeErr := p.CreateTime(); timeErr == nil && created > 0 {
			vm.UptimeSeconds = uint64(time.Since(time.UnixMilli(created)).Seconds())
		}

		c.applyVMCgroupMetrics(&vm, now, next)

		result = append(result, vm)
	}

	c.prevVM = next
	return result
}

// matchHypervisor maps a process name to a hypervisor, or "" when the process
// is not one.
func matchHypervisor(procName string) string {
	for prefix, hv := range hypervisorProcesses {
		if strings.HasPrefix(procName, prefix) {
			return hv
		}
	}
	return ""
}

// parseHypervisorCmdline extracts guest configuration from a qemu-style
// command line: name, UUID, vCPU count, configured memory, MACs and disks.
func parseHypervisorCmdline(cmdline string) types.VMInfo {
	var vm types.VMInfo

	fields := strings.Fields(cmdline)
	for i, f := range fields {
		next := ""
		if i+1 < len(fields) {
			next = fields[i+1]
		}

		switch f {
		case "-name":
			if m := qemuGuestName.FindStringSubmatch(next); len(m) > 1 {
				vm.Name = m[1]
			} else if next != "" && !strings.HasPrefix(next, "-") {
				vm.Name = next
			}
		case "-uuid":
			vm.UUID = next
		case "-smp":
			vm.VCPUs = parseSMP(next)
		case "-m":
			vm.MemoryBytes = parseQemuMemory(next)
		case "-accel":
			vm.Accelerator = strings.Split(next, ",")[0]
		case "-machine":
			if vm.Accelerator == "" && strings.Contains(next, "accel=") {
				_, accel, _ := strings.Cut(next, "accel=")
				vm.Accelerator = strings.Split(accel, ",")[0]
			}
		}

		if strings.Contains(f, "filename") && strings.Contains(f, ".img") {
			vm.DiskImages = appendDiskImage(vm.DiskImages, f)
		}
	}

	for _, m := range macAddress.FindAllStringSubmatch(cmdline, -1) {
		if len(m) > 1 {
			vm.MACAddresses = appendUnique(vm.MACAddresses, strings.ToLower(m[1]))
		}
	}

	return vm
}

// parseSMP reads the vCPU count from a -smp argument such as "24,sockets=24".
func parseSMP(s string) int {
	head := strings.Split(s, ",")[0]
	if v, err := strconv.Atoi(head); err == nil {
		return v
	}
	// Some forms omit the leading count and use cpus=N.
	for _, part := range strings.Split(s, ",") {
		key, value, found := strings.Cut(part, "=")
		if found && key == "cpus" {
			if v, err := strconv.Atoi(value); err == nil {
				return v
			}
		}
	}
	return 0
}

// parseQemuMemory converts a -m argument to bytes. Accepted forms are a bare
// megabyte count ("4096"), a suffixed size ("4G") and "size=65536000k".
func parseQemuMemory(s string) uint64 {
	if s == "" {
		return 0
	}
	value := s
	if _, after, found := strings.Cut(s, "size="); found {
		value = strings.Split(after, ",")[0]
	} else {
		value = strings.Split(s, ",")[0]
	}
	if value == "" {
		return 0
	}

	multiplier := uint64(1 << 20) // qemu defaults to megabytes
	last := value[len(value)-1]
	switch last {
	case 'k', 'K':
		multiplier = 1 << 10
		value = value[:len(value)-1]
	case 'm', 'M':
		multiplier = 1 << 20
		value = value[:len(value)-1]
	case 'g', 'G':
		multiplier = 1 << 30
		value = value[:len(value)-1]
	}

	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return n * multiplier
}

// appendDiskImage pulls a disk path out of a qemu -blockdev JSON fragment.
func appendDiskImage(images []string, field string) []string {
	_, after, found := strings.Cut(field, "filename")
	if !found {
		return images
	}
	after = strings.TrimLeft(after, `":= `)
	end := strings.IndexAny(after, `",}`)
	if end > 0 {
		after = after[:end]
	}
	if after == "" {
		return images
	}
	return appendUnique(images, after)
}

// tapInterfacesForMACs returns the host tap devices whose MAC matches one of
// the guest's NICs, linking a VM to its host-side networking.
func tapInterfacesForMACs(macs []string) []string {
	if len(macs) == 0 {
		return nil
	}

	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil
	}

	wanted := make(map[string]struct{}, len(macs))
	for _, m := range macs {
		wanted[strings.ToLower(m)] = struct{}{}
	}

	var taps []string
	for _, e := range entries {
		name := e.Name()
		// A tap device's own MAC differs from the guest NIC MAC by the locally
		// administered bit, so compare on the lower five octets which the
		// hypervisor preserves.
		addr := strings.ToLower(readSysfsString(filepath.Join(sysClassNet, name, "address")))
		if addr == "" {
			continue
		}
		for m := range wanted {
			if addr == m || sameMACSuffix(addr, m) {
				taps = append(taps, name)
				break
			}
		}
	}
	sort.Strings(taps)
	return taps
}

// sameMACSuffix compares the last five octets of two MAC addresses.
func sameMACSuffix(a, b string) bool {
	ap := strings.Split(a, ":")
	bp := strings.Split(b, ":")
	if len(ap) != 6 || len(bp) != 6 {
		return false
	}
	for i := 1; i < 6; i++ {
		if ap[i] != bp[i] {
			return false
		}
	}
	return true
}

// appendUnique appends v to list when it is not already present.
func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// --- container detail helpers ----------------------------------------------

// readCPUMax parses cpu.max ("QUOTA PERIOD", or "max PERIOD" when unlimited)
// and returns the quota expressed in whole cores. 0 means unlimited.
func readCPUMax(path string) float64 {
	raw := strings.Fields(readSysfsString(path))
	if len(raw) < 2 || raw[0] == "max" {
		return 0
	}
	quota, err := strconv.ParseFloat(raw[0], 64)
	if err != nil {
		return 0
	}
	period, err := strconv.ParseFloat(raw[1], 64)
	if err != nil || period == 0 {
		return 0
	}
	return quota / period
}

// readPressureSome returns the "some avg10" value from a PSI file: the share
// of the last ten seconds in which at least one task was stalled on that
// resource.
func readPressureSome(path string) float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			key, value, found := strings.Cut(field, "=")
			if found && key == "avg10" {
				v, convErr := strconv.ParseFloat(value, 64)
				if convErr != nil {
					return 0
				}
				return v
			}
		}
	}
	return 0
}

// readCgroupIOPS sums the per-device read and write operation counters in an
// io.stat file.
func readCgroupIOPS(path string) (uint64, uint64) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}

	var rios, wios uint64
	for _, field := range strings.Fields(string(raw)) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		v, convErr := strconv.ParseUint(value, 10, 64)
		if convErr != nil {
			continue
		}
		switch key {
		case "rios":
			rios += v
		case "wios":
			wios += v
		}
	}
	return rios, wios
}

// applyContainerDetail fills in the diagnostic fields that explain *why* a
// container is slow: CPU throttling, memory pressure, OOM kills and PSI.
func applyContainerDetail(ct *types.ContainerInfo, path string) {
	// CPU quota and throttling.
	ct.CPULimitCores = readCPUMax(filepath.Join(path, "cpu.max"))
	ct.CPUSet = readSysfsString(filepath.Join(path, "cpuset.cpus.effective"))

	cpuStat := filepath.Join(path, "cpu.stat")
	ct.NrPeriods = readCgroupKeyed(cpuStat, "nr_periods")
	ct.NrThrottled = readCgroupKeyed(cpuStat, "nr_throttled")
	ct.ThrottledUsec = readCgroupKeyed(cpuStat, "throttled_usec")
	if ct.NrPeriods > 0 {
		ct.ThrottledPercent = float64(ct.NrThrottled) / float64(ct.NrPeriods) * 100
	}

	// Memory breakdown and history.
	ct.MemoryPeakBytes = readSysfsUint64(filepath.Join(path, "memory.peak"))
	ct.SwapBytes = readSysfsUint64(filepath.Join(path, "memory.swap.current"))

	memStat := filepath.Join(path, "memory.stat")
	ct.AnonBytes = readCgroupKeyed(memStat, "anon")
	ct.FileBytes = readCgroupKeyed(memStat, "file")
	ct.MajorFaults = readCgroupKeyed(memStat, "pgmajfault")

	memEvents := filepath.Join(path, "memory.events")
	ct.OOMEvents = readCgroupKeyed(memEvents, "oom")
	ct.OOMKills = readCgroupKeyed(memEvents, "oom_kill")

	// Fork limits.
	ct.PIDsMax = readSysfsUint64(filepath.Join(path, "pids.max"))
	ct.PIDsPeak = readSysfsUint64(filepath.Join(path, "pids.peak"))

	// Stall pressure.
	ct.CPUPressure = readPressureSome(filepath.Join(path, "cpu.pressure"))
	ct.MemoryPressure = readPressureSome(filepath.Join(path, "memory.pressure"))
	ct.IOPressure = readPressureSome(filepath.Join(path, "io.pressure"))
}

// --- VM detail helpers ------------------------------------------------------

// countHypervisorThreads returns the number of vCPU threads and the total
// thread count for a hypervisor process. qemu names each vCPU thread
// "CPU N/KVM", which distinguishes them from I/O and emulation workers.
func countHypervisorThreads(pid int32) (int, int) {
	taskDir := filepath.Join(procRoot, strconv.Itoa(int(pid)), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return 0, 0
	}

	vcpus := 0
	for _, e := range entries {
		comm := readSysfsString(filepath.Join(taskDir, e.Name(), "comm"))
		if strings.HasPrefix(comm, "CPU ") {
			vcpus++
		}
	}
	return vcpus, len(entries)
}

// sumDiskImageBytes stats each guest image and returns the total size. Images
// owned by another user are skipped rather than failing the whole reading.
func sumDiskImageBytes(paths []string) uint64 {
	var total uint64
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		total += uint64(st.Size())
	}
	return total
}

// MergeNetworkIntoVMs attributes host tap-device throughput to the guest behind
// it, so a VM's real network usage is visible without a guest agent.
func MergeNetworkIntoVMs(vms []types.VMInfo, networks []types.NetworkInfo) {
	if len(vms) == 0 || len(networks) == 0 {
		return
	}

	byName := make(map[string]types.NetworkInfo, len(networks))
	for _, n := range networks {
		byName[n.Name] = n
	}

	for i := range vms {
		var rx, tx uint64
		for _, tap := range vms[i].TapInterfaces {
			n, ok := byName[tap]
			if !ok {
				continue
			}
			// A tap's counters are from the host's point of view, so traffic
			// the host receives is traffic the guest sent. Swap them so the
			// figures read from the guest's perspective.
			rx += uint64(n.BytesSentRate)
			tx += uint64(n.BytesRecvRate)
		}
		vms[i].NetRxRate = rx
		vms[i].NetTxRate = tx
	}
}

// vmCgroupPath returns the libvirt machine scope for a hypervisor process.
//
// libvirt nests the emulator under
// /machine.slice/machine-qemu\x2d<id>\x2d<name>.scope/libvirt/emulator, and
// the accounting lives on the top-level .scope. This is the unprivileged route
// to a guest's block I/O: /proc/<pid>/io is mode 0400 and owned by the
// hypervisor user, so no group membership can grant access to it.
func vmCgroupPath(pid int32) string {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(int(pid)), "cgroup"))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(raw), "\n") {
		// cgroup v2 lines look like "0::/machine.slice/...".
		_, path, found := strings.Cut(line, "::")
		if !found || path == "" {
			continue
		}
		idx := strings.Index(path, ".scope")
		if idx < 0 {
			continue
		}
		scope := path[:idx+len(".scope")]
		full := filepath.Join(cgroupRoot, scope)
		if dirExists(full) {
			return full
		}
	}
	return ""
}

// applyVMCgroupMetrics fills in the guest metrics that come from its cgroup
// rather than from the hypervisor process.
func (c *VirtCollector) applyVMCgroupMetrics(vm *types.VMInfo, now time.Time, next map[string]virtIOSnapshot) {
	path := vmCgroupPath(vm.PID)
	if path == "" {
		return
	}
	vm.CgroupPath = path

	vm.DiskReadBytes, vm.DiskWriteBytes = readCgroupIO(filepath.Join(path, "io.stat"))
	vm.DiskReadIOPS, vm.DiskWriteIOPS = readCgroupIOPS(filepath.Join(path, "io.stat"))
	vm.MemoryCurrentBytes = readSysfsUint64(filepath.Join(path, "memory.current"))
	vm.MemoryPeakBytes = readSysfsUint64(filepath.Join(path, "memory.peak"))
	vm.CPUPressure = readPressureSome(filepath.Join(path, "cpu.pressure"))
	vm.IOPressure = readPressureSome(filepath.Join(path, "io.pressure"))

	// Disk rates need a baseline, keyed separately from container IDs.
	key := "vm:" + strconv.Itoa(int(vm.PID))
	next[key] = virtIOSnapshot{
		ReadBytes:    vm.DiskReadBytes,
		WriteBytes:   vm.DiskWriteBytes,
		CPUUsageUsec: readCgroupKeyed(filepath.Join(path, "cpu.stat"), "usage_usec"),
		At:           now,
	}

	prev, ok := c.prevVM[key]
	if !ok {
		return
	}
	elapsed := now.Sub(prev.At).Seconds()
	if elapsed <= 0 {
		return
	}
	if vm.DiskReadBytes >= prev.ReadBytes {
		vm.DiskReadRate = uint64(float64(vm.DiskReadBytes-prev.ReadBytes) / elapsed)
	}
	if vm.DiskWriteBytes >= prev.WriteBytes {
		vm.DiskWriteRate = uint64(float64(vm.DiskWriteBytes-prev.WriteBytes) / elapsed)
	}
}

// vmCPUPercent converts a hypervisor's cumulative CPU seconds into a
// percentage of one core over the sampling window. A guest using 21 of its 24
// vCPUs reports 2100, the same convention as top and "docker stats".
func (c *VirtCollector) vmCPUPercent(
	pid int32,
	totalSeconds float64,
	now time.Time,
	next map[string]virtIOSnapshot,
) float64 {
	key := "vmcpu:" + strconv.Itoa(int(pid))

	// Store the reading whether or not a rate can be produced this cycle.
	prev, ok := c.prevVM[key]
	next[key] = virtIOSnapshot{
		CPUUsageUsec: uint64(totalSeconds * 1e6),
		At:           now,
	}
	if !ok {
		return 0
	}

	elapsed := now.Sub(prev.At).Seconds()
	if elapsed <= 0 {
		return 0
	}
	prevSeconds := float64(prev.CPUUsageUsec) / 1e6
	if totalSeconds < prevSeconds {
		return 0
	}
	return (totalSeconds - prevSeconds) / elapsed * 100
}
