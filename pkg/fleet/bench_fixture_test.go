package fleet

import (
	"fmt"
	"strings"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The busy-host fixture the fleet benchmarks poll.
//
// It is shaped on ci-node-1, the fleet's largest snapshot and the host #76's
// measurements were taken against: a 112-thread hypervisor with 1410
// processes. Two things dominate its snapshot and both are here at full size,
// because they are exactly what the summary and the history payload leave
// behind: the process list, and the CPU flags, which the kernel reports once
// per THREAD -- the same hundred-odd strings, 112 times. A fixture short of
// either would measure a payload the aggregator never sees.

const (
	busyThreads   = 112
	busyProcesses = 1410
	busyDisks     = 12
	busyNICs      = 30
)

// x86Flags is a realistic /proc/cpuinfo flags line for a server-class x86
// part: 120 entries.
var x86Flags = strings.Fields(`fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36
clflush dts acpi mmx fxsr sse sse2 ss ht tm pbe syscall nx pdpe1gb rdtscp lm constant_tsc art
arch_perfmon pebs bts rep_good nopl xtopology nonstop_tsc cpuid aperfmperf pni pclmulqdq dtes64
monitor ds_cpl vmx smx est tm2 ssse3 sdbg fma cx16 xtpr pdcm pcid dca sse4_1 sse4_2 x2apic movbe
popcnt tsc_deadline_timer aes xsave avx f16c rdrand lahf_lm abm 3dnowprefetch cpuid_fault epb
cat_l3 cdp_l3 invpcid_single intel_ppin ssbd mba ibrs ibpb stibp ibrs_enhanced tpr_shadow vnmi
flexpriority ept vpid ept_ad fsgsbase tsc_adjust bmi1 avx2 smep bmi2 erms invpcid cqm mpx rdt_a
avx512f avx512dq rdseed adx smap clflushopt clwb intel_pt avx512cd avx512bw avx512vl xsaveopt
xsavec xgetbv1 xsaves cqm_llc cqm_occup_llc cqm_mbm_total cqm_mbm_local dtherm ida arat pln pts
hwp hwp_act_window hwp_epp hwp_pkg_req pku ospke avx512_vnni md_clear flush_l1d arch_capabilities`)

// busyHost returns a snapshot of ci-node-1's shape.
func busyHost(host string) *types.Snapshot {
	s := &types.Snapshot{
		Timestamp:  time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		Version:    "0.1.0-alpha",
		GitCommit:  "a46f6cf",
		BuildDate:  "2026-09-22T12:00:00Z",
		Host:       types.HostInfo{Hostname: host, Uptime: 864000},
		CPUSummary: types.CPUSummary{Sockets: 2, CoresPerSocket: 28, ThreadsPerCore: 2, TotalCores: 56, TotalThreads: busyThreads},
		LoadAvg:    types.LoadAverage{Load1: 40, Load5: 38, Load15: 35},
		Memory: types.MemoryInfo{
			TotalBytes: 768 << 30, UsedPercent: 73.7, SwapPercent: 51.1,
			SwapInPages: 1 << 20, SwapOutPages: 3 << 20, PageSizeBytes: 4096, SwapCountersAvailable: true,
		},
	}

	s.CPUs = make([]types.CPUInfo, busyThreads)
	for i := range s.CPUs {
		s.CPUs[i] = types.CPUInfo{
			Index: i, VendorID: "GenuineIntel", Family: "6", Model: "85",
			ModelName:  "Intel(R) Xeon(R) Gold 6238R CPU @ 2.20GHz",
			PhysicalID: fmt.Sprint(i / 56), CoreID: fmt.Sprint(i % 28),
			Mhz: 2200, CacheSize: 39424, Microcode: "0x5003604",
			Flags:        x86Flags,
			UsagePercent: float64(i%50) + 10, UsageAvailable: true, TemperatureCelsius: 60,
		}
	}

	s.Processes = types.ProcessSummary{Total: busyProcesses, Running: 4, Sleeping: 1300, Idle: 106}
	s.Processes.Processes = make([]types.ProcessDetail, busyProcesses)
	for i := range s.Processes.Processes {
		s.Processes.Processes[i] = types.ProcessDetail{
			PID: int32(i + 1), Name: fmt.Sprintf("qemu-system-x86-%04d", i), Username: "libvirt-qemu",
			CPUPercent: 1.5, MemoryBytes: 4 << 30, ReadBytes: 1 << 34, WriteBytes: 1 << 33,
			ReadBytesRate: 4096, WriteBytesRate: 8192, Priority: 20, Status: "sleeping",
		}
	}

	s.Disks = make([]types.DiskInfo, busyDisks)
	for i := range s.Disks {
		attrs := make([]types.SMARTAttr, 20)
		for j := range attrs {
			attrs[j] = types.SMARTAttr{ID: uint8(j + 1), Name: fmt.Sprintf("Attribute_%02d", j), Value: 100, Worst: 100, Threshold: 10, Type: "Pre-fail"}
		}
		s.Disks[i] = types.DiskInfo{
			Index: i, Name: fmt.Sprintf("sd%c", 'a'+i), Model: "PERC H740P Mini", Serial: fmt.Sprintf("SN%08d", i),
			SizeBytes: 4 << 40, TotalBytes: 4 << 40, UsedBytes: 3 << 40, UsedPercent: 75,
			ReadBytesRate: 1 << 20, WriteBytesRate: 1 << 20,
			SMARTEnabled: true, SMARTHealthy: true, SMARTAttrs: attrs, Temperature: 38,
		}
	}

	s.Networks = make([]types.NetworkInfo, busyNICs)
	for i := range s.Networks {
		s.Networks[i] = types.NetworkInfo{
			Index: i + 1, Name: fmt.Sprintf("vnet%d", i), HardwareAddr: "fe:54:00:12:34:56",
			Addresses: []string{fmt.Sprintf("10.0.%d.1/24", i), "fe80::fc54:ff:fe12:3456/64"},
			MTU:       1500, Flags: []string{"up", "broadcast", "multicast"}, Speed: 10000, IsUp: true,
			BytesRecvRate: 1 << 20, BytesSentRate: 1 << 19, RatesAvailable: true,
		}
	}

	s.Sensors.CoreTemps = make([]types.CoreTemp, busyThreads/2)
	for i := range s.Sensors.CoreTemps {
		s.Sensors.CoreTemps[i] = types.CoreTemp{PackageID: i / 28, CoreID: i % 28, Label: fmt.Sprintf("Core %d", i%28), TempCelsius: 70}
	}
	s.Sensors.PackagePower = []types.PackagePower{{PackageName: "package-0", PowerW: 150}, {PackageName: "package-1", PowerW: 165}}
	s.Alerts = []types.Alert{
		{ID: "disk:/", Rule: "disk_used", Severity: types.SeverityWarning, Message: "filesystem / is 88.2% full"},
		{ID: "temp", Rule: "temperature", Severity: types.SeverityWarning, Message: "CPU package at 80 °C"},
	}
	types.ReplaceNilSlices(s)
	return s
}
