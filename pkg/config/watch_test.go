package config_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
)

// The watcher's log lines are its observable decisions. The tests wait on them
// instead of on the passage of time, so each step is reached by an event.
const (
	msgHeld     = "config: file is being written, reload waits until it is closed"
	msgReloaded = "config: reloaded from disk"
	msgKept     = "config: reload failed, keeping previous settings"
	msgStopped  = "config: stopped watching the config file"
	msgDirGone  = "config: the config directory is gone, changes are no longer watched"
)

// startingConfig is the file every watcher test begins from. Its values differ
// from the defaults (90 for both), so a reload that fell back to defaults is
// told apart from one that kept the last good configuration.
const startingConfig = "alerts:\n  cpu_pct: 42\n  memory_pct: 43\n"

// fullConfig is the complete file the tests write, and the values it carries.
const (
	fullConfig   = "alerts:\n  cpu_pct: 70\n  memory_pct: 71\n"
	firstLine    = "alerts:\n  cpu_pct: 70\n"
	restOfConfig = "  memory_pct: 71\n"
)

// recordingHandler hands every log record to the test, in order.
type recordingHandler struct{ records chan slog.Record }

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records <- r.Clone()
	return nil
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// alertValues is the pair of settings the tests follow through each reload.
type alertValues struct{ cpu, memory float64 }

// watched is a Manager with its file watcher running.
type watched struct {
	path    string
	mgr     *config.Manager
	records chan slog.Record
	// reloads receives the configuration each reload published, captured
	// inside the callback so a later reload cannot overwrite what is seen.
	reloads chan alertValues
	done    chan struct{}
	stopped bool
	// received counts reloads taken from the reloads channel, and logged the
	// reload records passed over, so a test can tell an expected reload's
	// log line from an unexpected one.
	received, logged int
}

// startWatch writes startingConfig to a temp dir and starts watching it.
func startWatch(t *testing.T) *watched {
	t.Helper()
	return startWatchAt(t, writeConfig(t, startingConfig))
}

// startWatchAt starts watching the config at path, which must already hold
// startingConfig.
func startWatchAt(t *testing.T, path string) *watched {
	t.Helper()
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	w := &watched{
		path:    path,
		mgr:     mgr,
		records: make(chan slog.Record, 256),
		reloads: make(chan alertValues, 16),
		done:    make(chan struct{}),
	}
	logger := slog.New(recordingHandler{records: w.records})
	if err := mgr.Watch(w.done, logger, func([]string) {
		a := mgr.Current().Alerts
		w.reloads <- alertValues{cpu: a.CPUPct, memory: a.MemoryPct}
	}); err != nil {
		t.Fatalf("Watch(): %v", err)
	}
	t.Cleanup(func() {
		if !w.stopped {
			close(w.done)
		}
	})
	return w
}

// next returns the next log record the watcher emits.
func (w *watched) next(t *testing.T, what string) slog.Record {
	t.Helper()
	return await.Recv(t, what, w.records)
}

// nextDecision returns the next record above debug level: a reload or a
// refusal to reload, skipping the watcher's commentary.
func (w *watched) nextDecision(t *testing.T, what string) slog.Record {
	t.Helper()
	for {
		if r := w.next(t, what); r.Level > slog.LevelDebug {
			return r
		}
	}
}

// awaitHeld waits for the watcher to report a write in progress. Any earlier
// record is a decision taken on a partly written file, which fails the test.
func (w *watched) awaitHeld(t *testing.T) {
	t.Helper()
	r := w.next(t, "the watcher to see the write in progress")
	if r.Message != msgHeld {
		t.Fatalf("first record while the file is partly written = %s %q, want the debug %q: the watcher acted on an incomplete file",
			r.Level, r.Message, msgHeld)
	}
}

// awaitHeldAfterDecisions waits for the watcher to report a write in progress,
// passing over the log records of reloads the test has already received
// through the callback. A reload record beyond those is a reload taken on the
// partly written file, and fails the test.
func (w *watched) awaitHeldAfterDecisions(t *testing.T) {
	t.Helper()
	for {
		r := w.next(t, "the watcher to see the write in progress")
		switch {
		case r.Message == msgHeld:
			return
		case r.Message == msgReloaded && w.logged < w.received:
			w.logged++
		case r.Level > slog.LevelDebug:
			t.Fatalf("while waiting for the write in progress the watcher logged %s %q", r.Level, r.Message)
		}
	}
}

// stop closes done and returns every record logged until the watcher reports
// that it has stopped, so a test can prove nothing else happened.
func (w *watched) stop(t *testing.T) []slog.Record {
	t.Helper()
	close(w.done)
	w.stopped = true
	var rest []slog.Record
	for {
		r := w.next(t, "the watcher to stop")
		if r.Message == msgStopped {
			return rest
		}
		rest = append(rest, r)
	}
}

// live returns the alert values Current() reports.
func (w *watched) live() alertValues {
	a := w.mgr.Current().Alerts
	return alertValues{cpu: a.CPUPct, memory: a.MemoryPct}
}

// assertOnlyReload checks that exactly one reload was published, with want.
func (w *watched) assertOnlyReload(t *testing.T, want alertValues) {
	t.Helper()
	for _, r := range w.stop(t) {
		if r.Level > slog.LevelDebug {
			t.Errorf("after the reload the watcher also logged %s %q, want nothing more", r.Level, r.Message)
		}
	}
	close(w.reloads)
	var got []alertValues
	for v := range w.reloads {
		got = append(got, v)
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("published reloads = %v, want exactly one with %v", got, want)
	}
}

// errorAttr returns the error a record carries under "error".
func errorAttr(r slog.Record) error {
	var found error
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			if err, ok := a.Value.Any().(error); ok {
				found = err
			}
			return false
		}
		return true
	})
	return found
}

func openInPlace(t *testing.T, path string, flag int) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|flag, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeString(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func closeFile(t *testing.T, f *os.File) {
	t.Helper()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestWatch_InPlaceWriteReloadsOnlyWhenClosed is the #220 defect: a tool that
// truncates the file and writes it in place, stalling between writes. The
// first line alone parses, so a reload taken mid-write would publish the new
// cpu_pct beside a default memory_pct -- a configuration nobody wrote. Nothing
// may be published until the writer closes the file, and then exactly once.
func TestWatch_InPlaceWriteReloadsOnlyWhenClosed(t *testing.T) {
	w := startWatch(t)

	f := openInPlace(t, w.path, os.O_TRUNC)
	writeString(t, f, firstLine)
	w.awaitHeld(t)

	// The writer is stalled mid-file. However long it stays there, the
	// running configuration is the last complete one.
	if got := w.live(); got != (alertValues{42, 43}) {
		t.Fatalf("while the file is half written Current() = %v, want the previous {42 43}", got)
	}

	writeString(t, f, restOfConfig)
	closeFile(t, f)

	if r := w.nextDecision(t, "the reload once the writer closes"); r.Message != msgReloaded {
		t.Fatalf("after close the watcher logged %s %q, want %q", r.Level, r.Message, msgReloaded)
	}
	w.assertOnlyReload(t, alertValues{70, 71})
}

// TestWatch_FileLeftEmptyKeepsLastGoodConfig covers a writer that truncates the
// file, closes it and has not yet written the new contents. Loading an empty
// file yields every default, which silently drops alert thresholds and fleet
// peers, so the watcher must refuse it with a warning and keep what is running.
func TestWatch_FileLeftEmptyKeepsLastGoodConfig(t *testing.T) {
	w := startWatch(t)

	closeFile(t, openInPlace(t, w.path, os.O_TRUNC))

	r := w.nextDecision(t, "the watcher's verdict on the empty file")
	if r.Level != slog.LevelWarn || r.Message != msgKept {
		t.Fatalf("empty file: watcher logged %s %q, want WARN %q", r.Level, r.Message, msgKept)
	}
	var empty *config.ConfigEmptyError
	if err := errorAttr(r); !errors.As(err, &empty) {
		t.Errorf("empty file: warning carries error %v, want a *config.ConfigEmptyError", err)
	} else if empty.Path != w.path {
		t.Errorf("ConfigEmptyError.Path = %q, want %q", empty.Path, w.path)
	}
	if got := w.live(); got != (alertValues{42, 43}) {
		t.Errorf("after an empty file Current() = %v, want the last good {42 43}", got)
	}

	// The contents arriving afterwards are picked up: the refusal is not a
	// state the watcher gets stuck in.
	f := openInPlace(t, w.path, os.O_TRUNC)
	writeString(t, f, fullConfig)
	closeFile(t, f)
	if r := w.nextDecision(t, "the reload of the completed file"); r.Message != msgReloaded {
		t.Fatalf("completed file: watcher logged %s %q, want %q", r.Level, r.Message, msgReloaded)
	}
	w.assertOnlyReload(t, alertValues{70, 71})
}

// TestWatch_WhitespaceOnlyFileIsEmpty treats a file of blank lines as empty: it
// decodes to exactly the same all-defaults configuration, so it is the same
// hazard.
func TestWatch_WhitespaceOnlyFileIsEmpty(t *testing.T) {
	w := startWatch(t)

	f := openInPlace(t, w.path, os.O_TRUNC)
	writeString(t, f, "\n   \n\n")
	closeFile(t, f)

	r := w.nextDecision(t, "the watcher's verdict on the blank file")
	var empty *config.ConfigEmptyError
	if !errors.As(errorAttr(r), &empty) {
		t.Fatalf("blank file: watcher logged %s %q (%v), want a warning carrying *config.ConfigEmptyError",
			r.Level, r.Message, errorAttr(r))
	}
	if got := w.live(); got != (alertValues{42, 43}) {
		t.Errorf("after a blank file Current() = %v, want the last good {42 43}", got)
	}
}

// TestWatch_AtomicReplaceReloadsOnce covers the way config-management tools
// save: a complete file written beside the config and renamed over it.
func TestWatch_AtomicReplaceReloadsOnce(t *testing.T) {
	w := startWatch(t)

	if err := replaceFile(w.path, fullConfig); err != nil {
		t.Fatal(err)
	}

	if r := w.nextDecision(t, "the reload after the rename"); r.Message != msgReloaded {
		t.Fatalf("after the rename the watcher logged %s %q, want %q", r.Level, r.Message, msgReloaded)
	}
	w.assertOnlyReload(t, alertValues{70, 71})
}

// TestWatch_EditorSwapFileThenRename covers an editor that writes its new copy
// to a sibling file in several stalled steps and renames it into place. Writes
// to any other name in the directory are not the config file and must not
// cause a reload; the rename does, once.
func TestWatch_EditorSwapFileThenRename(t *testing.T) {
	w := startWatch(t)

	swap := filepath.Join(filepath.Dir(w.path), ".sysmon.yaml.swp")
	f, err := os.OpenFile(swap, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writeString(t, f, firstLine)
	writeString(t, f, restOfConfig)
	closeFile(t, f)
	if err := os.Rename(swap, w.path); err != nil {
		t.Fatal(err)
	}

	if r := w.nextDecision(t, "the reload after the editor's rename"); r.Message != msgReloaded {
		t.Fatalf("after the rename the watcher logged %s %q, want %q", r.Level, r.Message, msgReloaded)
	}
	w.assertOnlyReload(t, alertValues{70, 71})
}

// TestWatch_RemovedThenRecreated covers a file deleted and written again from
// scratch. Removal is a reload -- with no file the service runs on defaults,
// exactly as it would have started -- and the new file, created and written in
// stalled steps, is published only once its writer closes it.
func TestWatch_RemovedThenRecreated(t *testing.T) {
	w := startWatch(t)

	if err := os.Remove(w.path); err != nil {
		t.Fatal(err)
	}
	if r := w.nextDecision(t, "the reload after the removal"); r.Message != msgReloaded {
		t.Fatalf("after removal the watcher logged %s %q, want %q", r.Level, r.Message, msgReloaded)
	}
	if got := await.Recv(t, "the reload after the removal", w.reloads); got != (alertValues{90, 90}) {
		t.Fatalf("after removal the published config = %v, want the defaults {90 90}", got)
	}

	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writeString(t, f, firstLine)
	// Wait for the watcher to see this write before stalling on it. The
	// create alone may already have produced a refusal of the still-empty
	// file; that is a warning, never a publication.
	for {
		r := w.next(t, "the watcher to see the new file being written")
		if r.Message == msgHeld {
			break
		}
		if r.Message == msgReloaded {
			t.Fatalf("the watcher published the new file before it was complete")
		}
	}
	if got := w.live(); got != (alertValues{90, 90}) {
		t.Fatalf("while the new file is half written Current() = %v, want {90 90}", got)
	}
	writeString(t, f, restOfConfig)
	closeFile(t, f)

	if got := await.Recv(t, "the reload of the completed file", w.reloads); got != (alertValues{70, 71}) {
		t.Fatalf("after the new file was closed the published config = %v, want {70 71}", got)
	}
}

// TestWatch_DirectoryRemovedStopsWatching covers the directory itself going
// away. The kernel drops the watch with it, so the watcher says so at warning
// level and releases its resources rather than looking alive while blind.
func TestWatch_DirectoryRemovedStopsWatching(t *testing.T) {
	w := startWatch(t)

	if err := os.RemoveAll(filepath.Dir(w.path)); err != nil {
		t.Fatal(err)
	}
	for {
		r := w.next(t, "the watcher to report the directory gone")
		if r.Message == msgDirGone {
			if r.Level != slog.LevelWarn {
				t.Errorf("directory gone logged at %s, want WARN", r.Level)
			}
			break
		}
	}
	if r := w.next(t, "the watcher to stop after its directory went"); r.Message != msgStopped {
		t.Errorf("after the directory went the watcher logged %q, want %q", r.Message, msgStopped)
	}
	w.stopped = true
}

// TestWatch_StopReleasesGoroutinesAndDescriptor proves that closing done ends
// the watcher entirely: none of its goroutines is left, and its inotify
// descriptor is closed. Both are checked by identity -- the watcher's own
// frames, the descriptor's inotify kind -- rather than by process-wide counts,
// which another test's goroutines winding down could mask.
func TestWatch_StopReleasesGoroutinesAndDescriptor(t *testing.T) {
	path := writeConfig(t, startingConfig)
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	done := make(chan struct{})
	records := make(chan slog.Record, 256)
	if err := mgr.Watch(done, slog.New(recordingHandler{records: records}), nil); err != nil {
		t.Fatalf("Watch(): %v", err)
	}
	// Watch starts its goroutines and returns; they are observed running
	// before the stop, so their absence afterwards means they exited.
	await.Until(t, "the watcher's goroutines and descriptor to be present", func(context.Context) bool {
		return watcherGoroutines() > 0 && inotifyDescriptors(t) > 0
	})
	close(done)

	await.Until(t, "the watcher's goroutines to exit and its descriptor to close", func(context.Context) bool {
		return watcherGoroutines() == 0 && inotifyDescriptors(t) == 0
	})
}

// watcherGoroutines counts the goroutines running the watcher's code.
func watcherGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "config.(*Manager).watchLoop") || strings.Contains(g, "config.(*inotify).read") {
			count++
		}
	}
	return count
}

// inotifyDescriptors counts this process's open inotify descriptors.
func inotifyDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("listing open descriptors: %v", err)
	}
	count := 0
	for _, e := range entries {
		// A descriptor can close between the listing and the readlink;
		// that one is simply not open any more.
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && target == "anon_inode:inotify" {
			count++
		}
	}
	return count
}

// TestReload_EmptyFileStillMeansDefaults pins the other half of the empty-file
// rule: only the watcher refuses an empty file. Starting on one, or an
// operator's explicit SIGHUP reload of one, runs on defaults as before.
func TestReload_EmptyFileStillMeansDefaults(t *testing.T) {
	path := writeConfig(t, "")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager() on an empty file = %v, want defaults", err)
	}
	if got := mgr.Current().Alerts.CPUPct; got != 90 {
		t.Errorf("starting on an empty file: cpu_pct = %v, want the default 90", got)
	}

	if err := os.WriteFile(path, []byte(startingConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}
	if got := mgr.Current().Alerts.CPUPct; got != 42 {
		t.Fatalf("after Reload() of the starting config cpu_pct = %v, want 42", got)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() of an empty file = %v, want defaults", err)
	}
	if got := mgr.Current().Alerts.CPUPct; got != 90 {
		t.Errorf("after Reload() of an empty file cpu_pct = %v, want the default 90", got)
	}
}
