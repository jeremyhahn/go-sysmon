// Package alert turns a snapshot into the list of threshold rules currently
// firing.
//
// Evaluation lives on the server, not in the browser, for one reason: the
// dashboard, the JSON API and the Prometheus endpoint must never disagree about
// whether a host is healthy. One evaluation feeds all three, per-host
// thresholds apply everywhere, and the first-seen timestamp lives next to the
// state that already spans snapshots.
package alert

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Rule identifiers. They appear in the API, in Prometheus labels and in the
// settings view, so they are stable names rather than sentence fragments.
const (
	RuleRAIDDegraded   = "raid_degraded"
	RuleRAIDResync     = "raid_resync"
	RuleRAIDMismatch   = "raid_mismatch"
	RuleSMARTUnhealthy = "smart_unhealthy"
	RuleNVMeSpareLow   = "nvme_spare_low"
	RuleNVMeWearHigh   = "nvme_wear_high"
	RuleDiskFull       = "disk_full"
	RuleDiskUtil       = "disk_util"
	RuleDiskTemp       = "disk_temp"
	RuleCPUTemp        = "cpu_temp"
	RuleCPUThrottle    = "cpu_throttle"
	RuleMemoryHigh     = "memory_high"
	RuleSwapHigh       = "swap_high"
	RulePSIFull        = "psi_full"
	RuleLoadHigh       = "load_high"
	RuleIfaceDown      = "iface_down"
	RuleIfaceErrors    = "iface_errors"
	RuleDiskQueue      = "disk_queue"
	RuleCPUHigh        = "cpu_high"
	RuleIfaceUtil      = "iface_util"
	RuleGPUTemp        = "gpu_temp"
	RuleGPUMemory      = "gpu_memory_high"
	RuleGPUPower       = "gpu_power_high"
	RuleContainerMem   = "container_memory_high"
	RuleContainerCPU   = "container_cpu_high"
	RuleRuntimeDown    = "container_runtime_unreachable"
	RuleRAIDNotActive  = "raid_not_active"
	// RuleRAIDMemberOffline fires for a drive behind a hardware RAID
	// controller that the controller has taken out of service. It is a new
	// rule rather than a reuse because there is no block-device equivalent to
	// inherit routing from -- an md array's failed component is already
	// visible as a device of its own.
	RuleRAIDMemberOffline = "raid_member_offline"

	// RuleSMARTUnreadable fires for a drive behind a controller that HAS
	// answered a health command in this run and has stopped.
	//
	// Warning, not critical: an absence is not a failure, and the drive may be
	// refusing a command it does not implement rather than dying. But an
	// otherwise-readable array where one member goes quiet is an event, and it
	// was silent.
	//
	// The gate is the whole design. A drive that has NEVER answered is a
	// capability fact -- some drives simply do not do SMART behind some
	// controllers -- and firing on that would put a permanent warning on every
	// such host, which trains people to ignore the category and is worse than
	// saying nothing. Only a drive that stops is worth telling anyone about.
	RuleSMARTUnreadable = "smart_unreadable"
)

// nonOperationalArrayStates are md array_state values in which the array is not
// serving I/O. They are distinct from "degraded": a degraded array still works
// with reduced redundancy, whereas these are unavailable. Treating an unknown
// or stopped array as healthy is a fail-open reading of the one signal this
// feature exists to surface.
var nonOperationalArrayStates = map[string]bool{
	"inactive":  true,
	"suspended": true,
	"clear":     true,
	"broken":    true,
	// An array whose state could not be read is reported rather than assumed
	// well; "" is what readMDAttr yields when sysfs gives us nothing.
	"": true,
}

// RAIDNotOperational reports an md array that is not serving I/O.
//
// Exported because the fleet roll-up needs the same answer and there must not
// be a second list: an array state counted as a fault by the alert rules and as
// healthy by the fleet card is a disagreement an operator sees as the dashboard
// contradicting its own alerts.
func RAIDNotOperational(state string) bool {
	return nonOperationalArrayStates[state]
}

// UI categories, which map to dashboard tabs.
const (
	catStorage    = "storage"
	catNetwork    = "network"
	catCPU        = "cpu"
	catMemory     = "memory"
	catGPU        = "gpu"
	catContainers = "containers"
	catVirt       = "virt"
)

// netCounters is the previous error/drop reading for one interface, used to
// turn cumulative counters into a rate.
type netCounters struct {
	errsIn, errsOut, dropsIn, dropsOut uint64
}

// Evaluator applies the rules and remembers the state that a single snapshot
// cannot carry: when each alert first fired, and the counter values needed to
// derive rates.
type Evaluator struct {
	mu sync.Mutex
	// firstSeen keeps the time an alert ID first fired so the UI can show how
	// long a condition has lasted. Entries for cleared alerts are dropped.
	firstSeen map[string]time.Time
	prevNet   map[string]netCounters
	prevThr   map[int]uint64
	prevAt    time.Time
	// prevSwapOut is the last pswpout reading, and prevSwapValid says whether
	// there is one to subtract from -- a zero counter is a real value on a host
	// that has never paged, so it cannot double as "no reading yet".
	prevSwapOut   uint64
	prevSwapValid bool
	// swapRuns counts consecutive evaluations whose swap-out rate was over the
	// threshold. Launching something large pages hard for one interval and then
	// stops; that is the machine working, not a machine in trouble, and it is
	// not worth a bell.
	swapRuns int
	// answered records the RAID members that have returned a SMART reading at
	// least once in this run, keyed by the same resource string the alert is
	// filed under. It is what separates "this drive has stopped answering"
	// from "this drive has never answered", which are an event and a
	// capability fact respectively.
	//
	// It lives here rather than on DiskMember because it is history, and a
	// snapshot cannot carry history. netCounters above is the precedent: the
	// same shape, for the same reason, and it keeps the payload unchanged for
	// a fact only this consumer needs.
	//
	// RESET ON RESTART, deliberately and unavoidably: "has answered in this
	// run" is the only claim a process can make honestly. A drive that was
	// already silent before a restart is indistinguishable from one that never
	// answered, so it is treated as the latter and stays quiet. The
	// alternative -- persisting it -- would mean alerting on a fact nobody in
	// this process observed.
	answered map[string]bool
}

// New returns an Evaluator with no history.
func New() *Evaluator {
	return &Evaluator{
		firstSeen: make(map[string]time.Time),
		prevNet:   make(map[string]netCounters),
		prevThr:   make(map[int]uint64),
		answered:  make(map[string]bool),
	}
}

// builder accumulates alerts during one evaluation.
type builder struct {
	cfg    config.Alerts
	alerts []types.Alert
}

// add records a firing rule. Message is the sentence the dashboard shows, so it
// is written for a reader, not a log parser.
func (b *builder) add(rule string, severity types.Severity, category, resource, message string, value, threshold float64) {
	id := rule
	if resource != "" {
		id = rule + ":" + resource
	}
	b.alerts = append(b.alerts, types.Alert{
		ID:        id,
		Rule:      rule,
		Severity:  severity,
		Category:  category,
		Resource:  resource,
		Message:   message,
		Value:     value,
		Threshold: threshold,
	})
}

// memberResource identifies a drive behind a RAID controller for alert
// de-duplication, which is what an alert ID is built from.
//
// IT IS THE PHYSICAL POSITION, NOT THE SERIAL, and the reason is worth stating
// because the serial looks like the better answer.
//
// A serial is read from the drive with a separate command every cycle. When
// that command fails -- which is exactly what happens to a drive that is on its
// way out, the drive this alert is about -- the serial goes empty. Keying on it
// would mean the ID flipping between the serial and the position and back as
// the command succeeded and failed, so an alert that had been firing for an
// hour would clear and re-fire as though it were a new fault on a different
// drive. The position comes from the controller's configuration, in the same
// reply that produced the member list, so it cannot disappear while the member
// still exists.
//
// The cost is that moving a drive to another bay clears its alert and raises a
// new one. That is a change somebody made deliberately, and a fresh alert for it
// is defensible; a transient command failure is not a change at all.
//
// Note this is the OPPOSITE choice from the metric label, which is the serial,
// and deliberately so: a metric is a long-lived history where a re-bay must not
// silently continue one drive's series under another drive's readings, and a
// missing serial there produces a gap rather than a renamed series. The two
// have different failure modes and therefore different answers.
func memberResource(volume string, m *types.DiskMember) string {
	return fmt.Sprintf("%s/%d:%d", volume, m.Enclosure, m.Slot)
}

// describeMember names a drive the way somebody standing at the rack needs it
// named: the bay to pull, what the drive is, and the volume it belongs to.
// A device path would be no use -- it does not have one.
func describeMember(volume string, m *types.DiskMember) string {
	model := strings.TrimSpace(m.Vendor + " " + m.Model)
	if model == "" {
		model = "drive"
	}
	if m.Serial != "" {
		return fmt.Sprintf("%s in bay %d:%d behind %s (serial %s)",
			model, m.Enclosure, m.Slot, volume, m.Serial)
	}
	return fmt.Sprintf("%s in bay %d:%d behind %s", model, m.Enclosure, m.Slot, volume)
}

// Evaluate returns the alerts firing for snap.// Evaluate returns the alerts firing for snap. Thresholds come from cfg, so a
// reload changes behaviour on the next snapshot with no restart.
func (e *Evaluator) Evaluate(cfg config.Alerts, snap *types.Snapshot, now time.Time) []types.Alert {
	if snap == nil {
		return nil
	}
	b := &builder{cfg: cfg}

	e.mu.Lock()
	elapsed := now.Sub(e.prevAt).Seconds()
	if e.prevAt.IsZero() {
		elapsed = 0
	}
	e.evalStorage(b, snap)
	e.evalCPUAndMemory(b, snap, elapsed)
	e.evalNetwork(b, snap, elapsed)
	evalGPU(b, snap)
	evalVirt(b, snap)
	e.prevAt = now

	// Stamp first-seen and drop history for anything that cleared, so a
	// flapping condition reports the age of its current episode.
	active := make(map[string]time.Time, len(b.alerts))
	for i := range b.alerts {
		id := b.alerts[i].ID
		seen, ok := e.firstSeen[id]
		if !ok {
			seen = now
		}
		active[id] = seen
		b.alerts[i].Since = seen
	}
	e.firstSeen = active
	e.mu.Unlock()

	sort.SliceStable(b.alerts, func(i, j int) bool {
		si, sj := severityRank(b.alerts[i].Severity), severityRank(b.alerts[j].Severity)
		if si != sj {
			return si < sj
		}
		return b.alerts[i].ID < b.alerts[j].ID
	})
	return b.alerts
}

// severityRank orders critical before warning before info.
func severityRank(s types.Severity) int {
	switch s {
	case types.SeverityCritical:
		return 0
	case types.SeverityWarning:
		return 1
	default:
		return 2
	}
}

// thresholds returns the effective thresholds for a resource, applying any
// per-resource override. An override field of zero means "not overridden"
// rather than "disabled", because a zero in an override block is the natural
// result of omitting the key.
func thresholds(cfg config.Alerts, resource string) config.Alerts {
	out := cfg
	for _, o := range cfg.Overrides {
		if o.Resource != resource {
			continue
		}
		if o.DiskFullPct > 0 {
			out.DiskFullPct = o.DiskFullPct
		}
		if o.DiskFullCritPct > 0 {
			out.DiskFullCritPct = o.DiskFullCritPct
		}
		if o.DiskUtilPct > 0 {
			out.DiskUtilPct = o.DiskUtilPct
		}
		if o.DiskQueueLen > 0 {
			out.DiskQueueLen = o.DiskQueueLen
		}
		if o.TempWarnC > 0 {
			out.TempWarnC = o.TempWarnC
		}
		if o.TempCritC > 0 {
			out.TempCritC = o.TempCritC
		}
	}
	return out
}

// evalStorage covers RAID arrays and physical disks.
func (e *Evaluator) evalStorage(b *builder, snap *types.Snapshot) {
	for i := range snap.Disks {
		d := &snap.Disks[i]
		t := thresholds(b.cfg, d.Name)

		// RAID health. A degraded array has no sensible "off" switch, so it is
		// not configurable.
		if d.RAID != nil {
			r := d.RAID
			switch {
			case nonOperationalArrayStates[r.State]:
				state := r.State
				if state == "" {
					state = "unreadable"
				}
				b.add(RuleRAIDNotActive, types.SeverityCritical, catStorage, d.Name,
					fmt.Sprintf("%s is not active (array_state: %s)", d.Name, state), 0, 0)
			case r.Degraded:
				b.add(RuleRAIDDegraded, types.SeverityCritical, catStorage, d.Name,
					fmt.Sprintf("%s (%s) is DEGRADED — %d of %d members in sync",
						d.Name, r.Level, r.ActiveDevices, r.TotalDevices),
					float64(r.ActiveDevices), float64(r.TotalDevices))
			case r.SyncAction != "" && r.SyncAction != "idle":
				b.add(RuleRAIDResync, types.SeverityInfo, catStorage, d.Name,
					fmt.Sprintf("%s is %s — %.0f%% complete", d.Name, r.SyncAction, r.SyncProgressPercent),
					r.SyncProgressPercent, 100)
			}
			if r.MismatchCount > 0 {
				b.add(RuleRAIDMismatch, types.SeverityWarning, catStorage, d.Name,
					fmt.Sprintf("%s reports %d mismatched sectors from its last check", d.Name, r.MismatchCount),
					float64(r.MismatchCount), 0)
			}
		}

		// SMART. Only meaningful when the drive actually answered.
		if d.SMARTEnabled && !d.SMARTHealthy {
			b.add(RuleSMARTUnhealthy, types.SeverityCritical, catStorage, d.Name,
				fmt.Sprintf("/dev/%s reports a failing SMART attribute", d.Name), 0, 0)
		}

		// The drives behind a hardware RAID controller. They are not block
		// devices and are not in this loop's list -- they hang off the volume
		// as Members -- so without this a failing drive in the owner's PERC
		// would appear on the dashboard and raise NOTHING. That is the same
		// gap as reporting health where there is no reading, in the opposite
		// direction: a real reading that says nothing at all.
		//
		// They raise the SAME rules as a block device deliberately. A separate
		// rule name would mean anyone who has already configured notification
		// on smart_unhealthy hears nothing when one of these fails, which is
		// exactly the silence being fixed. The message carries what makes a
		// member different: the bay to pull and the serial, since it has no
		// device name to print.
		for i := range d.Members {
			m := &d.Members[i]
			resource := memberResource(d.Name, m)

			// A drive the controller has taken out of service, named.
			//
			// raid_degraded does not cover this, and assuming it did would be
			// resting on a firmware invariant nobody has checked: the array
			// state and the member state are two separate fields, and a volume
			// reported optimal while one of its drives is offline is a
			// contradiction this would otherwise accept in silence. It also
			// says the thing raid_degraded cannot -- WHICH drive -- which is
			// the actionable half of a degraded array.
			if !m.Online {
				b.add(RuleRAIDMemberOffline, types.SeverityCritical, catStorage, resource,
					fmt.Sprintf("%s is not in service", describeMember(d.Name, m)), 0, 0)
			}
			if m.SMARTEnabled && !m.SMARTHealthy {
				b.add(RuleSMARTUnhealthy, types.SeverityCritical, catStorage, resource,
					fmt.Sprintf("%s reports a failing SMART attribute", describeMember(d.Name, m)), 0, 0)
			}
			// A drive that was answering and has stopped.
			//
			// Recovery needs no handling: the rule only fires while the reading
			// is absent, so a drive that answers again simply stops raising it
			// and the alert clears with every other cleared alert.
			switch {
			case m.SMARTEnabled:
				e.answered[resource] = true
			case e.answered[resource]:
				b.add(RuleSMARTUnreadable, types.SeverityWarning, catStorage, resource,
					fmt.Sprintf("%s has stopped reporting SMART health", describeMember(d.Name, m)), 0, 0)
			}
			// Temperature, on the same thresholds as a block device. On a host
			// whose only spinning disks are behind a controller these are the
			// only drive temperatures there are, so omitting them would leave
			// that machine with no thermal alerting on its disks at all.
			// Thresholds are looked up under the resource the alert IS, not
			// under the volume it hangs off. The alert's identity is
			// "sdb/64:7"; reading its configuration under "sdb" made an
			// override written for one member unreachable, and silently
			// applied an override written for the volume to every member
			// under it. A config override is the one place a user states an
			// intention, so the resource the alert is filed under and the
			// resource its configuration is read under have to be the same
			// string.
			mt := thresholds(b.cfg, resource)
			if m.Temperature > 0 {
				switch {
				case mt.TempCritC > 0 && m.Temperature >= mt.TempCritC:
					b.add(RuleDiskTemp, types.SeverityCritical, catStorage, resource,
						fmt.Sprintf("%s is at %.0f°C", describeMember(d.Name, m), m.Temperature),
						m.Temperature, mt.TempCritC)
				case mt.TempWarnC > 0 && m.Temperature >= mt.TempWarnC:
					b.add(RuleDiskTemp, types.SeverityWarning, catStorage, resource,
						fmt.Sprintf("%s is at %.0f°C", describeMember(d.Name, m), m.Temperature),
						m.Temperature, mt.TempWarnC)
				}
			}
		}

		// NVMe endurance. Gated on the device being NVMe rather than on the
		// readings being non-zero: zero available spare is not a missing value,
		// it is the most severe one there is, and a "> 0" guard silently
		// excluded exactly the drive that has run out.
		if d.SMARTEnabled && isNVMeDevice(d.Name) {
			if t.NVMeSparePct > 0 && float64(d.AvailableSparePercent) < t.NVMeSparePct {
				b.add(RuleNVMeSpareLow, types.SeverityCritical, catStorage, d.Name,
					fmt.Sprintf("/dev/%s has %d%% spare blocks left (below %.0f%%)",
						d.Name, d.AvailableSparePercent, t.NVMeSparePct),
					float64(d.AvailableSparePercent), t.NVMeSparePct)
			}
			if t.NVMeWearPct > 0 && float64(d.WearLevelPercent) >= t.NVMeWearPct {
				b.add(RuleNVMeWearHigh, types.SeverityWarning, catStorage, d.Name,
					fmt.Sprintf("/dev/%s is %d%% worn", d.Name, d.WearLevelPercent),
					float64(d.WearLevelPercent), t.NVMeWearPct)
			}
		}

		// Capacity. Reported against the device's aggregate filesystem usage.
		if d.TotalBytes > 0 {
			switch {
			case t.DiskFullCritPct > 0 && d.UsedPercent >= t.DiskFullCritPct:
				b.add(RuleDiskFull, types.SeverityCritical, catStorage, d.Name,
					fmt.Sprintf("%s is %.0f%% full", d.Name, d.UsedPercent),
					d.UsedPercent, t.DiskFullCritPct)
			case t.DiskFullPct > 0 && d.UsedPercent >= t.DiskFullPct:
				b.add(RuleDiskFull, types.SeverityWarning, catStorage, d.Name,
					fmt.Sprintf("%s is %.0f%% full", d.Name, d.UsedPercent),
					d.UsedPercent, t.DiskFullPct)
			}
		}

		// Saturation. Utilisation says the device is busy; queue depth says
		// requests are waiting behind each other, which is what latency
		// actually follows -- a device can sit at 100% util and still be fine
		// if nothing is queued.
		if t.DiskUtilPct > 0 && d.UtilPercent >= t.DiskUtilPct {
			b.add(RuleDiskUtil, types.SeverityWarning, catStorage, d.Name,
				fmt.Sprintf("%s is %.0f%% utilised (queue %.1f)", d.Name, d.UtilPercent, d.AvgQueueLength),
				d.UtilPercent, t.DiskUtilPct)
		}
		if t.DiskQueueLen > 0 && d.AvgQueueLength >= t.DiskQueueLen {
			b.add(RuleDiskQueue, types.SeverityWarning, catStorage, d.Name,
				fmt.Sprintf("%s has %.1f requests queued on average", d.Name, d.AvgQueueLength),
				d.AvgQueueLength, t.DiskQueueLen)
		}

		// Temperature.
		if d.Temperature > 0 {
			switch {
			case t.TempCritC > 0 && d.Temperature >= t.TempCritC:
				b.add(RuleDiskTemp, types.SeverityCritical, catStorage, d.Name,
					fmt.Sprintf("/dev/%s is at %.0f°C", d.Name, d.Temperature), d.Temperature, t.TempCritC)
			case t.TempWarnC > 0 && d.Temperature >= t.TempWarnC:
				b.add(RuleDiskTemp, types.SeverityWarning, catStorage, d.Name,
					fmt.Sprintf("/dev/%s is at %.0f°C", d.Name, d.Temperature), d.Temperature, t.TempWarnC)
			}
		}
	}
}

// evalSwap fires on how fast the host is paging OUT, not on how much swap is
// occupied.
//
// Occupancy is the wrong question and answering it wrongly had a cost: pages
// parked in swap by a burst that ended hours ago stay counted until something
// reclaims them, so two healthy fleet nodes sat amber indefinitely while a host
// genuinely thrashing with a small swap device stayed silent. The counter is
// cumulative because a rate needs two readings and a window, and the window
// belongs here.
//
// Three things gate it, each for its own reason:
//
//   - SwapCountersAvailable. An unreadable /proc/vmstat reports zero, and so
//     does a host that has never paged. Alerting on the first as if it were the
//     second would be inventing quiet out of ignorance; not alerting at all is
//     the honest reading of "we do not know".
//   - a previous reading to subtract from. The first evaluation of a run has no
//     window, and prevSwapValid says so rather than treating the whole
//     lifetime total as one interval's worth.
//   - two consecutive intervals over the threshold. One is a burst.
func (e *Evaluator) evalSwap(b *builder, snap *types.Snapshot, elapsed float64) {
	t := b.cfg
	m := &snap.Memory

	if t.SwapOutPagesPerSec <= 0 || !m.SwapCountersAvailable {
		e.swapRuns = 0
		e.prevSwapValid = false
		return
	}

	prev, hadPrev := e.prevSwapOut, e.prevSwapValid
	e.prevSwapOut, e.prevSwapValid = m.SwapOutPages, true
	if !hadPrev || elapsed <= 0 {
		return
	}

	// A counter that went backwards means the host rebooted between readings,
	// so the window is not what it appears to be and there is no rate to report.
	if m.SwapOutPages < prev {
		e.swapRuns = 0
		return
	}
	rate := float64(m.SwapOutPages-prev) / elapsed
	if rate < t.SwapOutPagesPerSec {
		e.swapRuns = 0
		return
	}
	e.swapRuns++
	if e.swapRuns < 2 {
		return
	}

	// Bytes are what the reader thinks in, and the page size is not 4096
	// everywhere, so it comes from the snapshot rather than from a constant.
	// Formatted here rather than through pkg/cli's helper, which this package
	// must not import: the renderer sits above the rules, not beside them.
	detail := fmt.Sprintf("Paging out %.0f pages/s", rate)
	if m.PageSizeBytes > 0 {
		detail = fmt.Sprintf("Paging out %.0f pages/s (%.0f KiB/s)", rate,
			rate*float64(m.PageSizeBytes)/1024)
	}
	b.add(RuleSwapHigh, types.SeverityWarning, catMemory, "", detail,
		rate, t.SwapOutPagesPerSec)
}

// evalCPUAndMemory covers processor temperature and throttling, memory, swap,
// pressure stall and load.
func (e *Evaluator) evalCPUAndMemory(b *builder, snap *types.Snapshot, elapsed float64) {
	t := b.cfg

	// Hottest core.
	var maxTemp float64
	for _, c := range snap.Sensors.CoreTemps {
		if c.TempCelsius > maxTemp {
			maxTemp = c.TempCelsius
		}
	}
	if maxTemp > 0 {
		switch {
		case t.TempCritC > 0 && maxTemp >= t.TempCritC:
			b.add(RuleCPUTemp, types.SeverityCritical, catCPU, "",
				fmt.Sprintf("CPU is at %.0f°C", maxTemp), maxTemp, t.TempCritC)
		case t.TempWarnC > 0 && maxTemp >= t.TempWarnC:
			b.add(RuleCPUTemp, types.SeverityWarning, catCPU, "",
				fmt.Sprintf("CPU is at %.0f°C", maxTemp), maxTemp, t.TempWarnC)
		}
	}

	// Throttling is reported as a counter, so only a rise since the previous
	// snapshot means the CPU is throttling now rather than at some point in the
	// machine's past.
	var throttling int
	for _, th := range snap.Sensors.ThermalThrottle {
		total := th.CoreThrottleCount + th.PackageThrottleCount
		if prev, ok := e.prevThr[th.CPU]; ok && total > prev {
			throttling++
		}
		e.prevThr[th.CPU] = total
	}
	if throttling > 0 {
		b.add(RuleCPUThrottle, types.SeverityWarning, catCPU, "",
			fmt.Sprintf("%d CPU(s) throttled since the last sample", throttling),
			float64(throttling), 0)
	}

	// Sustained processor usage. Reported as the mean across logical CPUs so
	// the number means the same thing on a 4-thread and a 64-thread host.
	if t.CPUPct > 0 && len(snap.CPUs) > 0 {
		var sum float64
		for _, c := range snap.CPUs {
			sum += c.UsagePercent
		}
		mean := sum / float64(len(snap.CPUs))
		if mean >= t.CPUPct {
			b.add(RuleCPUHigh, types.SeverityWarning, catCPU, "",
				fmt.Sprintf("CPU is %.0f%% busy across %d threads", mean, len(snap.CPUs)),
				mean, t.CPUPct)
		}
	}

	if t.MemoryPct > 0 && snap.Memory.UsedPercent >= t.MemoryPct {
		b.add(RuleMemoryHigh, types.SeverityWarning, catMemory, "",
			fmt.Sprintf("Memory is %.0f%% used", snap.Memory.UsedPercent),
			snap.Memory.UsedPercent, t.MemoryPct)
	}
	e.evalSwap(b, snap, elapsed)

	// "full" pressure means every runnable task was stalled, which is a much
	// stronger signal than "some" and is what is worth alerting on.
	// Each pressure resource is filed under the tab that owns it, so the alert
	// points at the panel that explains it.
	if t.PSIFullAvg10 > 0 {
		for _, p := range []struct {
			name string
			cat  string
			res  types.PSIResource
		}{
			{"cpu", catCPU, snap.Sensors.PSI.CPU},
			{"memory", catMemory, snap.Sensors.PSI.Memory},
			{"io", catStorage, snap.Sensors.PSI.IO},
		} {
			if p.res.FullAvg10 >= t.PSIFullAvg10 {
				b.add(RulePSIFull, types.SeverityWarning, p.cat, p.name,
					fmt.Sprintf("%s pressure: everything stalled %.0f%% of the last 10s", p.name, p.res.FullAvg10),
					p.res.FullAvg10, t.PSIFullAvg10)
			}
		}
	}

	if t.LoadPerCore > 0 && snap.CPUSummary.TotalThreads > 0 {
		perCore := snap.LoadAvg.Load1 / float64(snap.CPUSummary.TotalThreads)
		if perCore >= t.LoadPerCore {
			b.add(RuleLoadHigh, types.SeverityWarning, catCPU, "",
				fmt.Sprintf("Load %.2f is %.1f× the %d available threads",
					snap.LoadAvg.Load1, perCore, snap.CPUSummary.TotalThreads),
				perCore, t.LoadPerCore)
		}
	}
}

// evalNetwork covers interfaces that should be carrying traffic but are not,
// and interfaces accumulating errors.
func (e *Evaluator) evalNetwork(b *builder, snap *types.Snapshot, elapsed float64) {
	t := b.cfg

	// Rebuilt from scratch each pass rather than updated in place. Keeping
	// entries for interfaces that have gone away is not just a slow leak under
	// veth/tap churn: those names get reused, and a new interface inheriting a
	// vanished one's counters looks like a continuous series. Its whole
	// lifetime of errors would then be divided by a single snapshot interval
	// and reported as a burst that never happened.
	next := make(map[string]netCounters, len(snap.Networks))
	defer func() { e.prevNet = next }()

	for i := range snap.Networks {
		n := &snap.Networks[i]
		if n.IsLoopback {
			continue
		}

		// An interface being down is only notable if it is configured to be
		// used. Hosts routinely carry unused NICs, and alerting on those is
		// noise that trains people to ignore the bell.
		if !n.IsUp && len(n.Addresses) > 0 {
			b.add(RuleIfaceDown, types.SeverityWarning, catNetwork, n.Name,
				fmt.Sprintf("%s is down but has an address configured", n.Name), 0, 0)
		}

		// Link saturation, but only where the link speed is known: a virtual or
		// bridged interface reports 0 and a percentage of nothing is
		// meaningless.
		if t.NetUtilPct > 0 && n.Speed > 0 {
			capacity := float64(n.Speed) * 1_000_000 / 8 // Mbit/s to bytes/s
			busier := n.BytesRecvRate
			dir := "receiving"
			if n.BytesSentRate > busier {
				busier, dir = n.BytesSentRate, "sending"
			}
			if pct := busier / capacity * 100; pct >= t.NetUtilPct {
				b.add(RuleIfaceUtil, types.SeverityWarning, catNetwork, n.Name,
					fmt.Sprintf("%s is %s at %.0f%% of its %d Mbps link", n.Name, dir, pct, n.Speed),
					pct, t.NetUtilPct)
			}
		}

		cur := netCounters{n.ErrorsIn, n.ErrorsOut, n.DropsIn, n.DropsOut}
		prev, had := e.prevNet[n.Name]
		next[n.Name] = cur
		if !had || elapsed <= 0 || t.IfaceErrRate <= 0 {
			continue
		}
		delta := float64(diff(cur.errsIn, prev.errsIn) + diff(cur.errsOut, prev.errsOut) +
			diff(cur.dropsIn, prev.dropsIn) + diff(cur.dropsOut, prev.dropsOut))
		rate := delta / elapsed
		if rate >= t.IfaceErrRate {
			b.add(RuleIfaceErrors, types.SeverityWarning, catNetwork, n.Name,
				fmt.Sprintf("%s is dropping/erroring %.1f packets/s", n.Name, rate),
				rate, t.IfaceErrRate)
		}
	}
}

// isNVMeDevice reports whether a block device name is an NVMe namespace, which
// is what makes the endurance fields meaningful. Their zero values are real
// readings, so availability has to be decided by the device, not the number.
func isNVMeDevice(name string) bool { return strings.HasPrefix(name, "nvme") }

// diff subtracts counters, treating a decrease as a reset rather than a huge
// unsigned wrap.
func diff(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// evalGPU covers accelerator health. Thresholds are looser than the CPU/disk
// equivalents because a GPU under load is designed to sit hot and near its
// power limit; only the edges of that envelope are worth reporting. High
// utilisation is deliberately not an alert -- that is the card doing its job.
func evalGPU(b *builder, snap *types.Snapshot) {
	t := b.cfg
	for i := range snap.GPUs {
		g := &snap.GPUs[i]
		name := fmt.Sprintf("gpu%d", g.Index)

		if g.TemperatureGPU > 0 {
			switch {
			case t.GPUTempCritC > 0 && g.TemperatureGPU >= t.GPUTempCritC:
				b.add(RuleGPUTemp, types.SeverityCritical, catGPU, name,
					fmt.Sprintf("%s (%s) is at %.0f°C", name, g.Name, g.TemperatureGPU),
					g.TemperatureGPU, t.GPUTempCritC)
			case t.GPUTempWarnC > 0 && g.TemperatureGPU >= t.GPUTempWarnC:
				b.add(RuleGPUTemp, types.SeverityWarning, catGPU, name,
					fmt.Sprintf("%s (%s) is at %.0f°C", name, g.Name, g.TemperatureGPU),
					g.TemperatureGPU, t.GPUTempWarnC)
			}
		}

		// Exhausted video memory fails work outright rather than slowing it
		// down, so it is worth reporting even though high usage is normal.
		if t.GPUMemoryPct > 0 && g.MemoryTotalMiB > 0 && g.MemoryPercent >= t.GPUMemoryPct {
			b.add(RuleGPUMemory, types.SeverityWarning, catGPU, name,
				fmt.Sprintf("%s memory is %.0f%% used (%d of %d MiB)",
					name, g.MemoryPercent, g.MemoryUsedMiB, g.MemoryTotalMiB),
				g.MemoryPercent, t.GPUMemoryPct)
		}

		if t.GPUPowerPct > 0 && g.PowerLimitW > 0 {
			if pct := g.PowerDrawW / g.PowerLimitW * 100; pct >= t.GPUPowerPct {
				b.add(RuleGPUPower, types.SeverityWarning, catGPU, name,
					fmt.Sprintf("%s is drawing %.0f W, %.0f%% of its %.0f W limit",
						name, g.PowerDrawW, pct, g.PowerLimitW),
					pct, t.GPUPowerPct)
			}
		}
	}
}

// evalVirt covers containers and the container runtime.
func evalVirt(b *builder, snap *types.Snapshot) {
	t := b.cfg

	// A runtime that was detected but stopped answering hides every container
	// behind it, so the dashboard would show an idle host rather than an
	// unobservable one.
	if snap.Virt.Runtime.Engine != "" && !snap.Virt.Capability.RuntimeAPIReachable {
		b.add(RuleRuntimeDown, types.SeverityWarning, catVirt, snap.Virt.Runtime.Engine,
			fmt.Sprintf("the %s runtime is not answering on %s",
				snap.Virt.Runtime.Engine, snap.Virt.Runtime.SocketPath), 0, 0)
	}

	for i := range snap.Virt.Containers {
		c := &snap.Virt.Containers[i]

		// The container id, not its name, is the identity. Names are neither
		// unique nor stable -- a host can easily run several containers called
		// "sleep" -- and keying an alert on one would collide their ids, so a
		// second container's condition would overwrite the first's first-seen
		// time and the two would be indistinguishable in the UI. The readable
		// name goes in the message instead.
		id := c.ShortID
		if id == "" {
			id = c.Name
		}
		label := c.Name
		if label == "" {
			label = id
		}

		// Only meaningful against a limit: without one the percentage is of
		// the whole host and says nothing about the container approaching a
		// wall.
		if t.ContainerMemPct > 0 && c.MemoryLimitBytes > 0 && c.MemoryPercent >= t.ContainerMemPct {
			b.add(RuleContainerMem, types.SeverityWarning, catContainers, id,
				fmt.Sprintf("container %s (%s) is at %.0f%% of its memory limit", label, id, c.MemoryPercent),
				c.MemoryPercent, t.ContainerMemPct)
		}

		// CPUPercent sums across cores, so a busy container on a large host
		// routinely reads several hundred percent. Comparing that to a
		// percentage threshold directly made the rule fire for any multi-
		// threaded workload -- 504% on a 24-thread host is 21% of the machine,
		// which is unremarkable. The share of the whole host is what an
		// operator actually cares about, so that is what is compared.
		if t.ContainerCPUPct > 0 && snap.CPUSummary.TotalThreads > 0 {
			hostShare := c.CPUPercent / float64(snap.CPUSummary.TotalThreads)
			if hostShare >= t.ContainerCPUPct {
				b.add(RuleContainerCPU, types.SeverityWarning, catContainers, id,
					fmt.Sprintf("container %s (%s) is using %.0f%% of the host's CPU", label, id, hostShare),
					hostShare, t.ContainerCPUPct)
			}
		}
	}
}
