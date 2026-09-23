package fleet_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/metrics"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// recordingSink captures every sample metrics.Write emits, as text.
type recordingSink struct {
	samples []string
}

func (r *recordingSink) Gauge(name, _ string, value float64, labels ...string) {
	r.samples = append(r.samples, fmt.Sprintf("gauge %s%v=%v", name, labels, value))
}

func (r *recordingSink) Counter(name, _ string, value float64, labels ...string) {
	r.samples = append(r.samples, fmt.Sprintf("counter %s%v=%v", name, labels, value))
}

// written is everything metrics.Write records for snap, sorted.
func written(snap *types.Snapshot) []string {
	sink := &recordingSink{}
	metrics.Write(sink, snap)
	sort.Strings(sink.samples)
	return sink.samples
}

// filled returns a Snapshot with every exported field set to a distinct
// non-zero value and every list holding two populated elements, by reflection
// over the type -- so a field added to the snapshot tomorrow is filled without
// anyone remembering to.
func filled(t *testing.T) *types.Snapshot {
	t.Helper()
	var snap types.Snapshot
	seq := 0
	fill(reflect.ValueOf(&snap).Elem(), &seq, 0)
	return &snap
}

var timeType = reflect.TypeOf(time.Time{})

func fill(v reflect.Value, seq *int, depth int) {
	if depth > 12 {
		return
	}
	*seq++
	n := *seq
	switch {
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(time.Date(2026, 9, 22, 12, 0, n%60, 0, time.UTC)))
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(n%100 + 1))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n%100 + 1))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(n%97) + 0.5)
	case reflect.String:
		v.SetString(fmt.Sprintf("s%d", n))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), seq, depth+1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range 2 {
			fill(v.Index(i), seq, depth+1)
		}
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), seq, depth+1)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), seq, depth+1)
			}
		}
	}
}

// TestHistorySnapshot_KeepsEverythingHistoryRecords is the guard the owner's
// ruling asked for: a field metrics.Write reads cannot be stripped from the
// history payload without this failing.
//
// It is driven by what metrics.Write DOES, not by a list of what it reads. The
// snapshot is filled by reflection -- every field, every list populated -- and
// written twice: whole, and as HistorySnapshot slims it. Whatever the two
// produce differently is a series the aggregator would stop recording for its
// peers. If metrics.Write starts reading the process list or a CPU's flags, or
// HistorySnapshot starts removing something metrics.Write reads, the outputs
// differ and this names the samples that went missing.
func TestHistorySnapshot_KeepsEverythingHistoryRecords(t *testing.T) {
	full := filled(t)
	if len(full.Processes.Processes) == 0 || len(full.CPUs) == 0 || len(full.CPUs[0].Flags) == 0 {
		t.Fatal("the fixture did not populate what HistorySnapshot removes, so it would prove nothing")
	}

	want := written(full)
	if len(want) < 50 {
		t.Fatalf("metrics.Write emitted only %d samples for a fully populated snapshot; "+
			"the fixture is not reaching it", len(want))
	}
	got := written(fleet.HistorySnapshot(full))

	if !reflect.DeepEqual(got, want) {
		missing := difference(want, got)
		extra := difference(got, want)
		t.Errorf("history payload records differently from the whole snapshot:\n"+
			"  lost (%d): %s\n  gained (%d): %s\n"+
			"metrics.Write reads a field HistorySnapshot removes; stop removing it",
			len(missing), strings.Join(first(missing, 5), "; "),
			len(extra), strings.Join(first(extra, 5), "; "))
	}
}

// TestHistorySnapshot_RemovesOnlyTheProcessListAndCPUFlags pins the other
// side: what the payload drops is exactly what its documentation says. Put the
// two back and the result is the original, field for field.
func TestHistorySnapshot_RemovesOnlyTheProcessListAndCPUFlags(t *testing.T) {
	full := filled(t)
	before, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	slim := fleet.HistorySnapshot(full)
	if len(slim.Processes.Processes) != 0 {
		t.Errorf("process list kept: %d entries", len(slim.Processes.Processes))
	}
	for i, c := range slim.CPUs {
		if len(c.Flags) != 0 {
			t.Errorf("cpu %d kept %d flags", i, len(c.Flags))
		}
	}
	if slim.Processes.Processes == nil || slim.CPUs[0].Flags == nil {
		t.Error("a removed list is nil; it must encode as [], never null (#151)")
	}
	if slim.Processes.Total != full.Processes.Total {
		t.Errorf("process counts removed with the list: total %d, want %d", slim.Processes.Total, full.Processes.Total)
	}

	// The input is the monitor's published snapshot, shared with every other
	// reader, so slimming it must not touch it.
	after, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("HistorySnapshot modified the snapshot it was given")
	}

	restored := *slim
	restored.Processes.Processes = full.Processes.Processes
	restored.CPUs = make([]types.CPUInfo, len(slim.CPUs))
	for i := range slim.CPUs {
		restored.CPUs[i] = slim.CPUs[i]
		restored.CPUs[i].Flags = full.CPUs[i].Flags
	}
	if !reflect.DeepEqual(&restored, full) {
		t.Error("with the process list and CPU flags put back, the payload differs from the snapshot; " +
			"HistorySnapshot removed something else")
	}

	if fleet.HistorySnapshot(nil) != nil {
		t.Error("HistorySnapshot(nil) is not nil")
	}
}

func difference(a, b []string) []string {
	in := make(map[string]int, len(b))
	for _, s := range b {
		in[s]++
	}
	var out []string
	for _, s := range a {
		if in[s] > 0 {
			in[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}

func first(s []string, n int) []string {
	return s[:min(n, len(s))]
}
