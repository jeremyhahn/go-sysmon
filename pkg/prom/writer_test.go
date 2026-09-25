package prom

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
)

// capture records what a remote-write receiver saw.
type capture struct {
	requests int
	headers  http.Header
	body     []byte
}

// receiver returns a server that records one request and answers with status.
func receiver(t *testing.T, status int, message string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.requests++
		got.headers = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		got.body = body
		if status != http.StatusOK {
			http.Error(w, message, status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestWriter_Write_PostsSnappyProtobuf checks the whole request a receiver
// sees, not just that one was made: the headers are the protocol's, and the
// body is the golden protobuf under snappy. A receiver rejects any of these
// individually, and the rejection would only show up in production.
func TestWriter_Write_PostsSnappyProtobuf(t *testing.T) {
	srv, got := receiver(t, http.StatusOK, "")

	w := NewWriter(srv.URL, 5*time.Second, discardLog)
	if err := w.Write(context.Background(), goldenSeries()); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}

	if got.requests != 1 {
		t.Fatalf("requests = %d, want 1", got.requests)
	}
	for header, want := range map[string]string{
		"Content-Type":                      "application/x-protobuf",
		"Content-Encoding":                  "snappy",
		"X-Prometheus-Remote-Write-Version": "0.1.0",
	} {
		if v := got.headers.Get(header); v != want {
			t.Errorf("%s = %q, want %q", header, v, want)
		}
	}

	decoded, err := snappy.Decode(nil, got.body)
	if err != nil {
		t.Fatalf("body is not snappy: %v", err)
	}
	if want := hex.EncodeToString(encodeWriteRequest(goldenSeries())); hex.EncodeToString(decoded) != want {
		t.Fatalf("decoded body = %s, want %s", hex.EncodeToString(decoded), want)
	}
}

// TestWriter_Write_EmptyBatchMakesNoRequest: an empty WriteRequest is a valid
// request that costs a round trip and stores nothing, and the exporter reaches
// this case on every flush that found nothing to send.
func TestWriter_Write_EmptyBatchMakesNoRequest(t *testing.T) {
	srv, got := receiver(t, http.StatusOK, "")

	w := NewWriter(srv.URL, time.Second, discardLog)
	if err := w.Write(context.Background(), nil); err != nil {
		t.Fatalf("Write(nil) = %v, want nil", err)
	}
	if got.requests != 0 {
		t.Fatalf("requests = %d, want 0", got.requests)
	}
}

// TestWriter_Write_RejectionIsTyped covers the classification the exporter's
// retry policy depends on. Getting it backwards is silent in both directions:
// retrying a 400 forever blocks every later batch behind a request that can
// never succeed, and dropping a 503 discards history a healthy receiver would
// have taken a moment later.
func TestWriter_Write_RejectionIsTyped(t *testing.T) {
	tests := []struct {
		status        int
		wantRetryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
	}

	for _, tt := range tests {
		srv, _ := receiver(t, tt.status, "sample out of order")
		w := NewWriter(srv.URL, time.Second, discardLog)

		err := w.Write(context.Background(), goldenSeries())
		var rw *RemoteWriteError
		if !errors.As(err, &rw) {
			t.Fatalf("status %d: Write() = %v, want *RemoteWriteError", tt.status, err)
		}
		if rw.Status != tt.status {
			t.Errorf("status %d: RemoteWriteError.Status = %d", tt.status, rw.Status)
		}
		if rw.Retryable() != tt.wantRetryable {
			t.Errorf("status %d: Retryable() = %v, want %v", tt.status, rw.Retryable(), tt.wantRetryable)
		}
		if !strings.Contains(rw.Error(), "sample out of order") {
			t.Errorf("status %d: error %q does not carry the receiver's explanation", tt.status, rw.Error())
		}
	}
}

// TestWriter_Write_UnreachableEndpoint: a receiver that cannot be reached at
// all is a different error from one that answered no, and the exporter treats
// it as retryable by default rather than dropping the batch.
func TestWriter_Write_UnreachableEndpoint(t *testing.T) {
	w := NewWriter(deadpeer.URL(t)+"/api/v1/write", 500*time.Millisecond, discardLog)

	err := w.Write(context.Background(), goldenSeries())
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("Write() = %v, want *RequestError", err)
	}
	var rw *RemoteWriteError
	if errors.As(err, &rw) {
		t.Fatal("an unreachable endpoint must not look like a rejection")
	}
}

func TestWriter_Write_MalformedURL(t *testing.T) {
	w := NewWriter("http://\x7f/write", time.Second, discardLog)
	if err := w.Write(context.Background(), goldenSeries()); err == nil {
		t.Fatal("Write() to a malformed URL = nil, want an error")
	}
}

// TestBuilder_GroupsSamplesBySeries is the property that keeps a flush small:
// several collections of the same metrics must produce one series carrying
// several samples, not one series per sample repeating the whole label set.
func TestBuilder_GroupsSamplesBySeries(t *testing.T) {
	b := NewBuilder([]Label{{Name: "instance", Value: "mon0"}})
	b.Add("sysmon_cpu_usage_percent", 10, 1000)
	b.Add("sysmon_cpu_usage_percent", 20, 2000)
	b.Add("sysmon_disk_used_percent", 30, 1000, "device", "sda")
	b.Add("sysmon_disk_used_percent", 40, 2000, "device", "sda")
	b.Add("sysmon_disk_used_percent", 50, 1000, "device", "sdb")

	if b.Len() != 5 {
		t.Fatalf("Len() = %d, want 5", b.Len())
	}
	series := b.Drain()
	if len(series) != 3 {
		t.Fatalf("Drain() returned %d series, want 3 (cpu, sda, sdb)", len(series))
	}
	for _, s := range series {
		want := 2
		if s.Labels[labelIndex(t, s, "device")].Value == "sdb" {
			want = 1
		}
		if len(s.Samples) != want {
			t.Errorf("series %v has %d samples, want %d", s.Labels, len(s.Samples), want)
		}
	}

	if b.Len() != 0 || len(b.Drain()) != 0 {
		t.Fatal("Drain() must reset the builder")
	}
}

// labelIndex finds a label, tolerating its absence by returning the __name__
// slot, which never matches the value being compared.
func labelIndex(t *testing.T, s Series, name string) int {
	t.Helper()
	for i := range s.Labels {
		if s.Labels[i].Name == name {
			return i
		}
	}
	return 0
}

// TestBuilder_SortsAndStampsLabels covers the two things Prometheus rejects a
// series for: an unsorted label set, and a missing __name__.
func TestBuilder_SortsAndStampsLabels(t *testing.T) {
	b := NewBuilder([]Label{{Name: "job", Value: "go-sysmon"}, {Name: "instance", Value: "mon0"}})
	b.Add("sysmon_power_watts", 4, 1, "source", "cpu:0", "arch", "x86")

	series := b.Drain()
	if len(series) != 1 {
		t.Fatalf("Drain() returned %d series, want 1", len(series))
	}
	got := make([]string, 0, len(series[0].Labels))
	for _, l := range series[0].Labels {
		got = append(got, l.Name+"="+l.Value)
	}
	want := []string{
		"__name__=sysmon_power_watts",
		"arch=x86",
		"instance=mon0",
		"job=go-sysmon",
		"source=cpu:0",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("labels = %v, want %v (sorted by name, with __name__ present)", got, want)
	}
}

// TestBuilder_OddLabelIsIgnored: the label pairs come from the same variadic
// shape the text exposition writer uses, and a caller that passes a name with
// no value must not produce a label with an empty name -- which Prometheus
// rejects for the whole request, losing every other sample in the batch.
func TestBuilder_OddLabelIsIgnored(t *testing.T) {
	b := NewBuilder(nil)
	b.Add("sysmon_up", 1, 1, "device")

	series := b.Drain()
	if len(series[0].Labels) != 1 || series[0].Labels[0].Name != "__name__" {
		t.Fatalf("labels = %v, want only __name__", series[0].Labels)
	}
}

// TestBuilder_AnEmptyLabelValueIsLeftOff: a host-wide alert has no resource,
// and resource="" would put an empty label value on the wire, which a receiver
// may reject together with the whole batch. It is left off, which is what the
// empty value means to Prometheus, while every labelled value is kept.
func TestBuilder_AnEmptyLabelValueIsLeftOff(t *testing.T) {
	b := NewBuilder([]Label{{Name: "instance", Value: "node-1"}})
	b.Add("sysmon_alert", 1, 1, "rule", "cpu_high", "resource", "", "severity", "warning")

	series := b.Drain()
	want := []Label{
		{Name: "__name__", Value: "sysmon_alert"}, {Name: "instance", Value: "node-1"},
		{Name: "rule", Value: "cpu_high"}, {Name: "severity", Value: "warning"},
	}
	if len(series) != 1 || !reflect.DeepEqual(series[0].Labels, want) {
		t.Fatalf("Drain() = %+v, want one series labelled %v", series, want)
	}
	for _, l := range series[0].Labels {
		if l.Value == "" {
			t.Errorf("label %q is sent with an empty value", l.Name)
		}
	}
}

// TestBuilder_AnEmptyValueAndAnAbsentLabelAreOneSeries: the two are the same
// series to Prometheus, so they must not become two here.
func TestBuilder_AnEmptyValueAndAnAbsentLabelAreOneSeries(t *testing.T) {
	b := NewBuilder(nil)
	b.Add("m", 1, 1, "rule", "load_high", "resource", "")
	b.Add("m", 2, 2, "rule", "load_high")

	if series := b.Drain(); len(series) != 1 || len(series[0].Samples) != 2 {
		t.Fatalf("Drain() = %+v, want one series with both samples", series)
	}
}

// TestBuilder_ASecondSampleAtOneTimeIsDroppedAndReported: two samples of one
// series at one timestamp are a labelling defect upstream, and sent together
// they are a duplicate the receiver refuses with the whole request. The first
// stands, the second is dropped and reported once, and a later timestamp is
// kept as usual.
func TestBuilder_ASecondSampleAtOneTimeIsDroppedAndReported(t *testing.T) {
	b := NewBuilder([]Label{{Name: "instance", Value: "n"}})
	b.Add("sysmon_cpu_temperature_celsius", 61, 1000, "core", "0")
	b.Add("sysmon_cpu_temperature_celsius", 67, 1000, "core", "0")

	want := []string{`{__name__="sysmon_cpu_temperature_celsius",core="0",instance="n"}`}
	if got := b.TakeCollisions(); !reflect.DeepEqual(got, want) {
		t.Errorf("TakeCollisions() = %q, want %q", got, want)
	}
	if got := b.TakeCollisions(); len(got) != 0 {
		t.Errorf("TakeCollisions() again = %q, want them forgotten", got)
	}
	b.Add("sysmon_cpu_temperature_celsius", 62, 2000, "core", "0")

	series := b.Drain()
	if len(series) != 1 || !reflect.DeepEqual(series[0].Samples, []Sample{{Value: 61, TimeMs: 1000}, {Value: 62, TimeMs: 2000}}) {
		t.Fatalf("Drain() = %+v, want the first sample at 1000 and the one at 2000", series)
	}
	if b.Len() != 0 {
		t.Errorf("Len() after Drain = %d, want 0", b.Len())
	}
}

// TestBuilder_SameLabelsDifferentOrderIsOneSeries: the label set identifies the
// series, so two calls that name the same labels in a different order have to
// land in one series rather than two that Prometheus would see as duplicates.
func TestBuilder_SameLabelsDifferentOrderIsOneSeries(t *testing.T) {
	b := NewBuilder(nil)
	b.Add("m", 1, 1, "a", "1", "b", "2")
	b.Add("m", 2, 2, "b", "2", "a", "1")

	if series := b.Drain(); len(series) != 1 || len(series[0].Samples) != 2 {
		t.Fatalf("Drain() = %+v, want one series with two samples", series)
	}
}

// TestBuilder_DistinctSetsDoNotCollide guards the map key. Concatenating names
// and values without separators would make {ab="c"} and {a="bc"} the same
// series, silently merging two metrics into one line.
func TestBuilder_DistinctSetsDoNotCollide(t *testing.T) {
	b := NewBuilder(nil)
	b.Add("m", 1, 1, "ab", "c")
	b.Add("m", 2, 1, "a", "bc")

	if series := b.Drain(); len(series) != 2 {
		t.Fatalf("Drain() returned %d series, want 2", len(series))
	}
}

// TestBuilder_NeverRepeatsALabel.
//
// Prometheus identifies a series by its label set and rejects one carrying a
// repeated label -- rejecting the WHOLE write request, so a single such series
// loses every other sample sent with it. An operator whose external_labels
// happen to include "device" would therefore stop the node's entire history,
// and only the metrics using that label would show it, which is the hardest
// version of the bug to find. Configuration rejects the overlap up front; this
// is the second line, for callers outside this repository.
func TestBuilder_NeverRepeatsALabel(t *testing.T) {
	tests := []struct {
		name   string
		common []Label
		labels []string
		want   map[string]string
	}{
		{
			name:   "a common label the metric also sets",
			common: []Label{{Name: "device", Value: "from-external-labels"}, {Name: "instance", Value: "mon0"}},
			labels: []string{"device", "sda"},
			// The metric's own label wins: it is the one that means something
			// on this series.
			want: map[string]string{"__name__": "m", "device": "sda", "instance": "mon0"},
		},
		{
			name:   "a common label naming the metric",
			common: []Label{{Name: "__name__", Value: "hijacked"}},
			labels: nil,
			want:   map[string]string{"__name__": "m"},
		},
		{
			name:   "the metric setting one label twice",
			common: nil,
			labels: []string{"device", "sda", "device", "sdb"},
			want:   map[string]string{"__name__": "m", "device": "sda"},
		},
		{
			name:   "a duplicate inside the common set",
			common: []Label{{Name: "site", Value: "rack_3"}, {Name: "site", Value: "rack_4"}},
			labels: nil,
			want:   map[string]string{"__name__": "m", "site": "rack_3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuilder(tt.common)
			b.Add("m", 1, 1, tt.labels...)

			series := b.Drain()
			if len(series) != 1 {
				t.Fatalf("Drain() returned %d series, want 1", len(series))
			}

			got := make(map[string]string, len(series[0].Labels))
			for _, l := range series[0].Labels {
				if _, dup := got[l.Name]; dup {
					t.Fatalf("label %q appears twice: %+v", l.Name, series[0].Labels)
				}
				got[l.Name] = l.Value
			}
			if len(got) != len(tt.want) {
				t.Fatalf("labels = %v, want %v", got, tt.want)
			}
			for name, value := range tt.want {
				if got[name] != value {
					t.Errorf("label %q = %q, want %q", name, got[name], value)
				}
			}
		})
	}
}
