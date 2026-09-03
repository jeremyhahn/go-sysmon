// Package memlimit derives a Go memory limit from the cgroup ceiling the
// process is actually running under.
//
// The Go runtime knows nothing about cgroups: with no GOMEMLIMIT it grows the
// heap toward twice the live set and lets the kernel deal with the
// consequences. Under a cgroup that is not a crash but a stall -- crossing
// memory.high makes the kernel throttle allocation and reclaim, silently,
// while systemd still reports the unit active and nothing restarts it. That
// happened here: vm-host-5 crossed a 96 MiB ceiling 658 times without one
// line in the journal (issue #65).
//
// Deriving the limit rather than configuring it is deliberate. A GOMEMLIMIT
// written into the unit beside MemoryHigh is two numbers that must agree, and
// nothing fails when they stop agreeing.
package memlimit

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// headroomNumerator and headroomDenominator set the fraction of the cgroup
// ceiling handed to the Go runtime -- 90%.
//
// The remainder is not slack for its own sake. GOMEMLIMIT bounds memory the Go
// runtime manages; the cgroup also charges the binary's text, thread stacks and
// page cache, none of which the runtime can collect. Handing over the whole
// ceiling would let the kernel start throttling while the runtime believed it
// was still inside its budget, which is the failure this package exists to
// prevent.
const (
	headroomNumerator   = 9
	headroomDenominator = 10
)

// unlimited is what cgroup v2 writes when no ceiling is set.
const unlimited = "max"

// Limits describes the memory ceilings applying to this process and the Go
// limit derived from them.
//
// A zero High or Max means that ceiling is unset, which cgroup v2 spells "max"
// and which is a genuine absence rather than a limit of zero.
type Limits struct {
	// High is memory.high in bytes: the throttle-and-reclaim watermark.
	High int64 `json:"high_bytes"`
	// Max is memory.max in bytes: the hard limit, past which the cgroup is
	// OOM-killed.
	Max int64 `json:"max_bytes"`
	// Applied is the byte value handed to debug.SetMemoryLimit, or 0 when no
	// limit was derived.
	Applied int64 `json:"applied_bytes"`
	// Source names where Applied came from: "memory.high", "memory.max",
	// "GOMEMLIMIT" when the environment already set one, or "" when no ceiling
	// was found and the runtime default stands.
	Source string `json:"source"`
	// Incoherent reports a cgroup whose high watermark is at or above its hard
	// limit, which makes the watermark unreachable: the kernel enforces max
	// first, so the process is killed rather than throttled and memory.events
	// "high" stays at zero however much memory is used. A configuration in
	// this state reads as more generous than the one it replaced and its
	// counter agrees, which is why it has to be reported rather than inferred.
	Incoherent bool `json:"incoherent"`
}

// Ceiling returns the byte value Source names, or 0 when nothing was derived.
//
// It exists so a caller cannot report one number under another's name. The
// obvious shorthand -- the larger of High and Max -- prints 768M beside the
// label "memory.high" when the watermark is 512M, which is the labelling defect
// this codebase keeps finding in other people's output.
func (l Limits) Ceiling() int64 {
	switch l.Source {
	case "memory.high":
		return l.High
	case "memory.max":
		return l.Max
	default:
		return 0
	}
}

// Detect reads the cgroup v2 memory ceilings for this process.
//
// root is the filesystem root to read under, which is "" for the running
// system and a fixture directory in tests. getenv resolves environment
// variables; pass os.Getenv.
//
// A process outside cgroup v2, or in one with no ceilings, is not an error:
// the returned Limits has no Source and Apply does nothing.
func Detect(root string, getenv func(string) string) (Limits, error) {
	var l Limits

	// An explicit GOMEMLIMIT is an operator decision and outranks anything
	// derived. The runtime has already applied it by the time this runs, so
	// there is nothing to do but say so.
	if v := getenv("GOMEMLIMIT"); v != "" {
		l.Source = "GOMEMLIMIT"
		return l, nil
	}

	dir, err := cgroupDir(root)
	if err != nil {
		return l, err
	}
	if dir == "" {
		return l, nil
	}

	if l.High, err = readBytes(filepath.Join(dir, "memory.high")); err != nil {
		return l, err
	}
	if l.Max, err = readBytes(filepath.Join(dir, "memory.max")); err != nil {
		return l, err
	}

	l.Incoherent = l.High > 0 && l.Max > 0 && l.High >= l.Max

	// Prefer the watermark, because reaching it is meant to be survivable and
	// the point is to collect before the kernel intervenes. An incoherent pair
	// is the exception: there the watermark cannot be reached at all, so the
	// hard limit is the only real ceiling and deriving from the watermark would
	// hand the runtime a budget larger than the value it will be killed at.
	switch {
	case l.High > 0 && !l.Incoherent:
		l.Applied = l.High / headroomDenominator * headroomNumerator
		l.Source = "memory.high"
	case l.Max > 0:
		l.Applied = l.Max / headroomDenominator * headroomNumerator
		l.Source = "memory.max"
	}

	return l, nil
}

// Apply sets the Go runtime memory limit from l. It is a no-op when Detect
// found no ceiling or the environment already supplied one.
func Apply(l Limits) {
	if l.Applied <= 0 {
		return
	}
	debug.SetMemoryLimit(l.Applied)
}

// cgroupDir resolves the directory holding this process's cgroup v2 interface
// files, or "" when the process is not under cgroup v2.
func cgroupDir(root string) (string, error) {
	path := filepath.Join(root, "/proc/self/cgroup")
	raw, err := os.ReadFile(path)
	if err != nil {
		// No /proc/self/cgroup is not a broken host: it is a kernel without
		// cgroups, or a test fixture that does not model them.
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", &types.CgroupReadError{Path: path, Cause: err}
	}

	// The cgroup v2 line is the one with an empty controller list: "0::/path".
	// A v1-only host has none, and a hybrid host has both -- taking the v2 line
	// specifically is what keeps this from resolving a v1 controller path that
	// holds no memory.high at all.
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		return filepath.Join(root, "/sys/fs/cgroup", rest), nil
	}
	return "", nil
}

// readBytes reads one cgroup byte-valued file. The literal "max" means no
// limit and reads as 0; a missing file means the controller is not enabled
// here and reads the same way.
func readBytes(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, &types.CgroupReadError{Path: path, Cause: err}
	}

	text := strings.TrimSpace(string(raw))
	if text == unlimited {
		return 0, nil
	}

	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, &types.CgroupParseError{Path: path, Value: text}
	}
	if n < 0 {
		return 0, &types.CgroupParseError{Path: path, Value: text}
	}
	return n, nil
}
