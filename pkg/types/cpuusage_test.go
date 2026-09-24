package types_test

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

func TestMeanCPUUsage(t *testing.T) {
	measured := func(p float64) types.CPUInfo { return types.CPUInfo{UsagePercent: p, UsageAvailable: true} }
	unknown := types.CPUInfo{}
	cases := map[string]struct {
		cpus         []types.CPUInfo
		wantMean     float64
		wantMeasured int
	}{
		"no cores":            {cpus: nil},
		"every core measured": {cpus: []types.CPUInfo{measured(10), measured(30)}, wantMean: 20, wantMeasured: 2},
		"unknown cores are left out, not counted as idle": {
			cpus:     []types.CPUInfo{measured(40), unknown, measured(20), unknown},
			wantMean: 30, wantMeasured: 2,
		},
		"every core unknown": {cpus: []types.CPUInfo{unknown, unknown}},
		// A zero-percent reading that was measured is idle, and counts.
		"measured idle counts": {cpus: []types.CPUInfo{measured(0), measured(50)}, wantMean: 25, wantMeasured: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mean, n := types.MeanCPUUsage(tc.cpus)
			if mean != tc.wantMean || n != tc.wantMeasured {
				t.Errorf("MeanCPUUsage = (%v, %d), want (%v, %d)", mean, n, tc.wantMean, tc.wantMeasured)
			}
		})
	}
}
