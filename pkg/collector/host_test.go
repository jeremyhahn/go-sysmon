package collector_test

import (
	"context"
	"fmt"
	"reflect"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestHostCollector_Collect verifies that a successful Collect populates the
// HostInfo with a non-empty hostname.
func TestHostCollector_Collect(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	info := c.Info()
	if info.Hostname == "" {
		t.Error("expected non-empty hostname after Collect")
	}
	if info.OS == "" {
		t.Error("expected non-empty OS after Collect")
	}
	if info.KernelVersion == "" {
		t.Error("expected non-empty kernel version after Collect")
	}
}

// TestHostCollector_InfoBeforeCollect verifies that Info returns a zero-value
// HostInfo before the first Collect call (no panic or nil dereference).
func TestHostCollector_InfoBeforeCollect(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())
	if info := c.Info(); !reflect.DeepEqual(info, types.HostInfo{}) {
		t.Errorf("Info() before Collect = %+v, want the zero HostInfo", info)
	}
}

// TestHostCollector_IdentitySurvivesEveryCollect is the regression this split
// creates the room for. The fixed fields are read once, in the constructor, and
// every Collect has to carry them forward; an implementation that builds its
// HostInfo from scratch each tick silently drops the board and BIOS strings and
// the dashboard shows blanks where it used to show the machine.
//
// The assertion is equality across collections rather than "non-empty", because
// a virtual machine legitimately reports empty DMI strings -- on that host a
// non-empty check would pass whether the fields were carried forward or not.
func TestHostCollector_IdentitySurvivesEveryCollect(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())

	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	first := c.Info()

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	second := c.Info()

	fixed := []struct {
		name string
		a, b string
	}{
		{"BoardVendor", first.BoardVendor, second.BoardVendor},
		{"BoardName", first.BoardName, second.BoardName},
		{"BoardVersion", first.BoardVersion, second.BoardVersion},
		{"BIOSVendor", first.BIOSVendor, second.BIOSVendor},
		{"BIOSVersion", first.BIOSVersion, second.BIOSVersion},
		{"BIOSDate", first.BIOSDate, second.BIOSDate},
		{"KernelVersion", first.KernelVersion, second.KernelVersion},
		{"KernelArch", first.KernelArch, second.KernelArch},
	}
	for _, f := range fixed {
		if f.a != f.b {
			t.Errorf("%s changed between collections: %q then %q", f.name, f.a, f.b)
		}
	}
	if first.BootTime != second.BootTime {
		t.Errorf("BootTime changed between collections: %d then %d", first.BootTime, second.BootTime)
	}

	// Kernel version is readable on every Linux host, so this one can be
	// asserted absolutely -- it is what proves the constructor ran at all and
	// that the equality above is not two empty strings agreeing.
	if second.KernelVersion == "" {
		t.Error("KernelVersion is empty: the identity read in the constructor did not reach Info")
	}
}

// The live fields have to keep moving, or the split has traded a cost for a
// lie: a cached uptime is a number that looks current and is not.
func TestHostCollector_UptimeAdvances(t *testing.T) {
	c := collector.NewHostCollector(silentLogger())

	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	before := c.Info().Uptime

	// Uptime is the kernel's sysinfo(2) figure, in whole seconds. Wait for the
	// kernel itself to report a later second -- the real condition, read from
	// the source Collect reads -- rather than sleeping for one and hoping the
	// tick landed inside the sleep.
	await.Until(t, fmt.Sprintf("the kernel's uptime to pass %ds", before), func(context.Context) bool {
		var si syscall.Sysinfo_t
		if err := syscall.Sysinfo(&si); err != nil {
			t.Fatalf("sysinfo: %v", err)
		}
		return uint64(si.Uptime) > before
	})

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if after := c.Info().Uptime; after <= before {
		t.Errorf("uptime went %d -> %d after the kernel's passed %d: it is being cached", before, after, before)
	}
}
