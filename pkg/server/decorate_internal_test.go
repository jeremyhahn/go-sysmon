package server

import (
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// fullDisk is a reading that raises a disk alert under the default thresholds.
func fullDisk(at time.Time) *types.Snapshot {
	return &types.Snapshot{
		Timestamp: at,
		Disks:     []types.DiskInfo{{Name: "sda", TotalBytes: 1 << 40, UsedPercent: 97}},
	}
}

// TestSnapshotDecorator_EvaluatesAsOfTheReading: rates and durations are
// measured between readings, so a reading is evaluated at the time it was
// taken -- not when a server built later first sees it.
func TestSnapshotDecorator_EvaluatesAsOfTheReading(t *testing.T) {
	d := newSnapshotDecorator(&Server{})
	taken := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	snap := fullDisk(taken)
	d.Evaluate(snap)

	if len(snap.Alerts) == 0 {
		t.Fatal("no alert for a 97% full disk under the default thresholds")
	}
	if !snap.Alerts[0].Since.Equal(taken) {
		t.Errorf("Since = %v, want the reading's own time %v", snap.Alerts[0].Since, taken)
	}
}

// TestSnapshotDecorator_AnUndatedReadingUsesTheClock is the bad-input case: a
// reading with no time of its own is evaluated now rather than at the zero
// time, which would make every condition appear to have lasted two thousand
// years.
func TestSnapshotDecorator_AnUndatedReadingUsesTheClock(t *testing.T) {
	d := newSnapshotDecorator(&Server{})
	before := time.Now()

	snap := fullDisk(time.Time{})
	d.Evaluate(snap)

	if len(snap.Alerts) == 0 {
		t.Fatal("no alert for a 97% full disk under the default thresholds")
	}
	if since := snap.Alerts[0].Since; since.Before(before) {
		t.Errorf("Since = %v, want no earlier than %v", since, before)
	}
}

// TestSnapshotDecorator_IdentifiesTheServingNode: the build comes from the
// server and the name from its resolver.
func TestSnapshotDecorator_IdentifiesTheServingNode(t *testing.T) {
	d := newSnapshotDecorator(&Server{version: "1.0.0", gitCommit: "a46f6cf", buildDate: "2026-09-05"})
	snap := &types.Snapshot{}
	d.Identify(snap)

	if snap.Version != "1.0.0" || snap.GitCommit != "a46f6cf" || snap.BuildDate != "2026-09-05" {
		t.Errorf("build = %q/%q/%q, want 1.0.0/a46f6cf/2026-09-05", snap.Version, snap.GitCommit, snap.BuildDate)
	}
	if snap.NodeName == "" {
		t.Error("NodeName empty; a server always writes history under some name")
	}
}
