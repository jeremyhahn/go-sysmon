//go:build linux

package collector

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// writeMD lays down a fake /sys/block/<md>/md/ tree under root.
func writeMD(t *testing.T, root, md string, attrs map[string]string, members map[string]map[string]string) {
	t.Helper()
	mdDir := filepath.Join(root, md, "md")
	if err := os.MkdirAll(mdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range attrs {
		if err := os.WriteFile(filepath.Join(mdDir, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for dev, da := range members {
		dd := filepath.Join(mdDir, "dev-"+dev)
		if err := os.MkdirAll(dd, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range da {
			if err := os.WriteFile(filepath.Join(dd, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestApplyRAID_DegradedMirror models fs's real failure: a 2-disk raid1 with one
// member missing. The array must come back degraded with one active member.
func TestApplyRAID_DegradedMirror(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md2",
		map[string]string{
			"level": "raid1", "array_state": "clean", "raid_disks": "2",
			"degraded": "1", "sync_action": "idle", "mismatch_cnt": "0",
		},
		map[string]map[string]string{
			"sda2": {"state": "in_sync", "slot": "0", "errors": "0"},
		},
	)
	c := &DiskCollector{logger: slog.Default()}
	withSysBlockRoot(t, root)

	info := &types.DiskInfo{Name: "md2", Model: "unknown"}
	c.applyRAID("md2", info)

	if info.RAID == nil {
		t.Fatal("RAID is nil for an md array")
	}
	r := info.RAID
	if r.Level != "raid1" || !r.Degraded || r.TotalDevices != 2 || r.ActiveDevices != 1 {
		t.Errorf("got level=%q degraded=%v total=%d active=%d; want raid1/degraded/2/1",
			r.Level, r.Degraded, r.TotalDevices, r.ActiveDevices)
	}
	if r.SyncAction != "idle" || r.SyncProgressPercent != 100 {
		t.Errorf("idle array should report 100%% progress; got action=%q pct=%v", r.SyncAction, r.SyncProgressPercent)
	}
	if info.Model != "Linux md raid1" {
		t.Errorf("model not improved for array: %q", info.Model)
	}
}

// TestApplyRAID_Recovering checks that an in-flight rebuild reports its
// sync_completed progress.
func TestApplyRAID_Recovering(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md2",
		map[string]string{
			"level": "raid1", "array_state": "active", "raid_disks": "2",
			"degraded": "1", "sync_action": "recover", "sync_completed": "26361344 / 105445376",
		},
		map[string]map[string]string{
			"sda2": {"state": "in_sync", "slot": "0"},
			"sdc2": {"state": "spare", "slot": "none"},
		},
	)
	c := &DiskCollector{logger: slog.Default()}
	withSysBlockRoot(t, root)

	info := &types.DiskInfo{Name: "md2"}
	c.applyRAID("md2", info)

	if info.RAID == nil || info.RAID.SyncAction != "recover" {
		t.Fatalf("expected recover action, got %+v", info.RAID)
	}
	if got := info.RAID.SyncProgressPercent; got < 24 || got > 26 {
		t.Errorf("sync progress = %v, want ~25", got)
	}
	if info.RAID.SpareDevices != 1 {
		t.Errorf("spare count = %d, want 1", info.RAID.SpareDevices)
	}
	// Members must be ordered: slotted member first, slotless spare last.
	if len(info.RAID.Members) != 2 || info.RAID.Members[0].Name != "sda2" {
		t.Errorf("member ordering wrong: %+v", info.RAID.Members)
	}
}

// TestApplyRAID_Healthy confirms a fully in-sync array is not flagged degraded.
func TestApplyRAID_Healthy(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md0",
		map[string]string{
			"level": "raid10", "array_state": "clean", "raid_disks": "4",
			"degraded": "0", "sync_action": "idle", "mismatch_cnt": "0",
		},
		map[string]map[string]string{
			"sdb1": {"state": "in_sync", "slot": "0"},
			"sdd1": {"state": "in_sync", "slot": "1"},
			"sde1": {"state": "in_sync", "slot": "2"},
			"sdf1": {"state": "in_sync", "slot": "3"},
		},
	)
	c := &DiskCollector{logger: slog.Default()}
	withSysBlockRoot(t, root)

	info := &types.DiskInfo{Name: "md0"}
	c.applyRAID("md0", info)

	if info.RAID == nil {
		t.Fatal("RAID nil for md0")
	}
	if info.RAID.Degraded || info.RAID.ActiveDevices != 4 {
		t.Errorf("healthy raid10 flagged wrong: degraded=%v active=%d", info.RAID.Degraded, info.RAID.ActiveDevices)
	}
}

// TestApplyRAID_NonArray verifies a plain disk gets no RAID block.
func TestApplyRAID_NonArray(t *testing.T) {
	c := &DiskCollector{logger: slog.Default()}
	info := &types.DiskInfo{Name: "sda"}
	c.applyRAID("sda", info)
	if info.RAID != nil {
		t.Errorf("plain disk should have no RAID info, got %+v", info.RAID)
	}
}

// TestIsMDName covers the array-name predicate, including partitions which are
// not arrays.
func TestIsMDName(t *testing.T) {
	cases := map[string]bool{
		"md0": true, "md127": true, "md_d0": true,
		"sda": false, "sdb1": false, "nvme0n1": false, "md": false, "mdx": false,
	}
	for name, want := range cases {
		if got := isMDName(name); got != want {
			t.Errorf("isMDName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestApplyRAID_Raid0NotDegraded guards the fix for the degraded-inference
// false positive: a raid0 whose members do not carry an in_sync flag must not
// be reported degraded.
func TestApplyRAID_Raid0NotDegraded(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md1",
		map[string]string{
			"level": "raid0", "array_state": "clean", "raid_disks": "2",
			"degraded": "0", "sync_action": "none",
		},
		map[string]map[string]string{
			"sda1": {"state": "in_sync", "slot": "0"},
			"sdc1": {"state": "in_sync", "slot": "1"},
		},
	)
	c := &DiskCollector{logger: slog.Default()}
	withSysBlockRoot(t, root)

	info := &types.DiskInfo{Name: "md1"}
	c.applyRAID("md1", info)
	if info.RAID == nil || info.RAID.Degraded {
		t.Errorf("raid0 must not be flagged degraded: %+v", info.RAID)
	}
}

// TestIsRedundantRAIDLevel documents which levels drive degraded inference.
func TestIsRedundantRAIDLevel(t *testing.T) {
	for _, l := range []string{"raid1", "raid4", "raid5", "raid6", "raid10"} {
		if !isRedundantRAIDLevel(l) {
			t.Errorf("%s should be redundant", l)
		}
	}
	for _, l := range []string{"raid0", "linear", "", "faulty"} {
		if isRedundantRAIDLevel(l) {
			t.Errorf("%s should not be redundant", l)
		}
	}
}

// TestApplyRAID_Raid0WithoutDegradedAttr is a regression guard measured on real
// hardware: the kernel exposes no "degraded" attribute for a raid0, so treating
// its absence as an unreadable health reading flagged a perfectly healthy
// stripe set as non-operational.
func TestApplyRAID_Raid0WithoutDegradedAttr(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md1",
		// No "degraded" key at all, exactly as sysfs presents a raid0.
		map[string]string{
			"level": "raid0", "array_state": "clean", "raid_disks": "2", "sync_action": "none",
		},
		map[string]map[string]string{
			"sda1": {"state": "in_sync", "slot": "0"},
			"sdc1": {"state": "in_sync", "slot": "1"},
		},
	)
	c := &DiskCollector{logger: slog.Default()}
	withSysBlockRoot(t, root)

	info := &types.DiskInfo{Name: "md1"}
	c.applyRAID("md1", info)

	if info.RAID == nil {
		t.Fatal("RAID nil for md1")
	}
	if info.RAID.State != "clean" {
		t.Errorf("state = %q, want it preserved as clean; a missing degraded attr is normal for raid0", info.RAID.State)
	}
	if info.RAID.Degraded {
		t.Error("a healthy raid0 must not be reported degraded")
	}
}

// TestApplyRAID_RedundantArrayWithUnreadableHealth covers the other side: for a
// level that has redundancy to lose, an unreadable degraded attribute must not
// be reported as healthy.
func TestApplyRAID_RedundantArrayWithUnreadableHealth(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "md2",
		map[string]string{"level": "raid1", "array_state": "clean", "raid_disks": "2", "sync_action": "idle"},
		map[string]map[string]string{"sda2": {"state": "in_sync", "slot": "0"}},
	)
	c := &DiskCollector{logger: slog.Default()}
	withSysBlockRoot(t, root)

	info := &types.DiskInfo{Name: "md2"}
	c.applyRAID("md2", info)

	if info.RAID == nil {
		t.Fatal("RAID nil for md2")
	}
	if info.RAID.State != "" {
		t.Errorf("state = %q, want it cleared so an unreadable health reading is not shown as clean", info.RAID.State)
	}
}
