package prom

import (
	"encoding/binary"
	"math"
)

// Protobuf field numbers from the remote-write schema in the package comment.
const (
	fieldTimeseries = 1 // WriteRequest.timeseries
	fieldLabels     = 1 // TimeSeries.labels
	fieldSamples    = 2 // TimeSeries.samples
	fieldLabelName  = 1 // Label.name
	fieldLabelValue = 2 // Label.value
	fieldSampleVal  = 1 // Sample.value
	fieldSampleTime = 2 // Sample.timestamp
)

// Protobuf wire types. Only these three appear in the schema.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// encodeWriteRequest renders a WriteRequest.
//
// Lengths precede their contents on the wire, so each message is sized before
// it is written. Sizes are computed one level at a time -- a series is sized
// once, then written -- rather than recursively per element, which would make
// the whole encode quadratic in the number of samples.
//
// Zero-valued scalars are omitted, which is what proto3 specifies and what the
// reference encoder does. A sample whose value is exactly 0 therefore carries
// no value field; the decoder supplies the zero, so the sample arrives as 0
// either way. This looks like data loss and is not, which is why it is said
// here as well as pinned by the golden test.
func encodeWriteRequest(series []Series) []byte {
	sizes := make([]int, len(series))
	total := 0
	for i := range series {
		sizes[i] = seriesSize(&series[i])
		total += 1 + varintLen(uint64(sizes[i])) + sizes[i]
	}

	buf := make([]byte, 0, total)
	for i := range series {
		buf = appendTag(buf, fieldTimeseries, wireBytes)
		buf = appendVarint(buf, uint64(sizes[i]))
		buf = appendSeries(buf, &series[i])
	}
	return buf
}

// seriesSize is the encoded length of one TimeSeries body.
func seriesSize(s *Series) int {
	n := 0
	for i := range s.Labels {
		size := labelSize(&s.Labels[i])
		n += 1 + varintLen(uint64(size)) + size
	}
	for i := range s.Samples {
		size := sampleSize(&s.Samples[i])
		n += 1 + varintLen(uint64(size)) + size
	}
	return n
}

func appendSeries(buf []byte, s *Series) []byte {
	for i := range s.Labels {
		buf = appendTag(buf, fieldLabels, wireBytes)
		buf = appendVarint(buf, uint64(labelSize(&s.Labels[i])))
		buf = appendLabel(buf, &s.Labels[i])
	}
	for i := range s.Samples {
		buf = appendTag(buf, fieldSamples, wireBytes)
		buf = appendVarint(buf, uint64(sampleSize(&s.Samples[i])))
		buf = appendSample(buf, &s.Samples[i])
	}
	return buf
}

func labelSize(l *Label) int {
	n := 0
	if l.Name != "" {
		n += 1 + varintLen(uint64(len(l.Name))) + len(l.Name)
	}
	if l.Value != "" {
		n += 1 + varintLen(uint64(len(l.Value))) + len(l.Value)
	}
	return n
}

func appendLabel(buf []byte, l *Label) []byte {
	if l.Name != "" {
		buf = appendTag(buf, fieldLabelName, wireBytes)
		buf = appendVarint(buf, uint64(len(l.Name)))
		buf = append(buf, l.Name...)
	}
	if l.Value != "" {
		buf = appendTag(buf, fieldLabelValue, wireBytes)
		buf = appendVarint(buf, uint64(len(l.Value)))
		buf = append(buf, l.Value...)
	}
	return buf
}

func sampleSize(s *Sample) int {
	n := 0
	if s.Value != 0 {
		n += 1 + 8
	}
	if s.TimeMs != 0 {
		n += 1 + varintLen(uint64(s.TimeMs))
	}
	return n
}

func appendSample(buf []byte, s *Sample) []byte {
	if s.Value != 0 {
		buf = appendTag(buf, fieldSampleVal, wireFixed64)
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(s.Value))
	}
	if s.TimeMs != 0 {
		buf = appendTag(buf, fieldSampleTime, wireVarint)
		buf = appendVarint(buf, uint64(s.TimeMs))
	}
	return buf
}

// appendTag writes a field key. Every field number in this schema is 1 or 2,
// so the key is always a single byte, but it is encoded as a varint anyway
// rather than relying on that.
func appendTag(buf []byte, field, wire int) []byte {
	return appendVarint(buf, uint64(field)<<3|uint64(wire))
}

func appendVarint(buf []byte, v uint64) []byte {
	return binary.AppendUvarint(buf, v)
}

func varintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
