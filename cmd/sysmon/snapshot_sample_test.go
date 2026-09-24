package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// scriptedSnapshotter hands out its readings in order, as successive
// collections of a real collector would.
type scriptedSnapshotter struct {
	readings []types.Snapshot
	err      error
	taken    int
}

func (s *scriptedSnapshotter) Snapshot(context.Context) (types.Snapshot, error) {
	if s.err != nil {
		return types.Snapshot{}, s.err
	}
	snap := s.readings[s.taken]
	s.taken++
	return snap, nil
}

// recordingWait records the durations it was asked to wait, and returns at once.
type recordingWait struct{ waits []time.Duration }

func (w *recordingWait) wait(_ context.Context, d time.Duration) error {
	w.waits = append(w.waits, d)
	return nil
}

// unmeasured is what a collector's first reading holds: no core has a delta.
var unmeasured = types.Snapshot{CPUs: []types.CPUInfo{{Index: 0}, {Index: 1}}}

func renderCPU(t *testing.T, snap *types.Snapshot) string {
	t.Helper()
	var buf bytes.Buffer
	if err := cli.RenderCPU(&buf, snap); err != nil {
		t.Fatalf("RenderCPU: %v", err)
	}
	return buf.String()
}

// TestSampleSnapshot_OneShotMeasuresCPU verifies a one-shot command primes the
// collector, waits the sample interval, and reports the second reading, so its
// CPU usage is a measured percentage rather than the unknown every single
// reading has.
func TestSampleSnapshot_OneShotMeasuresCPU(t *testing.T) {
	measured := types.Snapshot{CPUs: []types.CPUInfo{
		{Index: 0, UsagePercent: 30, UsageAvailable: true},
		{Index: 1, UsagePercent: 10, UsageAvailable: true},
	}}
	sc := &scriptedSnapshotter{readings: []types.Snapshot{unmeasured, measured}}
	w := &recordingWait{}

	snap, err := sampleSnapshot(context.Background(), sc, w.wait)
	if err != nil {
		t.Fatalf("sampleSnapshot: %v", err)
	}
	if sc.taken != 2 {
		t.Errorf("took %d readings, want 2", sc.taken)
	}
	if len(w.waits) != 1 || w.waits[0] != oneShotSampleInterval {
		t.Errorf("waited %v, want once for oneShotSampleInterval (%v)", w.waits, oneShotSampleInterval)
	}
	if out := renderCPU(t, snap); !strings.Contains(out, "Average:        20.0%") {
		t.Errorf("one-shot output does not show the measured usage:\n%s", out)
	}
}

// TestSampleSnapshot_UnmeasurableSecondReading verifies a core the second
// reading could not measure -- its /proc/stat line malformed -- is still
// printed as unknown, not as the zero it carries.
func TestSampleSnapshot_UnmeasurableSecondReading(t *testing.T) {
	second := types.Snapshot{CPUs: []types.CPUInfo{
		{Index: 0},
		{Index: 1, UsagePercent: 40, UsageAvailable: true},
	}}
	sc := &scriptedSnapshotter{readings: []types.Snapshot{unmeasured, second}}
	w := &recordingWait{}

	snap, err := sampleSnapshot(context.Background(), sc, w.wait)
	if err != nil {
		t.Fatalf("sampleSnapshot: %v", err)
	}
	out := renderCPU(t, snap)
	if !strings.Contains(out, "Core  0:  "+strings.Repeat(" ", 20)+"       -") {
		t.Errorf("the unmeasurable core is not shown as unknown:\n%s", out)
	}
	if !strings.Contains(out, "Average:        40.0%") {
		t.Errorf("the average counted the unmeasurable core:\n%s", out)
	}
}

// TestSampleSnapshot_Failures verifies a failed reading or an interrupted wait
// is returned, and that nothing is read after it.
func TestSampleSnapshot_Failures(t *testing.T) {
	errRead := errors.New("injected read failure")
	sc := &scriptedSnapshotter{err: errRead}
	w := &recordingWait{}
	if _, err := sampleSnapshot(context.Background(), sc, w.wait); !errors.Is(err, errRead) {
		t.Errorf("failed priming reading: error %v, want %v", err, errRead)
	}
	if len(w.waits) != 0 {
		t.Errorf("waited %v after a failed priming reading, want no wait", w.waits)
	}

	errWait := errors.New("injected interrupt")
	sc = &scriptedSnapshotter{readings: []types.Snapshot{unmeasured, unmeasured}}
	failWait := func(context.Context, time.Duration) error { return errWait }
	if _, err := sampleSnapshot(context.Background(), sc, failWait); !errors.Is(err, errWait) {
		t.Errorf("interrupted wait: error %v, want %v", err, errWait)
	}
	if sc.taken != 1 {
		t.Errorf("took %d readings after an interrupted wait, want 1", sc.taken)
	}
}

// TestWaitSample verifies the real wait returns when its time is up, and
// returns a typed error at once when the context is already done. Neither case
// sleeps: the first waits zero, the second never starts waiting.
func TestWaitSample(t *testing.T) {
	if err := waitSample(context.Background(), 0); err != nil {
		t.Errorf("waitSample(0) = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitSample(ctx, time.Hour)
	var collErr *types.CollectorError
	if !errors.As(err, &collErr) || !errors.Is(err, context.Canceled) {
		t.Errorf("waitSample on a cancelled context = %v, want a *types.CollectorError wrapping context.Canceled", err)
	}
}
