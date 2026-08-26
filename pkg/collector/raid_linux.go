//go:build linux

package collector

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Linux md (software RAID) status.
//
// The kernel exposes every md array under /sys/block/<md>/md/ as a set of
// small, world-readable text files: level, array_state, degraded, raid_disks,
// sync_action, sync_completed, mismatch_cnt, and one dev-<member>/ directory
// per component with its state, slot and error count. Reading these needs no
// privilege, so RAID health is available even when the service runs
// unprivileged and SMART is not. This is what surfaces a degraded array in the
// dashboard — the single most important signal on a storage host.

// isMDName reports whether a block device name is a Linux md array (md0, md127,
// md_d0). Partitions of an array (mdXpN) are not arrays themselves.
func isMDName(name string) bool {
	if !strings.HasPrefix(name, "md") {
		return false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(name, "md"), "_d")
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isRedundantRAIDLevel reports whether an md level provides redundancy, so that
// a member being out of sync means the array is degraded. raid0 and linear have
// no redundancy and their members are not tracked with an in_sync flag.
func isRedundantRAIDLevel(level string) bool {
	switch level {
	case "raid1", "raid4", "raid5", "raid6", "raid10":
		return true
	default:
		return false
	}
}

// readMDAttr reads a single md sysfs attribute file and trims whitespace.
// Missing files return "" with ok=false so callers can distinguish absent from
// empty.
func readMDAttr(mdDir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(mdDir, name))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// applyRAID populates info.RAID for an md array device from
// <sysBlockRoot>/<name>/md/. It is a no-op (leaves info.RAID nil) for devices
// that are not arrays or expose no md directory.
func (c *DiskCollector) applyRAID(name string, info *types.DiskInfo) {
	if !isMDName(name) {
		return
	}
	mdDir := filepath.Join(sysBlockRoot, name, "md")
	if fi, err := os.Stat(mdDir); err != nil || !fi.IsDir() {
		return
	}

	r := &types.RAIDInfo{SyncAction: "idle", SyncProgressPercent: 100}

	if v, ok := readMDAttr(mdDir, "level"); ok {
		r.Level = v
	}
	if v, ok := readMDAttr(mdDir, "array_state"); ok {
		r.State = v
	}
	if v, ok := readMDAttr(mdDir, "raid_disks"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			r.TotalDevices = n
		}
	}
	if v, ok := readMDAttr(mdDir, "degraded"); ok {
		// degraded holds the count of missing/failed members; >0 means degraded.
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.Degraded = true
		}
	} else if isRedundantRAIDLevel(r.Level) {
		// Health could not be determined for an array that has redundancy to
		// lose. Reporting "not degraded" would turn a missing reading into a
		// healthy verdict, so the state is cleared instead: an empty state is
		// treated as non-operational by both the alert rules and the dashboard.
		//
		// Non-redundant levels are excluded deliberately: raid0 and linear have
		// no redundancy, so the kernel exposes no "degraded" attribute for them
		// at all, and treating its absence as a fault would flag every healthy
		// stripe set as critical.
		r.State = ""
	}
	if v, ok := readMDAttr(mdDir, "mismatch_cnt"); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			r.MismatchCount = n
		}
	}
	if v, ok := readMDAttr(mdDir, "sync_action"); ok && v != "" {
		r.SyncAction = v
	}

	// A running resync/recover/check reports progress as "done / total" sectors
	// in sync_completed; when idle it reads "none".
	if r.SyncAction != "idle" {
		if v, ok := readMDAttr(mdDir, "sync_completed"); ok {
			if done, total, ok := parseSyncCompleted(v); ok && total > 0 {
				r.SyncProgressPercent = float64(done) / float64(total) * 100
			} else {
				r.SyncProgressPercent = 0
			}
		} else {
			r.SyncProgressPercent = 0
		}
	}

	r.Members = readRAIDMembers(mdDir)
	for _, m := range r.Members {
		switch {
		case strings.Contains(m.State, "faulty"):
			r.FailedDevices++
		case strings.Contains(m.State, "spare"):
			r.SpareDevices++
		case strings.Contains(m.State, "in_sync"):
			r.ActiveDevices++
		}
	}

	// The kernel degraded attribute is authoritative. For redundant levels also
	// infer degraded when fewer members are in sync than configured — but not
	// for raid0/linear, whose members may not carry the in_sync flag and would
	// otherwise be flagged degraded while perfectly healthy.
	if isRedundantRAIDLevel(r.Level) && r.TotalDevices > 0 && r.ActiveDevices < r.TotalDevices {
		r.Degraded = true
	}

	info.RAID = r
	// Improve the generic identity for an array device.
	if info.Model == "" || info.Model == "unknown" {
		if r.Level != "" {
			info.Model = "Linux md " + r.Level
		} else {
			info.Model = "Linux md array"
		}
	}
	if info.DriveType == "" || info.DriveType == "Unknown" {
		info.DriveType = "RAID"
	}
}

// parseSyncCompleted parses an md sync_completed value of the form
// "<done> / <total>" (sectors). It returns ok=false for "none" or malformed
// input.
func parseSyncCompleted(v string) (done, total uint64, ok bool) {
	fields := strings.Fields(v)
	if len(fields) != 3 || fields[1] != "/" {
		return 0, 0, false
	}
	d, err1 := strconv.ParseUint(fields[0], 10, 64)
	t, err2 := strconv.ParseUint(fields[2], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return d, t, true
}

// readRAIDMembers reads the component devices of an md array from the
// dev-<name>/ directories under mdDir, returning them ordered by slot then
// name so the output is stable.
func readRAIDMembers(mdDir string) []types.RAIDMember {
	entries, err := os.ReadDir(mdDir)
	if err != nil {
		return nil
	}
	var members []types.RAIDMember
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "dev-") {
			continue
		}
		devDir := filepath.Join(mdDir, e.Name())
		m := types.RAIDMember{
			Name: strings.TrimPrefix(e.Name(), "dev-"),
			Slot: -1,
		}
		if v, ok := readMDAttr(devDir, "state"); ok {
			m.State = v
		}
		if v, ok := readMDAttr(devDir, "slot"); ok && v != "none" {
			if n, err := strconv.Atoi(v); err == nil {
				m.Slot = n
			}
		}
		if v, ok := readMDAttr(devDir, "errors"); ok {
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				m.Errors = n
			}
		}
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		si, sj := members[i].Slot, members[j].Slot
		// Members with a real slot sort ahead of slotless (spare/removed) ones.
		if (si < 0) != (sj < 0) {
			return si >= 0
		}
		if si != sj {
			return si < sj
		}
		return members[i].Name < members[j].Name
	})
	return members
}
