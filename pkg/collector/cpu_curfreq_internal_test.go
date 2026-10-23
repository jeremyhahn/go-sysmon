package collector

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const cpuInfoFixture = `processor	: 0
model name	: Fixture CPU
cpu MHz		: 2400.123

processor	: 1
cpu MHz		: 1800.000

processor	: 2
cpu MHz		: not-a-number

processor	: 3
cpu MHz		: 0.000

cpu MHz		: 999.0
`

func TestParseCPUInfoMHz(t *testing.T) {
	got := parseCPUInfoMHz(cpuInfoFixture)
	want := map[int]float64{0: 2400.123, 1: 1800}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCPUInfoMHz = %v, want %v (malformed, zero and orphan entries left out)", got, want)
	}
}

func TestParseCPUInfoMHz_Empty(t *testing.T) {
	for _, body := range []string{"", "garbage without separators", "processor : x\ncpu MHz : 1000\n"} {
		if got := parseCPUInfoMHz(body); len(got) != 0 {
			t.Errorf("parseCPUInfoMHz(%q) = %v, want empty", body, got)
		}
	}
}

// useCPUFixtures points the collector at temp directories for the test.
func useCPUFixtures(t *testing.T, cpuinfo string) (sysfs string) {
	t.Helper()
	dir := t.TempDir()
	oldBase, oldInfo := cpuSysfsBase, procCPUInfoPath
	cpuSysfsBase = filepath.Join(dir, "cpu")
	procCPUInfoPath = filepath.Join(dir, "cpuinfo")
	t.Cleanup(func() { cpuSysfsBase, procCPUInfoPath = oldBase, oldInfo })
	if cpuinfo != "" {
		if err := os.WriteFile(procCPUInfoPath, []byte(cpuinfo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cpuSysfsBase
}

func writeScalingCurFreq(t *testing.T, base string, idx, content string) {
	t.Helper()
	dir := filepath.Join(base, "cpu"+idx, "cpufreq")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scaling_cur_freq"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadCurMHz_Sources(t *testing.T) {
	base := useCPUFixtures(t, cpuInfoFixture)
	writeScalingCurFreq(t, base, "0", "3200000\n")
	writeScalingCurFreq(t, base, "2", "garbage\n")

	cpuinfo := readProcCPUInfoMHzByCPU()
	tests := map[string]struct {
		idx        int
		wantMHz    float64
		wantSource string
	}{
		"cpufreq wins over cpuinfo":     {0, 3200, curMHzSourceCPUFreq},
		"cpuinfo when cpufreq absent":   {1, 1800, curMHzSourceCPUInfo},
		"malformed cpufreq and cpuinfo": {2, 0, ""},
		"neither source":                {9, 0, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			mhz, source := readCurMHz(tc.idx, cpuinfo)
			if mhz != tc.wantMHz || source != tc.wantSource {
				t.Errorf("readCurMHz(%d) = %v, %q; want %v, %q", tc.idx, mhz, source, tc.wantMHz, tc.wantSource)
			}
		})
	}
}

func TestReadProcCPUInfoMHzByCPU_Unreadable(t *testing.T) {
	useCPUFixtures(t, "")
	if got := readProcCPUInfoMHzByCPU(); len(got) != 0 {
		t.Errorf("unreadable cpuinfo gave %v, want empty", got)
	}
}
