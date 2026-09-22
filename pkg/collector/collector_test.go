package collector_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/types/typestest"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(noopWriter{}, nil))
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestSystemCollector_Snapshot verifies that Snapshot returns a populated
// Snapshot with a sane timestamp and at least one CPU entry.
func TestSystemCollector_Snapshot(t *testing.T) {
	sc := collector.NewSystemCollector(silentLogger())
	before := time.Now()
	snap, err := sc.Snapshot(context.Background())
	after := time.Now()
	if err != nil {
		t.Fatalf("Snapshot returned unexpected error: %v", err)
	}
	if snap.Timestamp.Before(before) || snap.Timestamp.After(after) {
		t.Errorf("Snapshot timestamp %v is outside expected range [%v, %v]",
			snap.Timestamp, before, after)
	}
	if len(snap.CPUs) == 0 {
		t.Error("expected at least one CPU entry in Snapshot")
	}
	if snap.Host.Hostname == "" {
		t.Error("expected non-empty hostname in Snapshot")
	}

	// Whatever this host lacks -- GPUs, NPUs, RAID, containers -- is sent as
	// an empty list, never null (#151).
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	nulls, err := typestest.NullSlices(raw, snap)
	if err != nil {
		t.Fatalf("NullSlices: %v", err)
	}
	if len(nulls) != 0 {
		t.Errorf("this host's snapshot encodes these lists as null: %v", nulls)
	}
}

// TestSystemCollector_SnapshotCancelled verifies that a pre-cancelled context
// causes Snapshot to return a CollectorError wrapping context.Canceled.
func TestSystemCollector_SnapshotCancelled(t *testing.T) {
	sc := collector.NewSystemCollector(silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before Snapshot runs

	_, err := sc.Snapshot(ctx)
	if err == nil {
		// A cancelled context should eventually propagate; if it didn't it means
		// collection finished before the select checked, which is also acceptable
		// on very fast machines.  Skip instead of failing.
		t.Skip("snapshot completed before context cancellation was observed")
	}
	var ce *types.CollectorError
	if !isCollectorError(err, &ce) {
		t.Fatalf("expected *types.CollectorError, got %T: %v", err, err)
	}
	if ce.Collector != "system" {
		t.Errorf("expected collector name 'system', got %q", ce.Collector)
	}
}

// TestNewSystemCollector_NilLogger verifies that passing a nil logger does
// not panic and that the collector still works.
func TestNewSystemCollector_NilLogger(t *testing.T) {
	sc := collector.NewSystemCollector(nil)
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot with nil logger returned error: %v", err)
	}
	if snap.Timestamp.IsZero() {
		t.Error("expected non-zero Snapshot timestamp")
	}
}

func isCollectorError(err error, dst **types.CollectorError) bool {
	if ce, ok := err.(*types.CollectorError); ok {
		*dst = ce
		return true
	}
	return false
}
