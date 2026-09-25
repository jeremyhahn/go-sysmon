package prom

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"

	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
)

// remoteWriteVersion is the protocol version header remote-write receivers
// expect. It has been 0.1.0 since the endpoint existed and is not the version
// of anything in this repository.
const remoteWriteVersion = "0.1.0"

// errorBodyLimit bounds how much of a rejected write's response body is read
// back for the error message.
const errorBodyLimit = 512

// Writer posts samples to a Prometheus remote-write endpoint.
//
// It is stateless and safe for concurrent use: the batching, the retry policy
// and the buffer all live in Exporter, so a caller that wants to push one set
// of samples and handle its own failures can use this directly.
type Writer struct {
	url    string
	client *http.Client
	log    *slog.Logger
}

// NewWriter returns a Writer posting to url, bounding each request by timeout.
// log receives what cannot be returned: a response body that failed to drain
// after the write's outcome was already known.
func NewWriter(url string, timeout time.Duration, log *slog.Logger) *Writer {
	return &Writer{
		url: url,
		log: log,
		// Not a default client. A remote-write receiver that answers 302 could
		// otherwise move this node's metrics to another host, or walk an https
		// endpoint down to plaintext, with the write still reported as having
		// succeeded to the URL in the config file.
		client: outbound.NewClient(timeout),
	}
}

// URL returns the endpoint this writer posts to.
func (w *Writer) URL() string { return w.url }

// Write sends one batch. An empty batch is not sent at all: an empty
// WriteRequest is a valid request that costs a round trip and stores nothing.
func (w *Writer) Write(ctx context.Context, series []Series) error {
	if len(series) == 0 {
		return nil
	}

	body := snappy.Encode(nil, encodeWriteRequest(series))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return &RequestError{URL: w.url, Err: err}
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", remoteWriteVersion)
	req.Header.Set("User-Agent", "go-sysmon")

	resp, err := w.client.Do(req)
	if err != nil {
		return &RequestError{URL: w.url, Err: err}
	}
	// Draining before closing lets the connection go back to the pool instead
	// of being torn down after every write.
	defer releaseBody(resp.Body, w.url, w.log)

	if resp.StatusCode/100 != 2 {
		// The status is the rejection; the body only explains it. A body that
		// fails partway still leaves the status to report, so the read error
		// travels with it rather than replacing it.
		detail, readErr := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return &RemoteWriteError{
			URL:     w.url,
			Status:  resp.StatusCode,
			Body:    strings.TrimSpace(string(detail)),
			BodyErr: readErr,
		}
	}
	return nil
}

// Builder accumulates samples and groups them into the series remote write
// expects.
//
// Grouping is the point. A node produces the same couple of hundred label sets
// on every collection, so a flush holding several collections would otherwise
// repeat each label set once per sample -- for a 15s sample interval and a 60s
// flush, four copies of every label set on the wire. Collecting the samples of
// a label set into one TimeSeries is both smaller and what the ingest path is
// written for.
//
// A Builder is not safe for concurrent use; Exporter owns one and drives it
// from a single goroutine.
type Builder struct {
	// common is attached to every series: the node's identity and any operator
	// labels. Sorted once at construction.
	common []Label
	// series is keyed by the canonical rendering of a label set, so two samples
	// with the same labels in a different order still land in one series.
	series map[string]*Series
	// order preserves first-seen order, so a flush is deterministic. Prometheus
	// does not require it; tests and log reading do.
	order []string
	count int
	// collided holds the rendering of each series that was given a second
	// sample at a timestamp it already had, since the last TakeCollisions.
	collided []string
}

// NewBuilder returns a Builder that stamps every series with common.
func NewBuilder(common []Label) *Builder {
	c := append([]Label(nil), common...)
	sort.Slice(c, func(i, j int) bool { return c[i].Name < c[j].Name })
	return &Builder{common: c, series: make(map[string]*Series)}
}

// Add records one sample of a named metric.
//
// labels are the metric's own labels as alternating name/value pairs, matching
// the shape pkg/metrics already uses to write the text exposition format, so
// one description of a snapshot's metrics feeds both. A trailing name with no
// value is ignored rather than producing an empty label.
//
// A label whose value is empty is left off. In Prometheus' data model an empty
// value IS the label being absent -- a scrape of the same text exposition
// stores resource="" as no resource label at all -- so leaving it off stores
// exactly the same series. Sending it instead risks the whole request: a
// receiver that rejects an empty label value rejects the write it came in, and
// every other sample in the batch is lost with it. Host-wide alerts (CPU,
// memory, load, swap) carry no resource, so on a busy host that was every
// batch with an alert in it.
//
// A name appearing twice is resolved rather than emitted twice: Prometheus
// rejects a series carrying a repeated label, and it would reject the whole
// request, losing every other sample in the batch along with it. The metric's
// own label wins over a common one, and __name__ wins over both -- an operator
// whose external_labels happen to include "device" gets their value ignored on
// the disk metrics rather than an outage of the entire export. Configuration
// rejects that overlap up front; this is the second line, because a Builder is
// exported and its callers are not all in this repository.
func (b *Builder) Add(name string, value float64, timeMs int64, labels ...string) {
	set := make([]Label, 0, len(b.common)+1+len(labels)/2)
	seen := make(map[string]bool, len(b.common)+1+len(labels)/2)

	set = append(set, Label{Name: "__name__", Value: name})
	seen["__name__"] = true
	for i := 0; i+1 < len(labels); i += 2 {
		if labels[i+1] == "" || seen[labels[i]] {
			continue
		}
		seen[labels[i]] = true
		set = append(set, Label{Name: labels[i], Value: labels[i+1]})
	}
	for _, l := range b.common {
		if seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		set = append(set, l)
	}
	sort.Slice(set, func(i, j int) bool { return set[i].Name < set[j].Name })

	key := labelKey(set)
	s, ok := b.series[key]
	if !ok {
		s = &Series{Labels: set}
		b.series[key] = s
		b.order = append(b.order, key)
	}
	// Two samples of one series at one timestamp are two different things
	// given the same identity -- a caller's labelling defect. Sent together
	// they are a duplicate the receiver refuses, and it refuses the WHOLE
	// request: one defect would cost every other series in the batch. The
	// first sample stands, the collision is recorded for the caller to
	// report, and the batch stays deliverable.
	if n := len(s.Samples); n > 0 && s.Samples[n-1].TimeMs >= timeMs {
		b.collided = append(b.collided, renderLabels(set))
		return
	}
	s.Samples = append(s.Samples, Sample{Value: value, TimeMs: timeMs})
	b.count++
}

// TakeCollisions returns the series that were given a sample at a timestamp
// they already had since the last call, rendered as {name="value",...}, and
// forgets them. Each such sample was dropped rather than sent.
func (b *Builder) TakeCollisions() []string {
	c := b.collided
	b.collided = nil
	return c
}

// renderLabels renders a sorted label set the way Prometheus prints one.
func renderLabels(set []Label) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, l := range set {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(l.Name)
		sb.WriteString("=")
		sb.WriteString(strconv.Quote(l.Value))
	}
	sb.WriteByte('}')
	return sb.String()
}

// Len reports how many samples have been added, which is the unit the batch
// size is expressed in.
func (b *Builder) Len() int { return b.count }

// Drain returns the accumulated series and resets the Builder.
func (b *Builder) Drain() []Series {
	out := make([]Series, 0, len(b.order))
	for _, key := range b.order {
		out = append(out, *b.series[key])
	}
	b.series = make(map[string]*Series)
	b.order = nil
	b.count = 0
	b.collided = nil
	return out
}

// labelKey renders a sorted label set as a single string for map lookup. The
// separators are bytes that cannot occur in a label name and are rejected in a
// metric name, so no two distinct sets can collide on one key.
func labelKey(set []Label) string {
	var b strings.Builder
	for i := range set {
		b.WriteString(set[i].Name)
		b.WriteByte(0xff)
		b.WriteString(set[i].Value)
		b.WriteByte(0xfe)
	}
	return b.String()
}
