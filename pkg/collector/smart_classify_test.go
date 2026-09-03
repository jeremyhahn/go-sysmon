package collector

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	ghwblock "github.com/jaypipes/ghw/pkg/block"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withSCSIHostRoot points the classifier at a synthetic /sys/class/scsi_host.
func withSCSIHostRoot(t *testing.T, root string) {
	t.Helper()
	original := sysClassSCSIHost
	sysClassSCSIHost = root
	t.Cleanup(func() { sysClassSCSIHost = original })
}

// fakeBlockDevice builds one entry under a synthetic /sys/block.
//
// sizeSectors < 0 omits the size file entirely, which is how a host without
// sysfs looks; backing names the SCSI host the device hangs off, or is empty
// for a virtual device with no "device" link at all.
func fakeBlockDevice(t *testing.T, sysBlock, devicesRoot, name string, sizeSectors int64, backingHost string) {
	t.Helper()
	dir := filepath.Join(sysBlock, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if sizeSectors >= 0 {
		writeSysfsFile(t, filepath.Join(dir, "size"), strconv.FormatInt(sizeSectors, 10)+"\n")
	}
	if backingHost == "" {
		return
	}
	// Mirror the real shape: /sys/block/<dev>/device is a symlink into the
	// device tree, and the SCSI host is a component of the resolved path.
	target := filepath.Join(devicesRoot, backingHost, "target0:2:0", "0:2:0:0")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", target, err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "device")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// ---- hasNoMedium ----------------------------------------------------------

func TestHasNoMedium_DistinguishesEmptyFromAbsentSysfs(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	withSysBlockRoot(t, sysBlock)

	fakeBlockDevice(t, sysBlock, devices, "nbd0", 0, "")             // stub, no medium
	fakeBlockDevice(t, sysBlock, devices, "sda", 498860032, "host0") // a real disk
	fakeBlockDevice(t, sysBlock, devices, "nosize", -1, "host0")     // sysfs cannot say

	tests := []struct {
		device string
		want   bool
		why    string
	}{
		{"nbd0", true, "size 0 means the kernel put no medium behind the name"},
		{"sda", false, "a sized device has a medium"},
		{"nosize", false, "an unreadable size must not read as empty"},
		{"absent", false, "a device sysfs has never heard of must not read as empty"},
	}
	for _, tc := range tests {
		if got := hasNoMedium(tc.device); got != tc.want {
			t.Errorf("hasNoMedium(%q) = %v, want %v -- %s", tc.device, got, tc.want, tc.why)
		}
	}
}

// ---- isVirtualBlockDevice -------------------------------------------------

func TestIsVirtualBlockDevice_StructuralTest(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	withSysBlockRoot(t, sysBlock)

	fakeBlockDevice(t, sysBlock, devices, "dm-0", 209715200, "")     // no device link
	fakeBlockDevice(t, sysBlock, devices, "sda", 498860032, "host0") // hardware behind it

	tests := []struct {
		device                 string
		wantVirtual, wantKnown bool
	}{
		{"dm-0", true, true},
		{"sda", false, true},
		{"absent", false, false},
	}
	for _, tc := range tests {
		virtual, known := isVirtualBlockDevice(tc.device)
		if virtual != tc.wantVirtual || known != tc.wantKnown {
			t.Errorf("isVirtualBlockDevice(%q) = (%v, %v), want (%v, %v)",
				tc.device, virtual, known, tc.wantVirtual, tc.wantKnown)
		}
	}
}

// ---- scsiHostDriver / raidControllerFor -----------------------------------

func TestRAIDControllerFor_ReadsTheDriverBehindTheDevice(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)

	fakeBlockDevice(t, sysBlock, devices, "sda", 498860032, "host0") // PERC volume
	fakeBlockDevice(t, sysBlock, devices, "sdc", 936640512, "host15")
	fakeBlockDevice(t, sysBlock, devices, "dm-0", 209715200, "") // no device link
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), "megaraid_sas\n")
	writeSysfsFile(t, filepath.Join(hosts, "host15", "proc_name"), "ahci\n")

	if got := scsiHostDriver("sda"); got != "megaraid_sas" {
		t.Errorf("scsiHostDriver(sda) = %q, want megaraid_sas", got)
	}
	if got := raidControllerFor("sda"); got == "" {
		t.Error("raidControllerFor(sda) = \"\"; a megaraid_sas volume must be recognised as behind a controller")
	}
	// An AHCI disk is a disk. Naming it a RAID volume would send the reader
	// looking for member drives that do not exist.
	if got := raidControllerFor("sdc"); got != "" {
		t.Errorf("raidControllerFor(sdc) = %q, want \"\" for an ahci device", got)
	}
	if got := raidControllerFor("dm-0"); got != "" {
		t.Errorf("raidControllerFor(dm-0) = %q, want \"\" when there is no device link", got)
	}
}

// ---- classifyRefusal ------------------------------------------------------

func TestClassifyRefusal_TellsOpenFromIoctl(t *testing.T) {
	// The shape a withheld CAP_SYS_RAWIO actually arrives in: the node opened,
	// both ATA transports were rejected by the kernel.
	refusedIoctl := &ErrSMARTUnavailable{
		DevPath: "/dev/sdc",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sdc", Op: "HDIO_DRIVE_CMD(0xD0)", Errno: syscall.EPERM},
		SGIOErr: &ErrSMARTIoctl{DevPath: "/dev/sdc", Op: "SG_IO", Errno: syscall.EPERM},
	}
	if got := classifyRefusal(refusedIoctl); got != refusedAtIoctl {
		t.Errorf("classifyRefusal(EPERM under ErrSMARTUnavailable) = %v, want refusedAtIoctl", got)
	}
	if got := classifyRefusal(&ErrSMARTPermission{DevPath: "/dev/sda"}); got != refusedAtOpen {
		t.Errorf("classifyRefusal(open refused) = %v, want refusedAtOpen", got)
	}

	// A drive that answered and raised a check condition is not a refusal.
	// Calling it one would tell the operator to grant a capability that would
	// change nothing.
	declined := &ErrSMARTUnavailable{
		DevPath: "/dev/sdc",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sdc", Op: "HDIO", Errno: syscall.ENOTTY},
		SGIOErr: &ErrSMARTPassthrough{DevPath: "/dev/sdc", Op: "SG_IO", Status: 0x02},
	}
	if got := classifyRefusal(declined); got != notRefused {
		t.Errorf("classifyRefusal(check condition) = %v, want notRefused", got)
	}
	if got := classifyRefusal(errors.New("something else")); got != notRefused {
		t.Errorf("classifyRefusal(plain error) = %v, want notRefused", got)
	}
}

// TestRefusalReason_NamesTheRemedyThatApplies is the point of splitting the
// two. On ci-node-1 the node opens -- the service is in the 'disk' group --
// and the ioctl is refused, so advice about group membership is not merely
// unhelpful there, it is wrong.
func TestRefusalReason_NamesTheRemedyThatApplies(t *testing.T) {
	openReason := refusedAtOpen.reason()
	ioctlReason := refusedAtIoctl.reason()

	if !strings.Contains(openReason, "'disk' group") || strings.Contains(openReason, "CAP_SYS_RAWIO") {
		t.Errorf("open-time reason = %q; it must point at group membership, not at a capability", openReason)
	}
	if !strings.Contains(ioctlReason, "CAP_SYS_RAWIO") {
		t.Errorf("ioctl-time reason = %q; it must name the capability", ioctlReason)
	}
	if !strings.Contains(ioctlReason, "group membership does not grant it") {
		t.Errorf("ioctl-time reason = %q; it must say group membership will not fix this", ioctlReason)
	}
	if notRefused.reason() != "" {
		t.Error("notRefused carries a remedy; there is nothing to remedy")
	}
}

// ---- classifySMARTFailure -------------------------------------------------

func TestClassifySMARTFailure_PrefersTheAnswerThatSaysWhereToLook(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)
	fakeBlockDevice(t, sysBlock, devices, "sda", 498860032, "host0")
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), "megaraid_sas\n")

	// ci-node-1's exact situation: a PERC volume on a host that withholds
	// CAP_SYS_RAWIO. Both descriptions are true, and the refusal wins -- it is
	// the one that is fixable, and the controller test cannot tell a volume
	// from a passthrough disk on the same controller, so claiming "the drives
	// are behind the controller" would be a claim about drives that may not
	// exist.
	refused := &ErrSMARTUnavailable{
		DevPath: "/dev/sda",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "HDIO", Errno: syscall.EPERM},
		SGIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "SG_IO", Errno: syscall.EPERM},
	}
	var info types.DiskInfo
	classifySMARTFailure("sda", refused, &info)
	if info.SMARTStatus != types.SMARTUnreadable {
		t.Errorf("classifySMARTFailure(PERC volume, EPERM) status = %q, want %q",
			info.SMARTStatus, types.SMARTUnreadable)
	}
	// The controller fact is not lost, because on a volume the capability alone
	// will not produce a reading and the operator should know before granting.
	if !strings.Contains(info.SMARTReason, "MegaRAID") {
		t.Errorf("reason = %q; the controller must still be named", info.SMARTReason)
	}
	if !strings.Contains(info.SMARTReason, "CAP_SYS_RAWIO") {
		t.Errorf("reason = %q; the refusal must still name its remedy", info.SMARTReason)
	}

	// And with the capability present, a volume fails with a check condition
	// rather than EPERM -- measured on ci-node-1 as "unsupported scsi opcode"
	// -- so behind_controller is still reached wherever it is the whole truth.
	declined := &ErrSMARTUnavailable{
		DevPath: "/dev/sda",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "HDIO", Errno: syscall.ENOTTY},
		SGIOErr: &ErrSMARTPassthrough{DevPath: "/dev/sda", Op: "SG_IO", Status: 0x02},
	}
	var capable types.DiskInfo
	classifySMARTFailure("sda", declined, &capable)
	if capable.SMARTStatus != types.SMARTBehindController {
		t.Errorf("classifySMARTFailure(PERC volume, check condition) status = %q, want %q",
			capable.SMARTStatus, types.SMARTBehindController)
	}
}

// TestClassifySMARTFailure_AnsweringDeviceIsNotBlamedOnTheController covers the
// other half of the same false positive. A passthrough disk on a RAID
// controller that replies with a corrupt sector HAS answered; blaming the
// controller sends the reader looking for member drives instead of at the
// drive in front of them.
func TestClassifySMARTFailure_CompletedTransportIsNotBlamedOnTheController(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)
	fakeBlockDevice(t, sysBlock, devices, "sdd", 2343255552, "host0")
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), "megaraid_sas\n")

	answered := []error{
		&ErrSMARTChecksum{DevPath: "/dev/sdd", Sum: 7},
		&ErrSMARTEmptySector{DevPath: "/dev/sdd"},
		&ErrSMARTShortTransfer{DevPath: "/dev/sdd", Op: "SMART READ DATA", Want: 512, Got: 128},
		&ErrSMARTNoAttributes{DevPath: "/dev/sdd"},
	}
	for _, err := range answered {
		original := probeATASMARTSupported
		probeATASMARTSupported = func(string) (bool, error) { return true, nil }

		var info types.DiskInfo
		classifySMARTFailure("sdd", err, &info)
		probeATASMARTSupported = original

		if info.SMARTStatus == types.SMARTBehindController {
			t.Errorf("%T was blamed on the controller; the transport completed and bytes came back", err)
		}
	}

	// A device that never answered at all is still the controller's opacity.
	original := probeATASMARTSupported
	probeATASMARTSupported = func(string) (bool, error) { return false, &ErrIdentifyIndeterminate{} }
	t.Cleanup(func() { probeATASMARTSupported = original })

	var silent types.DiskInfo
	classifySMARTFailure("sdd", &ErrSMARTPassthrough{DevPath: "/dev/sdd", Op: "SG_IO", Status: 0x02}, &silent)
	if silent.SMARTStatus != types.SMARTBehindController {
		t.Errorf("a check condition on a RAID controller = %q, want %q", silent.SMARTStatus, types.SMARTBehindController)
	}
	// And it must be phrased as the conditional it is. The SCSI host driver is
	// knowable; whether THIS device is a volume rather than a passthrough disk
	// is not, so the sentence may not assert it.
	if !strings.Contains(silent.SMARTReason, "If it is a volume") {
		t.Errorf("reason = %q; behind_controller may not assert that the device is a volume", silent.SMARTReason)
	}
}

// TestClassifySMARTFailure_JBODDiskIsNotCalledAVolume is the false positive the
// original ordering had. A megaraid_sas controller in JBOD mode exposes real
// disks; refused, they must report the refusal rather than a claim about
// member drives they do not have.
func TestClassifySMARTFailure_JBODDiskIsNotCalledAVolume(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)
	fakeBlockDevice(t, sysBlock, devices, "sdd", 2343255552, "host0")
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), "megaraid_sas\n")

	var info types.DiskInfo
	classifySMARTFailure("sdd", &ErrSMARTUnavailable{
		DevPath: "/dev/sdd",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sdd", Op: "HDIO", Errno: syscall.EPERM},
		SGIOErr: &ErrSMARTIoctl{DevPath: "/dev/sdd", Op: "SG_IO", Errno: syscall.EPERM},
	}, &info)

	if info.SMARTStatus == types.SMARTBehindController {
		t.Error("a refused disk on a RAID controller was reported as a volume, hiding a fixable permission fault")
	}
	if info.SMARTStatus != types.SMARTUnreadable {
		t.Errorf("status = %q, want %q", info.SMARTStatus, types.SMARTUnreadable)
	}
}

func TestClassifySMARTFailure_NonRAIDCases(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)
	fakeBlockDevice(t, sysBlock, devices, "sdc", 936640512, "host15")
	writeSysfsFile(t, filepath.Join(hosts, "host15", "proc_name"), "ahci\n")

	tests := []struct {
		name        string
		err         error
		supported   bool
		probeErr    error
		wantStatus  types.SMARTStatus
		wantNoProbe bool
	}{
		{
			name: "refused by the kernel",
			err:  &ErrSMARTPermission{DevPath: "/dev/sdc"},
			// A refusal is already the whole answer, and the probe would be
			// refused identically. Asking anyway costs an ioctl per device per
			// cycle for the life of the deployment.
			wantStatus:  types.SMARTUnreadable,
			wantNoProbe: true,
		},
		{
			name:       "the drive says it has no SMART",
			err:        &ErrSMARTPassthrough{DevPath: "/dev/sdc", Op: "SG_IO", Status: 0x02},
			supported:  false,
			wantStatus: types.SMARTUnsupported,
		},
		{
			name:       "the drive has SMART and the read still failed",
			err:        &ErrSMARTChecksum{DevPath: "/dev/sdc", Sum: 7},
			supported:  true,
			wantStatus: types.SMARTUnreadable,
		},
		{
			name:       "the probe itself could not answer",
			err:        &ErrSMARTChecksum{DevPath: "/dev/sdc", Sum: 7},
			probeErr:   &ErrIdentifyIndeterminate{},
			wantStatus: types.SMARTUnreadable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probed := false
			original := probeATASMARTSupported
			probeATASMARTSupported = func(string) (bool, error) {
				probed = true
				return tc.supported, tc.probeErr
			}
			t.Cleanup(func() { probeATASMARTSupported = original })

			var info types.DiskInfo
			classifySMARTFailure("sdc", tc.err, &info)
			if probed == tc.wantNoProbe {
				t.Errorf("IDENTIFY probe issued = %v, want %v", probed, !tc.wantNoProbe)
			}
			if info.SMARTStatus != tc.wantStatus {
				t.Errorf("status = %q, want %q (reason %q)", info.SMARTStatus, tc.wantStatus, info.SMARTReason)
			}
			if info.SMARTReason == "" {
				t.Error("every absence carries a reason; that is the point of the field")
			}
		})
	}
}

// ---- the disk list ---------------------------------------------------------

// TestCollectFromSysfs_DropsDevicesWithNoMedium reproduces ci-node-1's block
// layout as measured on 2026-09-02: sixteen nbd stubs the kernel creates at
// boot, a device-mapper node carrying the root filesystem, and the disks that
// actually exist. Sorted by name the stubs come first, which is why the host
// looked like it had no disk activity at all.
//
// dm-0 must survive: it is where / lives, and dropping every virtual device --
// the obvious reading of "skip dm-*" -- would take the root filesystem's usage
// with it.
func TestCollectFromSysfs_DropsDevicesWithNoMedium(t *testing.T) {
	root := t.TempDir()
	devices := t.TempDir()
	withSysBlockRoot(t, root)

	for i := 0; i < 16; i++ {
		fakeBlockDevice(t, root, devices, "nbd"+strconv.Itoa(i), 0, "")
	}
	fakeBlockDevice(t, root, devices, "dm-0", 209715200, "")
	fakeBlockDevice(t, root, devices, "sda", 498860032, "host0")
	fakeBlockDevice(t, root, devices, "nvme0n1", 3074457344, "host20")

	c := NewDiskCollector(quietLogger())
	got := map[string]bool{}
	for _, d := range c.collectFromSysfs(nil, nil) {
		got[d.Name] = true
	}

	for _, want := range []string{"dm-0", "sda", "nvme0n1"} {
		if !got[want] {
			t.Errorf("%s was dropped; only devices with no medium should be", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("collectFromSysfs returned %d devices, want 3: %v", len(got), got)
	}
}

// TestApplySMART_VirtualDeviceIsNotApplicable pins the difference between "no
// medium to ask" and "asked and got nothing". A device-mapper node is the
// first; anything with hardware under it is not, and must still be probed.
func TestApplySMART_VirtualDeviceIsNotApplicable(t *testing.T) {
	root := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, root)
	withSCSIHostRoot(t, hosts)
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	fakeBlockDevice(t, root, devices, "dm-0", 209715200, "")
	fakeBlockDevice(t, root, devices, "md0", 209715200, "")
	fakeBlockDevice(t, root, devices, "sda", 498860032, "host0")
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), "ahci\n")

	c := NewDiskCollector(quietLogger())

	for _, name := range []string{"dm-0", "md0"} {
		info := types.DiskInfo{Name: name}
		c.applySMART(name, &info)
		if info.SMARTStatus != types.SMARTNotApplicable {
			t.Errorf("applySMART(%s) status = %q, want %q", name, info.SMARTStatus, types.SMARTNotApplicable)
		}
	}

	// sda has hardware under it, so its absence of a reading is a different
	// absence -- the probe ran and failed, because /dev/sda is not this test's
	// to open. Calling that "not applicable" would excuse a real failure.
	info := types.DiskInfo{Name: "sda"}
	c.applySMART("sda", &info)
	if info.SMARTStatus == types.SMARTNotApplicable {
		t.Error("applySMART(sda) reported not_applicable for a device with hardware behind it")
	}
	if info.SMARTStatus == types.SMARTUnknown {
		t.Error("applySMART(sda) left the status unset; every device gets a classification")
	}
}

// TestRecordSMARTReading_FollowsTheBoolean keeps the status and the boolean it
// is derived from from ever disagreeing.
func TestRecordSMARTReading_FollowsTheBoolean(t *testing.T) {
	healthy := types.DiskInfo{SMARTHealthy: true}
	recordSMARTReading(&healthy)
	if healthy.SMARTStatus != types.SMARTOK {
		t.Errorf("healthy reading status = %q, want %q", healthy.SMARTStatus, types.SMARTOK)
	}

	failing := types.DiskInfo{SMARTHealthy: false}
	recordSMARTReading(&failing)
	if failing.SMARTStatus != types.SMARTFailing {
		t.Errorf("failing reading status = %q, want %q", failing.SMARTStatus, types.SMARTFailing)
	}
	if failing.SMARTStatus.HasReading() != true || types.SMARTNotApplicable.HasReading() {
		t.Error("HasReading must be true for a reading and false for an absence")
	}
}

// TestApplySMARTRead_RecordsEveryOutcome exercises the one place both
// transports converge, with the read injected so a successful reading can be
// tested at all -- the real path needs a drive, so before this the success
// branch was the only one no test could reach.
func TestApplySMARTRead_RecordsEveryOutcome(t *testing.T) {
	root := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, root)
	withSCSIHostRoot(t, hosts)
	fakeBlockDevice(t, root, devices, "sdc", 936640512, "host15")
	writeSysfsFile(t, filepath.Join(hosts, "host15", "proc_name"), "ahci\n")
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewDiskCollector(quietLogger())

	tests := []struct {
		name       string
		read       func(*types.DiskInfo) error
		wantStatus types.SMARTStatus
	}{
		{
			name: "a healthy reading",
			read: func(info *types.DiskInfo) error {
				info.SMARTEnabled, info.SMARTHealthy = true, true
				return nil
			},
			wantStatus: types.SMARTOK,
		},
		{
			name: "a failing drive",
			read: func(info *types.DiskInfo) error {
				info.SMARTEnabled, info.SMARTHealthy = true, false
				return nil
			},
			wantStatus: types.SMARTFailing,
		},
		{
			name:       "refused",
			read:       func(*types.DiskInfo) error { return &ErrSMARTPermission{DevPath: "/dev/sdc"} },
			wantStatus: types.SMARTUnreadable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := types.DiskInfo{Name: "sdc"}
			c.applySMARTRead("sdc", &info, func() error { return tc.read(&info) })
			if info.SMARTStatus != tc.wantStatus {
				t.Errorf("status = %q, want %q", info.SMARTStatus, tc.wantStatus)
			}
		})
	}
}

// TestDisksFromBlockInfo_DropsDevicesWithNoMedium covers the enumeration path
// that actually runs. ghw lists every name in /sys/block, so the sixteen nbd
// stubs arrive here exactly as they arrive on ci-node-1 -- and this, not the
// sysfs fallback, is where they were getting through.
func TestDisksFromBlockInfo_DropsDevicesWithNoMedium(t *testing.T) {
	root := t.TempDir()
	devices := t.TempDir()
	withSysBlockRoot(t, root)

	var enumerated []*ghwblock.Disk
	for i := 0; i < 16; i++ {
		name := "nbd" + strconv.Itoa(i)
		fakeBlockDevice(t, root, devices, name, 0, "")
		enumerated = append(enumerated, &ghwblock.Disk{Name: name})
	}
	fakeBlockDevice(t, root, devices, "dm-0", 209715200, "")
	fakeBlockDevice(t, root, devices, "sdb", 4683000000, "host0")
	enumerated = append(enumerated,
		&ghwblock.Disk{Name: "dm-0", SizeBytes: 107374182400},
		&ghwblock.Disk{Name: "sdb", SizeBytes: 2398096195584, Model: "PERC H740P Mini"})

	c := NewDiskCollector(quietLogger())
	got := c.disksFromBlockInfo(enumerated, nil, nil)

	if len(got) != 2 {
		names := make([]string, 0, len(got))
		for _, d := range got {
			names = append(names, d.Name)
		}
		t.Fatalf("disksFromBlockInfo returned %d disks, want 2: %v", len(got), names)
	}
	// Index numbers the disks that survive, not the ones ghw offered. Leaving
	// the enumeration index in would number two disks 16 and 17, and Index is
	// what the frontend keys its expansion state on.
	for i, d := range got {
		if d.Index != i {
			t.Errorf("%s has Index %d, want %d", d.Name, d.Index, i)
		}
	}
}

// TestSCSIHostDriver_SkipsNonHostComponents pins the digit check. A path
// component named "host" without a number is not a SCSI host, and treating it
// as one ends the search at a lookup that returns nothing -- so the real host
// further along the path is never reached and the device looks like it has no
// driver at all.
func TestSCSIHostDriver_SkipsNonHostComponents(t *testing.T) {
	sysBlock := t.TempDir()
	devices := t.TempDir()
	hosts := t.TempDir()
	withSysBlockRoot(t, sysBlock)
	withSCSIHostRoot(t, hosts)

	dir := filepath.Join(sysBlock, "sda")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A decoy component that starts with "host" but names no SCSI host, ahead
	// of the real one.
	target := filepath.Join(devices, "hostbridge", "host0", "target0:2:0", "0:2:0:0")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "device")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	writeSysfsFile(t, filepath.Join(hosts, "host0", "proc_name"), "megaraid_sas\n")

	if got := scsiHostDriver("sda"); got != "megaraid_sas" {
		t.Errorf("scsiHostDriver(sda) = %q, want megaraid_sas; the decoy component was taken for a SCSI host", got)
	}
}

// TestSMARTOpenError_WrapsBothDenials covers the denial shape a device cgroup
// or an LSM produces. Both open sites wrapped EACCES only, so an EPERM from
// open arrived at the classifier as a raw *os.PathError and was reported as an
// ioctl-time refusal -- telling the operator the node opened when it had not,
// and naming a capability that cannot help.
func TestSMARTOpenError_WrapsBothDenials(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want refusal
	}{
		{"EACCES", &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.EACCES}, refusedAtOpen},
		// EPERM is the one worth pinning: Go maps it onto os.ErrPermission, so
		// the single check covers it, and it does not look like it does.
		{"EPERM", &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.EPERM}, refusedAtOpen},
		{"os.ErrPermission", os.ErrPermission, refusedAtOpen},
		// Not a refusal, and must not be dressed as one.
		{"ENOENT", &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.ENOENT}, notRefused},
		{"ENOTTY", &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.ENOTTY}, notRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRefusal(smartOpenError("/dev/sda", tc.err))
			if got != tc.want {
				t.Errorf("classifyRefusal(smartOpenError(%v)) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	// An error that is not a denial comes back unchanged, so the caller's
	// debug log still names what actually went wrong.
	notFound := &os.PathError{Op: "open", Path: "/dev/sda", Err: syscall.ENOENT}
	if smartOpenError("/dev/sda", notFound) != error(notFound) {
		t.Error("a non-denial open error was replaced; the original cause must survive")
	}
}

// TestClassifyRefusal_SeesAnHDIORefusalAlone is the gap that opened when
// classification started asking errors.Is through ErrSMARTUnavailable. Its
// Unwrap returned only the SG_IO error while its doc said both were
// inspectable, so an HDIO refusal paired with any other SG_IO failure was
// invisible -- and a device we were refused would have been reported as one
// that declined to answer, sending the operator to the hardware.
func TestClassifyRefusal_SeesAnHDIORefusalAlone(t *testing.T) {
	hdioRefused := &ErrSMARTUnavailable{
		DevPath: "/dev/sda",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "HDIO_DRIVE_CMD(0xD0)", Errno: syscall.EPERM},
		SGIOErr: &ErrSMARTPassthrough{DevPath: "/dev/sda", Op: "SG_IO", Status: 0x02},
	}
	if got := classifyRefusal(hdioRefused); got != refusedAtIoctl {
		t.Errorf("classifyRefusal(HDIO EPERM, SG_IO check condition) = %v, want refusedAtIoctl", got)
	}

	// And the mirror, so the test is not passing on the old single-Unwrap path.
	sgioRefused := &ErrSMARTUnavailable{
		DevPath: "/dev/sda",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "HDIO", Errno: syscall.ENOTTY},
		SGIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "SG_IO", Errno: syscall.EPERM},
	}
	if got := classifyRefusal(sgioRefused); got != refusedAtIoctl {
		t.Errorf("classifyRefusal(HDIO ENOTTY, SG_IO EPERM) = %v, want refusedAtIoctl", got)
	}

	// Neither refused: still not a refusal.
	neither := &ErrSMARTUnavailable{
		DevPath: "/dev/sda",
		HDIOErr: &ErrSMARTIoctl{DevPath: "/dev/sda", Op: "HDIO", Errno: syscall.ENOTTY},
		SGIOErr: &ErrSMARTPassthrough{DevPath: "/dev/sda", Op: "SG_IO", Status: 0x02},
	}
	if got := classifyRefusal(neither); got != notRefused {
		t.Errorf("classifyRefusal(no refusal) = %v, want notRefused", got)
	}
}

// TestTransportCompleted_DoesNotClaimTheDriveAnswered pins the modest claim.
// An all-zero sector is what a bridge returns when it answers WITHOUT asking
// the drive -- ErrSMARTEmptySector exists to say so -- and a short transfer
// proves the transfer completed, not who filled it. All this predicate decides
// is whether a controller's opacity is still a candidate explanation.
func TestTransportCompleted_DoesNotClaimTheDriveAnswered(t *testing.T) {
	completed := []error{
		&ErrSMARTChecksum{DevPath: "/dev/sda", Sum: 7},
		&ErrSMARTEmptySector{DevPath: "/dev/sda"},
		&ErrSMARTShortTransfer{DevPath: "/dev/sda", Op: "SMART READ DATA", Want: 512, Got: 128},
		&ErrSMARTNoAttributes{DevPath: "/dev/sda"},
	}
	for _, err := range completed {
		if !transportCompleted(err) {
			t.Errorf("transportCompleted(%T) = false; bytes came back", err)
		}
	}

	// Nothing came back from these, so the controller is still a candidate.
	for _, err := range []error{
		&ErrSMARTPassthrough{DevPath: "/dev/sda", Op: "SG_IO", Status: 0x02},
		&ErrSMARTIoctl{DevPath: "/dev/sda", Op: "SG_IO", Errno: syscall.EPERM},
		&ErrSMARTPermission{DevPath: "/dev/sda"},
		errors.New("something else"),
	} {
		if transportCompleted(err) {
			t.Errorf("transportCompleted(%T) = true; no response was returned", err)
		}
	}
}
