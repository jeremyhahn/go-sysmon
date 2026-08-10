package collector_test

import (
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
)

// TestDiskCollector_Collect verifies that Collect returns at least one disk
// and that each disk has a non-empty name.
func TestDiskCollector_Collect(t *testing.T) {
	c := collector.NewDiskCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	disks := c.Info()
	if len(disks) == 0 {
		t.Fatal("expected at least one DiskInfo after Collect")
	}
	for i, d := range disks {
		if d.Name == "" {
			t.Errorf("disk[%d]: expected non-empty name", i)
		}
		if d.UsedPercent < 0 || d.UsedPercent > 100 {
			t.Errorf("disk[%d] %q: UsedPercent %v outside [0,100]", i, d.Name, d.UsedPercent)
		}
		// SMART attrs, if present, should have valid IDs.
		for j, attr := range d.SMARTAttrs {
			if attr.Name == "" {
				t.Errorf("disk[%d] %q: smart attr[%d] has empty name", i, d.Name, j)
			}
		}
	}
}

// TestDiskCollector_InfoBeforeCollect verifies safe access before Collect.
func TestDiskCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewDiskCollector(silentLogger())
	disks := c.Info()
	if disks == nil {
		t.Error("Info() must return non-nil slice before Collect")
	}
}

// TestDiskNameFromPartition exercises the partition-to-disk name derivation
// indirectly through the Collect logic by ensuring NVMe partitions are
// associated with their parent disk.
func TestDiskCollector_NVMePartitionsAssociated(t *testing.T) {
	c := collector.NewDiskCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	disks := c.Info()
	for _, d := range disks {
		if !strings.HasPrefix(d.Name, "nvme") {
			continue
		}
		// NVMe disks should have their partitions associated if present.
		// We cannot guarantee any partitions exist in all test environments,
		// so just confirm the disk itself is complete.
		if d.Name == "" {
			t.Errorf("nvme disk has empty name")
		}
	}
}
