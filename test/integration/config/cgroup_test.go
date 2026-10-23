//go:build integration

package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// cgroupRoot is the devcontainer's cgroup namespace root. Its processes live
// in a child cgroup, so the root can hold siblings of its own with the memory
// controller enabled.
const cgroupRoot = "/sys/fs/cgroup"

// memoryCgroup creates a cgroup beside the container's own with the given
// memory.high and memory.max, inside the container's delegated cgroup
// namespace, and removes it when the test ends. It fails the test when the
// devcontainer does not allow it, rather than running without it.
func memoryCgroup(t *testing.T, high, max string) string {
	t.Helper()
	controllers, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"))
	if err != nil || !strings.Contains(string(controllers), "memory") {
		t.Fatalf("the devcontainer's cgroup root does not delegate the memory controller (%q, %v)", controllers, err)
	}
	dir := filepath.Join(cgroupRoot, "sysmon-it-"+strconv.Itoa(os.Getpid())+"-"+filepath.Base(t.TempDir()))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create a cgroup: %v", err)
	}
	// Registered before the process starts, so it runs after the process has
	// been stopped and the cgroup is empty again.
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Errorf("remove the cgroup %s: %v", dir, err)
		}
	})
	for name, value := range map[string]string{"memory.max": max, "memory.high": high} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	return dir
}

// serveInCgroup starts `sysmon serve` as a member of cgroup dir from its
// first instruction: a shell joins the cgroup and then becomes the service.
func serveInCgroup(t *testing.T, dir string) *harness.Server {
	t.Helper()
	script := `echo $$ > "$1/cgroup.procs" && shift && exec "$@"`
	s := harness.Start(t, "/bin/sh", nil, "-c", script, "sh", dir,
		harness.Bins.Sysmon, "serve", "--addr", "127.0.0.1:0", "--interval", "500", "--log-format", "json",
		"--config", filepath.Join(t.TempDir(), "absent.yaml"))
	s.Logs.AwaitMsg(t, harness.ListeningMsg)
	return s
}

// TestMemoryLimit_DerivedFromTheWatermark runs the service in a cgroup with a
// 512 MiB watermark under a 768 MiB hard limit, the shipped pair: the Go limit
// is 90% of the watermark, which is what it throttles at.
func TestMemoryLimit_DerivedFromTheWatermark(t *testing.T) {
	s := serveInCgroup(t, memoryCgroup(t, "536870912", "805306368"))

	r := s.Logs.AwaitMsg(t, "memory limit: derived from the cgroup")
	if src, _ := r["source"].(string); src != "memory.high" {
		t.Errorf("source = %q, want memory.high", src)
	}
	if got, _ := r["gomemlimit_bytes"].(float64); int64(got) != 536870912/10*9 {
		t.Errorf("gomemlimit_bytes = %v, want 90%% of the watermark", r["gomemlimit_bytes"])
	}
}

// TestMemoryLimit_AnUnreachableWatermarkIsReported runs the service in a
// cgroup whose watermark is above its hard limit: the kernel kills it before
// ever throttling it, so it says so, and derives from the hard limit instead.
func TestMemoryLimit_AnUnreachableWatermarkIsReported(t *testing.T) {
	s := serveInCgroup(t, memoryCgroup(t, "1073741824", "805306368"))

	if r := s.Logs.AwaitMsg(t, "memory limit: MemoryHigh is at or above MemoryMax, so the throttle "+
		"watermark is unreachable and the cgroup will be OOM-killed instead"); r.Level() != "WARN" {
		t.Errorf("an unreachable watermark is logged at %s, want WARN", r.Level())
	}
	r := s.Logs.AwaitMsg(t, "memory limit: derived from the cgroup")
	if src, _ := r["source"].(string); src != "memory.max" {
		t.Errorf("source = %q, want memory.max", src)
	}
}
