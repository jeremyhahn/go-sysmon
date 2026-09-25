package memlimit_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/memlimit"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// noEnv is the environment of a process with nothing set, which is the case
// every derivation path depends on.
func noEnv(string) string { return "" }

// fakeRoot builds a filesystem fixture: /proc/self/cgroup naming a cgroup, and
// that cgroup's memory.high and memory.max holding the given text. An empty
// string means the file is absent, which is not the same as "max" and is one of
// the cases under test.
func fakeRoot(t *testing.T, cgroupLine, high, max string) string {
	t.Helper()

	root := t.TempDir()
	if cgroupLine != "" {
		procSelf := filepath.Join(root, "proc", "self")
		if err := os.MkdirAll(procSelf, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", procSelf, err)
		}
		if err := os.WriteFile(filepath.Join(procSelf, "cgroup"), []byte(cgroupLine), 0o644); err != nil {
			t.Fatalf("write cgroup: %v", err)
		}
	}

	dir := filepath.Join(root, "sys", "fs", "cgroup", "system.slice", "go-sysmon.service")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, text := range map[string]string{"memory.high": high, "memory.max": max} {
		if text == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

const v2Line = "0::/system.slice/go-sysmon.service\n"

func TestDetect(t *testing.T) {
	// 512 MiB high under a 768 MiB max is the shipped pair; the rest are the
	// states a host can actually be in.
	const (
		high512 = 536870912
		max768  = 805306368
	)

	tests := []struct {
		name       string
		cgroupLine string
		high, max  string
		want       memlimit.Limits
	}{
		{
			name:       "the shipped pair derives from the watermark",
			cgroupLine: v2Line,
			high:       "536870912",
			max:        "805306368",
			// 90% of the watermark, not of the hard limit: reaching the
			// watermark is survivable and is the point at which the runtime
			// should already have collected.
			want: memlimit.Limits{High: high512, Max: max768, Applied: high512 / 10 * 9, Source: "memory.high"},
		},
		{
			name:       "a watermark at or above the hard limit is unreachable",
			cgroupLine: v2Line,
			high:       "536870912",
			max:        "134217728", // the state the fleet was in on 2026-09-03
			// Deriving from the watermark here would hand the runtime 460 MiB
			// while the kernel kills at 128 MiB.
			want: memlimit.Limits{
				High: high512, Max: 134217728,
				Applied: 134217728 / 10 * 9, Source: "memory.max", Incoherent: true,
			},
		},
		{
			name:       "no watermark falls back to the hard limit",
			cgroupLine: v2Line,
			high:       "max",
			max:        "805306368",
			want:       memlimit.Limits{Max: max768, Applied: max768 / 10 * 9, Source: "memory.max"},
		},
		{
			name:       "an unlimited cgroup derives nothing",
			cgroupLine: v2Line,
			high:       "max",
			max:        "max",
			want:       memlimit.Limits{},
		},
		{
			name:       "absent files are absence, not zero",
			cgroupLine: v2Line,
			// A cgroup with the memory controller disabled has neither file.
			// Reading that as a limit of zero bytes would set GOMEMLIMIT to
			// zero and wedge the runtime in permanent GC.
			want: memlimit.Limits{},
		},
		{
			name: "no cgroup at all is not an error",
			// A kernel without cgroups, or a container runtime that hides
			// /proc/self/cgroup. The runtime default has to stand.
			want: memlimit.Limits{},
		},
		{
			name:       "a v1-only cgroup line is not treated as v2",
			cgroupLine: "11:memory:/system.slice/go-sysmon.service\n1:name=systemd:/system.slice/go-sysmon.service\n",
			high:       "536870912",
			max:        "805306368",
			// The files exist in the fixture, so a resolver that ignored the
			// controller field would find them and report a limit that does
			// not apply to this process.
			want: memlimit.Limits{},
		},
		{
			name:       "a hybrid host still resolves the v2 line",
			cgroupLine: "11:memory:/system.slice/other.service\n0::/system.slice/go-sysmon.service\n",
			high:       "536870912",
			max:        "805306368",
			want:       memlimit.Limits{High: high512, Max: max768, Applied: high512 / 10 * 9, Source: "memory.high"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fakeRoot(t, tt.cgroupLine, tt.high, tt.max)

			got, err := memlimit.Detect(root, noEnv)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got != tt.want {
				t.Errorf("Detect = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// An operator who sets GOMEMLIMIT has already been obeyed by the runtime before
// any of this code runs. Deriving a second value would silently overwrite their
// decision with ours.
func TestDetect_ExplicitGOMEMLIMITWins(t *testing.T) {
	root := fakeRoot(t, v2Line, "536870912", "805306368")

	got, err := memlimit.Detect(root, func(k string) string {
		if k == "GOMEMLIMIT" {
			return "42MiB"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Source != "GOMEMLIMIT" {
		t.Errorf("Source = %q, want GOMEMLIMIT", got.Source)
	}
	if got.Applied != 0 {
		t.Errorf("Applied = %d, want 0: the runtime has already applied the operator's value", got.Applied)
	}
}

func TestDetect_Errors(t *testing.T) {
	t.Run("a ceiling that is not a number is reported, not ignored", func(t *testing.T) {
		root := fakeRoot(t, v2Line, "not-a-number", "805306368")

		_, err := memlimit.Detect(root, noEnv)
		var parseErr *types.CgroupParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("Detect error = %v, want *types.CgroupParseError", err)
		}
		if parseErr.Value != "not-a-number" {
			t.Errorf("Value = %q, want the verbatim file contents", parseErr.Value)
		}
	})

	t.Run("a negative ceiling is rejected", func(t *testing.T) {
		// Nothing writes this today. It is rejected because the alternative is
		// handing debug.SetMemoryLimit a negative budget.
		root := fakeRoot(t, v2Line, "-1", "805306368")

		_, err := memlimit.Detect(root, noEnv)
		var parseErr *types.CgroupParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("Detect error = %v, want *types.CgroupParseError", err)
		}
	})

	t.Run("an unreadable ceiling is reported with its path", func(t *testing.T) {
		// Staged as a directory rather than by removing read permission,
		// because chmod does not stop root and the suite has to behave the
		// same for both. os.ReadFile on a directory fails with EISDIR, which
		// is the branch under test: a read error that is NOT "not exist" and
		// must therefore be reported rather than read as an absent ceiling.
		root := fakeRoot(t, v2Line, "", "805306368")
		path := filepath.Join(root, "sys", "fs", "cgroup", "system.slice", "go-sysmon.service", "memory.high")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("stage unreadable memory.high: %v", err)
		}

		_, err := memlimit.Detect(root, noEnv)
		var readErr *types.CgroupReadError
		if !errors.As(err, &readErr) {
			t.Fatalf("Detect error = %v, want *types.CgroupReadError", err)
		}
		if readErr.Path != path {
			t.Errorf("Path = %q, want %q", readErr.Path, path)
		}
	})
}

// Apply is separated from Detect so that reading the host and mutating the
// runtime are different decisions. These pin the no-op cases, because a limit
// applied from an empty detection would be a limit of zero.
func TestApply_NoLimitLeavesTheRuntimeAlone(t *testing.T) {
	tests := []struct {
		name string
		l    memlimit.Limits
	}{
		{"nothing detected", memlimit.Limits{}},
		{"operator supplied", memlimit.Limits{Source: "GOMEMLIMIT"}},
		{"negative", memlimit.Limits{Applied: -1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := currentMemoryLimit(t)
			memlimit.Apply(tt.l)
			if after := currentMemoryLimit(t); after != before {
				t.Errorf("memory limit changed to %d, want it left at %d", after, before)
			}
		})
	}
}

func TestApply_SetsTheDerivedLimit(t *testing.T) {
	const want = 460 * 1 << 20

	before := currentMemoryLimit(t)
	t.Cleanup(func() { restoreMemoryLimit(t, before) })

	memlimit.Apply(memlimit.Limits{Applied: want, Source: "memory.high"})

	if got := currentMemoryLimit(t); got != want {
		t.Errorf("memory limit = %d, want %d", got, want)
	}
}

// currentMemoryLimit reads the runtime's limit without changing it:
// SetMemoryLimit returns the value in force and a negative argument means
// "report, do not set".
func currentMemoryLimit(t *testing.T) int64 {
	t.Helper()
	return debug.SetMemoryLimit(-1)
}

// restoreMemoryLimit puts back a value read by currentMemoryLimit. The limit is
// process-wide, so a test that sets it and does not restore it changes the
// conditions of every test that runs after it in the same binary.
func restoreMemoryLimit(t *testing.T, v int64) {
	t.Helper()
	debug.SetMemoryLimit(v)
}

// Ceiling must name the value Source names. Reporting the larger of the two
// prints 768M under the label "memory.high" when the watermark is 512M -- and
// that shorthand was in the first version of the doctor check, caught by the
// test above rather than by review.
func TestLimits_Ceiling(t *testing.T) {
	tests := []struct {
		name string
		l    memlimit.Limits
		want int64
	}{
		{"the watermark, not the larger value", memlimit.Limits{High: 512, Max: 768, Source: "memory.high"}, 512},
		{"the hard limit when that is the source", memlimit.Limits{High: 512, Max: 768, Source: "memory.max"}, 768},
		{"nothing derived", memlimit.Limits{High: 512, Max: 768}, 0},
		{"supplied by the environment", memlimit.Limits{Source: "GOMEMLIMIT"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.l.Ceiling(); got != tt.want {
				t.Errorf("Ceiling() = %d, want %d", got, tt.want)
			}
		})
	}
}

// writeCeiling writes one memory.* file for the cgroup at rel under root.
func writeCeiling(t *testing.T, root, rel, name, text string) {
	t.Helper()
	dir := filepath.Join(root, "sys", "fs", "cgroup", rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestDetect_AnAncestorsCeilingBinds covers a process whose own cgroup is
// unlimited under a limited parent -- a container's processes in a child such
// as /init, a service in a slice with MemoryMax. The kernel enforces the
// parent's ceiling, so that is the one derived from; and where both set one,
// the tighter wins.
func TestDetect_AnAncestorsCeilingBinds(t *testing.T) {
	const gib = int64(1 << 30)
	tests := []struct {
		name     string
		files    map[string]map[string]string
		wantHigh int64
		wantMax  int64
		wantSrc  string
	}{
		{
			name: "only the parent is limited",
			files: map[string]map[string]string{
				"system.slice":                   {"memory.high": "max", "memory.max": "6442450944"},
				"system.slice/go-sysmon.service": {"memory.high": "max", "memory.max": "max"},
			},
			wantMax: 6 * gib, wantSrc: "memory.max",
		},
		{
			name: "the child is tighter than the parent",
			files: map[string]map[string]string{
				"system.slice":                   {"memory.max": "6442450944"},
				"system.slice/go-sysmon.service": {"memory.high": "536870912", "memory.max": "1073741824"},
			},
			wantHigh: gib / 2, wantMax: gib, wantSrc: "memory.high",
		},
		{
			name: "the parent's watermark is above the child's hard limit",
			files: map[string]map[string]string{
				"":                               {"memory.high": "2147483648"},
				"system.slice/go-sysmon.service": {"memory.max": "1073741824"},
			},
			wantHigh: 2 * gib, wantMax: gib, wantSrc: "memory.max",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fakeRoot(t, v2Line, "", "")
			for rel, files := range tt.files {
				for name, text := range files {
					writeCeiling(t, root, rel, name, text)
				}
			}

			l, err := memlimit.Detect(root, noEnv)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if l.High != tt.wantHigh || l.Max != tt.wantMax || l.Source != tt.wantSrc {
				t.Errorf("Detect = high %d max %d from %q, want high %d max %d from %q",
					l.High, l.Max, l.Source, tt.wantHigh, tt.wantMax, tt.wantSrc)
			}
		})
	}
}

// TestDetect_AnUnreadableAncestorIsReported verifies a ceiling that cannot be
// read anywhere on the path is an error, not an absent limit.
func TestDetect_AnUnreadableAncestorIsReported(t *testing.T) {
	root := fakeRoot(t, v2Line, "max", "max")
	writeCeiling(t, root, "system.slice", "memory.max", "lots")

	_, err := memlimit.Detect(root, noEnv)
	var parseErr *types.CgroupParseError
	if !errors.As(err, &parseErr) || parseErr.Value != "lots" {
		t.Fatalf("Detect error = %v, want a *types.CgroupParseError for the parent's ceiling", err)
	}
}
