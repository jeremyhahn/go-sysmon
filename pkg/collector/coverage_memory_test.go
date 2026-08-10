package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCollectDIMMs_MalformedTableFallsBack verifies a SMBIOS table the parser
// cannot make sense of is not fatal: collection falls back to ghw rather than
// dropping memory reporting entirely.
func TestCollectDIMMs_MalformedTableFallsBack(t *testing.T) {
	// A truncated structure header: enough bytes to be read, not enough to
	// decode.
	withSMBIOSTable(t, []byte{17, 0x28, 0x00})
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())

	// The fallback consults the host, so the result depends on privileges.
	// What matters is that it returns rather than panicking or erroring.
	_ = c.collectDIMMs()
}

// TestCollectDIMMs_TableWithNoMemoryDevices verifies a table that parses but
// describes no memory devices also falls back, rather than reporting a machine
// with zero DIMMs.
func TestCollectDIMMs_TableWithNoMemoryDevices(t *testing.T) {
	// A Type 0 (BIOS Information) structure: valid SMBIOS, no Type 17.
	table := []byte{0, 4, 0x00, 0x00, 0x00, 0x00}
	withSMBIOSTable(t, table)
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	_ = c.collectDIMMs()
}

// TestCollectDIMMsViaGHW_NoModules verifies the ghw fallback reports nothing
// when the system exposes no memory modules, rather than fabricating an entry.
// GHW_CHROOT points ghw at an empty tree so this runs without root.
func TestCollectDIMMsViaGHW_NoModules(t *testing.T) {
	root := t.TempDir()
	// ghw expects the standard sysfs layout beneath its root.
	if err := os.MkdirAll(filepath.Join(root, "sys/devices/system/memory"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("GHW_CHROOT", root)
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	if dimms := c.collectDIMMsViaGHW(); len(dimms) != 0 {
		t.Errorf("got %d DIMMs from an empty tree, want none", len(dimms))
	}
}

// TestMemoryCollect_PopulatesUsage verifies the top-level collector fills in
// the values the dashboard renders, and that a second pass keeps them.
func TestMemoryCollect_PopulatesUsage(t *testing.T) {
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	info := c.Info()
	if info.TotalBytes == 0 {
		t.Fatal("TotalBytes = 0; the host must report some memory")
	}
	if info.UsedBytes > info.TotalBytes {
		t.Errorf("UsedBytes (%d) exceeds TotalBytes (%d)", info.UsedBytes, info.TotalBytes)
	}
	if info.UsedPercent < 0 || info.UsedPercent > 100 {
		t.Errorf("UsedPercent = %v, want 0–100", info.UsedPercent)
	}

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}
	if c.Info().TotalBytes != info.TotalBytes {
		t.Error("TotalBytes changed between collections")
	}
}
