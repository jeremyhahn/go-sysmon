//go:build prometheus

// Tagged because SampleSink exists only in the prometheus build (#100).
package metrics

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/prom"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// richSnapshot exercises every branch of Write, so the comparisons below run
// over the whole metric surface rather than the handful a bare snapshot emits.
func richSnapshot() *types.Snapshot {
	return &types.Snapshot{
		Host:       types.HostInfo{Uptime: 3600},
		CPUs:       []types.CPUInfo{{UsagePercent: 10, UsageAvailable: true}, {UsagePercent: 30, UsageAvailable: true}},
		CPUSummary: types.CPUSummary{TotalThreads: 2},
		LoadAvg:    types.LoadAverage{Load1: 1, Load5: 2, Load15: 3},
		Memory: types.MemoryInfo{
			TotalBytes: 100, UsedBytes: 60, UsedPercent: 60, SwapPercent: 5,
			SwapInPages: 1204, SwapOutPages: 5310, SwapCountersAvailable: true,
			PageSizeBytes: 16384,
		},
		Sensors: types.SensorData{
			CoreTemps:    []types.CoreTemp{{CoreID: 0, TempCelsius: 44}},
			PackagePower: []types.PackagePower{{PackageName: "package-0", PowerW: 25}},
			PSI: types.PSIData{
				CPU:    types.PSIResource{SomeAvg10: 1, FullAvg10: 0.5},
				Memory: types.PSIResource{SomeAvg10: 2, FullAvg10: 1},
				IO:     types.PSIResource{SomeAvg10: 3, FullAvg10: 1.5},
			},
		},
		GPUs: []types.GPUInfo{{Index: 0, GPUUtilPercent: 55, MemoryPercent: 70, TemperatureGPU: 60, PowerDrawW: 120}},
		// An awake accelerator doing no work: residency high, utilisation zero.
		// That combination is the one the two series exist to tell apart, so it
		// is the one the fixture carries.
		NPUs: []types.NPUInfo{{
			Index: 0, Device: "accel0", Driver: "intel_vpu", State: "active",
			ResidencyPercent: 97.5, UtilPercent: 0, ClientCount: 1,
			TemperatureC: 41, PowerDrawW: 1.8, FrequencyMHz: 950,
			CountersStale: true, CountersAgeSec: 12.5, BusyTimeUs: 1_234_567,
			Engines: []types.NPUEngine{{Name: "npu", BusyNs: 5_000, BusyPercent: 0}},
		}},
		Disks: []types.DiskInfo{
			{
				Name: "nvme0n1", TotalBytes: 1000, UsedPercent: 40, UtilPercent: 12,
				AvgQueueLength: 1.5, ReadBytesRate: 100, WriteBytesRate: 200, Temperature: 38,
				SMARTEnabled: true, SMARTHealthy: true, PowerOnHours: 500,
				WearLevelPercent: 3, AvailableSparePercent: 100,
			},
			{
				Name: "md0", TotalBytes: 2000, UsedPercent: 10,
				RAID: &types.RAIDInfo{Level: "raid1", Degraded: false, ActiveDevices: 2, TotalDevices: 2},
			},
		},
		DriveSlots: types.DriveSlots{
			Available: true, Source: types.DriveSlotSourceRedfish,
			Slots: []types.DriveSlot{{
				ID: "Disk.Bay.3", Slot: "Bay 3", Present: true, Health: types.DriveSlotHealthCritical,
				Fault: true, MediaLifeLeftPercent: 3, MediaLifeReported: true,
			}},
		},
		Networks: []types.NetworkInfo{
			{Name: "eno1", IsUp: true, BytesRecvRate: 10, BytesSentRate: 20, ErrorsIn: 1, DropsOut: 2},
			{Name: "lo", IsLoopback: true, IsUp: true},
		},
		Alerts: []types.Alert{
			{ID: "a", Rule: "disk_full", Severity: types.SeverityCritical, Resource: "/", Category: "storage"},
			{ID: "b", Rule: "cpu_high", Severity: types.SeverityWarning, Resource: "cpu", Category: "cpu"},
		},
	}
}

var seriesLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)`)

// textNames extracts the metric names from an exposition body, ignoring the
// HELP and TYPE preamble.
func textNames(body string) []string {
	seen := make(map[string]bool)
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := seriesLine.FindStringSubmatch(line); m != nil {
			seen[m[1]] = true
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSampleSink_DescribesTheSameMetricsAsTheText is the guarantee the package
// exists for.
//
// A scraped node and a pushed node have to report the same thing. They would
// diverge silently: the exposition endpoint would keep working, remote write
// would keep succeeding, and only a chart drawn from history would quietly be
// missing a series that /metrics still shows. Both paths go through Write, and
// this checks that the sink in front of each one preserves that.
func TestSampleSink_DescribesTheSameMetricsAsTheText(t *testing.T) {
	snap := richSnapshot()

	text := &TextWriter{}
	Write(text, snap)

	b := prom.NewBuilder(nil)
	Write(SampleSink{Builder: b, TimeMs: 1000}, snap)

	pushed := make(map[string]bool)
	for _, s := range b.Drain() {
		for _, l := range s.Labels {
			if l.Name == "__name__" {
				pushed[l.Value] = true
			}
		}
	}

	scraped := textNames(text.String())
	if len(scraped) == 0 {
		t.Fatal("the exposition body named no metrics; the fixture is not exercising Write")
	}
	for _, name := range scraped {
		if !pushed[name] {
			t.Errorf("%s appears on /metrics but is never pushed", name)
		}
	}
	for _, name := range sortedKeys(pushed) {
		if !contains(scraped, name) {
			t.Errorf("%s is pushed but never appears on /metrics", name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestSampleSink_CarriesLabelsAndTimestamp: the labels are what separate one
// device's series from another's, and the single timestamp is what makes a
// snapshot arrive as one instant rather than smeared over the collection.
func TestSampleSink_CarriesLabelsAndTimestamp(t *testing.T) {
	b := prom.NewBuilder([]prom.Label{{Name: "instance", Value: "mon0"}})
	Write(SampleSink{Builder: b, TimeMs: 4242}, richSnapshot())

	var perDevice int
	for _, s := range b.Drain() {
		name, device := "", ""
		for _, l := range s.Labels {
			switch l.Name {
			case "__name__":
				name = l.Value
			case "device":
				device = l.Value
			case "instance":
				if l.Value != "mon0" {
					t.Errorf("instance = %q, want mon0", l.Value)
				}
			}
		}
		for _, sample := range s.Samples {
			if sample.TimeMs != 4242 {
				t.Fatalf("%s sample timestamp = %d, want the single instant 4242", name, sample.TimeMs)
			}
		}
		if device == "nvme0n1" {
			perDevice++
		}
	}
	if perDevice == 0 {
		t.Fatal("no series carried device=nvme0n1; per-device labels were lost")
	}
}

// TestSampleSink_InstanceStampsEverySeries covers the aggregator's write path:
// a peer's reading has to carry the peer's name, or ten machines' histories
// merge into one series that no later query can separate.
func TestSampleSink_InstanceStampsEverySeries(t *testing.T) {
	b := prom.NewBuilder([]prom.Label{{Name: "instance", Value: "mon0"}, {Name: "job", Value: "go-sysmon"}})
	sink := SampleSink{Builder: b, TimeMs: 1700000000000, Instance: "omega"}

	sink.Gauge("sysmon_up", "", 1)
	sink.Gauge("sysmon_disk_used_percent", "", 55, "device", "sda")

	for _, series := range b.Drain() {
		var instance, device string
		for _, l := range series.Labels {
			switch l.Name {
			case "instance":
				instance = l.Value
			case "device":
				device = l.Value
			}
		}
		if instance != "omega" {
			t.Errorf("series %v carries instance %q, want omega", series.Labels, instance)
		}
		if device != "" && device != "sda" {
			t.Errorf("the metric's own label was lost: device = %q", device)
		}
	}
}

// TestSampleSink_WithoutInstanceKeepsTheNodesOwn is the unchanged path: a node
// writing its own readings must not start labelling them, or every existing
// series would move to a new one.
func TestSampleSink_WithoutInstanceKeepsTheNodesOwn(t *testing.T) {
	b := prom.NewBuilder([]prom.Label{{Name: "instance", Value: "mon0"}})
	SampleSink{Builder: b, TimeMs: 1700000000000}.Gauge("sysmon_up", "", 1)

	series := b.Drain()
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	for _, l := range series[0].Labels {
		if l.Name == "instance" && l.Value != "mon0" {
			t.Errorf("instance = %q, want the node's own mon0", l.Value)
		}
	}
}

// TestSampleSink_DoesNotWriteIntoTheCallersLabels is the aliasing case. The
// caller owns the slice it passes; stamping the instance by appending into its
// spare capacity would write a label into memory the caller is still using.
func TestSampleSink_DoesNotWriteIntoTheCallersLabels(t *testing.T) {
	b := prom.NewBuilder(nil)
	labels := make([]string, 2, 8) // spare capacity is what makes this visible
	labels[0], labels[1] = "device", "sda"

	SampleSink{Builder: b, TimeMs: 1, Instance: "omega"}.Gauge("sysmon_disk_used_percent", "", 1, labels...)

	reused := labels[:cap(labels)]
	for i := 2; i < len(reused); i++ {
		if reused[i] != "" {
			t.Fatalf("the sink wrote %q into the caller's slice at index %d", reused[i], i)
		}
	}
}

// TestSampleSink_CounterIsPushedLikeAGauge: remote write carries samples and
// labels only, with no TYPE, so a counter arrives exactly as a gauge would --
// same name, value, single timestamp and instance stamping. What makes it a
// counter downstream is the _total name and the TYPE the scrape endpoint
// declares, and both of those are Write's job, not this sink's.
func TestSampleSink_CounterIsPushedLikeAGauge(t *testing.T) {
	b := prom.NewBuilder([]prom.Label{{Name: "instance", Value: "mon0"}})
	SampleSink{Builder: b, TimeMs: 1700000000000, Instance: "omega"}.
		Counter("sysmon_swap_out_pages_total", "", 5310, "k", "v")

	series := b.Drain()
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	got := make(map[string]string)
	for _, l := range series[0].Labels {
		got[l.Name] = l.Value
	}
	if got["__name__"] != "sysmon_swap_out_pages_total" || got["instance"] != "omega" || got["k"] != "v" {
		t.Errorf("labels = %v, want the counter's name, its own label and instance omega", got)
	}
	if len(series[0].Samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(series[0].Samples))
	}
	if s := series[0].Samples[0]; s.Value != 5310 || s.TimeMs != 1700000000000 {
		t.Errorf("sample = %+v, want value 5310 at 1700000000000", s)
	}
}

// TestSampleSink_PushesTheSwapCounters: with the counters readable, both are
// pushed. The scrape-versus-push test would catch one side dropping them, but
// only if the fixture carries them; this pins that it does.
func TestSampleSink_PushesTheSwapCounters(t *testing.T) {
	b := prom.NewBuilder(nil)
	Write(SampleSink{Builder: b, TimeMs: 1}, richSnapshot())

	pushed := make(map[string]float64)
	for _, s := range b.Drain() {
		for _, l := range s.Labels {
			if l.Name == "__name__" && len(s.Samples) > 0 {
				pushed[l.Value] = s.Samples[0].Value
			}
		}
	}
	if pushed["sysmon_swap_in_pages_total"] != 1204 || pushed["sysmon_swap_out_pages_total"] != 5310 {
		t.Errorf("swap counters pushed as in=%v out=%v, want 1204 and 5310",
			pushed["sysmon_swap_in_pages_total"], pushed["sysmon_swap_out_pages_total"])
	}
}
