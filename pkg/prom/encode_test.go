package prom

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// goldenSeries is the exact input that produced testdata/write_request.golden.hex.
//
// It is chosen to pin the parts of the encoding that are easy to get wrong and
// invisible when wrong: two series so the repeated field is exercised, two
// samples in one of them so a series carries a sample list rather than a
// single value, a label set of a different length in the second so no offset
// can be shared between them, and -- the important one -- a sample whose value
// is exactly 0, which proto3 omits entirely.
func goldenSeries() []Series {
	return []Series{
		{
			Labels: []Label{
				{Name: "__name__", Value: "sysmon_cpu_usage_percent"},
				{Name: "instance", Value: "mon0"},
				{Name: "job", Value: "go-sysmon"},
			},
			Samples: []Sample{
				{Value: 12.5, TimeMs: 1788300000000},
				{Value: 0, TimeMs: 1788300001000},
			},
		},
		{
			Labels: []Label{
				{Name: "__name__", Value: "sysmon_disk_used_percent"},
				{Name: "device", Value: "nvme0n1"},
				{Name: "instance", Value: "mon0"},
				{Name: "job", Value: "go-sysmon"},
			},
			Samples: []Sample{{Value: 91.25, TimeMs: 1788300000000}},
		},
	}
}

// TestEncodeWriteRequest_MatchesPrompbGolden is the oracle for the hand-written
// encoder.
//
// The golden bytes were produced by the real prompb encoder from the identical
// input -- see testdata/gen_golden.go.txt -- so this compares against the
// Prometheus library rather than against a second copy of the logic in this
// package, which would prove nothing about the wire format.
func TestEncodeWriteRequest_MatchesPrompbGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/write_request.golden.hex")
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	want, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decoding golden: %v", err)
	}

	got := encodeWriteRequest(goldenSeries())
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("encoded WriteRequest does not match the prompb encoder\n got: %s\nwant: %s",
			hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// TestEncodeWriteRequest_Empty covers the degenerate input the exporter can
// reach when a flush finds nothing buffered: it must produce a valid empty
// request rather than a nil body or a malformed one.
func TestEncodeWriteRequest_Empty(t *testing.T) {
	if got := encodeWriteRequest(nil); len(got) != 0 {
		t.Fatalf("encodeWriteRequest(nil) = %x, want empty", got)
	}
	if got := encodeWriteRequest([]Series{}); len(got) != 0 {
		t.Fatalf("encodeWriteRequest([]) = %x, want empty", got)
	}
}

// TestEncodeWriteRequest_OmitsZeroFields states the proto3 omission rule
// outright, because "the value field is missing" is the kind of thing a reader
// of a hex dump reasonably mistakes for a bug.
func TestEncodeWriteRequest_OmitsZeroFields(t *testing.T) {
	tests := []struct {
		name   string
		series []Series
		want   string
	}{
		{
			name: "a zero value carries no value field",
			// samples{ timestamp: 1 } only: 12 02 10 01
			series: []Series{{Samples: []Sample{{Value: 0, TimeMs: 1}}}},
			want:   "0a04" + "1202" + "1001",
		},
		{
			name: "a zero timestamp carries no timestamp field",
			// samples{ value: 1.0 } only: 12 09 09 <8 bytes>
			series: []Series{{Samples: []Sample{{Value: 1, TimeMs: 0}}}},
			want:   "0a0b" + "1209" + "09000000000000f03f",
		},
		{
			name:   "an empty label value carries no value field",
			series: []Series{{Labels: []Label{{Name: "n", Value: ""}}}},
			want:   "0a05" + "0a03" + "0a016e",
		},
		{
			name:   "a wholly zero sample encodes as an empty message",
			series: []Series{{Samples: []Sample{{}}}},
			want:   "0a02" + "1200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hex.EncodeToString(encodeWriteRequest(tt.series)); got != tt.want {
				t.Fatalf("encodeWriteRequest() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestEncodeWriteRequest_LongStringsUseMultiByteLengths guards the size
// computation. Every label in the golden fixture is short enough for a
// one-byte length, so a varintLen that always returned 1 would pass every test
// above while producing a body whose declared lengths are short by one byte
// per long label -- garbage that Prometheus rejects at ingest.
func TestEncodeWriteRequest_LongStringsUseMultiByteLengths(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := encodeWriteRequest([]Series{{Labels: []Label{{Name: "n", Value: long}}}})

	// label{ name:"n" value:<200 bytes> } is 3 + 3 + 200 = 206 bytes, whose
	// length needs two varint bytes -- so the series wrapping it is 209, not
	// 208, and that extra byte is exactly what a one-byte-length bug loses.
	want := "0a" + "d101" + // timeseries, length 209
		"0a" + "ce01" + // labels, length 206
		"0a016e" + // name "n"
		"12" + "c801" + hex.EncodeToString([]byte(long)) // value, length 200

	if hex.EncodeToString(got) != want {
		t.Fatalf("encodeWriteRequest() = %s, want %s", hex.EncodeToString(got), want)
	}
}

func TestVarintLen(t *testing.T) {
	tests := []struct {
		v    uint64
		want int
	}{
		{0, 1}, {1, 1}, {127, 1}, {128, 2}, {16383, 2}, {16384, 3},
		{1788300000000, 6}, {^uint64(0), 10},
	}
	for _, tt := range tests {
		if got := varintLen(tt.v); got != tt.want {
			t.Errorf("varintLen(%d) = %d, want %d", tt.v, got, tt.want)
		}
	}
}
