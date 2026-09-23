package collector

import (
	"testing"

	psnet "github.com/shirou/gopsutil/v4/net"
)

// TestBuildNetworkInfo_RatesAvailableOnlyWhenMeasured: a rate is measured only
// when the interface's counters were read on this collection and the one
// before it. Otherwise the rates are zero and say so.
func TestBuildNetworkInfo_RatesAvailableOnlyWhenMeasured(t *testing.T) {
	iface := psnet.InterfaceStat{Name: "sysmon-test-nic0"}
	now := map[string]psnet.IOCountersStat{"sysmon-test-nic0": {BytesSent: 3000, BytesRecv: 6000}}
	before := map[string]psnet.IOCountersStat{"sysmon-test-nic0": {BytesSent: 1000, BytesRecv: 2000}}

	measured := buildNetworkInfo(iface, now, before, 2)
	if !measured.RatesAvailable || measured.BytesSentRate != 1000 || measured.BytesRecvRate != 2000 {
		t.Errorf("with a previous reading: available %v, sent %v, recv %v; want true, 1000, 2000",
			measured.RatesAvailable, measured.BytesSentRate, measured.BytesRecvRate)
	}

	cases := map[string]struct {
		current, prev map[string]psnet.IOCountersStat
		elapsed       float64
	}{
		"first collection":           {now, nil, 0},
		"interface just appeared":    {now, map[string]psnet.IOCountersStat{}, 2},
		"counters could not be read": {map[string]psnet.IOCountersStat{}, before, 2},
		"no time elapsed":            {now, before, 0},
		"counters reset":             {map[string]psnet.IOCountersStat{"sysmon-test-nic0": {BytesSent: 10, BytesRecv: 6000}}, before, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			info := buildNetworkInfo(iface, tc.current, tc.prev, tc.elapsed)
			if info.RatesAvailable {
				t.Errorf("available with sent %v, recv %v; want the rates marked unmeasured",
					info.BytesSentRate, info.BytesRecvRate)
			}
		})
	}
}
