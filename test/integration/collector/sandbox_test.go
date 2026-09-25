//go:build integration

package collector

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// cgroupRoot is the container's own cgroup v2 tree.
const cgroupRoot = "/sys/fs/cgroup"

// requireDevcontainer fails the test unless it runs in the devcontainer with
// cgroup controllers delegated (build/docker/devcontainer/entrypoint.sh).
// Every test here changes container-wide state -- cgroups, mounts, processes
// named like hypervisors -- which must never happen on a host.
func requireDevcontainer(t *testing.T) {
	t.Helper()
	if os.Getenv("SYSMON_DEVCONTAINER") != "1" {
		t.Fatal("this suite changes container-wide state and runs only in the devcontainer (make integration-test); SYSMON_DEVCONTAINER is not set")
	}
	control, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("read the cgroup root: %v", err)
	}
	if !strings.Contains(string(control), "memory") {
		t.Fatalf("cgroup controllers are not delegated in this container (subtree_control %q); the devcontainer's entrypoint does that", control)
	}
}

// ── A private view of the filesystem ────────────────────────────────────────

// sandbox runs a binary in a mount namespace of its own, and optionally a
// network namespace or a root filesystem of its own, prepared by shell
// commands before the binary starts. What it changes is seen by that process
// alone: the rest of the container, and the other suites running in it, see
// nothing.
type sandbox struct {
	// net gives the process a network namespace of its own, with sysfs and
	// the cgroup tree remounted so they describe it.
	net bool
	// root gives the process a root filesystem of its own: an overlay of the
	// container's, with /proc, /sys, /dev and /tmp shared. setup may then
	// create, change or delete any file, as on a host installed differently.
	// The container's /.dockerenv is removed from it, so sysmon sees a host
	// rather than a container.
	root bool
	// setup are shell commands run, as root, inside the namespaces -- and the
	// root, when there is one -- before the binary is executed. They run
	// under `set -eu`.
	setup []string
}

// rootPreamble builds the overlay root in $ROOT.
var rootPreamble = []string{
	"mount -t tmpfs sandbox \"$ROOT\"",
	"mkdir \"$ROOT/upper\" \"$ROOT/work\" \"$ROOT/root\"",
	"mount -t overlay overlay -o lowerdir=/,upperdir=\"$ROOT/upper\",workdir=\"$ROOT/work\" \"$ROOT/root\"",
	"for m in proc sys dev tmp; do mount --rbind \"/$m\" \"$ROOT/root/$m\"; done",
	"rm -f \"$ROOT/root/.dockerenv\"",
}

// wrap returns the path of an executable that runs bin inside the sandbox
// with whatever arguments it is given. It can stand in for bin anywhere the
// harness takes a binary.
func (s sandbox) wrap(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, lines []string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write the sandbox %s: %v", name, err)
		}
		return path
	}

	setup := []string{"set -eu"}
	if s.net {
		setup = append(setup,
			"mount -t sysfs sysfs /sys",
			"mount -t cgroup2 cgroup2 "+cgroupRoot,
			"ip link set lo up")
	}
	setup = append(setup, s.setup...)
	setup = append(setup, `exec "$@"`)
	entry := write("setup.sh", setup)
	if s.root {
		// The setup runs inside the new root; /tmp, where it and bin live, is
		// shared into it.
		// /mnt is empty in the image; the tmpfs mounted on it is this
		// namespace's alone.
		preamble := append([]string{"set -eu", "ROOT=/mnt"}, rootPreamble...)
		preamble = append(preamble, `exec chroot "$ROOT/root" /bin/sh "$@"`)
		entry = write("root.sh", preamble) + " " + entry
	}

	flags := "--mount --propagation private"
	if s.net {
		flags += " --net"
	}
	wrapper := filepath.Join(dir, "sandboxed-"+filepath.Base(bin))
	body := "#!/bin/sh\nexec unshare " + flags + " -- /bin/sh " + entry + " " + bin + ` "$@"` + "\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatalf("write the sandbox wrapper: %v", err)
	}
	return wrapper
}

// withRuntimeSocket is a setup that shows socket to the process as
// /run/docker.sock, on a /run of its own.
func withRuntimeSocket(socket string) []string {
	return []string{
		"mount -t tmpfs tmpfs /run",
		": >/run/docker.sock",
		"mount --bind " + socket + " /run/docker.sock",
	}
}

// ── Cgroups ─────────────────────────────────────────────────────────────────

// cgroup is a cgroup the test created. It is removed when the test ends,
// after every process the test put in it has exited.
type cgroup struct {
	path string
}

// newCgroup creates rel under the container's cgroup tree, enabling the
// memory, io, cpu and pids controllers down to it, the way a runtime's own
// cgroup is set up.
func newCgroup(t *testing.T, rel string) cgroup {
	t.Helper()
	requireDevcontainer(t)
	parts := strings.Split(rel, "/")
	dir := cgroupRoot
	for i, part := range parts {
		dir = filepath.Join(dir, part)
		created := false
		if err := os.Mkdir(dir, 0o755); err == nil {
			created = true
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatalf("create cgroup %s: %v", dir, err)
		}
		if created {
			path := dir
			t.Cleanup(func() {
				// Registered before the processes that go into it are
				// started, so it runs after they are stopped.
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("remove cgroup %s: %v", path, err)
				}
			})
		}
		if i < len(parts)-1 {
			enableControllers(t, dir)
		}
	}
	return cgroup{path: dir}
}

// enableControllers turns on, in dir's children, every controller a
// container's accounting needs that dir itself has.
func enableControllers(t *testing.T, dir string) {
	t.Helper()
	available, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		t.Fatalf("read %s controllers: %v", dir, err)
	}
	for _, c := range strings.Fields(string(available)) {
		switch c {
		case "cpu", "io", "memory", "pids":
			if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+"+c), 0); err != nil {
				t.Fatalf("enable %s under %s: %v", c, dir, err)
			}
		}
	}
}

// set writes value to one of the cgroup's control files.
func (c cgroup) set(t *testing.T, file, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.path, file), []byte(value), 0); err != nil {
		t.Fatalf("set %s in %s: %v", file, c.path, err)
	}
}

// ── Processes standing in for workloads ─────────────────────────────────────

// guestBinary builds testdata/guest under the name the test needs.
func guestBinary(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./test/integration/collector/testdata/guest")
	cmd.Dir = harness.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the guest stand-in: %v\n%s", err, output)
	}
	return out
}

// guest is a running stand-in workload.
type guest struct {
	cmd  *exec.Cmd
	done chan error
}

// pid is the process ID.
func (g *guest) pid() int { return g.cmd.Process.Pid }

// startGuest runs bin with args and waits until it reports ready. vcpus
// threads are named the way qemu names vCPU threads. With cg set the process
// starts in that cgroup, so everything it allocates is charged there, as it
// is for a runtime's own workloads. It is stopped with SIGTERM when the test
// ends.
func startGuest(t *testing.T, cg *cgroup, bin string, vcpus int, args ...string) *guest {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if cg != nil {
		// The shell joins the cgroup and then becomes the guest, so the guest
		// is in it from its first instruction.
		join := `echo $$ >"$0/cgroup.procs" && exec "$@"`
		cmd = exec.Command("/bin/sh", append([]string{"-c", join, cg.path, bin}, args...)...)
	}
	cmd.Env = append(os.Environ(), "GUEST_VCPUS="+strconv.Itoa(vcpus))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("capture the guest's output: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(bin), err)
	}
	g := &guest{cmd: cmd, done: make(chan error, 1)}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "ready"
		for scanner.Scan() {
		}
		g.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop %s: %v", filepath.Base(bin), err)
		}
		if err := await.Recv(t, "the guest stand-in to exit", g.done); err != nil {
			t.Errorf("%s on SIGTERM: %v", filepath.Base(bin), err)
		}
	})
	if !await.Recv(t, "the guest stand-in to report ready", ready) {
		t.Fatalf("%s did not report ready", filepath.Base(bin))
	}
	return g
}
