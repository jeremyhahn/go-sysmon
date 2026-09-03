package collector

import psprocess "github.com/shirou/gopsutil/v4/process"

// gopsutil's boot-time cache is off by default, and leaving it off costs more
// than half of every process walk.
//
// Nothing here asks for a boot time. gopsutil does, on our behalf, from inside
// almost every accessor: Times and Nice go through fillFromStat, Processes goes
// through NewProcess, and each of those computes a process create time, which
// needs the boot time. Uncached, every one of those calls re-reads /proc/stat
// AND re-runs virtualization detection -- which itself reads /proc/xen,
// /proc/modules, /proc/cpuinfo, /proc/self/status and /proc/self/cgroup. On a
// host with a thousand processes that is thousands of file reads per tick to
// answer a question whose answer cannot change.
//
// Measured on gpu-host (1070 processes) with make bench-collector:
//
//	                          before              after
//	ProcessCollector.Collect  170.7 ms  92.4 MB   70.8 ms  39.0 MB   -58%
//	VirtCollector.Collect      62.7 ms  30.7 MB   26.4 ms  12.7 MB   -58%
//	SystemCollector.Snapshot   75.3 ms  26.6 MB   50.2 ms  12.4 MB   -33%
//
// A CPU profile before the change put 48% of the whole process walk under
// common.BootTimeWithContext, and 63% of it in raw syscalls; the cost was the
// number of files opened, not the parsing.
//
// What caching changes, stated rather than waved away: the cached value is
// /proc/stat's btime, the wall-clock instant of boot, which the kernel derives
// from the current clock. A wall-clock step therefore moves it. Two collectors
// read a process create time -- container and VM uptime in virt.go -- so those
// two fields can be off by the size of any clock step that lands after the
// first collection. Nothing else in a snapshot depends on it. Measured
// 2026-09-03 on gpu-host, ci-node-1 and mon0: btime is a single constant value
// across a 12-sample window on each, and all three are NTP-synchronised.
//
// This runs in init rather than a constructor because the setting is a package
// global in gopsutil, shared by ProcessCollector and VirtCollector, and a
// constructor would leave whichever one ran first paying the uncached cost.
//
// NOTE FOR ANYONE TIDYING: deleting this line breaks nothing and fails no test.
// It cannot be tested -- gopsutil exposes a setter and no getter, and the
// change has no observable effect on a snapshot's contents. The guard is
// `make bench-collector`, and the numbers above are what to compare against.
func init() {
	psprocess.EnableBootTimeCache(true)
}
