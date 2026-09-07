package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// A trimmed but realistic /proc/vmstat. The counters of interest sit among
// dozens of unrelated keys, which is the parsing case that matters.
const vmstatFixture = `nr_free_pages 1049174
nr_zone_inactive_anon 62119
pgpgin 15928591
pgpgout 39221204
pswpin 1204
pswpout 98213
pgfault 3358928101
pgmajfault 138741
`

// withVmstat points the collector at a fixture /proc/vmstat and restores the
// real path afterwards. Unit tests must never read the host's own /proc: the
// values there change under the test and cannot express the absent case at all.
func withVmstat(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vmstat")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture vmstat: %v", err)
	}
	withVmstatPath(t, path)
}

// withVmstatPath points the collector at an arbitrary path, including one that
// does not exist.
func withVmstatPath(t *testing.T, path string) {
	t.Helper()
	orig := vmstatPath
	vmstatPath = path
	t.Cleanup(func() { vmstatPath = orig })
}

// TestReadSwapCounters_ParsesPageCounts verifies the cumulative counters are
// read as the kernel reports them: page counts, not bytes, and untouched by
// the surrounding keys.
func TestReadSwapCounters_ParsesPageCounts(t *testing.T) {
	withVmstat(t, vmstatFixture)

	in, out, ok := readSwapCounters(vmstatPath)
	if !ok {
		t.Fatal("readSwapCounters reported not-ok for a vmstat carrying both counters")
	}
	if in != 1204 {
		t.Errorf("swap-in: got %d pages, want 1204", in)
	}
	if out != 98213 {
		t.Errorf("swap-out: got %d pages, want 98213", out)
	}
}

// TestReadSwapCounters_MissingFile verifies an unreadable /proc/vmstat reports
// not-ok rather than zero counters. A container without /proc mounted must not
// look like a host that has never paged.
func TestReadSwapCounters_MissingFile(t *testing.T) {
	withVmstatPath(t, filepath.Join(t.TempDir(), "nonexistent"))

	in, out, ok := readSwapCounters(vmstatPath)
	if ok {
		t.Error("readSwapCounters reported ok for a missing file")
	}
	if in != 0 || out != 0 {
		t.Errorf("counters for a missing file: got in=%d out=%d, want 0 and 0", in, out)
	}
}

// TestReadSwapCounters_CountersAbsent verifies a vmstat that parses but carries
// neither counter reports not-ok. A kernel built without CONFIG_SWAP omits
// pswpin/pswpout entirely, and "never paged" is not something we know there.
func TestReadSwapCounters_CountersAbsent(t *testing.T) {
	withVmstat(t, "nr_free_pages 1049174\npgfault 3358928101\n")

	if _, _, ok := readSwapCounters(vmstatPath); ok {
		t.Error("readSwapCounters reported ok for a vmstat with no swap counters")
	}
}

// TestReadSwapCounters_BadInput verifies malformed content is rejected rather
// than silently contributing zeros: a truncated line, a non-numeric value, and
// only one of the two counters present.
func TestReadSwapCounters_BadInput(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no value", "pswpin\npswpout\n"},
		{"non-numeric", "pswpin abc\npswpout def\n"},
		{"negative", "pswpin -5\npswpout -9\n"},
		{"only swap-in", "pswpin 1204\n"},
		{"only swap-out", "pswpout 98213\n"},
		{"empty file", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withVmstat(t, tc.content)
			in, out, ok := readSwapCounters(vmstatPath)
			if ok {
				t.Errorf("reported ok for %q content, got in=%d out=%d", tc.name, in, out)
			}
			if in != 0 || out != 0 {
				t.Errorf("counters for %q content: got in=%d out=%d, want 0 and 0", tc.name, in, out)
			}
		})
	}
}

// TestMemoryCollect_SwapCountersUnchanged verifies the ZERO-DELTA case: two
// collections against an unchanged vmstat report identical counters. This is
// the healthy host that swapped days ago and has been quiet since — the one an
// occupancy-based red line wrongly refuses work.
func TestMemoryCollect_SwapCountersUnchanged(t *testing.T) {
	withVmstat(t, vmstatFixture)
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect() error = %v", err)
	}
	first := c.Info()

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}
	second := c.Info()

	if !first.SwapCountersAvailable || !second.SwapCountersAvailable {
		t.Fatalf("SwapCountersAvailable: got %v then %v, want true both times",
			first.SwapCountersAvailable, second.SwapCountersAvailable)
	}
	if second.SwapInPages != first.SwapInPages || second.SwapOutPages != first.SwapOutPages {
		t.Errorf("counters moved with an unchanged vmstat: in %d->%d, out %d->%d",
			first.SwapInPages, second.SwapInPages, first.SwapOutPages, second.SwapOutPages)
	}
	if first.PageSizeBytes != uint64(os.Getpagesize()) {
		t.Errorf("PageSizeBytes: got %d, want %d", first.PageSizeBytes, os.Getpagesize())
	}
}

// TestMemoryCollect_SwapCountersAdvance verifies the NONZERO-DELTA case: a
// vmstat whose counters have moved between collections is reported as moved.
// This is the thrashing host, and it is invisible to SwapUsedBytes when paging
// in and out are balanced — as they are here, with occupancy identical across
// both readings.
func TestMemoryCollect_SwapCountersAdvance(t *testing.T) {
	withVmstat(t, vmstatFixture)
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect() error = %v", err)
	}
	first := c.Info()

	// Same host, one interval later: 500 pages in and 500 out, so occupancy is
	// unchanged and only the counters show the churn.
	advanced := "pswpin 1704\npswpout 98713\n"
	if err := os.WriteFile(vmstatPath, []byte(advanced), 0o644); err != nil {
		t.Fatalf("rewrite fixture vmstat: %v", err)
	}
	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}
	second := c.Info()

	if got := second.SwapInPages - first.SwapInPages; got != 500 {
		t.Errorf("swap-in delta: got %d pages, want 500", got)
	}
	if got := second.SwapOutPages - first.SwapOutPages; got != 500 {
		t.Errorf("swap-out delta: got %d pages, want 500", got)
	}
}

// TestMemoryCollect_SwapCountersUnavailable verifies that a host where the
// counters cannot be read reports them as unavailable rather than as zero
// paging. Zero here would read as a perfectly quiet machine.
func TestMemoryCollect_SwapCountersUnavailable(t *testing.T) {
	withVmstatPath(t, filepath.Join(t.TempDir(), "nonexistent"))
	resetWarnOnce()
	t.Cleanup(resetWarnOnce)

	c := NewMemoryCollector(quietLogger())
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	info := c.Info()
	if info.SwapCountersAvailable {
		t.Error("SwapCountersAvailable: got true with vmstat unreadable, want false")
	}
	if info.SwapInPages != 0 || info.SwapOutPages != 0 {
		t.Errorf("counters: got in=%d out=%d, want 0 and 0 when unavailable",
			info.SwapInPages, info.SwapOutPages)
	}
	// The page size is a property of the host, not of the reading, so it is
	// still reported.
	if info.PageSizeBytes != uint64(os.Getpagesize()) {
		t.Errorf("PageSizeBytes: got %d, want %d", info.PageSizeBytes, os.Getpagesize())
	}
}
