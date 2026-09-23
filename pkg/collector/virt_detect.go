package collector

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Installed-software detection decides whether the dashboard shows the
// Containers and Virtualization tabs at all (#187). The owner's rulings:
//
//   - Containers: a runtime is installed, or containers are seen running.
//   - Virtualization: a VM stack is installed, or VMs are seen running.
//     /dev/kvm alone does not count; nearly every x86 CPU exposes it.
//   - Unknown shows the tab. Only a node that proves absence may hide one.
//
// "Installed" is a fixed set of files: executables in the system binary
// directories, daemon API sockets, kernel modules and the Xen control-domain
// flag. Nothing is executed and no socket is opened; existence is the whole
// question.

// hostRoot is the filesystem root the install probes resolve against. It is a
// variable so tests can point detection at a synthetic tree.
var hostRoot = "/"

// installRecheckInterval is how long an install scan is reused.
//
// A scan lists eight binary directories and stats a dozen other paths: about a
// millisecond, most of it listing /usr/bin. Collection can run every 500 ms, so
// scanning each time would spend that millisecond on an answer that changes
// only when someone installs or removes a package.
//
// Thirty seconds bounds how long a newly installed but idle runtime goes
// unnoticed, which is shorter than it takes to install one and then go and
// look at the dashboard. It bounds nothing else: running containers and VMs
// are evidence in every workload scan, so one that starts shows its tab in the
// same snapshot that first lists it.
const installRecheckInterval = 30 * time.Second

// Stack names for installed virtualization software that has no container
// runtime constant in virt.go.
const (
	runtimeNerdctl       = "nerdctl"
	runtimeLXD           = "lxd"
	runtimeIncus         = "incus"
	stackQEMU            = "qemu"
	stackLibvirt         = "libvirt"
	stackVirtualBox      = "virtualbox"
	stackXen             = "xen"
	stackCloudHypervisor = "cloud-hypervisor"
	stackFirecracker     = "firecracker"
	stackVMware          = "vmware"
)

// runningHypervisorStacks maps a VM's hypervisor label to the stack name its
// install evidence uses, where the two differ.
var runningHypervisorStacks = map[string]string{
	"qemu/kvm": stackQEMU,
}

// section is which tab a piece of evidence speaks for.
type section int

const (
	sectionContainers section = iota
	sectionHypervisor
	sectionCount
)

// stackProbe names the stack a probed file belongs to and the tab it decides.
type stackProbe struct {
	stack   string
	section section
}

// systemBinDirs are the directories searched for executables, in the order
// their paths are reported. The /usr directories come first so that on a
// merged-/usr system, where /bin and /sbin are symlinks into /usr, the path
// reported is the real one. /usr/libexec is here for RHEL's qemu-kvm, which
// lives nowhere else; /snap/bin for the docker and LXD snaps;
// /run/current-system/sw/bin for NixOS, whose system profile is the only
// place its packages appear, as symlinks into /nix/store.
var systemBinDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/usr/libexec",
	"/sbin",
	"/bin",
	"/snap/bin",
	"/run/current-system/sw/bin",
}

// binaryProbes maps an executable name to the stack it proves installed.
// Daemonless podman usually has no socket, which is why executables count.
var binaryProbes = map[string]stackProbe{
	"docker":           {runtimeDocker, sectionContainers},
	"dockerd":          {runtimeDocker, sectionContainers},
	"containerd":       {runtimeContainerd, sectionContainers},
	"nerdctl":          {runtimeNerdctl, sectionContainers},
	"podman":           {runtimePodman, sectionContainers},
	"crio":             {runtimeCRIO, sectionContainers},
	"lxc-start":        {runtimeLXC, sectionContainers},
	"lxd":              {runtimeLXD, sectionContainers},
	"incus":            {runtimeIncus, sectionContainers},
	"qemu-kvm":         {stackQEMU, sectionHypervisor},
	"libvirtd":         {stackLibvirt, sectionHypervisor},
	"virtqemud":        {stackLibvirt, sectionHypervisor},
	"VBoxManage":       {stackVirtualBox, sectionHypervisor},
	"xl":               {stackXen, sectionHypervisor},
	"cloud-hypervisor": {stackCloudHypervisor, sectionHypervisor},
	"firecracker":      {stackFirecracker, sectionHypervisor},
	"vmware":           {stackVMware, sectionHypervisor},
}

// binaryPrefixProbes maps an executable name prefix to its stack, for
// software installed as one binary per target: qemu-system-x86_64,
// qemu-system-aarch64 and so on.
var binaryPrefixProbes = map[string]stackProbe{
	"qemu-system-": {stackQEMU, sectionHypervisor},
}

// socketProbes maps a daemon API socket to its stack. Only /run is probed:
// /var/run is a symlink to it on every current distribution, and probing both
// would report one socket twice.
var socketProbes = map[string]stackProbe{
	"/run/docker.sock":                {runtimeDocker, sectionContainers},
	"/run/containerd/containerd.sock": {runtimeContainerd, sectionContainers},
	"/run/podman/podman.sock":         {runtimePodman, sectionContainers},
	"/run/crio/crio.sock":             {runtimeCRIO, sectionContainers},
	"/run/libvirt/libvirt-sock":       {stackLibvirt, sectionHypervisor},
	"/run/libvirt/virtqemud-sock":     {stackLibvirt, sectionHypervisor},
}

// sysModuleDir lists loaded kernel modules, one directory each.
const sysModuleDir = "/sys/module"

// moduleProbes maps a kernel module to the stack that loads it.
var moduleProbes = map[string]stackProbe{
	"vboxdrv": {stackVirtualBox, sectionHypervisor},
	"vmmon":   {stackVMware, sectionHypervisor},
}

// xenCapabilitiesPath holds "control_d" in a Xen control domain (dom0) only.
// A Xen guest has /proc/xen too, and a guest is not a host with a VM stack,
// so the directory's existence is not the evidence; the flag is.
const xenCapabilitiesPath = "/proc/xen/capabilities"

// xenControlDomain is the capability a dom0 advertises.
const xenControlDomain = "control_d"

// containerMarkers are files container engines place inside a container. When
// one exists, this process is looking at a container's filesystem, where the
// host's binaries are not the host's: finding none proves nothing, and finding
// one proves only that the image ships it. The markers are the ones systemd's
// container detection uses, plus the Kubernetes service account mount.
//
// A containerd or CRI-O pod with automountServiceAccountToken off leaves none
// of these, which is why selfCgroupPath is checked as well.
var containerMarkers = []string{
	"/.dockerenv",
	"/run/.containerenv",
	"/run/systemd/container",
	"/run/host/container-manager",
	"/var/run/secrets/kubernetes.io",
}

// selfCgroupPath is this process's cgroup membership.
//
// It catches a container that carries no marker file. On a host every process
// sits in a named cgroup ("/system.slice/go-sysmon.service"); in a container
// it is either the namespace root "/", under a private cgroup namespace, or a
// path through a container scope, under the host's. Both are read as
// "containerised". A host that genuinely keeps processes in the root cgroup --
// one without systemd -- is misread as a container, and the cost is a tab
// shown as unknown rather than hidden: the error the rulings allow.
const selfCgroupPath = "/proc/self/cgroup"

// selfMountInfo and selfStatus locate the /proc mount's options and this
// process's capabilities, which together say whether the process list shows
// every user's processes.
const (
	selfMountInfo = "/proc/self/mountinfo"
	selfStatus    = "/proc/self/status"
)

// capSysPtrace is CAP_SYS_PTRACE's bit in the CapEff mask. With it, hidepid
// hides nothing.
const capSysPtrace = 19

// Reasons a section is unknown when every probe it has came back empty.
const (
	reasonContainersUnobservable = "the cgroup v2 tree cannot be read, so running containers cannot be seen"
	reasonVMsUnobservable        = "the process list cannot be read, so running virtual machines cannot be seen"
)

// sectionScan is what an install scan found for one tab.
type sectionScan struct {
	evidence []types.StackEvidence
	// unreadable describes the first probe that failed for any reason other
	// than the file not existing. A failed probe proves nothing either way.
	unreadable string
}

// installScan is the result of one pass over the install probes.
type installScan struct {
	sections [sectionCount]sectionScan
	// containerized says why this process is judged to be in a container, or
	// is empty when it is not.
	containerized string
	// hostViewUnsettled is set when a container check failed, so whether the
	// visible filesystem is the host's is not known.
	hostViewUnsettled bool
}

// procViewUnverified says that whether /proc hides processes could not be
// checked. The process list then cannot vouch for being complete, which is
// the same consequence as a restriction that was found.
func procViewUnverified(p string, err error) string {
	return "could not check whether /proc hides processes (" + p + ": " + errorText(err) + ")"
}

// installDetector caches install scans. It is only used from Collect, which
// runs on a single goroutine.
type installDetector struct {
	scannedAt time.Time
	scan      installScan
}

// current returns an install scan no older than installRecheckInterval.
func (d *installDetector) current(now time.Time) installScan {
	stale := d.scannedAt.IsZero() ||
		now.Sub(d.scannedAt) >= installRecheckInterval ||
		now.Before(d.scannedAt)
	if stale {
		d.scan = scanInstalled()
		d.scannedAt = now
	}
	return d.scan
}

// scanInstalled checks every install probe against hostRoot.
func scanInstalled() installScan {
	var scan installScan
	// Container detection runs first because it decides whether binaries are
	// evidence at all.
	scanContainerMarkers(&scan)
	if scan.containerized == "" {
		scanSelfCgroup(&scan)
	}
	// Binaries are only evidence on a filesystem known to be the host's: in a
	// container, or where that could not be settled, they may be the image's.
	if scan.containerized == "" && !scan.hostViewUnsettled {
		scanBinaries(&scan)
	}
	scanFiles(&scan, socketProbes, types.EvidenceSocket, "")
	scanFiles(&scan, moduleProbes, types.EvidenceModule, sysModuleDir)
	scanXen(&scan)
	return scan
}

// hostPath maps a host path onto hostRoot.
func hostPath(p string) string {
	return filepath.Join(hostRoot, p)
}

// isNotFound reports whether err means the path does not exist. ENOTDIR counts:
// a path whose parent is a file cannot exist either.
func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// markUnreadable records a failed probe against a section, keeping the first.
func (s *installScan) markUnreadable(sec section, p string, err error) {
	if s.sections[sec].unreadable == "" {
		s.sections[sec].unreadable = "could not check " + p + ": " + errorText(err)
	}
}

// markAllUnreadable records a failed probe that speaks for every section.
func (s *installScan) markAllUnreadable(p string, err error) {
	for sec := range sectionCount {
		s.markUnreadable(sec, p, err)
	}
}

// add records one piece of evidence.
func (s *installScan) add(probe stackProbe, kind, p string) {
	s.sections[probe.section].evidence = append(s.sections[probe.section].evidence,
		types.StackEvidence{Stack: probe.stack, Kind: kind, Path: p})
}

// errorText strips the path a *fs.PathError repeats, since the reason already
// names it.
func errorText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// scanBinaries lists each system binary directory once. A directory that is a
// symlink to one already listed is skipped, so merged-/usr reports each
// executable once, under its /usr path.
func scanBinaries(scan *installScan) {
	listed := make(map[string]struct{}, len(systemBinDirs))
	for _, dir := range systemBinDirs {
		resolved, err := filepath.EvalSymlinks(hostPath(dir))
		if err != nil {
			if !isNotFound(err) {
				scan.markAllUnreadable(dir, err)
			}
			continue
		}
		if _, seen := listed[resolved]; seen {
			continue
		}
		listed[resolved] = struct{}{}

		entries, err := os.ReadDir(resolved)
		if err != nil {
			if !isNotFound(err) {
				scan.markAllUnreadable(dir, err)
			}
			continue
		}
		for _, e := range entries {
			probe, ok := matchBinary(e.Name())
			if !ok {
				continue
			}
			isFile, err := isFileEntry(resolved, e)
			if err != nil {
				scan.markUnreadable(probe.section, path.Join(dir, e.Name()), err)
				continue
			}
			if isFile {
				scan.add(probe, types.EvidenceBinary, path.Join(dir, e.Name()))
			}
		}
	}
}

// isFileEntry reports whether a directory entry is a file, following a
// symlink to what it points at. A dangling link is not a runtime, however it
// is named, and neither is a directory.
func isFileEntry(dir string, e fs.DirEntry) (bool, error) {
	if e.Type()&fs.ModeSymlink == 0 {
		return !e.IsDir(), nil
	}
	st, err := os.Stat(filepath.Join(dir, e.Name()))
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return !st.IsDir(), nil
}

// matchBinary maps an executable name to the stack it proves installed.
func matchBinary(name string) (stackProbe, bool) {
	if probe, ok := binaryProbes[name]; ok {
		return probe, true
	}
	for prefix, probe := range binaryPrefixProbes {
		if strings.HasPrefix(name, prefix) {
			return probe, true
		}
	}
	return stackProbe{}, false
}

// scanFiles checks a set of paths for existence, each under dir.
func scanFiles(scan *installScan, probes map[string]stackProbe, kind, dir string) {
	for name, probe := range probes {
		p := path.Join("/", dir, name)
		_, err := os.Stat(hostPath(p))
		switch {
		case err == nil:
			scan.add(probe, kind, p)
		case !isNotFound(err):
			scan.markUnreadable(probe.section, p, err)
		}
	}
}

// scanXen records a Xen control domain.
func scanXen(scan *installScan) {
	raw, err := os.ReadFile(hostPath(xenCapabilitiesPath))
	if err != nil {
		if !isNotFound(err) {
			scan.markUnreadable(sectionHypervisor, xenCapabilitiesPath, err)
		}
		return
	}
	if strings.Contains(string(raw), xenControlDomain) {
		scan.add(stackProbe{stackXen, sectionHypervisor}, types.EvidenceControlDomain, xenCapabilitiesPath)
	}
}

// scanContainerMarkers records whether this process is inside a container.
// A marker that cannot be checked leaves that question open, which is as
// inconclusive as a probe that cannot be checked.
func scanContainerMarkers(scan *installScan) {
	for _, marker := range containerMarkers {
		_, err := os.Lstat(hostPath(marker))
		switch {
		case err == nil:
			scan.containerized = marker + " exists"
			return
		case !isNotFound(err):
			scan.markAllUnreadable(marker, err)
			scan.hostViewUnsettled = true
		}
	}
}

// scanSelfCgroup records a container that left no marker file, from the
// cgroup this process is in.
func scanSelfCgroup(scan *installScan) {
	raw, err := os.ReadFile(hostPath(selfCgroupPath))
	if err != nil {
		// Every Linux procfs has this file, so its absence is a masked or
		// partial /proc, which settles nothing.
		scan.markAllUnreadable(selfCgroupPath, err)
		scan.hostViewUnsettled = true
		return
	}
	memberships, allAtRoot := 0, true
	defer func() {
		// Nothing parseable -- an empty or sanitised file -- says nothing
		// about where this process is, which settles nothing either.
		if memberships == 0 && scan.containerized == "" {
			scan.markAllUnreadable(selfCgroupPath,
				&types.ProcStatusFieldError{Path: selfCgroupPath, Field: "cgroup membership"})
			scan.hostViewUnsettled = true
		}
	}()
	for _, line := range strings.Split(string(raw), "\n") {
		// "hierarchy:controllers:path". SplitN keeps any colon in the path.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "/") {
			continue
		}
		memberships++
		hierarchy, cgPath := parts[0], parts[2]
		if cgPath != "/" {
			allAtRoot = false
		}
		// Only the unified hierarchy is judged on "/": on a cgroup v1
		// systemd host every process is at the root of cpuset and friends.
		if hierarchy == "0" && cgPath == "/" {
			scan.containerized = selfCgroupPath + " places sysmon at the root of a cgroup namespace"
			return
		}
		parent := ""
		for _, name := range strings.Split(cgPath, "/") {
			_, _, isContainer := parseContainerCgroup(parent, name)
			parent = name
			if isContainer || strings.HasPrefix(name, "kubepods") {
				scan.containerized = selfCgroupPath + " places sysmon in container cgroup " + cgPath
				return
			}
		}
	}
	// Pure cgroup v1 has no unified line to judge. A v1 host running systemd
	// always has a named, non-root name=systemd membership; every controller
	// at "/" is the root of private cgroup namespaces instead.
	if memberships > 0 && allAtRoot {
		scan.containerized = selfCgroupPath + " places sysmon at the root of every cgroup hierarchy"
	}
}

// procVisibility says why the process list does not show every process on
// the host, or returns "" when it does. hidepid hides other users' processes
// without making the list unreadable, and a private PID namespace hides
// everything outside it, so a libvirt guest would simply not be there and "no
// VMs running" would be a claim the collector cannot back.
//
// It describes a process scan, so it is read with one -- in Collect, beside
// collectVMs -- and never from the cached install scan, which may be older
// or newer than the process list it would be vouching for.
func procVisibility() string {
	hidepid, gid, err := procMountHidepid()
	if err != nil {
		// Missing counts too: every procfs has it, so its absence is a masked
		// /proc whose hidepid and namespace cannot be checked. (No procfs at
		// all fails the process list itself, which says so first.)
		return procViewUnverified(selfMountInfo, err)
	}

	// A private PID namespace -- unshare --pid, systemd's PrivatePIDs=, a
	// container without pid: host -- lists only its own processes, so a guest
	// running outside it is simply not there.
	initialPID, err := inInitialNamespace(selfPIDNamespace, "pid", initialPIDNamespaceInode)
	if err != nil {
		return procViewUnverified(selfPIDNamespace, err)
	}
	if !initialPID {
		return "sysmon is in a private PID namespace, which shows only its own processes"
	}

	if hidepid == "" || hidepid == "0" || hidepid == "off" {
		return ""
	}
	if gid >= 0 && inGroup(gid) {
		return ""
	}
	ptrace, err := hasCapability(capSysPtrace)
	if err != nil {
		return procViewUnverified(selfStatus, err)
	}
	if ptrace {
		// A capability held in a child user namespace (unshare -U, a rootless
		// sandbox) does not reach processes owned by the initial one.
		initial, nsErr := inInitialNamespace(selfUserNamespace, "user", initialUserNamespaceInode)
		if nsErr != nil {
			return procViewUnverified(selfUserNamespace, nsErr)
		}
		if initial {
			return ""
		}
	}
	return "/proc is mounted hidepid=" + hidepid + ", which hides other users' processes from sysmon"
}

// procMountHidepid returns the hidepid and gid options of the /proc mount this
// process sees, the last one mounted there winning. gid is -1 when unset.
func procMountHidepid() (string, int, error) {
	raw, err := os.ReadFile(hostPath(selfMountInfo))
	if err != nil {
		return "", -1, err
	}
	hidepid, gid, found := "", -1, false
	for _, line := range strings.Split(string(raw), "\n") {
		// "id parent maj:min root mountpoint opts [optional...] - fstype source superopts"
		pre, post, separated := strings.Cut(line, " - ")
		if !separated {
			continue
		}
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(tail) < 3 || fields[4] != "/proc" || tail[0] != "proc" {
			continue
		}
		hidepid, gid, found = "", -1, true
		for _, opt := range strings.Split(tail[2], ",") {
			key, value, _ := strings.Cut(opt, "=")
			switch key {
			case "hidepid":
				hidepid = value
			case "gid":
				if n, convErr := strconv.Atoi(value); convErr == nil {
					gid = n
				}
			}
		}
	}
	if !found {
		// A mount table with no procfs on /proc is a filtered one; whatever
		// /proc is, its options are unknown.
		return "", -1, &types.ProcStatusFieldError{Path: selfMountInfo, Field: "procfs mount on /proc"}
	}
	return hidepid, gid, nil
}

// inGroup reports whether this process is a member of gid.
func inGroup(gid int) bool {
	if os.Getegid() == gid {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	return slices.Contains(groups, gid)
}

// statusField returns the value of one field of this process's status file.
func statusField(field string) (string, error) {
	raw, err := os.ReadFile(hostPath(selfStatus))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(line, field+":"); found {
			return strings.TrimSpace(value), nil
		}
	}
	return "", &types.ProcStatusFieldError{Path: selfStatus, Field: field}
}

// hasCapability reports whether bit is set in this process's effective
// capabilities.
func hasCapability(bit uint) (bool, error) {
	value, err := statusField("CapEff")
	if err != nil {
		return false, err
	}
	mask, err := strconv.ParseUint(value, 16, 64)
	if err != nil {
		return false, err
	}
	return mask&(1<<bit) != 0, nil
}

// Initial namespaces have fixed inodes, PROC_PID_INIT_INO and
// PROC_USER_INIT_INO in the kernel's include/linux/proc_ns.h, unchanged since
// namespaces became files in 3.8. Comparing /proc/self/ns/<kind> with them is
// the one answer to "am I in the host's namespace?" that does not depend on
// where procfs was mounted from or on how IDs happen to be mapped: NSpid is
// relative to the namespace that mounted /proc, and a child user namespace
// can carry the identity UID map.
const (
	initialPIDNamespaceInode  = 0xEFFFFFFC
	initialUserNamespaceInode = 0xEFFFFFFD
)

// Links naming this process's PID and user namespaces.
const (
	selfPIDNamespace  = "/proc/self/ns/pid"
	selfUserNamespace = "/proc/self/ns/user"
)

// inInitialNamespace reports whether the namespace link at nsPath, of the
// given kind ("pid", "user"), names the initial namespace.
func inInitialNamespace(nsPath, kind string, initialInode uint64) (bool, error) {
	link, err := os.Readlink(hostPath(nsPath))
	if err != nil {
		return false, err
	}
	inode, found := strings.CutPrefix(link, kind+":[")
	inode, closed := strings.CutSuffix(inode, "]")
	if !found || !closed {
		return false, &types.ProcStatusFieldError{Path: nsPath, Field: kind + ":[inode]"}
	}
	n, err := strconv.ParseUint(inode, 10, 64)
	if err != nil {
		return false, err
	}
	return n == initialInode, nil
}

// runningEvidence turns the stacks of observed workloads into evidence.
func runningEvidence(stacks []string) []types.StackEvidence {
	seen := make(map[string]struct{}, len(stacks))
	var evidence []types.StackEvidence
	for _, stack := range stacks {
		if _, dup := seen[stack]; dup {
			continue
		}
		seen[stack] = struct{}{}
		evidence = append(evidence, types.StackEvidence{Stack: stack, Kind: types.EvidenceRunning})
	}
	return evidence
}

// detectStack decides one tab's tri-state.
//
// Any evidence makes it present. Without evidence it is absent only when every
// source that could have produced some was actually consulted: every install
// probe answered, this process can see the host's filesystem rather than a
// container's, and the running workloads could be observed.
func detectStack(
	found sectionScan,
	containerized string,
	running []types.StackEvidence,
	observable bool,
	unobservableReason string,
) types.StackDetection {
	// A fresh slice: the scan's is cached and shared across collections, and
	// the stored VirtInfo is read concurrently, so neither may be sorted in
	// place.
	evidence := make([]types.StackEvidence, 0, len(found.evidence)+len(running))
	evidence = append(evidence, found.evidence...)
	evidence = append(evidence, running...)

	if len(evidence) > 0 {
		sort.Slice(evidence, func(i, j int) bool {
			a, b := evidence[i], evidence[j]
			if a.Stack != b.Stack {
				return a.Stack < b.Stack
			}
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			return a.Path < b.Path
		})
		return types.StackDetection{State: types.PresencePresent, Evidence: evidence}
	}

	switch {
	case found.unreadable != "":
		return unknownStack(found.unreadable)
	case containerized != "":
		return unknownStack("sysmon is running in a container (" + containerized +
			"), where the host's binaries and sockets are not visible")
	case !observable:
		return unknownStack(unobservableReason)
	}
	return types.StackDetection{State: types.PresenceAbsent, Evidence: evidence}
}

// unknownStack is a detection that could not be settled, and why.
func unknownStack(reason string) types.StackDetection {
	return types.StackDetection{
		State:    types.PresenceUnknown,
		Evidence: []types.StackEvidence{},
		Reason:   reason,
	}
}

// applyStackDetection fills in the Containers and Virtualization tri-states
// from an install scan and the workloads info already holds. hypervisors are
// the stacks of every hypervisor process seen, including any whose guest
// could not be described. cgroupScanErr is the container scan's failure, if
// it had one, and vmNote the reason VMs were not observable, if they were not.
func applyStackDetection(
	info *types.VirtInfo,
	scan installScan,
	hypervisors []string,
	cgroupScanErr error,
	vmNote string,
) {
	containersReason := reasonContainersUnobservable
	if cgroupScanErr != nil {
		containersReason = "the cgroup tree could not be fully read (" + errorText(cgroupScanErr) +
			"), so running containers may have been missed"
	}
	info.ContainerRuntime = detectStack(
		scan.sections[sectionContainers],
		scan.containerized,
		runningEvidence(info.Runtimes),
		info.Capability.ContainersObservable,
		containersReason,
	)

	// A running guest's hypervisor label ("qemu/kvm") names the stack its
	// install evidence calls "qemu"; one name keeps the two on one row.
	stacks := make([]string, 0, len(hypervisors))
	for _, hv := range hypervisors {
		if stack, ok := runningHypervisorStacks[hv]; ok {
			hv = stack
		}
		stacks = append(stacks, hv)
	}

	vmsReason := reasonVMsUnobservable
	if vmNote != "" {
		vmsReason = vmNote
	}
	info.Hypervisor = detectStack(
		scan.sections[sectionHypervisor],
		scan.containerized,
		runningEvidence(stacks),
		info.Capability.VMsObservable,
		vmsReason,
	)
}
