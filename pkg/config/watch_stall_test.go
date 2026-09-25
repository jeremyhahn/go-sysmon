package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

const (
	stallStart = "alerts:\n  cpu_pct: 42\n  memory_pct: 43\n"
	stallFull  = "alerts:\n  cpu_pct: 70\n  memory_pct: 71\n"
	msgStall   = "config: file is still open for writing and has not changed; reload waits until it is closed"
)

// stallWatch starts the watcher on a fresh config with a short stall warning,
// so a test reaches the warning by waiting for it, not by waiting out the
// production period.
func stallWatch(t *testing.T) (string, *Manager, chan slog.Record) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.WriteFile(path, []byte(stallStart), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	records := make(chan slog.Record, 256)
	timing := watchTiming{debounce: watchDebounce, stall: stallWarnForTest}
	if err := mgr.watch(done, slog.New(channelHandler{records}), nil, timing); err != nil {
		t.Fatal(err)
	}
	return path, mgr, records
}

// stallWarnForTest is longer than the debounce, so a reload wrongly armed by
// any event before the stall would fire, and be seen, before the warning.
const stallWarnForTest = 2 * watchDebounce

// awaitStallWarning returns once the stall warning is logged, failing if a
// reload or any other warning comes first.
func awaitStallWarning(t *testing.T, records chan slog.Record) {
	t.Helper()
	for {
		r := await.Recv(t, "the warning that the writer has stalled with the file open", records)
		if r.Message == msgStall {
			if r.Level != slog.LevelWarn {
				t.Fatalf("stall logged at %s, want WARN", r.Level)
			}
			return
		}
		if r.Level > slog.LevelDebug {
			t.Fatalf("before the stall warning the watcher logged %s %q", r.Level, r.Message)
		}
	}
}

func awaitReloaded(t *testing.T, records chan slog.Record) {
	t.Helper()
	for {
		r := await.Recv(t, "the reload once the writer closes", records)
		if r.Message == "config: reloaded from disk" {
			return
		}
		if r.Level > slog.LevelDebug {
			t.Fatalf("waiting for the reload the watcher logged %s %q", r.Level, r.Message)
		}
	}
}

func assertAlerts(t *testing.T, mgr *Manager, cpu, memory float64, when string) {
	t.Helper()
	if a := mgr.Current().Alerts; a.CPUPct != cpu || a.MemoryPct != memory {
		t.Fatalf("%s: cpu_pct/memory_pct = %v/%v, want %v/%v", when, a.CPUPct, a.MemoryPct, cpu, memory)
	}
}

// TestWatch_WriterThatNeverClosesIsWarned is the residual case from the #226
// review: a writer that finishes the file but keeps it open. The kernel has
// no event for "finished", only the close, so the watcher cannot know the
// file is whole and must not publish it; but it must not stay silent either.
// It warns once the file has been quiet for the stall period, keeps the
// running configuration, and reloads when the writer finally closes.
func TestWatch_WriterThatNeverClosesIsWarned(t *testing.T) {
	path, mgr, records := stallWatch(t)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(stallFull); err != nil {
		t.Fatal(err)
	}

	awaitStallWarning(t, records)
	assertAlerts(t, mgr, 42, 43, "while the writer holds the file open")

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	awaitReloaded(t, records)
	assertAlerts(t, mgr, 70, 71, "after the writer closed")
}

// TestWatch_CloseThenReopenWithoutClosingIsWarned is the review's sequence: a
// writer truncates, closes partway, reopens and writes the rest, and never
// closes again. The partway close is the kernel's only signal of completion,
// so it is reloaded if the reopen comes after the debounce -- that file looked
// complete when it was closed. What must not happen is what happened before:
// the reopened writer's changes held with nothing ever logged.
func TestWatch_CloseThenReopenWithoutClosingIsWarned(t *testing.T) {
	path, mgr, records := stallWatch(t)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("alerts:\n  cpu_pct: 70\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := f.WriteString("  memory_pct: 71\n"); err != nil {
		t.Fatal(err)
	}

	// A reload of the partway-closed file may come first; the warning must
	// follow the reopened writer's stall however that race went.
	for {
		r := await.Recv(t, "the warning that the reopened writer has stalled", records)
		if r.Message == msgStall {
			break
		}
		if r.Level > slog.LevelInfo {
			t.Fatalf("waiting for the stall warning the watcher logged %s %q", r.Level, r.Message)
		}
	}
	if a := mgr.Current().Alerts; a.MemoryPct == 71 {
		t.Fatalf("memory_pct = 71 while the reopened writer still holds the file: its unclosed write was published")
	}
}

// TestWatch_ReaderDuringStalledWriteDoesNotPublish pins why the watcher does
// not subscribe to IN_OPEN. The kernel reports a reader's open exactly as it
// reports a writer's -- the mask carries no access mode -- so reading the file
// while a writer is stalled mid-file (cat, a backup, the watcher's own reload
// path) must not be taken as the writer having finished.
func TestWatch_ReaderDuringStalledWriteDoesNotPublish(t *testing.T) {
	path, mgr, records := stallWatch(t)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("alerts:\n  cpu_pct: 70\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}

	awaitStallWarning(t, records)
	assertAlerts(t, mgr, 42, 43, "after a reader opened the half-written file")

	if _, err := f.WriteString("  memory_pct: 71\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	awaitReloaded(t, records)
	assertAlerts(t, mgr, 70, 71, "after the writer closed")
}
