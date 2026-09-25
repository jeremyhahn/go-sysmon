//go:build integration

package collector

import (
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestMemory_UnusableSMBIOSTable verifies memory is still reported when the
// firmware's SMBIOS table cannot be used -- missing, as on hosts that do not
// expose it, or corrupt -- and that sysmon says which, once, before falling
// back to its second source.
func TestMemory_UnusableSMBIOSTable(t *testing.T) {
	requireDevcontainer(t)
	tests := map[string]struct {
		setup []string
		want  string
	}{
		"missing": {[]string{"mount -t tmpfs tmpfs /sys/firmware/dmi/tables"}, "memory: cannot read the SMBIOS table; module details come from ghw where it has them"},
		"corrupt": {[]string{
			"mount -t tmpfs tmpfs /sys/firmware/dmi/tables",
			`printf '\021\002\000\000' >/sys/firmware/dmi/tables/DMI`,
		}, "memory: SMBIOS parse failed, falling back to ghw"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bin := sandbox{setup: tt.setup}.wrap(t, harness.Bins.Sysmon)
			r := harness.Run(t, bin, nil, "memory", "--json")
			if r.Code != 0 {
				t.Fatalf("sysmon memory --json exited %d:\n%s", r.Code, r.Output())
			}
			var mem struct {
				TotalBytes uint64 `json:"total_bytes"`
			}
			harness.MustParseJSON(t, r.Stdout, &mem)
			if mem.TotalBytes == 0 {
				t.Error("no memory total reported")
			}
			if strings.Count(r.Stderr, tt.want) != 1 {
				t.Errorf("stderr says %q %d times, want once:\n%s", tt.want, strings.Count(r.Stderr, tt.want), r.Stderr)
			}
		})
	}
}

// cpuSummary is `sysmon cpu --json` in the parts these tests read.
type cpuSummary struct {
	Summary struct {
		Sockets      int     `json:"sockets"`
		TotalThreads int     `json:"total_threads"`
		MaxMHz       float64 `json:"max_mhz"`
	} `json:"cpu_summary"`
	CPUs []struct {
		ModelName string `json:"model_name"`
	} `json:"cpus"`
}

// TestCPU_WhereSysfsIsMasked verifies the CPU is still described, clock
// included, on hosts that hide parts of sysfs -- containers and guests often
// do: with no cpufreq the clock comes from /proc/cpuinfo, and with no CPU
// directory at all the counts do too.
func TestCPU_WhereSysfsIsMasked(t *testing.T) {
	requireDevcontainer(t)
	tests := map[string]string{
		// A host without cpufreq already lacks what is being masked.
		"no cpufreq":       "if [ -d /sys/devices/system/cpu/cpu0/cpufreq ]; then mount -t tmpfs tmpfs /sys/devices/system/cpu/cpu0/cpufreq; fi",
		"no CPU directory": "mount -t tmpfs tmpfs /sys/devices/system/cpu",
	}
	for name, mask := range tests {
		t.Run(name, func(t *testing.T) {
			bin := sandbox{setup: []string{mask}}.wrap(t, harness.Bins.Sysmon)
			r := harness.Run(t, bin, nil, "cpu", "--json")
			if r.Code != 0 {
				t.Fatalf("sysmon cpu --json exited %d:\n%s", r.Code, r.Output())
			}
			var p cpuSummary
			harness.MustParseJSON(t, r.Stdout, &p)
			if p.Summary.Sockets == 0 || p.Summary.TotalThreads == 0 || len(p.CPUs) == 0 || p.CPUs[0].ModelName == "" {
				t.Errorf("cpu = %+v, want sockets, threads, and every CPU with its model", p)
			}
			if p.Summary.MaxMHz == 0 {
				t.Error("max MHz = 0, want the clock /proc/cpuinfo reports")
			}
		})
	}
}
