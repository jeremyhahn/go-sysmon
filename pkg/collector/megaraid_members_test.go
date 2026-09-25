//go:build linux && (amd64 || arm64) && !nomegaraid

package collector

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withMegaRAIDNode points the controller sessions at path instead of the real
// management node.
func withMegaRAIDNode(t *testing.T, path string) {
	t.Helper()
	original := megaraidIoctlNode
	megaraidIoctlNode = path
	t.Cleanup(func() { megaraidIoctlNode = original })
}

// refusingNode returns an open regular file standing in for the management
// node: it opens, and the kernel refuses every controller command on it with
// ENOTTY, which is what a node that is not a MegaRAID controller answers.
func refusingNode(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "megaraid_sas_ioctl_node"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
			t.Errorf("close node: %v", err)
		}
	})
	return f
}

// megaraidVolumeDevice builds /sys/block/<name> as a volume on SCSI host
// "host0" at the given target ID, driven by megaraid_sas.
func megaraidVolumeDevice(t *testing.T, name, target string) {
	t.Helper()
	sysBlock, devices, hosts := t.TempDir(), t.TempDir(), t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)
	dir := filepath.Join(devices, "host0", "target0:2:"+target, "0:2:"+target+":0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysBlock, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(sysBlock, name, "device")); err != nil {
		t.Fatal(err)
	}
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), megaraidDriver+"\n")
}

func TestMegaraidIdentity_IsATAFollowsTheBridgeVendor(t *testing.T) {
	for vendor, want := range map[string]bool{"ATA": true, "ata": true, "SEAGATE": false, "": false} {
		if got := (megaraidIdentity{Vendor: vendor}).isATA(); got != want {
			t.Errorf("isATA(%q) = %v, want %v", vendor, got, want)
		}
	}
}

func TestSCSITargetID_ReadsTheTargetFromTheDevicePath(t *testing.T) {
	megaraidVolumeDevice(t, "sdb", "1")
	if id, ok := scsiTargetID("sdb"); !ok || id != 1 {
		t.Errorf("scsiTargetID(sdb) = %d, %v; want 1, true", id, ok)
	}

	if _, ok := scsiTargetID("absent"); ok {
		t.Error("scsiTargetID(absent) reported a target for a device with no device link")
	}

	// A device link that is not host:channel:id:lun, and one whose id is not
	// a byte, have no target to match a volume by.
	sysBlock := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	for name, leaf := range map[string]string{"nvme0n1": "nvme0", "sdx": "0:2:300:0"} {
		dir := filepath.Join(t.TempDir(), leaf)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sysBlock, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(sysBlock, name, "device")); err != nil {
			t.Fatal(err)
		}
		if id, ok := scsiTargetID(name); ok {
			t.Errorf("scsiTargetID(%s -> %s) = %d, want no target", name, leaf, id)
		}
	}
}

// TestMegaraidCycle_OneSessionPerControllerPerCycle verifies a controller is
// opened once in a cycle however many disks ask for it, a failure is sticky
// for the cycle, and ending the cycle closes what was opened.
func TestMegaraidCycle_OneSessionPerControllerPerCycle(t *testing.T) {
	node := filepath.Join(t.TempDir(), "megaraid_sas_ioctl_node")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withMegaRAIDNode(t, node)

	var m megaraidCycle
	m.begin()
	first := m.sessionFor(0)
	var ioctlErr *MegaRAIDIoctlError
	if !errors.As(first.err, &ioctlErr) || !errors.Is(first.err, syscall.ENOTTY) {
		t.Fatalf("session error = %v, want the CFG_READ refusal as a *MegaRAIDIoctlError", first.err)
	}
	if first.file == nil {
		t.Fatal("the node opened, so the session must hold it until the cycle ends")
	}
	if again := m.sessionFor(0); again != first {
		t.Error("a second disk on the same controller opened it again in one cycle")
	}
	if other := m.sessionFor(1); other == first {
		t.Error("two controllers shared one session")
	}

	m.end()
	if first.file != nil || m.sessions != nil {
		t.Error("end() left a session open")
	}
	var never *megaraidSession
	never.close() // a session that never opened must be safe to close
}

func TestMegaraidCycle_AbsentNodeIsTheSessionError(t *testing.T) {
	withMegaRAIDNode(t, filepath.Join(t.TempDir(), "absent"))

	var m megaraidCycle // sessionFor outside begin() starts the map itself
	s := m.sessionFor(0)
	if !errors.Is(s.err, fs.ErrNotExist) || s.file != nil {
		t.Errorf("session = %+v, want the absent node as its error and no file", s)
	}
}

// TestReadVolumeMembers_ADriveThatDoesNotAnswerIsStillAMember verifies every
// drive the controller has in the volume is reported, with the reason it said
// nothing, rather than omitted -- which would quietly shrink the array.
func TestReadVolumeMembers_ADriveThatDoesNotAnswerIsStillAMember(t *testing.T) {
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	node := refusingNode(t)
	ctrl := &megaraidController{hostNo: 0}

	for _, vol := range cfg.Volumes {
		members := ctrl.readVolumeMembers(node.Fd(), cfg, vol)
		if len(members) != len(cfg.MembersOf(vol)) {
			t.Fatalf("volume %q: %d members reported, want all %d", vol.Name, len(members), len(cfg.MembersOf(vol)))
		}
		for _, m := range members {
			if m.SMARTStatus != types.SMARTUnreadable || !strings.Contains(m.SMARTReason, "INQUIRY") {
				t.Errorf("member %d = %q (%s), want unreadable, naming the INQUIRY it did not answer",
					m.DeviceID, m.SMARTStatus, m.SMARTReason)
			}
		}
	}
}

func TestApplyHardwareRAID_ReportsTheVolumeItIs(t *testing.T) {
	megaraidVolumeDevice(t, "sdb", "1")
	cfg, err := parseControllerConfig(realConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	c := NewDiskCollector(slog.New(slog.DiscardHandler))
	c.megaraid.begin()
	c.megaraid.sessions[0] = &megaraidSession{file: refusingNode(t), config: cfg, ctrl: &megaraidController{hostNo: 0}}

	var info types.DiskInfo
	c.applyHardwareRAID("sdb", &info)

	if info.RAID == nil || info.RAID.Level != "raid10" {
		t.Fatalf("RAID = %+v, want the raid10 volume at target 1", info.RAID)
	}
	if len(info.Members) != 4 {
		t.Errorf("Members = %d, want the volume's 4 drives", len(info.Members))
	}
	if info.SMARTStatus != types.SMARTBehindController || !strings.Contains(info.SMARTReason, "4 member drives") {
		t.Errorf("SMART = %q (%s), want behind_controller naming its 4 members", info.SMARTStatus, info.SMARTReason)
	}

	// A target the controller has no volume for is left alone.
	megaraidVolumeDevice(t, "sdc", "5")
	var other types.DiskInfo
	c.applyHardwareRAID("sdc", &other)
	if other.RAID != nil || other.Members != nil {
		t.Errorf("a device matching no volume got %+v", other)
	}
}

func TestApplyHardwareRAID_LeavesWhatIsNotAMegaRAIDVolume(t *testing.T) {
	c := NewDiskCollector(slog.New(slog.DiscardHandler))
	withMegaRAIDNode(t, filepath.Join(t.TempDir(), "absent"))

	// An md array is an md array, whatever is under it.
	megaraidVolumeDevice(t, "sdb", "0")
	md := &types.RAIDInfo{Level: "raid1"}
	info := types.DiskInfo{RAID: md}
	c.applyHardwareRAID("sdb", &info)
	if info.RAID != md || info.Members != nil {
		t.Errorf("an md array was rewritten: %+v", info)
	}

	// A disk on another driver, and a device with no hardware behind it.
	sysBlock, devices, hosts := t.TempDir(), t.TempDir(), t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)
	fakeBlockDevice(t, sysBlock, devices, "sdd", 100, "host3")
	writeSysfsFile(t, filepath.Join(hosts, "host3", "proc_name"), "ahci\n")
	for _, name := range []string{"sdd", "dm-0"} {
		var plain types.DiskInfo
		c.applyHardwareRAID(name, &plain)
		if plain.RAID != nil || plain.Members != nil || plain.SMARTStatus != "" {
			t.Errorf("%s: not a MegaRAID volume, got %+v", name, plain)
		}
	}
}

// TestApplyHardwareRAID_UnavailableControllerWarnsOnce verifies a controller
// that cannot be opened costs one warning per run, not one per disk per cycle,
// and leaves the disk as it was.
func TestApplyHardwareRAID_UnavailableControllerWarnsOnce(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)
	megaraidVolumeDevice(t, "sdb", "0")
	withMegaRAIDNode(t, filepath.Join(t.TempDir(), "absent"))
	var logs bytes.Buffer
	c := NewDiskCollector(slog.New(slog.NewTextHandler(&logs, nil)))

	for cycle := 0; cycle < 2; cycle++ {
		c.megaraid.begin()
		var info types.DiskInfo
		c.applyHardwareRAID("sdb", &info)
		c.megaraid.end()
		if info.RAID != nil || info.Members != nil {
			t.Errorf("cycle %d: an unreadable controller produced %+v", cycle, info)
		}
	}
	if n := strings.Count(logs.String(), "RAID controller members unavailable"); n != 1 {
		t.Errorf("warned %d times over two cycles, want once:\n%s", n, logs.String())
	}
}
