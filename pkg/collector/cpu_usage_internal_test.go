package collector

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withProcStat points readProcStat at a synthetic /proc/stat holding content.
func withProcStat(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	orig := procStatPath
	procStatPath = path
	t.Cleanup(func() { procStatPath = orig })
}

// ---- readProcStat ---------------------------------------------------------

func TestReadProcStat_ParsesEveryCPULine(t *testing.T) {
	withProcStat(t, "cpu  10 0 10 80 0 0 0 0 0 0\n"+
		"cpu0 1 2 3 4 5 6 7 8 0 0\n"+
		"cpu1 10 20 30 40 50 60 70\n"+
		"intr 12345 0 0\n")

	got, err := readProcStat()
	if err != nil {
		t.Fatalf("readProcStat: %v", err)
	}
	want := map[int]cpuTimes{
		0: {user: 1, nice: 2, system: 3, idle: 4, iowait: 5, irq: 6, softirq: 7, steal: 8},
		// A kernel older than 2.6.11 has no steal column; that is steal at zero.
		1: {user: 10, nice: 20, system: 30, idle: 40, iowait: 50, irq: 60, softirq: 70},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d CPUs %+v, want %d (the aggregate and non-cpu lines are skipped)", len(got), got, len(want))
	}
	for idx, w := range want {
		if got[idx] != w {
			t.Errorf("cpu%d = %+v, want %+v", idx, got[idx], w)
		}
	}
}

// TestReadProcStat_MalformedLinesAreLeftOut verifies that a line which cannot
// be parsed is reported, typed, and left out -- never read as zero counters --
// while the lines around it still parse.
func TestReadProcStat_MalformedLinesAreLeftOut(t *testing.T) {
	withProcStat(t, "cpu0 1 2 3 4 5 6 7 8\n"+
		"cpu1 1 2 x 4 5 6 7 8\n"+
		"cpu2 1 2 3\n"+
		"cpuX 1 2 3 4 5 6 7 8\n"+
		"cpu3 1 2 3 4 5 6 7 -8\n"+
		"cpu4 1 2 3 4 5 6 7 8\n")

	got, err := readProcStat()
	if _, ok := got[0]; !ok {
		t.Error("cpu0 was well formed and is missing")
	}
	if _, ok := got[4]; !ok {
		t.Error("cpu4 was well formed and is missing")
	}
	for _, idx := range []int{1, 2, 3} {
		if v, ok := got[idx]; ok {
			t.Errorf("cpu%d was malformed and was read as %+v", idx, v)
		}
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("readProcStat() error = %v, want every malformed line reported, joined", err)
	}
	bad := map[string]bool{}
	for _, e := range joined.Unwrap() {
		var lineErr *types.ProcStatLineError
		if !errors.As(e, &lineErr) {
			t.Fatalf("error %v is not a *types.ProcStatLineError", e)
		}
		bad[lineErr.CPU] = true
	}
	for _, cpu := range []string{"cpu1", "cpu2", "cpuX", "cpu3"} {
		if !bad[cpu] {
			t.Errorf("%s was malformed and was not reported; reported %v", cpu, bad)
		}
	}

	var numErr *strconv.NumError
	if !errors.As(err, &numErr) {
		t.Errorf("error %v does not carry the parse failure", err)
	}
	var fieldErr *types.ProcStatusFieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "idle" {
		t.Errorf("error %v does not name cpu2's first missing counter", err)
	}
}

func TestReadProcStat_Unreadable(t *testing.T) {
	orig := procStatPath
	procStatPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { procStatPath = orig })

	got, err := readProcStat()
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("readProcStat() error = %v, want os.ErrNotExist", err)
	}
	if len(got) != 0 {
		t.Errorf("readProcStat() = %+v from an unreadable file, want no readings", got)
	}
}

// ---- cpuUsage -------------------------------------------------------------

func TestCPUUsage(t *testing.T) {
	base := cpuTimes{user: 100, system: 100, idle: 800}
	busier := cpuTimes{user: 150, system: 150, idle: 900} // +100 busy of +200
	cases := map[string]struct {
		prev, cur map[int]cpuTimes
		want      map[int]float64
	}{
		"first collection measures nothing": {
			prev: nil,
			cur:  map[int]cpuTimes{0: base},
			want: map[int]float64{},
		},
		"measured delta": {
			prev: map[int]cpuTimes{0: base},
			cur:  map[int]cpuTimes{0: busier},
			want: map[int]float64{0: 50},
		},
		"core missing from the earlier reading": {
			prev: map[int]cpuTimes{0: base},
			cur:  map[int]cpuTimes{0: busier, 1: busier},
			want: map[int]float64{0: 50},
		},
		"no time elapsed": {
			prev: map[int]cpuTimes{0: base},
			cur:  map[int]cpuTimes{0: base},
			want: map[int]float64{},
		},
		"counters went backwards": {
			prev: map[int]cpuTimes{0: busier},
			cur:  map[int]cpuTimes{0: base},
			want: map[int]float64{},
		},
		"busy went backwards while total advanced": {
			prev: map[int]cpuTimes{0: {user: 200, idle: 100}},
			cur:  map[int]cpuTimes{0: {user: 100, idle: 400}},
			want: map[int]float64{},
		},
		"fully idle is measured, and is zero": {
			prev: map[int]cpuTimes{0: base},
			cur:  map[int]cpuTimes{0: {user: 100, system: 100, idle: 900}},
			want: map[int]float64{0: 0},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := cpuUsage(tc.prev, tc.cur)
			if len(got) != len(tc.want) {
				t.Fatalf("cpuUsage = %v, want %v", got, tc.want)
			}
			for idx, w := range tc.want {
				if g, ok := got[idx]; !ok || g != w {
					t.Errorf("cpu%d = %v (measured %v), want %v", idx, g, ok, w)
				}
			}
		})
	}
}

// ---- Collect --------------------------------------------------------------

// TestCPUCollect_UsageAvailability walks the collector through the sequence
// that decides availability: nothing on the first collection, every core on a
// clean second, and a malformed core unknown -- not zero -- with the error
// returned and the rest of the CPU info still published.
func TestCPUCollect_UsageAvailability(t *testing.T) {
	c := NewCPUCollector(quietLogger())

	// Lines for every CPU index the host reports, since pscpu.Info reads the
	// host's /proc/cpuinfo and this file must cover the same indexes.
	statFor := func(busy uint64, malformed int) string {
		var s string
		for i := range 4096 {
			if i == malformed {
				s += "cpu" + strconv.Itoa(i) + " 1 2 garbage 4 5 6 7 8\n"
				continue
			}
			s += "cpu" + strconv.Itoa(i) + " " + strconv.FormatUint(busy, 10) + " 0 0 " + strconv.FormatUint(busy*3, 10) + " 0 0 0 0\n"
		}
		return s
	}

	withProcStat(t, statFor(100, -1))
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	for _, cpu := range c.Info() {
		if cpu.UsageAvailable || cpu.UsagePercent != 0 {
			t.Errorf("first collection: cpu%d usage %v available %v, want unknown", cpu.Index, cpu.UsagePercent, cpu.UsageAvailable)
		}
	}

	withProcStat(t, statFor(200, -1))
	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	for _, cpu := range c.Info() {
		if !cpu.UsageAvailable || cpu.UsagePercent != 25 {
			t.Errorf("second collection: cpu%d usage %v available %v, want a measured 25", cpu.Index, cpu.UsagePercent, cpu.UsageAvailable)
		}
	}

	withProcStat(t, statFor(300, 0))
	err := c.Collect()
	var collErr *types.CollectorError
	var lineErr *types.ProcStatLineError
	if !errors.As(err, &collErr) || !errors.As(err, &lineErr) || lineErr.CPU != "cpu0" {
		t.Fatalf("third Collect = %v, want a *types.CollectorError naming cpu0's line", err)
	}
	infos := c.Info()
	if len(infos) == 0 {
		t.Fatal("a malformed core withheld the whole CPU section")
	}
	for _, cpu := range infos {
		if cpu.ModelName == "" {
			t.Errorf("cpu%d: CPU info not published alongside the error", cpu.Index)
		}
		if cpu.Index == 0 {
			if cpu.UsageAvailable || cpu.UsagePercent != 0 {
				t.Errorf("malformed cpu0: usage %v available %v, want unknown", cpu.UsagePercent, cpu.UsageAvailable)
			}
			continue
		}
		if !cpu.UsageAvailable || cpu.UsagePercent != 25 {
			t.Errorf("cpu%d beside a malformed line: usage %v available %v, want a measured 25", cpu.Index, cpu.UsagePercent, cpu.UsageAvailable)
		}
	}

	// cpu0 has no baseline from the malformed reading, so the next clean
	// reading cannot measure it either: a delta across two intervals would be
	// reported as one.
	withProcStat(t, statFor(400, -1))
	if err := c.Collect(); err != nil {
		t.Fatalf("fourth Collect: %v", err)
	}
	for _, cpu := range c.Info() {
		if want := cpu.Index != 0; cpu.UsageAvailable != want {
			t.Errorf("after the malformed reading: cpu%d available %v, want %v", cpu.Index, cpu.UsageAvailable, want)
		}
	}
}
