package collector

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withDevPrivate replaces the namespace probe. It is a package-level func value
// precisely so this is possible: the real one is a sync.OnceValue and caches,
// which is right in production and useless in a test.
func withDevPrivate(t *testing.T, private bool) {
	t.Helper()
	orig := devIsPrivateTmpfs
	devIsPrivateTmpfs = func() bool { return private }
	t.Cleanup(func() { devIsPrivateTmpfs = orig })
}

// TestSmartOpenError_HiddenNodeIsDiagnosed covers the condition this state
// exists for: the unit sandboxes /dev away, so the node cannot be opened and no
// privilege change would help. Before this, the absence produced no remedy at
// all and doctor printed nothing.
func TestSmartOpenError_HiddenNodeIsDiagnosed(t *testing.T) {
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, "sda"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withSysBlockRoot(t, sysBlock)
	withDevPrivate(t, true)

	enoent := &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.ENOENT}
	got := smartOpenError("/dev/sda", enoent)

	var hidden *SMARTDeviceHiddenError
	if !errors.As(got, &hidden) {
		t.Fatalf("smartOpenError = %#v, want *SMARTDeviceHiddenError", got)
	}
	if hidden.DevPath != "/dev/sda" {
		t.Errorf("DevPath = %q, want /dev/sda", hidden.DevPath)
	}
	if r := classifyRefusal(got); r != hiddenByNamespace {
		t.Errorf("classifyRefusal = %v, want hiddenByNamespace", r)
	}

	// The whole point is the remedy, and what makes it right is the distinction
	// it draws: a hidden node is fixed by changing how the process is RUN, never
	// by changing who it runs AS. Asserted on that substance, and on both
	// concrete routes, rather than on one phrase -- a private /dev comes from a
	// systemd sandbox and from a container alike, and naming only one sends half
	// the readers to the wrong file.
	reason := hiddenByNamespace.reason()
	for _, want := range []string{"how this process is run", "PrivateDevices", "container"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason does not mention %q: %q", want, reason)
		}
	}
	// The remedies that cannot reach a node which is not in the namespace.
	for _, wrong := range []string{"add the service user", "'disk' group", "run as root"} {
		if strings.Contains(reason, wrong) {
			t.Errorf("reason offers %q, which cannot reach a node that is not in the namespace: %q", wrong, reason)
		}
	}
}

// TestSmartOpenError_AbsenceThatIsNotHidden covers the two ways ENOENT is
// ordinary. Neither may be dressed up as a sandbox, or a hot-unplugged disk
// would send someone editing unit files.
func TestSmartOpenError_AbsenceThatIsNotHidden(t *testing.T) {
	enoent := &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.ENOENT}

	t.Run("device gone from sysfs too", func(t *testing.T) {
		withSysBlockRoot(t, t.TempDir()) // no sda
		withDevPrivate(t, true)
		if got := smartOpenError("/dev/sda", enoent); got != error(enoent) {
			t.Errorf("smartOpenError = %#v, want the original error unchanged", got)
		}
		if r := classifyRefusal(smartOpenError("/dev/sda", enoent)); r != notRefused {
			t.Errorf("classifyRefusal = %v, want notRefused", r)
		}
	})

	t.Run("host dev, not a sandbox", func(t *testing.T) {
		sysBlock := t.TempDir()
		if err := os.MkdirAll(filepath.Join(sysBlock, "sda"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		withSysBlockRoot(t, sysBlock)
		withDevPrivate(t, false)
		if got := smartOpenError("/dev/sda", enoent); got != error(enoent) {
			t.Errorf("smartOpenError = %#v, want the original error unchanged", got)
		}
	})
}

// TestSmartOpenError_PermissionStillWins guards the existing classification: a
// denial is still a denial, and its remedy is still group membership. The new
// state must not have swallowed it.
func TestSmartOpenError_PermissionStillWins(t *testing.T) {
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, "sda"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withSysBlockRoot(t, sysBlock)
	withDevPrivate(t, true)

	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EPERM} {
		err := smartOpenError("/dev/sda", &os.PathError{Op: "open", Path: "/dev/sda", Err: errno})
		if r := classifyRefusal(err); r != refusedAtOpen {
			t.Errorf("%v classified as %v, want refusedAtOpen", errno, r)
		}
	}
}

// TestDevIsPrivateTmpfs_ReadsMountinfo covers the probe against real mountinfo
// text, including the two shapes that matter: the host's devtmpfs and a
// sandbox's tmpfs.
func TestDevIsPrivateTmpfs_ReadsMountinfo(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			// What a host reports.
			name: "host devtmpfs",
			line: "24 30 0:6 / /dev rw,nosuid,relatime shared:2 - devtmpfs udev rw,size=8192k,nr_inodes=4194304,mode=755",
			want: false,
		},
		{
			// What systemd's PrivateDevices=yes reports.
			name: "sandbox tmpfs",
			line: "24 30 0:24 / /dev rw,nosuid,relatime shared:2 - tmpfs tmpfs rw,size=65536k,mode=755",
			want: true,
		},
		{
			// Optional fields before the "-" vary in number, which is why the
			// separator is searched for rather than counted to.
			name: "extra optional fields",
			line: "24 30 0:24 / /dev rw,nosuid shared:2 master:3 propagate_from:1 - tmpfs tmpfs rw,mode=755",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc := t.TempDir()
			writeSysfs(t, filepath.Join(proc, "self", "mountinfo"),
				"23 28 0:22 / /proc rw,relatime - proc proc rw\n"+tc.line+
					"\n25 24 0:25 / /dev/shm rw - tmpfs tmpfs rw")
			withProcRoot(t, proc)

			// Call the parser directly rather than the cached wrapper.
			orig := devIsPrivateTmpfs
			t.Cleanup(func() { devIsPrivateTmpfs = orig })
			if got := readDevIsPrivateTmpfs(); got != tc.want {
				t.Errorf("readDevIsPrivateTmpfs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDevIsPrivateTmpfs_NoMountinfo verifies an unreadable mountinfo answers
// "not a sandbox" rather than guessing. Claiming a sandbox we cannot see would
// send an operator to a unit file over a disk that simply went away.
func TestDevIsPrivateTmpfs_NoMountinfo(t *testing.T) {
	withProcRoot(t, filepath.Join(t.TempDir(), "absent"))
	if readDevIsPrivateTmpfs() {
		t.Error("readDevIsPrivateTmpfs() = true with no mountinfo, want false")
	}
}

// TestDevIsPrivateTmpfs_ReadStoppedEarly verifies that a mountinfo read which
// fails partway answers "not a sandbox" even after it has seen a tmpfs /dev: the
// lines it never reached could shadow that entry, so the read settled nothing.
// A line longer than the scanner's buffer is what stops it.
func TestDevIsPrivateTmpfs_ReadStoppedEarly(t *testing.T) {
	proc := t.TempDir()
	writeSysfs(t, filepath.Join(proc, "self", "mountinfo"),
		"24 28 0:23 / /dev rw,nosuid - tmpfs tmpfs rw\n"+
			"25 24 0:25 / /mnt rw - ext4 /dev/sda1 rw,"+strings.Repeat("x", bufio.MaxScanTokenSize)+"\n")
	withProcRoot(t, proc)

	if readDevIsPrivateTmpfs() {
		t.Error("readDevIsPrivateTmpfs() = true from a read that stopped early, want false")
	}
}
