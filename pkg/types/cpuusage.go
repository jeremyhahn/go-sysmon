package types

// MeanCPUUsage returns the mean UsagePercent over the cores whose usage was
// measured (UsageAvailable), and how many of them there were.
//
// A core whose usage is unknown is left out rather than counted as idle: a mean
// that included it would report a node's first collection, or a core whose
// /proc/stat line could not be read, as a machine doing nothing. measured is
// zero when no core was measured, and the mean is then unknown -- every caller
// must check it before using the mean, which is zero in that case.
//
// It is the one definition of a node's CPU utilisation, shared by the alert
// rule, the metrics gauge, the fleet summary, the CLI and the tray, so they
// cannot disagree about which cores count.
func MeanCPUUsage(cpus []CPUInfo) (mean float64, measured int) {
	var sum float64
	for i := range cpus {
		if !cpus[i].UsageAvailable {
			continue
		}
		sum += cpus[i].UsagePercent
		measured++
	}
	if measured == 0 {
		return 0, 0
	}
	return sum / float64(measured), measured
}
